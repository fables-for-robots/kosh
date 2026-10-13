package cli

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/sshd"
	"github.com/fables-for-robots/kosh/tui"
)

// A read-only key's program is given no terminal to hand over; a full
// key's is given the session's.
func TestUIOptionsHandoverOnlyForAFullKey(t *testing.T) {
	calls := 0
	exec := func(run func(context.Context, sshd.Terminal) error, done func(error) tea.Msg) tea.Cmd {
		calls++
		return nil
	}
	ro := uiOptions(sshd.Session{Grant: keys.Grant{Name: "bob", ReadOnly: true}, Exec: exec})
	if ro.Handover != nil {
		t.Error("a read-only key's program has a hand-over")
	}
	full := uiOptions(sshd.Session{Grant: keys.Grant{Name: "alice"}, Exec: exec})
	if full.Handover == nil {
		t.Fatal("a full key's program has no hand-over")
	}
	full.Handover(func(context.Context, tui.Terminal) error { return nil }, func(error) tea.Msg { return nil })
	if calls != 1 {
		t.Errorf("the hand-over called the session's Exec %d times", calls)
	}
	if ro.Version != Version || full.Version != Version {
		t.Error("the version is not kosh's")
	}
}
