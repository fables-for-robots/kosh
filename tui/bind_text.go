package tui

import (
	"context"
	"reflect"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
)

// The keys of the text views: d describes the row, y shows its YAML, x
// decodes a Secret. Each needs the right to get the row: describe reads
// the events too where it may, and shows what it can without them.
func init() {
	bind(
		binding{Key: "d", Hint: hint{"d", "Describe"}, Order: 10, Applies: textOfObjects,
			Needs: []need{{Verb: "get"}}, Run: textRun(textDescribe)},
		binding{Key: "y", Hint: hint{"y", "YAML"}, Order: 11, Applies: textOfObjects,
			Needs: []need{{Verb: "get"}}, Run: textRun(textYAML)},
		binding{Key: "x", Hint: hint{"x", "Decode"}, Order: 12, Applies: textOfSecrets,
			Needs: []need{{Verb: "get", Resource: needSecrets}}, Run: textRun(textDecode)},
	)
}

// textOfObjects reports whether the rows of res are objects of the API:
// not containers and not rules.
func textOfObjects(res kube.Resource, _ keys.Grant) bool {
	return res.GVR != kube.Containers.GVR && res.GVR != kube.Rules.GVR
}

// textOfSecrets reports whether res is the Secrets.
func textOfSecrets(res kube.Resource, _ keys.Grant) bool {
	return res.Is("", "secrets")
}

// textRun returns what a key of kind runs: it opens the text view for
// the first of the rows.
func textRun(kind textKind) func(e env, q kube.Query, refs []kube.ObjectRef) tea.Cmd {
	return func(e env, _ kube.Query, refs []kube.ObjectRef) tea.Cmd {
		if len(refs) == 0 {
			return nil
		}
		return textOpen(e, kind, refs[0])
	}
}

// textOpening holds the sessions that have a text view's first reading
// in flight: until it is back, d, y and x open nothing more.
//
// The root gives the command of a binding no way to know whether the
// table that asked is still on top when the reading returns, nor tells
// it when that table goes, so the reading cannot be cancelled when its
// view is no longer wanted. This keeps at least a second key from
// opening a second view.
var textOpening sync.Map

// textOpen reads what a text view of kind shows of ref, in a command,
// and pushes the view with it. When the reading fails no view is pushed
// and the flash shows the error. While another first reading of the
// session is in flight it does nothing.
func textOpen(e env, kind textKind, ref kube.ObjectRef) tea.Cmd {
	var key any
	if t := reflect.TypeOf(e.sess); t != nil && t.Comparable() {
		key = e.sess
		if _, busy := textOpening.LoadOrStore(key, struct{}{}); busy {
			return nil
		}
	}
	return func() tea.Msg {
		if key != nil {
			defer textOpening.Delete(key)
		}
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		defer cancel()
		text, err := textRead(ctx, e, kind, ref, false)
		if err != nil {
			return flashMsg{text: err.Error(), err: true}
		}
		return pushViewMsg{textNew(e, kind, ref, text)}
	}
}
