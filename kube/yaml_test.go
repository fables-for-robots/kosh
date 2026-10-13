package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/fables-for-robots/kosh/keys"
)

// readSessionOf returns a session of e's Cluster with grant, which ends
// with the test.
func readSessionOf(t *testing.T, e *env, grant keys.Grant) *session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return e.c.Session(ctx, grant).(*session)
}

// readActionsOn returns the actions on resource that either fake
// recorded.
func readActionsOn(e *env, resource string) []k8stesting.Action {
	var out []k8stesting.Action
	for _, a := range append(e.dyn.Actions(), e.typed.Actions()...) {
		if a.GetResource().Resource == resource {
			out = append(out, a)
		}
	}
	return out
}

func readConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "default",
			Name:              "settings",
			ResourceVersion:   "42",
			CreationTimestamp: metav1.NewTime(time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)),
			Labels:            map[string]string{"app": "web"},
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager:    "kubectl",
				Operation:  metav1.ManagedFieldsOperationApply,
				APIVersion: "v1",
				FieldsType: "FieldsV1",
			}},
		},
		Data: map[string]string{"colour": "blue", "size": "large"},
	}
}

func readSecret() *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "token"},
		Data:       map[string][]byte{"token": []byte("s3cr3t")},
	}
}

func TestYAML(t *testing.T) {
	cm := readConfigMap()
	e := newEnv(t, envConfig{}, cm)
	s := e.session(t)
	ref := ObjectRef{Resource: e.resource(t, "", "configmaps"), Namespace: "default", Name: "settings"}

	text, err := s.YAML(context.Background(), ref, false)
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	var got corev1.ConfigMap
	if err := yaml.UnmarshalStrict([]byte(text), &got); err != nil {
		t.Fatalf("the YAML does not parse back: %v\n%s", err, text)
	}
	want := cm.DeepCopy()
	want.ManagedFields = nil
	if !equality.Semantic.DeepEqual(&got, want) {
		t.Errorf("the YAML is not the object:\ngot  %+v\nwant %+v", got, *want)
	}
	if got.ResourceVersion != "42" {
		t.Errorf("resourceVersion %q, want 42", got.ResourceVersion)
	}
	if strings.Contains(text, "managedFields") {
		t.Errorf("managedFields are in the YAML without being asked for:\n%s", text)
	}

	text, err = s.YAML(context.Background(), ref, true)
	if err != nil {
		t.Fatalf("YAML with managedFields: %v", err)
	}
	got = corev1.ConfigMap{}
	if err := yaml.UnmarshalStrict([]byte(text), &got); err != nil {
		t.Fatalf("the YAML does not parse back: %v", err)
	}
	if !equality.Semantic.DeepEqual(&got, cm) {
		t.Errorf("the YAML with managedFields is not the object:\ngot  %+v\nwant %+v", got, *cm)
	}
	if len(got.ManagedFields) != 1 {
		t.Errorf("managedFields %+v, want those of the object", got.ManagedFields)
	}

	ref.Name = "missing"
	if _, err := s.YAML(context.Background(), ref, false); err == nil {
		t.Error("YAML of an object that is not there: no error")
	}
}

func TestYAMLOfASecretIsRefusedReadOnly(t *testing.T) {
	e := newEnv(t, envConfig{}, readSecret())
	ref := ObjectRef{Resource: e.resource(t, "", "secrets"), Namespace: "default", Name: "token"}

	ro := readSessionOf(t, e, readOnlyGrant)
	if _, err := ro.YAML(context.Background(), ref, false); !errors.Is(err, ErrReadOnly) {
		t.Errorf("read-only: got %v, want ErrReadOnly", err)
	}
	if aa := readActionsOn(e, "secrets"); len(aa) != 0 {
		t.Errorf("read-only: the Secret was read: %v", aa)
	}

	text, err := e.session(t).YAML(context.Background(), ref, false)
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	var got corev1.Secret
	if err := yaml.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if string(got.Data["token"]) != "s3cr3t" {
		t.Errorf("full: the YAML holds %q, want the Secret's data", got.Data)
	}
}

func TestDescribeOfASecretIsRefusedReadOnly(t *testing.T) {
	e := newEnv(t, envConfig{}, readSecret())
	ref := ObjectRef{Resource: e.resource(t, "", "secrets"), Namespace: "default", Name: "token"}

	ro := readSessionOf(t, e, readOnlyGrant)
	if _, err := ro.Describe(context.Background(), ref); !errors.Is(err, ErrReadOnly) {
		t.Errorf("read-only: got %v, want ErrReadOnly", err)
	}
	if aa := readActionsOn(e, "secrets"); len(aa) != 0 {
		t.Errorf("read-only: the Secret was read: %v", aa)
	}

	// Without a config, a full grant gets as far as the describer, which
	// says that it needs one; TestEnvDescribeSecretShowsNoValue describes
	// a Secret for real.
	_, err := e.session(t).Describe(context.Background(), ref)
	if err == nil || errors.Is(err, ErrReadOnly) || !strings.Contains(err.Error(), "connection") {
		t.Errorf("full, without a config: got %v, want the error that describe needs a connection", err)
	}
}

// readActionCounts returns how many actions each fake has recorded.
func readActionCounts(e *env) (int, int) {
	return len(e.dyn.Actions()), len(e.typed.Actions())
}

func TestASecretByItsKindIsRefusedReadOnly(t *testing.T) {
	e := newEnv(t, envConfig{}, readSecret(), readConfigMap())
	// A configmap's resource that says it is of the kind Secret: the
	// Secret's describer would be chosen by the kind.
	cm := e.resource(t, "", "configmaps")
	cm.Kind = "Secret"
	ref := ObjectRef{Resource: cm, Namespace: "default", Name: "settings"}
	ro := readSessionOf(t, e, readOnlyGrant)
	dyn, typed := readActionCounts(e)
	if _, err := ro.YAML(context.Background(), ref, false); !errors.Is(err, ErrReadOnly) {
		t.Errorf("YAML: got %v, want ErrReadOnly", err)
	}
	if _, err := ro.Describe(context.Background(), ref); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Describe: got %v, want ErrReadOnly", err)
	}
	if d, ty := readActionCounts(e); d != dyn || ty != typed {
		t.Errorf("something was read: %v %v", e.dyn.Actions()[dyn:], e.typed.Actions()[typed:])
	}
}

func TestAResourceThatIsAPathIsRefused(t *testing.T) {
	e := newEnv(t, envConfig{}, readSecret(), readConfigMap(), readPodWith("default", "web", nil, "app"))
	f := &readFakeLogs{}
	e.c.openLog = f.open
	ctx := context.Background()
	bad := func(resource string) Resource {
		r := e.resource(t, "", "configmaps")
		r.GVR.Resource = resource
		return r
	}
	for _, res := range []Resource{bad("configmaps/../secrets"), bad("../secrets"), bad(".."), bad("")} {
		ref := ObjectRef{Resource: res, Namespace: "default", Name: "token"}
		for _, g := range []struct {
			name string
			s    *session
		}{{"read-only", readSessionOf(t, e, readOnlyGrant)}, {"full", e.session(t)}} {
			dyn, typed := readActionCounts(e)
			calls := map[string]error{}
			_, calls["YAML"] = g.s.YAML(ctx, ref, false)
			_, calls["Describe"] = g.s.Describe(ctx, ref)
			_, calls["Owner"] = g.s.Owner(ctx, ref)
			_, calls["Images"] = g.s.Images(ctx, ref)
			_, calls["Replicas"] = g.s.Replicas(ctx, ref)
			_, calls["Logs"] = g.s.Logs(ctx, ref, LogOptions{})
			_, calls["Watch Containers"] = g.s.Watch(Query{Resource: Containers, Of: &ref})
			_, calls["Watch Rules"] = g.s.Watch(Query{Resource: Rules, Of: &ref})
			if g.name == "full" {
				_, calls["SecretData"] = g.s.SecretData(ctx, ref)
			}
			for m, err := range calls {
				if !errors.Is(err, errNotAResource) {
					t.Errorf("%s, %s of %q: got %v, want errNotAResource", g.name, m, res.GVR.Resource, err)
				}
			}
			if d, ty := readActionCounts(e); d != dyn || ty != typed {
				t.Errorf("%s, %q: something was read: %v %v", g.name, res.GVR.Resource, e.dyn.Actions()[dyn:], e.typed.Actions()[typed:])
			}
		}
	}
	if len(f.calls()) != 0 {
		t.Errorf("a log was opened: %+v", f.calls())
	}
}

func TestDescribeConfig(t *testing.T) {
	cfg := &rest.Config{Host: "https://api.example:6443", QPS: 50}
	got := describeConfig(context.Background(), cfg)
	if got.Timeout != 30*time.Second || got.Host != cfg.Host || got.QPS != 50 {
		t.Errorf("without a deadline: timeout %v, host %q, QPS %v; want 30s and the rest as it was", got.Timeout, got.Host, got.QPS)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got = describeConfig(ctx, cfg)
	if got.Timeout <= 4*time.Second || got.Timeout > 5*time.Second {
		t.Errorf("with a deadline in 5s: timeout %v", got.Timeout)
	}
	if cfg.Timeout != 0 {
		t.Errorf("the Cluster's config was changed: timeout %v", cfg.Timeout)
	}
}
