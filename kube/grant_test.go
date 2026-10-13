package kube

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"
)

var actionsType = reflect.TypeOf((*Actions)(nil)).Elem()

func TestEveryMethodOfActionsTakesAContextAndReturnsAnError(t *testing.T) {
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	errType := reflect.TypeOf((*error)(nil)).Elem()
	if actionsType.NumMethod() == 0 {
		t.Fatal("Actions has no methods")
	}
	for i := 0; i < actionsType.NumMethod(); i++ {
		m := actionsType.Method(i)
		if m.Type.NumIn() == 0 || m.Type.In(0) != ctxType {
			t.Errorf("%s: the first parameter is not a context.Context", m.Name)
		}
		if n := m.Type.NumOut(); n == 0 || m.Type.Out(n-1) != errType {
			t.Errorf("%s: the last result is not an error", m.Name)
		}
	}
}

func TestReadOnlyGrantIsRefusedEveryAction(t *testing.T) {
	f := actionEnv(t, readOnlyGrant, nil, pod("default", "web", nil))
	v := reflect.ValueOf(f.s)
	for i := 0; i < actionsType.NumMethod(); i++ {
		m := actionsType.Method(i)
		args := make([]reflect.Value, m.Type.NumIn())
		args[0] = reflect.ValueOf(context.Background())
		for j := 1; j < len(args); j++ {
			args[j] = reflect.Zero(m.Type.In(j))
		}
		f.log.reset()
		out := v.MethodByName(m.Name).Call(args)
		err, _ := out[len(out)-1].Interface().(error)
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s: %v, want ErrReadOnly", m.Name, err)
		}
		if recs, all := f.log.with("refused"), f.log.all(); len(recs) != 1 || len(all) != 1 || recs[0].Level != slog.LevelWarn {
			t.Errorf("%s: records %+v, want one refusal", m.Name, all)
		}
	}
	if a := f.typed.Actions(); len(a) != 0 {
		t.Errorf("the clientset was sent %v", a)
	}
	if a := f.dyn.Actions(); len(a) != 0 {
		t.Errorf("the dynamic client was sent %v", a)
	}
}

func TestRefusalIsLogged(t *testing.T) {
	f := actionEnv(t, readOnlyGrant, nil)
	ref := f.ref(t, "apps", "deployments", "default", "web")
	if err := f.s.Scale(context.Background(), ref, 3); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Scale: %v", err)
	}
	recs := f.log.all()
	if len(recs) != 1 {
		t.Fatalf("records %+v, want one", recs)
	}
	r := recs[0]
	if r.Level != slog.LevelWarn || r.Message != "refused" {
		t.Errorf("level %v, message %q", r.Level, r.Message)
	}
	want := map[string]string{
		"key":         readOnlyGrant.Name,
		"fingerprint": readOnlyGrant.Fingerprint,
		"action":      "scale",
		"object":      "deployments/default/web",
	}
	for k, v := range want {
		if got, ok := r.attr(k); !ok || got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}
