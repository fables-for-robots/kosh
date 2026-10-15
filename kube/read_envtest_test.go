package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/fables-for-robots/kosh/render"
)

// readEnvResolve resolves name with s, and fails the test when it
// cannot.
func readEnvResolve(t *testing.T, s Session, name string) Resource {
	t.Helper()
	r, err := s.Resolve(context.Background(), name)
	if err != nil {
		t.Fatalf("Resolve %q: %v", name, err)
	}
	return r
}

// readEnvWidget makes the custom resource Widget of example.com, and one
// widget of it in default, named name.
func readEnvWidget(t *testing.T, cfg *rest.Config, c *Cluster, name string) {
	t.Helper()
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
						"spec": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"size": {Type: "integer"},
						}},
					},
				}},
			}},
		},
	}
	ext := apiextensions.NewForConfigOrDie(cfg)
	if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the CRD is established", func() bool {
		got, err := ext.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crd.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		for _, cond := range got.Status.Conditions {
			if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
				return true
			}
		}
		return false
	})
	gvr := schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1", "kind": "Widget",
		"metadata": map[string]any{"name": name, "namespace": "default"},
		"spec":     map[string]any{"size": int64(7)},
	}}
	eventually(t, "the widget is created", func() bool {
		_, err := c.dyn.Resource(gvr).Namespace("default").Create(ctx, widget, metav1.CreateOptions{})
		return err == nil || apierrors.IsAlreadyExists(err)
	})
}

func readEnvDeployment(name string) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx"}}},
			},
		},
	}
}

func TestEnvDescribe(t *testing.T) {
	cfg := startAPIServer(t)
	c := newEnvCluster(t, cfg, Options{})
	typed := kubernetes.NewForConfigOrDie(cfg)
	ctx := context.Background()
	s := c.Session(ctx, fullGrant)

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "settings"}, Data: map[string]string{"colour-of-the-door": "blue"}}
	if _, err := typed.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	text, err := s.Describe(ctx, ObjectRef{Resource: readEnvResolve(t, s, "configmaps"), Namespace: "default", Name: "settings"})
	if err != nil || !strings.Contains(text, "settings") || !strings.Contains(text, "colour-of-the-door") {
		t.Errorf("the configmap: %v\n%s", err, text)
	}

	if _, err := typed.AppsV1().Deployments("default").Create(ctx, readEnvDeployment("web"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	text, err = s.Describe(ctx, ObjectRef{Resource: readEnvResolve(t, s, "deployments"), Namespace: "default", Name: "web"})
	if err != nil || !strings.Contains(text, "Replicas:") {
		t.Errorf("the deployment: %v\n%s", err, text)
	}

	readEnvWidget(t, cfg, c, "sprocket")
	widgets := readEnvResolve(t, s, "widgets")
	text, err = s.Describe(ctx, ObjectRef{Resource: widgets, Namespace: "default", Name: "sprocket"})
	if err != nil || !strings.Contains(text, "sprocket") || !strings.Contains(text, "Widget") {
		t.Errorf("the widget: %v\n%s", err, text)
	}

	if _, err := s.Describe(ctx, ObjectRef{Resource: widgets, Namespace: "default", Name: "missing"}); err == nil {
		t.Error("an object that is not there: no error")
	}
}

func TestEnvDescribeOfAnOpaqueSecretShowsNoValue(t *testing.T) {
	cfg := startAPIServer(t)
	c := newEnvCluster(t, cfg, Options{})
	typed := kubernetes.NewForConfigOrDie(cfg)
	ctx := context.Background()
	const value = "correct-horse-battery-staple"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "db"}, Data: map[string][]byte{"db-password": []byte(value)}}
	if _, err := typed.CoreV1().Secrets("default").Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	s := c.Session(ctx, fullGrant)
	text, err := s.Describe(ctx, ObjectRef{Resource: readEnvResolve(t, s, "secrets"), Namespace: "default", Name: "db"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "db-password") || strings.Contains(text, value) {
		t.Errorf("the description holds the key %v, the value %v; want the key only:\n%s",
			strings.Contains(text, "db-password"), strings.Contains(text, value), text)
	}
}

func TestEnvYAMLRoundTrip(t *testing.T) {
	cfg := startAPIServer(t)
	c := newEnvCluster(t, cfg, Options{})
	typed := kubernetes.NewForConfigOrDie(cfg)
	ctx := context.Background()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "settings", Labels: map[string]string{"app": "web"}},
		Data:       map[string]string{"colour": "blue", "size": "large"},
	}
	if _, err := typed.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	s := c.Session(ctx, fullGrant)
	text, err := s.YAML(ctx, ObjectRef{Resource: readEnvResolve(t, s, "configmaps"), Namespace: "default", Name: "settings"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var got corev1.ConfigMap
	if err := yaml.UnmarshalStrict([]byte(text), &got); err != nil {
		t.Fatalf("the YAML does not parse: %v\n%s", err, text)
	}
	want, err := typed.CoreV1().ConfigMaps("default").Get(ctx, "settings", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(want.ManagedFields) == 0 {
		t.Fatal("the API server keeps no managedFields: the test shows nothing")
	}
	want.ManagedFields = nil
	want.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
	if !equality.Semantic.DeepEqual(&got, want) {
		t.Errorf("the YAML\n%+v\nis not the object less managed fields\n%+v", got, *want)
	}
}

func TestEnvOwner(t *testing.T) {
	cfg := startAPIServer(t)
	c := newEnvCluster(t, cfg, Options{})
	typed := kubernetes.NewForConfigOrDie(cfg)
	ctx := context.Background()
	d, err := typed.AppsV1().Deployments("default").Create(ctx, readEnvDeployment("web"), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	controller := true
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "web-123",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name, UID: d.UID, Controller: &controller}},
		},
		Spec: appsv1.ReplicaSetSpec{Selector: d.Spec.Selector, Template: d.Spec.Template},
	}
	if _, err := typed.AppsV1().ReplicaSets("default").Create(ctx, rs, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	s := c.Session(ctx, fullGrant)
	got, err := s.Owner(ctx, ObjectRef{Resource: readEnvResolve(t, s, "replicasets"), Namespace: "default", Name: "web-123"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (ObjectRef{Resource: readEnvResolve(t, s, "deployments"), Namespace: "default", Name: "web"}); got.String() != want.String() || got.Resource.Kind != "Deployment" {
		t.Errorf("owner %s (%s), want %s", got, got.Resource.Kind, want)
	}
}

// Ordinary references, as discovery made them, pass checkRef and are
// read.
func TestEnvOrdinaryReferencesAreRead(t *testing.T) {
	cfg := startAPIServer(t)
	c := newEnvCluster(t, cfg, Options{})
	typed := kubernetes.NewForConfigOrDie(cfg)
	ctx := context.Background()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx"}}},
	}
	if _, err := typed.CoreV1().Pods("default").Create(ctx, p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := typed.AppsV1().Deployments("default").Create(ctx, readEnvDeployment("web"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	readEnvWidget(t, cfg, c, "sprocket")
	s := c.Session(ctx, fullGrant)
	pod := ObjectRef{Resource: readEnvResolve(t, s, "pods"), Namespace: "default", Name: "web"}
	deploy := ObjectRef{Resource: readEnvResolve(t, s, "deployments"), Namespace: "default", Name: "web"}
	widget := ObjectRef{Resource: readEnvResolve(t, s, "widgets"), Namespace: "default", Name: "sprocket"}

	for _, ref := range []ObjectRef{pod, deploy, widget} {
		if text, err := s.YAML(ctx, ref, false); err != nil || !strings.Contains(text, ref.Name) {
			t.Errorf("YAML of %s: %v", ref, err)
		}
		if text, err := s.Describe(ctx, ref); err != nil || !strings.Contains(text, ref.Name) {
			t.Errorf("Describe of %s: %v", ref, err)
		}
		if _, err := s.Owner(ctx, ref); !errors.Is(err, ErrNoOwner) {
			t.Errorf("Owner of %s: %v, want ErrNoOwner", ref, err)
		}
	}
	if images, err := s.Images(ctx, deploy); err != nil || len(images) != 1 || images[0].Image != "nginx" {
		t.Errorf("Images: %+v, %v", images, err)
	}
	if n, err := s.Replicas(ctx, deploy); err != nil || n != 1 {
		t.Errorf("Replicas: %d, %v; want the default 1", n, err)
	}
	// Without a kubelet the log cannot be opened: the stream says so.
	for _, ref := range []ObjectRef{pod, deploy} {
		ls, err := s.Logs(ctx, ref, LogOptions{})
		if err != nil {
			t.Errorf("Logs of %s: %v", ref, err)
			continue
		}
		readCollect(t, ls)
		ls.Close()
	}
}

// The API server of envtest serves no metrics API, as a cluster without
// metrics-server: the tables are those of stage 1, and the header has no
// usage.
func TestNoMetricsAPIOnEnvtest(t *testing.T) {
	cfg := startAPIServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// render's renderers, whose pods and nodes are metered, not the
	// tests' own.
	c, err := New(ctx, cfg, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(c.Close)
	typed := kubernetes.NewForConfigOrDie(cfg)
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx"}}},
	}
	if _, err := typed.CoreV1().Pods("default").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	s := c.Session(context.Background(), fullGrant)
	w, err := s.Watch(Query{Resource: readEnvResolve(t, s, "pods")})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var tab Table
	eventually(t, "the pod's row", func() bool {
		tab = w.Table(time.Now())
		return len(tab.Rows) == 1 && !tab.Stale && tab.Err == nil
	})
	if column(tab, "CPU") >= 0 || column(tab, "MEM") >= 0 {
		t.Errorf("columns %v without the metrics API", tab.Columns)
	}
	if column(tab, "IP") < 0 {
		t.Errorf("columns %v: not the pods' renderer's", tab.Columns)
	}
	whole(t, tab)

	u := s.ClusterUsage()
	defer u.Close()
	if got, ok := u.Current(); ok || got.Served {
		t.Errorf("ClusterUsage %+v, %v without the metrics API", got, ok)
	}
	// The cluster has nodes kosh can read: the watch found no metrics
	// API, and so holds neither the nodes' cache nor a poller.
	if r, ok := c.knownResource(render.Nodes); !ok || !r.Typed {
		t.Errorf("nodes %+v, %v: not a kind kosh reads, and the test shows nothing", r, ok)
	}
	if c.running(render.Nodes) {
		t.Error("the usage watch holds the nodes' cache without the metrics API")
	}
	if n := c.pollerCount(); n != 0 {
		t.Errorf("%d pollers without the metrics API", n)
	}
}
