package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	batchv1 "k8s.io/api/batch/v1"
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

// actionJSON decodes JSON for comparison, numbers as written.
func actionJSON(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return v
}

func TestUpdateTakesOneDocument(t *testing.T) {
	const (
		parse  = "the YAML does not parse"
		more   = "the YAML holds more than one document"
		object = "the YAML is not an object"
	)
	tests := []struct {
		name string
		text func(obj string) string // obj: the object's YAML, edited
		want string                  // the refusal; empty: accepted
		// extra, when not nil, is what the top-level field extra must be
		// in what was sent, as JSON.
		extra any
	}{
		{"a single document", func(o string) string { return o }, "", nil},
		{"a leading ---", func(o string) string { return "---\n" + o }, "", nil},
		{"a comment and ---", func(o string) string { return "# edited\n---\n" + o }, "", nil},
		{"a trailing --- and nothing after it", func(o string) string { return o + "---\n" }, "", nil},
		{"a second document that is only a comment", func(o string) string { return "---\n" + o + "---\n# nothing more\n" }, "", nil},
		{"a second document that is null", func(o string) string { return o + "---\nnull\n" }, "", nil},
		{"a second document that is an anchored null", func(o string) string { return o + "---\n&a ~\n" }, "", nil},
		{"an end marker and a comment", func(o string) string { return o + "...\n# the end\n" }, "", nil},
		{"a %YAML 1.1 preamble", func(o string) string { return "%YAML 1.1\n---\n" + o }, "", nil},
		{"--- in a block scalar", func(o string) string { return o + "extra: |\n  a\n  ---\n  b\n" }, "", "a\n---\nb\n"},
		{"--- in a quoted string", func(o string) string { return o + "extra: \"a\n  ---\n  b\"\n" }, "", "a --- b"},
		{"folded, a more-indented line", func(o string) string { return o + "extra: >\n  a\n   b\n  c\n" }, "", "a\n b\nc\n"},
		{"folded, strip, a more-indented line", func(o string) string { return o + "extra: >-\n  a\n   b\n  c\n" }, "", "a\n b\nc"},
		{"folded, keep, a more-indented line", func(o string) string { return o + "extra: >+\n  a\n   b\n  c\n\n" }, "", "a\n b\nc\n\n"},
		{"folded, a tab-led line", func(o string) string { return o + "extra: >\n  a\n  \tb\n  c\n" }, "", nil},
		{"folded, a blank line before an indented one", func(o string) string { return o + "extra: >\n  a\n\n   b\n  c\n" }, "", nil},
		{"literal, trailing spaces", func(o string) string { return o + "extra: |\n  a  \n  b \n\n" }, "", "a  \nb \n"},
		{"literal, strip", func(o string) string { return o + "extra: |-\n  a  \n  b \n\n" }, "", "a  \nb "},
		{"literal, keep", func(o string) string { return o + "extra: |+\n  a  \n  b \n\n" }, "", "a  \nb \n\n"},
		{"YAML 1.1 booleans", func(o string) string { return o + "extra: {a: yes, b: on, c: Off}\n" }, "", nil},
		{"numbers", func(o string) string { return o + "extra: {a: 0123, b: \"0123\", c: 1e3, d: 0x1f}\n" }, "", nil},
		{"tags", func(o string) string { return o + "extra: {a: !!binary aGVsbG8=, b: !!str 0123}\n" }, "", nil},
		{"an anchor, an alias and a merge", func(o string) string {
			return o + "base: &b {x: 1, y: 2}\nextra:\n  <<: *b\n  y: 3\n  z: *b\n"
		}, "", nil},
		{"a duplicate key", func(o string) string { return o + "extra: one\nextra: two\n" }, "", "two"},
		{"a multi-line plain string", func(o string) string { return o + "extra: this is\n  a plain\n\n  string\n" }, "", "this is a plain\nstring"},

		{"a second document after ---", func(o string) string { return o + "---\nkind: B\n" }, more, nil},
		{"the same document twice", func(o string) string { return o + "---\n" + o }, more, nil},
		{"a second document that does not parse", func(o string) string { return o + "---\n[[[\n" }, more, nil},
		{"... kind: B", func(o string) string { return o + "... kind: B\n" }, more, nil},
		{"... {kind: B}", func(o string) string { return o + "... {kind: B}\n" }, more, nil},
		{"... and a line of %", func(o string) string { return o + "...\n%password: x\n" }, more, nil},
		{"... and more YAML", func(o string) string { return o + "...\nkind: B\n" }, more, nil},
		{"... and text that is not YAML", func(o string) string { return o + "...\n[[[ not yaml\n" }, more, nil},
		{"... and another document", func(o string) string { return o + "...\n%YAML 1.1\n---\nkind: B\n" }, more, nil},
		{"a %YAML 1.2 preamble, which the parser does not know", func(o string) string { return "%YAML 1.2\n---\n" + o }, parse, nil},
		{"a list", func(string) string { return "- just\n- a list\n" }, object, nil},
		{"a scalar", func(string) string { return "just a word\n" }, object, nil},
		{"nothing", func(string) string { return "" }, object, nil},
		{"only a comment", func(string) string { return "# nothing\n" }, object, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := actionEnv(t, fullGrant, nil, actionDeployment("default", "web", "app"))
			ref := f.ref(t, "apps", "deployments", "default", "web")
			obj := actionYAML(t, f, ref, func(m map[string]any) {
				_ = unstructured.SetNestedField(m, int64(9), "spec", "replicas")
			})
			text := tt.text(obj)
			err := f.s.Update(context.Background(), ref, text)
			if tt.want != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
					t.Errorf("%v, want %q", err, tt.want)
				}
				if a := f.sent(); len(a) != 0 {
					t.Errorf("sent %v", a)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var sent *unstructured.Unstructured
			for _, a := range f.dyn.Actions() {
				if u, ok := a.(k8stesting.UpdateAction); ok {
					sent = u.GetObject().(*unstructured.Unstructured)
				}
			}
			if sent == nil {
				t.Fatal("nothing was sent")
			}
			got, err := json.Marshal(sent.Object)
			if err != nil {
				t.Fatal(err)
			}
			want, err := yaml.YAMLToJSON([]byte(text))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actionJSON(t, got), actionJSON(t, want)) {
				t.Errorf("sent\n%s\nwant what sigs.k8s.io/yaml makes of the text\n%s", got, want)
			}
			if n, _, _ := unstructured.NestedInt64(sent.Object, "spec", "replicas"); n != 9 {
				t.Errorf("replicas %d, want 9", n)
			}
			if tt.extra != nil && !reflect.DeepEqual(sent.Object["extra"], tt.extra) {
				t.Errorf("extra %q, want %q", sent.Object["extra"], tt.extra)
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

// actionWorkload is a deployment, statefulset, daemonset or replicaset
// whose pod template has containers of the given names, each with the
// image "<name>:1".
func actionWorkload(kind, namespace, name string, containers ...string) runtime.Object {
	d := actionDeployment(namespace, name, containers...)
	meta := d.ObjectMeta
	meta.Labels = nil
	tm := metav1.TypeMeta{APIVersion: "apps/v1", Kind: kind}
	switch kind {
	case "Deployment":
		return d
	case "StatefulSet":
		return &appsv1.StatefulSet{TypeMeta: tm, ObjectMeta: meta, Spec: appsv1.StatefulSetSpec{Replicas: d.Spec.Replicas, Selector: d.Spec.Selector, Template: d.Spec.Template}}
	case "DaemonSet":
		return &appsv1.DaemonSet{TypeMeta: tm, ObjectMeta: meta, Spec: appsv1.DaemonSetSpec{Selector: d.Spec.Selector, Template: d.Spec.Template}}
	case "ReplicaSet":
		return &appsv1.ReplicaSet{TypeMeta: tm, ObjectMeta: meta, Spec: appsv1.ReplicaSetSpec{Replicas: d.Spec.Replicas, Selector: d.Spec.Selector, Template: d.Spec.Template}}
	}
	panic("no workload of kind " + kind)
}

// actionScaleSubresource makes the clientset fake serve the scale
// subresource of deployments, statefulsets and replicasets from their
// spec.replicas, which it does not by itself.
func actionScaleSubresource(e *env) {
	for _, res := range []string{"deployments", "statefulsets", "replicasets"} {
		e.typed.PrependReactor("get", res, func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.GetSubresource() != "scale" {
				return false, nil, nil
			}
			name := a.(k8stesting.GetAction).GetName()
			obj, err := e.typed.Tracker().Get(a.GetResource(), a.GetNamespace(), name)
			if err != nil {
				return true, nil, err
			}
			replicas := reflect.ValueOf(obj).Elem().FieldByName("Spec").FieldByName("Replicas").Interface().(*int32)
			return true, &autoscalingv1.Scale{
				ObjectMeta: metav1.ObjectMeta{Namespace: a.GetNamespace(), Name: name},
				Spec:       autoscalingv1.ScaleSpec{Replicas: *replicas},
			}, nil
		})
		e.typed.PrependReactor("update", res, func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.GetSubresource() != "scale" {
				return false, nil, nil
			}
			sc := a.(k8stesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
			obj, err := e.typed.Tracker().Get(a.GetResource(), a.GetNamespace(), sc.Name)
			if err != nil {
				return true, nil, err
			}
			n := sc.Spec.Replicas
			reflect.ValueOf(obj).Elem().FieldByName("Spec").FieldByName("Replicas").Set(reflect.ValueOf(&n))
			if err := e.typed.Tracker().Update(a.GetResource(), obj, a.GetNamespace()); err != nil {
				return true, nil, err
			}
			return true, sc, nil
		})
	}
}

// actionAcceptPatches makes the dynamic fake answer every patch with the
// object as it is: it cannot apply a strategic merge patch as the API
// does, so the tests look at the patch it was sent.
func actionAcceptPatches(e *env) {
	e.dyn.PrependReactor("patch", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := e.dyn.Tracker().Get(a.GetResource(), a.GetNamespace(), a.(k8stesting.PatchAction).GetName())
		return true, obj, err
	})
}

// actionPatches returns the patches the dynamic fake was sent.
func actionPatches(f *actionFixture) []k8stesting.PatchAction {
	var out []k8stesting.PatchAction
	for _, a := range f.dyn.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok {
			out = append(out, p)
		}
	}
	return out
}

// actionOnePatch returns the one patch the dynamic fake was sent, which
// must be a strategic merge patch, decoded.
func actionOnePatch(t *testing.T, f *actionFixture) map[string]any {
	t.Helper()
	pp := actionPatches(f)
	if len(pp) != 1 {
		t.Fatalf("patches %v, want one", pp)
	}
	if pp[0].GetPatchType() != types.StrategicMergePatchType {
		t.Errorf("patch type %s, want a strategic merge patch", pp[0].GetPatchType())
	}
	var body map[string]any
	if err := json.Unmarshal(pp[0].GetPatch(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestScale(t *testing.T) {
	for _, kind := range []struct{ kind, res string }{{"Deployment", "deployments"}, {"StatefulSet", "statefulsets"}, {"ReplicaSet", "replicasets"}} {
		t.Run(kind.res, func(t *testing.T) {
			f := actionEnv(t, fullGrant, actionScaleSubresource, actionWorkload(kind.kind, "default", "web", "app"))
			ref := f.ref(t, "apps", kind.res, "default", "web")
			if err := f.s.Scale(context.Background(), ref, 4); err != nil {
				t.Fatal(err)
			}
			var verbs []string
			for _, a := range f.typed.Actions() {
				verbs = append(verbs, a.GetVerb()+" "+a.GetResource().Resource+"/"+a.GetSubresource())
			}
			if want := []string{"get " + kind.res + "/scale", "update " + kind.res + "/scale"}; !reflect.DeepEqual(verbs, want) {
				t.Errorf("sent %v, want %v", verbs, want)
			}
			obj, err := f.typed.Tracker().Get(ref.Resource.GVR, "default", "web")
			if err != nil {
				t.Fatal(err)
			}
			if n := *reflect.ValueOf(obj).Elem().FieldByName("Spec").FieldByName("Replicas").Interface().(*int32); n != 4 {
				t.Errorf("replicas %d, want 4", n)
			}
		})
	}

	f := actionEnv(t, fullGrant, actionScaleSubresource, actionDeployment("default", "web", "app"), pod("default", "web-1", nil))
	if err := f.s.Scale(context.Background(), f.ref(t, "apps", "deployments", "default", "web"), -1); err == nil || !strings.Contains(err.Error(), "-1") {
		t.Errorf("-1: %v", err)
	}
	if err := f.s.Scale(context.Background(), f.ref(t, "", "pods", "default", "web-1"), 2); err == nil || err.Error() != "cannot scale pods/default/web-1" {
		t.Errorf("a pod: %v", err)
	}
	if a := f.sent(); len(a) != 0 {
		t.Errorf("sent %v", a)
	}
}

func TestRestart(t *testing.T) {
	for _, kind := range []struct{ kind, res string }{{"Deployment", "deployments"}, {"StatefulSet", "statefulsets"}, {"DaemonSet", "daemonsets"}} {
		t.Run(kind.res, func(t *testing.T) {
			f := actionEnv(t, fullGrant, actionAcceptPatches, actionWorkload(kind.kind, "default", "web", "app"))
			ref := f.ref(t, "apps", kind.res, "default", "web")
			if err := f.s.Restart(context.Background(), ref); err != nil {
				t.Fatal(err)
			}
			if p := actionPatches(f); len(p) == 1 && (p[0].GetResource() != ref.Resource.GVR || p[0].GetNamespace() != "default" || p[0].GetName() != "web") {
				t.Errorf("patched %v %s/%s", p[0].GetResource(), p[0].GetNamespace(), p[0].GetName())
			}
			body := actionOnePatch(t, f)
			stamp, ok := body["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)["kubectl.kubernetes.io/restartedAt"].(string)
			if !ok {
				t.Fatalf("patch %v", body)
			}
			want := map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"annotations": map[string]any{"kubectl.kubernetes.io/restartedAt": stamp}}}}}
			if !reflect.DeepEqual(body, want) {
				t.Errorf("patch %v holds more than the annotation", body)
			}
			at, err := time.Parse(time.RFC3339, stamp)
			if err != nil {
				t.Fatalf("%q is not RFC 3339: %v", stamp, err)
			}
			if d := time.Since(at); d < -time.Minute || d > time.Minute {
				t.Errorf("restartedAt %v is not now", at)
			}
		})
	}

	f := actionEnv(t, fullGrant, actionAcceptPatches)
	if err := f.s.Restart(context.Background(), f.ref(t, "batch", "jobs", "default", "once")); err == nil || err.Error() != "cannot restart jobs/default/once" {
		t.Errorf("a job: %v", err)
	}
	if a := f.sent(); len(a) != 0 {
		t.Errorf("sent %v", a)
	}
}

func TestSetImages(t *testing.T) {
	withInit := func() runtime.Object {
		d := actionDeployment("default", "web", "app", "side")
		d.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "init:1"}}
		return d
	}
	tests := []struct {
		name   string
		images map[string]string
		want   map[string]any
	}{
		{"one of two containers", map[string]string{"side": "side:2"},
			map[string]any{"metadata": map[string]any{"resourceVersion": "7"}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": []any{map[string]any{"name": "side", "image": "side:2"}},
			}}}}},
		{"an init container", map[string]string{"init": "init:2"},
			map[string]any{"metadata": map[string]any{"resourceVersion": "7"}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"initContainers": []any{map[string]any{"name": "init", "image": "init:2"}},
			}}}}},
		{"both", map[string]string{"init": "init:2", "app": "app:2", "side": "side:2"},
			map[string]any{"metadata": map[string]any{"resourceVersion": "7"}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers":     []any{map[string]any{"name": "app", "image": "app:2"}, map[string]any{"name": "side", "image": "side:2"}},
				"initContainers": []any{map[string]any{"name": "init", "image": "init:2"}},
			}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := actionEnv(t, fullGrant, actionAcceptPatches, withInit())
			ref := f.ref(t, "apps", "deployments", "default", "web")
			if err := f.s.SetImages(context.Background(), ref, tt.images); err != nil {
				t.Fatal(err)
			}
			if body := actionOnePatch(t, f); !reflect.DeepEqual(body, tt.want) {
				t.Errorf("patch %v, want %v", body, tt.want)
			}
		})
	}

	t.Run("a container that is not there", func(t *testing.T) {
		f := actionEnv(t, fullGrant, actionAcceptPatches, withInit())
		ref := f.ref(t, "apps", "deployments", "default", "web")
		err := f.s.SetImages(context.Background(), ref, map[string]string{"app": "app:2", "nope": "nope:2"})
		if err == nil || !strings.Contains(err.Error(), `"nope"`) {
			t.Errorf("%v, want an error that names the container", err)
		}
		if p := actionPatches(f); len(p) != 0 {
			t.Errorf("patched %v", p)
		}
	})

	t.Run("no images", func(t *testing.T) {
		f := actionEnv(t, fullGrant, actionAcceptPatches, withInit())
		if err := f.s.SetImages(context.Background(), f.ref(t, "apps", "deployments", "default", "web"), map[string]string{}); err != nil {
			t.Fatal(err)
		}
		if a := f.sent(); len(a) != 0 {
			t.Errorf("sent %v", a)
		}
	})
}

// actionRollbackObjects are a deployment web at revision 2 and the
// replicasets of its revisions 1 and 2, whose images are app:1 and app:2.
func actionRollbackObjects() (*appsv1.Deployment, *appsv1.ReplicaSet, *appsv1.ReplicaSet) {
	d := actionDeployment("default", "web", "app")
	d.Spec.Template.Spec.Containers[0].Image = "app:2"
	d.Annotations = map[string]string{"deployment.kubernetes.io/revision": "2"}
	rs := func(rev string) *appsv1.ReplicaSet {
		r := actionWorkload("ReplicaSet", "default", "web-"+rev, "app").(*appsv1.ReplicaSet)
		r.Labels = map[string]string{"app": "web", "pod-template-hash": rev}
		r.Annotations = map[string]string{"deployment.kubernetes.io/revision": rev}
		r.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "web", UID: d.UID, Controller: actionPtr(true)}}
		r.Spec.Selector = &metav1.LabelSelector{MatchLabels: r.Labels}
		r.Spec.Template = actionTemplate(map[string]string{"app": "web", "pod-template-hash": rev}, "app")
		r.Spec.Template.Spec.Containers[0].Image = "app:" + rev
		return r
	}
	return d, rs("1"), rs("2")
}

func TestRollback(t *testing.T) {
	d, rs1, rs2 := actionRollbackObjects()
	f := actionEnv(t, fullGrant, nil, d, rs1, rs2)
	if err := f.s.Rollback(context.Background(), f.ref(t, "apps", "replicasets", "default", "web-1")); err != nil {
		t.Fatal(err)
	}
	obj, err := f.typed.Tracker().Get(f.resource(t, "apps", "deployments").GVR, "default", "web")
	if err != nil {
		t.Fatal(err)
	}
	got := obj.(*appsv1.Deployment)
	if img := got.Spec.Template.Spec.Containers[0].Image; img != "app:1" {
		t.Errorf("image %q, want app:1, the first revision's", img)
	}
	if _, ok := got.Spec.Template.Labels["pod-template-hash"]; ok {
		t.Errorf("template labels %v", got.Spec.Template.Labels)
	}

	t.Run("no owner", func(t *testing.T) {
		d, rs1, _ := actionRollbackObjects()
		rs1.OwnerReferences = nil
		f := actionEnv(t, fullGrant, nil, d, rs1)
		err := f.s.Rollback(context.Background(), f.ref(t, "apps", "replicasets", "default", "web-1"))
		if err == nil || !strings.Contains(err.Error(), "belongs to no deployment") {
			t.Errorf("%v", err)
		}
		for _, a := range f.typed.Actions() {
			if a.GetVerb() != "get" {
				t.Errorf("sent %v", a)
			}
		}
	})
	t.Run("no revision", func(t *testing.T) {
		d, rs1, _ := actionRollbackObjects()
		rs1.Annotations = nil
		f := actionEnv(t, fullGrant, nil, d, rs1)
		err := f.s.Rollback(context.Background(), f.ref(t, "apps", "replicasets", "default", "web-1"))
		if err == nil || !strings.Contains(err.Error(), "revision") {
			t.Errorf("%v", err)
		}
		for _, a := range f.typed.Actions() {
			if a.GetVerb() != "get" {
				t.Errorf("sent %v", a)
			}
		}
	})
	// actionNoPatch fails the test when the clientset fake was sent a
	// patch.
	actionNoPatch := func(t *testing.T, f *actionFixture) {
		t.Helper()
		for _, a := range f.typed.Actions() {
			if a.GetVerb() == "patch" {
				t.Errorf("sent %v", a)
			}
		}
	}
	t.Run("a deployment made again under the same name", func(t *testing.T) {
		d, rs1, rs2 := actionRollbackObjects()
		// The new deployment web is at its own revision 2 (app:3); its
		// revision 1 is web-2 (app:2). web-1 still names the old one.
		d.UID = "another-uid"
		d.Spec.Template.Spec.Containers[0].Image = "app:3"
		rs2.OwnerReferences[0].UID = "another-uid"
		rs2.Annotations["deployment.kubernetes.io/revision"] = "1"
		f := actionEnv(t, fullGrant, nil, d, rs1, rs2)
		err := f.s.Rollback(context.Background(), f.ref(t, "apps", "replicasets", "default", "web-1"))
		if err == nil || err.Error() != "replicaset default/web-1 belongs to a deployment that no longer exists" {
			t.Errorf("%v", err)
		}
		actionNoPatch(t, f)
	})
	t.Run("the deployment is gone", func(t *testing.T) {
		_, rs1, _ := actionRollbackObjects()
		f := actionEnv(t, fullGrant, nil, rs1)
		err := f.s.Rollback(context.Background(), f.ref(t, "apps", "replicasets", "default", "web-1"))
		if err == nil || err.Error() != "replicaset default/web-1 belongs to a deployment that no longer exists" {
			t.Errorf("%v", err)
		}
	})
	t.Run("the revision the deployment is at", func(t *testing.T) {
		d, rs1, rs2 := actionRollbackObjects()
		f := actionEnv(t, fullGrant, nil, d, rs1, rs2)
		err := f.s.Rollback(context.Background(), f.ref(t, "apps", "replicasets", "default", "web-2"))
		if err == nil || !strings.Contains(err.Error(), "skipped rollback") || !strings.Contains(err.Error(), "default/web") {
			t.Errorf("%v, want the rollbacker's word that it skipped the rollback", err)
		}
		actionNoPatch(t, f)
	})
	t.Run("a deployment", func(t *testing.T) {
		f := actionEnv(t, fullGrant, nil, actionDeployment("default", "web", "app"))
		err := f.s.Rollback(context.Background(), f.ref(t, "apps", "deployments", "default", "web"))
		if err == nil || err.Error() != "cannot roll back to deployments/default/web" {
			t.Errorf("%v", err)
		}
		if a := f.sent(); len(a) != 0 {
			t.Errorf("sent %v", a)
		}
	})
}

func actionCronJob(namespace, name string) *batchv1.CronJob {
	return &batchv1.CronJob{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(name + "-uid"), Labels: map[string]string{"of": "cronjob"}},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"team": "night"}, Annotations: map[string]string{"note": "from the template"}},
				Spec: batchv1.JobSpec{
					BackoffLimit: actionPtr[int32](2),
					Template:     actionTemplate(map[string]string{"team": "night"}, "task"),
				},
			},
		},
	}
}

func TestTrigger(t *testing.T) {
	cj := actionCronJob("default", "nightly")
	f := actionEnv(t, fullGrant, nil, cj)
	name, err := f.s.Trigger(context.Background(), f.ref(t, "batch", "cronjobs", "default", "nightly"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^nightly-manual-[a-z0-9]{5}$`).MatchString(name) {
		t.Errorf("name %q", name)
	}
	obj, err := f.typed.Tracker().Get(f.resource(t, "batch", "jobs").GVR, "default", name)
	if err != nil {
		t.Fatalf("the job: %v", err)
	}
	job := obj.(*batchv1.Job)
	if !reflect.DeepEqual(job.Spec, cj.Spec.JobTemplate.Spec) {
		t.Errorf("spec %+v, want the template's", job.Spec)
	}
	if !reflect.DeepEqual(job.Labels, cj.Spec.JobTemplate.Labels) {
		t.Errorf("labels %v, want the template's", job.Labels)
	}
	if want := map[string]string{"note": "from the template", "cronjob.kubernetes.io/instantiate": "manual"}; !reflect.DeepEqual(job.Annotations, want) {
		t.Errorf("annotations %v, want %v", job.Annotations, want)
	}
	owner := metav1.GetControllerOf(job)
	if owner == nil || owner.APIVersion != "batch/v1" || owner.Kind != "CronJob" || owner.Name != "nightly" || owner.UID != cj.UID {
		t.Errorf("owner references %+v", job.OwnerReferences)
	}

	long := strings.Repeat("a", 60)
	f = actionEnv(t, fullGrant, nil, actionCronJob("default", long))
	name, err = f.s.Trigger(context.Background(), f.ref(t, "batch", "cronjobs", "default", long))
	if err != nil {
		t.Fatal(err)
	}
	if len(name) > 63 || !regexp.MustCompile(`^a+-manual-[a-z0-9]{5}$`).MatchString(name) {
		t.Errorf("name %q, of %d characters", name, len(name))
	}
	if _, err := f.typed.Tracker().Get(f.resource(t, "batch", "jobs").GVR, "default", name); err != nil {
		t.Errorf("the job: %v", err)
	}
}

func TestSetSuspended(t *testing.T) {
	f := actionEnv(t, fullGrant, nil, actionCronJob("default", "nightly"))
	ref := f.ref(t, "batch", "cronjobs", "default", "nightly")
	for _, on := range []bool{true, false} {
		if err := f.s.SetSuspended(context.Background(), ref, on); err != nil {
			t.Fatal(err)
		}
		obj, err := f.typed.Tracker().Get(ref.Resource.GVR, "default", "nightly")
		if err != nil {
			t.Fatal(err)
		}
		if s := obj.(*batchv1.CronJob).Spec.Suspend; s == nil || *s != on {
			t.Errorf("suspend %v, want %v", s, on)
		}
	}
}

func TestWrongKindIsRefusedBeforeAnythingIsSent(t *testing.T) {
	f := actionEnv(t, fullGrant, nil, actionConfigMap("default", "settings"))
	ctx := context.Background()
	ref := f.ref(t, "", "configmaps", "default", "settings")
	tests := []struct {
		verb string
		call func() error
	}{
		{"kill", func() error { return f.s.Kill(ctx, ref) }},
		{"scale", func() error { return f.s.Scale(ctx, ref, 1) }},
		{"restart", func() error { return f.s.Restart(ctx, ref) }},
		{"set the images of", func() error { return f.s.SetImages(ctx, ref, map[string]string{"app": "app:2"}) }},
		{"roll back to", func() error { return f.s.Rollback(ctx, ref) }},
		{"trigger", func() error { _, err := f.s.Trigger(ctx, ref); return err }},
		{"suspend", func() error { return f.s.SetSuspended(ctx, ref, true) }},
		{"resume", func() error { return f.s.SetSuspended(ctx, ref, false) }},
		{"cordon", func() error { return f.s.Cordon(ctx, ref, true) }},
		{"uncordon", func() error { return f.s.Cordon(ctx, ref, false) }},
		{"drain", func() error { return f.s.Drain(ctx, ref, DefaultDrainOptions()) }},
	}
	for _, tt := range tests {
		err := tt.call()
		if want := "cannot " + tt.verb + " configmaps/default/settings"; err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", tt.verb, err, want)
		}
	}
	if a := f.sent(); len(a) != 0 {
		t.Errorf("sent %v", a)
	}
}

func TestBadReferenceIsRefusedBeforeAnythingIsSent(t *testing.T) {
	ctx := context.Background()
	type call func(s *session, ref ObjectRef) error
	methods := []struct {
		name       string
		group, res string // a resource the method is for
		namespace  string
		call       call
	}{
		{"Delete", "", "secrets", "default", func(s *session, r ObjectRef) error { return s.Delete(ctx, r, DeleteOptions{}) }},
		{"Kill", "", "pods", "default", func(s *session, r ObjectRef) error { return s.Kill(ctx, r) }},
		{"Update", "", "secrets", "default", func(s *session, r ObjectRef) error {
			return s.Update(ctx, r, "apiVersion: v1\nkind: Secret\nmetadata: {name: secrets, namespace: default, resourceVersion: '1'}\n")
		}},
		{"Scale", "apps", "deployments", "default", func(s *session, r ObjectRef) error { return s.Scale(ctx, r, 2) }},
		{"Restart", "apps", "deployments", "default", func(s *session, r ObjectRef) error { return s.Restart(ctx, r) }},
		{"SetImages", "apps", "deployments", "default", func(s *session, r ObjectRef) error {
			return s.SetImages(ctx, r, map[string]string{"app": "app:2"})
		}},
		{"SetImages with no images", "apps", "deployments", "default", func(s *session, r ObjectRef) error {
			return s.SetImages(ctx, r, nil)
		}},
		{"Rollback", "apps", "replicasets", "default", func(s *session, r ObjectRef) error { return s.Rollback(ctx, r) }},
		{"Trigger", "batch", "cronjobs", "default", func(s *session, r ObjectRef) error { _, err := s.Trigger(ctx, r); return err }},
		{"SetSuspended", "batch", "cronjobs", "default", func(s *session, r ObjectRef) error { return s.SetSuspended(ctx, r, true) }},
		{"Cordon", "", "nodes", "", func(s *session, r ObjectRef) error { return s.Cordon(ctx, r, true) }},
		{"Drain", "", "nodes", "", func(s *session, r ObjectRef) error { return s.Drain(ctx, r, DefaultDrainOptions()) }},
	}
	refs := []struct {
		name string
		ref  func(t *testing.T, f *actionFixture, group, res, namespace string) ObjectRef
	}{
		{"the resource string .", func(t *testing.T, f *actionFixture, group, res, namespace string) ObjectRef {
			r := f.resource(t, group, res)
			r.GVR.Resource = "."
			return ObjectRef{Resource: r, Namespace: namespace, Name: "secrets"}
		}},
		{"a GVR discovery does not have", func(t *testing.T, f *actionFixture, group, res, namespace string) ObjectRef {
			r := f.resource(t, group, res)
			r.GVR.Version = "v9"
			return ObjectRef{Resource: r, Namespace: namespace, Name: "web"}
		}},
		{"a discovered GVR with another kind", func(t *testing.T, f *actionFixture, group, res, namespace string) ObjectRef {
			r := f.resource(t, group, res)
			r.Kind = "Secret"
			if res == "secrets" {
				r.Kind = "ConfigMap"
			}
			return ObjectRef{Resource: r, Namespace: namespace, Name: "web"}
		}},
		{"a discovered GVR with another scope", func(t *testing.T, f *actionFixture, group, res, namespace string) ObjectRef {
			r := f.resource(t, group, res)
			r.Namespaced = !r.Namespaced
			return ObjectRef{Resource: r, Namespace: namespace, Name: "web"}
		}},
	}
	for _, m := range methods {
		for _, rr := range refs {
			t.Run(m.name+"/"+rr.name, func(t *testing.T) {
				f := actionEnv(t, fullGrant, nil)
				ref := rr.ref(t, f, m.group, m.res, m.namespace)
				err := m.call(f.s, ref)
				if !errors.Is(err, ErrUnknownResource) {
					t.Errorf("%v, want ErrUnknownResource", err)
				}
				if a := f.sent(); len(a) != 0 {
					t.Errorf("sent %v", a)
				}
				if got, _ := auditOneAction(t, f).attr("error"); got != "no such resource" {
					t.Errorf("logged error %q", got)
				}
			})
		}
	}
}
