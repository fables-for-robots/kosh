# kosh: a shell in a pod: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task by task. Each task has a file of its own under `2026-10-13-kosh-shell/`; an implementer reads its own task file, this file's Global Constraints and Review Focus, and nothing else of the plan.

**Goal:** `s` on a pod, or on a container of a pod, opens a dialog with the container and an editable command, and then a shell in that container on the user's own terminal, until the shell ends.

**Architecture:** three packages each get one thing. `kube` gets `Exec`, the API's exec stream with a terminal, as an action of the session. `sshd` gets the hand-over of a session's terminal (its input, its raw output and its sizes) to a function, and back. `tui` gets the key, the dialog and the command's words, and joins the two. `cli` passes the hand-over from the session to the UI.

**Tech stack:** as stage 1 (Go 1.26.8, `charm.land/ssh`, Bubble Tea v2, client-go v0.37.1), plus `k8s.io/client-go/tools/remotecommand` and what it needs.

**Spec:** `docs/superpowers/specs/2026-10-13-kosh-shell-design.md`, which adds to `docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`.

**Settled first:** `2026-10-13-kosh-shell/task-00-probe.md`, the probe's report: how the terminal is handed over without losing a byte, and how the exec stream behaves at its edges. Tasks 2 and 3 build on its code.

## Global Constraints

Every task's requirements include these.

- **Module** `github.com/fables-for-robots/kosh`. Packages at the top level of the module. **No `internal/` packages**, anywhere. The command line is `github.com/urfave/cli/v2`.
- **`go.mod` is set by Task 1** and holds every dependency the plan uses. A task does not run `go get`, `go mod tidy`, or change a version. A task that finds it needs a module that is not there stops and says so in its report.
- **kosh starts no other process.** No `os/exec` outside tests and scripts. A shell in a pod is a stream of the API, not a process of kosh's.
- **The contract is code.** Task 1 puts the exported types, interfaces and signatures this plan adds into the repository, with their documentation. A task fills in its package behind them. It does not change an exported signature, a documented behaviour or another package's files; when the contract is wrong for what the task has to do, the task stops and reports that.
- **A task changes only the files its task file lists**, plus new test data beside them.
- **Tests:** written first and seen to fail. `go test -race ./<package>/...` passes, `go vet ./...` and `gofmt -l .` are clean, and `go build ./...` passes for the whole module, before a task is reported done. No test needs the network or a cluster, except those of Task 5. Waits have deadlines; a wait for work that takes the machine real time (drawing megabytes, a stream) gets a deadline of a minute, not of seconds: a shared CI runner under the race detector is several times slower than a developer's machine. No test sleeps to wait for something that can be waited for.
- **No data races.** Everything a session shares with another, or with a background goroutine, is guarded.
- **Nothing of a session outlives it.** Whatever a shell opens (a stream, goroutines, a pipe) ends when the shell ends and when the session's context ends, whichever is first, and the tests count what is left.
- **Text that is not kosh's is never trusted to the terminal while the UI draws.** What the UI draws that came from the API or from the user's typing (an error's text, a command) goes through the UI's cleaning (`clean`, `errorText`) first, and is measured in cells. What a shell writes while it has the terminal is the one exception: it is the user's own terminal session, and passes byte for byte.
- **The log** holds no Secret's contents and no text of an edit (stage 1, §13), and nothing of what is typed in a shell or printed by it. A key is named by its name and its fingerprint. An API error is logged as its reason and code.
- **Read-only keys.** The session (`kube`) is the one place the grant is checked; the UI binds no action key for a read-only grant. Both hold for the shell.
- **Documentation:** every exported identifier has a doc comment in the manner of the contract's: plain sentences that say what it does and what it does when things go wrong.
- **Licence** BSD-3-Clause.
- **Commits:** one or more per task, each leaving the module building. The message is `Area: a sentence that says what is now true`, as `Keys: a line with an unknown option gives no access`, and ends with the `Co-Authored-By:` line of the model that wrote it and

  ```
  Claude-Session: https://claude.ai/code/session_012Y76zZnbm2mtUzXqHDuySf
  ```

- **Leave no binaries behind.** A binary a task builds is removed before the task is reported done.
- **The clipboard.** No test executes a command that reads a clipboard.

## Review Focus

What the spec implies and a person or an attacker would meet first; each has its test in the task named.

1. **A byte at the seam.** The first key typed after the shell starts, and the first after it ends, typed fast (a paste, or type-ahead): each goes where the user meant it, once. (Task 3)
2. **A full-screen program in the shell** (`vi`, `top`): its bare line feeds and cursor moves arrive untouched, and after it the terminal is in the state the UI expects. (Tasks 3 and 5)
3. **The session ends while a shell has the terminal** (the key is removed, the client's network goes, kosh stops): the remote process is ended, the stream and every goroutine are gone, and the terminal is not left in the shell's modes. (Tasks 2, 3 and 5)
4. **A read-only key and the shell**: no key, no dialog, and `Exec` refuses it and logs the refusal even if something calls it. (Tasks 2 and 4)
5. **A command the image does not have, and a pod that has no shell at all**: the user gets the runtime's reason and the dialog back with their text, and nothing hangs. (Tasks 2, 4 and 5)
6. **What the user types into the Command field**: quotes that do not close, a line of a megabyte pasted, control characters, an escape sequence: cut into words by the rule or refused with a reason, cleaned where drawn and logged, never run through a shell of kosh's (there is none). (Task 4)

## Tasks

| # | Task | File | Depends on |
|---|---|---|---|
| 0 | The probe | `task-00-probe.md` | none |
| 1 | The contract and `go.mod` | `task-01-contract.md` | 0 |
| 2 | `kube`: Exec | `task-02-kube-exec.md` | 1 |
| 3 | `sshd`: the terminal, handed over | `task-03-sshd-terminal.md` | 1 |
| 4 | `tui`: the key, the dialog, the words | `task-04-tui-shell.md` | 1 |
| 5 | End to end, and the README | `task-05-e2e.md` | 2, 3, 4 |
| 6 | Closing: the feature reviewed as a whole | `task-06-closing.md` | 5 |

Tasks 2, 3 and 4 are built side by side, each in a worktree of its own: they own different packages and meet only at the contract.
