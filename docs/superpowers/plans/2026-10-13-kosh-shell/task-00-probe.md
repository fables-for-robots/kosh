# kosh shell: feasibility probe (2026-10-13)

Probe of "`s` on a pod opens a shell in one of its containers". Everything here was measured on kosh
`main` (detached checkout), Bubble Tea v2.0.10, `charm.land/ssh` v0.4.3, client-go v0.37.1; the
cluster part on `the Linux test machine` with kind (Kubernetes v1.35.0, containerd v2.2.0).

Reference code and all logs: `the probe's `keep/` directory (throwaway; not in the repository)`
(`README.md` there says what each file is). Nothing was committed, pushed or filed.

## Verdicts

| | | |
|---|---|---|
| Q1 | **FAIL** | plain `tea.Exec` on the SSH session loses input in every cycle, and stalls 500 ms per hand-over |
| Q2 | **PASS** | `sshd` pumps the channel; 6 variants x 200 cycles, 0 bytes lost, none out of order, hand-over about 0.3 ms |
| Q3 | **PASS** | release and restore are complete by themselves; the session writer does corrupt a command's output, the raw channel does not, and `sshd` can reach it |
| Q4 | **PASS** | `p.Send` blocks for the whole command (the forwarder stalls); the UI still ends with the right size; the command gets sizes from a channel of its own |
| Q5 | **PARTIAL** | the stream works and its errors can be told apart, but `remotecommand` needs go.mod/go.sum to change, and a cancelled stream leaves the remote shell running |
| Q6 | **PARTIAL** | everything shown through real `ssh -tt` (command, vi, top, resize, ctrl-c, ctrl-d, exit, table back and following) except: the client killed in a shell leaves the remote shell and its child alive (10 s observed, 11 min in Q5) |

---

## Q1. Does `tea.Exec` hand the terminal over and back without losing input, over an SSH channel?

**No.** The reader Bubble Tea uses for a non-file input cannot be interrupted; it stays in `Read`,
and throws away the next chunk it is given. Measured: the first byte typed to the command was lost in
200 of 200 cycles when typed with 1 ms pauses.

### What the code does

- `exec.go:102-131` (`Program.exec`): `p.releaseTerminal(false)`, `c.SetStdin(p.input)`,
  `c.SetStdout(p.output)`, `c.SetStderr(os.Stderr)`, `c.Run()`, `p.RestoreTerminal()`, then
  `go p.Send(fn(err))`. It is called from the event loop (`tea.go:839-841`, "NB: this blocks").
- `tea.go:1348-1363` (`releaseTerminal`): `p.cancelReader.Cancel()`, `p.waitForReadLoop()`, stop the
  renderer.
- `tty.go:56-82` (`initInputReader`): `p.cancelReader, err = uv.NewCancelReader(p.input)`, a
  `uv.NewTerminalReader` over it, `go p.readLoop()`.
- ultraviolet `cancelreader_other.go:14-16` is `cancelreader.NewReader(r)`; muesli/cancelreader
  `cancelreader_bsd.go:23-27` and `cancelreader_linux.go:22-26`: a reader that is not a `File`
  (`Fd()`, `Name()`) gets `newFallbackCancelReader`.
- `cancelreader.go:48-69`, the fallback: `Cancel()` only sets a flag and returns false. `Read` calls
  the underlying `Read`, and **after it returns** checks the flag: `if r.isCanceled() { return 0, ErrCanceled }`.
  The data that `Read` returned is dropped.
- `tty.go:97-105` (`waitForReadLoop`): waits for the read loop or **500 ms**, then goes on.
- `charm.land/ssh` `session.go`: the session has no `Read` of its own, so `p.input.Read` is
  `gossh.Channel.Read`; x/crypto `ssh/buffer.go:49,58,94`: readers wait on a `sync.Cond` that is
  woken with `Signal`, so the reader that waited longest, the stale one, gets the next chunk.

So, at a hand-over: the blocked `Read` stays blocked; Bubble Tea waits its 500 ms; `Run` starts and
reads the same channel next to the stale reader; the first chunk the client sends goes to the stale
reader, which discards it and ends. If the command ends before the client typed anything, the stale
reader is still there and eats the first key of the UI instead (it ate the trigger key of the next
cycle 7 times in the first variant below).

### Measurement

`sshd/probe_handover_test.go` (kept as `keep/sshd/probe_handover_test.go`): kosh's sshd harness, a
real x/crypto client with a pty request, a model that calls `tea.Exec` with an `ExecCommand` whose
`Run` echoes stdin to stdout until it has read the line `exit`. Per cycle: press the trigger, wait
until `Run` has begun, type `ABCDEFGH` (one byte per SSH write), type `exit\r`, wait for the callback
message in the model, type `01234567`. Variants: no pause or 1 ms between bytes; also typing `klmn`
right after the trigger and `wyzv` right after `exit\r`; and a command that reads its stdin in a
goroutine that outlives `Run` (as client-go's copier does, see Q5).

`go test ./sshd -run TestProbeHandover -v -count=1 -probe.mode=plain -probe.cycles=200`
(`keep/logs/q1-plain.log`), Bubble Tea reading the session as kosh does today:

| variant (200 cycles each) | to the command: sent / got / lost | to the UI after return: sent / got / lost | cycles with a loss | hand-over, key to `Run` |
|---|---|---|---|---|
| no pause | 1600 / 1292 / **308** | 1600 / 1502 / **98** | 169, plus trigger key lost 7 times, `exit` retyped in 38 | mean 500.8 ms |
| 1 ms pause | 1600 / 1400 / **200** (first byte in 200 of 200 cycles) | 1600 / 1600 / 0 | 200 | mean 500.9 ms |
| typing during, no pause | 1600 / 1355 / 245 | 1600 / 1598 / 2 | 145; 22 cycles with bytes out of order | mean 493.5 ms |
| typing during, 1 ms pause | 1600 / 1600 / 0 | 1600 / 1600 / 0 | 200 (the loss moved into the bytes typed during the hand-over: 200 of 800) | 5.2 ms (measured after the typing) |
| async stdin copier (2 variants) | test aborted: the trigger key was lost more than 5 times in a row in cycle 2 and 3 | | | |

Bytes typed during the hand-over, plain: of 800, 400 to the command, 200 to the UI, 200 lost (1 ms
pause). Bytes typed during the return: 716 to the command, 84 to the UI (1 ms pause).

Caveat on the baseline's two "typing during" rows: the echo command of that run read 64 bytes at a
time, so `exit\r` followed at once by more bytes was not always seen as the end ("exit retyped" 155
and 179 cycles). The first two rows are not affected and are the verdict. (The command was changed to
read one byte at a time before the Q2 runs.)

---

## Q2. The smallest thing in `sshd` that makes it a "yes"

**`sshd` reads the channel itself (one pump goroutine per session) and gives Bubble Tea a reader that
ends, with `io.EOF`, the moment a command is given the terminal; plus a drain mark, without which
Bubble Tea still stalls 500 ms when keys are on their way.** Built in the probe checkout
(`keep/sshd/terminal.go`, 369 lines with comments; `keep/sshd/sshd.diff`, +58 -4 in three files).

### How it works

- `input` (`terminal.go`): one buffer, one `sync.Cond`. `pump` reads the session channel into it
  (at most 32 KiB waiting: beyond that the channel is not read, so SSH flow control still holds).
  `uiInput` is what Bubble Tea gets with `tea.WithInput`; `holderInput` is what one command gets.
  Who may take bytes is one field, `holder`. No byte is ever in a reader that is not allowed to keep it.
- `give()` (in the `tea.Cmd` that `Session.Exec` returns): from that instant bytes belong to the
  command. Bubble Tea's reader is told `io.EOF`: `StreamEvents` treats EOF as a clean end
  (ultraviolet `terminal_reader.go:152-154`), `readLoop` closes `readLoopDone`, nothing goes to
  `p.errs`, and `releaseTerminal`'s `waitForReadLoop` returns at once instead of after 500 ms.
- `takeBack()` (deferred in `command.Run`, so before `RestoreTerminal` starts the new read loop):
  the command's reader is told `io.EOF` from then on (client-go's stdin copier, which outlives the
  stream, ends instead of eating the UI's next key), and what it has not read is the UI's.
- The candidate in the task, an `io.Pipe` with a zero-length write: a zero-length `Write` does wake a
  blocked `io.Pipe` reader, and the fallback reader would then return `ErrCanceled`, but only if the
  write comes after `Cancel()`, which `sshd` cannot see; a writer that tries again blocks until the
  next reader. An own reader that returns EOF needs no timing. `os.Pipe` would cost two file
  descriptors and an epoll/kqueue descriptor per session and still not fix the next point.
  `ReleaseTerminal`/`RestoreTerminal` from outside the event loop leave the model running with no
  renderer and race with the program's shutdown; not built.

### The second problem: Bubble Tea's read loop blocks on the program

Found with the first version (EOF only): with keys typed right after the trigger, hand-over took
350 to 500 ms and keys arrived out of order (`ui "klmwnyzv"`: `n` after `w`). Cause: `StreamEvents`
sends each parsed event with a blocking `eventc <- event` (`terminal_reader.go:231-237`) on
`p.msgs`, which is unbuffered (`tea.go:606`). If the event loop has already taken the `execMsg`, it
is inside `releaseTerminal` waiting for the read loop, which is waiting for the event loop:
500 ms, then the stale loop delivers its keys after the command.

The fix, in `terminal.go`: before its EOF, the UI's reader is given one mode report for a mode no
terminal has (`drainMark = "\x1b[?28657;0$y"`, a `tea.ModeReportMsg`). The read loop delivers
events in order, so when the session's filter (`tea.WithFilter`) sees that report, every key read
before the hand-over has been given to the model. Only then does the `tea.Cmd` return the
`execMsg`. A 250 ms timeout covers the cases where the mark cannot come out (the reader in the middle
of a paste or of an escape sequence). The filter also keeps what wish's filter did
(`SuspendMsg` becomes `ResumeMsg`), since a second `WithFilter` replaces the first.

### Measurement (same test, `-probe.mode=pump`)

`keep/logs/q1-pump.log`, 200 cycles per variant:

| variant | to the command: sent / got / lost | to the UI after return: sent / got / lost | typed during hand-over (800): command / UI / lost | typed during return (800): command / UI / lost | out of order | hand-over mean / max |
|---|---|---|---|---|---|---|
| no pause | 1600 / 1600 / 0 | 1600 / 1600 / 0 | | | 0 | 307 µs / 764 µs |
| 1 ms pause | 1600 / 1600 / 0 | 1600 / 1600 / 0 | | | 0 | 713 µs / 2.7 ms |
| typing during, no pause | 1600 / 1600 / 0 | 1600 / 1600 / 0 | 8 / 792 / 0 | 0 / 800 / 0 | 0 | 336 µs / 768 µs |
| typing during, 1 ms pause | 1600 / 1600 / 0 | 1600 / 1600 / 0 | 602 / 198 / 0 | 0 / 800 / 0 | 0 | (4.9 ms incl. typing) |
| async copier, no pause | 1600 / 1600 / 0 | 1600 / 1600 / 0 | | | 0 | 387 µs / 900 µs |
| async copier, typing during, 1 ms | 1600 / 1600 / 0 | 1600 / 1600 / 0 | 603 / 197 / 0 | 200 / 600 / 0 | 0 | (4.8 ms incl. typing) |

First byte to the command lost: 0 cycles. First key to the UI lost: 0 cycles. `exit` retyped: 0.
Repeated with `GOMAXPROCS=1` after the last change (`keep/logs/q1-pump-gomaxprocs1.log`): the same
zeros. The whole of `go test ./sshd -race -count=1 -probe.mode=pump -probe.cycles=200` passes (the
existing tests and the probes, 33.6 s).

**Where bytes typed during the hand-over go:** what Bubble Tea's reader had already read when the
model's command ran (in practice what arrives within about 0.3 ms of the trigger key) goes to the UI
as keys, in order, before the command starts; everything later goes to the command. Nothing is lost.
The model should ignore keys between asking for the shell and its done message, or accept them.
**During the return:** what the command did not read is the UI's. A command whose reader runs ahead
(client-go's copier) has taken some of it (200 of 800 above); those bytes went to a stream that was
ending, as with kubectl.

### What it costs

- One goroutine per session with a program (the pump), no file descriptor, at most 32 KiB + 4 KiB of
  buffer. While a command runs, nothing more from `sshd` (`Run` executes in Bubble Tea's event loop).
- Existing behaviour: `go test ./sshd -race` passes unchanged, including the goroutine-leak check of
  the harness (the pump ends when the channel closes; `input.close()` after `p.Run()` ends Bubble
  Tea's reader, which before stayed in `Read` until the channel closed). `go test ./cli ./keys ./tui`
  pass. `go.mod` and `go.sum` are untouched by this part.
- Changes of behaviour to know: the client's input is read into kosh's buffer even while the program
  is busy (bounded); wish's filter is replaced by the session's; a client that sends the drain mark
  itself has it swallowed (harmless: it never reaches the model, and can only end a drain early).
- Guard added after a thought experiment: two `Exec` commands at once (the key pressed twice in one
  write) would give the second command's reader to nobody and leave the first with EOF. `give()`
  refuses while a command has the terminal; `done` is told `ErrTerminalBusy`
  (`TestProbeTwice`: "one of the two was told: sshd: a command has the terminal", the other ran).

---

## Q3. Output

### (a) What Bubble Tea writes, and is the frame complete after the return?

**Complete, with nothing for the model to do.** `TestProbeOutput` (`keep/logs/q3-q4-pump.log`), a
model with `AltScreen = true` as kosh's root has (`tui/root.go:197`):

- release wrote `"\x1b[>4m\x1b[<1u\x1b[24;1H\x1b[?1049l\x1b[?25h\x1b[?2004l"`: modifyOtherKeys off,
  kitty keyboard popped, cursor to the last row, leave the alternate screen, show the cursor,
  bracketed paste off. The command gets a terminal in its plain state, on the normal screen.
- restore wrote `"\x1b[?1049h\x1b[?25l\x1b[?2004h\x1b[>4;2m\x1b[>1u\x1b[H\x1b[2J"` and then the
  whole frame (1856 bytes for 80x24): enter the alternate screen, hide the cursor, paste and
  keyboard modes back, home, clear, every row. No query is sent again (the start had
  `\x1b[?2026$p\x1b[?2027$p` and `\x1b[?u`; the restore has none), so no terminal reply lands in
  the UI's input.
- A terminal model fed with all of it: "after the return 24 of 24 rows of the screen are the
  program's frame". In Q6, through real ssh: "rows of the table that differ from before the shell:
  0 of 24".

### (b) The emulated pty's writer

**It rewrites, and it corrupts a full-screen program; the command must write to the channel
itself.** `charm.land/ssh` `session.go:145-150`: with an emulated pty `Write` is
`NewPtyWriter(sess.Channel).Write(p)`; `pty.go:27-38`: every `\n` becomes `\r\n`, then every
`\r\r\n` becomes `\r\n`. `tea.Exec` gives the command `p.output`, which is the session.

Measured (`keep/logs/q3-q4-plain.log` against `q3-q4-pump.log`), the command writing
`"<<ab\ncd\r\nef\r\r\ngh\x1b[5;7Hij\n>>"`:

- through `p.output`: the client got `"<<ab\r\ncd\r\nef\r\r\ngh\x1b[5;7Hij\r\n>>"`; byte for byte:
  false; `cd` and `>>` are at column 0, where a terminal that was sent a bare LF keeps the column.
- through `Terminal.Out` (the channel): byte for byte: true; the screen shows the staircase.
- Q6, real ssh and a real remote tty: `stty -onlcr; printf 'ab\ncd\nef\n'` shows `ab`, `  cd`,
  `    ef`; busybox `vi` drew all 27 `~` rows at column 0 and its status line; `top` drew.

Bubble Tea itself must keep the translating writer: with a non-tty input it draws with
`mapNl` (`tea.go:1084`), counting on LF being made CR LF. So: two writers, the session for the
program, the channel for the command.

**Where `sshd` gets the channel.** `ssh.Session` embeds `gossh.Channel` but overrides `Write`, and
the library's `session` type is not exported, so the channel cannot be reached from the session.
kosh already wraps the new channel (`filteredChannel`, `sshd/requests.go`) and calls
`ssh.DefaultSessionHandler(srv, sc, filteredChannel{nc}, ctx)` itself (`sshd/sshd.go`). The probe
passes a context of its own for that one channel: `channelContext{ctx, a}` embeds the connection's
`ssh.Context` and answers one extra key with a holder that `filteredChannel.Accept` fills. The
library stores that context in the session (`session.go:110`), so `ss.Context().Value(channelKey{})`
in the handler yields the accepted channel. `SetValue` still goes to the connection's context, as
before. 25 lines (`channelContext`, `accepted`, `rawWriter` in `terminal.go`). `Terminal.Out` wraps
it in a gate that is closed when the command's turn ends, so a late writer of the command cannot
draw into the UI.

---

## Q4. Window sizes while a command has the terminal

**`p.Send` blocks for the whole command, the forwarder stalls in it, and the UI still ends with the
right size; the command needs a channel of its own.** `p.msgs` is unbuffered (`tea.go:606`), `Send`
is a `select` on it (`tea.go:1192-1197`), and the event loop is inside `exec`.

`TestProbeSizes` (`keep/logs/q3-q4-pump.log`), three window changes during the command:

- "while the command runs: the program was given 0 more sizes, and has 90x25"
- "forward is in Send while the command runs" (the goroutine's stack: `sshd.(*window).forward` in
  `tea.(*Program).Send`)
- "the command saw: [start 90x25 100x30 110x35 120x40]"
- "after the return: the program was given 2 sizes since the command began, and has 120x40 (right: true)"

Why the UI ends right: `forward` took the first change and is stuck in `Send` with it (100x30);
later changes overwrite `window.size` and leave one token in `changed`; after the return `Send`
delivers the stale 100x30, the loop takes the token and sends the latest. No change to `forward`
is needed. The renderer redraws on each `WindowSizeMsg`, so there is one extra full frame.

The command cannot share `changed` (the forwarder has taken the token and is stuck). The probe adds
a second token channel to `window`, `resized` (capacity 1, set next to `changed` on every change),
and `window.latest()`. `command.Run` drains `resized` at its start and gives the command
`Terminal.Size` and `Terminal.Resized`. The size queue for client-go is then, without a goroutine
(`keep/exec/kube_exec.go`):

```go
func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	if q.started {
		select {
		case <-q.ctx.Done():
			return nil // client-go's resize goroutine ends
		case <-q.resized:
		}
	}
	q.started = true
	w, h := q.size()
	return &remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}
}
```

`ctx` must be one that is cancelled when `Exec` returns: client-go's goroutine sits in `Next` after
the stream has ended. Q5 and Q6 confirm the sizes arrive (`stty size`: `30 100`, then `40 120`).

---

## Q5. The exec stream against a real cluster

**It works, and the errors can be told apart, with three things to know: go.mod must change, the
fallback executor hides the typed errors, and a cancelled stream does not end the remote shell.**
Program: `keep/exec/execprobe_main.go`; logs `keep/logs/q5-part1..4.log`.

### go.mod

`k8s.io/client-go/tools/remotecommand` does not build in kosh as it is:
`missing go.sum entry for module providing package github.com/gorilla/websocket` and
`k8s.io/streaming/pkg/httpstream`. The modules are in the graph (`go list -m all` has them) but not
in `go.mod`/`go.sum`. After `go mod tidy` with the import (`keep/exec/go.mod.diff`), no other
version moves:

```
> 	k8s.io/streaming v0.37.1                      (direct, if kosh inspects the upgrade error; else indirect)
> 	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674 // indirect
> 	github.com/moby/spdystream v0.5.1 // indirect
```

and 8 lines in `go.sum`. So the rule "go.mod must not change" cannot hold for this feature. The
exec part of the probe therefore lives in a second copy of the checkout (`kosh-exec`); the `sshd`
part is in the first, with go.mod untouched.

### Executors

All three exist in v0.37.1: `NewWebSocketExecutor(config, "GET", url)` (protocol
`v5.channel.k8s.io`), `NewSPDYExecutor(config, "POST", url)`, `NewFallbackExecutor(primary,
secondary, shouldFallback)`. kubectl v0.37.1 (`pkg/cmd/exec/exec.go:146-166`) uses the fallback:
WebSocket first, SPDY when `httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)`.
All three ran a shell: `"/ # echo hello-$((6*7))\r\nhello-42\r\n/ # stty size\r\n30 100\r\n/ # exit\r\n"`, error nil.

`httpstream` here is `k8s.io/streaming/pkg/httpstream`. The older
`k8s.io/apimachinery/pkg/util/httpstream` still has an `UpgradeFailureError` and an
`IsUpgradeFailure`, of a different type: it answered false for client-go's errors (first run of the
probe).

### Errors (`StreamWithContext`, `Tty: true`, WebSocket executor unless said)

| case | what comes back |
|---|---|
| shell exits 0 | `nil` |
| `exit 7` | `exec.CodeExitError` (k8s.io/client-go/util/exec), `ExitStatus()` 7, text `command terminated with exit code 7`. Same over SPDY |
| command not in the container (kosh's own image `ghcr.io/fables-for-robots/kosh:v0.1.0`, `pause`, or `bash` in busybox) | an error from `Stream`, not an exit status; a plain `*errors.errorString`, not a `StatusError`. Text, name looked up in `$PATH`: `Internal error occurred: Internal error occurred: error executing command in container: failed to exec in container: failed to start exec "<64 hex>": OCI runtime exec failed: exec failed: unable to start container process: exec: "sh": executable file not found in $PATH`. With a path: `... exec: "/bin/sh": stat /bin/sh: no such file or directory`. Over SPDY the same with one `Internal error occurred: ` |
| container not running (pod `Completed`, `CrashLoopBackOff`, `ContainerCreating`) | `*errors.errorString`: `Internal error occurred: unable to upgrade connection: container not found ("main")` (SPDY: without the prefix) |
| pod gone | `*httpstream.UpgradeFailureError`, text `unable to upgrade streaming request: pods "nosuchpod" not found`; its `Cause` is a `*apierrors.StatusError` (404, `NotFound`). `apierrors.IsNotFound(err)` is false on the outer error (no `Unwrap`), true on `Cause` |
| container name wrong / missing on a pod with two | `UpgradeFailureError`, cause `StatusError` 400 `BadRequest`: `container nosuch is not valid for pod busybox`; `a container name must be specified for pod two, choose one of: [one two]` |
| RBAC refuses | `UpgradeFailureError`, cause `StatusError` 403 `Forbidden`: `pods "busybox" is forbidden: User "system:serviceaccount:default:sa-none" cannot get resource "pods/exec" in API group "" in the namespace "default"` |
| pod deleted under the shell (seen by accident in Q6) | `command terminated with exit code 137` |
| context cancelled | `context canceled`, `errors.Is(err, context.Canceled)` true |

With the **fallback** executor every one of the `UpgradeFailureError` cases falls back to SPDY, and
what comes back is SPDY's untyped `unable to upgrade connection: pods "nosuchpod" not found`: the
`StatusError` is gone.

### RBAC verbs (Kubernetes v1.35.0)

| service account may | WebSocket (`GET`) | SPDY (`POST`) | fallback |
|---|---|---|---|
| nothing | 403 `cannot get resource "pods/exec"` | 403 `cannot create resource "pods/exec"` | 403 (SPDY's text) |
| `create` | 403 `cannot get resource "pods/exec"` | works | works (after the fallback) |
| `get` | 403 `pods "busybox" is forbidden: ` (empty reason) | 403 `cannot create` | 403 |
| `get` and `create` | works | works | works |

So on 1.35 the WebSocket executor needs **`get` and `create`** on `pods/exec`; SPDY needs `create`.
The empty-reason 403 for `get` alone is a second authorization check for `create` on the upgrade
(my reading of the result; I did not read the API server's code or test an older cluster, where
`get` alone is said to be enough). The chart's role should grant both.

### Cancelling

`StreamWithContext` returned 110 µs (WebSocket) and 188 µs (SPDY) after `cancel()`. **The remote
shell did not end.** `ps` in the container minutes later still showed every shell whose stream was
cancelled or whose client was killed, each on its own pts:

```
   61     0 136,0  11:34   sh        (client process killed)
  150     0 136,3   1:16   sh        (context cancelled, WebSocket)
  168     0 136,4   1:15   sh        (context cancelled, SPDY)
```

`/proc/<pid>/status` of them: sleeping, `SigIgn 0000000000284004` (SIGHUP not ignored): nothing ever
signals them, containerd keeps the pty. Also tried: closing stdin (EOF) does not end a TTY exec
(stream still open after 3 s, both executors); typing `^C^D` and then cancelling left the shell too.
What does remove them: `kubectl exec busybox -- sh -c "kill -HUP <pid>"` removed the shell and its
`tail -f` child (the child stayed as a zombie only because that pod's pid 1 does not reap). A clean
`exit`, `exit 7` or ctrl-d leaves nothing behind.

Correction to my own first reading: the first log line "sleep gone=true" 36 ms after the cancel was
empty of meaning; busybox ash runs `sleep` as a builtin, there never was a `sleep` process.

### Other facts from the runs

- After the stream has returned, client-go's stdin copier is still in `Read` on the stdin it was
  given and takes the next byte ("reads begun 2, reads returned 1"; then "the byte was taken by the
  stale reader: true", with `"Copying stdin failed" err="write on closed stream 0"`). This is why the
  command's reader must end with its turn (Q2 does that).
- On a cancel client-go logs through klog at error level: `"Copying stdout failed" err="next reader:
  ... use of closed network connection"` and `"Waiting for server to close stdin failed"`. kosh routes
  klog into its log (`cli/serve.go`, `routeKlog`): two error lines per session that dies in a shell.
- busybox ash prints its prompt twice at the start (`"/ # \r/ # \x1b[J"`): the first size arrives
  just after it started and it redraws on SIGWINCH. Cosmetic.

---

## Q6. The whole thing once

**Everything works through the real `ssh -tt` except the last item.** kosh from the probe
(`keep/exec/*`: `kube.Exec` with the WebSocket executor, an `s` binding on pods for a full grant,
`cli` joining `sshd.Session.Exec` to `tui.Options.Shell`) served with
`kosh serve --kubeconfig … --iroh=false --ssh-listen 127.0.0.1:25185 --state-dir … --authorized-keys …`
on the box; `probe/drive` drives OpenSSH on a pseudo-terminal (`creack/pty`) and reads the screen
with `e2e/screen_test.go`'s model. `keep/logs/q6-run1.log` (`--- PASS: TestShell (4.92s)`):

- `s` on the `busybox` row: `kosh: sh in pods/default/busybox`, then `/ #`.
- A command: `echo hello-$((6*7))` shows `hello-42`; `stty size` shows `24 80`.
- Resize of the client's terminal to 100x30: `stty size` shows `30 100`.
- Bare LF: `stty -onlcr; printf 'ab\ncd\nef\n'` shows `ab` / `  cd` / `    ef` ("staircase: true").
- `vi /tmp/probe.txt`, insert two lines: rows 0 and 1 are the text, "rows 2..28 that are a lone ~ at
  the first column: 27 of 27", status line `- /tmp/probe.txt [Modified] 2/2 100%`; `:wq` returns to
  the prompt and `cat` shows the file.
- `top`: `Mem:`, `CPU:`, `Load average:` and the `  PID  PPID USER …` table drawn; `q` leaves it.
- ctrl-c on `tail -f /dev/null`: `^C`, prompt, `echo after-$?` shows `after-130`. ctrl-d in a nested
  `sh`: back in the outer shell, `echo outer-$?` shows `outer-0`.
- `exit`: kosh's table is back, "rows of the table that differ from before the shell: 0 of 24".
- Following the cluster: `kubectl run aaa-new …` after the shell, and `aaa-new` appears on the table.
- A second `s`, a second shell, `exit`, the table again.

**The client killed in the middle of a shell** (`keep/logs/q6-kill.log`): kosh ends the session at
once (`session end … duration=1.667s`, the moment of the kill; in `TestProbeEnds` the command's
context is done 109 µs after the client is gone, 32 µs after the key is removed, and a removed key
still gets `kosh: this key is no longer accepted` and exit status 1 within 1 ms). The remote side:

```
the shell is pid 515; before the kill:
  515     0 136,11  0:02   sh
  521   515 136,11  0:02   tail -f /dev/null
10.1s after ssh was killed, of the shell and its children:
  515     0 136,11  0:12   sh
  521   515 136,11  0:12   tail -f /dev/null
```

Still there, as in Q5. This is the exec stream's behaviour, not kosh's hand-over.

---

## Recommended design

### `sshd`: what it offers the UI

```go
// Session gets one more field (sshd/sshd.go).
type Session struct {
	// … Context, Grant, Transport, Remote as today …

	// Exec returns a command for the session's program that gives the terminal to run: the
	// program's screen is left, run is called with the terminal, and when run has returned the
	// program has the terminal again, draws its screen, and is given done's message with what run
	// returned. run must return when its context is done, which it is when the session ends (the
	// client gone, the channel closed, the key removed or changed, kosh shutting down); the program
	// takes no message while run has not returned. done is told ErrTerminalBusy, and run is not
	// called, when a command has the terminal already.
	Exec func(run func(context.Context, Terminal) error, done func(error) tea.Msg) tea.Cmd
}

type Terminal struct {
	In      io.Reader                  // the client's bytes from the hand-over on; io.EOF when the turn ends
	Out     io.Writer                  // the channel, untranslated; io.ErrClosedPipe after the turn
	Size    func() (width, height int) // the window now
	Resized <-chan struct{}            // has a value when Size has changed since it was last taken
}

var ErrTerminalBusy, ErrSessionEnded error
```

Inside: `tea.Exec` with an `ExecCommand` of `sshd`'s own (it ignores `SetStdin`/`SetStdout`), the
`input` pump, the drain mark and the session's filter, the channel through `channelContext`,
`window.resized`, and `session.end` cancelling the command's context (without it `go p.Quit()`
waits behind the shell and the session would be cut by `endTimeout` after 2 s without its message;
that is read from the code, the fixed path is what `TestProbeEnds` measured).
`tui` stays free of `sshd`: `cli` passes a function
(`keep/exec/cli_serve.diff`), e.g. `tui.Options.Shell func(run func(ctx, in, out, size, resized) error, done func(error) tea.Msg) tea.Cmd`.
Tests of `tui` give a fake that calls `run` with buffers.

### `kube`: `Exec`

```go
type ExecOptions struct {
	Container string   // required when the pod has more than one container (the API answers 400 otherwise)
	Command   []string // {"sh"}; the UI decides what to try
	Stdin     io.Reader
	Stdout    io.Writer
	Size      func() (width, height int)
	Resized   <-chan struct{}
}

// Exec runs Command in a container of pod with a terminal, until it ends or ctx or the session does.
func (s Session) Exec(ctx context.Context, pod ObjectRef, opts ExecOptions) error
```

- `mayChange()` first (a read-only grant gets `ErrReadOnly`), `bound(ctx)` as `Logs` does, and an
  audit record like the other changing actions.
- `remotecommand.NewWebSocketExecutor(cfg, "GET", url)` alone, not the fallback: the typed cause
  survives. If a fallback is wanted for proxies that cannot do WebSocket, fall back only when the
  `UpgradeFailureError`'s cause is not a `StatusError`.
- Errors the caller has to tell apart, and how `kube` can make them:

| for the UI | from |
|---|---|
| `nil` | the command exited 0 |
| `ErrReadOnly` | the grant |
| `*ExitError{Code}` (new, or pass `exec.CodeExitError` on) | `errors.As(err, &exec.CodeExitError{})`; an ordinary end for a shell (130 after ctrl-c then exit, 137 when the container was killed) |
| "no such command in the container" (new sentinel, e.g. `ErrNoCommand`) | text only: contains `executable file not found in $PATH`, or `no such file or directory` after `exec: "`; this is how the UI knows to say "no shell in this container" or to try the next shell |
| "container not running" (new sentinel) | text only: `container not found ("` |
| not found, forbidden, bad request | `var u *httpstream.UpgradeFailureError; errors.As(err, &u)` then return `u.Cause`: `apierrors.IsNotFound/IsForbidden/IsBadRequest` work on it |
| the session or ctx ended | `errors.Is(err, context.Canceled)` |

- The role in the chart: `pods/exec` with `get` and `create`.
- go.mod: `k8s.io/streaming` (direct), `gorilla/websocket` and `moby/spdystream` (indirect), go.sum +8.

### The orphan shell: a decision for the spec

Not solved by the probe. Options, cheapest first: (1) accept and document it (I expect kubectl to leave the same, since it is the same stream; not
tested); (2) when `Exec` ends by its context and not by the command, run a second, short exec that
hangs the shell up: it needs the shell's pid, e.g. start it as `sh -c 'printf "<mark>%s\n" $$; exec sh'`
and take the pid from the first line, then `sh -c 'kill -HUP <pid>'` (shown to work by hand; the
wrapper and the parsing were not built or tested).

---

## Surprises

1. Plain `tea.Exec` over SSH is worse than "may lose a byte": it loses one chunk per hand-over
   always, costs 500 ms per hand-over, and stale readers pile up and eat later keys, the trigger key
   included.
2. Owning the reader is not enough: Bubble Tea's read loop blocks sending to the program, so keys on
   their way at the hand-over cause the same 500 ms stall and arrive after the command. Hence the
   drain mark.
3. `remotecommand` does not build with kosh's go.mod/go.sum today.
4. `IsUpgradeFailure` exists twice in v0.37.1 (apimachinery and `k8s.io/streaming`), with different
   types; the wrong one silently answers false.
5. kubectl's fallback executor turns a typed 403/404/400 into SPDY's plain string.
6. Kubernetes 1.35: WebSocket exec needs `get` and `create`; `get` alone gives a 403 with an empty
   reason.
7. A cancelled or cut exec stream leaves the remote shell, and what it runs, alive. EOF on stdin does
   not end a TTY exec either.
8. client-go's stdin copier outlives the stream and takes one more read.
9. "Command not found" and "container not running" are untyped strings, the first with
   `Internal error occurred:` twice over WebSocket.
10. The library's `Write` translation and Bubble Tea's `mapNl` depend on each other: the program
    needs the translating writer, the command must not have it.
11. The shell starts on the normal screen where the user's terminal was before kosh (in Q6 on row 2,
    under old content), not on a cleared screen. A line such as
    `kosh: sh in pods/default/busybox` or a clear is the UI's choice.
12. A pod deleted while its shell is open: exit code 137, cleanly back to the table (seen when the
    test opened a shell in a pod it had just deleted).

## Not found out

- What kosh's watches do when the program takes no message for hours: `tui` pulls with `wait()`
  commands, so `kube`'s streams are not read during a shell. After a short shell the table followed
  the cluster again; a long one was not tried.
- Whether older API servers (before 1.35) accept WebSocket exec with `get` alone, and the error texts
  of CRI-O or of containerd 1.x: only kind v1.35.0 with containerd 2.2.0 was measured.
- The iroh transport: all runs were over TCP.
- A shell opened within milliseconds of the session's start, while the terminal's replies to Bubble
  Tea's start-up queries are still on their way: they would go to the shell. Not measured.
- The drain mark's timeout path (250 ms, the reader inside a paste or an escape sequence) is
  reasoned, not measured.
- The orphan-shell remedy (pid mark and a hang-up exec) is a sketch, tested only by hand with `kill`.
- How the Nix build pins Go modules (no `vendorHash` in `flake.nix`): whether the go.mod change
  needs a hash to move.
- The baseline's two "typing during" rows carry the echo command's coalescing artefact (see Q1).

