# kosh metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** CPU and memory from metrics-server in kosh's header and in the tables of pods, containers and nodes, present only while the cluster serves the metrics API.

**Architecture:** `render` gains metric columns (marked `Metric`) that `Row` fills with `n/a` and `Meter` fills from a reading, plus `StripMetrics`. `kube` gains one poller per metrics kind, shared and polled every 15 s while in use, and meters or strips the tables of pods, containers and nodes on every `Table` call according to discovery; `ClusterUsage` serves the header. `tui` draws two header lines. The e2e kind cluster gets metrics-server.

**Tech Stack:** Go, client-go's dynamic client and `unstructured`, `resource.ParseQuantity`; Bubble Tea v2; metrics-server in kind for the e2e.

**Spec:** `docs/superpowers/specs/2026-10-14-kosh-metrics-design.md` — the binding authority; this plan argues from it. The stage 1 spec `docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md` is the background (§8 cluster data, §9 renderers, §10 the TUI).

## Global Constraints

- No `internal/` packages. `urfave/cli/v2` for the CLI (unchanged here).
- No new module for metrics: parse `unstructured` (spec §4.2, §9).
- Numbers as k9s prints them: millicores and mebibytes whole, no unit; `0` is `0`; a percentage is `floor(100 × used / of)` and `n/a` when `of` is 0 (spec §1.6). An object the reading does not cover is `n/a`, not `0` (spec §2).
- Column order of pods, containers and nodes is k9s's at commit `8f29fba` less the columns kosh does not have (spec §1.2–1.4). The metric columns are narrow, `Right: true`, `Numeric: true`.
- The metric columns and the header lines exist only while discovery knows `pods.metrics.k8s.io` / `nodes.metrics.k8s.io` (spec §3).
- Every poll is one list of the whole cluster; default `MetricsPoll` 15 s; a poller stops as soon as it has no user (spec §4.1).
- Read-only keys see metrics (spec §1.7). Nothing is flashed for a failed reading (spec §1.8).
- Commits end with the session's attribution trailers. Files derived from k9s keep their notice line.
- Every test runs with `-race`. The `kube` and `cli` envtest tests need `KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)"`; run them so.

## Review Focus

1. A pod whose reading has a container the spec does not (a terminated init container still reported) — the sum must count only what the spec names; test in Task 1.
2. The metrics API served for pods but not nodes (a half-broken aggregation): pods have columns, nodes and the header do not; test in Task 3.
3. A watch opened before the API is served, then discovery finds it: columns appear on the next `Table` without reopening; test in Task 3.
4. `Close` of the Cluster while pollers run and while a `ClusterUsage` watch is open: no goroutine left, no send on a closed channel; test in Task 2 and 3 under `-race`.
5. A reading older than the API's removal: after discovery loses the API the columns go, and nothing is shown from the stale reading; test in Task 3.

---

### Task 1: `render` — metric columns, `Meter`, `StripMetrics`

**Files:**
- Modify: `render/render.go` (Column.Metric, Usage, Metered)
- Modify: `render/pod.go`, `render/container.go`, `render/node.go`
- Create: `render/metrics.go` (Usage, Metered, StripMetrics, the shared cell helpers `metricMc`, `metricMi`, `metricPerc`)
- Test: `render/metrics_test.go`, additions to `render/pod_test.go`, `render/container_test.go`, `render/node_test.go`; the fixtures' expected rows gain the `n/a` cells.

**Interfaces:**
- Produces:
  ```go
  // render
  type Column struct { Name string; Wide, Right, Numeric bool; Metric bool }
  type Usage struct { CPU, Mem int64 }               // millicores, bytes
  type Metered interface {
      Renderer
      Meter(row *Row, obj runtime.Object, usage map[string]Usage)
  }
  func MeterContainers(rows []Row, pod *corev1.Pod, usage map[string]Usage)
  func StripMetrics(cols []Column, rows []Row) ([]Column, []Row)
  ```
  `pod` and `node` implement `Metered`. The column positions (0-based, with NAMESPACE) are: pods `CPU` 6, `CPU/RL` 7, `%CPU/R` 8, `%CPU/L` 9, `MEM` 10, `MEM/RL` 11, `%MEM/R` 12, `%MEM/L` 13; containers `CPU` 7, `CPU/RL` 8, `%CPU/R` 9, `%CPU/L` 10, `MEM` 11, `MEM/RL` 12, `%MEM/R` 13, `%MEM/L` 14; nodes `CPU` 11, `CPU/A` 12, `%CPU` 13, `MEM` 14, `MEM/A` 15, `%MEM` 16. `Metric: true` on the six of each kind (`CPU/RL`, `MEM/RL` stay as they are, not Metric).

- [ ] **Step 1: Failing tests.** In `render/metrics_test.go`:
  - `TestMetricColumnsArePlaced`: for each of `pod{}.Columns()`, `ContainerColumns()`, `node{}.Columns()`, the names at the positions above, each with `Metric && Right && Numeric && !Wide`; and no other column is `Metric`.
  - `TestStripMetricsGivesStageOneColumns`: `StripMetrics` of each kind's columns and of a rendered row leaves exactly the stage 1 columns (copy the lists from Appendix A of the stage 1 spec into the test) and a row with as many cells and orders as columns.
  - `TestRowWithoutReadingIsNA`: `Row` of a running pod with requests gives `n/a` with order 0 in the six metric cells; the same for a node (its `CPU/A` and `MEM/A` are the allocatable numbers, not `n/a`) and for `ContainerRows`.
  - `TestPodMeter`: a pod with two containers, requests 100m/200Mi and 300m/600Mi, limits 200m/400Mi and 600m/1200Mi; usage `{"a": {CPU: 50, Mem: 100 MiB}, "b": {CPU: 150, Mem: 300 MiB}}` gives `CPU` `200` (order 200), `%CPU/R` `50`, `%CPU/L` `25`, `MEM` `400`, `%MEM/R` `50`, `%MEM/L` `25`. A usage with `"a"` only sums `a`. A usage with a container `"zz"` the spec lacks ignores it. Requests 0: `%CPU/R` `n/a` order 0. A nil usage leaves `n/a`.
  - `TestContainerMeter`: `MeterContainers` on the rows of that pod: row `a` has `CPU` `50`, `%CPU/R` `50`, `%CPU/L` `25`; row `b` as computed; a row without an entry stays `n/a`.
  - `TestNodeMeter`: node allocatable 4 cores (`4000` mc) and 8Gi; usage `""` `{CPU: 1000, Mem: 2 GiB}` gives `CPU` `1000`, `CPU/A` `4000`, `%CPU` `25`, `MEM` `2048`, `MEM/A` `8192`, `%MEM` `25`. Nil usage: `CPU`, `%CPU`, `MEM`, `%MEM` `n/a`; `CPU/A`, `MEM/A` still numbers.
  - `TestMetricNumbers`: `metricMc(0)` is `"0"`, `metricMi(1536*1024*1024)` is `"1536"`, `metricMi(1024*1024-1)` is `"0"`, `metricPerc(1, 3)` is `"33"`, `metricPerc(5, 0)` is `n/a`.
- [ ] **Step 2: Run** `go test ./render/ -run 'Metric|Meter|StripMetrics'` — fail to compile.
- [ ] **Step 3: Implement** `render/metrics.go` and the three renderers: add the columns in place, fill `n/a` cells in `Row` (through `b.Num(NAValue, 0)`), write `Meter`s that set `row.Cells[i]` and `row.Order[i]` at the indexes found by column name once (`metricIndex(cols, name)` computed at init into package vars). `StripMetrics` builds new slices; it does not modify its inputs.
- [ ] **Step 4: Run** `go test -race ./render/` — all pass, fixtures updated where a row's cells changed (the existing golden rows gain the `n/a` cells at the new positions; update them by hand, not by regenerating blindly, and check one of each kind by eye).
- [ ] **Step 5: Commit** `render: the metric columns of pods, containers and nodes, filled from a reading or stripped`.

---

### Task 2: `kube` — the metrics pollers and the reading

**Files:**
- Create: `kube/metrics.go` (metricsPoller, acquireMetrics/releaseMetrics, parsing, `metricsServed`, `Options.MetricsPoll`)
- Modify: `kube/cluster.go` (Options.MetricsPoll + default 15 s, `pollers map[schema.GroupVersionResource]*metricsPoller`, stopCaches also stops pollers), `kube/kube.go` (nothing public yet)
- Test: `kube/metrics_test.go`

**Interfaces:**
- Consumes: `render.Usage`.
- Produces:
  ```go
  // kube
  var PodMetrics  = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}
  var NodeMetrics = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}
  type reading map[string]map[string]render.Usage   // "ns/name" or "name" -> container ("" for nodes) -> usage
  func (c *Cluster) metricsServed(gvr schema.GroupVersionResource) bool   // from c.resources: a Resource with that GVR
  func (c *Cluster) acquireMetrics(gvr) (*metricsPoller, error)
  func (c *Cluster) releaseMetrics(p *metricsPoller)
  func (p *metricsPoller) current() (r reading, err error)   // last reading and the error of the last list
  func (p *metricsPoller) subscribe() (<-chan struct{}, func())
  func parseReading(gvr, list *unstructured.UnstructuredList, log func(msg string, args ...any)) reading
  ```
  Options: `MetricsPoll time.Duration` ("how often the metrics of a kind are read while a watch shows them. Zero means 15 seconds."), `defaultMetricsPoll = 15 * time.Second`.

- [ ] **Step 1: Failing tests** in `kube/metrics_test.go`, on the fakes of `testhelpers_test.go`. Serve the metrics API in the dynamic fake with a reactor on `list` for the two GVRs (`e.dyn.PrependReactor("list", "pods", …)` matched by `action.GetResource() == PodMetrics`; or `dynamicfake.NewSimpleDynamicClientWithCustomListKinds` with `PodMetricsList`/`NodeMetricsList` and unstructured objects — whichever the fake accepts; the reactor is known to work). Add the two resources to discovery through `envConfig.resources` (a `metav1.APIResourceList{GroupVersion: "metrics.k8s.io/v1beta1", APIResources: [{Name: "pods", Kind: "PodMetrics", Namespaced: true, Verbs: [get list]}, {Name: "nodes", Kind: "NodeMetrics", Verbs: [get list]}]}` appended to `standardResources()`).
  - `TestMetricsServedFollowsDiscovery`: without the group `metricsServed(PodMetrics)` is false; with it true; after `disc.setResources` drops it and `c.refresh`, false.
  - `TestPollerListsAtOnceAndEveryInterval`: `MetricsPoll` 50 ms; acquire; within 1 s the reactor was called ≥ 3 times; `current()` has the two pods' usage keyed `default/a` → `{"c1": {CPU: 12, Mem: 34<<20}}` (from `"12m"` and `"34Mi"`); release; the count stops growing (wait 200 ms, read count, wait 200 ms, same).
  - `TestPollerIsShared`: two acquires give the same poller, one goroutine (count the lists: not doubled); release one keeps polling, release both stops.
  - `TestParseReadingSkipsBadItems`: an item without a name, one with `cpu: "abc"`, one good: the reading has the good one; the log function was called once with "does not parse".
  - `TestPollerRecordsAFailedList`: the reactor fails with `apierrors.NewServiceUnavailable`; `current()` returns the last good reading and the error; the log has one `WARN "metrics could not be read"`; the reactor works again → `INFO "metrics can be read again"`, error nil; the warning is not logged twice across three failed polls.
  - `TestPollerSignals`: `subscribe` receives after each list (at least once per 50 ms interval over 300 ms).
  - `TestCloseStopsPollers`: acquire, `Close`, no goroutine of the poller left (the test's `t.Cleanup(e.c.Close)` plus `goleak`-free check: `Close` returns and a later `acquireMetrics` is `errClosed`).
- [ ] **Step 2: Run** them — compile failure.
- [ ] **Step 3: Implement** per spec §4.1–4.3. The goroutine: `list(); for { select { case <-ctx.Done(): return; case <-ticker.C: list() } }` with `c.wg.Add(1)` before start. `list` uses `c.dyn.Resource(gvr).List(ctx, metav1.ListOptions{})`, parses, locks, stores, logs per §4.3, signals. Nodes are keyed by name with the inner key `""`.
- [ ] **Step 4: Run** `KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)" go test -race ./kube/` — pass.
- [ ] **Step 5: Commit** `kube: one poller per metrics kind, shared, every 15 s while a watch needs it`.

---

### Task 3: `kube` — metered tables and `ClusterUsage`

**Files:**
- Modify: `kube/watch.go` (typedWatch: poller, meter/strip, merged Changed), `kube/pseudo.go` (containersWatch the same), `kube/kube.go` (Reader.ClusterUsage, UsageWatch, ClusterUsage), `kube/session.go`
- Create: `kube/usage.go` (usageWatch), `kube/changed.go` (`mergeChanged`)
- Modify: `kube/kubetest/kubetest.go` (ClusterUsage on the fake: `SetUsage(u kube.ClusterUsage, ok bool)`; `UsageWatch` fake whose Changed fires on SetUsage)
- Test: `kube/metrics_watch_test.go`, `kube/usage_test.go`, an envtest case in `kube/read_envtest_test.go`

**Interfaces:**
- Consumes: Task 1's `render.Metered`, `MeterContainers`, `StripMetrics`; Task 2's pollers.
- Produces (spec §4.5):
  ```go
  type ClusterUsage struct { CPU, Mem int; Served bool }
  type UsageWatch interface { Current() (ClusterUsage, bool); Changed() <-chan struct{}; Close() }
  // on Reader:
  ClusterUsage() UsageWatch
  ```
  `mergeChanged(a, b <-chan struct{}) (out <-chan struct{}, stop func())`: a goroutine that forwards either into a channel with room for one, ended by stop.

- [ ] **Step 1: Failing tests.**
  - `TestPodsTableHasMetricColumnsOnlyWhenServed` (fakes, Task 2's discovery and reactor): without the group, a watch of pods has no `CPU` column and its rows have as many cells as columns; with it, the columns are at the spec's positions, and after a poll `default/a`'s `CPU` cell is `12`; a pod not in the reading is `n/a`.
  - `TestContainersTableIsMetered`: the containers of `default/a`: row `c1` `CPU` `12`.
  - `TestNodesTableIsMetered`: `CPU/A` from allocatable, `CPU` from the reading.
  - `TestWatchPicksUpTheAPILater` (Review Focus 3): open the watch without the group; add it to discovery and `c.refresh`; the next `Table` has the columns and acquired the poller (one poller in `c.pollers`).
  - `TestWatchLetsTheAPIGo` (Review Focus 5): the reverse: after the group is dropped, `Table` has no metric columns and `c.pollers` is empty once the watch was the only user.
  - `TestPodsServedButNotNodes` (Review Focus 2): discovery with `pods` only: pods metered, nodes not, `ClusterUsage().Current()` says `Served: false`.
  - `TestChangedFiresOnAPoll`: `Changed()` receives within two intervals without any pod change.
  - `TestFailedListShowsNA`: a failing reactor after one good poll: cells `n/a`, columns still there.
  - `TestClusterUsage`: two nodes allocatable 4000 mc / 8 GiB and 2000 mc / 4 GiB; readings 1500 mc / 3 GiB and 1500 mc / 3 GiB: `CPU` 50, `Mem` 50, ok true; before the first list ok false; `Close` of the watch releases the poller and the nodes' cache (`c.caches` idle timer set).
  - `TestCloseWithWatchesOpen` (Review Focus 4): open two metered watches and a usage watch, `Close` the Cluster: returns within a second, under `-race`.
  - envtest, in `read_envtest_test.go`: `TestNoMetricsAPIOnEnvtest`: a watch of pods against the real API server has no `CPU` column; `ClusterUsage().Current()` is `Served: false`.
  - `kubetest`: `TestFakeUsage`: `SetUsage` makes `Current` answer and `Changed` fire.
- [ ] **Step 2: Run** — compile failure.
- [ ] **Step 3: Implement** per spec §4.4–4.5. In `typedWatch.Table`: `served := w.c.metricsServed(metricsOf(w.res.GVR))` where `metricsOf` maps `Pods`→`PodMetrics`, `Nodes`→`NodeMetrics`, else none; acquire/release under `w.mu`; after the rows: if `w.poller != nil` and the renderer is `Metered`, `r, err := w.poller.current()`; for each row, `usage := r[key]` (nil when `err != nil`), `m.Meter(&row, obj, usage)`; else `t.Columns, t.Rows = render.StripMetrics(...)`. The containers watch the same with `MeterContainers`. `Changed`: merged channel made at creation from the cache's and a `pollChanged` channel the watch forwards the current poller's signal into (when a poller is acquired later, subscribe then; keep it simple: the watch's own `changed chan struct{}` (cap 1) and a `notify()` that both subscriptions call — subscribing a poller means a goroutine that forwards until unsubscribed).
- [ ] **Step 4: Run** `KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)" go test -race -count=2 ./kube/...` — pass.
- [ ] **Step 5: Commit** `kube: the tables of pods, containers and nodes are metered while the API is served; ClusterUsage for the header`.

---

### Task 4: `tui` — the header's CPU and MEM lines

**Files:**
- Modify: `tui/header.go`, `tui/root.go` (usage watch, waitUsage command, usageMsg), `tui/styles.go` (styleUp red, styleDown green)
- Test: `tui/header_test.go`, `tui/root_test.go`; goldens.

**Interfaces:**
- Consumes: `kube.Reader.ClusterUsage()`, `kubetest.Session.SetUsage`.

- [ ] **Step 1: Failing tests.**
  - `TestHeaderShowsUsageWhenServed`: `SetUsage({CPU: 12, Mem: 47, Served: true}, true)` → header lines 4 and 5 start `CPU:     12%` and `MEM:     47%`, no arrow; then `SetUsage({CPU: 14, Mem: 40, …})` → `12%` becomes `14% ↑`, `47%` becomes `40% ↓`; then the same again → no arrow.
  - `TestHeaderSaysNAWhileServedWithoutAReading`: `SetUsage({Served: true}, false)` → `CPU:     n/a`.
  - `TestHeaderHasNoUsageLinesWithoutTheAPI`: the default fake (not served): the header's lines 4 and 5 are blank on the left (as today); every line is still `w` wide.
  - `TestHeaderAt80` and the goldens: unchanged for the default fake; add `TestHeaderWithUsage80.golden`.
- [ ] **Step 2: Run** — fail.
- [ ] **Step 3: Implement**: root opens the watch in `Init` (a `tea.Cmd` that returns `usageMsg{u, ok}` after `<-Changed()`, re-issued after each message); `root.usage, root.prevUsage`; `header()` adds the two lines when `usage.Served`; arrow by comparing with the previous reading's number when the previous was `ok`. Close the watch with the root (where the root closes its views at the end).
- [ ] **Step 4: Run** `go test -race ./tui/` — pass; `go test ./tui/ -update` for the new golden only; check it by eye.
- [ ] **Step 5: Commit** `tui: CPU and MEM in the header while the cluster serves the metrics of nodes`.

---

### Task 5: metrics-server in the e2e, the README, the chart's comment, Appendix A

**Files:**
- Modify: `scripts/test-e2e.sh` (install metrics-server before kosh; wait for `apiservice/v1beta1.metrics.k8s.io` `Available`), `e2e/e2e_test.go` (tests below), `README.md` (Metrics section; RBAC; Not yet), `charts/kosh/values.yaml` (rbac comment), `docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md` (Appendix A rows for pods, containers, nodes; §23 entry naming this change).

- [ ] **Step 1: The script.** After `kind create cluster` and before kosh: `kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.7.2/components.yaml` (pin the version; if the dev shell has no network for curl, the e2e already pulls images, so apply is fine), then `kubectl -n kube-system patch deployment metrics-server --type=json -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'`, then `kubectl wait --for=condition=Available apiservice/v1beta1.metrics.k8s.io --timeout=180s` and `kubectl wait -n kube-system --for=condition=Available deployment/metrics-server --timeout=180s`. Log a `step`.
- [ ] **Step 2: Failing e2e tests** (`-tags e2e`, vetted with `GOOS=linux go vet -tags e2e ./e2e/`):
  - `TestPodsShowCPUAndMemory`: the pods table has `CPU` and `MEM` in its header line; the row of the test's running pod (`marker`) gets a number in the `CPU` column within 90 s (`n/a` allowed before).
  - `TestNodesShowAllocatable`: `:nodes` → header has `CPU/A`, the kind node's row has a number there at once.
  - `TestHeaderShowsClusterUsage`: `CPU:` and `MEM:` lines with `N%` within 90 s.
  - `TestMetricsColumnsFollowTheAPI` behind `KOSH_E2E_SLOW=1`: `kubectl delete apiservice v1beta1.metrics.k8s.io`; within 6 minutes the `CPU` column is gone; apply the components again (the pinned manifest) and within 6 minutes it is back. `t.Skip` without the variable.
- [ ] **Step 3: Docs.** README **Metrics** section (spec §7); `rbac.clusterRole` comment; "Not yet" loses the metrics line; Appendix A rows; §23 entry: "**§9, Appendix A.** With metrics-server (`2026-10-14-kosh-metrics-design.md`, v0.4.0) pods, containers and nodes have the CPU and memory columns, present while the API is served."
- [ ] **Step 4: Run** `scripts/test-e2e.sh` where kind runs (the Linux box, with the home directory redirected as before; or the Mac if kind is there). All pass; run the slow test once on the box and note the times in the ledger.
- [ ] **Step 5: Commit** `e2e and docs: metrics-server in the kind cluster, the README's Metrics section, Appendix A`.

---

## Self-review

- Spec coverage: §1 (Tasks 1, 3, 4), §2 (Task 1 `n/a`), §3 (Task 2 `metricsServed`, Task 3 per-`Table` check), §4 (Tasks 2, 3), §5 (Task 1), §6 (Task 4), §7 (Task 5), §8 (each task's tests; e2e in Task 5), §4.6 RBAC (Task 5 docs). §1.5 sorting needs no code: `Numeric` columns sort by `Order` today.
- Placeholders: none; every test is named with its inputs and expectations.
- Types: `render.Usage` (Task 1) is what `reading` (Task 2) holds and `Meter` (Task 1) takes; `ClusterUsage`/`UsageWatch` (Task 3) are what `tui` (Task 4) and `kubetest` (Task 3) use. `MeterContainers(rows, pod, usage)` is called from `containersWatch` (Task 3).
- Review Focus: each line names its task and test.
