# Task 6: render, services, nodes, storage and configuration

The renderers of services, nodes, namespaces, events, ingresses, configmaps, secrets, persistent volume claims, persistent volumes and storage classes.

**Read first:** the spec, §2, §9 and Appendix A (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); the contract and the helpers of the package: `render/render.go`, `render/cell.go`, `render/kinds.go`, and for the tests `render/helpers_test.go` and `render/render_test.go`.

**The reference** is k9s's `internal/render` package, read-only at `/private/tmp/claude-502/-Users-dragan-fables-for-robots/25996946-215e-4a11-ba9e-574cacaa75eb/scratchpad/k9s-src/repo/internal/render/`. For each kind the table below names the file to read. It tells you what every cell holds, how a status is worked out, and what makes an object invalid (k9s calls that `diagnose`). Write kosh's renderer for kosh, with `render.B`; where your logic follows the k9s file closely, say so in a comment at the top of yours (`// Follows k9s's internal/render/pod.go (Apache-2.0).`).


**Files:**
- Create: `render/svc.go`, `render/node.go`, `render/ns.go`, `render/ev.go`, `render/ing.go`, `render/cm.go`, `render/secret.go`, `render/pvc.go`, `render/pv.go`, `render/sc.go`, and a `_test.go` for each

**Interfaces:**
- Consumes: `Register`, `Renderer`, `Column`, `Row`, `Health` and its values, `B` and `NewRow`, `MapText`, `Join`, `Or`, the value constants, and the GVRs `Services`, `Nodes`, `Namespaces`, `Events`, `Ingresses`, `ConfigMaps`, `Secrets`, `PersistentVolumeClaims`, `PersistentVolumes`, `StorageClasses`.
- Produces: a registered renderer for each of those ten GVRs.

## The kinds

| Kind | File | k9s file | Columns | Health beyond VALID |
|---|---|---|---|---|
| services | `svc.go` | `svc.go` | NAMESPACE, NAME, TYPE, CLUSTER-IP, EXTERNAL-IP, SELECTOR\*, PORTS, LABELS\*, VALID\*, AGE | none |
| nodes | `node.go` | `node.go` | NAME, STATUS, ROLE, ARCH\*, TAINTS, VERSION, OS-IMAGE\*, KERNEL\*, INTERNAL-IP\*, EXTERNAL-IP\*, PODS, LABELS\*, VALID\*, AGE | `Error` when the node is not `Ready` (its STATUS then says so, as k9s builds it: `NotReady`, `Ready,SchedulingDisabled`…); a node that is ready and cordoned is `Pending` |
| namespaces | `ns.go` | `ns.go` | NAME, STATUS, LABELS\*, VALID\*, AGE | `Pending` while it is `Terminating` |
| events | `ev.go` | none: see below | NAMESPACE, LAST SEEN, TYPE, REASON, OBJECT, MESSAGE, SUBOBJECT\*, SOURCE\*, FIRST SEEN\*, COUNT\*, NAME\* | `Error` for an event whose TYPE is not `Normal` |
| ingresses | `ing.go` | none: see below | NAMESPACE, NAME, CLASS, HOSTS, ADDRESS, PORTS, AGE | none |
| configmaps | `cm.go` | `cm.go` | NAMESPACE, NAME, DATA, VALID\*, AGE | none |
| secrets | `secret.go` | `secret.go` | NAMESPACE, NAME, TYPE, DATA, VALID\*, AGE | none |
| persistentvolumeclaims | `pvc.go` | `pvc.go` | NAMESPACE, NAME, STATUS, VOLUME, CAPACITY, ACCESS MODES, STORAGECLASS, LABELS\*, VALID\*, AGE | `Pending` while the claim's phase is `Pending`; `Error` when it is `Lost` |
| persistentvolumes | `pv.go` | `pv.go` | NAME, CAPACITY, ACCESS MODES, RECLAIM POLICY, STATUS, CLAIM, STORAGECLASS, REASON, VOLUMEMODE\*, LABELS\*, VALID\*, AGE | `Pending` for `Pending` and `Released`; `Error` for `Failed` |
| storageclasses | `sc.go` | `sc.go` | NAME, PROVISIONER, RECLAIMPOLICY, VOLUMEBINDINGMODE, ALLOWVOLUMEEXPANSION, LABELS\*, VALID\*, AGE | none |

**The node's PODS column** in k9s is the number of pods on the node, which the renderer cannot know from the node alone. kosh's cell holds `status.capacity.pods`, the number the node can take, as a number (`B.Int`), or `NAValue` when the node does not say. Say this in a comment.

**A secret's DATA** is the number of its entries and nothing of their contents. No cell of a secret's row holds a value of the secret, and neither does `Row.Labels` hold anything but its labels. Give the test a secret with a recognisable value and assert that the value is in no cell.

**Events and ingresses have no k9s renderer**: k9s shows the API server's table for them. kosh's have the columns of `kubectl get`:

- *events*: LAST SEEN is the age of the event's `lastTimestamp`, or of `eventTime`, or of `firstTimestamp`, whichever is set, in that order, else `UnknownValue`; it is `Numeric`. TYPE and REASON are the event's. OBJECT is `kind/name` of the involved object with the kind in lower case (`pod/web-7d4b9`). MESSAGE is the message with every run of white space, line feeds included, made one space, and the ends trimmed. SUBOBJECT is the involved object's `fieldPath` or empty. SOURCE is the source's component and host joined by `, `, the reporting controller when the source is empty. FIRST SEEN is the age of `firstTimestamp`, `Numeric`. COUNT is `count`, or of the series when there is one, `Numeric`. NAME is the event's name. There is no AGE column.
- *ingresses*: CLASS is `spec.ingressClassName` or `MissingValue`. HOSTS is the hosts of the rules joined by commas, `*` when no rule names one. ADDRESS is the IPs and hostnames of `status.loadBalancer.ingress` joined by commas, empty when there are none. PORTS is `80`, or `80, 443` when the ingress has a TLS section.

**Capacities** (the claim's and the volume's) are printed as the quantity is written (`10Gi`) and sorted by its value in bytes.

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
