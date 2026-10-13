package kube

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// actionNodeUnschedulable returns spec.unschedulable of the node as the
// clientset fake holds it.
func actionNodeUnschedulable(t *testing.T, f *actionFixture, name string) bool {
	t.Helper()
	n, err := f.typed.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return n.Spec.Unschedulable
}

func TestCordonAndUncordon(t *testing.T) {
	f := actionEnv(t, fullGrant, nil, actionNode("n1"))
	ref := f.ref(t, "", "nodes", "", "n1")
	for _, on := range []bool{true, false, false, true} {
		if err := f.s.Cordon(context.Background(), ref, on); err != nil {
			t.Fatal(err)
		}
		if got := actionNodeUnschedulable(t, f, "n1"); got != on {
			t.Errorf("unschedulable %v, want %v", got, on)
		}
	}
}

// actionDrainObjects are a node n1 and, on it, a pod of a replicaset
// (plain), a pod of a daemonset (agent-x), and a pod of a replicaset
// with an emptyDir (scratch); and the daemonset.
func actionDrainObjects() []runtime.Object {
	onNode := func(name, ownerKind, owner string) *corev1.Pod {
		p := pod("default", name, nil)
		p.Spec.NodeName = "n1"
		p.Spec.Containers = []corev1.Container{{Name: "c", Image: "c:1"}}
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: ownerKind, Name: owner, UID: "owner-uid", Controller: actionPtr(true)}}
		return p
	}
	scratch := onNode("scratch", "ReplicaSet", "web-1")
	scratch.Spec.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	return []runtime.Object{
		actionNode("n1"),
		actionWorkload("DaemonSet", "default", "agent", "agent"),
		onNode("plain", "ReplicaSet", "web-1"),
		onNode("agent-x", "DaemonSet", "agent"),
		scratch,
	}
}

// actionPods returns the names of the pods the clientset fake holds.
func actionPods(t *testing.T, f *actionFixture) map[string]bool {
	t.Helper()
	list, err := f.typed.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, p := range list.Items {
		out[p.Name] = true
	}
	return out
}

func TestDrain(t *testing.T) {
	f := actionEnv(t, fullGrant, nil, actionDrainObjects()...)
	ref := f.ref(t, "", "nodes", "", "n1")
	opts := DefaultDrainOptions()
	opts.IgnoreDaemonSets = true
	opts.DeleteEmptyDirData = true
	if err := f.s.Drain(context.Background(), ref, opts); err != nil {
		t.Fatal(err)
	}
	if !actionNodeUnschedulable(t, f, "n1") {
		t.Error("the node is not cordoned")
	}
	if got := actionPods(t, f); len(got) != 1 || !got["agent-x"] {
		t.Errorf("pods %v, want the daemonset's alone", got)
	}

	t.Run("without DeleteEmptyDirData", func(t *testing.T) {
		f := actionEnv(t, fullGrant, nil, actionDrainObjects()...)
		opts := DefaultDrainOptions()
		opts.IgnoreDaemonSets = true
		err := f.s.Drain(context.Background(), ref, opts)
		if err == nil || !strings.Contains(err.Error(), "local storage") || !strings.Contains(err.Error(), "default/scratch") {
			t.Fatalf("%v, want the helper's error that names the pod", err)
		}
		if got := actionPods(t, f); len(got) != 3 {
			t.Errorf("pods %v, want all three", got)
		}
	})
}

func TestDrainPrintsNothing(t *testing.T) {
	f := actionEnv(t, fullGrant, nil, actionDrainObjects()...)
	opts := DefaultDrainOptions()
	opts.IgnoreDaemonSets = true
	opts.DeleteEmptyDirData = true

	stdout, stderr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = w, w
	err = f.s.Drain(context.Background(), f.ref(t, "", "nodes", "", "n1"), opts)
	os.Stdout, os.Stderr = stdout, stderr
	w.Close()
	printed, _ := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(printed) != 0 {
		t.Errorf("printed %q", printed)
	}
}
