# Task 11: tui, the frame, the table and getting around

The terminal UI's foundation: the screen, the stack of views, the command and filter prompt, and the table view with everything k9s's has. Tasks 12, 13 and 14 hang their views, dialogs and keys into what this task builds, side by side, through the hooks of `tui/view.go`. They are fixed: build them exactly as declared.

**Read first:** the spec, §2, §10 and §12 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints and Review Focus (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); the contract: `tui/tui.go` and `tui/view.go`; `kube/kube.go` for `Session`, `Query`, `Table`, `Watch`; `render/render.go` for `Column`, `Row`, `Health`; `kube/kubetest/kubetest.go`, the fake every test here uses.

**Prior art in the same stack** (Bubble Tea v2, Lip Gloss v2, golden frames, a harness that runs commands in tests): `/Users/dragan/fables-for-robots/great-machine/tui/` and its `tuitest/`. Read how it drives a model in tests; write kosh's own.

**The look** is k9s's stock skin: read the defaults in `/private/tmp/claude-502/-Users-dragan-fables-for-robots/25996946-215e-4a11-ba9e-574cacaa75eb/scratchpad/k9s-src/repo/internal/config/styles.go` and `internal/model1/color.go` for the colours of the frame, the title, the header, the keys, the table's header, the cursor, and of a row that is new, changed, an error, pending or completed. kosh paints foregrounds in those colours and leaves the terminal's background alone, except under the cursor and a marked row.

**Files:**
- Modify: `tui/tui.go` (the body of `New`)
- Create: `tui/root.go`, `tui/header.go`, `tui/prompt.go`, `tui/crumbs.go`, `tui/flash.go`, `tui/stack.go`, `tui/styles.go`, `tui/clean.go`, `tui/table.go`, `tui/tablekeys.go`, `tui/navigate.go`, `tui/help.go`, `tui/aliases.go`
- Create: `tui/harness_test.go` and a `_test.go` for each of the above that has behaviour; golden frames under `tui/testdata/`
- Do not change `tui/view.go`.

**Interfaces:**
- Consumes: `kube.Session` and everything of `kube/kube.go`; `render.Column`, `render.Row`, `render.Health`; `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2/textinput`, `github.com/charmbracelet/x/ansi` (widths, truncation, stripping), `k8s.io/apimachinery/pkg/labels` (the label filter). No `k8s.io/client-go`.
- Consumes from `tui/view.go`, as declared: `view`, `filterable`, `hint`, `dialog`, the messages `pushViewMsg`, `popViewMsg`, `flashMsg`, `openDialogMsg`, `closeDialogMsg`, `refreshMsg` and their command helpers, `env`, `apiTimeout`, `binding`, `bind`, `bindingsFor`.
- Produces: `New(sess kube.Session, opts Options) tea.Model`.
- Produces for Tasks 12, 13 and 14, unexported, with exactly these names:

  ```go
  // clean makes text from the cluster safe to draw: escape sequences
  // are removed, every other control character but the line feed and
  // the tab becomes a visible placeholder, a tab becomes spaces, and
  // bytes that are not UTF-8 become U+FFFD. It is idempotent.
  func clean(s string) string

  // The styles every part draws with (fields of a package-level value
  // or functions, as you prefer, but with these names):
  //   styleError, styleDim, styleTitle, styleKey, styleHint,
  //   styleCursor, styleMatch
  // each a lipgloss.Style.

  // fit cuts or pads s to exactly width cells, with "…" at the end of
  // a cut line; it counts cells as the terminal does and keeps the
  // styles in s intact.
  func fit(s string, width int) string

  // newTableView returns the table of a query, with the cursor on the
  // row of the name selected when that is not empty.
  func newTableView(e env, q kube.Query, selected string) view
  ```

## Behaviour

**The root (spec §10.1)**

1. `New` returns the root model. Its `Init` resolves `pods` and opens their table in the namespace `default`. When that fails the body shows the error and `:` still works.
2. The root draws, top to bottom: the header (6 lines; `ctrl+e` hides and shows it), the prompt (1 line, only while it is open), the body in a frame with the view's `Title` in its top line, the crumbs (1 line), the flash (1 line). The view is given the frame's inside. The screen is the alternate one.
3. A window smaller than 80 by 12 shows one line, `kosh needs a terminal of at least 80 by 12 (this one is WxH)`, cut to the window; a window of no size shows nothing. **No window size panics**, and at every size every line of the frame is at most the window's width and there are at most the window's height of them (Review Focus 2).
4. Keys go, in this order, to: `ctrl+c`, which always ends the session; the dialog, when one is open; the prompt, when it is open; the top view, when it is `Capturing`; the root's own keys (5); the top view.
5. The root's keys: `:` opens the command prompt; `/` opens the filter prompt when the top view is `filterable`; `?` opens the help; `ctrl+a` the names; `ctrl+e` the header; `[` `]` `-` the history (9).
6. **Every message that is not a key press goes to every view of the stack**, top first, and to the dialog. A view knows its own messages by an id it puts into them and ignores the rest. That is how a table under another view keeps its data, and why a view's command may finish after the view is covered.
7. `pushViewMsg` puts the view on the stack and runs its `Init`. `popViewMsg` takes the top view off, calls its `Close`, and gives the view below a `refreshMsg`; the last view stays. A view that is `Fullscreen` is drawn alone, on the whole window.
8. `flashMsg` shows its text, cleaned and cut to one line, in the flash line, in the error style when `err` is set, for 5 seconds or until the next. `openDialogMsg` shows a dialog centred over the body and runs its `Init`; `closeDialogMsg` removes it.
9. **The command prompt.** As the text is typed, the first name of `sess.Resources()` (plural names, then short names) that begins with it is suggested after the cursor in the dim style, and `tab` or `→` takes it. `enter` runs the text; `esc` closes the prompt. A command is a name and optionally a namespace. `q`, `q!`, `qa`, `Q` end the session; `alias` and `a` open the names; `h`, `help` and `?` the help. Anything else is given to `sess.Resolve`, in a command: on success the stack is replaced by the table of that resource (every view of the old stack is closed), in the namespace given or else the session's, and the command is added to the history; on failure the flash says `no such resource: <name>`. The history has no two equal entries in a row; `[` goes back in it, `]` forward, `-` to the one before the current.
10. **The filter prompt** gives its text to the top view's `SetFilter` at every change, `enter` closes the prompt and leaves the filter, `esc` closes it and sets the empty filter.
11. **The namespace.** The session has one current namespace, `default` at first; the empty one is all namespaces. It has up to ten shortcuts: `0` is all, `1` is `default`, `2` to `9` are the other namespaces this session used, most recent first. Using a namespace (by a command's second word, by `enter` on a namespace's row, by a shortcut) makes it the current one and moves it to the front of the others. The header shows the shortcuts while the top view is a table of a namespaced resource without `Of`.
12. **The header** shows, left: `Key: <name>` with ` [read-only]` after it for a read-only grant, `Cluster: <version>`, `kosh: <version>`; middle: the shortcuts; right: the top view's hints as `<key> text`, in columns of five, as many as fit. All of it cleaned.
13. **The crumbs** are the `Crumb` of every view of the stack, the top one in the title style.

**The table (spec §10.2, §9)**

14. `newTableView` opens `sess.Watch(q)` in its `Init`, in a command. An error there is shown in the body. `Close` closes the watch.
15. The table reads `watch.Table(now)` in a command, never in `Update`: once at the start, then whenever `Changed` fires, but not sooner than 500 ms after the last read, and at least every 2 seconds. The two intervals are fields that tests shorten.
16. The body shows the columns that are not `Wide`, or all of them after `ctrl+w`. For a namespaced table of one known namespace (the query names one, or `Of` has one) the NAMESPACE column is left out. Columns are as wide as their widest cell, the header included, with two spaces between; `Right` columns are aligned right. What does not fit the width is cut at the right edge. Cells are cleaned.
17. Rows are sorted by NAMESPACE and NAME at first, the events by LAST SEEN with the newest first. `shift+n` sorts by NAME, `shift+a` by AGE, `shift+s` by STATUS (each only when the table has that column), `shift+o` by the column the column cursor is in, which `←` and `→` (and `h`, `l`) move and the header marks; the same key again reverses the order. A `Numeric` column is sorted by `Row.Order`, any other by its text; ties are broken by namespace and name. The header shows which column and which way (`NAME↑`).
18. The filter (10) keeps the rows of which some cell matches the text as a regular expression, without regard to case; a text that is no regular expression is matched as it stands. `!text` keeps the rows that do not match. `-l selector` keeps the rows whose labels match the label selector (`labels.Parse`); a selector that does not parse keeps nothing and says so in the flash. `ctrl+z` shows only the rows whose health is `render.Error`, and again all.
19. The row cursor moves with `↑` `↓` `j` `k`, a page with `pgup` `pgdown` `ctrl+b` `ctrl+f`, to the ends with `g` `G` `home` `end`. Across a new reading of the table it stays on its object (namespace and name), and at its position when that object is gone.
20. `space` marks and unmarks the cursor's row; `ctrl+space` marks every row from the last one marked to the cursor; `ctrl+\` clears the marks. A mark belongs to an object and goes when the object does.
21. A row is coloured, the first that applies: the cursor; a mark; for 2 seconds after it appeared in a table that was already shown, the colour of a new row, and after its cells changed, that of a changed row; its health. The first reading of a view colours nothing as new.
22. The title is the resource's plural name with a capital, what it is of, the number of rows shown, the filter, and whether it is stale: `Pods(default)[12]`, `Pods(all)[40]`, `Pods(deployments/default/web)[3]`, `Pods(default)[2] </web>`, `Pods(default)[12] (stale)`.
23. A table with `Err` shows the error, cleaned, in the error style, in place of the rows. A table without rows shows `No resources found`, or `Waiting for the first list…` while it is stale and has never had rows.
24. `esc` clears the filter when there is one, and otherwise pops the view. `ctrl+r` reads the table again now.
25. `0` to `9` are the namespace shortcuts (11), in a table of a namespaced resource without `Of`. Switching closes the watch and opens the one of the new namespace.
26. **`enter`** goes into the row:

    | The table's resource | Opens |
    |---|---|
    | deployments, replicasets, statefulsets, daemonsets, jobs, services, nodes | `Query{Resource: pods, Of: the row}` |
    | cronjobs | `Query{Resource: jobs, Of: the row}` |
    | pods | `Query{Resource: kube.Containers, Of: the row}` |
    | roles, clusterroles, rolebindings, clusterrolebindings | `Query{Resource: kube.Rules, Of: the row}` |
    | namespaces | the row's namespace becomes the current one and the stack is replaced by the table of pods, as the command `pods <namespace>` would |
    | customresourcedefinitions | `sess.Resolve` of the row's name, then the table of that resource in the current namespace |
    | `kube.Containers` | the binding of the key `l` is run with the row, when there is one |
    | anything else | the binding of the key `d` is run with the row, when there is one |

    The resources `pods` and `jobs` are found with `sess.Resolve`. The reference of a row is `kube.ObjectRef{Resource: the table's, Namespace: row.Namespace, Name: row.Name}`; of a container's row it is the same with the pod's namespace, and the binding learns the pod from `q.Of`.
27. `shift+j` asks `sess.Owner` of the row and pushes the table of the owner's resource, in the owner's namespace, with the cursor on the owner. `kube.ErrNoOwner` is a flash, not an error style.
28. **The bindings.** A key press that the table does not use itself is looked up in `bindingsFor(q.Resource, sess.Grant())`; its `Run` is called with the references of the marked rows that are shown, or of the cursor's row when none is marked, and what it returns is the table's command. A table without rows runs nothing. The table's own keys win over a binding of the same key, and a test fails when a binding is registered for one of them.
29. `Hints` are the table's own keys that apply (sort, wide, faults, filter, mark, owner, help) followed by the hints of its bindings.

**Help and names**

30. `?` pushes a view that lists every hint of the view it was opened from, in columns, and the root's keys; `esc`, `q` and `?` pop it.
31. `ctrl+a` (and the commands `alias`, `a`) pushes a view that lists `sess.Resources()`: the plural name, the short names, the kind, the group, whether namespaced, sorted by name. It is filterable and moves like a table; `enter` goes to the resource as the command would.

**Clean (Review Focus 1)**

32. `clean` is used on every string that came from the session before it is measured or drawn. After it, a string holds no ESC (0x1b), no other C0 control character but `\n`, no DEL and no C1 control character.

## Tests

`tui/harness_test.go` is the harness: it holds a model, feeds it messages, runs the commands that come back and feeds their messages in turn, until nothing is pending; a command that blocks (the wait for `Changed`) is left running and its message is taken when it comes, with a deadline. It gives the frame as plain text (`ansi.Strip`) and as drawn. Golden frames live in `tui/testdata/<test>.golden` as plain text, at 100 by 30 unless the test is about size, and are rewritten with `go test ./tui/ -update`. The clock is `Options.Now`, fixed at `2026-10-13T12:00:00Z` and moved by the test. Tables come from `kubetest.Session.SetTable`; helper functions build `kube.Table` values with a few rows.

| Test | What it shows |
|---|---|
| `TestStartsInThePodsOfDefault` | golden: header with the key's name, the frame titled `Pods(default)[3]`, the rows, crumbs `<pods>` |
| `TestReadOnlyIsInTheHeader` | ` [read-only]` after the name for a read-only grant, and not for a full one |
| `TestTooSmall` | 79x12 and 80x11 show the one line with their size; 0x0, 1x1 and 5x2 do not panic and draw nothing wider or higher than the window; 80x12 draws the frame |
| `TestEveryLineFits` | at 80x12, 100x30 and 250x70, with a table of 5000 rows and cells of 300 characters: at most height lines, none wider than the width (`ansi.StringWidth`) |
| `TestCommandGoesToAResource` | `:deploy` `enter`: title `Deployments(default)[…]`, the pods' watch is closed (`OpenWatches`), crumbs `<deployments>` |
| `TestCommandWithNamespace` | `:po kube-system`: title `Pods(kube-system)[…]`; `kube-system` is shortcut `2` in the header |
| `TestCommandUnknown` | `:nothing`: the flash says `no such resource: nothing`, the view is unchanged |
| `TestCommandSuggests` | typing `dep` shows `deployments` suggested; `tab` completes it |
| `TestCommandQuits` | `:q`, `:q!`, `:qa`, `:Q` and `ctrl+c` each yield `tea.Quit` |
| `TestHistory` | pods, deployments, services: `[` goes to deployments, `[` to pods, `]` to deployments, `-` back to pods |
| `TestFlashExpires` | a flash is shown, and gone after 5 seconds of the test's clock |
| `TestHeaderToggles` | `ctrl+e` gives the body six more lines, and again takes them back |
| `TestFilter` | table: `/web` keeps the rows with web in a cell; `/WEB` the same; `/!web` the others; `/-l app=web` those with the label; `/[` is matched as the text `[`; `/-l ===` keeps nothing and flashes; the title shows the filter |
| `TestEscClearsTheFilterThenPops` | with a filter, `esc` clears it and the view stays; again, the view is popped |
| `TestSort` | default order; `shift+n` twice reverses; `shift+a` sorts by `Order` of AGE, not by its text (`9d` before `10d` only by number); `shift+o` after `→` sorts by the second column; the header marks the column |
| `TestEventsStartNewestFirst` | a table of events opens sorted by LAST SEEN |
| `TestWideAndFaults` | `ctrl+w` shows the wide columns; `ctrl+z` only the error rows, and the title's count follows |
| `TestNamespaceColumnIsLeftOut` | `Pods(default)` has no NAMESPACE column; `Pods(all)` has it; a table of nodes is unchanged |
| `TestCursorStaysOnItsObject` | the cursor is on `web-2`; a new table with a row sorted in before it: the cursor is still on `web-2`. `web-2` gone: the cursor is at the same position |
| `TestMarks` | `space` marks, `ctrl+space` marks the range, `ctrl+\` clears; a marked row that disappears from the table is no longer marked |
| `TestNewAndChangedRowsAreColoured` | the first table colours nothing; a row added by the second is in the new colour, one whose cell changed in the changed colour; after 2 seconds of the clock both are back to their health's |
| `TestHealthColours` | rows of each health are drawn in their styles (compare the drawn frame, not the plain one) |
| `TestTableErrorAndEmpty` | `Err`: its text in the body. No rows: `No resources found`. Stale and never any rows: `Waiting for the first list…`. Stale with rows: `(stale)` in the title |
| `TestNamespaceShortcuts` | `0` opens `Pods(all)`; `1` `Pods(default)`; after `:po kube-system` and `:po monitoring`, `2` is monitoring and `3` kube-system; the old watch is closed each time |
| `TestEnter` | table-driven over every line of the table in 26: the `Query` of the watch that is opened (`Called("Watch")`), or the binding that is run |
| `TestEnterOnANamespace` | the current namespace changes and the stack is the pods' table alone |
| `TestEnterOnACRD` | `Resolve` is called with the row's name and the widgets' table opens |
| `TestOwnerJump` | `shift+j`: the owner's table opens with the cursor on the owner; `ErrNoOwner` flashes |
| `TestEscPopsAndClosesTheWatch` | after `enter` and `esc` the pods-of watch is closed and the first table is shown and reads again |
| `TestBindings` | a binding registered by the test (restore `bindings` afterwards): its hint is in the header and in the help; its key runs it with the cursor's row; with two rows marked, with both; not at all in an empty table; one whose `Applies` is false for a read-only grant is neither shown nor run |
| `TestTableKeysWinOverBindings` | registering a binding for a key the table uses fails the check that guards it |
| `TestCoveredViewsStillGetTheirMessages` | a table under another view takes a new table from its watch |
| `TestDialogTakesTheKeys` | a dialog of the test gets every key but `ctrl+c`, the view none; `closeDialogMsg` removes it |
| `TestCapturingViewTakesTheKeys` | a view of the test with `Capturing` true gets `:` and `?` |
| `TestFullscreenViewIsDrawnAlone` | a fullscreen view of the test is given the whole window and no header is drawn |
| `TestHelp` | golden: the hints of the table and the root's keys; `esc` returns |
| `TestNames` | golden of `ctrl+a`; `/deploy` filters; `enter` goes to the resource |
| `TestClean` | table: an SGR sequence, an OSC title sequence ending in BEL and one ending in ST, a CSI cursor move, `\x00`, `\r`, DEL, a C1 byte, invalid UTF-8, a tab; each result has none of the forbidden characters; `clean(clean(s)) == clean(s)` |
| `TestCellsFromTheClusterAreCleaned` | a row whose cell and a table whose `Err` hold `\x1b]0;x\x07` and `\x1b[2J`: the drawn frame holds neither sequence |

## Steps

- [ ] `tui/clean.go` and `fit`, tests first. Commit: `TUI: text from the cluster is made safe before it is drawn`.
- [ ] The harness, `tui/styles.go`, the root with header, crumbs, flash, stack, dialog and fullscreen handling, with test views and a test dialog; tests first. Commit: `TUI: the frame, the stack of views, the flash and the dialog`.
- [ ] The prompt: commands, suggestions, history, quitting; the filter prompt. Commit: `TUI: the command prompt and its history`.
- [ ] The table: reading, drawing, sorting, filtering, cursor, marks, colours, title. Commit: `TUI: the table`.
- [ ] Namespaces, `enter`, the owner, the bindings. Commit: `TUI: namespaces, going into a row, and the keys other parts add`.
- [ ] Help and names. Commit: `TUI: the help and the list of names`.
- [ ] `go test -race ./tui/...`; `gofmt -l . && go vet ./... && go build ./...` clean.

## Report

What you built and the test results; the exact names and signatures of what Tasks 12 to 14 will use (`clean`, `fit`, the styles, `newTableView`, how a test opens a view and reads the frame with your harness), where they differ in any detail from the list above, and why; anything in `tui/view.go` that did not fit.
