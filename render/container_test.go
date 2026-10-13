package render

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestContainerColumns(t *testing.T) {
	wantColumnList(t, ContainerColumns(), "IDX", "NAME", "IMAGE", "READY", "STATE", "RESTARTS", "PROBES(L:R:S)", "CPU/RL", "MEM/RL", "PORTS", "VALID*", "AGE")
}

// containerTestCells returns the cells of row by the name of their
// column.
func containerTestCells(t *testing.T, row Row) map[string]string {
	t.Helper()
	cols := ContainerColumns()
	if len(row.Cells) != len(cols) || len(row.Order) != len(cols) {
		t.Fatalf("%d cells and %d numbers for %d columns", len(row.Cells), len(row.Order), len(cols))
	}
	cells := make(map[string]string, len(cols))
	for i, c := range cols {
		cells[c.Name] = row.Cells[i]
	}
	return cells
}

// containerTestOrder returns the number row sorts by in column, which
// must be Numeric.
func containerTestOrder(t *testing.T, row Row, column string) float64 {
	t.Helper()
	for i, c := range ContainerColumns() {
		if c.Name == column {
			if !c.Numeric {
				t.Fatalf("column %s is not numeric", column)
			}
			return row.Order[i]
		}
	}
	t.Fatalf("no column %s", column)
	return 0
}

// containerTestPod has an init container that is done, two containers
// of which one has every cell filled and one has no status yet, and an
// ephemeral container being created.
func containerTestPod() *corev1.Pod {
	probe := &corev1.Probe{}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "shop",
			Name:              "web-1",
			Labels:            map[string]string{"app": "web"},
			CreationTimestamp: ago(5 * 24 * time.Hour),
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "migrate", Image: "migrate:2"}},
			Containers: []corev1.Container{
				{
					Name:           "app",
					Image:          "nginx:1.27",
					Resources:      podTestResources("100m", "128Mi", "500m", "256Mi"),
					LivenessProbe:  probe,
					ReadinessProbe: probe,
					Ports: []corev1.ContainerPort{
						{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
						{ContainerPort: 9090, Protocol: corev1.ProtocolUDP},
					},
				},
				{Name: "db", Image: "postgres:16"},
			},
			EphemeralContainers: []corev1.EphemeralContainer{
				{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "busybox:1"}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  "migrate",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", StartedAt: ago(4*24*time.Hour + time.Minute), FinishedAt: ago(4 * 24 * time.Hour)}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "app",
				Ready:        true,
				RestartCount: 2,
				State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: ago(3 * time.Hour)}},
			}},
			EphemeralContainerStatuses: []corev1.ContainerStatus{podTestWaiting("debug", "ContainerCreating")},
		},
	}
}

func TestContainerRows(t *testing.T) {
	rows := ContainerRows(containerTestPod(), testNow)
	want := []struct {
		cells  map[string]string
		health Health
	}{
		{
			cells: map[string]string{
				"IDX": "I1", "NAME": "migrate", "IMAGE": "migrate:2", "READY": "false", "STATE": "Completed",
				"RESTARTS": "0", "PROBES(L:R:S)": "off:off:off", "CPU/RL": "0:0", "MEM/RL": "0:0",
				"PORTS": MissingValue, "VALID": "", "AGE": "4d",
			},
			health: Completed,
		},
		{
			cells: map[string]string{
				"IDX": "M1", "NAME": "app", "IMAGE": "nginx:1.27", "READY": "true", "STATE": "Running",
				"RESTARTS": "2", "PROBES(L:R:S)": "on:on:off", "CPU/RL": "100:500", "MEM/RL": "128:256",
				"PORTS": "http:8080,9090╱UDP", "VALID": "", "AGE": "3h",
			},
			health: OK,
		},
		{
			cells: map[string]string{
				"IDX": "M2", "NAME": "db", "IMAGE": "postgres:16", "READY": "false", "STATE": MissingValue,
				"RESTARTS": "0", "PROBES(L:R:S)": "off:off:off", "CPU/RL": "0:0", "MEM/RL": "0:0",
				"PORTS": MissingValue, "VALID": "container is not ready", "AGE": "5d",
			},
			health: Error,
		},
		{
			cells: map[string]string{
				"IDX": "E1", "NAME": "debug", "IMAGE": "busybox:1", "READY": "false", "STATE": "ContainerCreating",
				"RESTARTS": "0", "PROBES(L:R:S)": "off:off:off", "CPU/RL": "0:0", "MEM/RL": "0:0",
				"PORTS": MissingValue, "VALID": "container is not ready", "AGE": "5d",
			},
			health: Pending,
		},
	}
	if len(rows) != len(want) {
		t.Fatalf("%d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		row := rows[i]
		wantCells(t, containerTestCells(t, row), w.cells)
		if row.Health != w.health {
			t.Errorf("%s: health = %v, want %v", w.cells["NAME"], row.Health, w.health)
		}
		if row.Namespace != "shop" || row.Name != w.cells["NAME"] {
			t.Errorf("row is %s/%s, want shop/%s", row.Namespace, row.Name, w.cells["NAME"])
		}
		if row.Labels != nil {
			t.Errorf("%s: labels %v, want none", row.Name, row.Labels)
		}
	}
}

func TestContainerStates(t *testing.T) {
	tests := []struct {
		name   string
		status corev1.ContainerStatus
		want   map[string]string
		health Health
	}{
		{
			name:   "waiting for the pod to initialize",
			status: podTestWaiting("app", "PodInitializing"),
			want:   map[string]string{"STATE": "PodInitializing", "VALID": "container is not ready"},
			health: Pending,
		},
		{
			name:   "crash loop",
			status: podTestWaiting("app", "CrashLoopBackOff"),
			want:   map[string]string{"STATE": "CrashLoopBackOff", "VALID": "container is not ready"},
			health: Error,
		},
		{
			name:   "waiting without a reason",
			status: podTestWaiting("app", ""),
			want:   map[string]string{"STATE": "Waiting", "VALID": "container is not ready"},
			health: Error,
		},
		{
			name: "terminated with an error",
			status: corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "Error", ExitCode: 1, FinishedAt: ago(10 * time.Minute),
			}}},
			want:   map[string]string{"STATE": "Error", "VALID": "container is not ready", "AGE": "10m"},
			health: Error,
		},
		{
			name:   "running and not ready",
			status: podTestRunning("app", false),
			want:   map[string]string{"STATE": "Running", "READY": "false", "VALID": "container is not ready", "AGE": "60m"},
			health: Error,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			po := podTestPod("one", corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{tt.status}}, "app")
			rows := ContainerRows(po, testNow)
			if len(rows) != 1 {
				t.Fatalf("%d rows, want 1", len(rows))
			}
			wantCells(t, containerTestCells(t, rows[0]), tt.want)
			if rows[0].Health != tt.health {
				t.Errorf("health = %v, want %v", rows[0].Health, tt.health)
			}
		})
	}
}

func TestContainerRowsOfNothing(t *testing.T) {
	if rows := ContainerRows(nil, testNow); len(rows) != 0 {
		t.Errorf("a nil pod gives %d rows", len(rows))
	}
	if rows := ContainerRows(&corev1.Pod{}, testNow); len(rows) != 0 {
		t.Errorf("a pod without containers gives %d rows", len(rows))
	}
}

func TestContainerOrder(t *testing.T) {
	po := podTestPod("two", corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			func() corev1.ContainerStatus {
				s := podTestRunning("small", true)
				s.RestartCount = 1
				s.State.Running.StartedAt = ago(2 * time.Minute)
				return s
			}(),
			func() corev1.ContainerStatus {
				s := podTestRunning("large", true)
				s.RestartCount = 7
				s.State.Running.StartedAt = ago(2 * time.Hour)
				return s
			}(),
		},
	}, "small", "large")
	po.Spec.Containers[0].Resources = podTestResources("250m", "64Mi", "", "")
	po.Spec.Containers[1].Resources = podTestResources("1", "1Gi", "", "")
	rows := ContainerRows(po, testNow)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	small, large := rows[0], rows[1]
	for _, col := range []string{"RESTARTS", "CPU/RL", "MEM/RL", "AGE"} {
		if a, b := containerTestOrder(t, small, col), containerTestOrder(t, large, col); a >= b {
			t.Errorf("%s: the small container sorts at %v, the large at %v", col, a, b)
		}
	}
}
