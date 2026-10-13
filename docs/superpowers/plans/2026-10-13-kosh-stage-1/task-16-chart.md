# Task 16: the Helm chart

The chart is how kosh is deployed; the repository holds no other manifests. A full key of kosh is a cluster administrator, so the chart's defaults expose nothing and its two refusals keep an install from silently losing its identity.

**Read first:** the spec, §4, §6, §7, §14 and §15 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`).

**Files:**
- Create: `charts/kosh/Chart.yaml`, `charts/kosh/values.yaml`, `charts/kosh/.helmignore`, `charts/kosh/templates/_helpers.tpl`, `templates/serviceaccount.yaml`, `templates/rbac.yaml`, `templates/configmap.yaml`, `templates/deployment.yaml`, `templates/service.yaml`, `templates/pvc.yaml`, `templates/NOTES.txt`
- Create: `charts/kosh/ci/` with one values file per case of the tests
- Create: `scripts/test-chart.sh`

**Tools.** `helm` and `kubeconform` are not on this machine's PATH. The script finds them or runs them through Nix: `nix shell nixpkgs#kubernetes-helm nixpkgs#kubeconform nixpkgs#yq-go --command …`. Use the same in your own work.

**Interfaces:**
- Consumes: the command line of `kosh serve` (spec §14): `--ssh-listen`, `--iroh`, `--iroh-bind`, `--iroh-relay`, `--iroh-mdns`, `--state-dir`, `--authorized-keys`. The image's entry point is `kosh`; the container runs `kosh serve` with flags.
- Produces: the chart `kosh`, with the values of spec §15, and `scripts/test-chart.sh`, which Task 18 runs in the workflow and Task 19 installs from.

## Behaviour

1. `Chart.yaml`: `apiVersion: v2`, `name: kosh`, `type: application`, `version: 0.0.0`, `appVersion: "0.0.0"` (a release sets both), a description, `home` and `sources` `https://github.com/fables-for-robots/kosh`.
2. **Values**, with these names and defaults, each documented by a comment in `values.yaml`:

   | Value | Default |
   |---|---|
   | `image.repository` | `ghcr.io/fables-for-robots/kosh` |
   | `image.tag` | `""`, which means the chart's `appVersion` |
   | `image.pullPolicy` | `IfNotPresent` |
   | `imagePullSecrets` | `[]` |
   | `authorizedKeys` | `""`: the text of the key list |
   | `ssh.enabled` | `true` |
   | `ssh.port` | `2222` |
   | `service.type` | `ClusterIP` |
   | `service.port` | `22` |
   | `service.annotations` | `{}` |
   | `service.loadBalancerSourceRanges` | `[]` |
   | `service.externalTrafficPolicy` | `Cluster` |
   | `iroh.enabled` | `true` |
   | `iroh.relay` | `true` |
   | `iroh.mdns` | `true` |
   | `iroh.hostNetwork` | `false` |
   | `iroh.port` | `null` |
   | `state.existingSecret` | `""` |
   | `state.persistence.enabled` | `false` |
   | `state.persistence.size` | `16Mi` |
   | `state.persistence.storageClass` | `""`: the cluster's default |
   | `rbac.create` | `true` |
   | `rbac.clusterRole` | `cluster-admin` |
   | `serviceAccount.create` | `true` |
   | `serviceAccount.name` | `""`: the release's full name |
   | `resources`, `nodeSelector`, `tolerations`, `affinity`, `podAnnotations`, `podLabels` | empty |

3. **What is made:** a ServiceAccount (when `serviceAccount.create`); a ClusterRoleBinding of it to the ClusterRole `rbac.clusterRole` (when `rbac.create`); a ConfigMap with one entry, `authorized_keys`, holding `authorizedKeys`; a Deployment; a Service (when `ssh.enabled`); a PersistentVolumeClaim (when `state.persistence.enabled`). Names come from the usual `kosh.fullname` helper; labels are the usual `app.kubernetes.io/*` ones.
4. **The Deployment** has `replicas: 1` and `strategy: {type: Recreate}`, and neither is a value: two kosh with one key evict each other at the relay. A comment says so.
5. **The container** runs `serve` with `--ssh-listen=:<ssh.port>` or `--ssh-listen=` when SSH is off; `--iroh=<bool>`; `--iroh-relay`, `--iroh-mdns`; `--iroh-bind=0.0.0.0:<iroh.port>` only when `iroh.port` is set; `--state-dir=/var/lib/kosh`; `--authorized-keys=/etc/kosh/authorized_keys`. It has a container port `ssh` (TCP) when SSH is on and `iroh` (UDP) when `iroh.port` is set.
6. **Security:** pod `runAsNonRoot: true`, `runAsUser`, `runAsGroup` and `fsGroup` 65532, `seccompProfile: RuntimeDefault`; container `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, `capabilities: {drop: [ALL]}`.
7. **The key list** is mounted as the ConfigMap's whole volume at `/etc/kosh`, read-only, and **not with `subPath`**: a `subPath` mount never sees the ConfigMap change, and kosh follows the file. The pod has no annotation with the ConfigMap's checksum: a changed list must not restart kosh.
8. **The state** is mounted at `/var/lib/kosh`: the Secret `state.existingSecret`, read-only, with `defaultMode: 0440`; or the claim. The Secret holds the two files of spec §7, `iroh.key` and `ssh_host_ed25519_key`.
9. **Probes:** when SSH is on, a readiness and a liveness probe that connect to the `ssh` port over TCP. When it is off, none.
10. **Host network:** `iroh.hostNetwork: true` sets `hostNetwork: true` and `dnsPolicy: ClusterFirstWithHostNet`.
11. **The Service** has the type, port, annotations and, for a `LoadBalancer`, the source ranges and `externalTrafficPolicy` of the values (the policy also for `NodePort`; not for `ClusterIP`, where the API refuses it). Its target is the `ssh` port. It carries nothing of iroh.
12. **The chart refuses to render**, with `fail` and a message that says what to set:
    - neither `state.existingSecret` nor `state.persistence.enabled`: `kosh needs its state: set state.existingSecret or state.persistence.enabled, or its endpoint ID and host key change at every start`;
    - both of them;
    - `ssh.enabled` and `iroh.enabled` both false;
    - `iroh.enabled` with `iroh.relay` and `iroh.mdns` both false.
13. **`NOTES.txt`** says how to learn the endpoint ID (`kubectl logs`, or `kosh id` on the state directory), how to connect by `ssh` for the Service's type, and by `iroh-ssh kosh@<endpoint ID>`, and that `authorizedKeys` is the list of who may.

## Tests

`scripts/test-chart.sh` runs from the repository's root, exits non-zero at the first failure, and prints what it checks. It uses `helm lint`, `helm template`, `kubeconform -strict -summary` for the Kubernetes schemas, and `yq` to look into the rendered objects.

The cases, each a values file in `charts/kosh/ci/`:

| Case | Values | Checked |
|---|---|---|
| `secret` | `state.existingSecret: kosh-state` | lints; conforms; the Service is `ClusterIP` on port 22 to `ssh`; the Deployment has one replica, `Recreate`, the security settings of 6, both probes, the Secret mounted read-only at `/var/lib/kosh`, the ConfigMap at `/etc/kosh` without `subPath`; the ClusterRoleBinding names `cluster-admin`; no PVC |
| `loadbalancer` | the above and `service.type: LoadBalancer`, annotations, two source ranges, `externalTrafficPolicy: Local` | the Service has all four |
| `persistence` | `state.persistence.enabled: true`, a storage class | a PVC of 16Mi with the class, mounted read-write; no Secret volume |
| `hostnetwork` | Secret state, `iroh.hostNetwork: true`, `iroh.port: 4436` | `hostNetwork`, the DNS policy, `--iroh-bind=0.0.0.0:4436`, a UDP container port |
| `no-ssh` | Secret state, `ssh.enabled: false` | no Service, no probes, `--ssh-listen=` |
| `no-iroh` | Secret state, `iroh.enabled: false` | `--iroh=false` |
| `keys` | Secret state, `authorizedKeys` of two lines, one `read-only` | the ConfigMap's `authorized_keys` is exactly that text |
| `narrow-rbac` | Secret state, `rbac.clusterRole: view` | the binding names `view` |

And the four refusals of 12, each run with `helm template` and expected to fail with its message on standard error.

## Steps

- [ ] Write `scripts/test-chart.sh` with the first case and see it fail for want of a chart.
- [ ] Write the chart until the `secret` case passes. Commit: `Chart: kosh as a Deployment, with its state from a Secret`.
- [ ] Add the other cases one at a time, each failing first, then passing. Commit: `Chart: the load balancer, persistence, the host network, and the transports switched off`.
- [ ] The refusals. Commit: `Chart: an install without state, or without a way in, is refused`.
- [ ] `scripts/test-chart.sh` passes from a clean checkout. Nothing it wrote is left in the tree.

## Report

What you built and the script's output; any value of spec §15 you could not give the meaning it has there, and why.
