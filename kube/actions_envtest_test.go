package kube

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// actionAPI is a Cluster from New against an API server of the test's,
// a session of it with a full grant, and a client of the test's own.
type actionAPI struct {
	c     *Cluster
	s     Session
	typed kubernetes.Interface
	ext   apiextensions.Interface
}

func actionAPIServer(t *testing.T) *actionAPI {
	t.Helper()
	cfg := startAPIServer(t)
	c := newEnvCluster(t, cfg, Options{})
	if _, ok := c.cfg.WarningHandlerWithContext.(*apiWarnings); !ok {
		t.Errorf("New left the API's warnings to client-go: %T", c.cfg.WarningHandlerWithContext)
	}
	return &actionAPI{
		c:     c,
		s:     c.Session(context.Background(), fullGrant),
		typed: kubernetes.NewForConfigOrDie(cfg),
		ext:   apiextensions.NewForConfigOrDie(cfg),
	}
}

// ref returns the ObjectRef of name in namespace of the resource that
// answers to resource.
func (a *actionAPI) ref(t *testing.T, resource, namespace, name string) ObjectRef {
	t.Helper()
	r, err := a.s.Resolve(context.Background(), resource)
	if err != nil {
		t.Fatal(err)
	}
	return ObjectRef{Resource: r, Namespace: namespace, Name: name}
}

func TestEnvScaleRestartSetImages(t *testing.T) {
	a := actionAPIServer(t)
	ctx := context.Background()
	d := actionDeployment("default", "web", "app", "side")
	d.UID, d.ResourceVersion = "", ""
	before, err := a.typed.AppsV1().Deployments("default").Create(ctx, d, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ref := a.ref(t, "deployments", "default", "web")

	if err := a.s.Scale(ctx, ref, 3); err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if err := a.s.Restart(ctx, ref); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if err := a.s.SetImages(ctx, ref, map[string]string{"side": "side:2"}); err != nil {
		t.Fatalf("SetImages: %v", err)
	}

	after, err := a.typed.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n := *after.Spec.Replicas; n != 3 {
		t.Errorf("replicas %d, want 3", n)
	}
	stamp := after.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"]
	if at, err := time.Parse(time.RFC3339, stamp); err != nil || time.Since(at) > time.Minute {
		t.Errorf("restartedAt %q", stamp)
	}
	images := map[string]string{}
	for _, c := range after.Spec.Template.Spec.Containers {
		images[c.Name] = c.Image
	}
	if images["app"] != "app:1" || images["side"] != "side:2" || len(images) != 2 {
		t.Errorf("images %v", images)
	}

	// Nothing else of the spec changed.
	want := before.Spec.DeepCopy()
	want.Replicas = after.Spec.Replicas
	want.Template.Annotations = map[string]string{"kubectl.kubernetes.io/restartedAt": stamp}
	want.Template.Spec.Containers[1].Image = "side:2"
	if !apiequality.Semantic.DeepEqual(*want, after.Spec) {
		t.Errorf("spec\n%+v\nwant\n%+v", after.Spec, *want)
	}

	// The images' patch with the resourceVersion of the object as it was
	// before the changes above is a conflict, and changes nothing.
	err = a.s.(*session).patchTemplate(ctx, ref, before.ResourceVersion, map[string]any{"spec": map[string]any{
		"containers": []any{map[string]any{"name": "side", "image": "side:3"}},
	}})
	if !apierrors.IsConflict(err) {
		t.Errorf("a patch with a stale resourceVersion: %v, want a conflict", err)
	}
	if got, _ := a.typed.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{}); got.Spec.Template.Spec.Containers[1].Image != "side:2" {
		t.Errorf("image %q after the conflict", got.Spec.Template.Spec.Containers[1].Image)
	}
}

func TestEnvUpdateAndConflict(t *testing.T) {
	a := actionAPIServer(t)
	ctx := context.Background()
	d := actionDeployment("default", "web", "app")
	d.UID, d.ResourceVersion = "", ""
	if _, err := a.typed.AppsV1().Deployments("default").Create(ctx, d, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ref := a.ref(t, "deployments", "default", "web")
	u, err := a.c.dyn.Resource(ref.Resource.GVR).Namespace("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(u.Object, "edited", "metadata", "labels", "touched"); err != nil {
		t.Fatal(err)
	}
	b, err := yaml.Marshal(u.Object)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.s.Update(ctx, ref, string(b)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := a.typed.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["touched"] != "edited" {
		t.Errorf("labels %v", got.Labels)
	}
	if err := a.s.Update(ctx, ref, string(b)); !apierrors.IsConflict(err) {
		t.Errorf("the same YAML again: %v, want a conflict", err)
	}
}

func TestEnvDeleteGeneric(t *testing.T) {
	a := actionAPIServer(t)
	ctx := context.Background()
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "widgets", Singular: "widget", Kind: "Widget", ListKind: "WidgetList"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"size": {Type: "integer"}}},
					},
				}},
			}},
		},
	}
	if _, err := a.ext.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	gvr := schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
	eventually(t, "the widget is created", func() bool {
		_, err := a.c.dyn.Resource(gvr).Namespace("default").Create(ctx, actionWidget("default", "sprocket"), metav1.CreateOptions{})
		return err == nil || apierrors.IsAlreadyExists(err)
	})

	ref := a.ref(t, "widgets", "default", "sprocket")
	if err := a.s.Delete(ctx, ref, DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := a.c.dyn.Resource(gvr).Namespace("default").Get(ctx, "sprocket", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("after the delete: %v, want not found", err)
	}
}

func TestEnvTriggerAndSuspend(t *testing.T) {
	a := actionAPIServer(t)
	ctx := context.Background()
	cj := actionCronJob("default", "nightly")
	cj.UID = ""
	cj.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	created, err := a.typed.BatchV1().CronJobs("default").Create(ctx, cj, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ref := a.ref(t, "cronjobs", "default", "nightly")

	name, err := a.s.Trigger(ctx, ref)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	job, err := a.typed.BatchV1().Jobs("default").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the job %q: %v", name, err)
	}
	if owner := metav1.GetControllerOf(job); owner == nil || owner.UID != created.UID || owner.Kind != "CronJob" {
		t.Errorf("owner references %+v", job.OwnerReferences)
	}
	if job.Annotations["cronjob.kubernetes.io/instantiate"] != "manual" {
		t.Errorf("annotations %v", job.Annotations)
	}

	for _, on := range []bool{true, false} {
		if err := a.s.SetSuspended(ctx, ref, on); err != nil {
			t.Fatalf("SetSuspended(%v): %v", on, err)
		}
		got, err := a.typed.BatchV1().CronJobs("default").Get(ctx, "nightly", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Spec.Suspend == nil || *got.Spec.Suspend != on {
			t.Errorf("suspend %v, want %v", got.Spec.Suspend, on)
		}
	}
}

func TestEnvCordon(t *testing.T) {
	a := actionAPIServer(t)
	ctx := context.Background()
	n := actionNode("n1")
	n.UID = ""
	if _, err := a.typed.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ref := a.ref(t, "nodes", "", "n1")
	for _, on := range []bool{true, false} {
		if err := a.s.Cordon(ctx, ref, on); err != nil {
			t.Fatalf("Cordon(%v): %v", on, err)
		}
		got, err := a.typed.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Spec.Unschedulable != on {
			t.Errorf("unschedulable %v, want %v", got.Spec.Unschedulable, on)
		}
	}
}
