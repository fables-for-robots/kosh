package kubetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
)

// The fake keeps the rule of the real Session: every method of
// kube.Actions refuses a read-only grant, and records nothing.
func TestReadOnlyGrantIsRefusedEveryAction(t *testing.T) {
	s := New(keys.Grant{Name: "guest", ReadOnly: true})
	actions := reflect.TypeOf((*kube.Actions)(nil)).Elem()
	v := reflect.ValueOf(s)
	for i := 0; i < actions.NumMethod(); i++ {
		m := actions.Method(i)
		args := []reflect.Value{reflect.ValueOf(context.Background())}
		for a := 1; a < m.Type.NumIn(); a++ {
			args = append(args, reflect.Zero(m.Type.In(a)))
		}
		out := v.MethodByName(m.Name).Call(args)
		err, _ := out[len(out)-1].Interface().(error)
		if !errors.Is(err, kube.ErrReadOnly) {
			t.Errorf("%s: %v, want ErrReadOnly", m.Name, err)
		}
	}
	if calls := s.Calls(); len(calls) != 0 {
		t.Errorf("recorded %v", calls)
	}
}

func TestReadOnlyGrantSeesNoSecret(t *testing.T) {
	s := New(keys.Grant{Name: "guest", ReadOnly: true})
	ref := Ref("secrets", "default", "db")
	ctx := context.Background()
	if _, err := s.YAML(ctx, ref, false); !errors.Is(err, kube.ErrReadOnly) {
		t.Errorf("YAML: %v", err)
	}
	if _, err := s.Describe(ctx, ref); !errors.Is(err, kube.ErrReadOnly) {
		t.Errorf("Describe: %v", err)
	}
	if _, err := s.SecretData(ctx, ref); !errors.Is(err, kube.ErrReadOnly) {
		t.Errorf("SecretData: %v", err)
	}
	if _, err := s.YAML(ctx, Ref("configmaps", "default", "cfg"), false); err != nil {
		t.Errorf("YAML of a configmap: %v", err)
	}
}

func TestResolveKnowsEveryName(t *testing.T) {
	s := New(keys.Grant{})
	for name, want := range map[string]string{
		"pods": "pods", "pod": "pods", "po": "pods", "Pod": "pods", "PO": "pods",
		"deploy": "deployments", "deployments.apps": "deployments",
		"crd": "customresourcedefinitions", "wd": "widgets", "widgets.example.com": "widgets",
	} {
		r, err := s.Resolve(context.Background(), name)
		if err != nil || r.GVR.Resource != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", name, r.GVR.Resource, err, want)
		}
	}
	if _, err := s.Resolve(context.Background(), "nothing"); !errors.Is(err, kube.ErrUnknownResource) {
		t.Errorf("Resolve(nothing): %v", err)
	}
}

func TestSetTableSignalsTheWatch(t *testing.T) {
	s := New(keys.Grant{})
	q := kube.Query{Resource: Resource("pods"), Namespace: "default"}
	w, err := s.Watch(q)
	if err != nil {
		t.Fatal(err)
	}
	s.SetTable(q, kube.Table{Namespaced: true})
	select {
	case <-w.Changed():
	default:
		t.Fatal("no signal")
	}
	if !w.Table(time.Now()).Namespaced {
		t.Error("the table is not the one that was set")
	}
	w.Close()
	if open := s.OpenWatches(); len(open) != 0 {
		t.Errorf("open after Close: %v", open)
	}
}

func TestFakeUsage(t *testing.T) {
	s := New(keys.Grant{})
	u := s.ClusterUsage()
	defer u.Close()
	if got, ok := u.Current(); ok || got.Served {
		t.Errorf("%+v, %v before SetUsage", got, ok)
	}
	want := kube.ClusterUsage{CPU: 12, Mem: 47, Served: true}
	s.SetUsage(want, true)
	select {
	case <-u.Changed():
	default:
		t.Fatal("no signal after SetUsage")
	}
	if got, ok := u.Current(); !ok || got != want {
		t.Errorf("%+v, %v; want %+v, true", got, ok, want)
	}
	s.SetUsage(kube.ClusterUsage{Served: true}, false)
	s.SetUsage(kube.ClusterUsage{Served: true}, false)
	select {
	case <-u.Changed():
	default:
		t.Fatal("no signal after the second SetUsage")
	}
	if got, ok := u.Current(); ok || !got.Served {
		t.Errorf("%+v, %v; want served and not ok", got, ok)
	}
}
