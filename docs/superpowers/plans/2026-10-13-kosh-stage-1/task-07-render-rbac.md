# Task 7: render, RBAC, policy and CRDs

The renderers of service accounts, roles, role bindings, cluster roles, cluster role bindings, horizontal pod autoscalers, pod disruption budgets, network policies and custom resource definitions, and the rows of a role's rules.

**Read first:** the spec, §2, §9 and Appendix A (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); the contract and the helpers of the package: `render/render.go`, `render/cell.go`, `render/kinds.go`, and for the tests `render/helpers_test.go` and `render/render_test.go`.

**The reference** is k9s's `internal/render` package, read-only at `/private/tmp/claude-502/-Users-dragan-fables-for-robots/25996946-215e-4a11-ba9e-574cacaa75eb/scratchpad/k9s-src/repo/internal/render/`. For each kind the table below names the file to read. It tells you what every cell holds, how a status is worked out, and what makes an object invalid (k9s calls that `diagnose`). Write kosh's renderer for kosh, with `render.B`; where your logic follows the k9s file closely, say so in a comment at the top of yours (`// Follows k9s's internal/render/pod.go (Apache-2.0).`).


**Files:**
- Create: `render/sa.go`, `render/ro.go`, `render/rob.go`, `render/cr.go`, `render/crb.go`, `render/hpa.go`, `render/pdb.go`, `render/np.go`, `render/crd.go`, and a `_test.go` for each
- Modify: `render/rule.go` (the bodies of `RuleColumns` and `RuleRows`; their signatures and documentation stay)
- Create: `render/rule_test.go`

**Interfaces:**
- Consumes: `Register`, `Renderer`, `Column`, `Row`, `Health` and its values, `B` and `NewRow`, `MapText`, `Join`, `Or`, the value constants, and the GVRs `ServiceAccounts`, `Roles`, `RoleBindings`, `ClusterRoles`, `ClusterRoleBindings`, `HorizontalPodAutoscalers`, `PodDisruptionBudgets`, `NetworkPolicies`, `CustomResourceDefinitions`.
- Produces: a registered renderer for each of those nine GVRs; `RuleColumns() []Column`; `RuleRows(rules []rbacv1.PolicyRule) []Row`.

## The kinds

| Kind | File | k9s file | Columns | Health beyond VALID |
|---|---|---|---|---|
| serviceaccounts | `sa.go` | `sa.go` | NAMESPACE, NAME, SECRET, LABELS\*, VALID\*, AGE | none |
| roles | `ro.go` | `ro.go` | NAMESPACE, NAME, LABELS\*, VALID\*, AGE | none |
| rolebindings | `rob.go` | `rob.go` | NAMESPACE, NAME, ROLE, KIND, SUBJECTS, LABELS\*, VALID\*, AGE | none |
| clusterroles | `cr.go` | `cr.go` | NAME, LABELS\*, AGE | none |
| clusterrolebindings | `crb.go` | `crb.go` | NAME, CLUSTERROLE, SUBJECT-KIND, SUBJECTS, LABELS\*, AGE | none |
| horizontalpodautoscalers | `hpa.go` | none: see below | NAMESPACE, NAME, REFERENCE, TARGETS, MINPODS, MAXPODS, REPLICAS, AGE | none |
| poddisruptionbudgets | `pdb.go` | `pdb.go` | NAMESPACE, NAME, MIN-AVAILABLE, MAX-UNAVAILABLE, ALLOWED-DISRUPTIONS, CURRENT, DESIRED, EXPECTED, LABELS\*, VALID\*, AGE | none |
| networkpolicies | `np.go` | `np.go` | NAMESPACE, NAME, POD-SELECTOR, ING-SELECTOR\*, ING-PORTS\*, ING-BLOCK\*, EGR-SELECTOR\*, EGR-PORTS\*, EGR-BLOCK\*, LABELS\*, VALID\*, AGE | none |
| customresourcedefinitions | `crd.go` | `crd.go` | NAME, GROUP, KIND, VERSIONS, SCOPE, ALIASES\*, LABELS\*, VALID\*, AGE | `Error` when the definition's condition `Established` is not true (k9s's `diagnose` of a CRD) |

The objects are `rbacv1` types, `autoscalingv2.HorizontalPodAutoscaler`, `policyv1.PodDisruptionBudget`, `networkingv1.NetworkPolicy`, and `apiextensionsv1.CustomResourceDefinition` (`k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1`).

**The HPA has no k9s renderer**: k9s shows the API server's table. kosh's has the columns of `kubectl get hpa`. REFERENCE is `Kind/name` of `spec.scaleTargetRef` (`Deployment/web`). TARGETS has one entry for each metric of the spec, joined by `, `: the metric's name, a colon, the current value from the status's metric of the same position (`UnknownValue` when the status has none), a slash, and the target (`cpu: 23%/80%`, `memory: 120Mi/200Mi`, `http_requests: 12/100`); for a resource metric with a utilization target the two are percentages, otherwise quantities as written. An HPA without metrics shows `MissingValue`. MINPODS is `spec.minReplicas`, or `<unset>` when nil. MAXPODS and REPLICAS (`status.currentReplicas`) are numbers. MINPODS, MAXPODS and REPLICAS are `Numeric`.

**A CRD's NAME** is the definition's name (`widgets.example.com`): the UI uses it to open the table of the resource's instances, so it must be exactly `metadata.name`.

**The rules** are not a kind of the API and have no renderer in the registry. `RuleColumns` and `RuleRows` follow k9s's view of a role's rules, `internal/render/policy.go` and `rbac.go`: one row for each resource a rule names (`pods`, `deployments.apps`, `pods/log`), and for a rule with non-resource URLs one row for each URL; the columns are the resource's name, its API group, and one column for each of the verbs `get`, `list`, `watch`, `create`, `patch`, `update`, `delete`, `deletecollection`, holding a mark where the rule allows it (a `*` verb marks them all), and a last column for whatever other verbs the rule has. Read the k9s files for the exact names of the columns and for the marks, and use them. Several rules that name the same resource are merged into one row. Rows are sorted by name. A row's `Name` is the first cell and its `Namespace` is empty. Nil or empty rules give no rows. `rule_test.go` uses `wantColumnList` for the columns and checks the rows directly, including a `*` verb, a `*` resource, a resource with a group, a subresource, a non-resource URL, and two rules for one resource.

## The rules of a renderer

1. **One file per kind**, named in the table, with its test beside it (`pod.go`, `pod_test.go`).
2. **An unexported type without fields** implements `Renderer`, and the file registers it from `init`:

   ```go
   type deployment struct{}

   func init() {
   	Register(Deployments, deployment{}, func() runtime.Object { return &appsv1.Deployment{} })
   }
   ```

3. **`Columns`** returns exactly the columns the table below lists for the kind, in that order; a star marks `Wide`. They are Appendix A's. `Right` is set where k9s aligns right (its `Align: tview.AlignRight`). `Numeric` is set for every column whose cells are counts, ages, durations or quantities: those are added with `B.Int`, `B.Num` or `B.Age`, and the number is what the column sorts by. Return the same slice contents at every call.
4. **`Row`** asserts the object's type and returns an error for any other (`fmt.Errorf("deployment renderer: got %T", obj)`). It starts with `NewRow(obj)` and adds one cell per column, in order.
5. **Nothing panics.** An object may lack anything: a nil pointer, an empty status, no containers. A cell that has nothing to show holds `MissingValue`, `NAValue` or `UnknownValue`, as k9s chooses between them. `TestEveryRendererTakesAnEmptyObject` in `render_test.go` renders a zero object of every registered kind; it must pass for yours.
6. **NAMESPACE** is the first column of a namespaced kind. **AGE** is `B.Age(obj.CreationTimestamp.Time, now)`. **LABELS** is `B.Map(obj.Labels)`.
7. **VALID** is `B.Valid(reason)`: empty for an object that is as it should be, and otherwise the reason, in k9s's words. A reason makes the row's health `Error`.
8. **Health** beyond that is the kind's own, given in the table. `Pending` is for what is on its way, `Completed` for what ran and is done.
9. **Leave out** what stage 1 has no data for, even where k9s's file has it: the metrics columns (CPU, MEM, %CPU…, %MEM…), the GPU columns, VS and PF. The requests and limits columns (`CPU/RL`, `MEM/RL`) stay: they come from the spec of the pod, not from metrics.

## The tests of a renderer

Each kind's `_test.go` has:

- **`Test<Kind>Columns`**: `wantColumns(t, <GVR>, …)` with the names from the table, stars included.
- **`Test<Kind>Rows`**: a table of objects built in the test, each with the cells it must give, checked with `rowOf` and `wantCells`, and the `Health` it must have. At least: one object that is as it should be, with every cell asserted; one for each way the kind can be invalid or unhealthy that the table names; and one that holds only its metadata. Ages come from `ago(…)`.
- **`Test<Kind>Order`**: for each `Numeric` column, `orderOf` of two rows that differ in it, in the order they must sort.

The expected cells are worked out from the object by you, from what the cell means, before the renderer exists; a test whose expectation was copied from the renderer's output proves nothing.

## Steps

For each kind, in the order of the table:

- [ ] Write its `_test.go`. Run `go test ./render/ -run Test<Kind>` and see it fail for want of a renderer.
- [ ] Write its renderer. Run until `go test -race ./render/` passes, `TestEveryRendererTakesAnEmptyObject` included.
- [ ] Commit: `Render: <kind>`.

At the end: `gofmt -l . && go vet ./... && go build ./...` are clean.

## Report

The kinds you built and the test results; for each kind, anything where you departed from the k9s file and why; anything in the contract or the helpers that did not fit.
