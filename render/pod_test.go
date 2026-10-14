package render

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodColumns(t *testing.T) {
	wantColumns(t, Pods, "NAMESPACE", "NAME", "READY", "STATUS", "RESTARTS", "LAST RESTART*", "CPU", "CPU/RL*", "%CPU/R", "%CPU/L", "MEM", "MEM/RL*", "%MEM/R", "%MEM/L", "IP", "NODE", "SERVICE-ACCOUNT*", "NOMINATED NODE*", "READINESS GATES*", "QOS*", "LABELS*", "VALID*", "AGE")
}

// podTestResources returns the requests and limits of a container.
func podTestResources(reqCPU, reqMem, limCPU, limMem string) corev1.ResourceRequirements {
	rr := corev1.ResourceRequirements{}
	if reqCPU != "" || reqMem != "" {
		rr.Requests = corev1.ResourceList{}
		if reqCPU != "" {
			rr.Requests[corev1.ResourceCPU] = resource.MustParse(reqCPU)
		}
		if reqMem != "" {
			rr.Requests[corev1.ResourceMemory] = resource.MustParse(reqMem)
		}
	}
	if limCPU != "" || limMem != "" {
		rr.Limits = corev1.ResourceList{}
		if limCPU != "" {
			rr.Limits[corev1.ResourceCPU] = resource.MustParse(limCPU)
		}
		if limMem != "" {
			rr.Limits[corev1.ResourceMemory] = resource.MustParse(limMem)
		}
	}
	return rr
}

// podTestWaiting is the status of a container waiting for reason.
func podTestWaiting(name, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}
}

// podTestRunning is the status of a running container.
func podTestRunning(name string, ready bool) corev1.ContainerStatus {
	started := true
	return corev1.ContainerStatus{Name: name, Ready: ready, Started: &started, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: ago(time.Hour)}}}
}

// podTestTerminated is the status of a container that ended.
func podTestTerminated(name, reason string, code, signal int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: code, Signal: signal}}}
}

// podTestReady is the pod's Ready condition.
var podTestReady = corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}

// podTestPod is a pod in ns "shop" with one container per name, and st
// as its status.
func podTestPod(name string, st corev1.PodStatus, containers ...string) *corev1.Pod {
	po := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, CreationTimestamp: ago(time.Hour)},
		Status:     st,
	}
	for _, c := range containers {
		po.Spec.Containers = append(po.Spec.Containers, corev1.Container{Name: c, Image: c + ":1"})
	}
	return po
}

// podTestHealthy is a running pod with a sidecar and two containers,
// all ready, in which every cell has something to show.
func podTestHealthy() *corev1.Pod {
	always := corev1.ContainerRestartPolicyAlways
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "shop",
			Name:              "web-1",
			Labels:            map[string]string{"tier": "front", "app": "web"},
			CreationTimestamp: ago(5 * 24 * time.Hour),
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "proxy", Image: "envoy:1", RestartPolicy: &always, Resources: podTestResources("50m", "32Mi", "100m", "64Mi")},
			},
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx:1.27", Resources: podTestResources("100m", "128Mi", "500m", "256Mi")},
				// No requests: k9s counts its limits as its requests.
				{Name: "log", Image: "busybox:1", Resources: podTestResources("", "", "200m", "64Mi")},
			},
			NodeName:           "node-a",
			ServiceAccountName: "web",
			ReadinessGates:     []corev1.PodReadinessGate{{ConditionType: "example.com/lb"}},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			PodIP:    "10.0.0.7",
			QOSClass: corev1.PodQOSBurstable,
			Conditions: []corev1.PodCondition{
				podTestReady,
				{Type: "example.com/lb", Status: corev1.ConditionTrue},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				func() corev1.ContainerStatus {
					s := podTestRunning("proxy", true)
					s.RestartCount = 1
					s.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: ago(2 * time.Hour)}
					return s
				}(),
			},
			ContainerStatuses: []corev1.ContainerStatus{
				func() corev1.ContainerStatus {
					s := podTestRunning("app", true)
					s.RestartCount = 2
					s.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 137, FinishedAt: ago(30 * time.Minute)}
					return s
				}(),
				podTestRunning("log", true),
			},
		},
	}
}

func TestPodRows(t *testing.T) {
	deleted := ago(time.Minute)
	twoInits := podTestPod("init-0", corev1.PodStatus{
		Phase: corev1.PodPending,
		InitContainerStatuses: []corev1.ContainerStatus{
			podTestRunning("migrate", false),
			podTestWaiting("seed", "PodInitializing"),
		},
		ContainerStatuses: []corev1.ContainerStatus{podTestWaiting("app", "PodInitializing")},
	}, "app")
	twoInits.Spec.InitContainers = []corev1.Container{{Name: "migrate"}, {Name: "seed"}}

	initCrash := podTestPod("init-crash", corev1.PodStatus{
		Phase:                 corev1.PodPending,
		InitContainerStatuses: []corev1.ContainerStatus{podTestWaiting("migrate", "CrashLoopBackOff")},
		ContainerStatuses:     []corev1.ContainerStatus{podTestWaiting("app", "PodInitializing")},
	}, "app")
	initCrash.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}

	initError := podTestPod("init-error", corev1.PodStatus{
		Phase:                 corev1.PodPending,
		InitContainerStatuses: []corev1.ContainerStatus{podTestTerminated("migrate", "Error", 1, 0)},
	}, "app")
	initError.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}

	podInit := podTestPod("pod-init", corev1.PodStatus{
		Phase:                 corev1.PodPending,
		InitContainerStatuses: []corev1.ContainerStatus{podTestTerminated("migrate", "Completed", 0, 0)},
		ContainerStatuses:     []corev1.ContainerStatus{podTestWaiting("app", "PodInitializing")},
	}, "app")
	podInit.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}

	terminating := podTestPod("leaving", corev1.PodStatus{
		Phase:             corev1.PodRunning,
		Conditions:        []corev1.PodCondition{podTestReady},
		ContainerStatuses: []corev1.ContainerStatus{podTestRunning("app", true)},
	}, "app")
	terminating.DeletionTimestamp = &deleted

	nodeLost := podTestPod("lost", corev1.PodStatus{
		Phase:             corev1.PodRunning,
		Reason:            "NodeLost",
		ContainerStatuses: []corev1.ContainerStatus{podTestRunning("app", true)},
	}, "app")
	nodeLost.DeletionTimestamp = &deleted

	gated := podTestPod("gated", corev1.PodStatus{
		Phase:             corev1.PodRunning,
		Conditions:        []corev1.PodCondition{podTestReady, {Type: "example.com/lb", Status: corev1.ConditionFalse}},
		ContainerStatuses: []corev1.ContainerStatus{podTestRunning("app", true)},
	}, "app")
	gated.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: "example.com/lb"}}

	tests := []struct {
		name   string
		pod    *corev1.Pod
		want   map[string]string
		health Health
	}{
		{
			name: "healthy, every cell",
			pod:  podTestHealthy(),
			want: map[string]string{
				"NAMESPACE":       "shop",
				"NAME":            "web-1",
				"READY":           "3/3",
				"STATUS":          "Running",
				"RESTARTS":        "3",
				"LAST RESTART":    "30m",
				"CPU":             NAValue,
				"CPU/RL":          "350:800",
				"%CPU/R":          NAValue,
				"%CPU/L":          NAValue,
				"MEM":             NAValue,
				"MEM/RL":          "224:384",
				"%MEM/R":          NAValue,
				"%MEM/L":          NAValue,
				"IP":              "10.0.0.7",
				"NODE":            "node-a",
				"SERVICE-ACCOUNT": "web",
				"NOMINATED NODE":  MissingValue,
				"READINESS GATES": "1/1",
				"QOS":             "BU",
				"LABELS":          "app=web,tier=front",
				"VALID":           "",
				"AGE":             "5d",
			},
			health: OK,
		},
		{
			name: "metadata only",
			pod:  &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "bare", CreationTimestamp: ago(10 * time.Minute)}},
			want: map[string]string{
				"NAMESPACE":       "shop",
				"NAME":            "bare",
				"READY":           "0/0",
				"STATUS":          UnknownValue,
				"RESTARTS":        "0",
				"LAST RESTART":    NAValue,
				"CPU/RL":          "0:0",
				"MEM/RL":          "0:0",
				"IP":              NAValue,
				"NODE":            NAValue,
				"SERVICE-ACCOUNT": NAValue,
				"NOMINATED NODE":  MissingValue,
				"READINESS GATES": MissingValue,
				"QOS":             "BE",
				"LABELS":          MissingValue,
				"VALID":           "container ready check failed: 0 of 0",
				"AGE":             "10m",
			},
			health: Error,
		},
		{
			name: "running, a container not ready",
			pod: podTestPod("half", corev1.PodStatus{
				Phase:             corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{podTestRunning("app", true), podTestRunning("db", false)},
			}, "app", "db"),
			want:   map[string]string{"READY": "1/2", "STATUS": "Running", "VALID": "container ready check failed: 1 of 2"},
			health: Error,
		},
		{
			name: "running, the ready condition false",
			pod: podTestPod("unready", corev1.PodStatus{
				Phase:             corev1.PodRunning,
				Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
				ContainerStatuses: []corev1.ContainerStatus{podTestRunning("app", true)},
			}, "app"),
			want:   map[string]string{"READY": "1/1", "STATUS": "Running", "VALID": "pod condition ready is false"},
			health: Error,
		},
		{
			name:   "running, a readiness gate not passed",
			pod:    gated,
			want:   map[string]string{"STATUS": "Running", "READINESS GATES": "0/1", "VALID": "readiness gate check failed: 0 of 1"},
			health: Error,
		},
		{
			name: "running, one container done and one running",
			pod: podTestPod("mixed", corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{podTestReady},
				ContainerStatuses: []corev1.ContainerStatus{
					podTestTerminated("once", "Completed", 0, 0),
					podTestRunning("app", true),
				},
			}, "once", "app"),
			want:   map[string]string{"READY": "1/2", "STATUS": "Running", "VALID": "container ready check failed: 1 of 2"},
			health: Error,
		},
		{
			name: "pending, no container scheduled",
			pod: podTestPod("unscheduled", corev1.PodStatus{
				Phase:      corev1.PodPending,
				Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"}},
			}, "app"),
			want:   map[string]string{"READY": "0/1", "STATUS": "Pending", "VALID": "container ready check failed: 0 of 1"},
			health: Pending,
		},
		{
			name: "container creating",
			pod: podTestPod("creating", corev1.PodStatus{
				Phase:             corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{podTestWaiting("app", "ContainerCreating")},
			}, "app"),
			want:   map[string]string{"STATUS": "ContainerCreating", "VALID": "container ready check failed: 0 of 1"},
			health: Pending,
		},
		{
			name:   "pod initializing",
			pod:    podInit,
			want:   map[string]string{"STATUS": "PodInitializing"},
			health: Pending,
		},
		{
			name:   "init containers running",
			pod:    twoInits,
			want:   map[string]string{"READY": "0/1", "STATUS": "Init:0/2"},
			health: Pending,
		},
		{
			name:   "init container crashing",
			pod:    initCrash,
			want:   map[string]string{"STATUS": "Init:CrashLoopBackOff"},
			health: Error,
		},
		{
			name:   "init container failed",
			pod:    initError,
			want:   map[string]string{"STATUS": "Init:Error"},
			health: Error,
		},
		{
			name: "crash loop",
			pod: podTestPod("crashing", corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{func() corev1.ContainerStatus {
					s := podTestWaiting("app", "CrashLoopBackOff")
					s.RestartCount = 5
					s.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: ago(90 * time.Second)}
					return s
				}()},
			}, "app"),
			want:   map[string]string{"STATUS": "CrashLoopBackOff", "RESTARTS": "5", "LAST RESTART": "90s", "VALID": "container ready check failed: 0 of 1"},
			health: Error,
		},
		{
			name: "image pull back-off",
			pod: podTestPod("pulling", corev1.PodStatus{
				Phase:             corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{podTestWaiting("app", "ImagePullBackOff")},
			}, "app"),
			want:   map[string]string{"STATUS": "ImagePullBackOff"},
			health: Error,
		},
		{
			name: "error, terminated with a non-zero exit code",
			pod: podTestPod("failed", corev1.PodStatus{
				Phase:             corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{podTestTerminated("app", "Error", 1, 0)},
			}, "app"),
			want:   map[string]string{"STATUS": "Error", "VALID": "container ready check failed: 0 of 1"},
			health: Error,
		},
		{
			name: "terminated with no reason, an exit code",
			pod: podTestPod("exit", corev1.PodStatus{
				Phase:             corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{podTestTerminated("app", "", 2, 0)},
			}, "app"),
			want:   map[string]string{"STATUS": "ExitCode:2"},
			health: Error,
		},
		{
			name: "terminated with no reason, a signal",
			pod: podTestPod("killed", corev1.PodStatus{
				Phase:             corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{podTestTerminated("app", "", 137, 9)},
			}, "app"),
			want:   map[string]string{"STATUS": "Signal:9"},
			health: Error,
		},
		{
			name: "completed",
			pod: podTestPod("done", corev1.PodStatus{
				Phase:             corev1.PodSucceeded,
				ContainerStatuses: []corev1.ContainerStatus{podTestTerminated("app", "Completed", 0, 0)},
			}, "app"),
			want:   map[string]string{"READY": "0/1", "STATUS": "Completed", "VALID": ""},
			health: Completed,
		},
		{
			name: "out of memory",
			pod: podTestPod("oom", corev1.PodStatus{
				Phase:             corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{podTestTerminated("app", "OOMKilled", 137, 0)},
			}, "app"),
			want:   map[string]string{"STATUS": "OOMKilled"},
			health: Error,
		},
		{
			name:   "terminating",
			pod:    terminating,
			want:   map[string]string{"READY": "1/1", "STATUS": "Terminating", "VALID": "pod is terminating"},
			health: Pending,
		},
		{
			name:   "evicted",
			pod:    podTestPod("evicted", corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}, "app"),
			want:   map[string]string{"STATUS": "Evicted"},
			health: Error,
		},
		{
			name:   "node lost",
			pod:    nodeLost,
			want:   map[string]string{"STATUS": "Unknown"},
			health: Error,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, Pods, tt.pod)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
			if row.Namespace != tt.pod.Namespace || row.Name != tt.pod.Name {
				t.Errorf("row is %s/%s, want %s/%s", row.Namespace, row.Name, tt.pod.Namespace, tt.pod.Name)
			}
		})
	}
}

func TestPodOrder(t *testing.T) {
	restarted := func(name string, restarts int32, last time.Duration) *corev1.Pod {
		s := podTestRunning("app", true)
		s.RestartCount = restarts
		s.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{FinishedAt: ago(last)}
		return podTestPod(name, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{s}}, "app")
	}
	sized := func(cpu, mem string, age time.Duration) *corev1.Pod {
		po := podTestPod("sized", corev1.PodStatus{}, "app")
		po.Spec.Containers[0].Resources = podTestResources(cpu, mem, "", "")
		po.CreationTimestamp = ago(age)
		return po
	}

	few, _ := rowOf(t, Pods, restarted("few", 1, 2*time.Hour))
	many, _ := rowOf(t, Pods, restarted("many", 12, 10*time.Minute))
	if a, b := orderOf(t, Pods, few, "RESTARTS"), orderOf(t, Pods, many, "RESTARTS"); a >= b {
		t.Errorf("RESTARTS: 1 sorts at %v, 12 at %v", a, b)
	}
	if a, b := orderOf(t, Pods, many, "LAST RESTART"), orderOf(t, Pods, few, "LAST RESTART"); a >= b {
		t.Errorf("LAST RESTART: 10m sorts at %v, 2h at %v", a, b)
	}

	small, _ := rowOf(t, Pods, sized("900m", "900Mi", 3*time.Minute))
	large, _ := rowOf(t, Pods, sized("2", "2Gi", 3*time.Hour))
	if a, b := orderOf(t, Pods, small, "CPU/RL"), orderOf(t, Pods, large, "CPU/RL"); a >= b {
		t.Errorf("CPU/RL: 900m sorts at %v, 2 at %v", a, b)
	}
	if a, b := orderOf(t, Pods, small, "MEM/RL"), orderOf(t, Pods, large, "MEM/RL"); a >= b {
		t.Errorf("MEM/RL: 900Mi sorts at %v, 2Gi at %v", a, b)
	}
	if a, b := orderOf(t, Pods, small, "AGE"), orderOf(t, Pods, large, "AGE"); a >= b {
		t.Errorf("AGE: 3m sorts at %v, 3h at %v", a, b)
	}
}

// podTestWithInit gives po init containers of the names given, a sidecar
// (restarted always) where sidecar is true.
func podTestWithInit(po *corev1.Pod, sidecar bool, names ...string) *corev1.Pod {
	always := corev1.ContainerRestartPolicyAlways
	for _, n := range names {
		c := corev1.Container{Name: n, Image: n + ":1"}
		if sidecar {
			c.RestartPolicy = &always
		}
		po.Spec.InitContainers = append(po.Spec.InitContainers, c)
	}
	return po
}

// podTestDeleted marks po as being deleted.
func podTestDeleted(po *corev1.Pod) *corev1.Pod {
	at := ago(time.Minute)
	po.DeletionTimestamp = &at
	return po
}

// The STATUS of a pod is what kubectl get pods prints for it, where that
// is not what k9s's STATUS column shows. The expectations are kubectl's
// (printPod in kubernetes' pkg/printers/internalversion).
func TestPodStatusAsKubectl(t *testing.T) {
	notStarted := false
	tests := []struct {
		name   string
		pod    *corev1.Pod
		status string
		health Health
	}{
		{
			name: "deleted while initializing",
			pod: podTestDeleted(podTestWithInit(podTestPod("a", corev1.PodStatus{
				Phase:                 corev1.PodPending,
				InitContainerStatuses: []corev1.ContainerStatus{podTestRunning("migrate", false)},
				ContainerStatuses:     []corev1.ContainerStatus{podTestWaiting("app", "PodInitializing")},
			}, "app"), false, "migrate")),
			status: "Terminating",
			health: Pending,
		},
		{
			name: "deleted while an init container crash-loops",
			pod: podTestDeleted(podTestWithInit(podTestPod("b", corev1.PodStatus{
				Phase:                 corev1.PodPending,
				InitContainerStatuses: []corev1.ContainerStatus{podTestWaiting("migrate", "CrashLoopBackOff")},
			}, "app"), false, "migrate")),
			status: "Terminating",
			health: Pending,
		},
		{
			name: "deleted while initializing, its node lost",
			pod: podTestDeleted(podTestWithInit(podTestPod("c", corev1.PodStatus{
				Phase:                 corev1.PodPending,
				Reason:                "NodeLost",
				InitContainerStatuses: []corev1.ContainerStatus{podTestRunning("migrate", false)},
			}, "app"), false, "migrate")),
			status: "Unknown",
			health: Error,
		},
		{
			name: "deleted after it succeeded",
			pod: podTestDeleted(podTestPod("d", corev1.PodStatus{
				Phase:             corev1.PodSucceeded,
				ContainerStatuses: []corev1.ContainerStatus{podTestTerminated("app", "Completed", 0, 0)},
			}, "app")),
			status: "Completed",
			health: Completed,
		},
		{
			name: "deleted after it failed",
			pod: podTestDeleted(podTestPod("d2", corev1.PodStatus{
				Phase:             corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{podTestTerminated("app", "Error", 1, 0)},
			}, "app")),
			status: "Error",
			health: Error,
		},
		{
			name: "a completed container beside a running one, the pod ready",
			pod: podTestPod("e", corev1.PodStatus{
				Phase:             corev1.PodRunning,
				Conditions:        []corev1.PodCondition{podTestReady},
				ContainerStatuses: []corev1.ContainerStatus{podTestTerminated("once", "Completed", 0, 0), podTestRunning("app", true)},
			}, "once", "app"),
			status: "Running",
			health: Error, // READY is 1/2
		},
		{
			name: "a completed container beside a running one, the pod not ready",
			pod: podTestPod("f", corev1.PodStatus{
				Phase:             corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{podTestTerminated("once", "Completed", 0, 0), podTestRunning("app", true)},
			}, "once", "app"),
			status: "NotReady",
			health: Error,
		},
		{
			name: "a completed container beside a failed one",
			pod: podTestPod("g", corev1.PodStatus{
				Phase:             corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{podTestTerminated("once", "Completed", 0, 0), podTestTerminated("app", "Error", 1, 0)},
			}, "once", "app"),
			status: "Error",
			health: Error,
		},
		{
			name: "held by a scheduling gate",
			pod: podTestPod("h", corev1.PodStatus{
				Phase:      corev1.PodPending,
				Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "SchedulingGated"}},
			}, "app"),
			status: "SchedulingGated",
			health: Pending,
		},
		{
			name: "a sidecar started and not ready beside a running container",
			pod: podTestWithInit(podTestPod("i", corev1.PodStatus{
				Phase:                 corev1.PodRunning,
				InitContainerStatuses: []corev1.ContainerStatus{podTestRunning("proxy", false)},
				ContainerStatuses:     []corev1.ContainerStatus{podTestRunning("app", true)},
			}, "app"), true, "proxy"),
			status: "Running",
			health: Error, // READY is 1/2
		},
		{
			name: "a sidecar not started yet",
			pod: podTestWithInit(podTestPod("j", corev1.PodStatus{
				Phase: corev1.PodPending,
				InitContainerStatuses: []corev1.ContainerStatus{{
					Name: "proxy", Started: &notStarted,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: ago(time.Minute)}},
				}},
				ContainerStatuses: []corev1.ContainerStatus{podTestWaiting("app", "PodInitializing")},
			}, "app"), true, "proxy"),
			status: "Init:0/1",
			health: Pending,
		},
		{
			name: "a sidecar that failed",
			pod: podTestWithInit(podTestPod("k", corev1.PodStatus{
				Phase:                 corev1.PodPending,
				InitContainerStatuses: []corev1.ContainerStatus{podTestTerminated("proxy", "Error", 1, 0)},
				ContainerStatuses:     []corev1.ContainerStatus{podTestWaiting("app", "PodInitializing")},
			}, "app"), true, "proxy"),
			status: "Init:Error",
			health: Error,
		},
		{
			name: "initialized, though an init container's status says otherwise",
			pod: podTestWithInit(podTestPod("l", corev1.PodStatus{
				Phase:                 corev1.PodRunning,
				Conditions:            []corev1.PodCondition{{Type: corev1.PodInitialized, Status: corev1.ConditionTrue}},
				InitContainerStatuses: []corev1.ContainerStatus{{Name: "migrate", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}}},
				ContainerStatuses:     []corev1.ContainerStatus{podTestWaiting("app", "CrashLoopBackOff")},
			}, "app"), false, "migrate"),
			status: "CrashLoopBackOff",
			health: Error,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, Pods, tt.pod)
			wantCells(t, cells, map[string]string{"STATUS": tt.status})
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}
