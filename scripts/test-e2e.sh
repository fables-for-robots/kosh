#!/usr/bin/env bash
# kosh end to end, as its users meet it: kosh's image in a kind cluster of
# its own, installed with the chart, reached with the real ssh and the
# real iroh-ssh by the Go tests of e2e/ (built with the tag e2e). Run
# from anywhere, on Linux with Docker, Nix and the network; it exits 0
# only when every test passed.
#
#   scripts/test-e2e.sh
#
# KOSH_E2E_IMAGE=<image> uses an image that is already loaded instead of
# building one. KOSH_E2E_NO_NETWORK=1 skips the tests over the relays.
# KOSH_E2E_CACHE is where iroh-ssh is kept between runs (.cache/e2e).
# KOSH_E2E_SLOW=1 runs the tests that wait for kosh's discovery too
# (TestMetricsColumnsFollowTheAPI, up to 12 minutes), with the tests'
# deadline 40 minutes instead of 20. KOSH_E2E_RUN=<regexp> runs only the
# tests it matches, as go test -run does.
#
# It touches nothing it did not make: the image gets a tag of its own,
# kosh-e2e:<random>, right after it is loaded, and only that tag is used
# and removed; the cluster, kosh-e2e-<random>, has a kubeconfig of its
# own. Loading the image built by Nix sets the tag that image carries,
# ghcr.io/fables-for-robots/kosh:nix, as docker load does: the script
# leaves that tag as it found it, put back onto the image it named
# before, or removed when there was none. Whatever happens, it deletes
# what it made when it ends.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$PWD

# The flake: a copy of the tree that is not a git repository needs path:.
FLAKE=.
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || FLAKE=path:.

# The temporary directory of the caller: nix develop sets one of its own.
export KOSH_E2E_TMPDIR=${KOSH_E2E_TMPDIR:-${TMPDIR:-/tmp}}

if ! command -v go >/dev/null || ! command -v helm >/dev/null || ! command -v kind >/dev/null ||
  ! command -v kubectl >/dev/null || ! command -v yq >/dev/null; then
  if [ -n "${KOSH_E2E_IN_SHELL:-}" ]; then
    echo "FAIL: go, helm, kind, kubectl and yq are not all in the development shell" >&2
    exit 1
  fi
  # By its path from the root: "$0" may be relative to where it was called.
  KOSH_E2E_IN_SHELL=1 exec nix develop "$FLAKE#bare" --command "$ROOT/scripts/test-e2e.sh" "$@"
fi

# iroh-ssh as its users have it: the released 0.2.12 for Linux x86_64,
# checked against its SHA-256.
IROH_SSH_RELEASE=https://github.com/rustonbsd/iroh-ssh/releases/download/0.2.12/iroh-ssh.linux
IROH_SSH_SHA256=6e39a6b22f14d683598600350ca642d3b67fd0b0a2f2a7b29928ad6cdf032826
# Where that does not run, the commit of its repository the probe used,
# built with Nix's cargo.
IROH_SSH_REPO=https://github.com/rustonbsd/iroh-ssh
IROH_SSH_COMMIT=82e99719f8b4e44c69622140f4acb25e5ae54184

# metrics-server: the released manifest of the version pinned here,
# checked against its SHA-256.
METRICS_SERVER_VERSION=v0.7.2
METRICS_SERVER=https://github.com/kubernetes-sigs/metrics-server/releases/download/$METRICS_SERVER_VERSION/components.yaml
METRICS_SERVER_SHA256=f103539a54ed72efe66616afc74a8bfaed651703cb3918797599046af5617441

NAMESPACE=kosh
RELEASE=kosh

CACHE=${KOSH_E2E_CACHE:-$ROOT/.cache/e2e}
WORK=$(mktemp -d "$KOSH_E2E_TMPDIR/kosh-e2e.XXXXXX")
RANDOM_ID=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
CLUSTER=kosh-e2e-$RANDOM_ID
TAG=kosh-e2e:$RANDOM_ID
export KUBECONFIG=$WORK/kubeconfig
# kubectl's discovery cache and helm's directories, which default to the
# home directory, are the run's own: for the script's calls and the
# tests'.
export KUBECACHEDIR=$WORK/kube-cache
export HELM_CACHE_HOME=$WORK/helm/cache HELM_CONFIG_HOME=$WORK/helm/config HELM_DATA_HOME=$WORK/helm/data

CREATED_CLUSTER=
TAGGED=
FORWARD=
BUILDING=      # a build of iroh-ssh is under way, in $CACHE/iroh-ssh-build
NIX_TAG=       # the tag docker load set, until it is as it was found
NIX_TAG_WAS=   # the image it named before, or empty for none

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "== $*"; }

# restore_nix_tag leaves the tag the loaded image carries as it was found.
restore_nix_tag() {
  [ -n "$NIX_TAG" ] || return 0
  local now ours
  if [ -n "$NIX_TAG_WAS" ]; then
    docker tag "$NIX_TAG_WAS" "$NIX_TAG" || echo "warning: $NIX_TAG could not be put back onto $NIX_TAG_WAS" >&2
  else
    # Removed only while it names the image loaded here.
    now=$(docker image inspect --format '{{.Id}}' "$NIX_TAG" 2>/dev/null || true)
    ours=$(docker image inspect --format '{{.Id}}' "$TAG" 2>/dev/null || true)
    if [ -n "$now" ] && [ "$now" = "$ours" ]; then
      docker rmi "$NIX_TAG" >/dev/null || echo "warning: $NIX_TAG could not be removed" >&2
    fi
  fi
  NIX_TAG=
}

cleanup() {
  local status=$?
  set +e
  restore_nix_tag
  [ -z "$BUILDING" ] || rm -rf "$CACHE/iroh-ssh-build"
  if [ "$status" != 0 ] && [ -n "$CREATED_CLUSTER" ]; then
    echo "== kosh's log, the test having failed"
    kubectl --namespace "$NAMESPACE" logs deploy/"$RELEASE" --tail=200 2>&1
    kubectl --namespace "$NAMESPACE" get pods -o wide 2>&1
  fi
  [ -z "$FORWARD" ] || kill "$FORWARD" 2>/dev/null
  [ -z "$CREATED_CLUSTER" ] || kind delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1
  [ -z "$TAGGED" ] || docker rmi "$TAG" >/dev/null
  rm -rf "$WORK"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# 1. The image, under a tag of its own.
if [ -n "${KOSH_E2E_IMAGE:-}" ]; then
  step "the image $KOSH_E2E_IMAGE, as $TAG"
  docker image inspect "$KOSH_E2E_IMAGE" >/dev/null || fail "no image $KOSH_E2E_IMAGE is loaded"
  docker tag "$KOSH_E2E_IMAGE" "$TAG"
  TAGGED=1
else
  step "the image, built by Nix, as $TAG"
  nix build "$FLAKE#image" --option allow-unsafe-native-code-during-evaluation true --out-link "$WORK/image"
  # The tag the archive carries, and the image it names now, if any.
  name=$(tar -xOf "$WORK/image" manifest.json | yq -p json '.[0].RepoTags[0]')
  NIX_TAG_WAS=$(docker image inspect --format '{{.Id}}' "$name" 2>/dev/null || true)
  NIX_TAG=$name
  loaded=$(docker load -i "$WORK/image" | sed -n 's/^Loaded image: //p' | tail -1)
  [ "$loaded" = "$name" ] || fail "docker load named '$loaded', the archive '$name'"
  docker tag "$loaded" "$TAG"
  TAGGED=1
  restore_nix_tag
  # The tag is the image that was built, and not one somebody loaded
  # under the same name in the meantime: its layers are the archive's.
  config=$(tar -xOf "$WORK/image" manifest.json | yq -p json '.[0].Config')
  want=$(tar -xOf "$WORK/image" "$config" | yq -p json -o json -I 0 '.rootfs.diff_ids')
  got=$(docker image inspect --format '{{json .RootFS.Layers}}' "$TAG" | yq -p json -o json -I 0 '.')
  [ "$want" = "$got" ] || fail "$TAG is not the image that was built: its layers are $got, the archive's $want"
  rm -f "$WORK/image"
fi

# 2. A cluster of its own, with the image in it, and the images the
# test's objects and metrics-server run, so that nothing is pulled while
# the tests run.
step "the kind cluster $CLUSTER"
CREATED_CLUSTER=1
kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --wait 120s
kind load docker-image "$TAG" --name "$CLUSTER"
curl -fsSL --retry 3 -o "$WORK/metrics-server.yaml" "$METRICS_SERVER"
echo "$METRICS_SERVER_SHA256  $WORK/metrics-server.yaml" | sha256sum -c --quiet - || fail "$METRICS_SERVER is not the one this script knows"
# A registry that does not answer at once is tried again, three times.
sed -n 's/^ *image: *//p' e2e/testdata/2-objects.yaml "$WORK/metrics-server.yaml" | while read -r image; do
  for try in 1 2 3; do
    docker exec "$CLUSTER-control-plane" crictl pull "$image" >/dev/null && break
    [ "$try" != 3 ] || fail "$image could not be pulled into the node"
    sleep 5
  done
  echo "ok   $image is in the node"
done

# metrics-server, as a cluster that has it: the released manifest, with
# --kubelet-insecure-tls since kind's kubelets serve certificates of
# their own signing, as kind's docs have it. Before kosh, whose
# discovery at start then finds the metrics API.
step "metrics-server $METRICS_SERVER_VERSION"
kubectl apply -f "$WORK/metrics-server.yaml" >/dev/null
kubectl --namespace kube-system patch deployment metrics-server --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
kubectl wait --for=condition=Available apiservice/v1beta1.metrics.k8s.io --timeout=180s
kubectl --namespace kube-system wait --for=condition=Available deployment/metrics-server --timeout=180s

# 3. The keys and the state.
step "the keys and the state"
ssh-keygen -q -t ed25519 -N '' -C alice@kosh-e2e/admin -f "$WORK/full"
ssh-keygen -q -t ed25519 -N '' -C bob@kosh-e2e -f "$WORK/readonly"
mkdir -m 0700 "$WORK/state"
docker run --rm --user "$(id -u):$(id -g)" -v "$WORK/state:/state" "$TAG" id --state-dir /state >"$WORK/id"
cat "$WORK/id"
ENDPOINT=$(sed -n 's/^endpoint ID *//p' "$WORK/id")
sed -n 's/^host key *//p' "$WORK/id" >"$WORK/host_key.pub"
[ -n "$ENDPOINT" ] && [ -s "$WORK/host_key.pub" ] || fail "kosh id printed no endpoint ID or host key"
kubectl create namespace "$NAMESPACE"
kubectl --namespace "$NAMESPACE" create secret generic kosh-state --from-file="$WORK/state"
cat "$WORK/full.pub" "$WORK/readonly.pub" >"$WORK/authorized_keys"

# 4. kosh, installed with its chart.
step "kosh, installed with the chart"
helm install "$RELEASE" charts/kosh --namespace "$NAMESPACE" \
  --set image.repository="${TAG%%:*}" --set image.tag="${TAG#*:}" --set image.pullPolicy=Never \
  --set state.existingSecret=kosh-state --set-file authorizedKeys="$WORK/authorized_keys" \
  --set iroh.mdns=false >/dev/null
kubectl --namespace "$NAMESPACE" rollout status deploy/"$RELEASE" --timeout=180s

# 5. The endpoint ID kosh serves on is the one kosh id printed.
step "the endpoint ID"
serving=
for _ in $(seq 120); do
  serving=$(kubectl --namespace "$NAMESPACE" logs deploy/"$RELEASE" | grep 'msg="kosh is serving"' || true)
  [ -z "$serving" ] || break
  sleep 1
done
[ -n "$serving" ] || fail "kosh's log has no line kosh is serving after 2 minutes"
echo "$serving"
logged=$(echo "$serving" | sed -n 's/.* endpoint=\([0-9a-f]*\).*/\1/p')
[ "$logged" = "$ENDPOINT" ] || fail "kosh serves on the endpoint ID '$logged', kosh id printed '$ENDPOINT'"
echo "ok   kosh serves on $ENDPOINT"

# 6. What the tests look at.
step "the test's objects"
kubectl apply -f e2e/testdata/1-crd.yaml
kubectl wait --for=condition=Established crd/widgets.e2e.kosh.test --timeout=60s
kubectl apply -f e2e/testdata/2-objects.yaml
kubectl --namespace default wait --for=condition=Ready pod/marker --timeout=120s
kubectl --namespace default rollout status deploy/web --timeout=120s

# 7. The Service, on a local port.
step "the Service, forwarded"
kubectl --namespace "$NAMESPACE" port-forward svc/"$RELEASE" 0:22 >"$WORK/forward.log" 2>&1 &
FORWARD=$!
port=
for _ in $(seq 300); do
  port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$WORK/forward.log" | head -1)
  [ -z "$port" ] || break
  kill -0 "$FORWARD" 2>/dev/null || { cat "$WORK/forward.log" >&2; fail "kubectl port-forward ended"; }
  sleep 0.1
done
[ -n "$port" ] || fail "kubectl port-forward said no port: $(cat "$WORK/forward.log")"
echo "ok   127.0.0.1:$port"

# iroh-ssh, released, or built where the release does not run.
step "iroh-ssh"
iroh_ssh=$CACHE/iroh-ssh-0.2.12/iroh-ssh
if [ "$(uname -s)/$(uname -m)" = Linux/x86_64 ]; then
  # fetch_iroh_ssh downloads the release into the cache.
  fetch_iroh_ssh() {
    mkdir -p "$(dirname "$iroh_ssh")"
    curl -fsSL --retry 3 -o "$iroh_ssh.download" "$IROH_SSH_RELEASE"
    echo "$IROH_SSH_SHA256  $iroh_ssh.download" | sha256sum -c --quiet - || fail "$IROH_SSH_RELEASE is not the one this script knows"
    chmod +x "$iroh_ssh.download"
    mv "$iroh_ssh.download" "$iroh_ssh"
  }
  [ -x "$iroh_ssh" ] || fetch_iroh_ssh
  # The cached one too is the one this script knows; one that is not is
  # fetched again, once.
  if ! echo "$IROH_SSH_SHA256  $iroh_ssh" | sha256sum -c --quiet - 2>/dev/null; then
    echo "the cached $iroh_ssh is not the release this script knows: fetching it again"
    rm -f "$iroh_ssh"
    fetch_iroh_ssh
    echo "$IROH_SSH_SHA256  $iroh_ssh" | sha256sum -c --quiet - || fail "$iroh_ssh is not the release this script knows"
  fi
  "$iroh_ssh" --help >/dev/null 2>&1 || {
    echo "the released iroh-ssh does not run here"
    iroh_ssh=
  }
else
  echo "no released iroh-ssh for $(uname -s)/$(uname -m)"
  iroh_ssh=
fi
if [ -z "$iroh_ssh" ]; then
  iroh_ssh=$CACHE/iroh-ssh-$IROH_SSH_COMMIT/bin/iroh-ssh
  if [ ! -x "$iroh_ssh" ]; then
    echo "building iroh-ssh at $IROH_SSH_COMMIT with Nix's cargo instead (a few minutes)"
    build=$CACHE/iroh-ssh-build
    BUILDING=1
    rm -rf "$build"
    git clone --quiet "$IROH_SSH_REPO" "$build/src"
    git -C "$build/src" checkout --quiet "$IROH_SSH_COMMIT"
    CARGO_HOME=$build/cargo CARGO_TARGET_DIR=$build/target \
      nix shell --inputs-from "$FLAKE" nixpkgs#cargo nixpkgs#rustc nixpkgs#gcc \
      --command cargo install --quiet --locked --path "$build/src" --root "$CACHE/iroh-ssh-$IROH_SSH_COMMIT"
    rm -rf "$build"
    BUILDING=
  fi
fi
echo "ok   $iroh_ssh"

# 8. The tests.
step "the tests"
deadline=20m
[ "${KOSH_E2E_SLOW:-}" != 1 ] || deadline=40m
KOSH_E2E_SSH=127.0.0.1:$port \
  KOSH_E2E_FULL_KEY=$WORK/full \
  KOSH_E2E_READONLY_KEY=$WORK/readonly \
  KOSH_E2E_HOST_KEY=$WORK/host_key.pub \
  KOSH_E2E_ENDPOINT=$ENDPOINT \
  KOSH_E2E_IROH_SSH=$iroh_ssh \
  go test -tags e2e -count=1 -timeout "$deadline" -v ${KOSH_E2E_RUN:+-run "$KOSH_E2E_RUN"} ./e2e/

echo "all checks passed"
