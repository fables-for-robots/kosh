package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
)

// The keys of the text views: d describes the row, y shows its YAML, x
// decodes a Secret. They apply to read-only keys too; the session
// refuses them a Secret's contents, and the flash says so.
func init() {
	bind(
		binding{Key: "d", Hint: hint{"d", "Describe"}, Order: 10, Applies: textOfObjects, Run: textRun(textDescribe)},
		binding{Key: "y", Hint: hint{"y", "YAML"}, Order: 11, Applies: textOfObjects, Run: textRun(textYAML)},
		binding{Key: "x", Hint: hint{"x", "Decode"}, Order: 12, Applies: textOfSecrets, Run: textRun(textDecode)},
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

// textOpen reads what a text view of kind shows of ref, in a command,
// and pushes the view with it. When the reading fails no view is pushed
// and the flash shows the error.
func textOpen(e env, kind textKind, ref kube.ObjectRef) tea.Cmd {
	return func() tea.Msg {
		text, err := textRead(e, kind, ref, false)
		if err != nil {
			return flashMsg{text: err.Error(), err: true}
		}
		return pushViewMsg{textNew(e, kind, ref, text)}
	}
}
