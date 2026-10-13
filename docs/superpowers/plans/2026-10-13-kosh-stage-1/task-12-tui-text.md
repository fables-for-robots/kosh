# Task 12: tui, describe, YAML and a secret decoded

The views that show text about one object: what `kubectl describe` prints, the object's YAML, and the entries of a Secret decoded. They are where the most text from the cluster reaches the terminal, so everything they draw is cleaned first.

**Read first:** the spec, §10.3, §10.5 and §12 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints and Review Focus (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); the frame you hang into: `tui/view.go` (the contract: `view`, `filterable`, `hint`, the messages, `env`, `apiTimeout`, `binding`, `bind`), and what Task 11 built: `tui/root.go`, `tui/table.go`, `tui/clean.go`, `tui/styles.go`, and `tui/harness_test.go` with one or two of its tests, to see how a test drives the UI and reads a frame; `kube/kube.go` for `Reader`; `kube/kubetest/kubetest.go`.

**Files:**
- Create: `tui/text.go` (the text view), `tui/yamlcolor.go` (colouring YAML), `tui/bind_text.go` (the keys `d`, `y`, `x`)
- Create: `tui/text_test.go`, `tui/yamlcolor_test.go`, `tui/bind_text_test.go`; golden frames under `tui/testdata/`
- Do not change any file of Task 11. Two other tasks are adding views to this package at this moment, in `tui/logs.go`, `tui/bind_logs.go`, `tui/dialog.go`, `tui/editor.go` and `tui/bind_actions.go`; every package-level name you add that the list below does not fix begins with `text` or `yaml`.

**Interfaces:**
- Consumes: `kube.Session` (`Describe`, `YAML`, `SecretData`, `Grant`), `kube.ErrReadOnly`; from Task 11: `clean`, `fit`, the styles, the harness; from `tui/view.go`: everything.
- Produces for Task 14, with exactly this name: nothing. (Task 14's editor reads the YAML itself.)
- Produces: the bindings of `d`, `y` and `x`, registered with `bind` from an `init` in `tui/bind_text.go`.

## Behaviour

**The keys**

1. `d` (hint `{"d", "Describe"}`, order 10) and `y` (hint `{"y", "YAML"}`, order 11) apply to every table whose resource is not `kube.Containers` and not `kube.Rules`: their rows are not objects of the API. They open the text view for the first of the references they are given.
2. `x` (hint `{"x", "Decode"}`, order 12) applies to tables of Secrets (`res.Is("", "secrets")`).
3. All three apply to a read-only grant too. For a Secret the session then refuses, and that refusal is shown as every error of the first load is (6): the view does not open; the flash says `this key is read-only`.

**The text view**

4. One view, three kinds of content. Its title is `Describe(<ref>)`, `YAML(<ref>)` or `Decode(<ref>)`, with `ref.String()`; its crumb is `describe`, `yaml` or `decode`.
5. The content is loaded in a command with a context of `apiTimeout`, never in `Update`: `sess.Describe`, `sess.YAML(ref, managedFields)`, or `sess.SecretData`, whose entries are drawn as `key: value`, a value of several lines indented under its key, sorted as they come.
6. An error of the **first** load: the view is not pushed, and the flash shows the error. An error of a later load (10): the text stays and the flash shows the error. A binding therefore returns a command that loads, and only then pushes the view with its content.
7. **Everything is cleaned before it is drawn** (Review Focus 1): each line of the content goes through `clean`. Lines are cut at the view's width with `fit` unless wrapping is on (9).
8. The view scrolls with `↑` `↓` `j` `k`, a page with `pgup` `pgdown` `ctrl+b` `ctrl+f` and `space`, to the ends with `g` `G` `home` `end`, and sideways with `←` `→` when lines are longer than the width and wrapping is off. A position past the end after the content shrank is brought back in.
9. `w` toggles wrapping of long lines. `f` toggles full screen (`Fullscreen`). `c` copies the whole content, cleaned, to the user's clipboard with `tea.SetClipboard` and flashes `copied <n> lines`.
10. YAML only: `r` turns reading the object again every 2 seconds on and off, off at first, and the title shows ` (auto)` while it is on; the interval is a field tests shorten. `m` toggles `managedFields` and reads again. The scroll position is kept across a reload.
11. **Search.** The view is `filterable`: the text of the `/` prompt is searched in the content as a regular expression without regard to case, and as it stands when it is no regular expression. Every match is drawn in `styleMatch`; the view scrolls to the first match at or after the top line. `n` goes to the next match and `shift+n` (`N`) to the one before, around the ends. The title shows `</text> 3/17`: the match the view is at and how many there are, or `0/0`. The empty text clears the search.
12. `esc` clears a search when there is one, and otherwise pops the view; `q` pops it.
13. `Hints` lists the keys above that apply to the kind of content.
14. `Close` stops the reloading.

**Colouring YAML**

15. `yamlColor(line string) string` takes one cleaned line of YAML and returns it styled: a key (what is before the first `: ` or a final `:` outside quotes, after indentation and an optional `- `) in `styleKey`, the value after it in the default colour, a comment in `styleDim`. It never changes the text: stripped of styles, the result is the line. A line it does not understand is returned as it is. The search's highlighting is applied over it.

## Tests

With the harness of Task 11 and `kubetest`: set `YAMLs`, `Descriptions`, `Secrets` and `Errs` on the fake, start in a table with rows, press the key.

| Test | What it shows |
|---|---|
| `TestDescribeOpens` | golden: `d` on a pod: title `Describe(pods/default/web-1)`, the description's text, crumbs `<pods> <describe>`; `esc` returns to the table |
| `TestYAMLOpens` | golden: `y`: title `YAML(…)`, the YAML |
| `TestDecodeOpens` | golden: `x` on a secret: `Decode(…)`, `key: value` lines, a value of three lines indented |
| `TestDecodeOnlyOnSecrets` | `x` in a table of pods does nothing and is not in the hints |
| `TestNoTextKeysOnContainersAndRules` | tables of `kube.Containers` and `kube.Rules`: neither `d` nor `y` |
| `TestFirstLoadErrorIsAFlash` | `Errs["Describe"]` set: no view is pushed, the flash has the error |
| `TestReadOnlySecret` | a read-only grant, a table of secrets: `d`, `y` and `x` each leave the table as it is and flash `this key is read-only` |
| `TestNothingIsLoadedInUpdate` | the session's method is called from a command: `Update` returns before `Called("Describe")` has an entry when the fake blocks (give the fake's method a way to block through `Errs` or a wrapper session in the test) |
| `TestScrolling` | 200 lines in a body of 20: the keys of 8 move as said; the position never leaves the content; after the content shrinks the position is back in |
| `TestSidewaysAndWrap` | a line of 300 characters: `→` scrolls sideways; `w` wraps it and sideways scrolling does nothing; no drawn line is wider than the body either way |
| `TestFullscreen` | `f`: the frame has no header and the view has the whole window; `f` again |
| `TestCopy` | `c` returns the clipboard command with the cleaned content, and flashes the number of lines |
| `TestSearch` | `/err`: matches are styled, the title shows `1/3`, `n` and `N` go round; `/[` is searched as the text; the empty text clears; `esc` clears first and pops second |
| `TestYAMLAutoRefresh` | `r`: the title shows `(auto)`; after the interval the session is asked again and a changed YAML is shown with the same scroll position; `r` again stops it; `Close` stops it |
| `TestYAMLManagedFields` | `m`: `YAML` is called with `true`, and with `false` after another `m` |
| `TestLaterLoadErrorKeepsTheText` | the reload fails: the text stays, the flash has the error |
| `TestTextIsCleaned` | a description, a YAML and a secret value that hold `\x1b]0;x\x07`, `\x1b[2J` and `\x00`: the drawn frame holds none of them, and the copied text neither |
| `TestYAMLColorKeepsTheText` | table of lines (a key and value, a list item with a key, a nested key, a comment, a quoted string with a colon, a line of a block scalar, an empty line, `---`): `ansi.Strip(yamlColor(l)) == l` for each, and the key part is styled where there is one |
| `TestBindingsAreRegistered` | `bindingsFor` of pods has `d` and `y` with their hints and orders; of secrets also `x` |

## Steps

- [ ] `tui/yamlcolor.go`: tests first. Commit: `TUI: YAML in colour, with its text unchanged`.
- [ ] The text view with scrolling, wrapping, full screen and copying, opened by test code: tests first. Commit: `TUI: a view for text`.
- [ ] Search. Commit: `TUI: searching in a text`.
- [ ] The bindings, the three kinds of content, reloading, the errors. Commit: `TUI: describe, YAML and a secret decoded`.
- [ ] `go test -race ./tui/...`; `gofmt -l . && go vet ./... && go build ./...` clean.

## Report

What you built and the test results; anything of Task 11 that you needed and did not find, or found different from `tui/view.go`; anything you were tempted to change in its files and why.
