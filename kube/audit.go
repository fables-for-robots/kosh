package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// logAction logs a call of an action that got past the grant, once the
// API has answered: the key, the action, the object, the action's own
// options in attrs, and the error when there is one.
func (s *session) logAction(ctx context.Context, action string, ref ObjectRef, err error, attrs ...slog.Attr) {
	all := make([]slog.Attr, 0, 5+len(attrs))
	all = append(all,
		slog.String("key", s.grant.Name),
		slog.String("fingerprint", s.grant.Fingerprint),
		slog.String("action", action),
		slog.String("object", ref.String()),
	)
	all = append(all, attrs...)
	if err != nil {
		all = append(all, slog.String("error", err.Error()))
	}
	s.c.log.LogAttrs(ctx, slog.LevelInfo, "action", all...)
}

// loggableError returns what kind of error err is, in words that hold
// nothing of its text, for an action whose error may quote what the user
// wrote or a Secret's contents: an edit refusal's reason; the API's
// reason, code and the paths of the fields it names; or "<action>
// failed". It returns nil for nil.
func loggableError(action string, err error) error {
	if err == nil {
		return nil
	}
	var refusal *editRefusal
	if errors.As(err, &refusal) {
		return errors.New(refusal.reason)
	}
	var api apierrors.APIStatus
	if errors.As(err, &api) {
		status := api.Status()
		reason := string(apierrors.ReasonForError(err))
		if reason == "" {
			reason = "an error"
		}
		text := fmt.Sprintf("the API answered %s (%d)", reason, status.Code)
		var fields []string
		seen := map[string]bool{}
		if status.Details != nil {
			for _, c := range status.Details.Causes {
				if c.Field != "" && !seen[c.Field] {
					seen[c.Field] = true
					fields = append(fields, c.Field)
				}
			}
		}
		if len(fields) > 0 {
			text += " at " + strings.Join(fields, ", ")
		}
		return errors.New(text)
	}
	return errors.New(action + " failed")
}
