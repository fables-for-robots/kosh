# kosh: CPU and memory from metrics-server

The first piece of stage 3 (§22 of the stage 1 design): the CPU and
memory figures k9s shows from metrics-server, in the header and in the
tables of pods, containers and nodes. Pulses, xray and "used by" stay for
later.

This design adds to `2026-10-13-kosh-stage-1-design.md`; where it names a
section (§8, §9, §10), it is a section of that document. The user's
choices, made in conversation on 2026-10-14: all of what k9s shows, and
**the columns and the header's lines exist only while the cluster serves
the metrics API** (k9s shows them always, as `n/a`, which pushes `IP` and
`NODE` off an 80-column terminal for nothing).

## 1. What the user sees

1. **The header** has two more lines under `kosh:`, when the metrics API
   is served:

   ```
   Key:     draganm/admin   <0> all       <shift-n> Sort Name   …
   Cluster: v1.35.4         <1> default   <shift-a> Sort Age    …
   kosh:    v0.4.0                        <shift-s> Sort Status …
   CPU:     12% ↑                         <shift-o> Sort Column …
   MEM:     47%                           <ctrl-w>  Wide        …
   ```

   The number is the cluster's use of what its nodes can give: the sum
   over the nodes of what metrics-server measured, over the sum of the
   nodes' allocatable, as k9s computes it (`ClusterLoad`), floored to a
   whole percent. The arrow is the change since the reading before: `↑`
   when the number went up, `↓` when it went down, nothing when it did
   not or there is no reading before. k9s colours the arrow red and
   green; kosh does the same (`styleUp`, `styleDown`). While the API is
   served and no reading has come yet, or the last reading failed, the
   lines say `n/a`. Without the API there are no lines, and the header
   is as it is today.

2. **The pods table** has six more columns, in k9s's places, when the
   API is served:

   | Column | Cell | Sorts by |
   |---|---|---|
   | `CPU` | millicores the pod's containers use, summed: `12` | the number |
   | `%CPU/R` | that over the containers' CPU requests, summed, floored: `40`; `n/a` when the requests are 0 | the number, `n/a` as 0 |
   | `%CPU/L` | the same over the limits | the number |
   | `MEM` | mebibytes the pod's containers use, summed: `34` | the number |
   | `%MEM/R` | that over the memory requests | the number |
   | `%MEM/L` | that over the memory limits | the number |

   In order: `NAMESPACE, NAME, READY, STATUS, RESTARTS, LAST RESTART*,
   CPU, CPU/RL*, %CPU/R, %CPU/L, MEM, MEM/RL*, %MEM/R, %MEM/L, IP, NODE,
   …` (`*` wide), which is k9s's order with the columns kosh does not
   have left out. The six are shown narrow, as in k9s, aligned right,
   numeric for sorting. A pod the last reading did not cover (new, or
   pending) has `n/a` in all six.

3. **The containers table** of a pod has the same six, per container, in
   k9s's places: `IDX, NAME, IMAGE, READY, STATE, RESTARTS,
   PROBES(L:R:S), CPU, CPU/RL, %CPU/R, %CPU/L, MEM, MEM/RL, %MEM/R,
   %MEM/L, PORTS, VALID*, AGE`. A container the reading does not cover
   (an init container that ended, an ephemeral one) has `n/a`.

4. **The nodes table** has six: `CPU` (millicores used), `CPU/A`
   (allocatable millicores), `%CPU` (used over allocatable, floored),
   `MEM` (mebibytes used), `MEM/A`, `%MEM`, after `PODS` and before
   `LABELS*`, as in k9s. A node the reading does not cover has `n/a` in
   `CPU`, `%CPU`, `MEM`, `%MEM`; `CPU/A` and `MEM/A` come from the node
   itself and are always there while the API is served.

5. **Sorting.** The six columns sort as numbers through Sort Column
   (`shift-o`), as every numeric column does. k9s at the reference commit
   binds no key of its own to CPU or MEM on these views, and kosh adds
   none.

6. **Numbers.** Millicores and mebibytes are whole numbers without a
   unit, as k9s prints them (`toMc`, `toMi`: a byte count over 1024²,
   floored); `0` is `0`. A percentage is `floor(100 × used / of)`, and
   `n/a` where `of` is 0, as k9s's `ToPercentageStr`.

7. **Read-only keys** see all of it: a reading is a read.

8. **When the API is served and the reading fails** (metrics-server is
   down, its APIService is not Available, the ServiceAccount may not
   list it): the columns and the lines stay, every cell and both lines
   say `n/a`, and the log says why, once per poller until a reading
   works again (§4.3). Nothing is flashed: it is not the user's doing.

## 2. Where it differs from k9s

- The columns and the header's lines exist only while the metrics API is
  served (the user's choice, above).
- A pod, container or node the reading does not cover shows `n/a`, not
  `0` as k9s shows: `0` would say it was measured and uses nothing.
- k9s marks a `%CPU/L` or `%MEM/L` over 100 in colour. kosh does not in
  this change.
- k9s's thresholds, which flash a warning when the cluster's CPU or MEM
  passes a level, are not in this change.

## 3. Whether the API is served

Discovery (§8.3) already reads every group the API serves, every 5
minutes and at start, and the metrics API is one of them:
`metrics.k8s.io/v1beta1` with the resources `pods` and `nodes` (kinds
`PodMetrics` and `NodeMetrics`, listable, no watch). They are in
`Resources()` today, as generic kinds. **The metrics API is served for
pods when discovery found `pods.metrics.k8s.io`, and for nodes when it
found `nodes.metrics.k8s.io`**; `Cluster.metricsServed(kind)` says so
from the resources known, without a request.

A watch of pods, containers or nodes looks at that on every `Table` call,
so a metrics-server installed after kosh started shows up within 5
minutes, and one removed goes away within 5 minutes (the poller's reads
fail in between, §1.8). The header's lines follow the same.

## 4. `kube`: the pollers

### 4.1 What a poller is

The metrics API can be listed and not watched, so a reading is a poll.
**One poller per kind** (`pods.metrics.k8s.io`, `nodes.metrics.k8s.io`)
for the whole process, like the informers (§8.1): started by the first
watch that needs it, shared by all, **polling every `Options.MetricsPoll`
(default 15 s, metrics-server's own resolution)** while it has a user,
and **stopped as soon as it has none** (a poll is one request; there is
nothing to keep warm, unlike an informer's cache). It lists the whole
cluster in one request (`dyn.Resource(gvr).List`, all namespaces, as the
informers do), and keeps the last reading.

```go
// metricsPoller reads the metrics of one kind every opts.MetricsPoll
// while it has a user, and keeps the last reading.
type metricsPoller struct {
	c      *Cluster
	gvr    schema.GroupVersionResource
	cancel context.CancelFunc
	done   chan struct{}

	users int // guarded by c.mu, as kindCache.users is

	mu      sync.Mutex
	subs    map[chan struct{}]struct{}
	reading render.Metrics // nil until a list worked
	err     error          // of the last list, nil when it worked
	logged  bool           // the error of the last list is in the log
}
```

`Cluster.acquireMetrics(gvr) (*metricsPoller, error)` and
`Cluster.releaseMetrics(p)` are `acquire` and `release` without the idle
timer: a release that leaves no user cancels the poller and drops it
from `c.pollers` under `c.mu`. `acquireMetrics` after `Close` is
`errClosed`. The poller's goroutine lists at once, then every
`MetricsPoll`, until cancelled; `Close` waits for it through `c.wg`.

### 4.2 The reading

`render.Metrics` is the reading of one kind at one moment, parsed from
what the API sent, without the `k8s.io/metrics` module: the objects are
`unstructured`, and the two fields that matter are quantities.

```go
// Usage is what metrics-server measured of one container or one node:
// CPU in millicores, memory in bytes.
type Usage struct {
	CPU, Mem int64
}

// Metrics is a reading of one kind: for pods, the usage of each
// container of each pod, keyed "namespace/name" then the container's
// name; for nodes, the usage of each node, keyed by name under the
// container key "". A nil Metrics is no reading.
type Metrics map[string]map[string]Usage
```

Parsing (`kube/metrics.go`): a `PodMetrics` item is
`metadata.namespace`, `metadata.name`, `containers[].name`,
`containers[].usage.cpu`, `containers[].usage.memory`; a `NodeMetrics`
item is `metadata.name`, `usage.cpu`, `usage.memory`. Quantities are
parsed with `resource.ParseQuantity`; `MilliValue()` for CPU, `Value()`
for memory. An item that lacks a name, or whose quantity does not parse,
is left out of the reading and logged once per poller
(`"a metrics item does not parse and is left out"`). A list that fails
keeps the last reading and records the error (§4.3).

### 4.3 Errors and the log

- A list that fails sets `err` and, if `logged` is false, logs
  `WARN "metrics could not be read" kind=… err=…` and sets `logged`. A
  list that works after one that failed logs
  `INFO "metrics can be read again" kind=…` and clears both. The reading
  of a failed list is not dropped: the cells show `n/a` while `err` is
  set (§1.8), so that a reading two polls old is not shown as current.
- A list that fails with `NotFound` (the APIService went away before
  discovery noticed) is the same as any failure.
- The poller signals its subscribers after every list, worked or not, as
  `kindCache.signal` does, so that a table re-renders.

### 4.4 The watches

`typedWatch` of pods and of nodes, and `containersWatch`, gain a poller:

- On every `Table(now)`: if `c.metricsServed(kind)` and the watch holds
  no poller, acquire one (an error from acquire, `errClosed` apart, is
  logged once and the watch goes on without); if not served and it
  holds one, release it. So a watch opened before metrics-server was
  installed picks it up, and one opened before it was removed lets it
  go.
- With a poller: the table's columns are the renderer's with the metric
  columns (§5); each row is metered with the poller's reading (nil, or
  with `err` set, meters every cell `n/a`).
- Without: the metric columns and their cells are stripped from the
  table (§5).
- `Changed()` is a channel that receives from the cache's signal and
  the poller's: the watch merges the two into one with room for one
  signal (a goroutine per watch, ended by Close, as the merge of a
  table watch's poll and the session's end is done today where one
  exists; else a small `merge(a, b <-chan struct{}) (<-chan struct{},
  stop func())`).
- `Close` releases the poller it holds.

### 4.5 The header's reading

`Reader` gains:

```go
// ClusterUsage opens the cluster's use of its nodes' CPU and memory,
// for the header: a reading every Options.MetricsPoll while the API
// serves the metrics of nodes. It is nil, with ok false, while the
// API does not serve them. It ends when the session's context does, or
// at Close.
ClusterUsage() UsageWatch

// UsageWatch is the cluster's use, kept up to date.
type UsageWatch interface {
	// Current returns the last reading. ok is false while there is
	// none: the API does not serve the metrics of nodes, no list has
	// worked yet, or the last one failed.
	Current() (u ClusterUsage, ok bool)
	// Changed receives after Current may answer differently.
	Changed() <-chan struct{}
	Close()
}

// ClusterUsage is the cluster's use of its nodes, in whole percents of
// what they can give, as k9s computes it.
type ClusterUsage struct {
	CPU, Mem int
	// Served is whether the API serves the metrics of nodes now: the
	// header's lines exist while it does.
	Served bool
}
```

The watch holds the nodes' cache (acquired as `watchTyped` acquires it;
the allocatable of each node is in the node object) and the nodes'
poller, **both only while the API serves the metrics of nodes**, acquired
and released together, checked at open and every `MetricsPoll`: a
session on a cluster without metrics-server holds no nodes informer for
a header that shows nothing. *(As built; the first draft held the cache
for the session, and the whole-branch review showed what that costs on a
large cluster without metrics-server, and that a role without `list` on
`nodes` then runs a failing informer for every session.)* When the nodes
cannot be listed, the watch logs one warning naming the reason, so that
the header's `n/a` is explained.

`Current` computes, from what the two hold then: for every node of the
cache, allocatable CPU and memory; for every node the reading covers,
its usage; `CPU = floor(100 × Σusage / Σallocatable)`, `n/a` when the
sum of allocatable is 0. `Changed` receives at open, after each list of
the poller, when the hold changes, and once when the nodes' cache first
syncs — not when a node changes between two polls: the header's arrow is
to show the change from one poll to the next. `Close` closes `Changed`,
once nothing can signal it any more, so that a wait on it ends with the
session that opened the watch; every signal and the close go through one
lock, so that a session that has already ended when it opens the watch
cannot make a signal race the close.

### 4.6 RBAC

`get` and `list` on `pods` and `nodes` in the group `metrics.k8s.io` for
the columns, and `list` and `watch` on `nodes` (core) for the header's
lines, which read the nodes' allocatable. `cluster-admin`, the chart's
default, has them; the built-in `view` has the metrics but not the
nodes. The README says what a narrower role needs, beside what it says
for `pods/exec`.

## 5. `render`: metric columns

A renderer's columns are fixed (§9). The metric columns are part of a
renderer's columns, in k9s's places, marked:

```go
type Column struct {
	…
	// Metric: the cells come from metrics-server, through Meter. The
	// column is in the table only while the metrics API is served.
	Metric bool
}
```

`Row(obj, now)` fills a metric cell with `NAValue` and order 0: a row is
whole, with one cell for every column, whatever the metrics API. A
renderer whose columns include metric ones is `Metered`:

```go
// Metered is a renderer with columns from metrics-server.
type Metered interface {
	Renderer
	// Meter fills the metric cells of row, the row of obj, from usage,
	// the reading's usage of obj's containers (for a pod: keyed by the
	// container's name; for a node: the one entry ""). A nil usage
	// leaves the cells NAValue.
	Meter(row *Row, obj runtime.Object, usage map[string]Usage)
}
```

- `pod.Meter` sums the containers' usage (only the containers the pod's
  spec has, as k9s's `currentRes` sums what metrics-server sent, which
  is the running containers), and sets `CPU`, `MEM`, and the four
  percentages against the sums `podResources` already computes.
- `ContainerRows(pod, now)` keeps its signature; `MeterContainers(rows
  []Row, pod *corev1.Pod, usage map[string]Usage)` meters each row by
  its container's name against that container's requests and limits.
- `node.Meter` sets `CPU`, `%CPU`, `MEM`, `%MEM` from the entry `""`,
  and `CPU/A`, `MEM/A` from `status.allocatable`, which `Row` already
  sets (they are not `n/a` without a reading: the node knows them).

  So `CPU/A` and `MEM/A` are `Metric` columns (they belong to the
  group and go away with it) whose cells `Row` fills, not `n/a`.

`render.StripMetrics(cols []Column, rows []Row) ([]Column, []Row)`
drops the `Metric` columns and their cells from the columns and from
every row's `Cells` and `Order`; `kube` calls it when the watch holds no
poller. It lives in `render` so that its test sits beside the columns.

The renderers' tests (`render/*_test.go`, fixtures) cover `Row` with the
`n/a` cells, `Meter` with a usage, with a partial usage (a container
missing: the pod's sums are of what is there; the container's row is
`n/a`), with requests or limits 0 (`n/a` percentages), and
`StripMetrics` leaving the columns of stage 1 exactly (Appendix A of the
stage 1 design, which this change amends).

## 6. `tui`

- **The header** (`header.go`): the left block gains `CPU:` and `MEM:`
  lines when the root's `UsageWatch` says `Served`, formatted as §1.1:
  `12% ↑`. The root opens the watch in `Init` (`r.usage =
  e.sess.ClusterUsage()`), waits on its `Changed` through a command
  (`waitUsage`), keeps the last two readings for the arrow, and redraws.
  The header's height does not change (`headerHeight` 6 has the room:
  five lines of hints). The percentages and arrows are plain text in
  the header's test frames (`ansi.Strip`).
- **Tables** need nothing: columns and rows come from `kube`, and the
  numeric sort is by `Order`. `kubetest.Session.SetTable` serves tables
  with the six columns in the tests of the tui, and `SetUsage(u
  ClusterUsage, ok bool)` the header's reading; `ClusterUsage()` on the
  fake returns a watch over that.
- **Help and hints:** nothing new (no new keys).

## 7. The chart and the README

- `charts/kosh/values.yaml`: no new value. The comment on
  `rbac.clusterRole` names the metrics group among what a narrower role
  needs.
- README: a section **Metrics** after **Using it**: what shows when the
  cluster has metrics-server, that the columns and lines exist only
  then (and appear within 5 minutes of its installation), the `n/a`
  cases, and the RBAC. "Not yet" loses "Metrics: the CPU and memory
  columns" and keeps pulses. The columns table (if the README has one)
  and Appendix A of the stage 1 design gain the six columns of pods,
  containers and nodes, marked as present with metrics-server.

## 8. Tests

- `render`: as §5. The zero-object test of every renderer (`Register`'s
  `zero`) holds, with the metric cells `n/a`.
- `kube`, unit (client-go fakes): a dynamic fake that serves
  `metrics.k8s.io/v1beta1` lists for pods and nodes, and discovery
  resources with and without the metrics API. The poller: lists at
  once and every `MetricsPoll` (a short one in tests), stops when
  released, is shared by two watches, parses items and skips bad ones,
  records and logs a failing list once. The watches: columns present
  iff served, acquired on a `Table` after discovery gains the API,
  released after it loses it, rows metered by `namespace/name`,
  `Changed` fires on a poll. `ClusterUsage`: the percentages of two
  nodes, `ok` false before the first list, `Served` false without the
  API. `Close` with pollers running ends them (the race detector runs
  all of it).
- `kube`, envtest: the real API server of envtest serves no metrics
  API: a watch of pods has no `CPU` column, and `ClusterUsage` says not
  served. (That is the "no metrics-server" path, for real.)
- `tui`: the header with and without the usage, the arrow on a rise, a
  fall and no change; a pods table with the six columns sorted by `CPU`
  through Sort Column; golden frames updated.
- **End to end** (`scripts/test-e2e.sh`): metrics-server is installed in
  the kind cluster before kosh (the upstream manifest at a pinned
  release, with `--kubelet-insecure-tls`, as kind's own docs do), and
  the script waits for its APIService to be `Available`. Tests, with
  the real `ssh`: the pods table shows `CPU` and `MEM` columns and, for
  the test's running pod, a number in `CPU` (`n/a` is allowed for up to
  60 s after the pod started, then it must be a number); the nodes
  table shows `CPU/A` with a number; the header shows `CPU:` and `MEM:`
  with a percent. One test deletes the `v1beta1.metrics.k8s.io`
  APIService and sees the columns go away, then applies it again and
  sees them back; each takes up to 5 minutes, discovery's interval,
  which is what a user would meet. It runs only with `KOSH_E2E_SLOW=1`,
  which CI does not set; the ledger records a run of it on the Linux
  box.

## 9. Settled first

- Polling every 15 s is metrics-server's default resolution; polling
  faster shows the same numbers. k9s polls the header every 15 s too.
- One list of all pods' metrics for the cluster per poll, not one per
  namespace: a view filters, as the informers' caches are of all
  namespaces (§8.1). A cluster of some thousands of pods is a list of a
  few MB every 15 s while someone looks at pods.
- The poller stops at once when it has no user, unlike an informer
  (idle 10 min): there is no cache to keep warm, and a view that opens
  gets a reading within one request.
- Metrics are parsed from `unstructured` rather than through
  `k8s.io/metrics`: two quantities per item do not warrant a module.
- The `Metric` columns are in the renderer and stripped by `kube`, so
  that a row is always whole and a renderer stays a pure function of
  the object; `Meter` is the one place a reading meets a row.

## 10. Not in this change

- Pulses, xray, "used by", k9s's thresholds and its colouring of a
  percentage over 100.
- GPU columns.
- A metrics column on deployments or other workloads (k9s has none).
- A `kosh top`-like command.

## 11. Release

v0.4.0: a new feature, no change to what a key may do. Shipyard has
metrics-server and gets it with the usual rollout.
