package render

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Usage is what metrics-server measured of a container or a node: CPU in
// millicores and memory in bytes.
type Usage struct {
	CPU, Mem int64
}

// Metered is a renderer with columns from metrics-server.
type Metered interface {
	Renderer
	// Meter fills the metric cells of row, the row of obj, from usage,
	// the reading's usage of obj's containers (for a pod: keyed by the
	// container's name; for a node: the one entry ""). A nil usage
	// leaves the cells NAValue.
	Meter(row *Row, obj runtime.Object, usage map[string]Usage)
}

// MeterContainers fills the metric cells of rows, the rows ContainerRows
// made of pod, from usage, keyed by the container's name. Each row is
// metered against its own container's requests and limits; a row whose
// container is not in usage keeps NAValue.
func MeterContainers(rows []Row, pod *corev1.Pod, usage map[string]Usage) {
	if pod == nil || len(usage) == 0 {
		return
	}
	spec := &pod.Spec
	for i := range rows {
		u, ok := usage[rows[i].Name]
		if !ok {
			continue
		}
		c := containerOf(spec, rows[i].Name)
		if c == nil {
			continue
		}
		req, lim := workloadResources(c)
		metricMeterPod(&rows[i], containerMetricCells, u, req, lim)
	}
}

// containerOf returns the init, main or ephemeral container of spec
// named name, or nil.
func containerOf(spec *corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == name {
			return &spec.InitContainers[i]
		}
	}
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	for i := range spec.EphemeralContainers {
		if spec.EphemeralContainers[i].Name == name {
			c := corev1.Container(spec.EphemeralContainers[i].EphemeralContainerCommon)
			return &c
		}
	}
	return nil
}

// StripMetrics returns the columns without the Metric ones, and the rows
// without their cells and numbers. It makes new slices and leaves cols
// and rows as they are.
func StripMetrics(cols []Column, rows []Row) ([]Column, []Row) {
	keep := make([]int, 0, len(cols))
	stripped := make([]Column, 0, len(cols))
	for i, c := range cols {
		if !c.Metric {
			keep = append(keep, i)
			stripped = append(stripped, c)
		}
	}
	if rows == nil {
		return stripped, nil
	}
	out := make([]Row, len(rows))
	for r, row := range rows {
		cells := make([]string, 0, len(keep))
		order := make([]float64, 0, len(keep))
		for _, i := range keep {
			if i < len(row.Cells) {
				cells = append(cells, row.Cells[i])
			}
			if i < len(row.Order) {
				order = append(order, row.Order[i])
			}
		}
		row.Cells, row.Order = cells, order
		out[r] = row
	}
	return stripped, out
}

// podMetricIndexes are the indexes of the six metric columns of a pod or
// a container, which have the same names.
type podMetricIndexes struct {
	cpu, cpuR, cpuL, mem, memR, memL int
}

func podMetricIndexesOf(cols []Column) podMetricIndexes {
	return podMetricIndexes{
		cpu:  metricIndex(cols, "CPU"),
		cpuR: metricIndex(cols, "%CPU/R"),
		cpuL: metricIndex(cols, "%CPU/L"),
		mem:  metricIndex(cols, "MEM"),
		memR: metricIndex(cols, "%MEM/R"),
		memL: metricIndex(cols, "%MEM/L"),
	}
}

var (
	podMetricCells       = podMetricIndexesOf(podColumns)
	containerMetricCells = podMetricIndexesOf(containerColumns)
)

// metricMeterPod sets the six metric cells of row, a pod's or a
// container's, from u against the requests req and the limits lim.
func metricMeterPod(row *Row, at podMetricIndexes, u Usage, req, lim workloadQuantities) {
	metricSetMc(row, at.cpu, u.CPU)
	metricSetPerc(row, at.cpuR, u.CPU, req.cpu)
	metricSetPerc(row, at.cpuL, u.CPU, lim.cpu)
	metricSetMi(row, at.mem, u.Mem)
	metricSetPerc(row, at.memR, u.Mem, req.mem)
	metricSetPerc(row, at.memL, u.Mem, lim.mem)
}

// metricIndex returns the index of the column named name. The columns
// are the program's own, so a name that is not there is a mistake in it
// and panics.
func metricIndex(cols []Column, name string) int {
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	panic(fmt.Sprintf("render: no column %s", name))
}

// metricSet sets the cell of row at i. A row shorter than its columns
// is left as it is.
func metricSet(row *Row, i int, text string, order float64) {
	if i < len(row.Cells) && i < len(row.Order) {
		row.Cells[i], row.Order[i] = text, order
	}
}

// metricSetMc sets a cell of millicores, sorted by them.
func metricSetMc(row *Row, i int, mc int64) {
	metricSet(row, i, metricMc(mc), float64(mc))
}

// metricSetMi sets a cell of memory in mebibytes, sorted by the bytes,
// as the MEM/RL cells are.
func metricSetMi(row *Row, i int, bytes int64) {
	metricSet(row, i, metricMi(bytes), float64(bytes))
}

// metricSetPerc sets a cell of used over of in percent, sorted by the
// percentage, or NAValue and 0 when of is 0.
func metricSetPerc(row *Row, i int, used, of int64) {
	if of == 0 {
		metricSet(row, i, NAValue, 0)
		return
	}
	p := metricPercent(used, of)
	metricSet(row, i, strconv.FormatInt(p, 10), float64(p))
}

// metricMc prints millicores as k9s does: a whole number without a
// unit.
func metricMc(mc int64) string {
	return strconv.FormatInt(mc, 10)
}

// metricMi prints bytes as k9s does: whole mebibytes, floored, without a
// unit.
func metricMi(bytes int64) string {
	return strconv.FormatInt(bytes/workloadMebi, 10)
}

// metricPerc prints used over of as k9s does: a whole percent, floored,
// or NAValue when of is 0.
func metricPerc(used, of int64) string {
	if of == 0 {
		return NAValue
	}
	return strconv.FormatInt(metricPercent(used, of), 10)
}

// metricPercent is floor(100 × used / of); of is not 0.
func metricPercent(used, of int64) int64 {
	return 100 * used / of
}
