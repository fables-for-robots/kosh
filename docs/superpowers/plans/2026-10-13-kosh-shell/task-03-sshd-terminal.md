# Task 3: `sshd`: the terminal, handed over

A session's program owns its input and output. For a shell it has to let go of both and get them back, and no byte may go to the wrong side at either seam. This task builds `Session.Exec`: the hand-over of a session's terminal to a function, and back. It knows nothing of Kubernetes or of the UI: what gets the terminal is any function.

**Read first:** the spec §5 and §10 (`docs/superpowers/specs/2026-10-13-kosh-shell-design.md`); the plan's Global Constraints and Review Focus (`docs/superpowers/plans/2026-10-13-kosh-shell.md`); the probe's report, Q1 to Q4 and "`sshd`: what it offers the UI" (`docs/superpowers/plans/2026-10-13-kosh-shell/task-00-probe.md`): it found that the library's own hand-over loses input over an SSH channel, built what does not, and measured it. Its code (`terminal.go`, `sshd.diff`, the two test files; their location is in your dispatch) is where you start: **build on it, do not rediscover it**, and make it production code: every line understood, named and documented in the manner of this package, its tests the tests of this package. The contract: `sshd/sshd.go` (`Session.Exec`, `Terminal`, `ErrTerminalBusy`, `ErrSessionEnded`). The code this joins: `sshd/session.go` (how a session's program is made and run, `window`, `forward`, `end`), `sshd/requests.go` and `sshd/sshd.go` (where the session channel is accepted and wrapped), `sshd/testhelpers_test.go`.

**Files:**
- Modify: `sshd/terminal.go` (the stub becomes the thing), `sshd/session.go`, `sshd/sshd.go`, `sshd/requests.go` as far as the hand-over needs
- Create: `sshd/terminal_test.go`
- No other file. `go.mod` does not change.

**Interfaces:**
- Consumes: Bubble Tea's `tea.Exec`, `tea.WithInput`, `tea.WithFilter`; the SSH channel of the session.
- Produces: `Session.Exec` and `Terminal`, as `sshd/sshd.go` documents them.

## Behaviour

1. **`sshd` reads the channel.** One goroutine per session with a program reads the SSH channel into a bounded buffer (when the buffer is full the channel is not read: SSH's flow control holds, and a client cannot make kosh hold more than the bound). The program gets its input from a reader of that buffer (`tea.WithInput`); a command gets its input from another. Who may take bytes is one piece of state: no byte is ever taken by a reader that is not allowed to keep it.
2. **`Session.Exec(run, done)`** returns a command for the session's program. When the program runs it:
   - the terminal is given to the command: from that moment the client's bytes are the command's;
   - the program's reader is first given a mark (a terminal report that no terminal sends) and then its end: the hand-over waits until the program has taken the mark, so that every key read before the hand-over has reached the model, in order; 250 ms at most, for a reader that is in the middle of a paste or of an escape sequence;
   - the program releases its screen and modes (`tea.Exec`), `run` is called with a context and the `Terminal`, and when `run` has returned the terminal is the program's again, its screen is drawn, and the model gets `done(err)` with what `run` returned.
3. **`Terminal.In`** gives the client's bytes from the hand-over on, and `io.EOF` from the end of the turn on, also to a `Read` that began during the turn and is still waiting: a reader that outlives its turn takes nothing of what the client sends afterwards. What the command did not read is the program's.
4. **`Terminal.Out`** writes to the SSH channel itself: no byte is changed, a bare line feed stays one. After the turn a write fails with `io.ErrClosedPipe`. The program's own output keeps the translating writer it has today.
5. **`Terminal.Size` and `Resized`**: `Size` returns the window's size now; `Resized` has a value (one, however many changes) when it has changed since `Size` was last asked. The forwarder that tells the program its sizes is not disturbed: after the turn the program has the size that holds then.
6. **One command at a time.** `Exec`'s command while a command has the terminal does not call `run`; `done` is told `ErrTerminalBusy`.
7. **The session ends during a turn** (the client gone, the channel closed, the key removed or changed, kosh shutting down): `run`'s context is done at once; `In` gives `io.EOF` and `Out` fails; when `run` has returned the session ends as it would have, with its message to the client where it has one (`kosh: this key is no longer accepted`, `kosh is shutting down`) and its reason in the log, within the time the session's end has today. A `run` that does not return when its context is done does not keep the session's end from happening: the connection is closed after the end's timeout as for a program that does not end, and the log says the command did not return.
8. **`Exec` after the session has ended** (the command arrives late): `run` is not called; `done` is told `ErrSessionEnded`.
9. **The filter.** Giving the program a filter of the session's own replaces wish's: what wish's did (a suspend turned into a resume) is kept. The mark never reaches the model, whoever sent it: a client that sends the mark itself has it swallowed.
10. **Nothing changes for a session that never hands over**: the behaviour and the tests of the package as they are; one goroutine and at most the buffer's bytes more per session, and both end with the session.

## Tests

Over real SSH connections with the package's harness; what gets the terminal is a function of the test.

| Test | What it shows |
|---|---|
| `TestHandoverLosesNothing` | at least 200 cycles of: a key that makes the model call `Exec`; bytes typed one per SSH write right after it; the command echoes what it reads until it reads its end mark; then keys typed right after the return. Variants: without pauses, with pauses of a millisecond, with bytes typed while the hand-over is under way and while the return is. In every cycle: the command got every byte meant for it, the first included, in order; the model got every key typed after the return, the first included, in order; no byte arrived twice. Also once under `GOMAXPROCS=1`. The wait for a cycle has a deadline of a minute |
| `TestHandoverIsFast` | the mean time from the key to `run` being called is under 50 ms (it was a third of a millisecond in the probe; plain `tea.Exec` took 500 ms): not a benchmark, a guard against the stall coming back; skipped under the race detector if it cannot hold there, with the reason |
| `TestCommandOutputIsRaw` | the command writes `a\nb\n` and bytes 0 to 255: the client receives exactly those bytes; the program's own output before and after still has its line feeds translated as today |
| `TestScreenAfterTheReturn` | the model's frame is complete on the client's screen after the return without the model doing anything for it (read with the screen model of `e2e/screen_test.go`, copied into this package's tests as the probe did) |
| `TestCommandSizes` | `Size` at the start is the window's; a window change during the turn puts one value on `Resized` and `Size` then returns the new size; three changes while the command does not look give one value; after the turn the model has the last size |
| `TestOneCommandAtATime` | two `Exec` commands in one update: one runs, the other's `done` gets `ErrTerminalBusy` |
| `TestSessionEndsDuringACommand` | for each of: the client closes its connection, the client closes the channel, the key is removed from the list, `Shutdown`: `run`'s context is done within 100 ms, a blocked `In.Read` returns `io.EOF`, the session's end is logged with its usual reason, the removed key's client still reads `kosh: this key is no longer accepted`, and nothing is left (the harness's goroutine check) |
| `TestCommandThatDoesNotReturn` | `run` ignores its context: the session still ends after the end's timeout, the connection is closed, and the log says the command did not return |
| `TestExecAfterTheEnd` | `done` is told `ErrSessionEnded`, `run` is not called |
| `TestReaderThatOutlivesItsTurn` | a goroutine of the command still in `In.Read` when `run` returns: it gets `io.EOF`, and the next key the client types reaches the model |
| `TestTheMarkIsSwallowed` | a client that sends the mark's bytes itself: the model never sees them, and a hand-over after it works |
| `TestInputIsBounded` | a client that sends megabytes while the program does not read: kosh's buffer does not grow past its bound (the heap, with a generous margin), and the bytes are not lost when the program reads again |
| the package's existing tests | pass unchanged, the leak checks included |

## Steps

- [ ] The pump and the two readers, with the existing tests green: tests first. Commit: `SSHD: the server reads a session's channel itself, and says who may have its bytes`.
- [ ] The hand-over with the mark, the raw output and the sizes: tests first. Commit: `SSHD: a session's terminal is handed to a command and taken back without a byte astray`.
- [ ] The session's end during a turn, one command at a time, the late command: tests first. Commit: `SSHD: a command that has the terminal ends with its session`.
- [ ] `gofmt -l . && go vet ./... && go build ./...`; `go test -race ./sshd/...`, and once `go test -race -count=3 -cpu 1,4 ./sshd/`.

## Report

Behaviour by behaviour what you built and where it is tested; the failing and passing runs; the numbers of `TestHandoverLosesNothing` per variant; what of the probe's code you carried over unchanged, what you changed and why; what the reader of Bubble Tea v2.0.10 really does at the hand-over, in the lines of its code that your design relies on (so that an upgrade of the library can be checked against them); anything in the contract that did not fit.
