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
count secret PersistentVolumeClaim 0
count secret ServiceAccount 1
count secret ConfigMap 1

echo "all checks passed"
