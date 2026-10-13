package kube

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func readTemplate(init []string, containers ...string) corev1.PodTemplateSpec {
	var t corev1.PodTemplateSpec
	for _, c := range init {
		t.Spec.InitContainers = append(t.Spec.InitContainers, corev1.Container{Name: c, Image: "registry.example/" + c + ":1"})
	}
	for _, c := range containers {
		t.Spec.Containers = append(t.Spec.Containers, corev1.Container{Name: c, Image: "registry.example/" + c + ":2"})
	}
	return t
}

func TestImages(t *testing.T) {
	e := newEnv(t, envConfig{},
		&appsv1.Deployment{ObjectMeta: objMeta("default", "web"), Spec: appsv1.DeploymentSpec{
			Selector: selector("web"), Template: readTemplate([]string{"migrate", "warm"}, "app", "sidecar")}},
		&appsv1.StatefulSet{ObjectMeta: objMeta("default", "db"), Spec: appsv1.StatefulSetSpec{
			Selector: selector("db"), Template: readTemplate(nil, "postgres")}},
		&appsv1.DaemonSet{ObjectMeta: objMeta("default", "agent"), Spec: appsv1.DaemonSetSpec{
			Selector: selector("agent"), Template: readTemplate([]string{"setup"}, "agent")}},
		readPodWith("default", "web-1", nil, "app"),
	)
	s := e.session(t)
	ctx := context.Background()
	for _, tc := range []struct {
		group, plural, name string
		want                []ContainerImage
	}{
		{"apps", "deployments", "web", []ContainerImage{
			{Name: "migrate", Image: "registry.example/migrate:1", Init: true},
			{Name: "warm", Image: "registry.example/warm:1", Init: true},
			{Name: "app", Image: "registry.example/app:2"},
			{Name: "sidecar", Image: "registry.example/sidecar:2"},
		}},
		{"apps", "statefulsets", "db", []ContainerImage{
			{Name: "postgres", Image: "registry.example/postgres:2"},
		}},
		{"apps", "daemonsets", "agent", []ContainerImage{
			{Name: "setup", Image: "registry.example/setup:1", Init: true},
			{Name: "agent", Image: "registry.example/agent:2"},
		}},
	} {
		got, err := s.Images(ctx, ObjectRef{Resource: e.resource(t, tc.group, tc.plural), Namespace: "default", Name: tc.name})
		if err != nil {
			t.Errorf("%s: %v", tc.plural, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.plural, got, tc.want)
		}
	}
	if _, err := s.Images(ctx, ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web-1"}); err == nil {
		t.Error("a pod's reference: no error")
	}
	if _, err := s.Images(ctx, ObjectRef{Resource: e.resource(t, "apps", "deployments"), Namespace: "default", Name: "missing"}); err == nil {
		t.Error("a deployment that is not there: no error")
	}
}

// readScaleReactor answers a get of a scale subresource with the number
// of replicas scales has for the object's name, which tells it from the
// object's own spec.
func readScaleReactor(e *env, scales map[string]int32) {
	e.typed.PrependReactor("get", "*", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		g, ok := a.(k8stesting.GetAction)
		if !ok || a.GetSubresource() != "scale" {
			return false, nil, nil
		}
		n, ok := scales[a.GetResource().Resource+"/"+g.GetName()]
		if !ok {
			return false, nil, nil
		}
		return true, &autoscalingv1.Scale{Spec: autoscalingv1.ScaleSpec{Replicas: n}}, nil
	})
}

func TestReplicas(t *testing.T) {
	one := int32(1)
	e := newEnv(t, envConfig{},
		&appsv1.Deployment{ObjectMeta: objMeta("default", "web"), Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: selector("web")}},
		&appsv1.StatefulSet{ObjectMeta: objMeta("default", "db"), Spec: appsv1.StatefulSetSpec{Replicas: &one, Selector: selector("db")}},
		&appsv1.ReplicaSet{ObjectMeta: objMeta("default", "web-123"), Spec: appsv1.ReplicaSetSpec{Replicas: &one, Selector: selector("web")}},
		&appsv1.DaemonSet{ObjectMeta: objMeta("default", "agent"), Spec: appsv1.DaemonSetSpec{Selector: selector("agent")}},
	)
	readScaleReactor(e, map[string]int32{"deployments/web": 3, "statefulsets/db": 4, "replicasets/web-123": 5})
	s := e.session(t)
	ctx := context.Background()
	for _, tc := range []struct {
		plural, name string
		want         int32
	}{
		{"deployments", "web", 3},
		{"statefulsets", "db", 4},
		{"replicasets", "web-123", 5},
	} {
		got, err := s.Replicas(ctx, ObjectRef{Resource: e.resource(t, "apps", tc.plural), Namespace: "default", Name: tc.name})
		if err != nil || got != tc.want {
			t.Errorf("%s: %d, %v; want the scale's %d", tc.plural, got, err, tc.want)
		}
	}
	if _, err := s.Replicas(ctx, ObjectRef{Resource: e.resource(t, "apps", "daemonsets"), Namespace: "default", Name: "agent"}); err == nil {
		t.Error("a daemonset: no error")
	}
}
