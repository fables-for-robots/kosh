package kube

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/fables-for-robots/kosh/render"
)

// startAPIServer starts kube-apiserver and etcd for one test, and stops
// them when it ends. It skips the test without KUBEBUILDER_ASSETS.
func startAPIServer(t *testing.T) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set: run scripts/envtest-assets.sh")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting the API server: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stopping the API server: %v", err)
		}
	})
	return cfg
}

// newEnvCluster makes a Cluster with New against cfg and the test
// renderers, with deployments and customresourcedefinitions besides
// pods and jobs, and closes it when the test ends.
func newEnvCluster(t *testing.T, cfg *rest.Config, opts Options) *Cluster {
	t.Helper()
	renderers := newTestRenderers()
	for _, gvr := range []schema.GroupVersionResource{render.Deployments, render.CustomResourceDefinitions} {
		renderers.extra[gvr] = &testRenderer{gvr: gvr}
	}
	opts.Renderers = renderers.For
	opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := New(ctx, cfg, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.zeroOf = testZero
	t.Cleanup(c.Close)
	return c
}

func TestRESTConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- name: here
  cluster: {server: "https://here.example:6443"}
- name: there
  cluster: {server: "https://there.example:6443"}
users:
- name: me
  user: {token: secret}
contexts:
- name: here
  context: {cluster: here, user: me}
- name: there
  context: {cluster: there, user: me}
current-context: there
`
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := RESTConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://there.example:6443" || cfg.BearerToken != "secret" {
		t.Errorf("host %q, token %q: not the current context's", cfg.Host, cfg.BearerToken)
	}
	if _, err := RESTConfig(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a kubeconfig that is not there")
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := RESTConfig(""); err == nil {
		t.Error("the in-cluster config outside a cluster")
	}
}

func TestNewUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := New(ctx, &rest.Config{Host: "http://127.0.0.1:1"}, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil || !strings.Contains(err.Error(), "could not be reached") {
		t.Errorf("got %v, want an error that says the API could not be reached", err)
	}
}

func TestEnvNewReadsVersionAndResources(t *testing.T) {
	cfg := startAPIServer(t)
	c := newEnvCluster(t, cfg, Options{})
	s := c.Session(context.Background(), fullGrant)
	if v := s.ClusterVersion(); !strings.HasPrefix(v, "v1.") {
		t.Errorf("version %q", v)
	}
	rr := s.Resources()
	for _, want := range []schema.GroupVersionResource{render.Pods, render.Deployments, render.CustomResourceDefinitions} {
		r, ok := has(rr, want.Group, want.Resource)
		if !ok {
			t.Errorf("%v is not among the resources", want)
			continue
		}
		if !r.Typed || r.GVR != want {
			t.Errorf("%v: %+v, want typed", want, r)
		}
	}
	if _, ok := has(rr, "", "pods/log"); ok {
		t.Error("a subresource is a resource")
	}
	if _, ok := has(rr, "", "configmaps"); !ok {
		t.Error("configmaps is not a resource")
	}
	// The user agent and the limits of an interactive tool.
	if c.cfg.UserAgent != "kosh" || c.cfg.QPS != 50 || c.cfg.Burst != 100 {
		t.Errorf("user agent %q, QPS %v, burst %v", c.cfg.UserAgent, c.cfg.QPS, c.cfg.Burst)
	}
}

func TestEnvWatchPods(t *testing.T) {
	cfg := startAPIServer(t)
	c := newEnvCluster(t, cfg, Options{})
	typed := kubernetes.NewForConfigOrDie(cfg)
	ctx := context.Background()

	s := c.Session(ctx, fullGrant)
	pods, err := s.Resolve(ctx, "po")
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Watch(Query{Resource: pods, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	eventually(t, "the first list", func() bool { return !w.Table(time.Now()).Stale })

	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx"}}},
	}
	if _, err := typed.CoreV1().Pods("default").Create(ctx, p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rowsWhen(t, w, "default/web")
	zero := int64(0)
	if err := typed.CoreV1().Pods("default").Delete(ctx, "web", metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
		t.Fatal(err)
	}
	rowsWhen(t, w)
}

func TestEnvGenericTableOfACustomResource(t *testing.T) {
	cfg := startAPIServer(t)
	c := newEnvCluster(t, cfg, Options{TablePoll: 50 * time.Millisecond})
	ctx := context.Background()
	s := c.Session(ctx, fullGrant)

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "widgets", Singular: "widget", Kind: "Widget", ListKind: "WidgetList", ShortNames: []string{"wd"}},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"size":  {Type: "integer"},
							"since": {Type: "string", Format: "date-time"},
						}},
					},
				}},
				AdditionalPrinterColumns: []apiextensionsv1.CustomResourceColumnDefinition{
					{Name: "Size", Type: "integer", JSONPath: ".spec.size"},
					{Name: "Since", Type: "date", JSONPath: ".spec.since", Priority: 1},
				},
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
	since := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1", "kind": "Widget",
		"metadata": map[string]any{"name": "sprocket", "namespace": "default", "labels": map[string]any{"size": "big"}},
		"spec":     map[string]any{"size": int64(7), "since": since},
	}}
	eventually(t, "the widget is created", func() bool {
		_, err := c.dyn.Resource(gvr).Namespace("default").Create(ctx, widget, metav1.CreateOptions{})
		return err == nil || apierrors.IsAlreadyExists(err)
	})

	start := time.Now()
	r, err := s.Resolve(ctx, "widgets")
	if err != nil {
		t.Fatalf("Resolve of a resource made after New: %v", err)
	}
	if time.Since(start) > 5*time.Second || r.GVR != gvr || r.Typed || !r.Namespaced {
		t.Fatalf("Resolve: %+v after %v", r, time.Since(start))
	}

	w, err := s.Watch(Query{Resource: r})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	tab := rowsWhen(t, w, "default/sprocket")
	var cols []string
	for _, col := range tab.Columns {
		cols = append(cols, col.Name)
	}
	if strings.Join(cols, ",") != "NAMESPACE,NAME,SIZE,SINCE" {
		t.Fatalf("columns %v", cols)
	}
	if !tab.Columns[2].Numeric || !tab.Columns[3].Numeric || !tab.Columns[3].Wide {
		t.Errorf("columns %+v", tab.Columns)
	}
	row := tab.Rows[0]
	if row.Cells[2] != "7" || row.Order[2] != 7 || row.Cells[3] != "120m" || row.Labels["size"] != "big" {
		t.Errorf("row %+v", row)
	}
}

func TestEnvForbiddenList(t *testing.T) {
	cfg := startAPIServer(t)
	nobody := rest.CopyConfig(cfg)
	nobody.Impersonate = rest.ImpersonationConfig{UserName: "nobody", Groups: []string{"system:authenticated"}}
	c := newEnvCluster(t, nobody, Options{})
	s := c.Session(context.Background(), fullGrant)
	pods, err := s.Resolve(context.Background(), "pods")
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Watch(Query{Resource: pods})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var tab Table
	eventually(t, "the list's error", func() bool {
		tab = w.Table(time.Now())
		return tab.Err != nil
	})
	if !apierrors.IsForbidden(tab.Err) {
		t.Errorf("Err %v, want forbidden", tab.Err)
	}
}
