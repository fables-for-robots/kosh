# Task 1: the contract, and `go.mod`

Done by the controller before the tasks that build behind it. What it put into the repository:

- **`kube/kube.go`**: `Exec` in `Actions`, with `ExecOptions` (`Container`, `Command`, `Stdin`, `Stdout`, `Size`, `Resized`), `ExecResult` (`Started`, `Status`) and `ErrExecNotStarted`. **`kube/exec.go`**: `Exec` of the session as a stub that begins as every action does (so that the tests which try every action with a read-only grant and with an ended session hold) and then says it is not built. **`kube/audit.go`**: the action's word, `exec`.
- **`kube/kubetest/kubetest.go`**: the fake's `Exec`: recorded as every action is, refused for a read-only grant, answered by `Errs["Exec"]`, by `ExecDo` when a test sets it, or by `ExecResult`.
- **`sshd/sshd.go`**: `Session.Exec`, `Terminal` (`In`, `Out`, `Size`, `Resized`), `ErrTerminalBusy`, `ErrSessionEnded`. **`sshd/terminal.go`**: `execNotBuilt`, which a session gets as its `Exec` until Task 3.
- **`tui/tui.go`**: `Options.Handover` and `Terminal`; **`tui/view.go`**: `env.handover`.
- **`cli/serve.go`**: the session's `Exec` given to the UI as `Options.Handover`, with `sshd.Terminal` converted to `tui.Terminal`, which has the same fields.
- **`go.mod`, `go.sum`**: `k8s.io/streaming v0.37.1`, and `github.com/gorilla/websocket` and `github.com/moby/spdystream` as indirect requirements, by `go mod tidy` with `k8s.io/client-go/tools/remotecommand` imported. No other version moved.

The module builds, also for linux/amd64 without cgo, and every package's tests pass.
