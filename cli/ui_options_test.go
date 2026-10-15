package cli

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/sshd"
	"github.com/fables-for-robots/kosh/tui"
)

// Every key's program is given the session's terminal to hand over: the
// role, through the API server, decides whether a shell opens.
func TestUIOptionsHandoverForEveryKey(t *testing.T) {
	for _, name := range []string{"alice/cluster-admin", "bob/view", "carol/edit@team-a"} {
		_, role, reason := keys.ParseRole(name)
		if reason != "" {
			t.Fatalf("%s: %s", name, reason)
		}
		calls := 0
		exec := func(run func(context.Context, sshd.Terminal) error, done func(error) tea.Msg) tea.Cmd {
			calls++
			return nil
		}
		opts := uiOptions(sshd.Session{Grant: keys.Grant{Name: name, Role: role}, Exec: exec})
		if opts.Handover == nil {
			t.Fatalf("%s: the program has no hand-over", name)
		}
		opts.Handover(func(context.Context, tui.Terminal) error { return nil }, func(error) tea.Msg { return nil })
		if calls != 1 {
			t.Errorf("%s: the hand-over called the session's Exec %d times", name, calls)
		}
		if opts.Version != Version {
			t.Errorf("%s: the version is not kosh's", name)
		}
	}
}
