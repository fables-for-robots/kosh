# kosh: a key's name maps it to a Kubernetes role

A key's name ends in the name of a Kubernetes role, and a session has
exactly that role's rights: `draganm/cluster-admin`, `bob/view`,
`carol/edit@team-a`. The API server decides what each session may do;
kosh's own `/admin` suffix (v0.3.0) and read-only rule go away.

This design adds to `2026-10-13-kosh-stage-1-design.md` and replaces its
§6 (the key list), §8.1's premise that every session has one identity,
and §12 (read-only keys); where it names a section, it is one of that
document. The user's choices, made in conversation on 2026-10-15: kosh
itself creates and reconciles the bindings; ClusterRoles and namespaced
Roles both; a key the role cannot use is hidden, not refused.

## 1. What the user sees

1. **The key list** names a role at the end of every key's name, after
   its last `/`:

   ```
   ssh-ed25519 AAAA... draganm/cluster-admin
   ssh-ed25519 AAAA... bob@laptop/view
   ssh-ed25519 AAAA... carol/edit@team-a
   ssh-ed25519 AAAA... mic92/key-1/cluster-admin
   ```

   `role` names a **ClusterRole**; `role@namespace` names a **Role** in
   that namespace. The name before the last `/` is the key's name as
   before, and may itself hold `/`. A line without a role gives no
   access, like a line with an option, and is logged with its number and
   the reason (`the name does not end in /<role> or /<role>@<namespace>`).
   A role or namespace that is not a valid Kubernetes name rejects the
   line the same way, and so does a comment with a character that does
   not show (a control, format or bidi character anywhere in it): what
   the operator reads must be what kosh reads.

2. **The session has the role's rights, as the API server grants them.**
   Everything kosh does for the session — tables, describe, YAML, the
   contents of a Secret, logs, the editor, every action, a shell, the
   metrics — is done as the key, and the API server allows or refuses it
   by the role. A table of a kind the role may not list shows the API's
   refusal, as a table shows any refusal today (§8.5). The header shows
   the name, so `Key: bob@laptop/view` says what the session may do; the
   ` [read-only]` mark is gone.

3. **Keys the role cannot use are not there.** On a table of pods a
   `view` key sees no `<ctrl-d> Delete`, no `<e> Edit`, no `<s> Shell`,
   no `<ctrl-k> Kill`; the help lists none of them. `x` on a Secret, `l`
   for logs, `d` and `y` are there when the role may `get` the thing
   (logs: `pods/log`). What a role may do is asked of the API server
   for the table's resource in the table's namespace (§5), so the keys
   follow the role, the kind and the namespace: an `edit@team-a` key has
   `<ctrl-d> Delete` in `team-a` and not in `default`.

4. **A key with a namespaced Role starts in its namespace.** The
   session opens on the pods of `<namespace>`, not of `default`; the
   namespace shortcuts of the header are `<0> all`, `<1> <namespace>`,
   then the recent ones, with `default` nowhere unless it is that
   namespace or was visited; and `:ns` lists what the role may list
   (the table of namespaces is refused to a Role, so `:ns` shows the
   refusal, and the namespace is reached by `:pods <namespace>` or the
   shortcut). Its tables of other namespaces, and `<0> all`, show the
   API's refusal. Its header's `CPU:`/`MEM:` lines need `nodes`, which a
   Role cannot grant, so they are absent.

5. **A role that does not exist** gives no access: every table says
   `forbidden`, and kosh's log says `the role of a key does not exist`
   with the key and the role, at every reading of the list. kosh makes
   the binding all the same (a binding may name a role that is made
   later; the API accepts it), so that the role works the moment it
   exists.

6. **Kubernetes' audit log names the key.** A request of a session is
   made by kosh's ServiceAccount impersonating the user
   `kosh:<key name>` (`kosh:bob@laptop/view`): an audit event's `user`
   is the ServiceAccount and its `impersonatedUser` is the key, with the
   role's group. Today every event says only the ServiceAccount. The
   reads that fill a shared cache are made as the role's own user, named
   as its group (§3.3), so that they are not put down to the key that
   happened to open the cache.

7. **kosh's own log** is as today: every session and every action with
   the key's name and fingerprint, plus the role at session start
   (`role=ClusterRole/view`). A refusal by the API is logged as a
   refusal today is.

## 2. The key list (§6, replaced)

A line is a key and, after it, a comment that is its name. The name's
last `/` segment is its role:

- `<name>/<role>`: the ClusterRole `<role>`.
- `<name>/<role>@<namespace>`: the Role `<role>` in `<namespace>`.
- `<role>` must be a valid RBAC object name (a path segment: not empty,
  no `/`, not `.` or `..`, no `%`); `<namespace>` a DNS label (RFC 1123
  label, ≤ 63 characters). Anything else rejects the line with the
  reason.
- `<name>` must not be empty: a line whose comment is just `/view` is
  rejected (`a key needs a name before its role`), since the name is
  what the log and the header show.
- Any option rejects the line (`read-only` with its own words, as since
  v0.3.0). A certificate is rejected. A comment-less key has no name and
  no role: rejected (`no name, no role`), where before it was a read-only
  key named by its fingerprint.
- A key listed twice: the first line's name and role; a later line with
  a different role is rejected (`the key is on line N with another
  role`), since one key cannot be two users.

```go
// Grant is what the key list says about one key.
type Grant struct {
	Name        string // the whole comment: "bob@laptop/view"
	Fingerprint string
	Role        Role
}

// Role is the Kubernetes role a key's name ends in.
type Role struct {
	// Kind is "ClusterRole" or "Role".
	Kind string
	// Namespace is set for a Role, empty for a ClusterRole.
	Namespace string
	Name      string
}

// User returns the user a session of the grant impersonates:
// "kosh:" + Name.
func (g Grant) User() string

// Group returns the group that carries the role's binding for the
// kosh instance named instance: "kosh:<instance>:clusterrole:<role>" or
// "kosh:<instance>:role:<namespace>:<role>".
func (r Role) Group(instance string) string
```

**The instance.** Several koshes may serve one cluster (two releases, a
`kosh serve --kubeconfig` beside a deployed one, the e2e's own second
kosh), each with its own key list. Each is an **instance**, named by
`--instance` (the chart passes `<namespace>-<release>`; the default is
`kosh`); the name is a path segment without `:` (`[a-z0-9.-]`, at most
63 characters). The instance's name is in every group, every binding's
name and the cache users (§3.3), so that two instances never share a
binding and one never prunes another's (§4).

`ReadOnly` is gone from `Grant`, and with it `ErrReadOnly`, `mayChange`
and `maySee` (§12): the API server refuses, and kosh shows and logs its
refusal.

**The `admin` warning.** The ClusterRole `admin` exists in every cluster
and is the namespaced administrator's role, meant for RoleBindings: bound
cluster-wide it grants every namespaced resource in every namespace and
little of the cluster's own (no nodes, no CRDs, no cluster-wide RBAC;
it can read the namespaces). A list from
v0.3/v0.4 names `…/admin`, which v0.5.0 reads as that ClusterRole, not as
`cluster-admin`. kosh logs, at every reading of a list that has a
`…/admin` ClusterRole key, `WARN "a key's role is the ClusterRole admin,
which is not cluster-admin: before 0.5.0 /admin meant full access"`, with
the keys. The release notes say the same.

## 3. `kube`: a session is the key

### 3.1 Impersonation

kosh talks to the API server with its ServiceAccount as before, and
every request of a session carries impersonation:

```
Impersonate-User:  kosh:<key name>
Impersonate-Group: <role's group, with the instance's name>
```

(client-go's `rest.ImpersonationConfig{UserName, Groups}`; the API server
adds `system:authenticated` itself, which discovery and version need.)
The ServiceAccount needs `impersonate` on `users` and `groups` (§7).

`Cluster` keeps one set of clients per **identity**, keyed by `(user,
group)` (`rest.CopyConfig(cfg)` with `Impersonate` set; typed, dynamic,
metadata, REST-tables and the log opener, as `newCluster` makes them for
the ServiceAccount today), made on first use and kept for the Cluster's
life: a client is a configuration over the one shared transport, and
the key list is bounded. `session` holds its identity's clients; every
method that uses `s.c.typed`, `s.c.dyn`, `s.c.meta`, `s.c.tables` or
`s.c.openLog` today uses the session's instead.

### 3.2 kosh's own requests

Discovery, the version, and the bindings (§4) are made as the
ServiceAccount. Nothing else is.

### 3.3 Caches per role

An informer's cache is shared by the sessions that may see the same
things: **one cache per `(role, gvr)`**, and for a namespaced Role **per
`(role, gvr, namespace)`**, listing that namespace alone (a cluster-wide
list by a Role-bound identity is refused, and would show the refusal for
every namespace). The cache's own requests impersonate the role's user,
named as its group is (`kosh:<instance>:clusterrole:view`,
`kosh:<instance>:role:team-a:edit`; the two prefixes keep a ClusterRole
`team-a:edit` and a Role `edit` in `team-a` apart) with the role's group: the audit
log then says that the list was made for the role's cache, not for
whichever key opened it first. `kindCache` gains the
identity and the namespace; `acquire`/`release`, the idle stop and
`stopCaches` are as they are, over a map keyed by the triple. A `view`
session and a `cluster-admin` session of the same kind hold two caches;
two `view` sessions hold one. Memory is what it was, times the roles in
use.

The pseudo kinds (containers, rules), the relations (`Of`), the owner,
the images, the replicas and the generic tables are reads of the session
and take its identity (the tables' REST client per identity).

### 3.4 Metrics

The pollers and the usage watch of `2026-10-14-kosh-metrics-design.md`
are per `(role, kind)` the same way, with the role's user; `view` has the
metrics (metrics-server aggregates its reader into `view`), a Role cannot
grant `nodes`, so a Role-bound session has the columns of pods and
containers and no header lines. A ClusterRole the API refuses the list of
the nodes (`view`, `edit`, `admin`) has no header lines either, rather
than lines that stay `n/a`; the log says so once for the role.

### 3.5 What is asked, and `Can`

```go
// Reader gains:
// Can reports whether the session's role allows verb on a resource
// (and subresource) in namespace, "" for the cluster scope or every
// namespace. It asks the API server (SelfSubjectAccessReview, as the
// key) and keeps the answer for canFor; an error is answered as false,
// and logged once per session.
Can(ctx context.Context, verb string, res Resource, subresource, namespace string) bool
```

`canFor` is 2 minutes; a reading of the key list that changes the key's
role ends the session anyway (§6). `ctrl-r` on a table has the session
forget its answers to the table's rights (`Reader.ForgetRights(res,
namespace)`), so that a binding made, or a role changed, since shows. The answer for `namespace == ""` of a
namespaced resource is the cluster-wide one (`SelfSubjectAccessReview`
with no namespace), which is what `<0> all` needs.

### 3.6 Sessions end with the list, as before

A key that leaves the list, or whose line changes in its name or its
role, has its sessions ended (`MsgGrantChanged`): the name is the user
the session impersonates and the audit log records, so a renamed key is
another user, and its old sessions do not go on under a name the list
no longer holds. (Until 0.5.0 a rename alone ended nothing; the grant
had nothing but its name and a flag.) The bindings are reconciled before
the sessions are ended (§4), so that a key whose role was narrowed
cannot act with the old role in between.

## 4. The bindings

kosh makes the bindings its key list needs, and no other, at every
reading of the list (at start and every change), as the ServiceAccount:

- For every distinct ClusterRole in the list: a **ClusterRoleBinding**
  named `kosh:<instance>:clusterrole:<role>`, `roleRef` the ClusterRole,
  one subject `Group kosh:<instance>:clusterrole:<role>`.
- For every distinct Role: a **RoleBinding** named
  `kosh:<instance>:role:<role>` in its namespace, `roleRef` the Role,
  one subject `Group kosh:<instance>:role:<namespace>:<role>`.
- Every binding kosh makes carries the labels
  `app.kubernetes.io/managed-by: kosh` and
  `kosh.fables-for-robots.dev/instance: <instance>`, and the annotation
  `kosh.fables-for-robots.dev/role: ClusterRole/<name>` or
  `Role/<namespace>/<name>`.
- A binding that exists with the right `roleRef` and subject is left as
  it is; one with another `roleRef` is replaced (a `roleRef` cannot be
  changed); one with other subjects has its subjects set. A binding at
  kosh's name without the labels is adopted (labelled) when its
  `roleRef` and subjects are already right, and otherwise left alone
  with a warning; one labelled by another manager is left alone with a
  warning.
- A binding with both of kosh's labels, the instance's own, that the
  list no longer needs is deleted (ClusterRoleBindings, and RoleBindings
  in every namespace, listed by the labels). Another instance's bindings
  are never touched. A namespace that is gone takes its RoleBindings
  with it.
- Order: bindings are made before the new list takes effect for
  sessions, and deleted after the sessions of the keys that lost them
  have been ended, so that no session acts without its binding and none
  acts with a binding it should have lost.
- A reconcile that fails (the ServiceAccount may not, the API is down,
  a Role's namespace does not exist) is logged once
  (`ERROR "the bindings of the key list could not be made"`, with the
  reason), retried every 30 seconds until it works, and logged once more
  when it does (`INFO "the bindings of the key list are made"`); the
  list takes effect regardless: a session whose binding is missing gets
  the API's refusals.
- A role named in the list that does not exist is logged at every
  reading (§1.5); the binding is made all the same.

The reconciler is `kube.Bindings` (`kube/bindings.go`), driven by the
key list's `OnChange`; `cli` wires it as it wires the list's watch. It
is the one place kosh writes RBAC.

## 5. `tui`: keys the role cannot use are hidden

A binding declares what it needs:

```go
type binding struct {
	…
	// Needs are the rights the key needs on the table's resource, in
	// the table's namespace, all of them: the key is bound only when
	// Session.Can says yes to each. Nil needs nothing.
	Needs []need
}

// need is one right: verb on the table's resource, or on sub of it,
// or on another resource altogether (Resource set).
type need struct {
	Verb     string
	Sub      string
	Resource *kube.Resource // nil: the table's
}
```

| Key | Needs |
|---|---|
| `ctrl-d` Delete | `delete` |
| `e` Edit | `get`, `update` |
| `ctrl-k` Kill | `delete` on pods |
| `s` Scale | `get`, `update` on `<res>/scale` |
| `r` Restart | `patch` |
| `i` Set Image | `patch` |
| `ctrl-l` Rollback | `get` on replicasets, `patch` on deployments |
| `t` Trigger | `get` on cronjobs, `create` on jobs |
| `s` Suspend/Resume | `patch` on cronjobs |
| `c`/`u` Cordon/Uncordon | `patch` on nodes |
| `r` Drain | `patch` on nodes, `list` on pods (cluster), `create` on `pods/eviction` |
| `s` Shell | `get` on pods, `create` on `pods/exec` |
| `l`/`p` Logs | `get` on `pods/log` |
| `x` Decode | `get` on secrets |
| `d` Describe, `y` YAML | `get` (describe also reads events where it may: `list` on events is not required, and describe shows what it can) |

`enter` into a pod's containers or a role's rules, and `J` to the owner,
need nothing: they read what the table's own cache or `get` already
allowed, and are bound as today.

`actionApplies` loses its read-only check; `bindingsFor(res, grant)`
becomes `bindingsFor(res, grant, can)` where `can` is the table's
answers. A table view, when it opens and when its namespace changes,
asks `Session.Can` for every need of every binding that applies by kind
(one command, the answers gathered; `Can`'s cache makes a second table
of the same kind and namespace free), then binds the keys whose needs
are all met and draws the hints. Until the answers are in, no action
key is bound and the hints show none: a fraction of a second on the
first table of a kind, nothing after.

The help lists what is bound. The `[read-only]` suffix of the header
goes; the name is the role.

**The starting namespace** is the grant's role's namespace when it has
one (`root.ns.current` and the first table's query), else `default` as
today; the shortcuts' second entry is that namespace (`namespaces.use`
and `shortcuts()` take the starting namespace in place of the fixed
`default`).

`x` on a Secret, `d`, `y`, `l` are bound by the same rule (`get` on the
resource or `pods/log`); a key that may not read Secrets has no `x`
and, on the secrets table, no `d`/`y`. (The table itself shows what
`list` allows: a role with `list` and not `get` on secrets, rare, sees
the table and no keys.)

## 6. `sshd`, `cli`

- `sshd` passes the grant through as it does; `MsgGrantChanged` covers a
  changed role (the grant compares unequal).
- `cli serve` wires `kube.Bindings` to the key list: reconcile at start
  (before the listeners open, logged; a failure does not stop the
  start) and on every change.
- `kosh id` and the rest are unchanged.

### 4.1 The key list is a way to any role

Whoever can change the key list can name any role kosh's ServiceAccount
may bind, `cluster-admin` among them: the list is the access control. A
key whose role can write ConfigMaps in kosh's namespace (the ClusterRoles
`admin` and `edit` bound cluster-wide, any Role there with `update` on
`configmaps`) can therefore grant itself anything. At every reading of
the list kosh asks the API server, as each role, whether it may `update`
or `patch` `configmaps` in kosh's own namespace (the ServiceAccount's
namespace, from the pod, or `--namespace`), and logs `WARN "a key's role
may edit the key list, and so grant itself any role"` with the keys for
every role that may and is not `cluster-admin`. The README says it in
plain words; the chart's notes say it at install.

## 7. The chart and RBAC

- `rbac.clusterRole` stays, default `cluster-admin`. The README says
  what kosh's ServiceAccount needs at the least: `impersonate` on
  `users` and `groups`; `get`, `list`, `create`, `update`, `delete` on
  `clusterrolebindings` and `rolebindings` (`rbac.authorization.k8s.io`);
  `get` on `clusterroles` and `roles`, so that a role that does not
  exist can be told from one that does (§1.5); and the right to bind the
  roles the list names (`bind` on `clusterroles` and `roles`, or the
  roles' own rights — Kubernetes refuses a binding by a subject that has
  neither); plus discovery, which every authenticated subject has. A ServiceAccount that can impersonate any
  user and bind any role is as powerful as the roles it binds, so the
  default is honest.
- The chart's `authorizedKeys` comment and NOTES say the name's form.
- Nothing else in the chart changes.

## 8. The README

- **Keys**: the name's form, ClusterRole and Role, the rejections, the
  `admin` warning, the bindings kosh makes and their names and label.
- **What a key can do**: the role decides; the keys shown follow it;
  where the role lacks a right the table says so; the audit log names
  the key; what kosh's ServiceAccount needs.
- **Metrics**: the RBAC paragraph as before, now per role.
- **Upgrading from 0.4**: every name must end in a role; `/admin` is not
  `cluster-admin`; the read-only keys become `/view` (which also hides
  Secrets' contents, as read-only did, and shows no action keys).

## 9. Tests

- `keys`: the parse of every form and rejection above; `Role.Group`,
  `Grant.User`; the duplicate rule; the `admin` warning.
- `kube`, fakes: per-identity clients (a request's impersonation headers,
  through a reactor that records them); caches per `(role, gvr[, ns])`
  (two sessions of one role share, two roles do not; a Role's cache lists
  its namespace alone); `Can` through a reactor on
  `selfsubjectaccessreviews` (allowed, denied, error → false and one log
  line; cached); the bindings reconciler against the fake (made, kept,
  replaced on another `roleRef`, subjects set, deleted when unneeded,
  RoleBindings per namespace, labels and annotation, the `admin`
  warning, a missing role logged, a failing API retried); the order of
  reconcile vs session end.
- `kube`, envtest: a real API server with RBAC: a `view` key cannot read
  a Secret's data (forbidden from the API, logged); a `cluster-admin`
  key can; a Role `edit@team-a` key lists `team-a` and is refused
  `default` and the cluster; the bindings exist as specified; the
  ServiceAccount's own rights are as §7 says (the envtest cluster has
  RBAC on; kosh runs as a user with the minimum role of §7, to prove it).
- `tui`: keys bound by `Can`'s answers (a fake `Can` on `kubetest`); none
  until the answers are in; the help; no `[read-only]`.
- `sshd`, `cli`: the grant's role in the session; a changed role ends the
  session; `cli` wires the reconciler.
- **End to end**: keys `alice@kosh-e2e/cluster-admin`,
  `bob@kosh-e2e/view` and `carol@kosh-e2e/edit@kosh-e2e-team`: alice
  scales and shells as today; bob has no action keys, cannot decode a
  Secret (forbidden), and sees pods; carol starts in her namespace, has
  `<ctrl-d> Delete` there and not in `default`, and `<0> all` shows the
  refusal; the three bindings exist with the label; a role removed from
  the list has its binding gone within a minute; the old `/admin` list
  logs the warning.

## 10. Settled first

- Impersonation, not per-key ServiceAccounts or tokens: nothing to
  create per key, the audit log names the key, and the ServiceAccount
  already has every right it hands out.
- Groups, not users, in the bindings: one binding per role, not per
  key, and a key's rights change by renaming it, not by editing RBAC.
- kosh makes the bindings: the key list is the single source of truth,
  and whoever may edit it may already name `cluster-admin`.
- Caches per role, not per key: a key's rights are its role's; sharing
  by role keeps the memory of a cluster of many admins what it is.
- The cache impersonates the role's own user: the audit log should not
  say that one key listed what every key of the role reads.
- Keys hidden by asking the API server (SelfSubjectAccessReview), not by
  reading the role: the API's answer is right under any authorizer and
  aggregated ClusterRoles; a review per need per table is cheap and
  cached.
- `role@namespace`, not `namespace/role`: a key's name may hold `/`
  already (`mic92/key-1`), and `@` cannot be in a namespace's name.

## 11. Not in this change

- Per-key rights beyond the role (a second suffix, groups of keys).
- Hiding the keys by reading the role's rules locally.
- A `kosh roles` command that lists or checks the bindings.
- Namespaced Roles bound across several namespaces for one key.

## 12. Release

v0.5.0, breaking: every key needs a role; `/admin` is the ClusterRole
`admin`; read-only keys become `/view`. Shipyard: the admins'
`/cluster-admin`, magicrb `/view`.
