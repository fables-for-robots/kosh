# Task 10: kube, the actions and the grant

Everything a session does that changes the cluster, and the one place where a read-only key is stopped from doing it. A full key of kosh acts with the pod's ServiceAccount, which is usually a cluster administrator, so each action does exactly what it says, to exactly the object it was given, and leaves a line in the log that names the key.

**Read first:** the spec, §2, §3, §11, §12 and §13 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); the contract: `kube/kube.go` (`Actions`, `DeleteOptions`, `DrainOptions`, `ErrReadOnly`); and what Task 8 built that you stand on: `kube/cluster.go` and `kube/session.go` (`Cluster` with `typed`, `dyn`, `log`; `newCluster`; `session` with `c`, `ctx`, `grant`; `mayChange`; `resourceFor`), and `kube/testhelpers_test.go` for the fakes its tests use.

**Files:**
- Delete: `kube/stubs_actions.go`
- Create: `kube/actions.go` (delete, kill, update, scale, restart, set images, rollback, trigger, suspend), `kube/drain.go` (cordon, drain), `kube/audit.go` (the log line)
- Create: `kube/actions_test.go`, `kube/drain_test.go`, `kube/audit_test.go`, `kube/grant_test.go`, `kube/actions_envtest_test.go`
- Do not change any other file of `kube`. If a helper of Task 8 does not do what you need, say so in your report instead of changing it.

**Interfaces:**
- Consumes: `(*session).mayChange`, `session.c.typed` (`kubernetes.Interface`), `session.c.dyn` (`dynamic.Interface`), `session.c.log`, `session.grant`, `(*Cluster).resourceFor`; `k8s.io/kubectl/pkg/drain`, `k8s.io/kubectl/pkg/polymorphichelpers` (`RollbackerFor`), `sigs.k8s.io/yaml`.
- Produces: every method of `kube.Actions` on `*session`, exactly as `kube/kube.go` declares them.

## Behaviour

**The grant, first and everywhere**

1. Every method of `Actions` begins by calling `mayChange`. For a read-only grant it returns `ErrReadOnly` (the error itself, so `errors.Is` holds) **before anything is read from or sent to the API**, and logs the refusal (18).
2. A method that is given an object of a kind it is not for returns an error that says so (`cannot scale pods/default/web`) before anything is sent: `Kill` is for pods; `Scale` for deployments, statefulsets and replicasets; `Restart` and `SetImages` for deployments, statefulsets and daemonsets; `Rollback` for replicasets; `Trigger` and `SetSuspended` for cronjobs; `Cordon` and `Drain` for nodes. Kinds are told by `ref.Resource.Is(group, resource)`.

**The actions**

3. `Delete` deletes through the dynamic client, so it works for every resource: `PropagationPolicy` is the option's, `Background` when it is empty; `GracePeriodSeconds` is 0 when `Force` is set and not set otherwise. A namespaced resource is deleted in `ref.Namespace`.
4. `Kill` is a delete of the pod with a grace period of 0 and background propagation.
5. `Update` parses the YAML into an unstructured object. It refuses, before sending anything: YAML that does not parse; an `apiVersion`, `kind`, `metadata.name` or `metadata.namespace` that is not `ref`'s (`the YAML names another object: …`); a missing `metadata.resourceVersion` (`the YAML has no resourceVersion: it must be the object as it was read`). Then it updates through the dynamic client. The API's errors, a conflict and a validation failure among them, are returned as the API worded them.
6. `Scale` reads and writes the scale subresource (`GetScale`, `UpdateScale` of the typed client for the kind). A negative number is refused before anything is sent.
7. `Restart` patches `spec.template.metadata.annotations["kubectl.kubernetes.io/restartedAt"]` to the time now in RFC 3339, with a strategic merge patch, as `kubectl rollout restart` does.
8. `SetImages` reads the object, and refuses with the container's name when `images` names a container (or init container) its pod template does not have. Then it patches the images of the named containers, with a strategic merge patch that names each container, so that the others and every other field are untouched. An empty map changes nothing and sends nothing.
9. `Rollback` reads the replicaset, finds the deployment that controls it among its owner references (none: `replicaset … belongs to no deployment`) and the revision in its annotation `deployment.kubernetes.io/revision` (none: an error that says so), and rolls the deployment back to that revision with kubectl's rollbacker (`polymorphichelpers.RollbackerFor` for `apps/Deployment`, with the typed client; its `Rollback` with the revision and a dry-run strategy of none).
10. `Trigger` reads the cronjob and creates a job from its `spec.jobTemplate`, as `kubectl create job --from=cronjob/…` does: the template's labels and spec; the annotation `cronjob.kubernetes.io/instantiate: manual` beside the template's annotations; an owner reference to the cronjob with `controller: true`; the name `<cronjob>-manual-<5 random lower-case letters and digits>`, with the cronjob's name cut so that the whole is at most 63 characters. It returns the job's name.
11. `SetSuspended` patches `spec.suspend`.
12. `Cordon` sets or clears the node's unschedulable flag with `drain.RunCordonOrUncordon`.
13. `Drain` cordons the node and then evicts its pods with `drain.RunNodeDrain`, with a `drain.Helper` that carries the options (`GracePeriodSeconds`, `Timeout`, `IgnoreAllDaemonSets`, `DeleteEmptyDirData`, `Force`, `DisableEviction`), the session's context, and writers that collect what the helper prints, which goes into the log line and not to standard output. The helper's error is returned as it is.
14. Every call to the API uses the context it was given. Nothing here starts a goroutine that outlives the call.

**The log (spec §13)**

15. Every call of a method of `Actions` that got past the grant leaves exactly one line at info level through `c.log`, after the API has answered: message `action`; attributes `key` (the grant's name), `fingerprint`, `action` (`delete`, `kill`, `update`, `scale`, `restart`, `set-images`, `rollback`, `trigger`, `suspend`, `resume`, `cordon`, `uncordon`, `drain`), `object` (`ref.String()`), the action's own options (`propagation`, `force`; `replicas`; `images` as `name=image` pairs; `job` for a trigger; the drain options), and `error` with the error's text when it failed.
16. The line of an update holds nothing of the YAML.
17. A call that is refused for its kind (2) or its arguments is logged the same way, with its error.
18. A refusal of the grant is one line at warn level: message `refused`, attributes `key`, `fingerprint`, `action`, `object`.

## Tests

Unit tests use `newCluster` with client-go's fake clientset and fake dynamic client holding the same objects (Task 8's helpers show how), and a `slog` handler of the test's that keeps the records. An action's effect is asserted on the object in the fake afterwards, or on the fake's recorded actions where the fake does not apply a patch (a strategic merge patch against the fake dynamic client is not applied as the API would: assert the patch's type and body there).

`kube/grant_test.go`:

| Test | What it shows |
|---|---|
| `TestReadOnlyGrantIsRefusedEveryAction` | By reflection over `reflect.TypeOf((*Actions)(nil)).Elem()`: every method, called on a real `*session` with a read-only grant, a background context and zero values for its other arguments, returns an error for which `errors.Is(err, ErrReadOnly)` holds; and afterwards **neither fake has recorded a single action**. A method added to `Actions` later is covered without touching the test |
| `TestEveryMethodOfActionsTakesAContextAndReturnsAnError` | the shape the test above relies on: first parameter `context.Context`, last result `error` |
| `TestRefusalIsLogged` | one warn record with the key's name, the action and the object |

`kube/actions_test.go`:

| Test | What it shows |
|---|---|
| `TestDelete` | table: default options send background propagation and no grace period; foreground and orphan are sent as given; force sends a grace period of 0; a namespaced and a cluster-scoped object; a generic resource (a widget) is deleted through the dynamic client; a missing object returns the API's not-found error |
| `TestKill` | a pod is deleted with a grace period of 0; a deployment is refused and nothing is sent |
| `TestUpdate` | the edited YAML replaces the object in the fake; table of refusals with nothing sent: not YAML, another name, another namespace, another kind, another apiVersion, no resourceVersion |
| `TestUpdateConflict` | the fake returns a conflict: the error is the API's (`apierrors.IsConflict`) |
| `TestScale` | deployments, statefulsets and replicasets get the new count in their scale subresource; `-1` is refused; a pod is refused |
| `TestRestart` | the patch is a strategic merge patch that sets the annotation to a time within a minute of now; for all three kinds; a job is refused |
| `TestSetImages` | one of two containers: the patch names that container and its image and no other; an init container; a container that does not exist is refused by name and nothing is patched; an empty map sends nothing |
| `TestRollback` | a deployment with two replicasets of revisions 1 and 2: rolling back to the first makes the deployment's pod template that replicaset's; a replicaset without an owner, and one without the revision annotation, are refused |
| `TestTrigger` | the job exists with the template's spec and labels, the `instantiate: manual` annotation, the owner reference with `controller: true`, and a name of the form `<cronjob>-manual-xxxxx`, which is returned; a cronjob with a name of 60 characters gives a job name of at most 63 |
| `TestSetSuspended` | `spec.suspend` true, then false |
| `TestWrongKindIsRefusedBeforeAnythingIsSent` | each method with a configmap's reference: an error that names the action and the object; no action recorded on either fake |

`kube/drain_test.go`:

| Test | What it shows |
|---|---|
| `TestCordonAndUncordon` | the node's `spec.unschedulable` |
| `TestDrain` | a node with a plain pod, a daemonset's pod and a pod with an emptyDir: with `IgnoreDaemonSets` and `DeleteEmptyDirData` the node is cordoned and the plain and the emptyDir pods are evicted or deleted while the daemonset's stays; without `DeleteEmptyDirData` the drain fails with the helper's error and names the pod |
| `TestDrainPrintsNothing` | nothing is written to `os.Stdout` or `os.Stderr` |

`kube/audit_test.go`:

| Test | What it shows |
|---|---|
| `TestEveryActionIsLoggedOnce` | table over every method with valid arguments: exactly one info record with message `action`, the key's name, the action's word, `object`, and the options of 15 |
| `TestFailedActionIsLoggedWithItsError` | the fake fails the call: the record has `error` |
| `TestUpdateLogHoldsNoYAML` | a YAML with a recognisable value: no record's attributes hold it |

`kube/actions_envtest_test.go` (skipped without `KUBEBUILDER_ASSETS`, like Task 8's): against the real API, with a `Cluster` from `New`:

| Test | What it shows |
|---|---|
| `TestEnvScaleRestartSetImages` | a deployment: the replicas, the restart annotation and the image are what the API then returns, and nothing else of the spec changed |
| `TestEnvUpdateAndConflict` | the YAML read with the dynamic client, edited, updates the object; sent a second time unchanged, it is a conflict |
| `TestEnvDeleteGeneric` | an instance of a custom resource is deleted |
| `TestEnvTriggerAndSuspend` | a cronjob: the job exists and is owned; `spec.suspend` follows |
| `TestEnvCordon` | a node object created in the test is cordoned and uncordoned |

## Steps

- [ ] `kube/grant_test.go`; delete `kube/stubs_actions.go`; the methods in `kube/actions.go` and `kube/drain.go` with the grant check and nothing else; the reflection test passes. Commit: `Kube: every action refuses a read-only key before it does anything`.
- [ ] Delete, kill, update: tests first. Commit: `Kube: delete, kill and update`.
- [ ] Scale, restart, set images, rollback: tests first. Commit: `Kube: scale, restart, set images and roll back`.
- [ ] Trigger, suspend, cordon, drain: tests first. Commit: `Kube: trigger and suspend a cronjob, cordon and drain a node`.
- [ ] The log: tests first, then `kube/audit.go` and its calls. Commit: `Kube: every action is logged with the name of its key`.
- [ ] The API-server tests: `KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)" go test -race ./kube/ -run TestEnv`. Commit: `Kube: the actions seen to work against an API server`.
- [ ] `go test -race ./kube/...` with and without the assets; `gofmt -l . && go vet ./... && go build ./...` clean.

## Report

What you built and the test results, with and without the API server; for each action, how its effect is asserted (the object, or the recorded patch) and why; anything of Task 8 that you needed changed; anything in the contract that did not fit.
