# Task 13: tui, logs

The log view: the log of a pod, of one of its containers, or of all the pods of a workload, followed as it is written. A log is the rawest text the cluster has, written by programs kosh knows nothing about, and it can come faster than a terminal can be drawn.

**Read first:** the spec, §8.4, §10.4 and §10.5 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints and Review Focus (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); the frame you hang into: `tui/view.go` (the contract), and what Task 11 built: `tui/root.go`, `tui/table.go`, `tui/clean.go`, `tui/styles.go`, and `tui/harness_test.go` with one or two of its tests; `kube/kube.go` for `Logs`, `LogOptions`, `LogFrom`, `LogLine`, `LogStream` and the `Log…` constants; `kube/kubetest/kubetest.go` for its `LogStream` (`LogOf`, `Send`, `End`, `SetSkipped`, `Closed`).

**Files:**
- Create: `tui/logs.go` (the log view), `tui/bind_logs.go` (the keys `l` and `p`)
- Create: `tui/logs_test.go`, `tui/bind_logs_test.go`; golden frames under `tui/testdata/`
- Do not change any file of Task 11. Two other tasks are adding views to this package at this moment, in `tui/text.go`, `tui/yamlcolor.go`, `tui/bind_text.go`, `tui/dialog.go`, `tui/editor.go` and `tui/bind_actions.go`; every package-level name you add begins with `log`.

**Interfaces:**
- Consumes: `kube.Session.Logs` and the log types; from Task 11: `clean`, `fit`, the styles, the harness; from `tui/view.go`: everything.
- Produces: the bindings of `l` and `p`, registered with `bind` from an `init` in `tui/bind_logs.go`.

## Behaviour

**The keys**

1. `l` (hint `{"l", "Logs"}`, order 20) applies to tables of pods, deployments, replicasets, statefulsets, daemonsets, jobs and services, and of `kube.Containers`. `p` (hint `{"p", "Logs Previous"}`, order 21) applies to tables of pods and of `kube.Containers`.
2. In a table of `kube.Containers` the reference to read is the pod, which is the table's `q.Of`, and the container is the row's name. Elsewhere it is the row's reference and no container is named. The view opens for the first of the references it is given.
3. A read-only grant has both keys: reading a log changes nothing.

**The stream**

4. The view opens `sess.Logs(ctx, ref, opts)` in a command, never in `Update`, with a context that lives as long as the view, and reads `Lines()` in commands, one batch per message. An error of opening the first stream: the view is not pushed and the flash shows the error. A later one (9): the view stays and shows the error as a line of its own.
5. The view keeps the last `kube.LogBufferLines` lines; older ones are dropped. The text of each line is cleaned once, when it arrives (Review Focus 1).
6. **Drawing is bounded by time, not by lines.** However fast batches arrive, the view is told to redraw at most 20 times a second: batches are added to the buffer as they come, and what is drawn is the buffer at the next redraw. (The harness shows how the table throttles its reads; do the same in kind.)
7. When `Lines()` closes, the view says so in a last line: `— the log has ended —`, or `— the log broke: <error> —` with `Err()`, cleaned. When `Skipped()` is not zero the title says `(<n> containers left out)`.
8. `Close` closes the stream and cancels the context.

**What a line looks like**

9. Lines of a stream of more than one container begin with `pod/container ` in a colour that is the same for the same pod and container and differs between them (a small fixed palette, chosen by a hash of the two names); lines of a single container have no prefix. `t` shows and hides each line's time before its text, as `2026-10-13T12:00:00Z`, hidden at first; a line without a time shows none.
10. Long lines are cut at the width with `fit`, or wrapped after `w`. No drawn line is wider than the view.

**The keys of the view (spec §10.4)**

11. `s` toggles autoscroll, on at first: while it is on the view shows the end, and scrolling up turns it off. The title shows `[autoscroll:on]` or `off`.
12. `↑` `↓` `j` `k`, the page keys and `g` `G` scroll.
13. `w` wrap; `t` times; `f` full screen.
14. `0` to `6` choose where the log starts, close the stream and open it again with the new options, and clear the buffer: `0` `LogTail`, `1` `LogHead`, `2` to `6` `LogSince` of 1, 5, 15, 30 and 60 minutes. The title shows which. `1` turns autoscroll off and shows the beginning.
15. `a` toggles all containers, in a view opened for a pod without a named container: the stream is opened again with `AllContainers`. In other views it is not in the hints and does nothing.
16. `m` adds a line across the view at the end of the buffer, a rule with the time of the session's clock in it, to mark where one stood. `shift+c` (`C`) clears the buffer.
17. `c` copies what the buffer holds, as it is drawn without styles (prefixes and times as shown), with `tea.SetClipboard`, and flashes `copied <n> lines`.
18. **Filter.** The view is `filterable`: with a text, only the lines that match it (a regular expression without regard to case; as it stands when it is none) are shown, and the matches are in `styleMatch`. The buffer still takes every line. The title shows `</text>`.
19. `esc` clears a filter when there is one, and otherwise pops the view; `q` pops it.
20. The title is `Logs(<ref>)`, with `:container` after the reference for one container, ` previous` for the previous run, and the notes of 7, 11, 14 and 18. The crumb is `logs`.

## Tests

With the harness of Task 11 and `kubetest`: start in a table, press `l`, get the fake's stream with `LogOf(ref)`, and `Send` lines on it.

| Test | What it shows |
|---|---|
| `TestLogsOpen` | golden: `l` on a pod: `Logs` was called with the pod's reference and `LogTail`; lines sent appear; title `Logs(pods/default/web-1) [autoscroll:on] tail`; crumbs `<pods> <logs>` |
| `TestLogsOfAContainer` | `l` in the containers' table of a pod: the reference is the pod, `Container` is the row's name; the title has `:app` |
| `TestLogsPrevious` | `p`: `Previous` is set; the title says `previous`; `p` is not a key of a deployments table |
| `TestLogsOfAWorkload` | `l` on a deployment: lines of two pods carry their `pod/container` prefix, each in its own colour, the same pod always in the same |
| `TestLogKeysApply` | `bindingsFor` has `l` for each of the eight kinds of 1 and not for configmaps; a read-only grant has them |
| `TestOpenErrorIsAFlash` | `Errs["Logs"]`: no view, a flash |
| `TestBufferKeepsTheLastLines` | 6000 lines sent: 5000 are held, and the first one held is the 1001st |
| `TestDrawingIsThrottled` | 200 batches sent as fast as they go: every line is in the buffer afterwards, and the view asked for a redraw at most a few times (count them through the hook you throttle with), not 200 |
| `TestAutoscroll` | new lines keep the end in view; `↑` turns autoscroll off and the view stays where it is while lines arrive; `s` turns it on and the end is shown again |
| `TestTimes` | `t` shows each line's time before it; a line with a zero time shows none; `t` again hides them |
| `TestWrap` | a line of 300 characters: cut at the width; `w`: wrapped over several lines; no drawn line wider than the body |
| `TestRanges` | `2`: the first stream is closed (`Closed()`), `Logs` is called again with `LogSince` and one minute, the buffer is empty, the title says `1m`; `1`: `LogHead`, autoscroll off; `0`: `LogTail` |
| `TestAllContainers` | `a` in a pod's log: reopened with `AllContainers`; in a container's log `a` does nothing and is not a hint |
| `TestMarkAndClear` | `m` adds a rule with the clock's time; `C` empties the view |
| `TestCopy` | `c` gives the clipboard the drawn lines without styles, and flashes the count |
| `TestFilter` | `/error`: only matching lines are shown, matches styled; lines that arrive later and match appear, others do not; clearing shows all, including those that arrived meanwhile; `esc` clears first and pops second |
| `TestEndOfLog` | `End(nil)`: the last line says the log has ended. `End(err)`: it says it broke, with the error |
| `TestSkippedContainers` | `SetSkipped(4)`: the title says `(4 containers left out)` |
| `TestCloseClosesTheStream` | `esc`: `Closed()` is true |
| `TestLogLinesAreCleaned` | lines that hold `\x1b]0;x\x07`, `\x1b[2J`, `\r` and `\x00`, and an `Err()` that holds an escape sequence: the drawn frame and the copied text hold none of them |
| `TestFullscreen` | `f` gives the view the whole window |

## Steps

- [ ] The view with a stream opened by test code: buffer, drawing, autoscroll, scrolling, the end of the log; tests first. Commit: `TUI: a view for a log that is being written`.
- [ ] Times, wrap, prefixes and their colours, mark, clear, copy, full screen. Commit: `TUI: a log's lines, with their times and where they are from`.
- [ ] The ranges, all containers, the filter. Commit: `TUI: where a log starts, which containers it reads, and what it shows`.
- [ ] The bindings and the open error. Commit: `TUI: logs of pods, containers and workloads`.
- [ ] `go test -race ./tui/...`; `gofmt -l . && go vet ./... && go build ./...` clean.

## Report

What you built and the test results; how drawing is throttled; anything of Task 11 that you needed and did not find, or found different from `tui/view.go`; anything you were tempted to change in its files and why.
