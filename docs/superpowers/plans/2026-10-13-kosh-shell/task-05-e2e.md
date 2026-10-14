# Task 5: end to end, and the README

The shell as its users meet it: kosh's image in a kind cluster, the real `ssh` and the real `iroh-ssh`, a real container's shell. And the README.

**Read first:** the spec, whole (`docs/superpowers/specs/2026-10-13-kosh-shell-design.md`); the plan's Global Constraints and Review Focus (`docs/superpowers/plans/2026-10-13-kosh-shell.md`); the probe's "Q6" (`docs/superpowers/plans/2026-10-13-kosh-shell/task-00-probe.md`) for what was already seen to work by hand; `e2e/e2e_test.go` and `e2e/screen_test.go` for how a session is driven and its screen read, and `scripts/test-e2e.sh`; `README.md`.

**Where this runs.** As the end-to-end tests of stage 1: on Linux with Docker and Nix (your dispatch says which machine and the rules for it).

**Files:**
- Modify: `e2e/e2e_test.go` (the tests below), `e2e/testdata/2-objects.yaml` (the pods the tests need), `scripts/test-e2e.sh` only if a pod's image must be loaded into the cluster beforehand, `README.md`, `docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md` (§21 and §22: a shell is no longer "not in this stage")
- No other file. If a test shows a defect in kosh, report it with the evidence; do not fix it here.

## The tests

Each reads the screen as a user sees it, waits with deadlines, and prints the screen and kosh's log when it fails. They are placed before the tests that change the key list, the role, or restart kosh.

| Test | What it shows |
|---|---|
| `TestShell` | full key, over the forwarded TCP port, 120 by 40: `s` on the row of `marker` (a busybox pod): the dialog with `A shell in pod default/marker`, the container and the default command; `enter`: the line `kosh: a shell in pod default/marker, container …` and a prompt; `echo kosh-$((6*7))` shows `kosh-42`; `stty size` shows `40 120`; the terminal resized to 100 by 30: `stty size` shows `30 100`; `exit`: the pods table is back, complete (`fills`), with `marker` in it, and it follows the cluster (a pod created now appears) |
| `TestShellFullScreen` | in the shell, `vi /tmp/kosh-e2e.txt`, a line typed, `:wq`: while in `vi` the screen's last line is `vi`'s status line and the tildes are in the first column of every empty row (bare line feeds arrived untouched); `cat` shows the line; ctrl-c ends a `sleep 100` with the prompt back and `echo $?` showing 130 |
| `TestShellEditedCommand` | the Command field cleared and `/bin/sh -c "echo edited-$0; exec sh" kosh-e2e` typed (or another command that proves the words arrived as cut): its output is on the screen, then a prompt; `exit` returns |
| `TestShellCommandThatIsNotThere` | the Command field set to `/no/such/shell`: the dialog is back, with the text as typed and the runtime's reason under it (`no such file or directory`); `esc` leaves; the table is as before |
| `TestShellPodWithoutAShell` | `s`, `enter` on kosh's own pod in the namespace `kosh` (an image without any shell): the dialog is back with the reason (`executable file not found`), nothing hangs, `esc` leaves |
| `TestShellStatus` | `exit 3` in the shell: the table is back and the flash says `the shell ended with status 3` |
| `TestShellFromTheContainersTable` | `enter` on `marker` for its containers, `s` on the container's row: the dialog's container is that one and cannot be changed; the shell opens |
| `TestShellSeveralContainers` | a pod `pair` with two containers, the annotation `kubectl.kubernetes.io/default-container` naming the second: the dialog's container is the second; changed to the first with the arrow keys; the shell's `hostname`/a file only that container has shows which container it is in |
| `TestShellOverIroh` | the same as the first steps of `TestShell` through `iroh-ssh` (a command, its output, `exit`, the table back) |
| `TestShellReadOnlyKey` | the read-only key: no `<s> Shell` in the header on the pods table; `s` does nothing (no dialog after 2 seconds, the frame unchanged); the positive control is `TestShell`, which sees `<s> Shell` for the full key |
| `TestShellIsLogged` | after a shell that ran `echo kosh-secret-marker` and exited: kosh's log has an `action` line with `action=exec`, `phase=start`, the full key's name and fingerprint, `pods/default/marker`, the container and the command's words; and one with `phase=end`, `started=true`, `status=0` and a duration; and `kosh-secret-marker` is nowhere in kosh's log |
| `TestShellEndsWithItsKey` (among the tests that change the key list, with their cleanup that waits for the key to be accepted again) | a key added for this test opens a shell and starts `sleep 300`; the key is removed from the list: within the time a removed key's session ends, the client reads `this key is no longer accepted` and its `ssh` ends; kosh's log has the shell's end line with `ended="the session ended"`; and, as a record of the limit the spec states, the test logs (not asserts) whether `sleep 300` is still running in the container |
| `TestShellSurvivesNothing` | the client's `ssh` killed while a shell is open: kosh logs the session's end and the shell's end line within seconds; a new session with the same key works and shows the table |

## The README

- "Using it": `s` on a pod or a container opens a shell; the dialog and why the command can be changed (an image without `sh`, with only `bash`, with another shell); that the command is words, not a shell line (`sh -c '…'` for that); `exit` or ctrl-d to come back.
- "What a key can do": a shell is an action, so full keys only; into whatever the role allows; a narrower role needs `get` and `create` on `pods/exec`; kosh logs who opened a shell in what, with which command, for how long, and never what was typed in it; nothing is recorded.
- "Known issues": when a session ends while its shell is open (the key removed, the connection lost, kosh restarted), what was running in the container goes on running there without a terminal until it ends or the container does, as with `kubectl exec`; kosh's log names the pod and container.
- "Not yet": a shell leaves the list; attach, port-forward and copying files stay.
- The frame at the top and the list of keys get the new key where they list keys.

Every command and key sequence the README shows is one you ran.

## Steps

- [ ] The pods the tests need, and `TestShell`, seen to fail first (against the released image `ghcr.io/fables-for-robots/kosh:v0.1.0`, which has no shell key: `KOSH_E2E_IMAGE` lets the script use an image that is already loaded). Commit: `E2E: a shell in a pod, typed into and left`.
- [ ] The other tests. Commit: `E2E: the shell's command changed, refused, logged, and ended with its key`.
- [ ] The README and the two sections of the stage 1 spec. Commit: `README: a shell in a pod`.
- [ ] `scripts/test-e2e.sh` and `scripts/test-mdns.sh` from a clean checkout, twice in a row: all four exit 0. Nothing is left on the machine.

## Report

The four runs' test lines and exit codes; every defect the tests showed in kosh, with the screen or the log; what you had to work around; what of the README you could not verify; what the machine looked like at the end.
