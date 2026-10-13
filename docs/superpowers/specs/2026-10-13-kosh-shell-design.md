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
   $PATH`), the API's refusal (`pods "web" is forbidden: User … cannot
   create resource "pods/exec"`), or that the container is not running.
   That is the moment the Command field is for.
6. **When kosh or the key goes.** A shell ends when its session does
   (§5 below): the key leaves the list, the client goes away, kosh
   stops. The user sees what a session sees then (`kosh: this key is no
   longer accepted`, `kosh is shutting down`), on a terminal that is
   back in its normal state.

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
// kubectl exec -it does, and returns when the command has ended. Its
// input and output are the caller's; the sizes of the terminal are read
// from Sizes until it is closed or the command ends.
Exec(ctx context.Context, pod ObjectRef, opts ExecOptions) (ExecResult, error)

type ExecOptions struct {
	Container string        // "" is the pod's default container
	Command   []string      // the command and its arguments; not empty
	Stdin     io.Reader     // the terminal's input
	Stdout    io.Writer     // the terminal's output; with a terminal there is no separate stderr
	Sizes     <-chan TermSize // the terminal's size: the first one before anything is drawn, then each change
}

type TermSize struct{ Width, Height int }

type ExecResult struct {
	Started bool // the command was started in the container
	Status  int  // its exit status; 0 when it ended well
}
```

- It is an action: `begin` refuses a read-only grant and a session that
  has ended, and it is logged (§6). It runs with a context that ends
  with the caller's and with the session's, as every action does: when
  either ends, the stream is closed and the API ends the command.
- The stream is the API's exec subresource of the pod, with a terminal,
  over WebSocket with the fallback to SPDY that kubectl uses.
- **Errors a caller tells apart.** `ErrReadOnly`; the API's refusal
  (forbidden, not found), as it is; and a new `ErrExecNotStarted`,
  wrapped around the runtime's words, when the stream was made and the
  command could not be started (no such program, container not
  running). A command that started and ended with a status that is not
  zero is not an error: it is `ExecResult{Started: true, Status: n}`.
- The reference is checked as every reference is (`checkRef`); the
  container's name must be a DNS label; no word of the command is
  checked beyond there being at least one.

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
// Terminal hands the session's terminal to run, which has it until it
// returns: its input, its output as raw bytes, and its sizes. The
// command it returns is run by the program like any other; the message
// of the callback arrives when the terminal is the program's again.
Terminal func(run func(t RawTerminal) error, done func(error) tea.Msg) tea.Cmd

type RawTerminal struct {
	In    io.Reader
	Out   io.Writer
	Sizes <-chan Size // the size now, then each change; closed at the return
}
```

How it is done inside `sshd` is settled by the probe (§10) and written
here when it is.

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
  narrower role needs `create` and `get` on `pods/exec`; without them
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
- **A shell does not outlive its session.** The stream is tied to the
  session's context; a key that leaves the list loses its shell with
  its session, within the time §6 of the stage 1 design gives.
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
  shell is open: the shell ends and the remote process is gone; the two
  log lines.

## 10. Settled first

Whether the UI library's own hand-over (`tea.Exec`) loses or misroutes
input over an SSH channel that is not a terminal device, and what
`sshd` does about it if so; how the shell's output gets past the
emulated terminal's line-feed translation; how the sizes reach the
shell while the UI is not running; and how the exec stream of this
client-go behaves at its edges (a command that is not there, a status,
a cancelled context). A probe answers these against the real libraries
and a real cluster before the plan is written; its report is beside the
plan.

## 11. Not in this change

Attach, port-forward, copying files; Windows containers (the default
command is a POSIX one; the field takes any); starting a debug
container; a record of what was typed; remembering a command between
dialogs or sessions; a limit on which pods a full key may enter beyond
the role's.

## 12. Release

This is v0.2.0. The spec's §21 and §22 and the README's "Not yet" are
brought up to date with it.
