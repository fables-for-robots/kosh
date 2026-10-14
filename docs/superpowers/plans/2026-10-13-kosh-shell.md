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

## Departures

What was built differs from what this plan and its task briefs said in the
places below. Each was decided while the plan was carried out; the ledger
beside the task files (`2026-10-13-kosh-shell/ledger.md`) has each decision
with its reason and what it costs if it was wrong. Where the spec itself
was amended, the section is named.

### How it was carried out

| The plan said | What was done | Why |
|---|---|---|
| Task 5 depends on tasks 2, 3 and 4 | Task 5 was cut from a branch that held Task 3 before its review; Task 3's fixes, and later the fixes of the whole feature's review, were merged into it and its scripts run again | The three parts had never run together, and the end-to-end task is the longest step |
| The feature is reviewed as a whole after Task 5 | The review of the whole ran on the three merged parts while Task 5 ran its tests | Task 5 adds tests and the README, no code of the feature |
| Review Focus 3: when the session ends while a shell has the terminal, the remote process is ended | The stream is closed and nothing more is done: what ran in the container can go on (spec §7, a known issue in the README, `ended="the session ended"` in the log) | It is what the API's exec does and kubectl leaves the same; the API has no call to end the process |
| What the review finds is fixed in one wave and reviewed once more | One wave, as three rounds side by side, one per package, by the implementers who had built them, and one re-review of all three; then a second round for `sshd` and a second re-review; then a third, which the controller amended itself, and a third re-review | The findings fell into `kube`, `sshd` and `tui` with nothing shared; the re-review found that the wave's reset sequence moved the cursor and that its bound on a paste could be got round; the tests then failed on Linux with the race detector, where a session's end left the library's reader behind, and the third round's repair still left it when keys were in hand |
| Every fix round is reviewed again | Task 4's follow-up round (76 lines outside tests) was merged on the controller's reading of its diff | Each hunk was one of the four points asked for; the review of the whole read it again |

### The contract and `kube`

| The plan said | What was built | Why |
|---|---|---|
| The terminal's sizes reach `Exec` as a channel of sizes (spec, first version) | A function `Size` and a channel of tokens `Resized`, in `kube`, `sshd` and `tui` alike (spec §4) | It is what the probe built and measured, and needs no goroutine between the layers |
| Nothing about which executor | The WebSocket executor alone, without kubectl's fallback to SPDY | The fallback turns the API's typed refusals into plain text |
| "The command did not start" is the runtime's error | Decided by an error of the stream that has no type and came before any byte of output | The runtimes word it differently; only containerd's words were seen |
| The command is logged | As quoted words, cut after 1 KiB outside the quotes with ` …` | A slice of words is printed by the log's handler without word boundaries: `sh -c "a b"` and `sh -c a b` read the same |
| An end of `Exec` by its context is an error like another | Logged as `ended="the session ended"`, with no `error` (spec §7) | The only caller is a session's turn, whose context ends only with the session; the spec promises that line to whoever removes a key |
| Nothing about what a stream holds | The stream runs on a context of its own; the end line and the release of the session's hold are written in a defer, and a panic under the stream is logged without its value | A race in client-go can leave four goroutines of a stream behind; they now hold nothing of the session |

### `sshd`

| The plan said | What was built | Why |
|---|---|---|
| The terminal is handed over with Bubble Tea's `Exec` | `sshd` reads the SSH channel itself: one pump, a reader for the program and one for the command, and a mark that the terminal answers to tell where one ends | The probe: the plain way loses input at every hand-over and stalls for half a second |
| A command writes through the session | A command writes to the raw channel | The session's writer turns every line feed into CR LF and breaks a full-screen program |
| Nothing about a turn's length | A turn lasts at least 10 ms, and a watchdog repairs a screen that does not draw after one | A race in Bubble Tea v2.0.10 after a command that returns at once leaves a screen that never draws again |
| Nothing about the terminal's state after a shell | One fixed reset sequence is written after every turn in which a command was called, and before the session's last message; it leaves the cursor where the command left it (spec §5) | The spec promises a terminal in its normal state, and no task owned it |
| Nothing about what a client's input can make a session hold | The program's input passes at most 256 KiB of one bracketed paste; a session whose reader was handed 64 KiB of which the terminal library made no event is ended with a message; of one control sequence's parameter separators 24 are passed on, of a device control string's 15 (spec §5) | The library gathers a paste, and a sequence that never ends, without limit, and its parser writes a 33rd parameter beyond its array, a panic that ends the process: any key could do either, in v0.1.0 too |
| A session's program is ended with a quit | Its reader is ended first, while the program still takes what the reader has: it is given a read of no bytes, from which it comes back when the program has every event of what was read, and then its end; the program is ended when no event has come for 50 ms after that. Every session's end lasts 50 ms longer (spec §5) | A reader left behind a program that has ended holds what it had gathered, for good: found on Linux with the race detector, where the program's loop is slow |
| A terminal that is busy refuses a command | A command waits up to 200 ms for a terminal that a screen repair holds | The user's key press would have been answered with "busy" for a repair of 10 ms |
| A removed key's connection is closed with its last session | It is left for its client to close, and closed a second after the session's end (spec §1.6) | Closed under the client's last writes it is reset, and a `kubectl port-forward` then ends for all its connections: found end to end |
| Nothing about a client that closes only the channel | The end's timer runs then too | A command that does not return would have kept the session of a client that keeps its connection |

### `tui` and `cli`

| The plan said | What was built | Why |
|---|---|---|
| A binding applies by resource and grant | A binding can say that it needs the session's terminal (`binding.Handover`); the keys, the hints and the help skip it when the session has none, and such a binding is given the cursor's row alone | `Applies` does not see the session's environment |
| A function `words` | `commandWords` | Six test files have a helper `words` |
| The text is trimmed of spaces at its ends first | It is not trimmed | Trimming took an escaped space at the end with it |
| `tui/dialog.go` changes only as far as needed | A title, a field that cannot be changed, `enter` in a field pressing a button and a hook for cancel, each off unless a dialog asks for it | No existing dialog draws or behaves differently |
| The pod is read for its containers | Through the session's `YAML`, decoded into a small struct of the UI's own, one request | The containers table has no annotations, and `k8s.io/api` is not imported into `tui` |
| What the API refuses brings the dialog back (spec §1.5, first version) | It is said in the flash; the dialog comes back only for a command that did not start (spec §1.5) | Another command does not help against a refusal |
| A paste into a dialog's field joins its lines | A paste that holds a line break or a control character is refused whole in the Command field | A paste must not smuggle a second line into a command |
| The action keys are held from `s` until the return | The dialog gives the hold back when it opens and takes it again at the press | A session that ended with the dialog open kept the hold |
| A refused hand-over is an error flash | The dialog comes back with the reason | The text the user typed is kept |
| Every session is given the hand-over function | Only a full key's session is | A read-only key has no use for it |

### End to end

| The plan said | What was done | Why |
|---|---|---|
| Task 5 changes no code of kosh | Two defects that its tests found were fixed in `kube` and `sshd` by those packages' rounds, and its scripts run again: the log line of a shell cut by its session's end, and the reset of a removed key's connection | The tests were right and the product was not |
| `scripts/test-e2e.sh` changes only if an image must be loaded beforehand | It also keeps kubectl's and helm's caches and configuration in its work directory | Both write into the home directory by default |
| CI is green before the feature is handed over | The jobs' commands were run by hand on a Linux machine first | Publishing the branch publishes the input that crashes the terminal library's parser, which its authors have not been told of: the push waits for the user's word |
