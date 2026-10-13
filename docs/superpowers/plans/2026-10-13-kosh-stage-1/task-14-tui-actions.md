# Task 14: tui, the actions, their dialogs and the editor

The keys that change the cluster, the dialogs that ask before they do, and the editor for an object's YAML. A full key of kosh is usually a cluster administrator, and a key pressed by mistake must cost nothing: every action asks first, and the answer that changes nothing is where the cursor starts.

**Read first:** the spec, §10.5, §11 and §12 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints and Review Focus (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); the frame you hang into: `tui/view.go` (the contract: `dialog`, `view`, `Capturing`, the messages, `env`, `apiTimeout`, `binding`, `bind`), and what Task 11 built: `tui/root.go`, `tui/table.go`, `tui/clean.go`, `tui/styles.go`, and `tui/harness_test.go` with one or two of its tests; `kube/kube.go` for `Actions`, `DeleteOptions`, `DrainOptions`, `DefaultDrainOptions`, `Propagation`, `ContainerImage`, and `Reader`'s `YAML`, `Images`, `Replicas`; `kube/kubetest/kubetest.go` (`Calls`, `Called`, `Errs`, `ImagesOf`, `ReplicasOf`, `YAMLs`, `TriggerName`).

**Files:**
- Create: `tui/dialog.go` (the confirm dialog and the form dialog), `tui/editor.go` (the YAML editor), `tui/bind_actions.go` (the keys), `tui/actions.go` (running an action over rows and saying what came of it)
- Create: `tui/dialog_test.go`, `tui/editor_test.go`, `tui/bind_actions_test.go`, `tui/actions_test.go`; golden frames under `tui/testdata/`
- Do not change any file of Task 11. Two other tasks are adding views to this package at this moment, in `tui/text.go`, `tui/yamlcolor.go`, `tui/bind_text.go`, `tui/logs.go` and `tui/bind_logs.go`; every package-level name you add begins with `action`, `dialog`, `confirm`, `form` or `editor`.

**Interfaces:**
- Consumes: `kube.Session` (every method of `Actions`; `YAML`, `Images`, `Replicas`, `Grant`); `charm.land/bubbles/v2/textarea` and `textinput`; from Task 11: `clean`, `fit`, the styles, the harness; from `tui/view.go`: everything.
- Produces: the bindings below, registered with `bind` from an `init` in `tui/bind_actions.go`.

## Behaviour

**Who has these keys**

1. Every binding of this task applies only to a grant that is not read-only: `Applies` returns false for `grant.ReadOnly`, so a read-only session has none of them in its hints or its help, and pressing them does nothing (spec §12). One helper holds that check; every binding goes through it.
2. None applies to tables of `kube.Containers` or `kube.Rules`.

**The keys (spec §11.1)**

| Key | Hint | Order | Applies to tables of | Does |
|---|---|---|---|---|
| `ctrl+d` | `{"ctrl-d", "Delete"}` | 30 | any resource with the verb `delete` | the delete dialog, then `Delete` |
| `e` | `{"e", "Edit"}` | 31 | any resource with the verb `update` | the editor, for the first row it is given |
| `ctrl+k` | `{"ctrl-k", "Kill"}` | 32 | pods | confirm, then `Kill` |
| `s` | `{"s", "Scale"}` | 33 | deployments, statefulsets, replicasets | the scale dialog, then `Scale` |
| `r` | `{"r", "Restart"}` | 34 | deployments, statefulsets, daemonsets | confirm, then `Restart` |
| `i` | `{"i", "Set Image"}` | 35 | deployments, statefulsets, daemonsets | the image dialog for the first row, then `SetImages` |
| `ctrl+l` | `{"ctrl-l", "Rollback"}` | 36 | replicasets | confirm, then `Rollback` |
| `t` | `{"t", "Trigger"}` | 37 | cronjobs | confirm, then `Trigger` |
| `s` | `{"s", "Suspend/Resume"}` | 38 | cronjobs | confirm, then `SetSuspended` |
| `c` | `{"c", "Cordon"}` | 39 | nodes | confirm, then `Cordon(true)` |
| `u` | `{"u", "Uncordon"}` | 40 | nodes | confirm, then `Cordon(false)` |
| `r` | `{"r", "Drain"}` | 41 | nodes | the drain dialog, then `Drain` |

`s` and `r` each have two bindings for kinds that do not overlap. A test shows that no table has two bindings of this task for one key, and that none of these keys is one the table uses itself.

Whether a cronjob is suspended now is in its row: the binding cannot see the row, only its reference, so `Suspend/Resume` asks which: its dialog offers `Suspend`, `Resume` and `Cancel`.

**Every action asks first (spec §11.1)**

3. The confirm dialog says what will be done to what: `Kill pods/default/web-1?`, and for several rows `Restart 3 deployments?` with their names listed below, at most ten and then `… and 4 more`. Names are cleaned. Its buttons are `Cancel` and the action's word, and **`Cancel` has the focus when it opens**. `←` `→` `tab` `shift+tab` move the focus; `enter` presses the focused button; `esc` cancels; `y` and `n` do nothing: an answer is always two deliberate keys. Cancel closes the dialog and calls nothing.
4. The form dialog is the confirm dialog with fields above its buttons: a text field, a number field, a choice of several, a checkbox. `tab` and `↓` go to the next field or button, `shift+tab` and `↑` to the one before; `space` toggles a checkbox and steps through a choice; `enter` on a field goes to the next; the focus starts on the first field, and the buttons start with `Cancel` before the action's word, so that `enter` held down does not confirm. A form whose values do not validate shows why in a line of the dialog and does not close.
5. **Delete**: a form with a choice `Propagation` of `Background`, `Foreground`, `Orphan` (`Background` first) and a checkbox `Force` (off).
6. **Scale**: a form with a number field `Replicas`, filled with `sess.Replicas` of the first row, read in a command before the dialog opens; an error there is a flash and no dialog. Only digits are taken; an empty field does not validate.
7. **Set image**: a form with one text field for each container `sess.Images` returns, labelled with its name (init containers marked `(init)`), filled with its image. Only the containers whose image was changed are sent; none changed closes the dialog with the flash `nothing changed`.
8. **Drain**: a form with `Grace Period` (a number, may be negative: `-1` means each pod's own), `Timeout` (a duration as `time.ParseDuration` reads it), and checkboxes `Ignore DaemonSets`, `Delete EmptyDir Data`, `Force`, `Disable Eviction`, starting from `kube.DefaultDrainOptions()`.

**Running an action**

9. On confirm the dialog closes and the action runs in a command with a context of `apiTimeout`, never in `Update`; for a drain the context's timeout is the dialog's `Timeout` plus `apiTimeout` when the dialog's is not zero, and none when it is. For several rows the session's method is called once for each row, one after the other, and an error on one does not stop the rest.
10. The outcome is a flash: `Restarted deployments/default/web`; for several rows `Restarted 3 deployments`, or `Restarted 2 of 3 deployments: <the first error>` in the error style. An error is shown as the session worded it, cleaned. A trigger's flash names the job. After the action the table is told to read again (`refreshMsg`).
11. While an action of a session is running, a second key of this task shows the flash `an action is still running` and opens nothing.

**The editor (spec §11.2)**

12. `e` reads `sess.YAML(ref, false)` in a command; an error is a flash and no editor. Then it pushes the editor: a view that is `Capturing`, with the YAML in a `textarea` that fills the body, line numbers on. Its title is `Edit(<ref>)`, with ` [modified]` once the text differs from what was read; its crumb is `edit`.
13. `ctrl+s`: text that was not changed closes the editor with the flash `nothing changed` and calls nothing. Changed text is sent with `sess.Update(ctx, ref, text)` in a command. Accepted: the editor closes, the flash says `Updated <ref>`, and the table reads again. Refused: the editor stays with the text exactly as it was typed, and the error, cleaned, is shown in a line at the bottom of the editor until the next key press.
14. `esc`: unchanged text closes the editor. Changed text opens a confirm dialog, `Discard the changes to <ref>?`, with `Cancel` (focused) and `Discard`.
15. `ctrl+c` still ends the session: the root takes it before any view.
16. Every other key goes to the text area. The YAML that is put into it is cleaned line by line first; what the user types is sent as typed.
17. While `Update` is running the editor takes no keys and its title says ` [saving]`.
18. The log of an edit holds no YAML; that is `kube`'s business. Nothing in this package logs.

## Tests

With the harness of Task 11 and `kubetest`: a full grant unless the test says otherwise; tables set with `SetTable`; what was done is read from `Calls()`.

| Test | What it shows |
|---|---|
| `TestReadOnlyHasNoActionKeys` | for every resource of `kubetest.Standard()` and a read-only grant, `bindingsFor` holds no binding of this task; pressing each key of the table of keys above in a pods, a deployments and a nodes table opens no dialog and records no call |
| `TestActionKeysByKind` | table-driven over the table above: each kind has exactly its keys, with their hints and orders; configmaps have `ctrl-d` and `e` only; `kube.Containers` and `kube.Rules` have none; a resource without the verb `delete` has no `ctrl-d`, one without `update` no `e` |
| `TestNoKeyTwiceInATable` | for every resource of `Standard()`: no key has two bindings of this task, and none is a key of the table itself |
| `TestConfirmStartsOnCancel` | golden: `ctrl+k` on a pod: the dialog `Kill pods/default/web-1?`, focus on `Cancel`; `enter` at once closes it and `Called("Kill")` is empty |
| `TestConfirmNeedsTwoKeys` | `→` then `enter` kills; `y` does nothing; `esc` cancels |
| `TestConfirmListsMarkedRows` | 14 pods marked: the dialog says `Kill 14 pods?`, lists ten names and `… and 4 more`; confirm calls `Kill` 14 times, in the table's order |
| `TestActionGoesOnAfterAnError` | three marked, `Errs["Kill"]` set for all: three calls, and the flash is in the error style with `0 of 3` and the error |
| `TestFlashAfterAnAction` | one row: `Killed pods/default/web-1`; three: `Killed 3 pods`; the table reads again (`refreshMsg` reached it) |
| `TestDeleteDialog` | golden; defaults give `DeleteOptions{Propagation: Background}`; choosing `Foreground` and `Force` gives those; cancel calls nothing |
| `TestScaleDialog` | the field starts with the fake's `ReplicasOf`; typing `5` and confirming calls `Scale` with 5; letters are not taken; an empty field shows why and stays open; `Errs["Replicas"]`: a flash and no dialog |
| `TestSetImageDialog` | golden with two containers and an init container; changing one sends only that one; changing none flashes `nothing changed` and calls nothing; `Errs["Images"]`: a flash and no dialog |
| `TestDrainDialog` | golden; the defaults are `kube.DefaultDrainOptions()`; `Timeout` of `abc` shows why and stays open; ticking two boxes and setting the grace period to 30 gives those options to `Drain` |
| `TestSuspendResume` | the dialog offers both; each calls `SetSuspended` with its value |
| `TestRestartRollbackTriggerCordon` | table-driven: each key, confirmed, calls its method with the row's reference (`Cordon` with true for `c`, false for `u`); the trigger's flash has `TriggerName` |
| `TestNoSecondActionWhileOneRuns` | with the first `Kill` blocked in the fake, `ctrl+k` again flashes `an action is still running` |
| `TestNothingIsCalledInUpdate` | the session's method is called from a command, not from `Update` |
| `TestDialogNamesAreCleaned` | a row whose name holds `\x1b[2J`: the dialog's drawn text does not |
| `TestEditorOpens` | golden: `e` on a configmap: `Edit(configmaps/default/cfg)`, the YAML with line numbers, crumbs `<configmaps> <edit>` |
| `TestEditorTakesEveryKey` | `:` and `?` and `/` typed in the editor are text, not commands |
| `TestEditorSaveUnchanged` | `ctrl+s` at once: `nothing changed`, no `Update`, the editor is closed |
| `TestEditorSave` | type a character, `ctrl+s`: `Update` is called with the reference and the exact text; the editor closes; the flash says `Updated …`; the table reads again |
| `TestEditorSaveRefused` | `Errs["Update"]`: the editor stays, the text is byte for byte what was typed, the error is shown, and goes at the next key |
| `TestEditorEscape` | unchanged: closes. Changed: the discard dialog with `Cancel` focused; `Cancel` returns to the editor with the text intact; `Discard` closes it and calls nothing |
| `TestEditorYAMLErrorIsAFlash` | `Errs["YAML"]`: no editor |
| `TestEditorCleansWhatItShows` | a YAML holding `\x1b]0;x\x07`: the text area's content does not |
| `TestEditorWhileSaving` | with `Update` blocked: keys change nothing and the title says `[saving]` |

## Steps

- [ ] The confirm dialog and the form dialog with its four kinds of field, opened by test code: tests first. Commit: `TUI: a dialog that asks, and starts on the answer that changes nothing`.
- [ ] Running an action over rows, the flash, the refresh, one action at a time: tests first, with a binding of the test. Commit: `TUI: an action over the marked rows, and what came of it`.
- [ ] The bindings and their dialogs. Commit: `TUI: delete, kill, scale, restart, set image, roll back, trigger, suspend, cordon and drain`.
- [ ] The editor. Commit: `TUI: an editor for an object's YAML`.
- [ ] `go test -race ./tui/...`; `gofmt -l . && go vet ./... && go build ./...` clean.

## Report

What you built and the test results; anything of Task 11 that you needed and did not find, or found different from `tui/view.go`; anything you were tempted to change in its files and why.
