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

// The fake refuses nothing by the grant, as the real Session does not:
// the API server refuses a role, and a test that wants that puts the
// refusal into Errs.
func TestAViewGrantIsRefusedNothing(t *testing.T) {
	s := New(keys.Grant{Name: "guest/view", Role: keys.Role{Kind: keys.KindClusterRole, Name: "view"}})
	actions := reflect.TypeOf((*kube.Actions)(nil)).Elem()
	v := reflect.ValueOf(s)
	for i := 0; i < actions.NumMethod(); i++ {
		m := actions.Method(i)
		args := []reflect.Value{reflect.ValueOf(context.Background())}
		for a := 1; a < m.Type.NumIn(); a++ {
			args = append(args, reflect.Zero(m.Type.In(a)))
		}
		out := v.MethodByName(m.Name).Call(args)
		if err, _ := out[len(out)-1].Interface().(error); err != nil {
			t.Errorf("%s: %v", m.Name, err)
		}
	}
	if calls := s.Calls(); len(calls) != actions.NumMethod() {
		t.Errorf("recorded %d calls, want %d", len(calls), actions.NumMethod())
	}

	ref := Ref("secrets", "default", "db")
	s.Secrets[ref.String()] = []kube.SecretEntry{{Key: "password", Value: "hunter2"}}
	got, err := s.SecretData(context.Background(), ref)
	if err != nil || len(got) != 1 || got[0].Value != "hunter2" {
		t.Errorf("SecretData: %v, %v", got, err)
	}
	forbidden := errors.New("secrets \"db\" is forbidden")
	s.Errs["SecretData"] = forbidden
	if _, err := s.SecretData(context.Background(), ref); !errors.Is(err, forbidden) {
		t.Errorf("SecretData with a refusal in Errs: %v", err)
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

func TestCanAnswersWhatWasAllowed(t *testing.T) {
	s := New(keys.Grant{})
	ctx := context.Background()
	pods, deployments := Resource("pods"), Resource("deployments")
	if s.Can(ctx, "get", pods, "", "default") {
		t.Error("allowed before Allow")
	}
	s.Allow("delete", "", "pods", "", "team-a")
	s.Allow("get", "*", "*", "", "*")
	s.Allow("*", "apps", "deployments", "scale", "default")
	for _, tc := range []struct {
		verb string
		res  kube.Resource
		sub  string
		ns   string
		want bool
	}{
		{"delete", pods, "", "team-a", true},
		{"delete", pods, "", "default", false},
		{"delete", pods, "", "", false},
		{"delete", pods, "eviction", "team-a", false},
		{"delete", deployments, "", "team-a", false},
		{"get", pods, "", "", true},
		{"get", deployments, "", "kube-system", true},
		{"get", pods, "log", "default", false},
		{"update", deployments, "scale", "default", true},
		{"update", deployments, "scale", "team-a", false},
		{"update", deployments, "", "default", false},
	} {
		if got := s.Can(ctx, tc.verb, tc.res, tc.sub, tc.ns); got != tc.want {
			t.Errorf("Can(%s, %s, %q, %q) = %v, want %v", tc.verb, tc.res.GVR.Resource, tc.sub, tc.ns, got, tc.want)
		}
	}
	if n := len(s.Called("Can")); n != 0 {
		t.Errorf("Can was recorded %d times among the calls", n)
	}
}
