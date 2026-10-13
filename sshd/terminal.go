package sshd

import (
	"context"
	"errors"

	tea "charm.land/bubbletea/v2"
)

// execNotBuilt is Session.Exec until the hand-over is built: done is told
// that it is not.
func execNotBuilt(run func(context.Context, Terminal) error, done func(error) tea.Msg) tea.Cmd {
	return func() tea.Msg { return done(errors.New("not implemented")) }
}
