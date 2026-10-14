package render

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const metricTestMiB = 1024 * 1024

func TestMetricColumnsArePlaced(t *testing.T) {
	tests := []struct {
		name string
		cols []Column
		want map[int]string
	}{
		{"pods", pod{}.Columns(), map[int]string{6: "CPU", 8: "%CPU/R", 9: "%CPU/L", 10: "MEM", 12: "%MEM/R", 13: "%MEM/L"}},
		{"containers", ContainerColumns(), map[int]string{7: "CPU", 9: "%CPU/R", 10: "%CPU/L", 11: "MEM", 13: "%MEM/R", 14: "%MEM/L"}},
		{"nodes", node{}.Columns(), map[int]string{11: "CPU", 12: "CPU/A", 13: "%CPU", 14: "MEM", 15: "MEM/A", 16: "%MEM"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i, name := range tt.want {
				if i >= len(tt.cols) {
					t.Errorf("no column %d for %s", i, name)
					continue
				}
				c := tt.cols[i]
				if c.Name != name {
					t.Errorf("column %d is %s, want %s", i, c.Name, name)
				}
				if !c.Metric || !c.Right || !c.Numeric || c.Wide {
					t.Errorf("%s is %+v, want Metric, Right, Numeric and not Wide", c.Name, c)
				}
			}
			for i, c := range tt.cols {
				if _, ok := tt.want[i]; !ok && c.Metric {
					t.Errorf("%s at %d is Metric", c.Name, i)
				}
			}
		})
	}
	// CPU/RL and MEM/RL stay where k9s has them, between the metric
	// columns, and are not Metric.
	for _, cols := range [][]Column{pod{}.Columns(), ContainerColumns()} {
		for _, c := range cols {
			if (c.Name == "CPU/RL" || c.Name == "MEM/RL") && c.Metric {
				t.Errorf("%s is Metric", c.Name)
			}
		}
	}
}

func TestStripMetricsGivesStageOneColumns(t *testing.T) {
	po := metricTestPod()
	no := metricTestNode()
	podRow, err := pod{}.Row(po, testNow)
	if err != nil {
		t.Fatal(err)
	}
	nodeRow, err := node{}.Row(no, testNow)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		cols []Column
		rows []Row
		want []string
	}{
		{
			name: "pods", cols: pod{}.Columns(), rows: []Row{podRow},
			want: []string{"NAMESPACE", "NAME", "READY", "STATUS", "RESTARTS", "LAST RESTART*", "CPU/RL*", "MEM/RL*", "IP", "NODE", "SERVICE-ACCOUNT*", "NOMINATED NODE*", "READINESS GATES*", "QOS*", "LABELS*", "VALID*", "AGE"},
		},
		{
			name: "containers", cols: ContainerColumns(), rows: ContainerRows(po, testNow),
			want: []string{"IDX", "NAME", "IMAGE", "READY", "STATE", "RESTARTS", "PROBES(L:R:S)", "CPU/RL", "MEM/RL", "PORTS", "VALID*", "AGE"},
		},
		{
			name: "nodes", cols: node{}.Columns(), rows: []Row{nodeRow},
			want: []string{"NAME", "STATUS", "ROLE", "ARCH*", "TAINTS", "VERSION", "OS-IMAGE*", "KERNEL*", "INTERNAL-IP*", "EXTERNAL-IP*", "PODS", "LABELS*", "VALID*", "AGE"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := append([]Column(nil), tt.cols...)
			firstCells := len(tt.rows[0].Cells)
			cols, rows := StripMetrics(tt.cols, tt.rows)
			wantColumnList(t, cols, tt.want...)
			if len(rows) != len(tt.rows) {
				t.Fatalf("%d rows, want %d", len(rows), len(tt.rows))
			}
			for _, row := range rows {
				if len(row.Cells) != len(cols) || len(row.Order) != len(cols) {
					t.Errorf("%s: %d cells and %d numbers for %d columns", row.Name, len(row.Cells), len(row.Order), len(cols))
				}
				for i, c := range cols {
					if c.Name == "AGE" && row.Cells[i] == NAValue {
						t.Errorf("%s: the cells moved: AGE is %q", row.Name, row.Cells[i])
					}
				}
			}
			// The inputs are as they were.
			if len(tt.cols) != len(before) || len(tt.rows[0].Cells) != firstCells || len(tt.rows[0].Order) != firstCells {
				t.Error("StripMetrics changed its inputs")
			}
			for i := range before {
				if tt.cols[i] != before[i] {
					t.Errorf("column %d changed from %+v to %+v", i, before[i], tt.cols[i])
				}
			}
		})
	}
}

func TestStripMetricsKeepsCellsWithTheirColumns(t *testing.T) {
	cols := []Column{{Name: "A"}, {Name: "M1", Metric: true}, {Name: "B"}, {Name: "M2", Metric: true}}
	rows := []Row{{Name: "r", Cells: []string{"a", "m1", "b", "m2"}, Order: []float64{1, 2, 3, 4}}}
	gotCols, gotRows := StripMetrics(cols, rows)
	if len(gotCols) != 2 || gotCols[0].Name != "A" || gotCols[1].Name != "B" {
		t.Errorf("columns %+v, want A and B", gotCols)
	}
	r := gotRows[0]
	if len(r.Cells) != 2 || r.Cells[0] != "a" || r.Cells[1] != "b" || r.Order[0] != 1 || r.Order[1] != 3 {
		t.Errorf("row %q %v, want [a b] [1 3]", r.Cells, r.Order)
	}
	if rows[0].Cells[1] != "m1" || len(rows[0].Cells) != 4 {
		t.Errorf("the input row changed: %q", rows[0].Cells)
	}
}

// metricTestPod is a running pod with two containers, a and b: requests
// 100m/200Mi and 300m/600Mi, limits 200m/400Mi and 600m/1200Mi.
func metricTestPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1", CreationTimestamp: ago(time.Hour)},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "a", Image: "a:1", Resources: podTestResources("100m", "200Mi", "200m", "400Mi")},
			{Name: "b", Image: "b:1", Resources: podTestResources("300m", "600Mi", "600m", "1200Mi")},
		}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			Conditions:        []corev1.PodCondition{podTestReady},
			ContainerStatuses: []corev1.ContainerStatus{podTestRunning("a", true), podTestRunning("b", true)},
		},
	}
}

// metricTestNode is a ready node with 4 cores and 8Gi allocatable.
func metricTestNode() *corev1.Node {
	n := nodeWithReady(corev1.ConditionTrue)
	n.Status.Allocatable = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
	}
	return n
}

// metricTestUsage is the usage of metricTestPod's containers: a 50m and
// 100Mi, b 150m and 300Mi.
func metricTestUsage() map[string]Usage {
	return map[string]Usage{
		"a": {CPU: 50, Mem: 100 * metricTestMiB},
		"b": {CPU: 150, Mem: 300 * metricTestMiB},
	}
}

// metricTestCells returns the cells of row by the name of their column,
// and the numbers it sorts by.
func metricTestCells(t *testing.T, cols []Column, row Row) (map[string]string, map[string]float64) {
	t.Helper()
	if len(row.Cells) != len(cols) || len(row.Order) != len(cols) {
		t.Fatalf("%d cells and %d numbers for %d columns", len(row.Cells), len(row.Order), len(cols))
	}
	cells := make(map[string]string, len(cols))
	order := make(map[string]float64, len(cols))
	for i, c := range cols {
		cells[c.Name] = row.Cells[i]
		order[c.Name] = row.Order[i]
	}
	return cells, order
}

// wantNA fails the test for every column named whose cell is not
// NAValue with order 0.
func wantNA(t *testing.T, cells map[string]string, order map[string]float64, names ...string) {
	t.Helper()
	for _, n := range names {
		if cells[n] != NAValue || order[n] != 0 {
			t.Errorf("%s = %q (order %v), want %s (order 0)", n, cells[n], order[n], NAValue)
		}
	}
}

// wantOrders fails the test for every column of want whose order is
// another.
func wantOrders(t *testing.T, got, want map[string]float64) {
	t.Helper()
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s sorts by %v, want %v", name, got[name], w)
		}
	}
}

var podMetricNames = []string{"CPU", "%CPU/R", "%CPU/L", "MEM", "%MEM/R", "%MEM/L"}

func TestRowWithoutReadingIsNA(t *testing.T) {
	po := metricTestPod()
	row, err := pod{}.Row(po, testNow)
	if err != nil {
		t.Fatal(err)
	}
	cells, order := metricTestCells(t, pod{}.Columns(), row)
	wantNA(t, cells, order, podMetricNames...)
	if cells["CPU/RL"] != "400:800" {
		t.Errorf("CPU/RL = %q, want 400:800", cells["CPU/RL"])
	}

	for _, row := range ContainerRows(po, testNow) {
		cells, order := metricTestCells(t, ContainerColumns(), row)
		wantNA(t, cells, order, podMetricNames...)
	}

	row, err = node{}.Row(metricTestNode(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	cells, order = metricTestCells(t, node{}.Columns(), row)
	wantNA(t, cells, order, "CPU", "%CPU", "MEM", "%MEM")
	wantCells(t, cells, map[string]string{"CPU/A": "4000", "MEM/A": "8192"})
	wantOrders(t, order, map[string]float64{"CPU/A": 4000, "MEM/A": 8192 * metricTestMiB})
}

// podMetered returns the cells and orders of metricTestPod's row metered
// with usage.
func podMetered(t *testing.T, po *corev1.Pod, usage map[string]Usage) (map[string]string, map[string]float64) {
	t.Helper()
	var m Metered = pod{}
	row, err := m.Row(po, testNow)
	if err != nil {
		t.Fatal(err)
	}
	m.Meter(&row, po, usage)
	return metricTestCells(t, m.Columns(), row)
}

func TestPodMeter(t *testing.T) {
	t.Run("both containers", func(t *testing.T) {
		cells, order := podMetered(t, metricTestPod(), metricTestUsage())
		wantCells(t, cells, map[string]string{
			"CPU": "200", "%CPU/R": "50", "%CPU/L": "25",
			"MEM": "400", "%MEM/R": "50", "%MEM/L": "25",
			"CPU/RL": "400:800", "MEM/RL": "800:1600",
		})
		wantOrders(t, order, map[string]float64{
			"CPU": 200, "%CPU/R": 50, "%CPU/L": 25,
			"MEM": 400 * metricTestMiB, "%MEM/R": 50, "%MEM/L": 25,
		})
	})
	t.Run("one container", func(t *testing.T) {
		cells, _ := podMetered(t, metricTestPod(), map[string]Usage{"a": {CPU: 50, Mem: 100 * metricTestMiB}})
		wantCells(t, cells, map[string]string{"CPU": "50", "%CPU/R": "12", "%CPU/L": "6", "MEM": "100", "%MEM/R": "12", "%MEM/L": "6"})
	})
	t.Run("a container the spec lacks", func(t *testing.T) {
		usage := metricTestUsage()
		usage["zz"] = Usage{CPU: 1000, Mem: 1000 * metricTestMiB}
		cells, _ := podMetered(t, metricTestPod(), usage)
		wantCells(t, cells, map[string]string{"CPU": "200", "MEM": "400"})
	})
	t.Run("an init container", func(t *testing.T) {
		po := metricTestPod()
		po.Spec.InitContainers = []corev1.Container{{Name: "i"}}
		usage := metricTestUsage()
		usage["i"] = Usage{CPU: 10, Mem: 10 * metricTestMiB}
		cells, _ := podMetered(t, po, usage)
		wantCells(t, cells, map[string]string{"CPU": "210", "MEM": "410"})
	})
	t.Run("no requests or limits", func(t *testing.T) {
		po := metricTestPod()
		for i := range po.Spec.Containers {
			po.Spec.Containers[i].Resources = corev1.ResourceRequirements{}
		}
		cells, order := podMetered(t, po, metricTestUsage())
		wantCells(t, cells, map[string]string{"CPU": "200", "MEM": "400"})
		wantNA(t, cells, order, "%CPU/R", "%CPU/L", "%MEM/R", "%MEM/L")
	})
	t.Run("nil usage", func(t *testing.T) {
		cells, order := podMetered(t, metricTestPod(), nil)
		wantNA(t, cells, order, podMetricNames...)
	})
	t.Run("no container of the pod in the usage", func(t *testing.T) {
		cells, order := podMetered(t, metricTestPod(), map[string]Usage{"zz": {CPU: 1, Mem: 1}})
		wantNA(t, cells, order, podMetricNames...)
	})
}

func TestContainerMeter(t *testing.T) {
	po := metricTestPod()
	po.Spec.InitContainers = []corev1.Container{{Name: "done"}}
	rows := ContainerRows(po, testNow)
	MeterContainers(rows, po, metricTestUsage())
	byName := map[string]Row{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	cells, order := metricTestCells(t, ContainerColumns(), byName["a"])
	wantCells(t, cells, map[string]string{"CPU": "50", "%CPU/R": "50", "%CPU/L": "25", "MEM": "100", "%MEM/R": "50", "%MEM/L": "25"})
	wantOrders(t, order, map[string]float64{"CPU": 50, "%CPU/R": 50, "%CPU/L": 25, "MEM": 100 * metricTestMiB, "%MEM/R": 50, "%MEM/L": 25})
	cells, _ = metricTestCells(t, ContainerColumns(), byName["b"])
	wantCells(t, cells, map[string]string{"CPU": "150", "%CPU/R": "50", "%CPU/L": "25", "MEM": "300", "%MEM/R": "50", "%MEM/L": "25"})
	cells, order = metricTestCells(t, ContainerColumns(), byName["done"])
	wantNA(t, cells, order, podMetricNames...)

	rows = ContainerRows(po, testNow)
	MeterContainers(rows, po, nil)
	for _, r := range rows {
		cells, order := metricTestCells(t, ContainerColumns(), r)
		wantNA(t, cells, order, podMetricNames...)
	}
}

func TestNodeMeter(t *testing.T) {
	no := metricTestNode()
	var m Metered = node{}
	row, err := m.Row(no, testNow)
	if err != nil {
		t.Fatal(err)
	}
	m.Meter(&row, no, map[string]Usage{"": {CPU: 1000, Mem: 2048 * metricTestMiB}})
	cells, order := metricTestCells(t, m.Columns(), row)
	wantCells(t, cells, map[string]string{"CPU": "1000", "CPU/A": "4000", "%CPU": "25", "MEM": "2048", "MEM/A": "8192", "%MEM": "25"})
	wantOrders(t, order, map[string]float64{"CPU": 1000, "CPU/A": 4000, "%CPU": 25, "MEM": 2048 * metricTestMiB, "MEM/A": 8192 * metricTestMiB, "%MEM": 25})

	row, _ = m.Row(no, testNow)
	m.Meter(&row, no, nil)
	cells, order = metricTestCells(t, m.Columns(), row)
	wantNA(t, cells, order, "CPU", "%CPU", "MEM", "%MEM")
	wantCells(t, cells, map[string]string{"CPU/A": "4000", "MEM/A": "8192"})
}

func TestMetricNumbers(t *testing.T) {
	tests := []struct{ got, want string }{
		{metricMc(0), "0"},
		{metricMc(1500), "1500"},
		{metricMi(1536 * metricTestMiB), "1536"},
		{metricMi(metricTestMiB - 1), "0"},
		{metricPerc(1, 3), "33"},
		{metricPerc(2, 3), "66"},
		{metricPerc(5, 0), NAValue},
	}
	for i, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%d: %q, want %q", i, tt.got, tt.want)
		}
	}
}
