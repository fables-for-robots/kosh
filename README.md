# kosh

kosh runs in a Kubernetes cluster and gives whoever holds an accepted SSH
key a terminal UI for that cluster that behaves like
[k9s](https://github.com/derailed/k9s). Nobody who uses it needs a
kubeconfig, a VPN or a client of ours: they connect with `ssh`, or with
[`iroh-ssh`](https://github.com/rustonbsd/iroh-ssh) when the cluster
offers no address they can reach. One list of keys says who gets in, and
whether each may change the cluster or only look.

```
Key:     alice@laptop   <0> all       <shift-n> Sort Name     <ctrl-z> Faults   <d> Describe
Cluster: v1.33.1        <1> default   <shift-a> Sort Age      </> Filter        <y> YAML
kosh:    v0.1.0                       <shift-s> Sort Status   <space> Mark      <l> Logs
                                      <shift-o> Sort Column   <shift-j> Owner   <p> Logs Previous
                                      <ctrl-w> Wide           <?> Help          <ctrl-d> Delete

┌──────────────────────────────────────── Pods(default)[3] ────────────────────────────────────────┐
│NAME↑                READY  STATUS   RESTARTS  AGE                                                │
│db-0                 1/1    Pending         0   2m                                                │
│web-7d9f8b6c4-2xkqp  1/1    Running         0   3h                                                │
│web-7d9f8b6c4-9zt4m  1/1    Running         0   3h                                                │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
│                                                                                                  │
└──────────────────────────────────────────────────────────────────────────────────────────────────┘
<pods>
```

## Install

kosh keeps two keys: the iroh key, whose public half is the endpoint ID
that `iroh-ssh` users dial, and the SSH host key. Make them once, with
`kosh id` from the image, and keep them in a Secret, so that they stay
the same whatever happens to the pod:

```sh
mkdir kosh-state
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/kosh-state:/state" \
  ghcr.io/fables-for-robots/kosh:VERSION id --state-dir /state
kubectl create namespace kosh
kubectl --namespace kosh create secret generic kosh-state --from-file=kosh-state
```

`kosh id` prints what you hand to users:

```
endpoint ID   df698fcfd50f40b4d9dd9b30bbb74c740e649d594a796cb2e6f79302a53f0ad5
iroh-ssh      iroh-ssh kosh@df698fcfd50f40b4d9dd9b30bbb74c740e649d594a796cb2e6f79302a53f0ad5
host key      ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
fingerprint   SHA256:CxAqTUSbcg7iwVGHzjZmC80WrjnMkEzvBL3oItc3qQs
```

Then install the chart, with the list of keys that may connect
(`authorized_keys` below, in the format of [Keys](#keys)):

```sh
helm install kosh ./charts/kosh --namespace kosh \
  --set image.tag=VERSION \
  --set state.existingSecret=kosh-state \
  --set-file authorizedKeys=authorized_keys
```

A release publishes the image as `ghcr.io/fables-for-robots/kosh` and the
chart as `oci://ghcr.io/fables-for-robots/charts/kosh`, both with the
release's tag as their version.

kosh also logs its endpoint ID, once it serves:

```sh
kubectl --namespace kosh logs deploy/kosh | grep 'kosh is serving'
```

## Connect

`ssh` checks kosh's host key as it checks any other's. Put the `host key`
line `kosh id` printed into `~/.ssh/known_hosts`, once under the name of
each way in: `[localhost]:2222` for the port-forward below, and the
endpoint ID for `iroh-ssh`, which hands `ssh` the endpoint ID as the
host's name:

```
[localhost]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
df698fcfd50f40b4d9dd9b30bbb74c740e649d594a796cb2e6f79302a53f0ad5 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
```

**With `ssh`.** The Service is inside the cluster; a port-forward is
enough for a first look:

```sh
kubectl --namespace kosh port-forward service/kosh 2222:22
ssh -t -p 2222 localhost
```

`--set service.type=LoadBalancer` exposes it through a load balancer
(`service.loadBalancerSourceRanges` says who may reach it), and `ssh -t
<its address>` connects; its `known_hosts` line names that address.

**With `iroh-ssh`**, by the endpoint ID alone:

```sh
iroh-ssh kosh@<endpoint ID>
```

kosh reaches out to iroh's relays and publishes where it is under its
endpoint ID; `iroh-ssh` finds it there and connects, directly when it
can and through a relay when it cannot. There is no address to expose
and nothing to open in a firewall: kosh needs outgoing traffic only. The
SSH host key is checked by `ssh` as always, inside the iroh connection.
The record that lets `iroh-ssh` find kosh by its endpoint ID lists kosh's
direct addresses, private ones included, in public DNS; the endpoint ID is
what authenticates kosh, and an address is of use only to whoever can
reach it.

The released `iroh-ssh` (0.2.12) finds kosh through the relays. A build
of `iroh-ssh` from its repository also finds it by mDNS on the same
network, without the relays, when kosh runs on the host's network
(`--set iroh.hostNetwork=true`). That puts the pod on the node's network:
the SSH port then listens on every address of the node, whatever
`service.type` is, and the pod is outside every NetworkPolicy. kosh was
seen on the host network of a kind node, found by mDNS from the same
network and reached through the relays.

## Keys

`authorizedKeys` is the text of an OpenSSH `authorized_keys` file: one key
to a line, the comment after the key is its name.

```
ssh-ed25519 AAAAC3Nza... alice@laptop
read-only ssh-ed25519 AAAAC3Nza... bob@laptop
```

- `read-only` in front makes a key read-only. It is the one option kosh
  knows: a line with any other option, or one that does not parse, gives
  no access.
- A read-only key has no action: the keys that change the cluster are
  not bound for it. It sees no Secret's contents: the table of secrets
  is there, with names, types and the number of entries, and YAML,
  describe and decode on a secret say `this key is read-only`. It sees
  everything else the ServiceAccount may read, the environment of pods
  and the data of ConfigMaps included.
- kosh follows the list without a restart: change the value with `helm
  upgrade`, and once the kubelet has updated the mounted file (up to a
  minute or two), a session whose key left the list ends with `this key
  is no longer accepted`, and one whose key changed between full and
  read-only ends so that it reconnects with its new rights.
- A certificate is not accepted. A comment may not begin with
  `read-only`: `ssh-ed25519 AAAA... read-only bob` is a read-only key
  written wrongly, and the line is rejected.

## What a key can do

A full key can do whatever kosh's ServiceAccount can, and by default that
is bound to `cluster-admin`: a full key is a cluster administrator. The
ClusterRole it is bound to is `rbac.clusterRole`; to make it less, name a
narrower one:

```sh
helm upgrade kosh ./charts/kosh --namespace kosh --reuse-values --set rbac.clusterRole=view
```

Where the ServiceAccount lacks a right, kosh shows the API's refusal.
With a Role bound in one namespace instead of a ClusterRole, the tables of
the kinds kosh knows show `forbidden`, since kosh lists them across the
cluster. An action under way ends when its session does.
Kubernetes' audit log names kosh's ServiceAccount for everything kosh
does; kosh's own log names the key, with a line for every action:

```
level=INFO msg=action key=alice@kosh-e2e fingerprint=SHA256:... action=scale object=deployments/default/web replicas=3
```

## Using it

| Key | |
|---|---|
| `:` | go to a kind by one of its names: `:deploy`, `:ns`, `:po kube-system`; `:q` ends the session |
| `/` | filter the rows |
| `enter` | go into the row: a deployment's pods, a pod's containers, a namespace's pods |
| `esc` | back |
| `?` | every key of the view |
| `d` `y` `l` | describe, YAML, logs |
| `e` | edit |
| `ctrl-d` `ctrl-k` | delete, kill |
| `0`–`9` | namespaces: `0` all, `1` default, the others the last used |

`?` lists the rest. Where kosh differs from k9s: edit is built in, copy
goes to the clipboard of your own machine through the terminal, and kill
asks first.

The editor opens objects of up to 1,000 lines and 48 KiB; for a larger
one use `kubectl edit`. When the API refuses an edit as a conflict (the
object changed meanwhile), close the editor and open the object again.
Marks and actions go by namespace and name. The PODS column of the nodes'
table is a node's capacity.

## Values

| Value | Default | |
|---|---|---|
| `image.repository`, `image.tag`, `image.pullPolicy` | `ghcr.io/fables-for-robots/kosh`, the chart's `appVersion`, `IfNotPresent` | |
| `imagePullSecrets` | none | |
| `authorizedKeys` | empty | the key list, as the text of the file |
| `ssh.enabled` | `true` | SSH over TCP; without it there is no Service |
| `ssh.port` | `2222` | the port in the container |
| `service.type` | `ClusterIP` | `LoadBalancer` exposes SSH through a load balancer; `NodePort` works too |
| `service.port` | `22` | the Service's port |
| `service.annotations` | none | for the load balancer of the cloud at hand |
| `service.loadBalancerSourceRanges` | none | who may reach the load balancer |
| `service.externalTrafficPolicy` | `Cluster` | `Local` keeps the client's address, which is then what kosh logs |
| `iroh.enabled` | `true` | SSH over iroh |
| `iroh.relay` | `true` | the relays, and the record that makes kosh reachable by its endpoint ID |
| `iroh.mdns` | `true` | announce on the local network by mDNS |
| `iroh.hostNetwork` | `false` | the pod on the node's network, for mDNS and direct connections: the SSH port listens on every address of the node, and no NetworkPolicy applies; only with `iroh.enabled` |
| `iroh.port` | unset | a fixed UDP port for the endpoint |
| `state.existingSecret` | unset | a Secret with `iroh.key` and `ssh_host_ed25519_key`, made by `kosh id` |
| `state.persistence.enabled`, `.size`, `.storageClass` | `false`, `16Mi`, the default class | a PersistentVolumeClaim instead, on which kosh makes its keys at first start |
| `rbac.create`, `rbac.clusterRole` | `true`, `cluster-admin` | the ClusterRole the ServiceAccount is bound to |
| `serviceAccount.create`, `serviceAccount.name` | `true`, the release's full name | |
| `resources`, `nodeSelector`, `tolerations`, `affinity`, `podAnnotations`, `podLabels` | none | |

The chart refuses to install without state (`state.existingSecret` or
`state.persistence.enabled`, not both) and with both `ssh.enabled` and
`iroh.enabled` off.

## Not yet

- A shell in a pod, attach, copying files.
- Port-forward.
- Metrics: the CPU and memory columns.

## Known issues

- **"Discovery produced no results" in the first minutes.** A connection
  made with `iroh-ssh` before kosh has published where it is (in the first
  minute or so after kosh starts) can fail with `Discovery produced no
  results`, and go on failing for as long as your DNS resolver caches the
  record it did not find, up to several minutes. Wait until the cache
  forgets, or point your machine at another DNS resolver, one that has not
  asked for the record yet.
- **A direct connection from an `iroh-ssh` built on iroh 1.x can be cut.**
  The iroh library kosh uses can misread a QUIC key update on a connection
  that uses several network paths, as a client on iroh 1.x does, and close
  the connection: `ssh` then says `Connection closed by UNKNOWN port 65535`.
  kosh no longer starts a key update early in a connection, which was the
  frequent case; one that the client starts, or a later regular one in a
  very long session, can in principle still cut a direct connection.
  Connect again.
- **A key's 17th connection is closed without a word.** A key has at most
  16 open connections; the next is closed without a message to the client
  (kosh's log says why). Many connections opened within a few seconds can
  meet it, since a connection whose client went away without closing it
  takes some seconds to be given up.

## Development

```sh
direnv allow                # or: nix develop .#bare
go test ./...
```

`direnv` enters the flake's development shell, which carries kosh built
by Nix besides the tools; the `bare` shell has the tools alone (Go, Helm,
kind, kubectl) and needs no evaluation option.

The tests that run a real API server need kube-apiserver and etcd, which
`scripts/envtest-assets.sh` fetches on first use:

```sh
KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)" go test ./...
```

`scripts/test-chart.sh` checks the chart. `scripts/test-e2e.sh` builds the
image and tests it in a kind cluster of its own, with `ssh` and
`iroh-ssh`; `scripts/test-mdns.sh` shows `iroh-ssh` finding kosh by mDNS
alone. Both need Linux, Docker, Nix and the network.

The design is in [`docs/superpowers/specs/`](docs/superpowers/specs/),
the plan of the first stage in [`docs/superpowers/plans/`](docs/superpowers/plans/).

## Licence

kosh is free software under the GNU Lesser General Public License,
version 3 only (`LGPL-3.0-only`); see [`LICENSE`](LICENSE) and
[`COPYING`](COPYING). Its UI follows k9s, by the Authors of K9s, under
the Apache License 2.0; [`NOTICE`](NOTICE) says where.
