package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
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
	actExec      = "exec"
)

// The words of the reads whose refusal by the API is logged, as the log
// names them.
const (
	readYAML       = "yaml"
	readDescribe   = "describe"
	readSecretData = "secret-data"
)

// errSessionEnded is what an action of a session that has ended gets,
// and why one that runs when its session ends is stopped.
var errSessionEnded = errors.New("the session has ended")

// begin is how every action begins. A session that has ended is refused
// with errSessionEnded, and one without clients with why it has none;
// either refusal is logged at warn level. Nothing else is refused here:
// the API server allows or refuses the action by the session's role, and
// logAction logs its answer. Otherwise begin returns the context the
// action runs with: ctx, ended also when the session ends, with
// errSessionEnded as its cause; and the function that releases it, which
// the action defers. After begin, s.clients is the session's.
func (s *session) begin(ctx context.Context, action string, ref ObjectRef) (context.Context, context.CancelFunc, error) {
	if s.ctx.Err() != nil {
		s.logRefusal(ctx, action, ref, slog.String("reason", errSessionEnded.Error()))
		return nil, nil, errSessionEnded
	}
	if s.clientsErr != nil {
		s.logRefusal(ctx, action, ref, slog.String("reason", "the session has no clients"))
		return nil, nil, s.clientsErr
	}
	actx, release := s.bound(ctx)
	return actx, release, nil
}

// logReadRefusal logs, at warn level as kosh's own refusals are, a read
// that the API server refused the session's role: a Secret's data, an
// object's YAML or its description. Any other error is the caller's to
// show and is not logged. The reason is the API's, in words that hold
// nothing of its text, as loggableError gives them.
func (s *session) logReadRefusal(ctx context.Context, read string, ref ObjectRef, err error) {
	if !apierrors.IsForbidden(err) && !apierrors.IsUnauthorized(err) {
		return
	}
	s.logRefusal(ctx, read, ref, slog.String("reason", loggableError(read, err).Error()))
}

// logRefusal logs at warn level that what was refused to the object:
// the key's name, fingerprint and role, the word of the action or read,
// the object, and attrs. Nothing of the object itself.
func (s *session) logRefusal(ctx context.Context, what string, ref ObjectRef, attrs ...slog.Attr) {
	s.c.log.LogAttrs(ctx, slog.LevelWarn, "refused", append([]slog.Attr{
		slog.String("key", s.grant.Name),
		slog.String("fingerprint", s.grant.Fingerprint),
		slog.String("role", s.grant.Role.String()),
		slog.String("action", what),
		slog.String("object", ref.String()),
	}, attrs...)...)
}

// logAction logs a call of an action that began, once the API has
// answered: the key and its role, the action, the object, the action's
// own options in attrs, and the error when there is one: a refusal of
// the API's among them, as "the API answered Forbidden (403)".
func (s *session) logAction(ctx context.Context, action string, ref ObjectRef, err error, attrs ...slog.Attr) {
	all := make([]slog.Attr, 0, 6+len(attrs))
	all = append(all,
		slog.String("key", s.grant.Name),
		slog.String("fingerprint", s.grant.Fingerprint),
		slog.String("role", s.grant.Role.String()),
		slog.String("action", action),
		slog.String("object", ref.String()),
	)
	all = append(all, attrs...)
	if err != nil {
		// Of an error of the API, only its reason and code: the rest may
		// be a webhook's text, which may quote the object. Of a refused
		// reference, only that it was: ref is the object attribute.
		var api apierrors.APIStatus
		if errors.Is(err, ErrUnknownResource) || errors.As(err, &api) {
			err = loggableError(action, err)
		}
		all = append(all, slog.String("error", err.Error()))
	}
	s.c.log.LogAttrs(ctx, slog.LevelInfo, "action", all...)
}

// loggableError returns what kind of error err is, in words that hold
// nothing of its text: an edit refusal's reason; or that the API
// answered, with its reason when it is one of metav1's and its code when
// it is an HTTP one; or "<action> failed". It returns nil for nil.
//
// Nothing else of an API error is logged, not even the paths of the
// fields it names: an admission webhook's denial reaches kosh with the
// webhook's own reason and causes, which may quote the object it was
// given, a Secret or an edit among them. The user sees the whole error.
func loggableError(action string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrUnknownResource) {
		return ErrUnknownResource
	}
	var refusal *editRefusal
	if errors.As(err, &refusal) {
		return errors.New(refusal.reason)
	}
	var api apierrors.APIStatus
	if !errors.As(err, &api) {
		return errors.New(action + " failed")
	}
	status := api.Status()
	reason := "unknown"
	if knownReasons[status.Reason] {
		reason = string(status.Reason)
	}
	text := "the API answered " + reason
	if status.Code >= 100 && status.Code <= 599 {
		text += fmt.Sprintf(" (%d)", status.Code)
	}
	return errors.New(text)
}

// knownReasons are the reasons of metav1. Any other reason is a
// webhook's text.
var knownReasons = map[metav1.StatusReason]bool{
	metav1.StatusReasonUnauthorized:          true,
	metav1.StatusReasonForbidden:             true,
	metav1.StatusReasonNotFound:              true,
	metav1.StatusReasonAlreadyExists:         true,
	metav1.StatusReasonConflict:              true,
	metav1.StatusReasonGone:                  true,
	metav1.StatusReasonInvalid:               true,
	metav1.StatusReasonServerTimeout:         true,
	metav1.StatusReasonStoreReadError:        true,
	metav1.StatusReasonTimeout:               true,
	metav1.StatusReasonTooManyRequests:       true,
	metav1.StatusReasonBadRequest:            true,
	metav1.StatusReasonMethodNotAllowed:      true,
	metav1.StatusReasonNotAcceptable:         true,
	metav1.StatusReasonRequestEntityTooLarge: true,
	metav1.StatusReasonUnsupportedMediaType:  true,
	metav1.StatusReasonInternalError:         true,
	metav1.StatusReasonExpired:               true,
	metav1.StatusReasonServiceUnavailable:    true,
}

// apiWarnings handles the warnings the API server sends. Their text may
// be an admission webhook's and quote an edited Secret, so nothing of it
// is logged: at most one debug line a second says that warnings came,
// with the code of the last and how many since the line before.
type apiWarnings struct {
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	last    time.Time // of the last line
	pending int       // warnings since then
}

// quietWarnings makes cfg's clients hand every warning to an apiWarnings
// that logs to log, instead of client-go's default, which writes the
// text to standard error. client-go uses WarningHandlerWithContext when
// it is set; WarningHandler is set too, for whatever uses only that.
func quietWarnings(cfg *rest.Config, log *slog.Logger) {
	w := &apiWarnings{log: log, now: time.Now}
	cfg.WarningHandler = w
	cfg.WarningHandlerWithContext = w
}

func (w *apiWarnings) HandleWarningHeader(code int, agent, text string) {
	w.HandleWarningHeaderWithContext(context.Background(), code, agent, text)
}

func (w *apiWarnings) HandleWarningHeaderWithContext(ctx context.Context, code int, _, _ string) {
	w.mu.Lock()
	now := w.now()
	w.pending++
	if !w.last.IsZero() && now.Sub(w.last) < time.Second {
		w.mu.Unlock()
		return
	}
	n := w.pending
	w.last, w.pending = now, 0
	w.mu.Unlock()
	w.log.LogAttrs(ctx, slog.LevelDebug, "the API sent a warning", slog.Int("code", code), slog.Int("warnings", n))
}
