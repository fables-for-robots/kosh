package kube

import (
	"context"
	"log/slog"
)

// The words of the actions, as the log names them.
const (
	actDelete    = "delete"
	actKill      = "kill"
	actUpdate    = "update"
	actScale     = "scale"
	actRestart   = "restart"
	actSetImages = "set-images"
	actRollback  = "rollback"
	actTrigger   = "trigger"
	actSuspend   = "suspend"
	actResume    = "resume"
	actCordon    = "cordon"
	actUncordon  = "uncordon"
	actDrain     = "drain"
)

// begin is how every action begins: a read-only grant gets ErrReadOnly,
// and its refusal is logged at warn level.
func (s *session) begin(ctx context.Context, action string, ref ObjectRef) error {
	if err := s.mayChange(); err != nil {
		s.c.log.LogAttrs(ctx, slog.LevelWarn, "refused",
			slog.String("key", s.grant.Name),
			slog.String("fingerprint", s.grant.Fingerprint),
			slog.String("action", action),
			slog.String("object", ref.String()),
		)
		return err
	}
	return nil
}
