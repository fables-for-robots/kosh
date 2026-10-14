# kosh stage 1: the front door, the browser and the actions

2026-10-13

## 1. Purpose

kosh is a service that is deployed into a Kubernetes cluster and gives
whoever holds an accepted SSH key a terminal UI that behaves like
[k9s](https://github.com/derailed/k9s), for the cluster kosh runs in.
Nobody who uses it needs a kubeconfig, a VPN or a client of ours: they
connect with `ssh`, or with
[`iroh-ssh`](https://github.com/rustonbsd/iroh-ssh) when the cluster
offers no address they can reach.

```
 ssh ──────── TCP ───────────────┐
                                 ├─▶ SSH server ─▶ key list ─▶ one TUI per session ─▶ Kubernetes API
 iroh-ssh ─── iroh, /iroh/ssh ───┘   (wish v2)                 (Bubble Tea v2)         (ServiceAccount)
```

kosh is built in stages, each with a design, a plan and a build of its
own. This document is stage 1:

- **The front door.** SSH over TCP and SSH over iroh, one list of
  accepted keys for both, full keys and read-only keys.
- **The browser.** k9s's navigation over every resource type of the
  cluster, with k9s's own tables for 27 kinds, describe, YAML and logs.
- **The actions.** Delete, kill, edit, scale, restart, set image, roll
  back, trigger and suspend a CronJob, cordon, uncordon and drain.
- **The deployment.** An image and a Helm chart.

Stage 1 is done when, on a cluster kosh was installed in with its chart,
`ssh` and `iroh-ssh` both land in the TUI with a key from the list, a
full key can do every action of §11, and a read-only key can do none of
them and read no Secret.

What later stages hold is in §22.

## 2. Terms

- **Key**: an SSH public key of a user. The **key list** (§6) is the
  file of accepted keys.
- **Grant**: what the key list says about one key: its name, and whether
  it is **full** or **read-only**.
- **Session**: one SSH session channel with a terminal, and the TUI that
  runs on it.
- **Endpoint ID**: the public half of kosh's iroh key, 64 hex
  characters. It is what an `iroh-ssh` user dials.
- **Host key**: the SSH host key kosh proves itself with to an SSH
  client.
- **Kind**: a resource type of the cluster. A **typed kind** is one of
  the 27 that kosh has a renderer for (§9); every other one is
  **generic**.
- **Action**: something a session does that changes the cluster (§11).
- **The reference commit**: k9s at `8f29fba` (2026-10-11). "As k9s" in
  this document means as k9s at that commit.

## 3. The process

kosh is one program and, in the pod, one process: `kosh serve`. It
starts no other process, for any purpose. Everything a session sees is
drawn by kosh itself and everything it does is a call to the Kubernetes
API with the pod's ServiceAccount.

Both transports end in the same SSH server. What comes after the SSH
handshake does not know which transport carried it, apart from what the
log says of the connection (§13).

Every session acts with the same identity towards Kubernetes: the
ServiceAccount. What a session may do is bounded twice: by the RBAC of
that account, and by its grant (§12). Kubernetes' audit log therefore
names kosh and not the person; kosh's own log names the key (§13).

## 4. Transports

### 4.1 TCP

kosh listens on `--ssh-listen` (default `:2222`) and gives every
connection to the SSH server. The empty string switches this transport
off.

### 4.2 iroh

kosh binds one iroh endpoint with its iroh key (§7) and accepts
connections for the ALPN `/iroh/ssh`, which is `iroh-ssh`'s. That
protocol has no handshake of its own: the client opens one bidirectional
stream and SSH runs in it.

- For every accepted connection kosh waits for one stream, 30 seconds at
  most, and gives it to the SSH server as a connection
  (`Conn.AcceptStreamConn`, then `Server.HandleConn`). The SSH client
  speaks first, which is what makes the stream visible to kosh.
- When the SSH connection ends kosh closes the iroh connection, after
  giving the client up to 2 seconds to close it first, so that the last
  bytes are not cut off. Further streams of the same connection are not
  accepted.
- `--iroh=false` switches this transport off. With both transports off
  kosh refuses to start.

**How a client finds the endpoint.** `iroh-ssh` looks an endpoint ID up
through number0's DNS, the mainline DHT and mDNS. kosh announces itself
two ways, each with a flag that is on by default:

- `--iroh-relay`: the default relays, and a pkarr record with number0's
  DNS that follows the endpoint's addresses. This is what makes kosh
  reachable from anywhere, through a relay or directly when a path can
  be made.
- `--iroh-mdns`: mDNS on the local network, under `_irohv1._udp.local.`,
  the service name `iroh-ssh` asks for, by an announcer of kosh's own:
  the one go-iroh has is not found by `iroh-ssh` (§20). Of `iroh-ssh`,
  only a build from its repository looks anything up by mDNS at the
  time of writing; the released 0.2.12 does not.

Neither announces the address of a tunnel: only addresses of interfaces
that are up, broadcast, multicast and not point-to-point. A tunnel's
address in an announcement was seen to make connections time out.

With `--iroh` on, at least one of the two has to be on. kosh does not
publish to the mainline DHT.

`--iroh-bind` is the UDP address the endpoint binds; the default is
every address and a port the system picks.

mDNS reaches the local network only from a pod on the host network
(§15). On the pod network it is harmless and finds nobody.

The package that does this is kosh's own, modelled on the `node` package
of jaccard-store, which kosh does not import: its module brings a
dependency tree kosh has no other use for.

## 5. The SSH server

The server is `charm.land/wish/v2` over `charm.land/ssh`: one Bubble
Tea v2 program per session, on the session's terminal, in kosh's
process. kosh runs the program itself, with the options wish makes for a
session, and not through wish's Bubble Tea middleware: the middleware
drops the program's error, which is how a panic is known, and leaves the
program listening for the process's signals.

- **Authentication** is by public key and nothing else. A key is
  accepted when the key list holds it at that moment. The user name is
  not looked at: `iroh-ssh` asks for one, and any will do.
- **A session needs a terminal.** A session without one is told
  `kosh needs a terminal: connect with ssh -t` and ended with status 1.
- **No commands.** A session that asks for a command (`ssh host cmd`) is
  told `kosh runs no commands` and ended with status 1.
- **Nothing else.** No port forwarding in either direction, no agent
  forwarding, no subsystems. Environment requests are accepted; only
  what tells the terminal's type and colours is used.
- **The handshake has a deadline** of 30 seconds, and at most 64
  connections may be in it at once, over both transports together; one
  more is closed at once.
- **Keepalive.** kosh sends an SSH keepalive request on every connection
  every 30 seconds. After three in a row without an answer it closes the
  connection, and with it the sessions and what they had open.
- **A connection without a session is closed** after 5 seconds, counted
  from its authentication and from the end of its last session. A client
  of `iroh-ssh` that exits often sends no close, and the connection
  would otherwise linger until QUIC's idle timeout.
- **A key may have 16 connections open at once, and a connection 10
  session channels.** More are refused. The handshake's limit bounds
  peers that have not authenticated; these two bound what one key, a
  read-only one included, can make kosh hold: every channel lets its
  client send two megabytes that nothing reads.
- **Twenty tries to authenticate**, not the library's six: `iroh-ssh`
  lets `ssh` offer every key of the user's agent before the one it was
  given.
- **No idle limit for a session.** A session that only shows a log may
  stay.

## 6. The key list

`--authorized-keys` (default `/etc/kosh/authorized_keys`) names a file
in OpenSSH's `authorized_keys` format: one key to a line, a comment after
the key, which is the key's name and says what it may do. Empty lines and
lines that begin with `#` are skipped.

```
ssh-ed25519 AAAAC3Nza...  dragan@mac/admin
ssh-ed25519 AAAAC3Nza...  guest@laptop
```

*(As of 0.3.0. Until then a key was full unless it had the option
`read-only` in front; the rule below replaced that, on the user's
request, so that the list and every log line say what a key may do.)*

- **A name that ends in `/admin`** is an admin's: the key may do
  everything kosh does. Every other key is read-only. The suffix is
  exactly `/admin`, in lower case, at the end of the name; a name that
  ends in it in another case (`/Admin`) rejects the line, as an admin
  written wrongly, rather than making a read-only key that nobody
  notices. Anything else (`bob-admin`, `admin`, `bob/admin/`) is a
  read-only key.
- **Any option rejects the line**, and so does a line that does not
  parse. The option `read-only` of earlier versions is rejected with its
  own words, so that a list written for them is not read in silence as
  something else: a key without the suffix is read-only whatever a line
  says. A list of keys none of which is an admin is accepted, with a
  warning in the log that every session is read-only.
- **The name** of a key is its comment, or its SHA256 fingerprint when
  it has none (such a key is read-only).
- **A key that is listed twice** is read-only unless every line names it
  an admin, and has the name of its first line.
- Every key type `golang.org/x/crypto/ssh` reads is accepted, **except
  certificates**: kosh checks no certificate's validity, principals or
  authority, so a line that holds one is rejected.

**Reloading.** kosh reads the file again every 2 seconds and takes it
over when its content has changed. That follows the way a ConfigMap is
updated in a pod. When the list changes:

- a session whose key is no longer listed is closed, with a line that
  says so;
- a session whose key went from admin to read-only or back, by its
  name, is closed the same way: the user connects again and gets the new
  grant.

**When the file cannot be read.** At start that is an error and kosh
exits. Later it means an empty list: nobody is accepted and every
session is closed, until the file can be read again. A file that is
readable and holds no key is accepted at start, with a warning.

## 7. Identity and state

`--state-dir` (default `/var/lib/kosh`) holds two files:

| File | What it is | Format |
|---|---|---|
| `iroh.key` | the iroh key, whose public half is the endpoint ID | 64 hex characters and a line feed, as the key files of the sibling projects |
| `ssh_host_ed25519_key` | the SSH host key | an OpenSSH private key |

The two keys are separate. On the iroh path the endpoint ID already
proves who answers, so nothing is gained by making them one, and each
protocol keeps a key of its own.

A file that is missing is created, mode 0600, and the directory with
0700. A file that is there and cannot be parsed is an error and is left
as it is: writing a new key over it would silently change the identity.
A directory that cannot be written to and lacks a file is an error.

`kosh id --state-dir DIR` does the same loading and creating and prints
(the values are shortened here):

```
endpoint ID   11bdf843…c08d02
iroh-ssh      iroh-ssh kosh@11bdf843…c08d02
host key      ssh-ed25519 AAAAC3Nza…
fingerprint   SHA256:…
```

It is how the Secret of §15 is prepared, and how an operator learns what
to hand to users.

Two endpoints with one key evict each other at the relay, so kosh runs
as exactly one replica (§15).

## 8. Cluster data

kosh reads the cluster in four ways.

### 8.1 Informers, for the typed kinds

Each typed kind has one informer for the whole cluster and the whole
process: a shared informer over client-go's dynamic client, which turns
each object into its typed form as it stores it. One informer per kind,
and not a factory of typed ones, is what lets a single kind be stopped.

- An informer is started the first time a session opens its kind, and
  all sessions share it. That is sound because all sessions have the
  same identity towards the API.
- An informer that no view has used for 10 minutes is stopped and its
  cache dropped. Secrets, for one, are then not held in memory for as
  long as kosh runs.
- `managedFields` are dropped from what the cache holds.
- The cache is of all namespaces. A view filters by the session's
  namespace.

This keeps every object of a kind that is being looked at in memory.
That suits clusters of some thousands of pods. Informers per namespace,
for clusters far beyond that, are not in this stage.

"Containers" is not a resource: its rows are the init containers,
containers and ephemeral containers of one pod, from the pods' informer.

### 8.2 Tables, for the generic kinds

Every other resource, custom resources included, is listed in the API
server's Table format (`Accept: application/json;as=Table;v=v1;g=meta.k8s.io`),
which gives the columns `kubectl get` prints. The list is asked for
again every 2 seconds while a view shows it. A namespaced resource shown
for all namespaces gets a NAMESPACE column in front. This is what k9s
does for the kinds it has no renderer for.

### 8.3 Discovery

The list of the API's resources (name, short names, kind, group, whether
namespaced, verbs) is read at start, every 5 minutes, and once more
right away when a `:` command names something unknown, so that a custom
resource is reachable as soon as its definition exists.

A kind answers to its plural name, its singular name, its short names
and its kind in lower case (`pods`, `pod`, `po`), as in k9s, which takes
its names for resources from the same list. Beside them are the commands
that are no resource: `q`, `q!`, `qa` and `Q` to leave, `alias` and `a`
for the list of names, `h` and `?` for help.

### 8.4 Direct calls

- **YAML**: a fresh GET of the object, without `managedFields` unless
  the view's toggle asks for them.
- **Describe**: `k8s.io/kubectl/pkg/describe`, as k9s, so the text is
  that of `kubectl describe`, events included.
- **Logs**: the pod log API, followed. A log view starts with the last
  100 lines and keeps the last 5000, as k9s by default. The logs of a
  workload or a service are the logs of the pods its selector matches
  when the view opens, merged, at most 30 containers; when there are
  more the view says how many it left out.

### 8.5 When the API says no

A kind the ServiceAccount may not list shows the API's error where its
table would be. An informer that has lost its watch reconnects by
itself; while a view's data may be stale the view says so in its title.

## 9. Renderers

A renderer turns one object into one row. It is a pure function of the
object and the time: it returns the cells, the values the cells sort by
(a number as a number, an age as a time), and the row's **health**: ok,
pending, error or completed. It knows nothing of the terminal and
nothing of the API, and is tested with objects from fixtures against the
rows expected (§19).

**Columns.** For 24 of the kinds the columns, their order and which of
them are shown only wide are those of k9s's `internal/render` package at
the reference commit, less the columns stage 1 has no data for:

- the metrics columns (CPU, MEM and their percentages), which need
  metrics-server (§22);
- the GPU columns;
- VS, the vulnerability scan, and PF, the port-forward mark.

For three kinds k9s has no renderer and shows the server's table:
ingresses, events and HorizontalPodAutoscalers. kosh's renderers for
them have the columns of `kubectl get`, wide columns included.

Appendix A lists the columns of every kind.

**Colour.** A row's colour follows k9s's rules: by health (error,
pending, completed), and for 2 seconds after a row was added or
changed, the colours k9s gives those. What makes a row an error is the
renderer's business and follows k9s's (a pod that is not ready when it
should be, a deployment with fewer ready replicas than desired, and so
on); it is also what the VALID column says.

**Redrawing.** A table is drawn again when its cache has changed, at
most twice a second, and at least every 2 seconds so that ages move.

**Where the logic comes from.** kosh's renderers are written for kosh;
k9s's are the reference for what a column holds. Where kosh's code
follows k9s's closely the file says so, and a `NOTICE` file credits k9s
(Apache-2.0, which may be used in a program under the BSD licence).

## 10. The TUI

The TUI is Bubble Tea v2 with Lip Gloss v2, as great-machine's. It reads
and changes the cluster only through an interface of the `kube` package
(§17), so that it is tested against a fake (§19).

### 10.1 The screen

From top to bottom, as k9s:

- **Header.** The key's name, with `[read-only]` when it is; the
  cluster's version and kosh's; the namespace shortcuts; the keys of the
  current view. `ctrl-e` hides it.
- **Prompt.** One line, shown while `:` or `/` is being typed.
- **Body.** The current view in a frame, with its title: for a table
  the kind, the namespace, the number of rows and the filter, as
  `Pods(default)[12]`.
- **Crumbs.** The views that lead to this one.
- **Flash.** One line for what kosh has to say: an action done, an
  error.

The colours are those of k9s's stock skin, reduced to what the
terminal of the session can show. A terminal smaller than 80 by 12 gets
one line that says so, until it is larger.

A session starts in the pods of the namespace `default`, as k9s does
without a configuration. A table starts sorted by namespace and name;
the events start sorted by LAST SEEN, as in k9s. Nothing of a session is
kept when it ends.

### 10.2 Getting around

| Keys | |
|---|---|
| `:` | Go to a kind by one of its names (§8.3), with suggestions; a second word is the namespace (`:po kube-system`). `:q` and `ctrl-c` end the session. |
| `ctrl-a` | The list of names. |
| `/` | Filter the rows: a regular expression over the cells; `!` in front inverts it; `-l app=x` filters by a label selector. `esc` clears it. |
| `enter` | Go into the row (below). |
| `esc` | Back to the view before. |
| `[` `]` `-` | Back and forward in the history of `:` commands; the last one. |
| `0`–`9` | `0` is all namespaces, `1` is `default`, the others are the namespaces this session used last. |
| arrows, `j` `k`, `g` `G`, `ctrl-f` `ctrl-b`, page keys | Move. |
| `shift-n` `shift-a` `shift-s` `shift-o` | Sort by name, by age, by status, by the column the cursor is in; again to reverse. |
| `ctrl-w` | Wide: all columns. |
| `ctrl-z` | Only the rows that are errors. |
| `space` `ctrl-space` `ctrl-\` | Mark a row, mark a range, clear the marks. An action is for the marked rows when there are any. |
| `shift-j` | Go to the owner of the row. |
| `ctrl-r` | Read again now. |
| `?` | Every key of this view. |

**Where `enter` leads:**

| From | To |
|---|---|
| deployment, replicaset, statefulset, daemonset, job, service, node | its pods |
| cronjob | its jobs |
| pod | its containers |
| container | its log |
| namespace | the pods of that namespace, which becomes the session's |
| role, clusterrole, and a binding through its role | the rules, in the columns of k9s's rbac view |
| CRD | the instances of that resource |
| anything else | describe |

### 10.3 Text views

Describe (`d`) and YAML (`y`) open on any row. Both scroll, search with
`/` and step through the matches with `n` and `shift-n`, go full screen
with `f`, and copy their text with `c`. YAML is coloured; `r` turns
reading the object again every 2 seconds on and off, and `m` shows
`managedFields`.

On a secret, `x` opens its data decoded: every entry by its key, its
value as text. It is for full keys (§12).

### 10.4 Logs

`l` opens the log of a pod, a container, or the pods of a workload or a
service; `p` the log of the container's previous run.

| Key | |
|---|---|
| `s` | follow the end or stay where the view is |
| `w` | wrap lines |
| `t` | show the time of each line |
| `f` | full screen |
| `0`–`6` | from where: the end, the beginning, the last 1, 5, 15, 30 or 60 minutes |
| `/` | show only the lines that match |
| `m` | draw a line across, to mark where one stood |
| `shift-c` | clear |
| `a` | all containers of the pod |
| `c` | copy |

A line of a view over several containers begins with the pod and the
container it is from, each in a colour of its own.

### 10.5 Where kosh differs from k9s

- **Copy** goes to the clipboard of the user's own machine through the
  terminal (OSC 52, `tea.SetClipboard`). A terminal that does not do
  that copies nothing, and nothing says so.
- **No saving to a file** (`ctrl-s` in k9s's views): the file would be
  written in the pod.
- **Edit** is in kosh (§11.2); k9s starts `kubectl edit`.
- **Kill asks** (§11.1); k9s kills at once.

## 11. Actions

Only a full key has these keys. Each is logged with the key's name
(§13).

### 11.1 The actions and their keys

| Kind | Key | Action |
|---|---|---|
| any | `ctrl-d` | Delete. The dialog offers the propagation (background, foreground, orphan; background first, as k9s) and "force", which is a grace period of zero. |
| any | `e` | Edit (§11.2). |
| pod | `ctrl-k` | Kill: delete with a grace period of zero. |
| deployment, statefulset, replicaset | `s` | Scale: the dialog asks for the number of replicas. |
| deployment, statefulset, daemonset | `r` | Restart, as `kubectl rollout restart`. |
| deployment, statefulset, daemonset | `i` | Set image: the dialog has a field for each container. |
| replicaset | `ctrl-l` | Roll its deployment back to it, as `kubectl rollout undo --to-revision`. |
| cronjob | `t` | Trigger: create a job from its template now, as `kubectl create job --from`. |
| cronjob | `s` | Suspend or resume. |
| node | `c` `u` | Cordon; uncordon. |
| node | `r` | Drain. The dialog has k9s's fields: grace period, timeout, ignore DaemonSets, delete emptyDir data, force, disable eviction. It is done with `k8s.io/kubectl/pkg/drain`. |

**Every action asks first**, kill included. The dialog names what will
be done to what, and to how many when rows are marked. The answer that
changes nothing is the one the cursor starts on.

The outcome is said in the flash line. An error of the API is shown as
the API worded it, and for marked rows the action goes on to the next
row.

### 11.2 Edit

`e` opens the object's YAML, without `managedFields`, in an editor that
is part of kosh (the text area of `charm.land/bubbles/v2`).

- `ctrl-s` sends the text as an update of the object as it was read.
  When the API accepts it the editor closes. When it does not, because
  the YAML does not parse, the object is invalid, or someone changed it
  in the meantime, the error is shown and the text stays as it is.
- `esc` leaves without saving, and asks first when the text was changed.
- Text that was not changed is not sent.

## 12. Read-only keys

A read-only session differs in two things.

**It has no actions.** The keys of §11 are not bound for it: they are in
neither the hints nor the help, and pressing them does nothing.

**It sees no Secret's contents.** The table of secrets is there, with
names, types and the number of entries. YAML, describe, edit and `x` on
a secret are refused, with a line in the flash that says the key is
read-only. A Secret often holds a credential that can write, so a
read-only key that could read them would be read-only in name.

The rule is enforced in one place besides the keys: the session's handle
on the `kube` package (§17) carries the grant, and every call that
changes the cluster or returns a Secret's contents refuses a read-only
grant. A test goes through all of them (§19).

What a read-only key can still see is whatever else the ServiceAccount
may read, the environment of pods and the data of ConfigMaps included.

## 13. The log

kosh logs to standard error with `log/slog`, one line per event:

- a connection: transport, the remote address or the client's endpoint
  ID, and for a rejected one the reason;
- a session's start and end: the key's name and fingerprint, full or
  read-only, and how long it lasted;
- **every action**: the key's name, what was done to which object, with
  which options, and whether the API accepted it;
- a refusal of §12 that reached the `kube` package;
- the key list being reloaded: how many keys, which lines were rejected,
  how many sessions were closed;
- what the endpoint has to say about relays, mDNS and its addresses.

The log holds no Secret's contents and no text of an edit.

## 14. Commands and flags

The command line is `urfave/cli/v2`.

| Command | |
|---|---|
| `kosh serve` | the service |
| `kosh id` | print the endpoint ID and the host key of a state directory, creating what is missing (§7) |
| `kosh version` | print the version |

Flags of `kosh serve`, each also read from the environment variable
named:

| Flag | Variable | Default | |
|---|---|---|---|
| `--ssh-listen` | `KOSH_SSH_LISTEN` | `:2222` | TCP address of the SSH server; empty for none |
| `--iroh` | `KOSH_IROH` | `true` | accept SSH over iroh |
| `--iroh-bind` | `KOSH_IROH_BIND` | every address, any port | UDP address of the endpoint |
| `--iroh-relay` | `KOSH_IROH_RELAY` | `true` | relays and the pkarr record |
| `--iroh-mdns` | `KOSH_IROH_MDNS` | `true` | mDNS |
| `--state-dir` | `KOSH_STATE_DIR` | `/var/lib/kosh` | the two keys |
| `--authorized-keys` | `KOSH_AUTHORIZED_KEYS` | `/etc/kosh/authorized_keys` | the key list |
| `--kubeconfig` | `KUBECONFIG` | none | a kubeconfig instead of the pod's ServiceAccount, for running kosh outside a cluster |

`kosh id` takes `--state-dir`.

## 15. The Helm chart

The chart at `charts/kosh` is how kosh is deployed; the repository holds
no other manifests. Its `appVersion` is the image's tag.

**What it makes:** a ServiceAccount; a ClusterRoleBinding of it to a
ClusterRole; a ConfigMap with the key list, mounted as a directory at
`/etc/kosh`; a Deployment; a Service, when SSH over TCP is on; a
PersistentVolumeClaim, when asked for.

**The Deployment** has one replica and the strategy `Recreate`; neither
is a value, because two kosh with one key evict each other at the relay.
The container runs as a user that is not root, with a read-only root
file system, no capabilities and the runtime's default seccomp profile.
When SSH over TCP is on, the probes are a TCP connect to its port.

**Values:**

| Value | Default | |
|---|---|---|
| `image.repository`, `image.tag`, `image.pullPolicy` | `ghcr.io/fables-for-robots/kosh`, the `appVersion`, `IfNotPresent` | |
| `authorizedKeys` | empty | the key list, as the text of the file |
| `ssh.enabled` | `true` | SSH over TCP; without it there is no Service |
| `ssh.port` | `2222` | the port in the container |
| `service.type` | `ClusterIP` | `LoadBalancer` exposes SSH through a load balancer; `NodePort` works too |
| `service.port` | `22` | the Service's port |
| `service.annotations` | none | for the load balancer of the cloud at hand |
| `service.loadBalancerSourceRanges` | none | who may reach the load balancer |
| `service.externalTrafficPolicy` | `Cluster` | `Local` keeps the client's address, which is then what kosh logs |
| `iroh.enabled` | `true` | SSH over iroh |
| `iroh.relay`, `iroh.mdns` | `true` | as the flags of §14 |
| `iroh.hostNetwork` | `false` | the pod on the host's network, for mDNS and direct connections |
| `iroh.port` | unset | a fixed UDP port for the endpoint |
| `state.existingSecret` | unset | a Secret with the two files of §7, mounted read-only |
| `state.persistence.enabled`, `.size`, `.storageClass` | `false`, `16Mi`, unset | a PersistentVolumeClaim instead, on which kosh makes its keys at first start |
| `rbac.create`, `rbac.clusterRole` | `true`, `cluster-admin` | the ClusterRole the ServiceAccount is bound to |
| `serviceAccount.create`, `serviceAccount.name` | `true`, the release's | |
| `resources`, `nodeSelector`, `tolerations`, `affinity`, `podAnnotations` | none | |

**What the chart refuses to render:**

- neither `state.existingSecret` nor `state.persistence.enabled`, or
  both: without state the endpoint ID and the host key would be new at
  every start;
- `ssh.enabled` and `iroh.enabled` both false.

**What the defaults mean.** A fresh install exposes nothing: the Service
is inside the cluster, and the iroh path needs outgoing traffic only. A
load balancer is a choice somebody makes. It carries SSH and nothing of
iroh, which does not need one.

**`cluster-admin`.** A full key is a cluster administrator, and the
default says that plainly. The README shows how to bind a narrower
ClusterRole; kosh then shows the API's refusal wherever it lacks a right
(§8.5).

## 16. Build, image and releases

- **Module** `github.com/fables-for-robots/kosh`, packages at the top
  level of the module, none under `internal/`. Licence BSD-3-Clause
  (LGPL-3.0-only, as the siblings, until the first release).
- **Flake** as great-machine's: nixpkgs `nixos-26.05`, the program built
  by `draganm/gonixgo` without cgo, a development shell with Go, Helm,
  kind and kubectl. The kube-apiserver and etcd that the tests of §19
  start are not in the shell: `scripts/envtest-assets.sh` fetches them
  on first use, the same ones on Linux and on a Mac, where nixpkgs has
  no kube-apiserver. `.envrc` with `use flake`.
- **Image** `ghcr.io/fables-for-robots/kosh`, built by Nix: the program,
  the CA certificates (the relays and number0's DNS are reached over
  HTTPS), and a `passwd` with the user it runs as. No shell and no other
  program.
- **Workflows** as great-machine's. `test.yml`: the Go tests, the chart's
  checks, the Nix build, the image, the end-to-end test. `release.yml`,
  on a tag: the image for x86_64 and aarch64 under the tag, and the
  chart, with the tag as its version, to
  `oci://ghcr.io/fables-for-robots/charts/kosh`.

## 17. Packages

| Package | What it is |
|---|---|
| `cmd/kosh` | `main` |
| `cli` | the commands and flags, and the wiring of `serve` |
| `keys` | the key list: parsing, the grant of a key, reloading, telling who listens that the list changed |
| `state` | loading and creating the two keys |
| `sshd` | the SSH server: authentication against `keys`, the rules of §5, a session's life, closing sessions whose grant is gone |
| `irohnet` | the endpoint, announcing, and connections for `/iroh/ssh` as `net.Conn` |
| `kube` | the cluster: clients, discovery, informers, tables, YAML, describe, logs, and the actions; the grant's rule of §12 |
| `kube/kubetest` | a fake of `kube`'s interface for the tests of `tui` |
| `render` | the renderers of §9 |
| `tui` | the terminal UI |
| `e2e` | the end-to-end tests |

**The boundaries:**

- `sshd` knows `keys` and is given a function that makes the Bubble Tea
  model of a session from its grant. It knows nothing of Kubernetes.
- `irohnet` knows nothing of SSH: it hands out connections.
- `tui` sees the cluster only as the interface `kube` defines for a
  session, made from a grant. It imports no client-go.
- `render` imports the API's types and nothing of `kube` or `tui`.
- `kube`'s handle for a session is the one place that checks the grant.

## 18. Errors

- **Start.** A port that cannot be bound, a state directory or a key
  list that cannot be read, an API that does not answer within 10
  seconds: one line that says which, and a status that is not 0. A
  relay that has not answered within 15 seconds is a warning, and kosh
  serves; so is mDNS that cannot be had.
- **The API, later.** An action's error is in the flash line; a list's
  error is where the table would be (§8.5). No error of the API ends a
  session.
- **A panic in a session's TUI** is recovered: the session is told and
  closed, the panic is logged with its stack, and every other session
  goes on.
- **A client that vanished** is found by the keepalive of §5. Its log
  streams and views end with its sessions.
- **A client that does not read** holds up the drawing of its own
  session and nothing else.
- **SIGTERM.** kosh stops accepting, tells every session
  `kosh is shutting down`, closes them, closes the endpoint and exits,
  within 10 seconds.

## 19. Tests

| Level | What |
|---|---|
| Unit | `keys`: the format, `read-only`, unknown options, duplicates, reloading, the unreadable file. `state`: creating, reading, the file that does not parse. `render`: every renderer, objects from fixtures against expected rows, health included. `kube`: every action against client-go's fake clientset; **every call that changes the cluster or returns a Secret's contents refuses a read-only grant**, found by going through the interface's methods so that a new one cannot be forgotten. `tui`: models against `kube/kubetest`, with golden frames for each view and dialog, as great-machine's `tuitest`. |
| In process | `sshd` with a Go SSH client: an accepted key, a rejected key, no terminal, a command, a forwarding request; a session closed when its key leaves the list or changes its grant; the keepalive; the handshake's deadline and limit. The same accepted and rejected keys over two iroh endpoints on the loopback address, with no relay. |
| API server | `kube` against a real kube-apiserver with etcd, started by the test (controller-runtime's `envtest`): informers and their stopping, tables of a custom resource, discovery of a new CRD, YAML, and each action's effect on the objects. These tests are skipped, saying so, when the two programs are not to be found. |
| Chart | `helm lint`; the chart rendered with its defaults plus a state Secret, with the load balancer's values, with the host network's values, each checked against the Kubernetes schemas; the two refusals of §15. |
| End to end | The image in a kind cluster, installed with the chart. With `ssh`: the pods table shows a pod the test made; its log shows what the pod printed; a full key scales a deployment; a read-only key has no such key and is refused a secret's YAML. With the real `iroh-ssh`, built by Nix at a pinned commit: the same pods table, over the relays. And outside the cluster: kosh as a process on the machine of the test, with the kind cluster's kubeconfig and `--iroh-relay=false`, found by `iroh-ssh` on the same machine through mDNS alone. |

The end-to-end tests run in the workflow on Linux and can be run on a
Linux machine by hand. Those over the relays need the internet.

## 20. Settled first

*What the probe found is in the plan's
[`task-00-probe.md`](../plans/2026-10-13-kosh-stage-1/task-00-probe.md):
the relay path holds, a Bubble Tea program runs over the stream as over
TCP, and go-iroh's mDNS is not found by `iroh-ssh`, so `irohnet`
announces by itself. §4.2 and §5 above say what followed from it. The
section below is as it was written before the probe.*

Two things this design rests on have not been tried. The plan's first
task tries them with a throwaway program, before anything of kosh is
built, and the plan records what it found.

**`iroh-ssh` against go-iroh.** A go-iroh v0.3.0 endpoint accepts
`/iroh/ssh` and hands the stream to an SSH server, and the real
`iroh-ssh` (iroh 1.3.0) connects to it:

1. by the endpoint ID alone, over the relays and number0's DNS;
2. on one network with the relays off, by mDNS alone.

The first is what go-iroh's compatibility matrix says of iroh 1.3.0 and
is expected to hold. If it does not, stage 1 stops and this design is
looked at again: the iroh path cannot be designed around.

The second is the open one. `iroh-ssh` looks up through the crate
`iroh-mdns-peer-lookup`, and go-iroh's mDNS was modelled on another
crate. The service name is the same; the records differ in where they
put an endpoint's further addresses. If `iroh-ssh` does not find a
go-iroh endpoint, `irohnet` announces the records in the layout that
crate reads, by itself or through a change to go-iroh, whichever the
probe shows to be smaller.

**A Bubble Tea program over that stream.** The same probe runs a
Bubble Tea v2 program under wish v2 on the stream and checks that keys,
colours and a change of the window's size arrive as over TCP.

## 21. Not in this stage

- Anything else that enters a pod: attach, port-forward, copying files
  (§22). A shell in a pod came after this stage, with a design of its
  own (`2026-10-13-kosh-shell-design.md`).
- Metrics: the CPU and memory columns, pulses (§22).
- xray, and "used by" for ConfigMaps, Secrets, claims and
  ServiceAccounts (§22).
- Contexts: kosh shows the one cluster it runs in.
- Helm releases, benchmarks, image scans, plugins, skins, custom
  columns, hotkeys, screen dumps.
- A configuration per user, and anything kept between sessions.
- An identity per key towards Kubernetes (impersonation). The handle of
  §17 is where it would go.
- Informers per namespace, for very large clusters.
- A client of ours.

## 22. Later stages

- **Stage 2: into the pods.** A shell in a container, attach,
  port-forward, copying files. The shell is built
  (`2026-10-13-kosh-shell-design.md`, kosh v0.2.0); attach,
  port-forward and copying files are not yet.
- **Stage 3: what k9s has besides.** The metrics columns and pulses with
  metrics-server, xray, "used by", and what else of k9s is missed.

Each gets a design of its own when its turn comes.

## 23. What changed while it was built

Sections 4 to 6, 8 and 16 were amended in place while stage 1 was built,
and say what is built. What follows was decided after kosh first ran in a
real cluster and was read as a whole; it adds to the sections named, and
where it differs from them, this section is what is built. The plan's
"Departures" and its ledger have the reasons.

- **§4.2.** kosh's side does not start a QUIC key update early in a
  connection (a defect of the iroh library with connections that use
  several paths). The mDNS announcer joins only interfaces that have an
  address it announces.
- **§5.** A session channel takes `pty-req`, `shell`, `exec`,
  `window-change` and a bounded amount of `env`, and refuses every other
  request, agent forwarding among them. A key that leaves the list loses
  its connections as well as its sessions.
- **§6.** Until 0.3.0: a full key's comment that holds `read-only`,
  `readonly`, `read_only` or `read only` anywhere rejected its line.
  Since 0.3.0 the name decides (§6 as amended), and that guard is gone
  with the option.
- **§8, §12.** The session refuses a query whose namespace is not a name
  or whose resource discovery did not find, as it refuses such a
  reference; no API path is built from an unchecked string. An action
  ends with its session, and is refused once the session has ended.
  AGE of a generic table sorts by the object's creation time.
- **§10.** A dialog is laid over the whole screen and always shows its
  buttons; on a screen too small for it the action is refused. An error
  is wrapped onto up to four lines of the flash and stays until a key is
  pressed. The header shows only whole columns of hints. The help lists
  every key and scrolls sideways when it does not fit. `:kind all` and
  `:kind -A` mean every namespace; a word that is no namespace's name is
  refused. At most 32 views are open, and a view that is covered does
  not read. The editor opens objects of up to 1,000 lines and 48 KiB,
  and its text may grow to 64 KiB; other inputs take 1,024 characters.
  A log view holds at most 5,000 lines and 16 MiB. Bidirectional
  controls are cleaned like other control characters.
- **§13.** A read that a read-only key is refused is logged like a
  refused action. A connection that ends without having sent a byte is
  logged at debug level. What libraries print through Go's standard
  logger is part of the log, at warn level.
- **§15.** The ClusterRoleBinding is named for the release and its role,
  so that the role can be changed by an upgrade. The host network is
  used only with iroh on.
- **§16.** The image also holds `/licenses`. A release tag must be a
  SemVer version.
- **§18.** A stop tells the sessions before it closes what carries them,
  on both transports; the whole stop takes 10 seconds at most.

## Appendix A: the columns

A column marked `*` is shown only wide (`ctrl-w`). Namespaced kinds
drop NAMESPACE when the view is of one namespace.

| Kind | Columns |
|---|---|
| pods | NAMESPACE, NAME, READY, STATUS, RESTARTS, LAST RESTART\*, CPU/RL\*, MEM/RL\*, IP, NODE, SERVICE-ACCOUNT\*, NOMINATED NODE\*, READINESS GATES\*, QOS\*, LABELS\*, VALID\*, AGE |
| containers | IDX, NAME, IMAGE, READY, STATE, RESTARTS, PROBES(L:R:S), CPU/RL, MEM/RL, PORTS, VALID\*, AGE |
| deployments | NAMESPACE, NAME, READY, UP-TO-DATE, AVAILABLE, LABELS\*, VALID\*, AGE |
| replicasets | NAMESPACE, NAME, DESIRED, CURRENT, READY, CONTAINERS\*, IMAGES\*, SELECTOR\*, VALID\*, AGE |
| statefulsets | NAMESPACE, NAME, READY, SELECTOR\*, SERVICE, CONTAINERS\*, IMAGES\*, LABELS\*, VALID\*, AGE |
| daemonsets | NAMESPACE, NAME, DESIRED, CURRENT, READY, UP-TO-DATE, AVAILABLE, LABELS\*, VALID\*, AGE |
| jobs | NAMESPACE, NAME, COMPLETIONS, DURATION, SELECTOR\*, CONTAINERS\*, IMAGES\*, VALID\*, AGE |
| cronjobs | NAMESPACE, NAME, SCHEDULE, SUSPEND, ACTIVE, LAST_SCHEDULE, SELECTOR\*, CONTAINERS\*, IMAGES\*, LABELS\*, VALID\*, AGE |
| services | NAMESPACE, NAME, TYPE, CLUSTER-IP, EXTERNAL-IP, SELECTOR\*, PORTS, LABELS\*, VALID\*, AGE |
| nodes | NAME, STATUS, ROLE, ARCH\*, TAINTS, VERSION, OS-IMAGE\*, KERNEL\*, INTERNAL-IP\*, EXTERNAL-IP\*, PODS, LABELS\*, VALID\*, AGE |
| namespaces | NAME, STATUS, LABELS\*, VALID\*, AGE |
| events | NAMESPACE, LAST SEEN, TYPE, REASON, OBJECT, MESSAGE, SUBOBJECT\*, SOURCE\*, FIRST SEEN\*, COUNT\*, NAME\* |
| ingresses | NAMESPACE, NAME, CLASS, HOSTS, ADDRESS, PORTS, AGE |
| configmaps | NAMESPACE, NAME, DATA, VALID\*, AGE |
| secrets | NAMESPACE, NAME, TYPE, DATA, VALID\*, AGE |
| persistentvolumeclaims | NAMESPACE, NAME, STATUS, VOLUME, CAPACITY, ACCESS MODES, STORAGECLASS, LABELS\*, VALID\*, AGE |
| persistentvolumes | NAME, CAPACITY, ACCESS MODES, RECLAIM POLICY, STATUS, CLAIM, STORAGECLASS, REASON, VOLUMEMODE\*, LABELS\*, VALID\*, AGE |
| storageclasses | NAME, PROVISIONER, RECLAIMPOLICY, VOLUMEBINDINGMODE, ALLOWVOLUMEEXPANSION, LABELS\*, VALID\*, AGE |
| serviceaccounts | NAMESPACE, NAME, SECRET, LABELS\*, VALID\*, AGE |
| roles | NAMESPACE, NAME, LABELS\*, VALID\*, AGE |
| rolebindings | NAMESPACE, NAME, ROLE, KIND, SUBJECTS, LABELS\*, VALID\*, AGE |
| clusterroles | NAME, LABELS\*, AGE |
| clusterrolebindings | NAME, CLUSTERROLE, SUBJECT-KIND, SUBJECTS, LABELS\*, AGE |
| horizontalpodautoscalers | NAMESPACE, NAME, REFERENCE, TARGETS, MINPODS, MAXPODS, REPLICAS, AGE |
| poddisruptionbudgets | NAMESPACE, NAME, MIN-AVAILABLE, MAX-UNAVAILABLE, ALLOWED-DISRUPTIONS, CURRENT, DESIRED, EXPECTED, LABELS\*, VALID\*, AGE |
| networkpolicies | NAMESPACE, NAME, POD-SELECTOR, ING-SELECTOR\*, ING-PORTS\*, ING-BLOCK\*, EGR-SELECTOR\*, EGR-PORTS\*, EGR-BLOCK\*, LABELS\*, VALID\*, AGE |
| customresourcedefinitions | NAME, GROUP, KIND, VERSIONS, SCOPE, ALIASES\*, LABELS\*, VALID\*, AGE |
