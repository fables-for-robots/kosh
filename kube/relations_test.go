package kube

import (
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func selector(app string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}}
}

func objMeta(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name}
}

func TestPodsOfAWorkload(t *testing.T) {
	objs := []k8sruntime.Object{
		&appsv1.Deployment{ObjectMeta: objMeta("ns", "deploy"), Spec: appsv1.DeploymentSpec{Selector: selector("deploy")}},
		&appsv1.StatefulSet{ObjectMeta: objMeta("ns", "sts"), Spec: appsv1.StatefulSetSpec{Selector: selector("sts")}},
		&appsv1.DaemonSet{ObjectMeta: objMeta("ns", "ds"), Spec: appsv1.DaemonSetSpec{Selector: selector("ds")}},
		&appsv1.ReplicaSet{ObjectMeta: objMeta("ns", "rs"), Spec: appsv1.ReplicaSetSpec{Selector: selector("rs")}},
		&batchv1.Job{ObjectMeta: objMeta("ns", "job"), Spec: batchv1.JobSpec{Selector: selector("job")}},
		&corev1.Service{ObjectMeta: objMeta("ns", "svc"), Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "svc"}}},
	}
	for _, app := range []string{"deploy", "sts", "ds", "rs", "job", "svc"} {
		objs = append(objs,
			pod("ns", app+"-1", map[string]string{"app": app}),
			pod("ns", app+"-2", map[string]string{"app": app, "tier": "web"}),
			pod("other", app+"-elsewhere", map[string]string{"app": app}),
		)
	}
	objs = append(objs, pod("ns", "unrelated", map[string]string{"app": "nothing"}), pod("ns", "bare", nil))
	e := newEnv(t, envConfig{}, objs...)
	s := e.session(t)
	pods := e.resource(t, "", "pods")

	for _, tc := range []struct{ group, plural, name string }{
		{"apps", "deployments", "deploy"},
		{"apps", "statefulsets", "sts"},
		{"apps", "daemonsets", "ds"},
		{"apps", "replicasets", "rs"},
		{"batch", "jobs", "job"},
		{"", "services", "svc"},
	} {
		t.Run(tc.plural, func(t *testing.T) {
			of := ObjectRef{Resource: e.resource(t, tc.group, tc.plural), Namespace: "ns", Name: tc.name}
			// The query's namespace is not looked at.
			w := watchOf(t, s, Query{Resource: pods, Namespace: "other", Of: &of})
			rowsWhen(t, w, "ns/"+tc.name+"-1", "ns/"+tc.name+"-2")
		})
	}
}

func TestPodsOfAServiceWithoutSelector(t *testing.T) {
	e := newEnv(t, envConfig{},
		&corev1.Service{ObjectMeta: objMeta("ns", "external")},
		pod("ns", "a", map[string]string{"app": "a"}), pod("ns", "b", nil))
	s := e.session(t)
	of := ObjectRef{Resource: e.resource(t, "", "services"), Namespace: "ns", Name: "external"}
	w := watchOf(t, s, Query{Resource: e.resource(t, "", "pods"), Of: &of})
	// The pods are there, and none of them is the service's.
	all := watchOf(t, s, Query{Resource: e.resource(t, "", "pods")})
	rowsWhen(t, all, "ns/a", "ns/b")
	rowsWhen(t, w)
}

func TestPodsOfANode(t *testing.T) {
	onNode := func(namespace, name, node string) *corev1.Pod {
		p := pod(namespace, name, nil)
		p.Spec.NodeName = node
		return p
	}
	e := newEnv(t, envConfig{},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}},
		onNode("one", "a", "n1"), onNode("two", "b", "n1"),
		onNode("one", "c", "n2"), onNode("two", "d", "n2"),
		onNode("one", "pending", ""),
	)
	of := ObjectRef{Resource: e.resource(t, "", "nodes"), Name: "n1"}
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods"), Namespace: "two", Of: &of})
	rowsWhen(t, w, "one/a", "two/b")
}

func TestJobsOfACronJob(t *testing.T) {
	const uid = types.UID("cron-uid")
	owned := func(namespace, name string, owner types.UID) *batchv1.Job {
		j := &batchv1.Job{ObjectMeta: objMeta(namespace, name)}
		if owner != "" {
			yes := true
			j.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "CronJob", Name: "nightly", UID: owner, Controller: &yes}}
		}
		return j
	}
	cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "nightly", UID: uid}}
	e := newEnv(t, envConfig{}, cron,
		owned("ns", "nightly-1", uid), owned("ns", "nightly-2", uid),
		owned("ns", "manual", ""), owned("ns", "other-owner", "someone-else"),
		owned("elsewhere", "nightly-x", uid),
	)
	of := ObjectRef{Resource: e.resource(t, "batch", "cronjobs"), Namespace: "ns", Name: "nightly"}
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "batch", "jobs"), Of: &of})
	rowsWhen(t, w, "ns/nightly-1", "ns/nightly-2")
}

func TestNoRelation(t *testing.T) {
	e := newEnv(t, envConfig{},
		&corev1.ConfigMap{ObjectMeta: objMeta("ns", "cm")},
		&appsv1.Deployment{ObjectMeta: objMeta("ns", "deploy"), Spec: appsv1.DeploymentSpec{Selector: selector("deploy")}},
	)
	s := e.session(t)
	pods, jobs, widgets := e.resource(t, "", "pods"), e.resource(t, "batch", "jobs"), e.resource(t, "example.com", "widgets")
	cm := ObjectRef{Resource: e.resource(t, "", "configmaps"), Namespace: "ns", Name: "cm"}
	deploy := ObjectRef{Resource: e.resource(t, "apps", "deployments"), Namespace: "ns", Name: "deploy"}
	for _, q := range []Query{
		{Resource: pods, Of: &cm},
		{Resource: jobs, Of: &deploy},
		{Resource: widgets, Of: &deploy},
	} {
		w, err := s.Watch(q)
		if !errors.Is(err, ErrNoRelation) {
			t.Errorf("%s of %s: %v, want ErrNoRelation", q.Resource.GVR.Resource, q.Of, err)
		}
		if w != nil {
			t.Errorf("%s of %s: a watch", q.Resource.GVR.Resource, q.Of)
		}
	}
}

func TestRelationOwnerMissing(t *testing.T) {
	e := newEnv(t, envConfig{})
	s := e.session(t)
	for _, of := range []ObjectRef{
		{Resource: e.resource(t, "apps", "deployments"), Namespace: "ns", Name: "gone"},
		{Resource: e.resource(t, "", "nodes"), Name: "gone"},
		{Resource: e.resource(t, "", "services"), Namespace: "ns", Name: "gone"},
	} {
		_, err := s.Watch(Query{Resource: e.resource(t, "", "pods"), Of: &of})
		if !apierrors.IsNotFound(err) {
			t.Errorf("pods of %s: %v, want not found", of, err)
		}
	}
	of := ObjectRef{Resource: e.resource(t, "batch", "cronjobs"), Namespace: "ns", Name: "gone"}
	if _, err := s.Watch(Query{Resource: e.resource(t, "batch", "jobs"), Of: &of}); !apierrors.IsNotFound(err) {
		t.Errorf("jobs of %s: %v, want not found", of, err)
	}
	if e.c.running(e.resource(t, "", "pods").GVR) {
		t.Error("a watch that failed left its informer acquired")
	}
}

func TestPseudoResourcesGoToWatchPseudo(t *testing.T) {
	e := newEnv(t, envConfig{})
	of := ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "ns", Name: "p"}
	for _, r := range []Resource{Containers, Rules} {
		if _, err := e.session(t).Watch(Query{Resource: r, Of: &of}); !errors.Is(err, ErrNoRelation) {
			t.Errorf("%s: %v, want the placeholder's ErrNoRelation", r.GVR.Resource, err)
		}
	}
}
