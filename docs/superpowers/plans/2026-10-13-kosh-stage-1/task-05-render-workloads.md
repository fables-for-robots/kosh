# Task 5: render, the workloads

The renderers of pods, a pod's containers, deployments, replicasets, statefulsets, daemonsets, jobs and cronjobs: the tables people look at most.

**Read first:** the spec, §2, §9 and Appendix A (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); the contract and the helpers of the package: `render/render.go`, `render/cell.go`, `render/kinds.go`, and for the tests `render/helpers_test.go` and `render/render_test.go`.

**The reference** is k9s's `internal/render` package, read-only at `/private/tmp/claude-502/-Users-dragan-fables-for-robots/25996946-215e-4a11-ba9e-574cacaa75eb/scratchpad/k9s-src/repo/internal/render/`. For each kind the table below names the file to read. It tells you what every cell holds, how a status is worked out, and what makes an object invalid (k9s calls that `diagnose`). Write kosh's renderer for kosh, with `render.B`; where your logic follows the k9s file closely, say so in a comment at the top of yours (`// Follows k9s's internal/render/pod.go (Apache-2.0).`).


**Files:**
- Create: `render/pod.go`, `render/dp.go`, `render/rs.go`, `render/sts.go`, `render/ds.go`, `render/job.go`, `render/cronjob.go`, and a `_test.go` for each
- Modify: `render/container.go` (the bodies of `ContainerColumns` and `ContainerRows`; their signatures and documentation stay)
- Create: `render/container_test.go`, and `render/podstatus.go` if the pod's status is better in a file of its own

**Interfaces:**
- Consumes: `Register`, `Renderer`, `Column`, `Row`, `Health` and its values, `B` and `NewRow`, `MapText`, `Join`, `Or`, the value constants, and the GVRs `Pods`, `Deployments`, `ReplicaSets`, `StatefulSets`, `DaemonSets`, `Jobs`, `CronJobs`.
- Produces: a registered renderer for each of those seven GVRs; `ContainerColumns() []Column`; `ContainerRows(pod *corev1.Pod, now time.Time) []Row`.

## The kinds

| Kind | File | k9s file | Columns | Health beyond VALID |
|---|---|---|---|---|
| pods | `pod.go` | `pod.go` | NAMESPACE, NAME, READY, STATUS, RESTARTS, LAST RESTART\*, CPU/RL\*, MEM/RL\*, IP, NODE, SERVICE-ACCOUNT\*, NOMINATED NODE\*, READINESS GATES\*, QOS\*, LABELS\*, VALID\*, AGE | As k9s's `ColorerFunc` of the pod: STATUS `Pending`, `ContainerCreating`, `PodInitializing`, an `Init:…` that is still running, and `Terminating` are `Pending`; `Completed` is `Completed`; `Running` with every container ready is `OK`; anything else (`CrashLoopBackOff`, `Error`, `ImagePullBackOff`, `Running` with a container that is not ready…) is `Error` |
| containers | `container.go` | `container.go` | IDX, NAME, IMAGE, READY, STATE, RESTARTS, PROBES(L:R:S), CPU/RL, MEM/RL, PORTS, VALID\*, AGE | `Completed` for a container that terminated with exit code 0; `Pending` for one that is waiting with `ContainerCreating` or `PodInitializing`; `Error` for one that is waiting or terminated otherwise, or running and not ready |
| deployments | `dp.go` | `dp.go` | NAMESPACE, NAME, READY, UP-TO-DATE, AVAILABLE, LABELS\*, VALID\*, AGE | `Pending` when it wants replicas and none is ready (k9s's colorer: READY is `n/0`… read it) |
| replicasets | `rs.go` | `rs.go` | NAMESPACE, NAME, DESIRED, CURRENT, READY, CONTAINERS\*, IMAGES\*, SELECTOR\*, VALID\*, AGE | none |
| statefulsets | `sts.go` | `sts.go` | NAMESPACE, NAME, READY, SELECTOR\*, SERVICE, CONTAINERS\*, IMAGES\*, LABELS\*, VALID\*, AGE | none |
| daemonsets | `ds.go` | `ds.go` | NAMESPACE, NAME, DESIRED, CURRENT, READY, UP-TO-DATE, AVAILABLE, LABELS\*, VALID\*, AGE | none |
| jobs | `job.go` | `job.go` | NAMESPACE, NAME, COMPLETIONS, DURATION, SELECTOR\*, CONTAINERS\*, IMAGES\*, VALID\*, AGE | `Completed` when every completion it wants has succeeded |
| cronjobs | `cronjob.go` | `cronjob.go` | NAMESPACE, NAME, SCHEDULE, SUSPEND, ACTIVE, LAST_SCHEDULE, SELECTOR\*, CONTAINERS\*, IMAGES\*, LABELS\*, VALID\*, AGE | none |

**The pod's STATUS** is the heart of this task. It is what `kubectl get pods` prints, and k9s's `Pod.Phase` reproduces it: the phase, overridden by a reason, by init containers that have not finished (`Init:1/3`, `Init:CrashLoopBackOff`, `Init:Error`), by a container that is waiting or terminated with a reason, by `Terminating` when the pod has a deletion timestamp, and so on. Follow it case by case, and give each case a test row: Running, Pending with no containers scheduled, ContainerCreating, PodInitializing, `Init:0/2`, `Init:CrashLoopBackOff`, CrashLoopBackOff, ImagePullBackOff, Error (terminated, non-zero), Completed, OOMKilled, Terminating, Evicted (a phase `Failed` with reason `Evicted`), and a pod whose node is lost (`NodeLost` → `Unknown`).

**The containers** are not a kind of the API and have no renderer in the registry: `ContainerRows` returns a row for each init container, container and ephemeral container of the pod, in that order, and IDX tells them apart as k9s does. A row's `Name` is the container's name and its `Namespace` the pod's; its `Labels` are nil. AGE is the age of the container's current state where it has one, else of the pod. A pod without containers gives no rows. A nil pod gives no rows and does not panic. `container_test.go` uses `wantColumnList` for the columns and checks the rows directly.

**Restart counts** are summed over the containers for the pod. **LAST RESTART** is the latest `lastTerminationState.terminated.finishedAt` of any container, as an age, or `NAValue` when none has restarted.

**CPU/RL and MEM/RL** are requests and limits summed over the containers, printed as k9s prints them (`100m:500m`, memory in Mi), with k9s's value where one of the two is not set.

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
