#!/usr/bin/env bash
# iroh-ssh finds kosh by mDNS alone. kosh runs as a process on this
# machine, built from the tree, with the relays off and no SSH port, so
# that nothing but its mDNS announcement says where it is; iroh-ssh runs
# on the same machine, on a pseudo-terminal, until it shows kosh's pods
# (the test TestIrohSSHFindsKoshByMDNS of e2e/). Run from anywhere, on
# Linux with Docker, Nix and the network; it exits 0 only when the test
# passed.
#
#   scripts/test-mdns.sh
#
# The released iroh-ssh 0.2.12 looks nothing up by mDNS: the script
# builds iroh-ssh from its repository at the commit the probe used, with
# Nix's cargo, into KOSH_E2E_CACHE (.cache/e2e), which takes a few
# minutes the first time and is kept for the next.
#
# kosh needs an API server: the script makes a kind cluster of its own,
# kosh-mdns-<random>, with a kubeconfig of its own, unless
# KOSH_E2E_KUBECONFIG names one; the KUBECONFIG of the environment is not
# used. Its kosh is the instance mdns, so that its bindings are its own
# and it never prunes a deployed kosh's. Its key is of the ClusterRole
# view, whose binding, kosh:mdns:clusterrole:view, kosh makes; in a
# cluster that is not the script's own, the binding is deleted at the end
# unless it was there before. It removes what it built, started and made
# when it ends.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$PWD

FLAKE=.
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || FLAKE=path:.

# The temporary directory of the caller: nix develop sets one of its own.
export KOSH_E2E_TMPDIR=${KOSH_E2E_TMPDIR:-${TMPDIR:-/tmp}}

if ! command -v go >/dev/null || ! command -v kind >/dev/null || ! command -v kubectl >/dev/null; then
  if [ -n "${KOSH_E2E_IN_SHELL:-}" ]; then
    echo "FAIL: go, kind and kubectl are not all in the development shell" >&2
    exit 1
  fi
  # By its path from the root: "$0" may be relative to where it was called.
  KOSH_E2E_IN_SHELL=1 exec nix develop "$FLAKE#bare" --command "$ROOT/scripts/test-mdns.sh" "$@"
fi

IROH_SSH_REPO=https://github.com/rustonbsd/iroh-ssh
IROH_SSH_COMMIT=82e99719f8b4e44c69622140f4acb25e5ae54184

CACHE=${KOSH_E2E_CACHE:-$ROOT/.cache/e2e}
WORK=$(mktemp -d "$KOSH_E2E_TMPDIR/kosh-e2e-mdns.XXXXXX")
RANDOM_ID=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
CLUSTER=kosh-mdns-$RANDOM_ID

CREATED_CLUSTER=
KOSH=
# The binding kosh makes for its key's role, and whether the script is to
# delete it.
BINDING=kosh:mdns:clusterrole:view
DELETE_BINDING=
BUILDING=      # a build of iroh-ssh is under way, in $CACHE/iroh-ssh-build

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "== $*"; }

cleanup() {
  local status=$?
  set +e
  if [ -n "$KOSH" ]; then
    kill "$KOSH" 2>/dev/null
    wait "$KOSH" 2>/dev/null
  fi
  [ -z "$DELETE_BINDING" ] || kubectl delete clusterrolebinding "$BINDING" --ignore-not-found >/dev/null
  [ -z "$BUILDING" ] || rm -rf "$CACHE/iroh-ssh-build"
  if [ "$status" != 0 ] && [ -s "$WORK/kosh.log" ]; then
    echo "== kosh's log, the test having failed"
    cat "$WORK/kosh.log"
  fi
  [ -z "$CREATED_CLUSTER" ] || kind delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1
  rm -rf "$WORK"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# iroh-ssh from its repository, at the commit with mDNS.
step "iroh-ssh at $IROH_SSH_COMMIT"
iroh_ssh=$CACHE/iroh-ssh-$IROH_SSH_COMMIT/bin/iroh-ssh
if [ ! -x "$iroh_ssh" ]; then
  echo "building it with Nix's cargo (a few minutes)"
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
echo "ok   $iroh_ssh"

# An API server for kosh to show.
if [ -n "${KOSH_E2E_KUBECONFIG:-}" ]; then
  step "the cluster of $KOSH_E2E_KUBECONFIG"
  export KUBECONFIG=$KOSH_E2E_KUBECONFIG
else
  step "the kind cluster $CLUSTER"
  export KUBECONFIG=$WORK/kubeconfig
  CREATED_CLUSTER=1
  kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --wait 120s
fi

# kosh, as a process, found by nothing but mDNS.
step "kosh serve, with the relays off"
go build -o "$WORK/kosh" ./cmd/kosh
# The test reads the key as the full key's (KOSH_E2E_FULL_KEY); all it
# needs is to see the pods, which view may.
ssh-keygen -q -t ed25519 -N '' -C alice@kosh-mdns/view -f "$WORK/full"
if [ -z "$CREATED_CLUSTER" ] && ! kubectl get clusterrolebinding "$BINDING" >/dev/null 2>&1; then
  DELETE_BINDING=1
fi
cp "$WORK/full.pub" "$WORK/authorized_keys"
"$WORK/kosh" id --state-dir "$WORK/state" >"$WORK/id"
ENDPOINT=$(sed -n 's/^endpoint ID *//p' "$WORK/id")
sed -n 's/^host key *//p' "$WORK/id" >"$WORK/host_key.pub"
[ -n "$ENDPOINT" ] && [ -s "$WORK/host_key.pub" ] || fail "kosh id printed no endpoint ID or host key"
"$WORK/kosh" serve --ssh-listen "" --iroh-relay=false --iroh-mdns=true --instance mdns \
  --state-dir "$WORK/state" --authorized-keys "$WORK/authorized_keys" --kubeconfig "$KUBECONFIG" \
  2>"$WORK/kosh.log" &
KOSH=$!
for _ in $(seq 300); do
  grep -q 'msg="kosh is serving"' "$WORK/kosh.log" && break
  kill -0 "$KOSH" 2>/dev/null || fail "kosh serve ended: $(cat "$WORK/kosh.log")"
  sleep 0.1
done
grep 'msg="kosh is serving"' "$WORK/kosh.log" || fail "kosh is not serving after 30 seconds: $(cat "$WORK/kosh.log")"
grep -q " endpoint=$ENDPOINT" "$WORK/kosh.log" || fail "kosh does not serve on the endpoint ID kosh id printed, $ENDPOINT"
# What it announces, for whoever reads this when the test fails.
grep 'mDNS' "$WORK/kosh.log" || true

step "iroh-ssh, by mDNS"
KOSH_E2E_MDNS=1 \
  KOSH_E2E_FULL_KEY=$WORK/full \
  KOSH_E2E_HOST_KEY=$WORK/host_key.pub \
  KOSH_E2E_ENDPOINT=$ENDPOINT \
  KOSH_E2E_IROH_SSH=$iroh_ssh \
  KOSH_E2E_LOG=$WORK/kosh.log \
  go test -tags e2e -count=1 -timeout 5m -v -run '^TestIrohSSHFindsKoshByMDNS$' ./e2e/

echo "all checks passed"
