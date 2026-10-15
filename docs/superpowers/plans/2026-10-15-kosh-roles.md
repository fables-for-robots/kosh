# kosh roles Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A key's name ends in a Kubernetes role (`name/role` a ClusterRole, `name/role@namespace` a Role); a session has exactly that role's rights, enforced by the API server through impersonation; kosh makes the bindings; keys the role cannot use are hidden.

**Architecture:** `keys` parses the role. `kube` gives every session clients that impersonate `kosh:<key name>` with the role's group, keeps informer caches per role (and per namespace for a Role), asks the API what a role may do (`Can`), and reconciles the bindings at every reading of the list. `tui` binds action keys only when `Can` says yes, and starts in the Role's namespace. kosh's own read-only rule, `ErrReadOnly`, `[read-only]` and the full-key-only handover go.

**Tech Stack:** Go, client-go (`rest.ImpersonationConfig`, `SelfSubjectAccessReview`, RBAC clients), envtest with RBAC, Bubble Tea v2, kind for the e2e.

**Spec:** `docs/superpowers/specs/2026-10-15-kosh-roles-design.md` — the binding authority. Background: `docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md` (§6, §8, §12 are replaced or amended by this), `2026-10-14-kosh-metrics-design.md` (§4: pollers, now per role).

## Global Constraints

- No `internal/` packages; `urfave/cli/v2`.
- A session never touches the API server except as its key: every client the session uses impersonates (spec §3.1); kosh's own requests (discovery, version, bindings) are the ServiceAccount's and nothing else is (§3.2).
- A cache is shared only by sessions of one role; a Role's cache lists its namespace alone (§3.3). The cache's requests impersonate `kosh:role:<role>` with the role's group.
- Bindings: names, labels, annotation, subjects and the order of make/end/delete exactly as §4.
- Every key hidden by §5 is hidden by `Can`'s answer, not by reading the role.
- `Grant.ReadOnly`, `ErrReadOnly`, `mayChange`, `maySee`, the header's ` [read-only]` and `cli`'s read-only handover rule are removed, not kept behind a flag.
- Log lines in the codebase's register; every rejected key-list line logged with its number and reason; the `admin` warning as §2.
- Tests run with `-race`; `kube` and `cli` envtest tests with `KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)"`. envtest's API server runs with RBAC (`--authorization-mode=RBAC` is in controller-runtime's defaults; verify in Task 2 and set it if not).
- Commits end with the session's attribution trailers.

## Review Focus

1. A session acting with a stale right: the list narrows a key's role, and between the reading and the session's end the old binding is still there — order of make/end/delete (§4), tested in Task 4.
2. Two sessions of different roles sharing a cache by mistake (a `view` key seeing Secrets listed as cluster-admin): cache keyed by role, tested in Task 2.
3. A Role-bound key whose cache is cluster-wide (refused, every table forbidden): per-namespace caches for Roles, tested in Task 2 and the envtest case.
4. kosh deleting a binding it did not make (another tool's CRB named like kosh's, or one without the label): only labelled bindings, tested in Task 4.
5. The UI binding a key before `Can` answered, or after a namespace switch with the old answers: tested in Task 5.

---

### Task 1: `keys` — the role in the name

**Files:**
- Modify: `keys/keys.go`; Test: `keys/keys_test.go`, `keys/watch_test.go`.

**Interfaces (produces):**
```go
type Grant struct { Name, Fingerprint string; Role Role }
type Role struct { Kind, Namespace, Name string }   // Kind "ClusterRole" | "Role"
func (g Grant) User() string                        // "kosh:" + Name
func (r Role) Group() string                        // "kosh:clusterrole:<name>" | "kosh:role:<ns>:<name>"
func (r Role) String() string                       // "ClusterRole/view", "Role/team-a/edit"
func ParseRole(comment string) (name string, role Role, reason string)   // reason != "" rejects
```
`AdminSuffix`, `ReadOnly` and `reasonAdminCase` go. The log at each reading: `keys=N clusterroles=K roles=M rejected=R` (distinct roles counted), plus the `admin` warning (`msgAdminRole`) with the keys' names when any ClusterRole is named `admin`.

- [ ] **Step 1: Failing tests.** `TestParseRoleForms` (table: `a/cluster-admin` → ClusterRole; `bob@laptop/view`; `carol/edit@team-a` → Role team-a/edit; `mic92/key-1/cluster-admin` → name `mic92/key-1`; `a/role@ns@x` rejected; `/view` rejected `a key needs a name before its role`; `alice` rejected (no `/`); `alice/` rejected; `alice/a b` rejected (space in role); `alice/..` rejected; `alice/v%w` rejected; `alice/edit@Team` rejected (namespace not a DNS label); `alice/edit@` rejected; trailing spaces trimmed). `TestNoCommentIsRejected`. `TestDuplicateKeyWithAnotherRoleIsRejected` (same role twice: one grant). `TestReadOnlyOptionStillRejected`. `TestAdminWarning` (watch_test: a list with `x/admin` logs `msgAdminRole` with `keys=x/admin`; `x/admin@ns` does not). `TestCounts` (clusterroles, roles). `Grant.User`, `Role.Group`, `Role.String`.
- [ ] **Step 2: Run** — compile failure.
- [ ] **Step 3: Implement.** Validation: role name by `k8s.io/apimachinery/pkg/api/validation/path.IsValidPathSegmentName`; namespace by `k8s.io/apimachinery/pkg/util/validation.IsDNS1123Label`.
- [ ] **Step 4:** `go test -race ./keys/`; the rest of the repo will not compile until Task 2 — that is expected; commit `keys` alone.
- [ ] **Step 5: Commit** `keys: a key's name ends in its Kubernetes role`.

---

### Task 2: `kube` — a session is the key: identities, caches per role, no read-only

**Files:**
- Create: `kube/identity.go` (identity clients), modify `kube/cluster.go`, `kube/session.go`, `kube/informers.go`, `kube/watch.go`, `kube/pseudo.go`, `kube/table.go`, `kube/metrics.go`, `kube/usage.go`, `kube/audit.go`, `kube/kube.go`, `kube/secret.go`, `kube/logs.go`, `kube/exec.go`, `kube/actions.go`, `kube/drain.go`, `kube/describe.go`, `kube/yaml.go`, `kube/owner.go`, `kube/relations.go`, `kube/workload.go`; `kube/kubetest/kubetest.go` (Grant without ReadOnly; the fake no longer refuses by read-only).
- Test: `kube/identity_test.go`, changes across `kube/*_test.go`, `kube/read_envtest_test.go`/`kube/envtest_test.go` (RBAC on; users via `envtest`'s `AddUser`).

**Interfaces:**
- Consumes: Task 1's `Grant`, `Role`.
- Produces:
  ```go
  // identity is one impersonated caller: the user and the role's group.
  type identity struct { user, group string }
  func (g keys.Grant) identity() identity           // in kube: identityOf(grant)
  func roleIdentity(r keys.Role) identity          // "kosh:role:<name>" / "kosh:role:<ns>:<name>" with the group
  // clients of one identity, made on first use and kept: typed, dyn, meta, tables, openLog, cfg
  func (c *Cluster) clientsFor(id identity) (*clients, error)
  // caches keyed by (identity of the role, gvr, namespace)
  type cacheKey struct { id identity; gvr schema.GroupVersionResource; namespace string }
  func (c *Cluster) acquire(key cacheKey) (*kindCache, error)
  ```
  `session` holds `role identity`, `self identity` and `*clients`. `Watch` of a typed kind for a Role-bound session uses `cacheKey{roleIdentity, gvr, role.Namespace}`; a query of another namespace by such a session uses a cache of that namespace (which the API refuses: the table shows it). Pollers: `acquireMetrics(id, gvr)`; the usage watch per role identity. `Session` loses nothing public but `ErrReadOnly`; `Grant()` returns the new Grant.
  The session start log gains `role=<Role.String()>`; `sshd` logs it (Task 6).

- [ ] **Step 1: Failing tests (fakes).** A reactor on the dynamic and typed fakes cannot see headers; instead, `clients` are built through a `newClients func(*rest.Config) (*clients, error)` hook on `Cluster` (production: real clients; tests: fakes per identity, recording the `cfg.Impersonate` they were made with). Tests: `TestSessionClientsImpersonateTheKey` (user `kosh:alice/view`, groups `[kosh:clusterrole:view]`); `TestCachesArePerRole` (two `view` sessions share one cache, a `cluster-admin` session has another; the cache's clients were made with user `kosh:role:view`); `TestRoleCacheIsNamespaced` (a `edit@team-a` session's pods cache lists `team-a` only — the fake records the list's namespace); `TestAnotherNamespaceIsAnotherCache`; `TestNoReadOnlyRefusal` (a `view` grant's `SecretData` goes to the API, which the fake refuses with 403, and the error is the API's; nothing is refused by kosh); `TestMetricsPollersPerRole`; `TestClusterUsagePerRole`. The audit log test: refusals by the API logged as before.
- [ ] **Step 2: envtest, RBAC.** In `envtest_test.go` make a `ClusterRole` for kosh's own minimum (§7) and run the Cluster as a user with it (`env.AddUser(envtest.User{Name: "kosh", Groups: ...}, cfg)`), and bind `view`/`cluster-admin`/a Role `edit` in `team-a` to the three groups by hand (Task 4 makes the reconciler; here the bindings are fixtures). Tests: `TestViewCannotReadASecret` (forbidden from the API), `TestClusterAdminCan`, `TestRoleSeesItsNamespaceOnly` (pods of `team-a` listed; `default` and cluster-wide refused with the API's message in `Table.Err`), `TestImpersonatedUserInTheAPIsAnswer` (a `SelfSubjectReview` as the session names `kosh:alice/view` and the group).
- [ ] **Step 3: Implement.** Remove `mayChange`/`maySee`/`ErrReadOnly`; every `s.c.typed` etc. → `s.clients.typed`; `kindCache` gets `id`, `namespace`; `startCache` lists/watches with `dyn.Resource(gvr).Namespace(ns)` when `ns != ""`; `stopCaches`, `release`, `stopIdle` over the new map; `metricsPoller` keyed by `(id, gvr)`; `usageWatch` acquires with the role identity; `openLog` per identity.
- [ ] **Step 4:** `KUBEBUILDER_ASSETS=… go test -race ./kube/...` and `go build ./...` (tui/sshd/cli will not build until their tasks; build `./kube/...` only) — pass.
- [ ] **Step 5: Commit** `kube: a session acts as its key, with caches per role`.

---

### Task 3: `kube` — `Can`, and the fake's `Can`

**Files:**
- Create: `kube/can.go`; modify `kube/kube.go` (Reader.Can), `kube/kubetest/kubetest.go` (`Allow(verb, group, resource, sub, namespace)`, `Can`); Test: `kube/can_test.go`, envtest case in `read_envtest_test.go`.

**Interfaces (produces):**
```go
// Reader:
Can(ctx context.Context, verb string, res Resource, subresource, namespace string) bool
// kubetest:
func (s *Session) Allow(verb, group, resource, subresource, namespace string)   // "*" wildcards
```
`canFor = 2 * time.Minute`; the cache is per session, keyed by `(verb, group, resource, sub, namespace)`; an error answers false and logs `WARN "the key's rights could not be checked"` once per session.

- [ ] **Step 1: Failing tests.** Fakes: a reactor on `create selfsubjectaccessreviews` that answers by a table and records the review's spec (verb, group, resource, subresource, namespace, and that the request impersonated the key); `TestCanAsksOnceWithinCanFor`; `TestCanErrorIsFalseAndLoggedOnce`; `TestCanClusterWideWhenNamespaceEmpty`. envtest: `TestCanAgainstRBAC` (a `view` user: `list pods` true, `delete pods` false, `get secrets` false; `edit@team-a`: `delete pods` in `team-a` true, in `default` false, in `""` false).
- [ ] **Step 2–4:** fail, implement, pass (`-race`, envtest).
- [ ] **Step 5: Commit** `kube: Can asks the API server what the key's role allows`.

---

### Task 4: `kube` — the bindings reconciler; `cli` wiring

**Files:**
- Create: `kube/bindings.go`; modify `cli/serve.go` (wire `kube.NewBindings(cluster, list, log)`: reconcile at start, `list.OnChange` registered before sshd's handler for the make phase and after it for the delete phase — see Step 3), `sshd/sshd.go` only if the hook order needs a second `OnChange`; Test: `kube/bindings_test.go` (fakes), envtest case, `cli` test of the wiring.

**Interfaces (produces):**
```go
type Bindings struct { … }
func NewBindings(c *Cluster, roles func() []keys.Role, log *slog.Logger) *Bindings
// Make makes the bindings every role of roles() needs (create or fix), and reports the roles that do not exist.
func (b *Bindings) Make(ctx context.Context) error
// Prune deletes kosh's bindings no role of roles() needs.
func (b *Bindings) Prune(ctx context.Context) error
// Run reconciles at once and on every tick of the retry interval while an earlier Make or Prune failed, until ctx ends.
func (b *Bindings) Run(ctx context.Context, changed <-chan struct{})
```
`keys.Set` gains `Roles() []Role` (distinct, sorted). Names, label, annotation per spec §4. The order §4 asks for: `cli` registers on the list, in this order: (1) `bindings.makeNow` (synchronous `Make`; a failure logged and left to `Run`'s retry), (2) sshd's `keysChanged` (ends sessions), (3) `bindings.pruneNow`. `keys.List.Replace` calls `OnChange` functions in registration order (it does today; add a test that pins it).

- [ ] **Step 1: Failing tests (fakes, typed RBAC client).** `TestMakeCreatesClusterRoleBinding` (name, roleRef, subject, label, annotation); `TestMakeCreatesRoleBindingInNamespace`; `TestMakeKeepsACorrectBinding` (no update call); `TestMakeReplacesAnotherRoleRef` (delete+create); `TestMakeSetsSubjects`; `TestPruneDeletesOnlyLabelledUnneeded` (Review Focus 4: an unlabelled CRB named `kosh:clusterrole:x` stays; a labelled unneeded one goes; RoleBindings in two namespaces); `TestMissingRoleIsLoggedAndBound`; `TestRunRetriesAFailure` (reactor fails twice; third works; retry interval short); `TestOrderOnChange` (make before sessions end before prune: a recording list handler). envtest: `TestBindingsWorkForReal` (make, then a `view` session can list pods; prune removes).
- [ ] **Step 2–4:** fail, implement, pass. `cli`'s envtest test (`TestServeEndToEnd` family): the server starts with the bindings made; a `view` user's screen has no `<ctrl-d>` (needs Task 5 — leave the cli assertion for Task 6).
- [ ] **Step 5: Commit** `kube, cli: kosh makes the bindings its key list needs`.

---

### Task 5: `tui` — keys follow `Can`; start in the Role's namespace; no read-only

**Files:**
- Modify: `tui/view.go` (binding.Needs, bindingsFor(res, grant, can)), `tui/bind_actions.go`, `tui/bind_logs.go`, `tui/bind_text.go`, `tui/shell.go`, `tui/tablekeys.go`/`tui/table.go` (ask `Can` on open and namespace change; `rightsMsg`; keys unbound until answered), `tui/header.go` (no `[read-only]`), `tui/root.go`/`tui/navigate.go` (starting namespace from `grant.Role.Namespace`; shortcuts' second entry), `tui/help.go`; `tui/options.go` or wherever `Handover` is set (always given).
- Test: `tui/*_test.go` with `kubetest.Allow`; goldens.

- [ ] **Step 1: Failing tests.** `TestActionKeysFollowCan` (allow delete on pods in `default` → `<ctrl-d> Delete` shown; not allowed → absent; `x` on secrets needs `get secrets`; `s` Shell needs `create pods/exec` and `get pods`; `l` needs `get pods/log`); `TestNoKeysUntilCanAnswers` (Review Focus 5: a slow fake `Can` — keys absent, then present); `TestNamespaceSwitchAsksAgain` (`:pods team-a` → different answers); `TestRoleKeyStartsInItsNamespace` (grant `carol/edit@team-a` → first table `Pods(team-a)`, shortcuts `<0> all <1> team-a`); `TestHeaderShowsNoReadOnly`; help lists only bound keys; every golden with a read-only key updated (the fake's default grant is `cluster-admin` with everything allowed, so existing goldens stay).
- [ ] **Step 2–4:** fail, implement, pass (`-race`).
- [ ] **Step 5: Commit** `tui: a key's keys are what its role allows, asked of the API server`.

---

### Task 6: `sshd`, `cli`, the chart, docs, e2e

**Files:**
- Modify: `sshd/session.go` (log `role`; `MsgGrantChanged` on a changed Role), `cli/serve.go` (handover for every session; bindings wiring from Task 4 if not done), `charts/kosh/values.yaml` (authorizedKeys comment; rbac comment with the minimum of §7), `charts/kosh/templates/NOTES.txt`, `README.md` (§8 of the spec), `docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md` (§6, §12 marked replaced; §23 entry), `scripts/test-e2e.sh` (three keys: `alice@kosh-e2e/cluster-admin`, `bob@kosh-e2e/view`, `carol@kosh-e2e/edit@kosh-e2e-team`; the namespace and a Role `edit` in it made by the script, plus a pod there), `e2e/e2e_test.go`.
- Test: `sshd` tests (grant role, change ends), `cli` envtest (`view` screen: no `<ctrl-d>`, `[read-only]` gone), e2e tests per spec §9.

- [ ] **Step 1:** sshd/cli tests and changes; chart and docs; e2e script and tests (`TestViewHasNoActionKeys`, `TestViewCannotDecodeASecret`, `TestRoleStartsInItsNamespace`, `TestRoleHasDeleteOnlyInItsNamespace`, `TestAllNamespacesRefusedToARole`, `TestBindingsExistAndGo` (remove carol from the list → her RoleBinding gone within 90 s), `TestOldAdminListWarns`); `GOOS=linux go vet -tags e2e ./e2e/`.
- [ ] **Step 2:** `scripts/test-e2e.sh` on the Linux box (rules as in the metrics plan's Task 5), full run; `scripts/test-chart.sh`.
- [ ] **Step 3: Commit** `sshd, cli, chart, docs, e2e: roles end to end`.

---

## Departures

Where what was built differs from the plan's text, and why. The task
reports (`.superpowers/sdd/2026-10-15-kosh-roles/task-N-report.md`) have
the details.

| Plan | Built | Why |
|---|---|---|
| Task 1: a role is a valid path segment (`IsValidPathSegmentName`); the no-admin warning kept | a role with a space is rejected, and so is a whole comment with a character that does not show (control, format, bidi); `KindClusterRole`/`KindRole` constants; the no-admin warning (`msgNoAdmin`) removed | `IsValidPathSegmentName` takes `a b` and zero-width characters, which can make a line show one role and grant another; the warning means nothing under roles |
| Task 2: a cache's requests as the role's group, pollers per `(role, kind)`; the user of a cache named by the role | the cache's user is the group's string (`kosh:clusterrole:view`, `kosh:role:team-a:edit`); pollers keyed as caches, with a Role's namespace; a Role-bound session's usage watch never takes the hold; `apiStatusOf` fixed in `table.go` | the two prefixes keep a ClusterRole `team-a:edit` and a Role `edit` in `team-a` apart; a Role's cluster-wide PodMetrics list is refused, and it cannot grant nodes; a refused generic table showed no refusal |
| Task 3: the envtest of `Can` in `read_envtest_test.go`; `kube/session.go` untouched | in `roles_envtest_test.go`, beside `newRolesEnv`; one field on the session for the cache of answers; `TestCanAsksAsTheKey` added | the answers belong to the session; the attributes sent needed a test of their own |
| Task 4: `Run(ctx, changed)`, unexported `makeNow`/`pruneNow`; Forbidden on a role's get a failure; the missing-role warning names the role | `Run(ctx)` only retries; `MakeNow`/`PruneNow` exported for `cli`; reconcile timeout 10 s per call, begun once the lock is held; Forbidden is a warning once (`the role of a key cannot be checked`); `get` on roles added to §7's minimum; the warning names the role's keys (`List.Roles() []RoleKeys`); a binding at kosh's name without the label is adopted, one with another `managed-by` left alone; a lasting failure logged once | `OnChange` drives the reconciles; a hung API holds up the end of a removed key's sessions; without `get` a missing role could not be told; tools that manage a binding must not fight kosh over it; an ERROR every 30 s was noise |
| Task 5: `bindingsFor` filters by kind and needs; `Can` asked one need after another; a `kubetest` delay hook for the no-keys-yet test | split into `applying` and the needs filter; the rights asked concurrently, once per right; ctrl-r asks again; the help reads its table's keys at every drawing; a harness hold in place of the hook | the table asks the needs of the bindings that apply by kind; one round trip, not six; a refused answer is not kept, and a help opened before the answers listed none |
| Task 6: `MsgGrantChanged` on a changed Role; the envtest through `kube`'s `startRBACAPIServer`; the view key refused a Secret's YAML in the e2e; `TestRoleCanBeChanged` as it was | the grant compared whole, so a change of the name alone ends the session too; `cli`'s envtest has its own RBAC start, which fills `view`, and one test runs kosh as a user with no rights; the view key is refused the table of secrets, and kubectl as its user the Secret; with the ServiceAccount `view`, the full key has no action key and the log says its rights could not be checked; `test-mdns.sh`'s key is `/view`, and the script deletes the binding kosh made in a cluster not its own | the name is the user the session impersonates and the audit log names; `kube`'s helpers are its tests', not importable; the start must survive a reconcile that fails; `view` grants no list of secrets, so no row to press `y` on; a ServiceAccount that may not impersonate gets no answer to Can, so no scale dialog opens to be refused; a key without a role is rejected |

## Self-review

- Spec coverage: §1–2 (Task 1, 5, 6), §3.1–3.4 (Task 2), §3.5 (Task 3), §3.6 (Task 4 order, Task 6 sshd), §4 (Task 4), §5 (Task 5), §6–8 (Task 6), §9 tests across tasks.
- Types: `keys.Role`/`Grant` (T1) are what `kube` (T2–4), `tui` (T5), `sshd`/`cli` (T6) use; `identity`, `cacheKey` (T2) feed T3–4 internally; `Can` (T3) is what `tui` (T5) and `kubetest.Allow` give; `Bindings` (T4) is what `cli` (T4/T6) wires.
- The repo does not build between Tasks 1 and 5 (the `ReadOnly` removal cascades); each task's commit says so, and CI runs on the PR, not per commit.
- Review Focus items each name a task and a test.
