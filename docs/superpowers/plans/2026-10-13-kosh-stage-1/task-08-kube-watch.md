# Task 8: kube, the cluster, discovery, informers and tables

The part of `kube` everything else of the package stands on: connecting, knowing the API's resources, the informers all sessions share, and the tables a session watches. Tasks 9 and 10 add the rest of `Session` in files of their own, against what this task builds, so the names below that they rely on are fixed.

**Read first:** the spec, §2, §3, §8, §12 and §17 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints and Review Focus (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); the contract: `kube/kube.go` and `kube/cluster.go`; `render/render.go` and `render/kinds.go`; `kube/kubetest/kubetest.go` to see how the UI will use what you build.

**Files:**
- Modify: `kube/cluster.go` (the bodies, and the fields of `Cluster`; signatures and documentation stay)
- Create: `kube/session.go`, `kube/discovery.go`, `kube/informers.go`, `kube/watch.go`, `kube/table.go`, `kube/relations.go`
- Create: `kube/stubs_read.go`, `kube/stubs_actions.go`, `kube/pseudo.go` (placeholders that Tasks 9 and 10 replace: see below)
- Create: `kube/discovery_test.go`, `kube/informers_test.go`, `kube/watch_test.go`, `kube/table_test.go`, `kube/relations_test.go`, `kube/envtest_test.go`, `kube/testhelpers_test.go`
- Create: `scripts/envtest-assets.sh`

**Interfaces:**
- Consumes: `keys.Grant`; `render.Renderer`, `render.Column`, `render.Row`, `render.For`, `render.Zero`, `render.All`; client-go's dynamic client, discovery client and typed clientset.
- Produces, exactly as the contract declares them: `RESTConfig`, `New`, `(*Cluster).Session`, `(*Cluster).Close`, and of `Session`: `Grant`, `ClusterVersion`, `Resources`, `Resolve`, `Watch`.
- Produces for Tasks 9 and 10, unexported, with exactly these names and types:

  ```go
  type Cluster struct {
  	cfg     *rest.Config          // nil in unit tests
  	typed   kubernetes.Interface
  	dyn     dynamic.Interface
  	log     *slog.Logger
  	opts    Options               // with every zero value replaced by its default
  	version string
  	// … what this task needs besides
  }

  // newCluster makes a Cluster from clients, without connecting: what New
  // is made of, and what unit tests use with client-go's fakes. resources
  // is what discovery would have found; nil means it is read from disc,
  // which may be nil only when resources is not.
  func newCluster(cfg *rest.Config, typed kubernetes.Interface, dyn dynamic.Interface, disc discovery.DiscoveryInterface, resources []Resource, opts Options) *Cluster

  type session struct {
  	c     *Cluster
  	ctx   context.Context
  	grant keys.Grant
  }

  // mayChange returns ErrReadOnly for a read-only grant.
  func (s *session) mayChange() error
  // maySee returns ErrReadOnly for a read-only grant and a Secret.
  func (s *session) maySee(ref ObjectRef) error

  // resourceFor returns the Resource of a kind in a group, as discovery
  // knows it: resourceFor("apps", "Deployment").
  func (c *Cluster) resourceFor(group, kind string) (Resource, bool)

  // podSelector returns the namespace and the selector of the pods that
  // belong to a deployment, replicaset, statefulset, daemonset, job or
  // service, read from the API now. ErrNoRelation for anything else. A
  // service without a selector gives labels.Nothing().
  func (c *Cluster) podSelector(ctx context.Context, ref ObjectRef) (string, labels.Selector, error)

  // acquire returns the cache of a typed kind, started when it was not
  // running, and counts one more user of it; release counts one less.
  func (c *Cluster) acquire(gvr schema.GroupVersionResource) (*kindCache, error)
  func (c *Cluster) release(k *kindCache)

  // kindCache is the informer of one kind.
  //   list returns the objects it holds, typed (*corev1.Pod …), in no order.
  //   get returns one by namespace and name.
  //   subscribe returns a channel with room for one signal that receives
  //   after anything of the kind changed, and a function that ends that.
  //   status reports whether the first list is in (synced), whether the
  //   watch is broken at the moment (stale), and the error of a list that
  //   failed.
  func (k *kindCache) list() []runtime.Object
  func (k *kindCache) get(namespace, name string) (runtime.Object, bool)
  func (k *kindCache) subscribe() (<-chan struct{}, func())
  func (k *kindCache) status() (synced, stale bool, err error)
  ```

## The placeholders

`Session` has more methods than this task builds. So that the package builds and `*session` is a `Session`:

- `kube/stubs_read.go` holds `YAML`, `Describe`, `SecretData`, `Logs`, `Owner`, `Images` and `Replicas` on `*session`, each returning `errors.New("not implemented")`. Task 9 deletes the file.
- `kube/stubs_actions.go` holds every method of `Actions` on `*session`, each returning `errors.New("not implemented")`. Task 10 deletes the file.
- `kube/pseudo.go` holds `func (s *session) watchPseudo(q Query) (Watch, error)` returning `ErrNoRelation`; `Watch` calls it for a query whose resource is `Containers` or `Rules`. Task 9 replaces the file.

Put nothing else in those three files.

## Behaviour

**Connecting**

1. `RESTConfig("")` is `rest.InClusterConfig()`; with a path it is that kubeconfig's current context (`clientcmd.BuildConfigFromFlags("", path)`).
2. `New` copies the config and sets its user agent (`opts.UserAgent`, default `kosh`), `QPS` 50 and `Burst` 100: an interactive tool, not a controller. It builds the three clients, reads the server's version within `ctx` (an error says the API could not be reached, and wraps the cause), discovers the resources, starts the background discovery, and returns.
3. `Close` stops every informer, every poll and the discovery. It may be called more than once. After it, nothing of the Cluster runs: check with a goroutine count in a test.

**Discovery (spec §8.3)**

4. The resources are those of `ServerPreferredResources`, less subresources (a name with `/`) and less those without the verb `list`. A group that fails to be discovered is logged and left out; the others are kept (`discovery.IsGroupDiscoveryFailedError`).
5. For a kind kosh has a renderer for (`opts.Renderers`, default `render.For`), `Resource.GVR` is the renderer's GVR and `Typed` is true, provided the API serves that group, version and resource; look at the full `ServerGroupsAndResources` for that, because the preferred version may be another. When the API does not serve it, the resource is what discovery prefers and is not typed.
6. `Resources()` returns them sorted by resource name, then group.
7. `Resolve` matches, without regard to case: the plural name, the singular name, a short name, the kind, and `plural.group` (`deployments.apps`, `widgets.example.com`). When several resources answer to a name, the typed one wins, then the one in the core group, then the first by group name. A name that nothing answers to triggers one new discovery, at most once in 5 seconds for the whole Cluster, and is looked up again; then `ErrUnknownResource`, wrapped with the name.
8. Discovery runs again every `opts.DiscoveryEvery` (default 5 minutes) until `Close`. A discovery that fails keeps what was known and logs.

**Informers (spec §8.1)**

9. A typed kind has one informer for the whole Cluster, over the **dynamic client**, for all namespaces: a `cache.SharedIndexInformer` on a `cache.ListWatch` whose list and watch functions call `c.dyn.Resource(gvr).List` and `.Watch`. One builder serves all 26 kinds, and client-go's fake dynamic client serves the tests. Do not use a typed informer factory: it cannot stop one kind.
10. A transform turns each object into its typed struct before it is stored: `runtime.DefaultUnstructuredConverter.FromUnstructured` into a new object from `render.Zero(gvr)`, with `managedFields` cleared. `kindCache.list` and `get` therefore return `*corev1.Pod` and the like, which is what a `render.Renderer` takes. An object that does not convert is logged once per kind and dropped.
11. The list and watch functions record what became of each call: a list that fails sets the error that `status` returns and that `Table.Err` shows; a list that succeeds clears it; a watch that cannot be opened, or ends with an error, sets `stale` until the next list or watch succeeds.
12. `acquire` starts the informer the first time and counts users; `release` counts down. A kind without users for `opts.IdleStop` (default 10 minutes) is stopped and its store dropped; `acquire` within that time finds it running. A timer does this, not a poll.
13. Every add, update and delete of the informer, and every change of `status`, signals every subscriber. Signals never block the informer: a subscriber's channel has room for one, and a signal that does not fit is dropped.

**Watch of a typed kind**

14. `Watch(q)` for a typed resource acquires the kind's cache, and resolves `q.Of`, when set, into a filter once, by reading the owner from the API (see Relations). It returns at once: it does not wait for the first list.
15. `Table(now)`: the columns are the renderer's; the rows are the renderer's row of every object that passes the filter, in no particular order (the UI sorts). `Namespaced` is the resource's. Before the first list is in, there are no rows and `Stale` is true; when the list failed, `Err` is the error and `Columns` and `Rows` are empty; when the watch is broken, the rows are the last known and `Stale` is true. An object whose renderer returns an error is left out and logged once per watch.
16. Without `Of`, the filter is the namespace: `q.Namespace` empty, or a resource that is not namespaced, passes everything.
17. `Changed()` receives after the cache's signal. `Close` unsubscribes and releases, once however often it is called. Every watch of a session is closed when the session's context is done.

**Relations (`Query.Of`)**

18. As the table in `kube/kube.go` says. `pods` of a deployment, replicaset, statefulset, daemonset, job or service: the pods in the owner's namespace that `podSelector` matches. `pods` of a node: the pods of every namespace whose `spec.nodeName` is the node's name. `jobs` of a cronjob: the jobs in its namespace with an owner reference of its UID.
19. The owner is read once, when the watch is opened; if it cannot be read, `Watch` returns that error. Any other pair of resource and owner is `ErrNoRelation`. `Namespace` of the query is not looked at when `Of` is set.
20. A query for `Containers` or `Rules` goes to `watchPseudo`.

**Tables of the other resources (spec §8.2)**

21. A resource that is not typed is listed in the API server's Table format: a GET of the resource's list path with `Accept: application/json;as=Table;v=v1;g=meta.k8s.io` and the parameter `includeObject=Metadata`. Hide this behind a small unexported interface (`tableLister`, one method that takes a context, a `Resource` and a namespace and returns a `*metav1.Table`), with the real one made from `c.cfg` and a fake for the unit tests.
22. A column definition with a priority above 0 is `Wide`. Names are upper-cased. A column of type `integer` or `number` is `Numeric`, sorted by its value. A column of type `date` shows the age of the timestamp in the cell (`duration.HumanDuration`) and is `Numeric`, sorted by the seconds; a cell that is not a timestamp is shown as it is.
23. A row's `Name`, `Namespace` and `Labels` come from the row's object, which is partial metadata. A row without an object has an empty name and is still shown.
24. For a namespaced resource, a NAMESPACE column is put in front and `Namespaced` is true, whether or not the query names a namespace.
25. The table is read when the watch opens and again every `opts.TablePoll` (default 2 seconds) for as long as it is open, by one goroutine per watch that ends with `Close`. `Changed` signals after each read whose rows differ from the last. A read that fails with nothing known gives `Err`; one that fails after a good one keeps the rows and sets `Stale`.
26. `Of` with a resource that is not typed is `ErrNoRelation`.

**The session**

27. `Session(ctx, grant)` returns a `*session`. `mayChange` and `maySee` are as declared above; `maySee` knows a Secret by `ref.Resource.Is("", "secrets")`.

## Tests

Unit tests use `newCluster` with `dynamicfake.NewSimpleDynamicClient` (give it a scheme with the core, apps, batch and rbac types and the list kinds it needs), `fake.NewSimpleClientset`, and a fake discovery (`fakediscovery.FakeDiscovery` from the fake clientset's `Discovery()`, with `Resources` set). Renderers in the tests are the tests' own, given through `Options.Renderers`: a tiny renderer for pods and one for jobs, with columns NAMESPACE, NAME and AGE. Do not depend on the renderers of Tasks 5 to 7. Put shared helpers in `kube/testhelpers_test.go`. Wait for conditions with a deadline, never with a fixed sleep.

| Test | What it shows |
|---|---|
| `TestDiscoverySkipsSubresourcesAndUnlistable` | `pods/log` and a resource without `list` are not in `Resources()` |
| `TestDiscoveryMarksTypedKinds` | pods are typed with the renderer's GVR; a kind whose renderer version the API does not serve is not typed |
| `TestDiscoveryKeepsGoingWhenAGroupFails` | one group's discovery fails: the others are there |
| `TestResolveNames` | table: plural, singular, short name, kind, `plural.group`, each in odd case |
| `TestResolvePrefersTypedThenCore` | `events` in the core group and in `events.k8s.io`: the core one |
| `TestResolveRediscoversOnceOnAMiss` | a resource added to the fake discovery after start is found by `Resolve`; two misses within 5 seconds discover once (count the calls) |
| `TestResolveUnknown` | `errors.Is(err, ErrUnknownResource)`, and the message has the name |
| `TestWatchShowsObjectsAndFollowsChanges` | pods in the fake: the table has their rows; a pod created through the fake is there after `Changed` fires; a deleted one is gone |
| `TestWatchBeforeTheFirstListIsStale` | a list that blocks: no rows, `Stale` true; when it returns, `Changed` fires and the rows are there |
| `TestWatchFiltersByNamespace` | two namespaces: a query for one shows only its pods; an empty namespace shows all |
| `TestWatchListErrorIsInTheTable` | the fake's list returns forbidden: `Table.Err` is that error, no rows |
| `TestWatchStaleWhenTheWatchBreaks` | the fake's watch fails after the list: the rows stay and `Stale` is true |
| `TestObjectsAreTypedAndWithoutManagedFields` | what the test renderer is given is `*corev1.Pod`, with `ManagedFields` nil |
| `TestInformerIsShared` | two watches of pods, from two sessions: one list call to the fake |
| `TestInformerStopsWhenIdle` | `IdleStop` of 20 ms: after both watches close, a new watch lists again; a watch reopened before that does not |
| `TestWatchClosesWithTheSession` | the session's context cancelled: the kind becomes idle and stops |
| `TestCloseStopsEverything` | after `Close`, the goroutines the Cluster started are gone (compare `runtime.NumGoroutine` before `newCluster` and after `Close`, with a deadline) |
| `TestPodsOfAWorkload` | table: a deployment, a statefulset, a daemonset, a replicaset, a job and a service, each with pods that match its selector and pods that do not, in its namespace and in another: only the matching pods of its namespace |
| `TestPodsOfAServiceWithoutSelector` | no rows |
| `TestPodsOfANode` | pods of two nodes in two namespaces: those of the one node |
| `TestJobsOfACronJob` | jobs with and without the owner reference |
| `TestNoRelation` | `pods` of a configmap, `jobs` of a deployment, `Of` with a generic resource: `ErrNoRelation` |
| `TestRelationOwnerMissing` | the owner does not exist: `Watch` returns the API's not-found error |
| `TestGenericTable` | a fake `tableLister` with columns of types string, integer and date and priorities 0 and 1: names upper-cased, `Wide` and `Numeric` set, the date shown as an age and sorted by seconds, NAMESPACE in front, names and labels from the rows' objects |
| `TestGenericTablePolls` | `TablePoll` of 10 ms: a change in what the fake returns is in the table after `Changed`; no signal when nothing changed |
| `TestGenericTableErrors` | a first read that fails: `Err`. A later one that fails: the rows stay, `Stale` |
| `TestGenericTableStopsWithClose` | after `Close` the fake is not called again |
| `TestGrantChecks` | `mayChange` and `maySee` for a full and a read-only grant, for a Secret and a ConfigMap |

`kube/envtest_test.go` runs against a real kube-apiserver with etcd, started by `sigs.k8s.io/controller-runtime/pkg/envtest`. It is skipped with `t.Skip("KUBEBUILDER_ASSETS is not set: run scripts/envtest-assets.sh")` when that variable is empty. One `TestMain`-free helper starts the environment once per test binary (`sync.Once`) and stops it in a `t.Cleanup` of the last test, or simply once per test function that needs it if that is fast enough.

| Test | What it shows |
|---|---|
| `TestEnvNewReadsVersionAndResources` | `New` against the real API: a version that starts with `v1.`, and pods, deployments and customresourcedefinitions among the resources, typed (with `render.For` these are typed only once Tasks 5 to 7 are merged: give `Options.Renderers` the test renderers here too) |
| `TestEnvWatchPods` | a pod created with the typed client appears in a watch of pods; deleted, it goes |
| `TestEnvGenericTableOfACustomResource` | a CRD with a printer column of type `integer` and one of type `date` is created, then an instance: `Resolve` of its plural name finds it without waiting 5 minutes, and its table has the columns and the row |
| `TestEnvForbiddenList` | a `Cluster` made from a config that impersonates a user without rights: the table of pods has `Err` and it is a forbidden error |

`scripts/envtest-assets.sh` prints the directory of the binaries, fetching them when they are not there:

```bash
#!/usr/bin/env bash
# Prints the directory that holds kube-apiserver and etcd for the tests
# that start an API server, fetching them on first use:
#
#   KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)" go test ./kube/...
set -euo pipefail
cd "$(dirname "$0")/.."
go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25 \
  use 1.34.x --bin-dir "${ENVTEST_BIN_DIR:-$PWD/.cache/envtest}" -p path
```

Run `go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25 list` first and use the newest `1.x` it offers for this machine in place of `1.34.x`. `.cache/` is in `.gitignore`.

## Steps

- [ ] `kube/testhelpers_test.go`: the fakes, the test renderers, `eventually`. `kube/session.go`, the three placeholder files, and `newCluster` with only its fields: the package builds.
- [ ] Discovery: tests first, see them fail, then `kube/discovery.go`. Commit: `Kube: the resources of the API, and the names they answer to`.
- [ ] Informers: tests first, then `kube/informers.go`. Commit: `Kube: one informer for each kind, shared, typed, and stopped when nobody looks`.
- [ ] Watch of typed kinds and relations: tests first, then `kube/watch.go` and `kube/relations.go`. Commit: `Kube: the table of a kind, of a namespace, and of what belongs to an object`.
- [ ] Generic tables: tests first, then `kube/table.go`. Commit: `Kube: the API server's table for every other resource`.
- [ ] `RESTConfig`, `New`, `Close`; `scripts/envtest-assets.sh`; `kube/envtest_test.go`. Run it: `KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)" go test -race ./kube/ -run TestEnv`. Commit: `Kube: a Cluster from a config, seen to work against an API server`.
- [ ] `go test -race ./kube/...`, with and without `KUBEBUILDER_ASSETS`; `gofmt -l . && go vet ./... && go build ./...` clean.

## Report

What you built and the test results, with and without the API server; the exact names and signatures of everything unexported that Tasks 9 and 10 will use, where they differ in any detail from the list above, and why; anything in the contract that did not fit.
