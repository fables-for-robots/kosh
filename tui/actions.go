package tui

import (
	"context"
	"fmt"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/kube"
)

// Running an action over rows: one call of the session for each row,
// one after the other, in a command and never in Update; the outcome in
// the flash; the table read again. One action of a session runs at a
// time.

// textBusy is the flash of a key of an action while another runs.
const textBusy = "an action is still running"

// textNothingChanged is the flash of a dialog or the editor that had
// nothing to send.
const textNothingChanged = "nothing changed"

// actionWords are how an action is spoken of: ask in the question
// ("Kill pods/default/web-1?"), button on its button, done in the flash
// ("Killed pods/default/web-1").
type actionWords struct{ ask, button, done string }

// actionDo does an action to one object. note is added to the flash of
// a single row ("as job backup-28"); err is the session's error.
type actionDo func(ctx context.Context, ref kube.ObjectRef) (note string, err error)

// actionRunFunc is a binding's Run.
type actionRunFunc = func(e env, q kube.Query, refs []kube.ObjectRef) tea.Cmd

// actionsRunning holds the sessions that have an action running. A
// session is in it from the confirmation of an action until its last
// call has returned.
var actionsRunning = struct {
	sync.Mutex
	m map[kube.Session]bool
}{m: map[kube.Session]bool{}}

// actionBusy reports whether sess has an action running.
func actionBusy(sess kube.Session) bool {
	actionsRunning.Lock()
	defer actionsRunning.Unlock()
	return actionsRunning.m[sess]
}

// actionBegin marks sess as running an action, and reports false when it
// was already.
func actionBegin(sess kube.Session) bool {
	actionsRunning.Lock()
	defer actionsRunning.Unlock()
	if actionsRunning.m[sess] {
		return false
	}
	actionsRunning.m[sess] = true
	return true
}

// actionEnd marks sess as running no action.
func actionEnd(sess kube.Session) {
	actionsRunning.Lock()
	defer actionsRunning.Unlock()
	delete(actionsRunning.m, sess)
}

// actionGuard wraps the Run of every key of an action: while an action
// of the session runs, the key says so in the flash and does nothing.
func actionGuard(run actionRunFunc) actionRunFunc {
	return func(e env, q kube.Query, refs []kube.ObjectRef) tea.Cmd {
		if len(refs) == 0 {
			return nil
		}
		if actionBusy(e.sess) {
			return flash(textBusy)
		}
		return run(e, q, refs)
	}
}

// actionTarget names what an action is done to: the reference of one
// row, or "3 deployments".
func actionTarget(refs []kube.ObjectRef) string {
	if len(refs) == 1 {
		return refs[0].String()
	}
	return fmt.Sprintf("%d %s", len(refs), refs[0].Resource.GVR.Resource)
}

// actionNames returns the names a dialog lists for several rows: none
// for one row, whose reference the question holds.
func actionNames(refs []kube.ObjectRef) []string {
	if len(refs) < 2 {
		return nil
	}
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.Name
		if r.Namespace != "" {
			names[i] = r.Namespace + "/" + r.Name
		}
	}
	return names
}

// actionQuestion is the question of a dialog: "Kill pods/default/web-1?".
func actionQuestion(ask string, refs []kube.ObjectRef) string {
	return ask + " " + actionTarget(refs) + "?"
}

// actionAsk returns a command that opens the confirm dialog of an action
// over refs; its button starts the action, with a context of timeout for
// each call.
func actionAsk(e env, refs []kube.ObjectRef, w actionWords, timeout time.Duration, do actionDo) tea.Cmd {
	return openDialog(newConfirmDialog(actionQuestion(w.ask, refs), actionNames(refs), formButton{
		label: w.button,
		press: func(*formDialog) (tea.Cmd, string) {
			return actionStart(e, refs, w.done, timeout, do), ""
		},
	}))
}

// actionStart starts an action over refs: it marks the session as
// running one, and returns the command that calls do for each row, one
// after the other, each with a context of timeout (none when it is
// zero), and then says what came of it and has the table read again.
// When an action of the session is running already, it says so instead.
func actionStart(e env, refs []kube.ObjectRef, done string, timeout time.Duration, do actionDo) tea.Cmd {
	if !actionBegin(e.sess) {
		return flash(textBusy)
	}
	sess := e.sess
	return waitCmd(func() tea.Msg {
		defer actionEnd(sess)
		var (
			ok    int
			note  string
			first error
		)
		for _, ref := range refs {
			n, err := actionCall(timeout, func(ctx context.Context) (string, error) { return do(ctx, ref) })
			if err != nil {
				if first == nil {
					first = err
				}
				continue
			}
			ok++
			note = n
		}
		return tea.BatchMsg{actionOutcome(done, refs, ok, note, first), refresh()}
	})
}

// actionCall calls fn with a context of timeout, or without a deadline
// when timeout is zero.
func actionCall(timeout time.Duration, fn func(ctx context.Context) (string, error)) (string, error) {
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	return fn(ctx)
}

// actionOutcome is the flash of an action over refs, of which ok
// succeeded: "Killed pods/default/web-1", "Killed 3 pods", or "Killed
// 2 of 3 pods: <the first error>" as an error. The error of a single row
// is shown as the session worded it.
func actionOutcome(done string, refs []kube.ObjectRef, ok int, note string, first error) tea.Cmd {
	switch {
	case first == nil && len(refs) == 1:
		return flash(done + " " + refs[0].String() + note)
	case first == nil:
		return flash(done + " " + actionTarget(refs))
	case len(refs) == 1:
		return flashErr(first)
	}
	return flashErr(fmt.Errorf("%s %d of %s: %w", done, ok, actionTarget(refs), first))
}
