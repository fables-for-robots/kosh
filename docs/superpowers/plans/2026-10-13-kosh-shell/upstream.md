# Found in the libraries while the shell was built

Six defects of libraries kosh uses, each found by a test or a review of this plan, each worked around in kosh, and none reported to its authors yet. The versions are the ones kosh's `go.mod` names. For each: what happens, where, what kosh does about it, and what the library would have to change.

## 1. `charmbracelet/x/ansi`: a 33rd parameter is written beyond the parser's array

**What.** `DecodeSequence` with a parser panics on a control sequence or a device control string that has more than 32 parameters:

```go
p := ansi.NewParser()
in := "\x1b[" + strings.Repeat("1;", 32) + "1m" // 68 bytes, 33 parameters
ansi.DecodeSequence(in, 0, p)
// panic: runtime error: index out of range [32] with length 32
//   parser_decode.go:200
```

A run of 33 separators alone does not panic; a digit or a `:` after the 32nd separator does.

**Where.** `parser_decode.go`, `ParamsState`: `p.params[p.paramsLen]` is written (lines 200 to 212) without a check, while `p.paramsLen++` (218) has no bound; the array has `parser.MaxParamsSize` (32) places. Seen in v0.11.8, and unchanged in v0.11.9.

**Why it matters.** ultraviolet's terminal reader runs every byte of a program's input through it, in a goroutine of its own, where nothing recovers. A Bubble Tea v2 program that is served to others, over SSH say, ends for everybody when one client sends those 68 bytes.

**In kosh.** `sshd`'s `paramGuard` passes on at most 24 parameter separators of one sequence on the way to the program (`sshd/terminal.go`).

**The fix.** Write a parameter only while `p.paramsLen < len(p.params)`, as line 219 already does for the missing-parameter mark.

## 2. `charmbracelet/ultraviolet`: the terminal reader gathers without limit

**What.** A bracketed paste whose end mark does not come, and an escape sequence or a string (OSC, DCS, APC, SOS, PM) whose end does not come, are gathered for as long as bytes arrive within the escape timeout. Measured through kosh: 19 MiB sent after a paste's start held 22 MiB; 19 MiB after `ESC P`, 50 MiB.

**Where.** `terminal_reader.go`, `StreamEvents` and its `buf`; `decoder.go:543-560` for the paste (any CSI whose first parameter is 200 and whose final byte is `~` starts one). Pseudo-version `v0.0.0-20260906173415-0277a179edd9`.

**In kosh.** `sshd` cuts a paste at 256 KiB before the library sees more, and ends a session whose reader was handed 64 KiB of which the library made no event.

**The fix.** A limit on the buffer of one sequence and of one paste, with what lies beyond it dropped or handed on in pieces.

## 3. `charmbracelet/ultraviolet`: the reader's send does not watch its context

**What.** `sendEvents` (`terminal_reader.go:231-237`) hands each event to the program with a blocking send. A program that has ended takes none, and the reader's goroutine stays for good with what it had gathered. It happens when keys are queued behind the one that ends the program, and when the reader is given its end while the program takes no message.

**In kosh.** A session's reader is ended before its program is, while the program still takes what the reader has (`sshd`, `wind`): the reader is given a read of no bytes, which it takes only when it has handed over every event of the reads before, then its end, and the program is ended when no event has come for 50 ms. What is left: a client that stops reading its output keeps the program from taking messages; such a session's reader is left with what the library made of at most 64 KiB of input or one paste of 256 KiB, and a key can repeat that session by session. kosh's log has a warning with the key's name each time.

**The fix.** `select` on the send and on the context that `StreamEvents` was given.

## 4. `charmbracelet/ultraviolet`: the rest of a device control string with more than 16 parameters is typed as keys

**What.** The reader of device control strings has room for 16 parameters (`decoder.go:947`). What follows the 16th separator it hands the program as key presses, one for each byte of the rest of the string, its final byte included.

**In kosh.** The guard of §1 passes on 15 separators of a device control string's beginning.

**The fix.** Ignore the parameters beyond the room there is, as a terminal does, and go on reading the string.

## 5. `charm.land/bubbletea/v2`: a renderer that is stopped late stops the next one's ticker

**What.** After `ReleaseTerminal` and `RestoreTerminal` in quick succession (a command run with `Exec` that returns at once) the program takes keys and never draws again.

**Where.** v2.0.10, `tea.go`: `startRenderer` (1420-1446) keeps one `p.ticker`; the renderer's goroutine stops it when it is told its end (`case <-p.rendererDone: p.ticker.Stop(); return`); `stopRenderer` (1452-1464) only sends on `rendererDone` and does not wait. When the restore comes before that goroutine has run, it resets the ticker and starts a new goroutine, and then the old one stops the ticker.

**In kosh.** A turn with the terminal lasts at least 10 ms, and a watchdog hands the terminal over once more when nothing was drawn after a turn (`sshd`). With both: no repair in 1,200 hand-overs that return at once.

**The fix.** `stopRenderer` stops the ticker itself, or waits for the goroutine.

## 6. `k8s.io/client-go`: a stream whose context ends as it opens leaves four goroutines

**What.** In v0.37.1's WebSocket executor, `StreamWithContext` returns on a cancelled context and closes the connection; the goroutine that was to start `readDemuxLoop` can take `readyChan` after that, and `readDemuxLoop` then fails at its first `SetReadDeadline` and returns without `closeAllStreamReaders`. The error stream's reader, the stdout copier, the stdin waiter and the stream's goroutine block on their pipes for good.

**In kosh.** It needs a session's end in the microseconds between the upgrade and the start of the stream's reading. `kube`'s `Exec` runs the stream on a context of its own and lets go of everything of the session, so that what is left holds only the library's own memory, some hundred kilobytes.

**The fix.** `readDemuxLoop` closes the stream readers on every return.
