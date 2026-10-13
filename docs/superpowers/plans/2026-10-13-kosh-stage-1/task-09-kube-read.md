# Task 9: kube, YAML, describe, logs and the rest of reading

What a session reads besides tables: an object's YAML and description, a Secret decoded, logs, an object's owner, a workload's images and replicas, and the two tables that are no resources, a pod's containers and a role's rules.

**Read first:** the spec, §2, §8.4, §10.3, §10.4 and §12 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints and Review Focus (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); the contract: `kube/kube.go` (`Reader`, the log types and constants, `SecretEntry`, `ContainerImage`, the errors); what Task 8 built that you stand on: `kube/cluster.go`, `kube/session.go`, `kube/informers.go`, `kube/watch.go`, `kube/relations.go` (`Cluster` with `cfg`, `typed`, `dyn`, `log`, `opts`; `newCluster`; `session`; `maySee`; `resourceFor`; `podSelector`; `acquire`, `release`, `kindCache`), and `kube/testhelpers_test.go`; and `render/container.go` and `render/rule.go` for `ContainerColumns`, `ContainerRows`, `RuleColumns`, `RuleRows`.

**Files:**
- Delete: `kube/stubs_read.go`
- Replace: `kube/pseudo.go` (the placeholder `watchPseudo` becomes the real one)
- Create: `kube/yaml.go`, `kube/describe.go`, `kube/secret.go`, `kube/logs.go`, `kube/owner.go`, `kube/workload.go`
- Create: `kube/yaml_test.go`, `kube/secret_test.go`, `kube/logs_test.go`, `kube/owner_test.go`, `kube/workload_test.go`, `kube/pseudo_test.go`, `kube/read_envtest_test.go`
- Do not change any other file of `kube`. If a helper of Task 8 does not do what you need, say so in your report instead of changing it. The one exception: `Cluster` needs one new field for how a log is opened (below); add that field and its default in `newCluster`, and nothing else, to `kube/cluster.go`.

**Interfaces:**
- Consumes: what the list above names; `k8s.io/kubectl/pkg/describe`; `sigs.k8s.io/yaml`.
- Produces on `*session`, exactly as `kube/kube.go` declares them: `YAML`, `Describe`, `SecretData`, `Logs`, `Owner`, `Images`, `Replicas`; and `watchPseudo(q Query) (Watch, error)`.

## Behaviour

**YAML**

1. `YAML` first calls `maySee(ref)`: a read-only grant is refused a Secret with `ErrReadOnly`, before anything is read.
2. It reads the object with the dynamic client, now, removes `metadata.managedFields` unless they are asked for, and returns it as YAML (`sigs.k8s.io/yaml`). The text holds `metadata.resourceVersion`, which `Update` needs.

**Describe**

3. `Describe` calls `maySee(ref)` first.
4. It returns what `kubectl describe` prints: `describe.DescriberFor` for the object's group and kind with `c.cfg`, and for a kind it has no describer for, `describe.GenericDescriberFor` with a REST mapping made from the `Resource` (its GVR, its kind, its scope). Events are shown (`describe.DescriberSettings{ShowEvents: true}`).
5. With a `Cluster` that has no config (unit tests) it returns an error that says so; it is tested against the API server.

**A Secret decoded**

6. `SecretData` returns `ErrReadOnly` for a read-only grant, whatever `ref` is, and an error for a `ref` that is not a Secret.
7. It reads the Secret with the typed client and returns its `data` entries, values as the bytes are (the API has already decoded the base64), sorted by key. A Secret without data gives no entries.

**Logs**

8. For a pod, `Logs` reads one container: `opts.Container`, or the pod's first container when that is empty; with `AllContainers`, every init container, container and ephemeral container of the pod. For a deployment, replicaset, statefulset, daemonset, job or service it reads every container of every pod that `podSelector` matches at that moment, and `opts.Container` and `opts.Previous` are not looked at. Any other kind is an error.
9. At most `LogMaxContainers` containers are read, in the order pods and containers come; `Skipped` is how many were left out.
10. Each container's log is opened with `PodLogOptions`: `Timestamps` always; `Follow` unless `Previous` or `From` is `LogHead`; `TailLines` of `LogTailLines` for `LogTail`; `SinceSeconds` for `LogSince`; `Previous` as given. How a log is opened is a field of the `Cluster`, a function from context, namespace, pod and `*corev1.PodLogOptions` to an `io.ReadCloser`, whose default asks the typed client (`GetLogs(…).Stream`), so that tests give streams of their own.
11. A line of the stream is `<RFC 3339 time> <text>`: the time goes to `LogLine.Time` and the rest to `Text`; a line without a time that parses keeps its whole text and a zero time.
12. **A line is at most 64 KiB** (Review Focus 4). A longer one is delivered cut at that length with `…` at its end, and what follows up to the next line feed is read and dropped, without being held in memory. A stream that never sends a line feed therefore costs at most that much memory, and delivers its line when the limit is reached, not never.
13. Lines are delivered in batches: what a container's reader has after 50 ms, or 256 lines, whichever is first. `Lines()` is not closed while any container is still being read.
14. With `LogHead` the whole stream ends after `LogBufferLines` lines in total.
15. A container whose log cannot be opened (it has not started, it has no previous run) contributes one line that says so, in the error's words, with its pod and container, and the others go on. `Lines()` is closed when every container's log has ended; `Err()` is nil then, and it is the first error when a stream broke in the middle.
16. `Close`, and the end of the session's context, close every stream and close `Lines()`. No goroutine of the stream is left afterwards.

**Owner, images, replicas**

17. `Owner` reads the object's metadata with the dynamic client. No owner references: `ErrNoOwner`. Otherwise the reference with `controller: true`, or else the first. Its `Resource` is `resourceFor` of the reference's group and kind (unknown: an error that names the kind); its namespace is the object's when the owner's resource is namespaced.
18. `Images` reads a deployment, statefulset or daemonset with the typed client and returns the containers of its pod template, init containers first, each with `Init` set accordingly. Any other kind is an error.
19. `Replicas` returns `spec.replicas` from the scale subresource of a deployment, statefulset or replicaset. Any other kind is an error.

**The containers and the rules**

20. `watchPseudo` for `Containers` wants `q.Of` to be a pod (else `ErrNoRelation`). It acquires the pods' cache. `Table(now)` is `render.ContainerColumns()` and `render.ContainerRows(pod, now)` of that pod as the cache has it; a pod that is gone gives no rows. `Namespaced` is false: the rows are of one pod. `Stale` and `Err` follow the cache's status, as Task 8's tables do. `Changed` follows the cache. `Close` releases it.
21. `watchPseudo` for `Rules` wants `q.Of` to be a role or a clusterrole, whose rules it shows, or a rolebinding or clusterrolebinding, whose `roleRef` names the role or clusterrole whose rules it shows (a rolebinding may name either; a role is looked up in the binding's namespace). It reads them with the typed client when the watch opens, and again every `opts.TablePoll`, by a goroutine that ends with `Close`; `Changed` signals when the rows differ. `Table` is `render.RuleColumns()` and `render.RuleRows(rules)`. A read that fails with nothing known gives `Err`; one that fails later keeps the rows and sets `Stale`.

## Tests

Unit tests use `newCluster` with the fakes, as Task 8's helpers do.

| Test | What it shows |
|---|---|
| `TestYAML` | a configmap: the YAML parses back to the object; it has `resourceVersion`; it has no `managedFields`, and has them when asked |
| `TestYAMLOfASecretIsRefusedReadOnly` | read-only: `ErrReadOnly` and no action recorded on the fakes; full: the YAML |
| `TestDescribeOfASecretIsRefusedReadOnly` | the same for `Describe` |
| `TestSecretData` | entries sorted by key, values as the bytes are, binary included; no data: none; read-only: `ErrReadOnly` with nothing read; a configmap's reference: an error |
| `TestLogsOfAPod` | table over `LogOptions`: the `PodLogOptions` the opener is given for tail, head, since, previous, a named container, the default container, all containers |
| `TestLogLinesAreParsed` | a time and a text; a line without a time; an empty line |
| `TestLogLongLineIsCut` | a line of 1 MiB: one `LogLine` of 64 KiB ending in `…`, and the line after it arrives whole |
| `TestLogWithoutLineFeedIsBounded` | a stream that sends 8 MiB without a line feed, from a reader that produces it on the fly: a cut line arrives while the stream is still open, and the heap did not grow by more than 1 MiB (`runtime.ReadMemStats` before and after, after a GC) |
| `TestLogBatches` | 1000 lines written at once arrive in batches of at most 256; a single line arrives within 200 ms |
| `TestLogHeadEnds` | `LogHead` with a stream of 6000 lines: `Lines()` closes after 5000 |
| `TestLogsOfAWorkload` | a deployment with two pods of two containers each: four openers, lines carry their pod and container |
| `TestLogsSkipBeyondTheLimit` | 20 pods of two containers: 30 are opened, `Skipped()` is 10 |
| `TestLogContainerThatCannotBeOpened` | one opener fails: a line with the error for that container, the other container's lines arrive, `Err()` is nil at the end |
| `TestLogStreamBreaks` | a stream that returns an error in the middle: `Lines()` closes and `Err()` is that error |
| `TestLogClose` | `Close` closes every stream given by the opener and `Lines()`; so does the end of the session's context; no goroutine is left (count with a deadline) |
| `TestLogsOfAnotherKind` | a configmap: an error |
| `TestOwner` | a pod owned by a replicaset: the replicaset's reference in the pod's namespace; a controller among several owners is preferred; a namespaced object owned by a cluster-scoped one has no namespace in the owner; none: `ErrNoOwner`; an owner of an unknown kind: an error that names it |
| `TestImages` | init containers first, flagged; the three kinds; a pod's reference: an error |
| `TestReplicas` | the three kinds; another: an error |
| `TestContainersOfAPod` | rows for the pod's containers (use the real `render.ContainerRows`); a change of the pod is followed after `Changed`; the pod deleted: no rows; `Of` that is not a pod: `ErrNoRelation`; `Close` releases the pods' cache (it becomes idle) |
| `TestRulesOfARole` | a role; a clusterrole; a rolebinding to a role; a rolebinding to a clusterrole; a clusterrolebinding: each shows the right rules |
| `TestRulesFollowAChange` | `TablePoll` of 10 ms: a rule added to the role is in the table after `Changed` |
| `TestRulesErrors` | the role does not exist: `Err`; `Of` that is a pod: `ErrNoRelation`; after `Close` the fake is not read again |

`kube/read_envtest_test.go` (skipped without `KUBEBUILDER_ASSETS`), against the real API with a `Cluster` from `New`:

| Test | What it shows |
|---|---|
| `TestEnvDescribe` | a configmap's description holds its name and a data key; a deployment's holds `Replicas:`; an instance of a custom resource is described by the generic describer and holds its name |
| `TestEnvDescribeSecretShowsNoValue` | a Secret with a recognisable value, full grant: the description holds the key and not the value |
| `TestEnvYAMLRoundTrip` | `YAML` of a configmap, parsed, equals the object read with the typed client, less managed fields |
| `TestEnvOwner` | a replicaset made with an owner reference to a deployment: `Owner` returns the deployment |

Logs cannot be read from an API server without a kubelet; they are seen for real in Task 19.

## Steps

- [ ] Delete `kube/stubs_read.go`; YAML and the Secret: tests first. Commit: `Kube: an object's YAML and a Secret's entries, and neither of a Secret for a read-only key`.
- [ ] Logs: tests first. Commit: `Kube: logs of a pod and of a workload's pods, bounded in every direction`.
- [ ] Owner, images, replicas: tests first. Commit: `Kube: an object's owner, a workload's images and replicas`.
- [ ] The containers and the rules: tests first. Commit: `Kube: the tables of a pod's containers and of a role's rules`.
- [ ] Describe and the API-server tests: `KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)" go test -race ./kube/ -run TestEnv`. Commit: `Kube: describe, as kubectl prints it`.
- [ ] `go test -race ./kube/...` with and without the assets; `gofmt -l . && go vet ./... && go build ./...` clean.

## Report

What you built and the test results, with and without the API server; what you added to `kube/cluster.go`; anything of Task 8 that you needed changed; anything in the contract that did not fit.
