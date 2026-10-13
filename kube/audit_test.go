package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

// auditRecord is one record of an auditLog, its attributes resolved.
type auditRecord struct {
	Level   slog.Level
	Message string
	Attrs   map[string]slog.Value
}

// attr returns the attribute key as a string, and whether it is there.
func (r auditRecord) attr(key string) (string, bool) {
	v, ok := r.Attrs[key]
	if !ok {
		return "", false
	}
	return v.String(), true
}

// auditLog is a slog.Handler that keeps every record.
type auditLog struct {
	mu      *sync.Mutex
	records *[]auditRecord
	attrs   []slog.Attr
}

func newAuditLog() *auditLog {
	return &auditLog{mu: &sync.Mutex{}, records: &[]auditRecord{}}
}

// logger returns a logger that writes to h.
func (h *auditLog) logger() *slog.Logger { return slog.New(h) }

func (h *auditLog) Enabled(context.Context, slog.Level) bool { return true }

func (h *auditLog) Handle(_ context.Context, r slog.Record) error {
	rec := auditRecord{Level: r.Level, Message: r.Message, Attrs: map[string]slog.Value{}}
	for _, a := range h.attrs {
		rec.Attrs[a.Key] = a.Value.Resolve()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.Resolve()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, rec)
	return nil
}

func (h *auditLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &auditLog{mu: h.mu, records: h.records, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h *auditLog) WithGroup(string) slog.Handler { return h }

// all returns every record so far.
func (h *auditLog) all() []auditRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]auditRecord(nil), *h.records...)
}

// with returns the records whose message is msg.
func (h *auditLog) with(msg string) []auditRecord {
	var out []auditRecord
	for _, r := range h.all() {
		if r.Message == msg {
			out = append(out, r)
		}
	}
	return out
}

// reset forgets every record so far.
func (h *auditLog) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = nil
}

// auditOneAction returns the one record of an action, and fails the test
// when there is not exactly one, or when it is not at info level, or
// does not name the key.
func auditOneAction(t *testing.T, f *actionFixture) auditRecord {
	t.Helper()
	recs := f.log.with("action")
	if len(recs) != 1 {
		t.Fatalf("action records %+v, want one", recs)
	}
	if r := f.log.with("refused"); len(r) != 0 {
		t.Errorf("refusals %+v", r)
	}
	r := recs[0]
	if r.Level != slog.LevelInfo {
		t.Errorf("level %v, want info", r.Level)
	}
	for k, v := range map[string]string{"key": fullGrant.Name, "fingerprint": fullGrant.Fingerprint} {
		if got, _ := r.attr(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	return r
}

func TestEveryActionIsLoggedOnce(t *testing.T) {
	ctx := context.Background()
	before := func(e *env) { actionScaleSubresource(e); actionAcceptPatches(e) }
	drainOpts := DefaultDrainOptions()
	drainOpts.IgnoreDaemonSets = true
	drainOpts.DeleteEmptyDirData = true
	tests := []struct {
		name string
		objs func() []runtime.Object
		// call calls the action and returns the attributes it expects
		// besides key and fingerprint.
		call func(t *testing.T, f *actionFixture) map[string]string
	}{
		{"delete", nil, func(t *testing.T, f *actionFixture) map[string]string {
			ref := f.ref(t, "apps", "deployments", "default", "web")
			if err := f.s.Delete(ctx, ref, DeleteOptions{Propagation: PropagationForeground, Force: true}); err != nil {
				t.Fatal(err)
			}
			return map[string]string{"action": "delete", "object": "deployments/default/web", "propagation": "Foreground", "force": "true"}
		}},
		{"delete with defaults", nil, func(t *testing.T, f *actionFixture) map[string]string {
			if err := f.s.Delete(ctx, f.ref(t, "apps", "deployments", "default", "web"), DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			return map[string]string{"action": "delete", "object": "deployments/default/web", "propagation": "Background", "force": "false"}
		}},
		{"kill", nil, func(t *testing.T, f *actionFixture) map[string]string {
			if err := f.s.Kill(ctx, f.ref(t, "", "pods", "default", "web-1")); err != nil {
				t.Fatal(err)
			}
			return map[string]string{"action": "kill", "object": "pods/default/web-1"}
		}},
		{"update", nil, func(t *testing.T, f *actionFixture) map[string]string {
			ref := f.ref(t, "apps", "deployments", "default", "web")
			if err := f.s.Update(ctx, ref, actionYAML(t, f, ref, nil)); err != nil {
				t.Fatal(err)
			}
			return map[string]string{"action": "update", "object": "deployments/default/web"}
		}},
		{"scale", nil, func(t *testing.T, f *actionFixture) map[string]string {
			if err := f.s.Scale(ctx, f.ref(t, "apps", "deployments", "default", "web"), 3); err != nil {
				t.Fatal(err)
			}
			return map[string]string{"action": "scale", "object": "deployments/default/web", "replicas": "3"}
		}},
		{"restart", nil, func(t *testing.T, f *actionFixture) map[string]string {
			if err := f.s.Restart(ctx, f.ref(t, "apps", "deployments", "default", "web")); err != nil {
				t.Fatal(err)
			}
			return map[string]string{"action": "restart", "object": "deployments/default/web"}
		}},
		{"set images", nil, func(t *testing.T, f *actionFixture) map[string]string {
			if err := f.s.SetImages(ctx, f.ref(t, "apps", "deployments", "default", "web"), map[string]string{"side": "side:2", "app": "app:2"}); err != nil {
				t.Fatal(err)
			}
			return map[string]string{"action": "set-images", "object": "deployments/default/web", "images": "[app=app:2 side=side:2]"}
		}},
		{"rollback", func() []runtime.Object {
			d, rs1, rs2 := actionRollbackObjects()
			return []runtime.Object{d, rs1, rs2}
		}, func(t *testing.T, f *actionFixture) map[string]string {
			if err := f.s.Rollback(ctx, f.ref(t, "apps", "replicasets", "default", "web-1")); err != nil {
				t.Fatal(err)
			}
			return map[string]string{"action": "rollback", "object": "replicasets/default/web-1"}
		}},
		{"trigger", nil, func(t *testing.T, f *actionFixture) map[string]string {
			job, err := f.s.Trigger(ctx, f.ref(t, "batch", "cronjobs", "default", "nightly"))
			if err != nil {
				t.Fatal(err)
			}
			return map[string]string{"action": "trigger", "object": "cronjobs/default/nightly", "job": job}
		}},
		{"suspend", nil, func(t *testing.T, f *actionFixture) map[string]string {
			if err := f.s.SetSuspended(ctx, f.ref(t, "batch", "cronjobs", "default", "nightly"), true); err != nil {
				t.Fatal(err)
			}
			return map[string]string{"action": "suspend", "object": "cronjobs/default/nightly"}
		}},
		{"resume", nil, func(t *testing.T, f *actionFixture) map[string]string {
			if err := f.s.SetSuspended(ctx, f.ref(t, "batch", "cronjobs", "default", "nightly"), false); err != nil {
				t.Fatal(err)
			}
			return map[string]string{"action": "resume", "object": "cronjobs/default/nightly"}
		}},
		{"cordon", nil, func(t *testing.T, f *actionFixture) map[string]string {
			if err := f.s.Cordon(ctx, f.ref(t, "", "nodes", "", "n1"), true); err != nil {
				t.Fatal(err)
			}
			return map[string]string{"action": "cordon", "object": "nodes/n1"}
		}},
		{"uncordon", nil, func(t *testing.T, f *actionFixture) map[string]string {
			if err := f.s.Cordon(ctx, f.ref(t, "", "nodes", "", "n1"), false); err != nil {
				t.Fatal(err)
			}
			return map[string]string{"action": "uncordon", "object": "nodes/n1"}
		}},
		{"drain", actionDrainObjects, func(t *testing.T, f *actionFixture) map[string]string {
			if err := f.s.Drain(ctx, f.ref(t, "", "nodes", "", "n1"), drainOpts); err != nil {
				t.Fatal(err)
			}
			return map[string]string{
				"action": "drain", "object": "nodes/n1",
				"gracePeriod": "-1", "timeout": "5s", "ignoreDaemonSets": "true", "deleteEmptyDirData": "true",
				"force": "false", "disableEviction": "false",
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []runtime.Object{
				actionDeployment("default", "web", "app", "side"), pod("default", "web-1", nil),
				actionCronJob("default", "nightly"), actionNode("n1"),
			}
			if tt.objs != nil {
				objs = tt.objs()
			}
			f := actionEnv(t, fullGrant, before, objs...)
			want := tt.call(t, f)
			r := auditOneAction(t, f)
			for k, v := range want {
				if got, ok := r.attr(k); !ok || got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			if e, ok := r.attr("error"); ok {
				t.Errorf("error %q", e)
			}
		})
	}

	t.Run("what the drain printed", func(t *testing.T) {
		f := actionEnv(t, fullGrant, nil, actionDrainObjects()...)
		if err := f.s.Drain(ctx, f.ref(t, "", "nodes", "", "n1"), drainOpts); err != nil {
			t.Fatal(err)
		}
		out, _ := auditOneAction(t, f).attr("output")
		if !strings.Contains(out, "ignoring DaemonSet-managed Pods: default/agent-x") {
			t.Errorf("output %q, want the helper's warning", out)
		}
	})
}

func TestFailedActionIsLoggedWithItsError(t *testing.T) {
	ctx := context.Background()
	failing := func(e *env) {
		e.dyn.PrependReactor("delete", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", errors.New("no"))
		})
		e.typed.PrependReactor("patch", "cronjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInvalid(schema.GroupKind{Group: "batch", Kind: "CronJob"}, "nightly", field.ErrorList{
				field.Invalid(field.NewPath("spec", "jobTemplate", "spec", "template", "spec", "containers").Index(0).Child("env").Index(0).Child("value"), auditMarker, "bad"),
			})
		})
		e.typed.PrependReactor("patch", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.(k8stesting.PatchAction).GetName() != "n2" {
				return false, nil, nil
			}
			return true, nil, auditWebhookStatus(400, "denied "+auditMarker, metav1.StatusCause{Field: "spec.hunter2", Message: auditMarker})
		})
	}
	tests := []struct {
		name string
		call func(t *testing.T, f *actionFixture) error
		word string
		// logged is the error the record holds; empty: the error's text.
		logged string
	}{
		{"the API refuses", func(t *testing.T, f *actionFixture) error {
			return f.s.Delete(ctx, f.ref(t, "apps", "deployments", "default", "web"), DeleteOptions{})
		}, "delete", "the API answered Forbidden (403)"},
		{"the object is not there", func(t *testing.T, f *actionFixture) error {
			return f.s.Kill(ctx, f.ref(t, "", "pods", "default", "gone"))
		}, "kill", "the API answered NotFound (404)"},
		{"the API finds it invalid", func(t *testing.T, f *actionFixture) error {
			return f.s.SetSuspended(ctx, f.ref(t, "batch", "cronjobs", "default", "nightly"), true)
		}, "suspend", "the API answered Invalid (422)"},
		{"a webhook denies a cordon", func(t *testing.T, f *actionFixture) error {
			return f.s.Cordon(ctx, f.ref(t, "", "nodes", "", "n2"), true)
		}, "cordon", "the API answered unknown (400)"},
		{"the kind", func(t *testing.T, f *actionFixture) error {
			return f.s.Scale(ctx, f.ref(t, "", "pods", "default", "web-1"), 2)
		}, "scale", ""},
		{"the arguments", func(t *testing.T, f *actionFixture) error {
			return f.s.Scale(ctx, f.ref(t, "apps", "deployments", "default", "web"), -2)
		}, "scale", ""},
		{"the YAML", func(t *testing.T, f *actionFixture) error {
			return f.s.Update(ctx, f.ref(t, "apps", "deployments", "default", "web"), "kind: [")
		}, "update", "the YAML does not parse"},
		{"a container", func(t *testing.T, f *actionFixture) error {
			return f.s.SetImages(ctx, f.ref(t, "apps", "deployments", "default", "web"), map[string]string{"nope": "x"})
		}, "set-images", ""},
		{"the drain", func(t *testing.T, f *actionFixture) error {
			return f.s.Drain(ctx, f.ref(t, "", "nodes", "", "n1"), DefaultDrainOptions())
		}, "drain", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append(actionDrainObjects(), actionDeployment("default", "web", "app"), pod("default", "web-1", nil), actionCronJob("default", "nightly"), actionNode("n2"))
			f := actionEnv(t, fullGrant, failing, objs...)
			err := tt.call(t, f)
			if err == nil {
				t.Fatal("no error")
			}
			r := auditOneAction(t, f)
			if got, _ := r.attr("action"); got != tt.word {
				t.Errorf("action %q, want %q", got, tt.word)
			}
			want := tt.logged
			if want == "" {
				want = err.Error()
			}
			if got, ok := r.attr("error"); !ok || got != want {
				t.Errorf("error %q, want %q", got, want)
			}
			auditNoMarker(t, f)
		})
	}
}

// auditMarker stands for a Secret's contents or text of an edit.
const auditMarker = "zebra-7d1f-marker"

// auditNoMarker fails the test when a record's message or attributes
// hold auditMarker.
func auditNoMarker(t *testing.T, f *actionFixture) {
	t.Helper()
	for _, r := range f.log.all() {
		if strings.Contains(r.Message, auditMarker) {
			t.Errorf("message %q holds the marker", r.Message)
		}
		for k, v := range r.Attrs {
			if strings.Contains(v.String(), auditMarker) {
				t.Errorf("%s = %q holds the marker", k, v.String())
			}
		}
	}
}

func TestUpdateLogHoldsNoYAML(t *testing.T) {
	updateFails := func(err error) func(*env) {
		return func(e *env) {
			e.dyn.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, err
			})
		}
	}
	edited := func(edit func(map[string]any)) func(*testing.T, *actionFixture, ObjectRef) string {
		return func(t *testing.T, f *actionFixture, ref ObjectRef) string { return actionYAML(t, f, ref, edit) }
	}
	plain := func(text string) func(*testing.T, *actionFixture, ObjectRef) string {
		return func(*testing.T, *actionFixture, ObjectRef) string { return text }
	}
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: "apps", Kind: "Deployment"}, "web", field.ErrorList{
		field.Invalid(field.NewPath("spec", "replicas"), auditMarker, "must be a number"),
		field.Invalid(field.NewPath("metadata", "labels"), auditMarker, "is not a label"),
	})
	tests := []struct {
		name   string
		before func(*env)
		text   func(*testing.T, *actionFixture, ObjectRef) string
		// inErr: the parser or the API put the marker into the error the
		// caller gets.
		inErr  bool
		logged string // the record's error; empty: none
	}{
		{"a label", nil, edited(func(m map[string]any) { _ = unstructured.SetNestedField(m, auditMarker, "metadata", "labels", "mark") }), false, ""},
		{"the name", nil, edited(func(m map[string]any) { _ = unstructured.SetNestedField(m, auditMarker, "metadata", "name") }), false, "the YAML names another object"},
		{"the namespace", nil, edited(func(m map[string]any) { _ = unstructured.SetNestedField(m, auditMarker, "metadata", "namespace") }), false, "the YAML names another object"},
		{"the kind", nil, edited(func(m map[string]any) { m["kind"] = auditMarker }), false, "the YAML names another object"},
		{"the apiVersion", nil, edited(func(m map[string]any) { m["apiVersion"] = auditMarker }), false, "the YAML names another object"},
		{"the value of a null key", nil, plain("data:\n  ~: " + auditMarker + "\n"), true, "the YAML does not parse"},
		{"a map under a top-level null key", nil, plain("~:\n  password: " + auditMarker + "\nkind: Secret\n"), true, "the YAML does not parse"},
		{"an !!int scalar", nil, plain("spec:\n  replicas: !!int " + auditMarker + "\n"), true, "the YAML does not parse"},
		{"an anchor's name", nil, plain("spec: *" + auditMarker + "\n"), true, "the YAML does not parse"},
		{"the API finds it invalid", updateFails(invalid), edited(nil), true, "the API answered Invalid (422)"},
		{"a plain error", updateFails(errors.New("denied: " + auditMarker)), edited(nil), true, "update failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := actionEnv(t, fullGrant, tt.before, actionDeployment("default", "web", "app"))
			ref := f.ref(t, "apps", "deployments", "default", "web")
			err := f.s.Update(context.Background(), ref, tt.text(t, f, ref))
			if tt.inErr && (err == nil || !strings.Contains(err.Error(), auditMarker)) {
				t.Errorf("the caller's error %v does not hold what the parser or the API said", err)
			}
			r := auditOneAction(t, f)
			if got, _ := r.attr("error"); got != tt.logged {
				t.Errorf("logged error %q, want %q", got, tt.logged)
			}
			auditNoMarker(t, f)
		})
	}
}

func TestDeleteOfASecretLogsNoContents(t *testing.T) {
	secret := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "creds"},
		Data:       map[string][]byte{"password": []byte(auditMarker)},
	}
	for _, tt := range []struct {
		name   string
		err    error
		logged string
	}{
		{"a webhook's denial", errors.New("admission webhook denied: password is " + auditMarker), "delete failed"},
		{"the API's error", apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "creds", errors.New("the password "+auditMarker+" is in use")), "the API answered Forbidden (403)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := actionEnv(t, fullGrant, func(e *env) {
				e.dyn.PrependReactor("delete", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.err
				})
			}, secret)
			err := f.s.Delete(context.Background(), f.ref(t, "", "secrets", "default", "creds"), DeleteOptions{})
			if err == nil || !strings.Contains(err.Error(), auditMarker) {
				t.Errorf("the caller's error %v is not the API's", err)
			}
			if got, _ := auditOneAction(t, f).attr("error"); got != tt.logged {
				t.Errorf("logged error %q, want %q", got, tt.logged)
			}
			auditNoMarker(t, f)
		})
	}
}

// auditWebhookStatus is an error as the API server passes on a webhook's
// denial: its reason and its causes' fields are the webhook's own text.
func auditWebhookStatus(code int32, reason string, causes ...metav1.StatusCause) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    code,
		Reason:  metav1.StatusReason(reason),
		Message: "admission webhook denied the request: " + auditMarker,
		Details: &metav1.StatusDetails{Causes: causes},
	}}
}

func TestLoggableError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"a plain error", errors.New(auditMarker), "update failed"},
		{"an edit refusal", &editRefusal{"the YAML does not parse", auditMarker}, "the YAML does not parse"},
		{"a validation error: its reason and code, not its fields", apierrors.NewInvalid(schema.GroupKind{Kind: "Secret"}, "creds", field.ErrorList{
			field.Invalid(field.NewPath("data").Key("password"), auditMarker, "bad"),
			field.Invalid(field.NewPath("spec", "containers").Index(0).Child("image"), auditMarker, "bad"),
		}), "the API answered Invalid (422)"},
		{"a webhook's reason", auditWebhookStatus(400, "password "+auditMarker+" not allowed"), "the API answered unknown (400)"},
		{"a webhook's reason that looks like a word", auditWebhookStatus(400, auditMarker), "the API answered unknown (400)"},
		{"a webhook's fields", auditWebhookStatus(422, "Invalid",
			metav1.StatusCause{Field: "data[" + auditMarker + "]", Message: auditMarker},
			metav1.StatusCause{Field: "data." + auditMarker, Message: auditMarker},
			metav1.StatusCause{Field: "the password is " + auditMarker, Message: "x"},
		), "the API answered Invalid (422)"},
		{"a code out of range", auditWebhookStatus(7, "Forbidden"), "the API answered Forbidden"},
		{"another code out of range", auditWebhookStatus(600, "Forbidden"), "the API answered Forbidden"},
		{"no reason", auditWebhookStatus(500, ""), "the API answered unknown (500)"},
		{"an API error inside another", fmt.Errorf("cordon error: %w", apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, auditMarker)), "the API answered NotFound (404)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := loggableError(actUpdate, tt.err)
			text := ""
			if got != nil {
				text = got.Error()
			}
			if text != tt.want {
				t.Errorf("%q, want %q", text, tt.want)
			}
			if strings.Contains(text, auditMarker) {
				t.Errorf("%q holds the marker", text)
			}
		})
	}
}

func TestWebhookDenialOfASecretIsNotLogged(t *testing.T) {
	denial := auditWebhookStatus(400, "password "+auditMarker+" not allowed",
		metav1.StatusCause{Field: "data[" + auditMarker + "]", Message: "the value " + auditMarker + " is weak"},
		metav1.StatusCause{Field: "password " + auditMarker, Message: auditMarker},
		metav1.StatusCause{Field: "data." + auditMarker, Message: auditMarker},
	)
	secret := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "creds", ResourceVersion: "5"},
		Data:       map[string][]byte{"password": []byte(auditMarker)},
	}
	for _, verb := range []string{"update", "delete"} {
		t.Run(verb, func(t *testing.T) {
			f := actionEnv(t, fullGrant, func(e *env) {
				e.dyn.PrependReactor(verb, "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, denial
				})
			}, secret)
			ref := f.ref(t, "", "secrets", "default", "creds")
			var err error
			if verb == "update" {
				err = f.s.Update(context.Background(), ref, actionYAML(t, f, ref, nil))
			} else {
				err = f.s.Delete(context.Background(), ref, DeleteOptions{})
			}
			if err == nil || !strings.Contains(err.Error(), auditMarker) {
				t.Errorf("the caller's error %v is not the API's", err)
			}
			if got, _ := auditOneAction(t, f).attr("error"); got != "the API answered unknown (400)" {
				t.Errorf("logged error %q", got)
			}
			auditNoMarker(t, f)
		})
	}
}

// auditStderr runs fn with standard error going to a pipe, and returns
// what was written there.
func auditStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	was := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = was
	w.Close()
	b, _ := io.ReadAll(r)
	r.Close()
	return string(b)
}

func TestAPIWarningsAreNotLogged(t *testing.T) {
	log := newAuditLog()
	cfg := &rest.Config{}
	quietWarnings(cfg, log.logger())
	if cfg.WarningHandlerWithContext == nil || cfg.WarningHandler == nil {
		t.Fatal("the config has no warning handler")
	}
	h := cfg.WarningHandlerWithContext.(*apiWarnings)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	ctx := context.Background()
	text := "the password " + auditMarker + " is weak"

	printed := auditStderr(t, func() {
		cfg.WarningHandlerWithContext.HandleWarningHeaderWithContext(ctx, 299, "-", text) // logged
		now = now.Add(100 * time.Millisecond)
		cfg.WarningHandler.HandleWarningHeader(299, "-", text) // counted
		now = now.Add(500 * time.Millisecond)
		cfg.WarningHandlerWithContext.HandleWarningHeaderWithContext(ctx, 299, "-", text) // counted
		now = now.Add(time.Second)
		cfg.WarningHandlerWithContext.HandleWarningHeaderWithContext(ctx, 299, "-", text) // logged, of three
	})
	if printed != "" {
		t.Errorf("printed %q", printed)
	}
	recs := log.all()
	if len(recs) != 2 {
		t.Fatalf("records %+v, want two", recs)
	}
	for i, want := range []string{"1", "3"} {
		r := recs[i]
		if r.Level != slog.LevelDebug || r.Message != "the API sent a warning" {
			t.Errorf("record %+v", r)
		}
		if code, _ := r.attr("code"); code != "299" {
			t.Errorf("code %q", code)
		}
		if n, _ := r.attr("warnings"); n != want {
			t.Errorf("warnings %q, want %s", n, want)
		}
	}
	auditNoMarker(t, &actionFixture{log: log})
}

func TestAPIWarningsOfAClient(t *testing.T) {
	text := "the password " + auditMarker + " is weak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Warning", `299 - "`+text+`"`)
		fmt.Fprint(w, `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"default"}}`)
	}))
	defer srv.Close()
	log := newAuditLog()
	cfg := &rest.Config{Host: srv.URL}
	quietWarnings(cfg, log.logger())
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	printed := auditStderr(t, func() {
		if _, err := typed.CoreV1().Namespaces().Get(context.Background(), "default", metav1.GetOptions{}); err != nil {
			t.Error(err)
		}
	})
	if printed != "" {
		t.Errorf("printed %q", printed)
	}
	if recs := log.with("the API sent a warning"); len(recs) != 1 {
		t.Errorf("records %+v, want one", log.all())
	}
	auditNoMarker(t, &actionFixture{log: log})
}
