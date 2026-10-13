#!/usr/bin/env bash
# Tests the Helm chart at charts/kosh: lint, render, schemas, and the
# refusals. Run from the repository root. Needs helm, kubeconform and yq;
# when they are not on the PATH the script runs itself under Nix.
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v helm >/dev/null || ! command -v kubeconform >/dev/null || ! command -v yq >/dev/null; then
  exec nix shell nixpkgs#kubernetes-helm nixpkgs#kubeconform nixpkgs#yq-go --command "$0" "$@"
fi

CHART=charts/kosh
CI=$CHART/ci
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# render CASE: lints, renders to $OUT/CASE.yaml and checks the schemas.
render() {
  local c=$1
  echo "== case $c"
  helm lint "$CHART" -f "$CI/$c-values.yaml" >/dev/null || fail "$c: helm lint"
  echo "ok   lints"
  helm template kosh "$CHART" -f "$CI/$c-values.yaml" >"$OUT/$c.yaml" || fail "$c: helm template"
  kubeconform -strict -summary "$OUT/$c.yaml" || fail "$c: kubeconform"
  echo "ok   conforms"
}

# q CASE KIND EXPR: yq over the object of KIND in the rendered CASE.
q() { yq "select(.kind == \"$2\") | $3" "$OUT/$1.yaml"; }

# eq CASE KIND EXPR WANT DESCRIPTION
eq() {
  local got
  got=$(q "$1" "$2" "$3")
  [ "$got" = "$4" ] || fail "$1: $5: $2 $3 is '$got', want '$4'"
  echo "ok   $5"
}

# count CASE KIND WANT
count() {
  local got
  got=$(yq "select(.kind == \"$2\") | .kind" "$OUT/$1.yaml" | grep -c . || true)
  [ "$got" = "$3" ] || fail "$1: $2 objects: $got, want $3"
  echo "ok   $3 $2"
}

D='.spec.template.spec'
C="$D.containers[0]"

# --- secret
render secret
eq secret Service .spec.type ClusterIP "Service is ClusterIP"
eq secret Service .spec.ports[0].port 22 "Service port 22"
eq secret Service .spec.ports[0].targetPort ssh "Service targets ssh"
eq secret Service .spec.externalTrafficPolicy null "ClusterIP has no externalTrafficPolicy"
eq secret Deployment .spec.replicas 1 "one replica"
eq secret Deployment .spec.strategy.type Recreate "strategy Recreate"
eq secret Deployment "$D.securityContext.runAsNonRoot" true "pod runs as non-root"
eq secret Deployment "$D.securityContext.runAsUser" 65532 "runAsUser"
eq secret Deployment "$D.securityContext.runAsGroup" 65532 "runAsGroup"
eq secret Deployment "$D.securityContext.fsGroup" 65532 "fsGroup"
eq secret Deployment "$D.securityContext.seccompProfile.type" RuntimeDefault "seccomp"
eq secret Deployment "$C.securityContext.allowPrivilegeEscalation" false "no privilege escalation"
eq secret Deployment "$C.securityContext.readOnlyRootFilesystem" true "read-only root"
eq secret Deployment "$C.securityContext.capabilities.drop[0]" ALL "capabilities dropped"
eq secret Deployment "$C.readinessProbe.tcpSocket.port" ssh "readiness probe on ssh"
eq secret Deployment "$C.livenessProbe.tcpSocket.port" ssh "liveness probe on ssh"
eq secret Deployment "$D.volumes[] | select(.name == \"state\") | .secret.secretName" kosh-state "state is the Secret"
eq secret Deployment "$D.volumes[] | select(.name == \"state\") | .secret.defaultMode" 0440 "state mode 0440"
eq secret Deployment "$C.volumeMounts[] | select(.name == \"state\") | .mountPath" /var/lib/kosh "state mounted"
eq secret Deployment "$C.volumeMounts[] | select(.name == \"state\") | .readOnly" true "state read-only"
eq secret Deployment "$C.volumeMounts[] | select(.name == \"keys\") | .mountPath" /etc/kosh "keys mounted"
eq secret Deployment "$C.volumeMounts[] | select(.name == \"keys\") | .readOnly" true "keys read-only"
eq secret Deployment "$C.volumeMounts[] | select(.name == \"keys\") | .subPath" null "keys without subPath"
eq secret Deployment ".spec.template.metadata.annotations" null "no checksum annotation"
eq secret Deployment "$D.hostNetwork" null "no host network"
eq secret Deployment "$C.args | join(\" \")" "serve --ssh-listen=:2222 --iroh=true --iroh-relay=true --iroh-mdns=true --state-dir=/var/lib/kosh --authorized-keys=/etc/kosh/authorized_keys" "arguments"
eq secret ClusterRoleBinding .roleRef.name cluster-admin "binds cluster-admin"
eq secret ClusterRoleBinding .metadata.name kosh-cluster-admin "the binding is named for cluster-admin"
count secret PersistentVolumeClaim 0
count secret ServiceAccount 1
count secret ConfigMap 1

# --- loadbalancer
render loadbalancer
eq loadbalancer Service .spec.type LoadBalancer "Service is a LoadBalancer"
eq loadbalancer Service '.metadata.annotations["service.beta.kubernetes.io/aws-load-balancer-type"]' nlb "Service annotation"
eq loadbalancer Service '.spec.loadBalancerSourceRanges | join(",")' "203.0.113.0/24,198.51.100.7/32" "source ranges"
eq loadbalancer Service .spec.externalTrafficPolicy Local "externalTrafficPolicy Local"
eq loadbalancer Service '.spec.ports | length' 1 "Service carries nothing of iroh"

# --- persistence
render persistence
eq persistence PersistentVolumeClaim .spec.resources.requests.storage 16Mi "PVC of 16Mi"
eq persistence PersistentVolumeClaim .spec.storageClassName fast "PVC storage class"
eq persistence Deployment "$D.volumes[] | select(.name == \"state\") | .persistentVolumeClaim.claimName" kosh "state is the claim"
eq persistence Deployment "$D.volumes[] | select(.name == \"state\") | .secret" null "no Secret volume"
eq persistence Deployment "$C.volumeMounts[] | select(.name == \"state\") | .mountPath" /var/lib/kosh "state mounted"
eq persistence Deployment "$C.volumeMounts[] | select(.name == \"state\") | .readOnly" null "state read-write"

# --- hostnetwork
render hostnetwork
eq hostnetwork Deployment "$D.hostNetwork" true "host network"
eq hostnetwork Deployment "$D.dnsPolicy" ClusterFirstWithHostNet "DNS policy"
eq hostnetwork Deployment "$C.args[] | select(test(\"^--iroh-bind\"))" "--iroh-bind=0.0.0.0:4436" "iroh bind"
eq hostnetwork Deployment "$C.ports[] | select(.name == \"iroh\") | .protocol" UDP "UDP container port"
eq hostnetwork Deployment "$C.ports[] | select(.name == \"iroh\") | .containerPort" 4436 "UDP container port number"

# --- no-ssh
render no-ssh
count no-ssh Service 0
eq no-ssh Deployment "$C.readinessProbe" null "no readiness probe"
eq no-ssh Deployment "$C.livenessProbe" null "no liveness probe"
eq no-ssh Deployment "$C.args[] | select(test(\"^--ssh-listen\"))" "--ssh-listen=" "ssh listen off"
eq no-ssh Deployment "$C.ports" null "no container ports"

# --- no-iroh
render no-iroh
eq no-iroh Deployment "$C.args[] | select(test(\"^--iroh=\"))" "--iroh=false" "iroh off"

# --- keys
render keys
want=$(yq '.authorizedKeys' "$CI/keys-values.yaml")
got=$(yq 'select(.kind == "ConfigMap") | .data.authorized_keys' "$OUT/keys.yaml")
[ "$got" = "$want" ] || fail "keys: authorized_keys differs from authorizedKeys"
echo "ok   authorized_keys is exactly the text"
eq keys ConfigMap '.data.authorized_keys | split("\n") | map(select(test("^read-only "))) | length' 1 "one read-only line"

# --- narrow-rbac
render narrow-rbac
eq narrow-rbac ClusterRoleBinding .roleRef.name view "binds view"
eq narrow-rbac ClusterRoleBinding .metadata.name kosh-view "the binding is named for view"
# A changed role is a new binding, which Helm makes and the old one it
# removes: a binding's roleRef cannot be changed.
[ "$(q secret ClusterRoleBinding .metadata.name)" != "$(q narrow-rbac ClusterRoleBinding .metadata.name)" ] ||
  fail "narrow-rbac: the binding of view is named as that of cluster-admin"
echo "ok   another role is another binding"

# --- system-role
render system-role
eq system-role ClusterRoleBinding .roleRef.name system:aggregate-to-view "binds system:aggregate-to-view"
eq system-role ClusterRoleBinding .metadata.name kosh-system:aggregate-to-view "the binding is named for system:aggregate-to-view"

# --- the refusals
# refuses NAME MESSAGE ARGS...: helm template with ARGS must fail with MESSAGE on standard error.
refuses() {
  local name=$1 msg=$2
  shift 2
  echo "== refusal $name"
  if helm template kosh "$CHART" "$@" >"$OUT/refusal.out" 2>"$OUT/refusal.err"; then
    fail "$name: helm template succeeded, want a refusal"
  fi
  grep -qF -- "$msg" "$OUT/refusal.err" || { cat "$OUT/refusal.err" >&2; fail "$name: standard error lacks '$msg'"; }
  echo "ok   refused with '$msg'"
}

refuses no-state "kosh needs its state: set state.existingSecret or state.persistence.enabled, or its endpoint ID and host key change at every start"
refuses both-states "not both" --set state.existingSecret=kosh-state --set state.persistence.enabled=true
refuses no-transport "set ssh.enabled or iroh.enabled" -f "$CI/secret-values.yaml" --set ssh.enabled=false --set iroh.enabled=false
refuses no-discovery "set iroh.relay or iroh.mdns" -f "$CI/secret-values.yaml" --set iroh.relay=false --set iroh.mdns=false

echo "all checks passed"
