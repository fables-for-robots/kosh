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
                                      <ctrl-w> Wide           <?> Help          <s> Shell

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
  ghcr.io/fables-for-robots/kosh:v0.1.0 id --state-dir /state
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
helm install kosh oci://ghcr.io/fables-for-robots/charts/kosh --version 0.1.0 \
  --namespace kosh \
  --set state.existingSecret=kosh-state \
  --set-file authorizedKeys=authorized_keys
```

A release publishes the image as `ghcr.io/fables-for-robots/kosh:vX.Y.Z`
and the chart as `oci://ghcr.io/fables-for-robots/charts/kosh` in version
`X.Y.Z`; the chart runs the image of its own release unless `image.tag`
says otherwise. From a checkout, `./charts/kosh` installs the same way.

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
seen on the host network of a kind node: found by mDNS from the same
network, and, with the relays on, found through its published record
and reached directly.

## Keys

`authorizedKeys` is the text of an OpenSSH `authorized_keys` file: one key
to a line, the comment after the key is its name, and the name says what
the key may do.

```
ssh-ed25519 AAAAC3Nza... alice@laptop/admin
ssh-ed25519 AAAAC3Nza... bob@laptop
```

- **A name that ends in `/admin` is an admin's**, who may do everything
  kosh does. Every other key is read-only. The suffix is exactly that, in
  lower case, at the end of the name: `mic92/key-1/admin` is an admin,
  `bob@laptop`, `bob-admin` and `admin` are read-only, and `bob/Admin` is
  rejected as an admin written wrongly. The header shows the name, so
  `Key: alice@laptop/admin` says what the session may do, and so does
  every line of the log, which names the key.
- A read-only key has no action: the keys that change the cluster are
  not bound for it, and it opens no shell. It sees no Secret's contents:
  the table of secrets is there, with names, types and the number of
  entries, and YAML, describe and decode on a secret say `this key is
  read-only`. It sees everything else the ServiceAccount may read, the
  environment of pods and the data of ConfigMaps included.
- A line with an option, such as `no-pty` or `restrict`, or one that does
  not parse, gives no access. So does a line with `read-only` in front,
  the mark of a read-only key in kosh before 0.3.0: a list written for
  those versions is read-only throughout once its keys lose the mark,
  until the admins' names get the suffix, and kosh warns `key list has no
  admin: every session is read-only` while that is so.
- kosh follows the list without a restart: change the value with `helm
  upgrade`, and once the kubelet has updated the mounted file (up to a
  minute or two), a session whose key left the list ends with `this key
  is no longer accepted`, and one whose key changed between admin and
  read-only, by its name, ends so that it reconnects with its new rights.
  The connection of a key that left the list is left for its client to
  close, and kosh closes it a second after its last session ended.
- A certificate is not accepted.

## What a key can do

An admin's key can do whatever kosh's ServiceAccount can, and by default
that is bound to `cluster-admin`: an admin of kosh is a cluster
administrator. The ClusterRole it is bound to is `rbac.clusterRole`; to
make it less, name a narrower one:

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
level=INFO msg=action key=alice@kosh-e2e/admin fingerprint=SHA256:... action=scale object=deployments/default/web replicas=3
```

A shell in a pod is an action: admins only, into whatever container
the ServiceAccount's role allows. A narrower role needs `get` and
`create` on `pods/exec` for it; without them the API refuses, and kosh
says so. For the CPU and memory of [Metrics](#metrics) it needs `get`
and `list` on `pods` and `nodes` in the group `metrics.k8s.io`. kosh logs
who opened a shell in what, with which command, and how and when it
ended:

```
level=INFO msg=action key=alice@kosh-e2e/admin fingerprint=SHA256:... action=exec object=pods/default/marker phase=start container=marker command="\"sh\" \"-c\" \"command -v bash >/dev/null && exec bash || exec sh\""
level=INFO msg=action key=alice@kosh-e2e/admin fingerprint=SHA256:... action=exec object=pods/default/marker phase=end container=marker duration=1m29.686359728s started=true status=0
```

What is typed in a shell, and what it prints, is never logged, and
nothing of it is recorded.

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
| `s` | a shell in the pod, or in the container of the row |
| `0`–`9` | namespaces: `0` all, `1` default, the others the last used |

`?` lists the rest. Where kosh differs from k9s: edit is built in, copy
goes to the clipboard of your own machine through the terminal, and kill
asks first.

**A shell.** `s` on a pod, or on a container in a pod's containers,
opens a dialog with the container and the command:

```
╭─────────────────────────── Shell ────────────────────────────╮
│                                                              │
│ A shell in pod default/marker                                │
│                                                              │
│   Container  marker                                          │
│ ▸ Command    nd -v bash >/dev/null && exec bash || exec sh"  │
│                                                              │
│                      Cancel      Shell                       │
│                                                              │
╰──────────────────────────────────────────────────────────────╯
```

`enter` starts it: `s`, `enter` is the whole way in. The command is
`sh -c "command -v bash >/dev/null && exec bash || exec sh"`, bash where
the image has it and sh otherwise; change it in the field when the image
has another shell than `sh`: `/bin/bash` for an image with only bash,
`/busybox/sh`, `nu`. When it cannot be started, the dialog comes back with
the container runtime's reason (`exec: "sh": executable file not found in
$PATH`). An image with no shell at all, as kosh's own, cannot be helped
by the field. The command is words,
as a shell cuts a simple command, quotes and all, and is not run through
a shell: for a shell's language write `sh -c '…'`, as in
`sh -c 'cd /tmp && exec sh'`. The shell has your terminal as `kubectl
exec -it` would give it, full-screen programs and ctrl-c included;
`exit` or ctrl-d brings kosh back, on the table you left. A status other
than 0 is said in the flash.

The editor opens objects of up to 1,000 lines and 48 KiB; for a larger
one use `kubectl edit`. When the API refuses an edit as a conflict (the
object changed meanwhile), close the editor and open the object again.
Marks and actions go by namespace and name. The PODS column of the nodes'
table is a node's capacity.

## Metrics

When the cluster has
[metrics-server](https://github.com/kubernetes-sigs/metrics-server),
kosh shows the CPU and memory it measures, as k9s does, read every 15
seconds:

```
Key:     alice@laptop   <0> all       <shift-n> Sort Name     <ctrl-z> Faults   <d> Describe
Cluster: v1.33.1        <1> default   <shift-a> Sort Age      </> Filter        <y> YAML
kosh:    v0.4.0                       <shift-s> Sort Status   <space> Mark      <l> Logs
CPU:     12% ↑                        <shift-o> Sort Column   <shift-j> Owner   <p> Logs Previous
MEM:     47%                          <ctrl-w> Wide           <?> Help          <s> Shell
```

- **The header** has the lines `CPU:` and `MEM:`: what the nodes use
  over what they can give (their allocatable), summed over the nodes, in
  whole percents, with `↑` or `↓` when it rose or fell since the last reading
  that worked.
- **The pods table** and **a pod's containers** have `CPU` (millicores),
  `%CPU/R` and `%CPU/L` (that in percent of the requests and of the
  limits), `MEM` (mebibytes), `%MEM/R` and `%MEM/L`, in k9s's places.
  The wide view of pods (`ctrl-w`) shows the requests and limits
  themselves, `CPU/RL` and `MEM/RL`, as before.
- **The nodes table** has `CPU` and `MEM`, what is used; `CPU/A` and
  `MEM/A`, what is allocatable; and `%CPU` and `%MEM`, the one in percent
  of the other.

They sort as numbers with `shift-o`, as every numeric column does.

The columns and the lines are there only while the cluster serves the
metrics API (`metrics.k8s.io`); without metrics-server, the tables and
the header have none of them. kosh looks for the API at start
and every 5 minutes, so the columns and the lines appear within 5 minutes
of metrics-server's installation, and go within 5 minutes of its
removal, in sessions already open.

`n/a` where there is no number to show:

- in all six cells of a pod or a container that the last reading did not
  cover: a pod that is new or pending, an init container that ended, an
  ephemeral container; in `CPU`, `%CPU`, `MEM` and `%MEM` of a node it
  did not cover (`CPU/A` and `MEM/A` are the node's own);
- in a percentage of requests or limits that are 0;
- in every cell and in both lines while the API is served and the
  reading fails: metrics-server is down, its APIService is not
  available, or kosh's ServiceAccount may not read it. kosh's log says
  `metrics could not be read` once, with why, and `metrics can be read
  again` when a reading works again;
- in the lines until the first reading has come.

kosh reads the metrics with its ServiceAccount: a role narrower than
`cluster-admin` needs `get` and `list` on `pods` and `nodes` in the group
`metrics.k8s.io`. metrics-server's own manifest adds those to `view`,
`edit` and `admin`.

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

- Attach, copying files.
- Port-forward.
- Pulses and xray; k9s's warnings when the cluster's CPU or memory passes
  a level.

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
- **What runs in a shell can outlive the shell's session.** When a
  session ends while its shell is open (the key leaves the list, the
  connection is lost, kosh restarts), the shell and what it started can
  go on running in the container, without a terminal, until they end or
  the container does, as with `kubectl exec` when its client goes away:
  the API offers no way to end them. The shell's end line in kosh's log
  then says `ended="the session ended"`, with the pod and the container,
  so that whoever removes a key knows where to look.
- **A terminal that sends kosh 64 KiB that are no key, no paste and no
  report loses its session**, with a message that says so. No terminal
  we know does; it is the limit on what a client's input can make kosh
  hold. A paste into kosh's own UI is cut at 256 KiB. Neither applies to
  what is typed or pasted in a shell.
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
alone. Both need Linux, Docker, Nix and the network. `scripts/test-e2e.sh`
installs metrics-server in its cluster; `KOSH_E2E_SLOW=1` adds the test
that takes the metrics API away and back, up to 12 minutes, and
`KOSH_E2E_RUN=<regexp>` runs only the tests it matches.

The design is in [`docs/superpowers/specs/`](docs/superpowers/specs/),
the plan of the first stage in [`docs/superpowers/plans/`](docs/superpowers/plans/).

## Licence

kosh is free software under the BSD 3-Clause License (`BSD-3-Clause`);
see [`LICENSE`](LICENSE). Its UI follows k9s, by the Authors of K9s, under
the Apache License 2.0; [`NOTICE`](NOTICE) says where.
