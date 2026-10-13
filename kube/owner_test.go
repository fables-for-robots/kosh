package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func readOwned(name string, owners ...metav1.OwnerReference) *corev1.Pod {
	p := pod("default", name, nil)
	p.OwnerReferences = owners
	return p
}

func readOwnerRef(apiVersion, kind, name string, controller bool) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: types.UID("uid-" + name), Controller: &controller}
}

func TestOwner(t *testing.T) {
	e := newEnv(t, envConfig{},
		readOwned("by-rs", readOwnerRef("apps/v1", "ReplicaSet", "web-123", true)),
		readOwned("by-several",
			readOwnerRef("v1", "ConfigMap", "first", false),
			readOwnerRef("apps/v1", "ReplicaSet", "web-456", true),
			readOwnerRef("batch/v1", "Job", "last", false)),
		readOwned("by-the-first",
			readOwnerRef("batch/v1", "Job", "first", false),
			readOwnerRef("apps/v1", "ReplicaSet", "second", false)),
		readOwned("by-a-node", readOwnerRef("v1", "Node", "node-1", true)),
		readOwned("orphan"),
		readOwned("by-a-gizmo", readOwnerRef("example.com/v1", "Gizmo", "g", true)),
	)
	s := e.session(t)
	pods := e.resource(t, "", "pods")
	owner := func(name string) (ObjectRef, error) {
		return s.Owner(context.Background(), ObjectRef{Resource: pods, Namespace: "default", Name: name})
	}

	for _, tc := range []struct {
		pod  string
		want ObjectRef
	}{
		{"by-rs", ObjectRef{Resource: e.resource(t, "apps", "replicasets"), Namespace: "default", Name: "web-123"}},
		{"by-several", ObjectRef{Resource: e.resource(t, "apps", "replicasets"), Namespace: "default", Name: "web-456"}},
		{"by-the-first", ObjectRef{Resource: e.resource(t, "batch", "jobs"), Namespace: "default", Name: "first"}},
		{"by-a-node", ObjectRef{Resource: e.resource(t, "", "nodes"), Name: "node-1"}},
	} {
		got, err := owner(tc.pod)
		if err != nil {
			t.Errorf("%s: %v", tc.pod, err)
			continue
		}
		if got.String() != tc.want.String() || got.Resource.Kind != tc.want.Resource.Kind || got.Resource.Namespaced != tc.want.Resource.Namespaced {
			t.Errorf("%s: owner %s (%s), want %s (%s)", tc.pod, got, got.Resource.Kind, tc.want, tc.want.Resource.Kind)
		}
	}

	if _, err := owner("orphan"); !errors.Is(err, ErrNoOwner) {
		t.Errorf("no owner: got %v, want ErrNoOwner", err)
	}
	if _, err := owner("by-a-gizmo"); err == nil || !strings.Contains(err.Error(), "Gizmo") {
		t.Errorf("an owner of an unknown kind: got %v, want an error that names it", err)
	}
	if _, err := owner("missing"); err == nil || errors.Is(err, ErrNoOwner) {
		t.Errorf("an object that is not there: got %v, want the API's error", err)
	}
}
