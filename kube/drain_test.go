package kube

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
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

func TestDrainHelper(t *testing.T) {
	opts := DrainOptions{GracePeriod: 7, Timeout: 3 * time.Second, IgnoreDaemonSets: true, DeleteEmptyDirData: true, Force: true, DisableEviction: true}
	ctx := context.Background()
	out := &drainOutput{}
	h := newDrainHelper(ctx, nil, opts, out)
	if h.EvictErrorRetryDelay != 5*time.Second {
		t.Errorf("EvictErrorRetryDelay %v, want kubectl's 5s", h.EvictErrorRetryDelay)
	}
	if h.GracePeriodSeconds != 7 || h.Timeout != 3*time.Second || !h.IgnoreAllDaemonSets || !h.DeleteEmptyDirData || !h.Force || !h.DisableEviction {
		t.Errorf("helper %+v does not carry the options", h)
	}
	if h.Ctx != ctx || h.Out != io.Writer(out) || h.ErrOut != io.Writer(out) {
		t.Error("the helper does not have the context and the output")
	}
}

func TestDrainWaitsBeforeItEvictsAgain(t *testing.T) {
	const delay = 150 * time.Millisecond
	was := evictRetryDelay
	evictRetryDelay = delay
	t.Cleanup(func() { evictRetryDelay = was })

	var (
		mu       sync.Mutex
		attempts []time.Time
	)
	evictions := func(e *env) {
		rr := standardResources()
		rr[0].APIResources = append(rr[0].APIResources, metav1.APIResource{Name: "pods/eviction", Namespaced: true, Group: "policy", Version: "v1", Kind: "Eviction", Verbs: metav1.Verbs{"create"}})
		e.disc.setResources(rr)
		e.typed.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.GetSubresource() != "eviction" {
				return false, nil, nil
			}
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, time.Now())
			if len(attempts) == 1 {
				return true, nil, apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 0)
			}
			return true, nil, e.typed.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "default", "plain")
		})
	}
	objs := actionDrainObjects()[:3] // the node, the daemonset, and plain
	f := actionEnv(t, fullGrant, evictions, objs...)
	opts := DefaultDrainOptions()
	opts.IgnoreDaemonSets = true
	if err := f.s.Drain(context.Background(), f.ref(t, "", "nodes", "", "n1"), opts); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 {
		t.Fatalf("%d evictions, want 2", len(attempts))
	}
	if gap := attempts[1].Sub(attempts[0]); gap < delay {
		t.Errorf("evicted again after %v, sooner than %v", gap, delay)
	}
	if got := actionPods(t, f); got["plain"] {
		t.Errorf("pods %v", got)
	}
}

func TestDrainOutputIsCut(t *testing.T) {
	var short drainOutput
	fmt.Fprintf(&short, "evicting pod %s\n", "default/plain")
	if got := short.String(); got != "evicting pod default/plain\n" {
		t.Errorf("%q", got)
	}

	var long drainOutput
	line := strings.Repeat("x", 99) + "\n"
	var all strings.Builder
	for i := 0; i < 200; i++ { // 20 000 bytes
		n, err := long.Write([]byte(line))
		if n != len(line) || err != nil {
			t.Fatalf("Write: %d, %v", n, err)
		}
		all.WriteString(line)
	}
	want := all.String()[:16<<10] + "\n[the rest was cut]"
	if got := long.String(); got != want {
		t.Errorf("%d bytes ending in %q, want the first 16 KiB and the mark", len(got), got[len(got)-30:])
	}
}
