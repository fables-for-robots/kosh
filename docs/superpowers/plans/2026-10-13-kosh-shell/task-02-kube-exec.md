# Task 2: `kube`: Exec

The API's exec stream with a terminal, as an action of the session: `Exec` runs a command in a container until it ends, or until its context or the session does. This task knows nothing of SSH or of the UI: its terminal is a reader, a writer, a function that says a size and a channel that says the size changed.

**Read first:** the spec §4, §7 and §10 (`docs/superpowers/specs/2026-10-13-kosh-shell-design.md`); the plan's Global Constraints and Review Focus (`docs/superpowers/plans/2026-10-13-kosh-shell.md`); the probe's report, "Q5. The exec stream against a real cluster" and "`kube`: `Exec`" under "Recommended design" (`docs/superpowers/plans/2026-10-13-kosh-shell/task-00-probe.md`), and the probe's own `Exec` (its location is in your dispatch): it ran against a real cluster; build on it, do not rediscover it. The contract: `kube/kube.go` (`Exec`, `ExecOptions`, `ExecResult`, `ErrExecNotStarted`). The code this joins: `kube/audit.go` (`begin`, `logAction`, `logRefusal`, `loggableError`), `kube/session.go` (`bound`), `kube/yaml.go` (`checkRef`), `kube/actions.go` for how an action is written, `kube/logs.go` for how a stream is tied to the session, `kube/cluster.go` for what the `Cluster` holds (`cfg`, the clients).

**Files:**
- Modify: `kube/exec.go` (the stub becomes the thing), `kube/audit.go` only if the log needs a helper it does not have
- Create: `kube/exec_test.go`
- No other file. `go.mod` does not change (Task 1 put `k8s.io/client-go/tools/remotecommand` and `k8s.io/streaming` there).

**Interfaces:**
- Consumes: `remotecommand.NewWebSocketExecutor(cfg, "GET", url)` and `StreamWithContext` with `Tty: true`; `k8s.io/streaming/pkg/httpstream`'s `UpgradeFailureError` (NOT the type of the same name in `k8s.io/apimachinery/pkg/util/httpstream`, which answers false for client-go's errors); `k8s.io/client-go/util/exec`'s `CodeExitError`.
- Produces: `(*session).Exec`, as `kube/kube.go` documents it.

## Behaviour

1. **It is an action.** `begin` first, with the action's word `exec`: a read-only grant gets `ErrReadOnly`, an ended session its refusal, both logged, and nothing is sent. The context it runs with ends with the caller's and with the session's. Then `checkRef` on the pod's reference, which must be a pod's (`pods` of the core group); `opts.Container`, when it is not empty, must be a DNS-1123 label; `opts.Command` must have at least one word, and its words are not looked at otherwise. A refused reference, container or command is an error before any request, and is logged as the action's failure.
2. **The request** is the pod's `exec` subresource with `container`, each word of the command as a `command` parameter, `stdin`, `stdout` and `tty` true and `stderr` false, built with the typed client's REST client and the scheme's parameter codec as kubectl builds it (`VersionedParams(&corev1.PodExecOptions{…}, scheme.ParameterCodec)`): no path or query is put together from strings.
3. **The stream** is the WebSocket executor alone. `Stdin` and `Stdout` are the options'; the size queue gives `opts.Size()` once at the start and again each time `opts.Resized` has a value, and ends (returns nil from `Next`) when the stream's context is done. A nil `Size` sends no size; a nil `Resized` sends the first size only. A size of zero or less in either direction is not sent.
4. **Output is counted**: the writer the stream is given counts the bytes that pass, so that behaviour 6 can tell a command that never started.
5. **How it ends, and what comes back:**
   - the stream returns nil: `ExecResult{Started: true, Status: 0}`, nil;
   - an exit status (`CodeExitError`): `ExecResult{Started: true, Status: n}`, nil;
   - the context was done (the caller's or the session's): `ExecResult{Started: …}` and the context's error (`context.Cause`, so that the session's end can be told from the caller's);
   - an `UpgradeFailureError` whose cause is an API status error: that status error (so that `apierrors.IsForbidden`, `IsNotFound`, `IsBadRequest` hold on what `Exec` returns);
   - any other error with no byte of output counted: `ErrExecNotStarted` wrapping the reason, which is the error's text with every leading `Internal error occurred: ` taken off, and, when the text holds `exec: "`, the part from there on (the runtime's own words: `exec: "sh": executable file not found in $PATH`), cut to 1 KiB;
   - any other error after output: `ExecResult{Started: true}` and the error.
6. **The log.** Two records with the action's attributes (key, fingerprint, `action=exec`, the object) at info level, as `logAction` writes them:
   - at the start, once the checks of behaviour 1 have passed: `container` and `command` (the words, each cleaned of control characters and the whole cut to 1 KiB) and `phase=start`;
   - at the end: `phase=end`, `container`, `duration`, `started`, `status` when it started, and for an error what `loggableError` makes of it (an API error as reason and code; the session's end as `ended="the session ended"`; `ErrExecNotStarted` as `error="the command could not be started"`, without the runtime's words, which may quote the command).
   Nothing of the stream's content is ever logged.
7. **Nothing is left**: when `Exec` returns, the stream's goroutines have ended or will end without reading `Stdin` again in a way that takes a byte from the next reader (client-go's stdin copier may be in one last `Read`: the contract says so, and the caller's reader ends with its turn; do not try to fix client-go, but do not start goroutines of your own that outlive `Exec`).
8. Client-go logs a cancelled stream through klog at error level (`Copying stdout failed`, `Waiting for server to close stdin failed`); kosh's klog routing lowers those to debug. Nothing to do; say in the report if you saw them in a test's log.

## Tests

The stream needs a kubelet, which no unit test has. What is tested here is everything around it, and the stream against a server of the test's own:

| Test | What it shows |
|---|---|
| the two tests that try every action (`TestReadOnlyGrantIsRefusedEveryAction`, `TestActionsEndWithTheSession`) | still pass with the real `Exec`: refused, logged once, nothing sent |
| `TestExecChecksBeforeItSends` | a reference that is not a pod, a pod name with a slash, a container `a/b`, an empty command: an error, no request (the test's HTTP server saw none), the action's failure logged |
| `TestExecRequest` | against an `httptest` server standing in for the API (the `Cluster`'s config pointed at it): the request is `GET …/api/v1/namespaces/default/pods/web/exec` with `container=web`, `command=sh&command=-c&command=a b` in that order, `stdin=true`, `stdout=true`, `tty=true`, no `stderr=true`, and asks for a WebSocket upgrade with the protocol `v5.channel.k8s.io` first |
| `TestExecStream` | the test's server speaks the exec protocol over WebSocket (`gorilla/websocket` is in `go.mod`; channel bytes as `remotecommand` v5 has them: read the library for the framing, or use `k8s.io/streaming`'s server side if it has one): what the test writes to `Stdin` arrives on the server's stdin channel; what the server writes to stdout arrives on `Stdout` byte for byte, a bare line feed included; the first size and a later one arrive on the resize channel as JSON `{"Width":…,"Height":…}`; the server ends with success: `Started` true, `Status` 0 |
| `TestExecStatus` | the server ends with a status of exit code 7 in the error channel: `ExecResult{true, 7}`, nil error |
| `TestExecNotStarted` | the server fails the stream with the containerd text of the probe before any output: `errors.Is(err, ErrExecNotStarted)`, `Started` false, and the error's text is `… exec: "sh": executable file not found in $PATH` without `Internal error occurred` and without the 64 hex of the exec's id; the same for `container not found ("web")` |
| `TestExecRefusedByTheAPI` | the server answers the upgrade with a 403 status object, with a 404, with a 400: `apierrors.IsForbidden` / `IsNotFound` / `IsBadRequest` on the returned error |
| `TestExecEndsWithItsContext` | the caller's context cancelled while the stream is open: `Exec` returns within a second with `context.Canceled`; the same when the session's context ends, and then `context.Cause` of what is returned says the session ended |
| `TestExecLog` | a shell that ran and ended with 0: two records, start and end, with the attributes of behaviour 6; one that did not start: the end record has `started=false` and `error="the command could not be started"` and nothing of the runtime's text; a command with a control character and one of 4 KiB: cleaned and cut in the record; no record holds what was typed or printed (markers in both directions) |
| `TestExecLeavesNothing` | after each of: a normal end, a cancelled context, a refused upgrade: the goroutines the call started are gone (count with a deadline, as the package's other tests do) |

If the exec protocol's server side cannot be built in a test with what is in `go.mod`, say exactly what is missing; then `TestExecStream`, `TestExecStatus` and `TestExecNotStarted` test the function that turns the stream's result into `Exec`'s (behaviour 5) with errors the test constructs, and the stream itself is left to the end-to-end task: but try the server first, the probe's notes on the protocol are in its report.

## Steps

- [ ] Behaviours 1 and 2, tests first. Commit: `Kube: Exec checks what it is asked and asks the API for a terminal in a container`.
- [ ] Behaviours 3 to 5 and 7, tests first. Commit: `Kube: a command in a container runs on the caller's terminal until it ends, and says how`.
- [ ] Behaviour 6, tests first. Commit: `Kube: a shell is logged when it starts and when it ends, and nothing of what was in it`.
- [ ] `gofmt -l . && go vet ./... && go build ./...`; `go test -race ./kube/...` without and with `KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)"`.

## Report

Behaviour by behaviour what you built and where it is tested; the failing and passing runs; how the test's server speaks the protocol, or why it could not; what of the probe's code you carried over and what you changed; anything in the contract that did not fit.
