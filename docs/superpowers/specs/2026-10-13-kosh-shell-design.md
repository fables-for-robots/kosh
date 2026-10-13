# kosh: a shell in a pod

The first piece of stage 2 ("into the pods", §22 of the stage 1 design):
`s` on a pod opens a shell in one of its containers, as k9s does. Attach,
port-forward and copying files stay for later.

This design adds to `2026-10-13-kosh-stage-1-design.md`; where it names a
section (§5, §11, §13), it is a section of that document.

## 1. What the user sees

1. **The key.** `s` on a row of the pods table, or on a row of a pod's
   containers table, for a full key. The key is free on both: `s` is
   Scale on workloads and Suspend on cronjobs, which are other tables.
   The header shows `<s> Shell`, and the help lists it. A read-only key
   has no such key (§12).
2. **The dialog.** `s` opens a dialog, which is where the shell can be
   changed before it starts:

   ```
   ┌──────────────────────── Shell ────────────────────────┐
   │ A shell in pod default/web-7d4b9c6f5-x2x9p            │
   │                                                       │
   │ Container:  ‹ web ›                                   │
   │ Command:    sh -c "command -v bash >/dev/null && exec…│
   │                                                       │
   │                   [ Cancel ]   [ Shell ]              │
   └───────────────────────────────────────────────────────┘
   ```

   - **Container** is a choice of the pod's containers that are running
     (§3). With one container it is shown and cannot be changed. From a
     row of the containers table it is that row's container.
   - **Command** is a line of text, filled with the default (§2) and
     free to edit: `/bin/bash`, `/busybox/sh`, `nu`, whatever the image
     has. It scrolls sideways when it is longer than the dialog.
   - The focus starts **in the Command field, and `enter` there starts
     the shell**: `s`, `enter` is the whole way in when the default
     will do. `tab` and the arrows move between the fields and the
     buttons as in every dialog; `esc` cancels.

   This is the one dialog whose first `enter` does the thing, against
   the rule of §11.1 that the cursor starts on the answer that changes
   nothing. Opening a shell changes nothing in the cluster; what is
   done in it is typed by the user, one command at a time.
3. **The shell.** The UI steps aside: the screen is the user's own
   terminal again (the main screen, with its scrollback), kosh prints
   one line that says where this is,

   ```
   kosh: a shell in pod default/web-7d4b9c6f5-x2x9p, container web. It ends with exit or ctrl-d.
   ```

   and from then on every byte the user types goes to the shell and
   every byte the shell writes goes to the terminal, untouched:
   full-screen programs, colours, ctrl-c, ctrl-z and the rest behave as
   over `kubectl exec -it`. The terminal's size is the shell's, and a
   resize of the window is followed.
4. **The return.** When the shell ends, the UI comes back as it was:
   the same table, the same row, following the cluster. The flash says
   how the shell ended when that is worth saying: `the shell ended with
   status 3`; nothing for status 0.
5. **When it does not start.** The dialog comes back, with the text as
   the user left it and the reason under it, wrapped: the container
   runtime's own words (`exec: "sh": executable file not found in
   $PATH`), or that the container is not running. That is the moment
   the Command field is for. What the API itself refuses (`pods "web"
   is forbidden: User … cannot create resource "pods/exec"`) is said in
   the flash, as every action's refusal is: another command would not
   help.
6. **When kosh or the key goes.** A shell's connection ends when its
   session does (§5 below): the key leaves the list, the client goes
   away, kosh stops. The user sees what a session sees then (`kosh:
   this key is no longer accepted`, `kosh is shutting down`), on a
   terminal that is back in its normal state. What was running in the
   container at that moment is not ended by this: see §7.

## 2. The command

The default is what k9s runs:

```
sh -c "command -v bash >/dev/null && exec bash || exec sh"
```

bash when the image has it, sh otherwise. It needs an `sh`; an image
without one is what the field is for.

The field's text is cut into words as a shell would cut a simple
command, and nothing else of a shell's language is done to it:

- words are separated by spaces and tabs;
- `'…'` and `"…"` make one word of what they hold, spaces included;
  inside `"…"` a backslash before `"`, `\` or `$` takes it literally;
  outside quotes a backslash takes the next character literally;
- no variables, no globbing, no `;`, `|` or `&&`: `ls *.txt` runs `ls`
  with the argument `*.txt`. Whoever wants a shell's language writes
  `sh -c '…'`, as the default does.

An empty command, or a quote that is not closed, is said under the field
and nothing is started. The words are the command and its arguments as
the container's runtime gets them; kosh adds nothing.

The command is not remembered: every dialog starts from the default.

## 3. Containers

The choice holds the pod's containers that are running now: its
containers, its init containers that are still running (sidecars), and
its ephemeral containers, in that order. The one chosen first is the
container named by the pod's annotation
`kubectl.kubernetes.io/default-container` when it is among them, else
the first. A pod with no running container gets no dialog: the flash
says `pod default/web has no running container`.

## 4. `kube`: Exec

`kube.Actions` gets one method:

```go
// Exec runs a command in a container of a pod with a terminal, as
// kubectl exec -it does, and returns when the command has ended, or
// when ctx or the session has.
Exec(ctx context.Context, pod ObjectRef, opts ExecOptions) (ExecResult, error)

type ExecOptions struct {
	Container string                     // may be empty for a pod with one container
	Command   []string                   // the command and its arguments; not empty
	Stdin     io.Reader                  // the terminal's input
	Stdout    io.Writer                  // the terminal's output; with a terminal there is no separate stderr
	Size      func() (width, height int) // the terminal's size now
	Resized   <-chan struct{}            // has a value when the size has changed
}

type ExecResult struct {
	Started bool // the command was started in the container
	Status  int  // its exit status; 0 when it ended well
}
```

- It is an action: `begin` refuses a read-only grant and a session that
  has ended, and it is logged (§7). It runs with a context that ends
  with the caller's and with the session's, as every action does: when
  either ends, the stream is closed at once.
- The stream is the API's exec subresource of the pod, with a terminal,
  over WebSocket. The fallback to SPDY that kubectl has is not used: it
  turns the API's typed refusals into plain text, and kosh talks to the
  API server directly, with no proxy between that cannot do WebSocket.
- **Errors a caller tells apart.** `ErrReadOnly`; the API's refusal
  (forbidden, not found, bad request), as the API's own error, which
  the stream's upgrade error carries as its cause; the context's error
  when the session or the caller ended it; and a new
  `ErrExecNotStarted`, wrapped around the runtime's words, when the
  command could not be started: the image has no such program, or the
  container is not running. The runtime says these as plain text
  (`… exec: "sh": executable file not found in $PATH`, `container not
  found ("web")`), which differs between runtimes, so `Exec` does not
  go by the words: an error of the stream that is none of the above,
  with not one byte of output received, is a command that did not
  start. A command that started and ended with a status that is not
  zero is not an error: it is `ExecResult{Started: true, Status: n}`.
- The reference is checked as every reference is (`checkRef`); the
  container's name must be a DNS label when it is given; no word of the
  command is checked beyond there being at least one.

`kube/kubetest` gets the same method on its fake, with what it was
given recorded and an answer the test sets.

## 5. `sshd`: the terminal, handed over and taken back

The UI's program owns the session's input and output. For a shell it
has to let go of both, completely, and get them back, completely:

- **Input.** From the hand-over to the return every byte the client
  sends goes to the shell, the first included; after the return every
  key goes to the UI, the first included. Nothing is read twice and
  nothing is lost at either seam.
- **Output.** What the shell writes reaches the client byte for byte.
  The line-feed translation that the server's emulated terminal applies
  to the UI's output is not applied to it.
- **Sizes.** The shell gets the window's size at its start and each
  change after it; the UI gets the size that holds when it returns.
- **The end of the session** while a shell has the terminal ends the
  hand-over: the reads and writes of the shell fail, and the session
  ends as it would have.

`sshd.Session`, which a session's model is made from, gets what does
this:

```go
// Exec returns a command for the session's program that gives the
// session's terminal to run until run returns, and then gives the
// program done's message with what run returned.
Exec func(run func(context.Context, Terminal) error, done func(error) tea.Msg) tea.Cmd

type Terminal struct {
	In      io.Reader                  // the client's bytes from the hand-over on; io.EOF when the turn ends
	Out     io.Writer                  // the client's terminal, byte for byte
	Size    func() (width, height int) // the window now
	Resized <-chan struct{}            // has a value when Size has changed
}
```

run's context is done when the session ends. A second command while
one has the terminal is refused (`ErrTerminalBusy`).

**How, as the probe settled it** (`task-00-probe.md` beside the plan).
The UI library's own hand-over (`tea.Exec`) does the terminal's modes
and screens rightly, and is used for that; over an SSH channel it loses
input at every hand-over, because it cannot stop a read of something
that is not a file. So `sshd` reads the channel itself, in one goroutine
per session, and gives the bytes to whoever has the terminal: the UI
through a reader that ends when a command gets the terminal, the
command through one that ends when its turn does. Before the UI's
reader ends it is given a mark, a terminal report that no terminal
sends, and the hand-over waits until the UI has taken it: every key
typed before the hand-over has then been delivered, in order. The
command's output goes to the SSH channel itself, past the emulated
terminal's writer, which turns every line feed into carriage return and
line feed and would break any full-screen program. The window's sizes
reach the command from the session's window directly, since the UI's
program takes no message while a command runs. Measured: 1,200
hand-overs with typing across both seams, no byte lost or out of order,
about a third of a millisecond each.

## 6. `tui`

- A binding `s`, `Shell`, for pods and for the containers of a pod,
  that a read-only grant does not have and that needs the resource
  `pods/exec` to exist (it does wherever pods do).
- The dialog of §1: the choice field and the text field that the
  dialogs have, with the starting focus and the meaning of `enter` in
  the Command field as §1 says. The pod is read once more when `s` is
  pressed, for its running containers and its annotation; that reading
  holds the action keys as the readings before the other dialogs do.
- On Shell the dialog closes and the terminal is handed over (§5) to a
  function that writes the line of §1.3 and calls `Exec` with the
  terminal's input, output and sizes. One shell at a time: while one
  has the terminal the UI takes no key at all.
- On the return: a status that is not zero, in the flash; a command
  that did not start, the dialog again with its reason (§1.5); any
  other error, in the flash as every action's is. The table is read
  again.
- `tui.Options` gets the terminal function from `cli`, which has it
  from the session. Without one (the tests of other things) the binding
  is not there.

## 7. Security, and the log

- **Who.** A full key, and only through the UI's dialog. A read-only
  key has no binding, and `Exec` refuses it whatever the UI does.
- **Into what.** Whatever the ServiceAccount's role allows: with the
  chart's default, `cluster-admin`, every container of every pod. A
  narrower role needs `get` and `create` on `pods/exec`; without them
  the API refuses and the dialog says so. kosh adds no rule of its own
  about which pods.
- **The log** has two lines for every shell, with the key's name and
  fingerprint as every action's:
  - `action=exec` when it is asked for: the pod, the container, the
    command as its words;
  - `action=exec` again when it has ended: how long it lasted, whether
    it started, its status or the error (an API error as its reason and
    code, as always).
  The log never holds what was typed in a shell or what it printed, and
  kosh keeps no record of either. Kubernetes' audit log shows the exec
  request under kosh's ServiceAccount.
- **The command's text** is the user's; it is cleaned before it is
  drawn or logged, and cut to 1,024 characters as every input is.
- **A shell's connection does not outlive its session.** The stream is
  tied to the session's context; a key that leaves the list loses its
  shell with its session, within the time §6 of the stage 1 design
  gives, and nobody can type into that shell again.
- **What was running in the container can outlive it.** When the
  stream is cut rather than the shell ended (`exit`, ctrl-d), the
  container's runtime leaves the shell and what it started running,
  without a terminal, until they end by themselves or the container
  does. That is the API's exec, the same with `kubectl exec` when its
  client goes away, and kosh has no call to end them: the API offers
  none. kosh says so in its log: the end line of such a shell carries
  `ended="the session ended"`, with the pod and the container, so that
  whoever revokes a key knows where to look.
- **What it holds.** One stream and a few goroutines per shell, one
  shell per session; nothing is buffered beyond the stream's own
  windows.
- kosh still starts no process of its own, and nothing new is exposed.

## 8. The chart and the README

Nothing changes in the chart: `cluster-admin` has `pods/exec`. The
README says what the key does, that the command can be changed and why,
that a narrower role needs `pods/exec`, and that a shell is not recorded.
"A shell in a pod" leaves "Not yet"; attach, port-forward and copying
stay.

## 9. Tests

- **`kube`**: with the fake clients, that a read-only grant and an
  ended session are refused before any request and logged, and what is
  logged at the start and at the end; the words of the command reach
  the request as given. The stream itself needs a kubelet: it is tested
  end to end.
- **`sshd`**: the hand-over over a real SSH connection, without a
  cluster: a function that echoes gets every byte typed after the
  hand-over and the UI every key after the return, over at least 200
  cycles, with bytes typed across both seams; raw output (a bare line
  feed arrives as one); sizes at the start and on a change; a session
  that ends in the middle; nothing left behind.
- **`tui`**: the binding for a full key on pods and containers, none
  for a read-only key and on other kinds; the dialog (one container,
  several, the default by the annotation, no running container); the
  cutting of the command into words, as a table; `enter` in the Command
  field starts it; a refused start brings the dialog back with the
  reason and the text; a status in the flash; golden frames of the
  dialog at 80 by 24 and 120 by 40.
- **End to end** (`scripts/test-e2e.sh`, the real `ssh` and
  `iroh-ssh`): a shell in a busybox pod: a command and its output; a
  resize seen by `stty size`; `exit` and the table is back and follows
  the cluster; a command changed to one that does not exist: the dialog
  again with the reason; a pod without a shell (kosh's own image):
  the default fails with the reason, and nothing hangs; the read-only
  key has no `<s> Shell` and `s` does nothing; a key removed while its
  shell is open: the user reads that the key is no longer accepted, the
  session ends, and the log's end line says the session ended the
  shell; the two log lines of an ordinary shell.

## 10. Settled first

A probe answered these against the real libraries and a real cluster
before the plan was written; its report is `task-00-probe.md` beside
the plan.

- The UI library's hand-over loses input over an SSH channel, always;
  `sshd` reading the channel itself, with a mark that drains the UI's
  reader, loses nothing (§5).
- The emulated terminal's writer breaks a full-screen program's output;
  the SSH channel itself does not.
- The exec stream over WebSocket gives typed errors for the API's
  refusals and an exit status for a command that ended; "no such
  program" and "container not running" are plain text.
- On Kubernetes 1.35 the WebSocket exec needs `get` and `create` on
  `pods/exec`.
- A stream that is cut leaves the remote shell running (§7).
- `go.mod` gains `k8s.io/streaming`, and `gorilla/websocket` and
  `moby/spdystream` as indirect requirements.

## 11. Not in this change

Attach, port-forward, copying files; Windows containers (the default
command is a POSIX one; the field takes any); starting a debug
container; a record of what was typed; remembering a command between
dialogs or sessions; a limit on which pods a full key may enter beyond
the role's.

## 12. Release

This is v0.2.0. The spec's §21 and §22 and the README's "Not yet" are
brought up to date with it.
