package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
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
// wrote or a Secret's contents: an edit refusal's reason; or that the
// API answered, with its reason when it is one of metav1's, its code when
// it is an HTTP one, and the paths of at most maxLoggedFields fields it
// names, with every [key] as [*]; or "<action> failed". It returns nil
// for nil.
//
// An admission webhook's denial reaches kosh with the webhook's own
// reason and causes, which may quote the object it was given: that is
// why nothing of them is logged that is not a known word or a path.
func loggableError(action string, err error) error {
	if err == nil {
		return nil
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
	var fields []string
	seen := map[string]bool{}
	if status.Details != nil {
		for _, c := range status.Details.Causes {
			if !fieldPath.MatchString(c.Field) {
				continue
			}
			f := fieldKey.ReplaceAllString(c.Field, "[*]")
			if !seen[f] && len(fields) < maxLoggedFields {
				seen[f] = true
				fields = append(fields, f)
			}
		}
	}
	if len(fields) > 0 {
		text += " at " + strings.Join(fields, ", ")
	}
	return errors.New(text)
}

// maxLoggedFields is how many fields of an API error loggableError names.
const maxLoggedFields = 8

var (
	// fieldPath is a field path as the API writes one:
	// spec.containers[0].image, data[password].
	fieldPath = regexp.MustCompile(`^[A-Za-z0-9_-]+(\[[^\]]*\])*(\.[A-Za-z0-9_-]+(\[[^\]]*\])*)*$`)
	// fieldKey is an index or a key of a field path.
	fieldKey = regexp.MustCompile(`\[[^\]]*\]`)
)

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
