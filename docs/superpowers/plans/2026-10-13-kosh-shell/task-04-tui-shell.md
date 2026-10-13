# Task 4: `tui`: the key, the dialog, the words

`s` on a pod or on a container of a pod: a dialog with the container and the command, and then the hand-over of the terminal to a shell. This task builds everything of that which is the UI's: it never opens a stream and never touches the SSH session. It calls two things of the contract: the session's `Exec`, and the function that hands the terminal over.

**Read first:** the spec, whole (`docs/superpowers/specs/2026-10-13-kosh-shell-design.md`), above all §1, §2, §3, §6; stage 1's spec §10.5 and §11.1 for what dialogs are (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints and Review Focus (`docs/superpowers/plans/2026-10-13-kosh-shell.md`); the contract of Task 1: `tui/tui.go` (`Options.Handover`, `Terminal`) and `tui/view.go` (`env.handover`), `kube/kube.go` (`Exec`, `ExecOptions`, `ExecResult`, `ErrExecNotStarted`), `kube/kubetest/kubetest.go` (the fake's `Exec`); and the code this joins: `tui/bind_actions.go`, `tui/actions.go` (`actionGuard`, `actionRead`, `actionBegin`/`actionEnd`, `actionOutcome`), `tui/dialog.go`, `tui/root.go` (how a dialog is opened and closed and what a resize does to it), `tui/flash.go`, `tui/clean.go` (`clean`, `cleanLine`, `errorText`), `tui/help.go`, `tui/harness_test.go`.

**Files:**
- Create: `tui/shell.go` (the binding, the dialog, what happens on Shell and on the return), `tui/words.go` (the command's text cut into words), and their tests `tui/shell_test.go`, `tui/words_test.go`; golden frames under `tui/testdata/`
- Modify: `tui/dialog.go` only as far as the dialog of §1 needs what the form dialog does not have (a field that cannot be changed; a field in which `enter` presses a button; a reason under the fields is there already), `tui/tui.go` and `tui/view.go` not at all (Task 1 put `Options.Handover` into `env.handover`), `tui/help.go` only if the help does not pick the binding up by itself
- No other file. `go.mod` does not change.

**Interfaces:**
- Consumes: `kube.Session.Exec`, `kube.ExecOptions`, `kube.ExecResult`, `kube.ErrExecNotStarted`, `kube.ErrReadOnly`; `env.handover`, which is `tui.Options.Handover`: `func(run func(context.Context, Terminal) error, done func(error) tea.Msg) tea.Cmd` (the UI returns that command from its `Update`; `run` is called with the terminal and a context that is done when the session ends; the message `done` makes arrives when the terminal is the UI's again; `done` is told an error and `run` is not called when the terminal could not be handed over); reading a pod for its containers through what the session already has (`YAML`, or the containers table's own query: choose the one that costs one request, and say which).
- Produces: nothing another task consumes.

## Behaviour

**The binding**

1. `s`, hint `Shell`, applies to the pods table and to the containers table of a pod, for a full grant, when `env.handover` is set. It does not apply for a read-only grant, on any other table, or without a hand-over function. It has an order that puts it beside `Logs` in the header's hints. The help lists it.
2. It runs on the row under the cursor (one row: marks do not make it several; with rows marked it is still the cursor's row, and the dialog says which pod).
3. Like every action key it is behind `actionGuard`: while another action or dialog of this session is under way it flashes that one is.

**The dialog**

4. Pressing `s` reads the pod (holding the action keys as the readings before the other dialogs do) for: its containers that are running now, in the order containers, then init containers that are running, then ephemeral containers that are running; and its annotation `kubectl.kubernetes.io/default-container`.
5. No running container: no dialog; the flash `pod <namespace>/<name> has no running container`. A reading that fails: the flash with the error, as every action's.
6. Otherwise the dialog of the spec's §1.2: the title `Shell`; the line `A shell in pod <namespace>/<name>`; a field `Container`; a field `Command`; the buttons `Cancel` and `Shell`.
   - `Container`: from the pods table it is a choice of the running containers, the one chosen first being the annotation's when it is among them, else the first. With one container, and always from the containers table (where it is the row's container), it is shown and cannot be changed, and the focus never stops on it.
   - `Command`: a text field holding `sh -c "command -v bash >/dev/null && exec bash || exec sh"`, with the limit of characters every input has, the cursor at its end, scrolling sideways when it is longer than the dialog is wide.
   - The focus starts in `Command`. `enter` in `Command` does what the button `Shell` does. `tab`, `shift+tab` and the arrows move as in every dialog (`left` and `right` inside the text field move its cursor); `enter` on `Cancel` and `esc` close the dialog and nothing happens.
   - It obeys what every dialog obeys: laid over the whole screen, never taller than it, its buttons always drawn, refused with the flash on a screen too small for it.
7. On `Shell`: the command's text is cut into words (below). No words, or a quote that is not closed: the reason is shown in the dialog (`the command is empty`, `a quote is not closed`) and the dialog stays. Otherwise the dialog closes and the terminal is handed over.

**The words** (`tui/words.go`, a function `words(text string) ([]string, error)`)

8. Words are separated by spaces and tabs. `'…'` makes one word of what it holds, nothing in it is special. `"…"` makes one word of what it holds; inside it a backslash before `"`, `\` or `$` takes that character literally, and any other backslash is itself. Outside quotes a backslash takes the next character literally (a backslash at the end is itself). Quoted and unquoted pieces that touch are one word (`a"b c"d` is `ab cd`). `''` and `""` are an empty word.
9. Nothing else is done: no variables, no globbing, no `;`, `|`, `&&`, no comments. A line feed, a carriage return or any other control character in the text is refused (`the command holds a control character`): a paste must not smuggle a second line.
10. The text is trimmed of spaces at its ends first.

**The shell**

11. The hand-over: the UI returns the command that `env.handover` makes from a function `run` and a callback. `run` gets a context and the terminal (`In`, `Out`, `Size`, `Resized`) and:
    - writes the line of the spec's §1.3 to the output, ending in `\r\n` (the output is raw: the terminal does what the bytes say), with the pod's and the container's names cleaned;
    - calls `Exec` with that context, the pod, the container, the words, and the terminal's `In`, `Out`, `Size` and `Resized` as they are;
    - returns what `Exec` returned (the result is kept for the callback).
    No timeout: a shell lasts as long as the user wants. The context is the hand-over's, which ends with the session.
12. While the terminal is handed over the UI receives no key. The action keys stay held from `s` until the return has been dealt with.
13. The return, by what `Exec` gave:
    - started, status 0: nothing is said;
    - started, another status: the flash `the shell ended with status <n>` (information, not an error);
    - `ErrExecNotStarted`: the dialog again, with the pod, the container as it was chosen, the Command's text as the user left it, and the error's text (cleaned, wrapped, as the dialogs' reason line is) under the fields;
    - `ErrReadOnly`: the flash `this key is read-only`, as everywhere;
    - the hand-over refused (`done` told an error without `run` having been called: a command has the terminal already, or the session has ended): the error flash;
    - any other error (the API's refusal, the stream breaking): the error flash, with the error's text as every action's.
    In every case the table is read again and the cursor is on the row it was on.

## Tests

The fake session (`kube/kubetest`) records the calls of `Exec` and answers with what the test sets. The hand-over function of the tests is a fake of yours: its command runs `run` with a context, an input, an output buffer, a `Size` and a `Resized` that the test holds, and returns `done`'s message. No test starts an SSH server or a cluster.

| Test | What it shows |
|---|---|
| `TestWords` | a table: `sh` → [`sh`]; the default → [`sh`, `-c`, `command -v bash >/dev/null && exec bash \|\| exec sh`]; `/bin/bash -l`; `a  b\tc`; `'a b' c`; `"a \"b\" \\ \$c"` → [`a "b" \ $c`]; `"a\nb"` keeps `\n` as two characters; `a\ b` → [`a b`]; `a"b c"d` → [`ab cd`]; `''` → [``]; `ls *.txt` → [`ls`, `*.txt`]; `echo $HOME; ls` → [`echo`, `$HOME;`, `ls`]; errors: ``, `   `, `"abc`, `'abc`, `a\nb` with a real line feed, `a\x1b[2Jb`, `a\x00b` |
| `TestWordsNeverPanics` | 10,000 random strings of quotes, backslashes, spaces and letters: an answer or an error, in linear time |
| `TestShellBinding` | the hint and the key on pods and on a pod's containers for a full grant; neither for a read-only grant; neither on deployments, nodes, a custom resource; neither without a hand-over function; the help lists it |
| `TestShellDialogOneContainer` | a pod with one running container: the frame (golden, 80x24 and 120x40) has the pod's name, the container as a fixed line, the default command, the focus in Command |
| `TestShellDialogChoosesTheDefaultContainer` | three running containers and the annotation naming the second: the second is chosen first; `left`/`right` on the field change it; without the annotation the first is |
| `TestShellDialogListsWhatRuns` | a pod with two containers of which one is waiting, a sidecar init container that runs, an init container that completed, an ephemeral container that runs: the choice is the running container, the sidecar, the ephemeral one, in that order |
| `TestShellNoRunningContainer` | the flash, no dialog, no `Exec` |
| `TestShellFromTheContainersTable` | `s` on a row of a pod's containers: the dialog's container is that row's and cannot be changed; `Exec` gets it |
| `TestShellEnterStartsIt` | `s`, `enter`: `Exec` is called once with the pod, the container, the default's three words, and the fake terminal's input and output; the first line written to the output is the line of §1.3 |
| `TestShellEditedCommand` | the field cleared and `/bin/bash -l` typed, `enter`: `Exec` gets [`/bin/bash`, `-l`] |
| `TestShellBadCommandStays` | an unclosed quote: the reason in the dialog, the dialog still open, no `Exec`; the same for an empty field |
| `TestShellCancel` | `esc`, and `Cancel`: no `Exec`, the table as before, the action keys free again |
| `TestShellSizes` | `ExecOptions.Size` and `ExecOptions.Resized` are the fake terminal's: what the test makes `Size` return is what `Exec` reads, and a value the test puts on `Resized` is read from `ExecOptions.Resized` |
| `TestShellStatus` | the fake's `Exec` returns started with status 3: the information flash; with status 0: no flash |
| `TestShellDidNotStart` | the fake returns `ErrExecNotStarted` wrapping `exec: "sh": executable file not found in $PATH`: the dialog is open again with that text under the fields and the Command as typed; editing it and `enter` calls `Exec` again with the new words |
| `TestShellRefusedByTheAPI` | the fake returns a forbidden error: the error flash with its text, no dialog |
| `TestShellErrorTextIsCleaned` | an error text and a container name that hold an escape sequence: neither the dialog's reason, the flash nor the line written at the hand-over holds a raw escape |
| `TestShellHoldsTheActionKeys` | from `s` until the return was dealt with, another action key flashes that one is under way; after it they work |
| `TestShellTableIsReadAgain` | after the return the table's watch is read again and the cursor is on the same row |
| `TestShellReadOnlyGrant` | with a read-only grant `s` on a pod does nothing: no dialog, no `Exec`, the frame unchanged |
| `TestEveryTextInputHasNoPaste` and the guard for the character limit | still pass with the dialog's field (the guard finds it) |

## Steps

- [ ] `tui/words.go`, tests first. Commit: `TUI: a command's text is cut into words, and into nothing else`.
- [ ] The binding and the dialog, tests first, golden frames read before they are kept. Commit: `TUI: s on a pod asks which container and which command`.
- [ ] The hand-over and the return, tests first. Commit: `TUI: a shell has the terminal until it ends, and its end is said`.
- [ ] `gofmt -l . && go vet ./... && go build ./...`; `go test -race ./tui/...`; every golden file that changed is looked at, and the report says what changed in each.

## Report

Behaviour by behaviour what you built and where it is tested; the failing and passing runs; how the pod's running containers are read and what it costs; what you changed in `tui/dialog.go` and why the form dialog could not do it as it was; the two golden frames pasted; anything in the contract that did not fit.
