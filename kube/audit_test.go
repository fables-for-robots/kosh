package kube

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	}
	tests := []struct {
		name string
		call func(t *testing.T, f *actionFixture) error
		word string
	}{
		{"the API refuses", func(t *testing.T, f *actionFixture) error {
			return f.s.Delete(ctx, f.ref(t, "apps", "deployments", "default", "web"), DeleteOptions{})
		}, "delete"},
		{"the object is not there", func(t *testing.T, f *actionFixture) error {
			return f.s.Kill(ctx, f.ref(t, "", "pods", "default", "gone"))
		}, "kill"},
		{"the kind", func(t *testing.T, f *actionFixture) error {
			return f.s.Scale(ctx, f.ref(t, "", "pods", "default", "web-1"), 2)
		}, "scale"},
		{"the arguments", func(t *testing.T, f *actionFixture) error {
			return f.s.Scale(ctx, f.ref(t, "apps", "deployments", "default", "web"), -2)
		}, "scale"},
		{"the YAML", func(t *testing.T, f *actionFixture) error {
			return f.s.Update(ctx, f.ref(t, "apps", "deployments", "default", "web"), "kind: [")
		}, "update"},
		{"a container", func(t *testing.T, f *actionFixture) error {
			return f.s.SetImages(ctx, f.ref(t, "apps", "deployments", "default", "web"), map[string]string{"nope": "x"})
		}, "set-images"},
		{"the drain", func(t *testing.T, f *actionFixture) error {
			return f.s.Drain(ctx, f.ref(t, "", "nodes", "", "n1"), DefaultDrainOptions())
		}, "drain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append(actionDrainObjects(), actionDeployment("default", "web", "app"), pod("default", "web-1", nil))
			f := actionEnv(t, fullGrant, failing, objs...)
			err := tt.call(t, f)
			if err == nil {
				t.Fatal("no error")
			}
			r := auditOneAction(t, f)
			if got, _ := r.attr("action"); got != tt.word {
				t.Errorf("action %q, want %q", got, tt.word)
			}
			if got, ok := r.attr("error"); !ok || got != err.Error() {
				t.Errorf("error %q, want %q", got, err.Error())
			}
		})
	}
}

func TestUpdateLogHoldsNoYAML(t *testing.T) {
	const marker = "zebra-7d1f-marker"
	f := actionEnv(t, fullGrant, nil, actionDeployment("default", "web", "app"))
	ref := f.ref(t, "apps", "deployments", "default", "web")
	texts := []string{
		actionYAML(t, f, ref, func(m map[string]any) { _ = unstructured.SetNestedField(m, marker, "metadata", "labels", "mark") }),
		actionYAML(t, f, ref, func(m map[string]any) { _ = unstructured.SetNestedField(m, marker, "metadata", "name") }),
		actionYAML(t, f, ref, func(m map[string]any) { _ = unstructured.SetNestedField(m, marker, "metadata", "namespace") }),
		actionYAML(t, f, ref, func(m map[string]any) { m["kind"] = marker }),
		actionYAML(t, f, ref, func(m map[string]any) { m["apiVersion"] = marker }),
	}
	for _, text := range texts {
		_ = f.s.Update(context.Background(), ref, text)
	}
	recs := f.log.all()
	if len(recs) != len(texts) {
		t.Errorf("%d records, want %d", len(recs), len(texts))
	}
	for _, r := range recs {
		for k, v := range r.Attrs {
			if strings.Contains(v.String(), marker) {
				t.Errorf("%s = %q holds the YAML", k, v.String())
			}
		}
	}
}
