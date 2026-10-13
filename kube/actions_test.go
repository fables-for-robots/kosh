package kube

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/fables-for-robots/kosh/keys"
)

// actionFixture is a Cluster on fakes that hold the same objects, a
// session of it, and the records of its log.
type actionFixture struct {
	*env
	s   *session
	log *auditLog
}

// actionEnv makes a Cluster on fakes that hold objs, with a log that
// keeps its records, and a session with grant. before runs before the
// Cluster is made, for reactors. What the Cluster sent while it was
// made is forgotten, so that the fakes' actions are the session's.
func actionEnv(t *testing.T, grant keys.Grant, before func(*env), objs ...runtime.Object) *actionFixture {
	t.Helper()
	log := newAuditLog()
	e := newEnv(t, envConfig{opts: Options{Log: log.logger()}, before: before}, objs...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := &actionFixture{env: e, s: e.c.Session(ctx, grant).(*session), log: log}
	f.forget()
	return f
}

// forget clears what the fakes recorded and what was logged.
func (f *actionFixture) forget() {
	f.typed.ClearActions()
	f.dyn.ClearActions()
	f.log.reset()
}

// sent returns what the session sent to either fake.
func (f *actionFixture) sent() []k8stesting.Action {
	return append(f.typed.Actions(), f.dyn.Actions()...)
}

// ref returns the ObjectRef of an object of the resource group/plural.
func (f *actionFixture) ref(t *testing.T, group, plural, namespace, name string) ObjectRef {
	t.Helper()
	return ObjectRef{Resource: f.resource(t, group, plural), Namespace: namespace, Name: name}
}

// actionDeployment is a deployment whose pod template has containers of
// the given names, each with the image "<name>:1".
func actionDeployment(namespace, name string, containers ...string) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, UID: types.UID(name + "-uid"), ResourceVersion: "7",
			Labels: labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: actionPtr[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: actionTemplate(labels, containers...),
		},
	}
}

func actionTemplate(labels map[string]string, containers ...string) corev1.PodTemplateSpec {
	tpl := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}}
	for _, c := range containers {
		tpl.Spec.Containers = append(tpl.Spec.Containers, corev1.Container{Name: c, Image: c + ":1"})
	}
	return tpl
}

func actionNode(name string) *corev1.Node {
	return &corev1.Node{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid")},
	}
}

func actionConfigMap(namespace, name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, ResourceVersion: "3"},
		Data:       map[string]string{"k": "v"},
	}
}

func actionWidget(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1", "kind": "Widget",
		"metadata": map[string]any{"namespace": namespace, "name": name},
		"spec":     map[string]any{"size": int64(3)},
	}}
}

// actionDeletes returns the deletes the dynamic fake was sent.
func actionDeletes(f *actionFixture) []k8stesting.DeleteActionImpl {
	var out []k8stesting.DeleteActionImpl
	for _, a := range f.dyn.Actions() {
		if d, ok := a.(k8stesting.DeleteActionImpl); ok {
			out = append(out, d)
		}
	}
	return out
}

// actionExists reports whether the dynamic fake holds the object.
func actionExists(t *testing.T, f *actionFixture, ref ObjectRef) bool {
	t.Helper()
	_, err := f.dyn.Tracker().Get(ref.Resource.GVR, ref.Namespace, ref.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestDelete(t *testing.T) {
	zero := int64(0)
	tests := []struct {
		name        string
		group, res  string
		namespace   string
		objName     string
		opts        DeleteOptions
		propagation metav1.DeletionPropagation
		grace       *int64
	}{
		{"default options", "apps", "deployments", "default", "web", DeleteOptions{}, metav1.DeletePropagationBackground, nil},
		{"background", "apps", "deployments", "default", "web", DeleteOptions{Propagation: PropagationBackground}, metav1.DeletePropagationBackground, nil},
		{"foreground", "apps", "deployments", "default", "web", DeleteOptions{Propagation: PropagationForeground}, metav1.DeletePropagationForeground, nil},
		{"orphan", "apps", "deployments", "default", "web", DeleteOptions{Propagation: PropagationOrphan}, metav1.DeletePropagationOrphan, nil},
		{"force", "apps", "deployments", "default", "web", DeleteOptions{Force: true}, metav1.DeletePropagationBackground, &zero},
		{"cluster-scoped", "", "nodes", "", "n1", DeleteOptions{}, metav1.DeletePropagationBackground, nil},
		{"generic", "example.com", "widgets", "default", "sprocket", DeleteOptions{}, metav1.DeletePropagationBackground, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := actionEnv(t, fullGrant, nil, actionDeployment("default", "web", "app"), actionNode("n1"), actionWidget("default", "sprocket"))
			ref := f.ref(t, tt.group, tt.res, tt.namespace, tt.objName)
			if err := f.s.Delete(context.Background(), ref, tt.opts); err != nil {
				t.Fatal(err)
			}
			dd := actionDeletes(f)
			if len(dd) != 1 {
				t.Fatalf("deletes %v, want one", dd)
			}
			d := dd[0]
			if d.GetResource() != ref.Resource.GVR || d.GetNamespace() != tt.namespace || d.GetName() != tt.objName {
				t.Errorf("deleted %v %q %q", d.GetResource(), d.GetNamespace(), d.GetName())
			}
			if p := d.DeleteOptions.PropagationPolicy; p == nil || *p != tt.propagation {
				t.Errorf("propagation %v, want %v", p, tt.propagation)
			}
			if g := d.DeleteOptions.GracePeriodSeconds; !reflect.DeepEqual(g, tt.grace) {
				t.Errorf("grace period %v, want %v", g, tt.grace)
			}
			if actionExists(t, f, ref) {
				t.Error("the object is still there")
			}
			if a := f.typed.Actions(); len(a) != 0 {
				t.Errorf("the clientset was sent %v", a)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		f := actionEnv(t, fullGrant, nil)
		err := f.s.Delete(context.Background(), f.ref(t, "apps", "deployments", "default", "gone"), DeleteOptions{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("%v, want the API's not found", err)
		}
	})
}

func TestKill(t *testing.T) {
	f := actionEnv(t, fullGrant, nil, pod("default", "web-1", nil), actionDeployment("default", "web", "app"))
	ref := f.ref(t, "", "pods", "default", "web-1")
	if err := f.s.Kill(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	dd := actionDeletes(f)
	if len(dd) != 1 || dd[0].GetName() != "web-1" || dd[0].GetNamespace() != "default" {
		t.Fatalf("deletes %v", dd)
	}
	opts := dd[0].DeleteOptions
	if opts.GracePeriodSeconds == nil || *opts.GracePeriodSeconds != 0 {
		t.Errorf("grace period %v, want 0", opts.GracePeriodSeconds)
	}
	if opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationBackground {
		t.Errorf("propagation %v, want background", opts.PropagationPolicy)
	}
	if actionExists(t, f, ref) {
		t.Error("the pod is still there")
	}

	f.forget()
	err := f.s.Kill(context.Background(), f.ref(t, "apps", "deployments", "default", "web"))
	if err == nil || !strings.Contains(err.Error(), "deployments/default/web") {
		t.Errorf("%v, want a refusal that names the deployment", err)
	}
	if a := f.sent(); len(a) != 0 {
		t.Errorf("sent %v", a)
	}
}

// actionYAML returns the object as the dynamic fake holds it, edited, as
// YAML, and forgets that it was read.
func actionYAML(t *testing.T, f *actionFixture, ref ObjectRef, edit func(map[string]any)) string {
	t.Helper()
	obj, err := f.dyn.Tracker().Get(ref.Resource.GVR, ref.Namespace, ref.Name)
	if err != nil {
		t.Fatal(err)
	}
	u := obj.(*unstructured.Unstructured).DeepCopy()
	if edit != nil {
		edit(u.Object)
	}
	b, err := yaml.Marshal(u.Object)
	if err != nil {
		t.Fatal(err)
	}
	f.forget()
	return string(b)
}

func TestUpdate(t *testing.T) {
	t.Run("replaces the object", func(t *testing.T) {
		f := actionEnv(t, fullGrant, nil, actionDeployment("default", "web", "app"))
		ref := f.ref(t, "apps", "deployments", "default", "web")
		text := actionYAML(t, f, ref, func(m map[string]any) {
			_ = unstructured.SetNestedField(m, int64(5), "spec", "replicas")
			_ = unstructured.SetNestedField(m, "edited", "metadata", "labels", "touched")
		})
		if err := f.s.Update(context.Background(), ref, text); err != nil {
			t.Fatal(err)
		}
		var updates int
		for _, a := range f.dyn.Actions() {
			if a.GetVerb() == "update" {
				updates++
			}
		}
		if updates != 1 {
			t.Errorf("%d updates, want 1", updates)
		}
		obj, err := f.dyn.Tracker().Get(ref.Resource.GVR, "default", "web")
		if err != nil {
			t.Fatal(err)
		}
		u := obj.(*unstructured.Unstructured)
		if n, _, _ := unstructured.NestedInt64(u.Object, "spec", "replicas"); n != 5 {
			t.Errorf("replicas %d, want 5", n)
		}
		if u.GetLabels()["touched"] != "edited" {
			t.Errorf("labels %v", u.GetLabels())
		}
	})

	t.Run("a cluster-scoped object", func(t *testing.T) {
		n := actionNode("n1")
		n.ResourceVersion = "4"
		f := actionEnv(t, fullGrant, nil, n)
		ref := f.ref(t, "", "nodes", "", "n1")
		text := actionYAML(t, f, ref, func(m map[string]any) {
			_ = unstructured.SetNestedField(m, true, "spec", "unschedulable")
		})
		if err := f.s.Update(context.Background(), ref, text); err != nil {
			t.Fatal(err)
		}
		obj, _ := f.dyn.Tracker().Get(ref.Resource.GVR, "", "n1")
		if on, _, _ := unstructured.NestedBool(obj.(*unstructured.Unstructured).Object, "spec", "unschedulable"); !on {
			t.Error("the node is not unschedulable")
		}
	})

	refusals := []struct {
		name string
		text func(t *testing.T, f *actionFixture, ref ObjectRef) string
		want string
	}{
		{"not YAML", func(*testing.T, *actionFixture, ObjectRef) string { return "metadata: [unclosed\n  - {" }, "the YAML does not parse"},
		{"not an object", func(*testing.T, *actionFixture, ObjectRef) string { return "- just\n- a list\n" }, "the YAML is not an object"},
		{"another name", func(t *testing.T, f *actionFixture, ref ObjectRef) string {
			return actionYAML(t, f, ref, func(m map[string]any) { _ = unstructured.SetNestedField(m, "api", "metadata", "name") })
		}, "the YAML names another object: "},
		{"another namespace", func(t *testing.T, f *actionFixture, ref ObjectRef) string {
			return actionYAML(t, f, ref, func(m map[string]any) { _ = unstructured.SetNestedField(m, "prod", "metadata", "namespace") })
		}, "the YAML names another object: "},
		{"no namespace", func(t *testing.T, f *actionFixture, ref ObjectRef) string {
			return actionYAML(t, f, ref, func(m map[string]any) { unstructured.RemoveNestedField(m, "metadata", "namespace") })
		}, "the YAML names another object: "},
		{"another kind", func(t *testing.T, f *actionFixture, ref ObjectRef) string {
			return actionYAML(t, f, ref, func(m map[string]any) { m["kind"] = "StatefulSet" })
		}, "the YAML names another object: "},
		{"another apiVersion", func(t *testing.T, f *actionFixture, ref ObjectRef) string {
			return actionYAML(t, f, ref, func(m map[string]any) { m["apiVersion"] = "apps/v1beta2" })
		}, "the YAML names another object: "},
		{"no resourceVersion", func(t *testing.T, f *actionFixture, ref ObjectRef) string {
			return actionYAML(t, f, ref, func(m map[string]any) { unstructured.RemoveNestedField(m, "metadata", "resourceVersion") })
		}, "the YAML has no resourceVersion: it must be the object as it was read"},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			f := actionEnv(t, fullGrant, nil, actionDeployment("default", "web", "app"))
			ref := f.ref(t, "apps", "deployments", "default", "web")
			err := f.s.Update(context.Background(), ref, tt.text(t, f, ref))
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%v, want %q", err, tt.want)
			}
			if a := f.sent(); len(a) != 0 {
				t.Errorf("sent %v", a)
			}
		})
	}
}

func TestUpdateConflict(t *testing.T) {
	conflict := func(e *env) {
		e.dyn.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", errors.New("the object has been modified"))
		})
	}
	f := actionEnv(t, fullGrant, conflict, actionDeployment("default", "web", "app"))
	ref := f.ref(t, "apps", "deployments", "default", "web")
	err := f.s.Update(context.Background(), ref, actionYAML(t, f, ref, nil))
	if !apierrors.IsConflict(err) {
		t.Errorf("%v, want the API's conflict", err)
	}
}

func actionPtr[T any](v T) *T { return &v }
