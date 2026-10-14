// Derived from k9s's internal/render/pod.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type pod struct{}

func init() {
	Register(Pods, pod{}, func() runtime.Object { return &corev1.Pod{} })
}

var podColumns = []Column{
	{Name: "NAMESPACE"},
	{Name: "NAME"},
	{Name: "READY"},
	{Name: "STATUS"},
	{Name: "RESTARTS", Right: true, Numeric: true},
	{Name: "LAST RESTART", Wide: true, Right: true, Numeric: true},
	{Name: "CPU", Right: true, Numeric: true, Metric: true},
	{Name: "CPU/RL", Wide: true, Right: true, Numeric: true},
	{Name: "%CPU/R", Right: true, Numeric: true, Metric: true},
	{Name: "%CPU/L", Right: true, Numeric: true, Metric: true},
	{Name: "MEM", Right: true, Numeric: true, Metric: true},
	{Name: "MEM/RL", Wide: true, Right: true, Numeric: true},
	{Name: "%MEM/R", Right: true, Numeric: true, Metric: true},
	{Name: "%MEM/L", Right: true, Numeric: true, Metric: true},
	{Name: "IP"},
	{Name: "NODE"},
	{Name: "SERVICE-ACCOUNT", Wide: true},
	{Name: "NOMINATED NODE", Wide: true},
	{Name: "READINESS GATES", Wide: true},
	{Name: "QOS", Wide: true},
	{Name: "LABELS", Wide: true},
	{Name: "VALID", Wide: true},
	{Name: "AGE", Numeric: true},
}

func (pod) Columns() []Column {
	return append([]Column(nil), podColumns...)
}

func (pod) Row(obj runtime.Object, now time.Time) (Row, error) {
	po, ok := obj.(*corev1.Pod)
	if !ok || po == nil {
		return Row{}, fmt.Errorf("pod renderer: got %T", obj)
	}
	spec, st := &po.Spec, &po.Status

	ready, total, restarts, lastRestart := podContainerStats(spec, st)
	gatesReady, gates := podGateStats(spec, st)
	req, lim := podResources(spec)

	status := podStatus(po)
	valid := podDiagnose(status, ready, total, podCondition(st, corev1.PodReady), gatesReady, gates)

	b := NewRow(po).
		Text(po.Namespace).
		Text(po.Name).
		Text(strconv.Itoa(ready) + "/" + strconv.Itoa(total)).
		Text(workloadOr(status, UnknownValue)).
		Int(int64(restarts))
	if lastRestart.IsZero() {
		b.Num(NAValue, 0)
	} else {
		b.Age(lastRestart, now)
	}
	// The metric cells are NAValue until Meter fills them.
	b.Num(NAValue, 0).
		Num(workloadCPU(req)+":"+workloadCPU(lim), float64(req.cpu)).
		Num(NAValue, 0).
		Num(NAValue, 0).
		Num(NAValue, 0).
		Num(workloadMem(req)+":"+workloadMem(lim), float64(req.mem)).
		Num(NAValue, 0).
		Num(NAValue, 0).
		Text(workloadOr(st.PodIP, NAValue)).
		Text(workloadOr(spec.NodeName, NAValue)).
		Text(workloadOr(spec.ServiceAccountName, NAValue)).
		Text(Or(st.NominatedNodeName)).
		Text(podGates(gatesReady, gates)).
		Text(podQOS(st.QOSClass)).
		Map(po.Labels).
		Valid(valid).
		Age(po.CreationTimestamp.Time, now).
		Health(podHealth(status, valid))
	return b.Row(), nil
}

// Meter sets the pod's metric cells from the usage of its containers,
// summed: only the init, main and ephemeral containers its spec names,
// of which metrics-server reports those that run. The percentages are of
// the requests and limits the CPU/RL and MEM/RL cells show. A usage
// that names none of the pod's containers leaves the cells NAValue, as
// a pod the reading did not cover.
func (pod) Meter(row *Row, obj runtime.Object, usage map[string]Usage) {
	po, ok := obj.(*corev1.Pod)
	if !ok || po == nil {
		return
	}
	var sum Usage
	found := false
	add := func(name string) {
		if u, ok := usage[name]; ok {
			sum.CPU += u.CPU
			sum.Mem += u.Mem
			found = true
		}
	}
	for i := range po.Spec.InitContainers {
		add(po.Spec.InitContainers[i].Name)
	}
	for i := range po.Spec.Containers {
		add(po.Spec.Containers[i].Name)
	}
	for i := range po.Spec.EphemeralContainers {
		add(po.Spec.EphemeralContainers[i].Name)
	}
	if !found {
		return
	}
	req, lim := podResources(&po.Spec)
	metricMeterPod(row, podMetricCells, sum, req, lim)
}

// podContainerStats counts the pod's containers and those that are
// ready, and adds up their restarts, as k9s does: the containers and the
// sidecars, the init containers that run beside them. lastRestart is the
// latest time one of them ended before it was restarted; it is zero when
// none has.
func podContainerStats(spec *corev1.PodSpec, st *corev1.PodStatus) (ready, total, restarts int, lastRestart time.Time) {
	count := func(cs *corev1.ContainerStatus) {
		if cs.Ready {
			ready++
		}
		restarts += int(cs.RestartCount)
		if t := cs.LastTerminationState.Terminated; t != nil && t.FinishedAt.After(lastRestart) {
			lastRestart = t.FinishedAt.Time
		}
	}
	for i := range st.ContainerStatuses {
		count(&st.ContainerStatuses[i])
	}
	total = len(spec.Containers)

	sidecars := make(map[string]bool, len(spec.InitContainers))
	for i := range spec.InitContainers {
		if podIsSidecar(&spec.InitContainers[i]) {
			sidecars[spec.InitContainers[i].Name] = true
		}
	}
	for i := range st.InitContainerStatuses {
		if sidecars[st.InitContainerStatuses[i].Name] {
			total++
			count(&st.InitContainerStatuses[i])
		}
	}
	return ready, total, restarts, lastRestart
}

// podGateStats counts the pod's readiness gates and those whose
// condition is true.
func podGateStats(spec *corev1.PodSpec, st *corev1.PodStatus) (ready, total int) {
	for _, g := range spec.ReadinessGates {
		for _, c := range st.Conditions {
			if c.Type == g.ConditionType {
				if c.Status == corev1.ConditionTrue {
					ready++
				}
				break
			}
		}
	}
	return ready, len(spec.ReadinessGates)
}

// podGates is the READINESS GATES cell: "1/2", or MissingValue for a pod
// without gates.
func podGates(ready, total int) string {
	if total == 0 {
		return MissingValue
	}
	return strconv.Itoa(ready) + "/" + strconv.Itoa(total)
}

// podDiagnose returns what is wrong with a pod, in k9s's words, or ""
// when nothing is. A completed pod is fine.
func podDiagnose(status string, ready, total int, readyCondition bool, gatesReady, gates int) string {
	switch {
	case status == podStatusCompleted:
		return ""
	case ready != total || total == 0:
		return fmt.Sprintf("container ready check failed: %d of %d", ready, total)
	case gates > 0 && gatesReady != gates:
		return fmt.Sprintf("readiness gate check failed: %d of %d", gatesReady, gates)
	case !readyCondition:
		return "pod condition ready is false"
	case status == podStatusTerminating:
		return "pod is terminating"
	}
	return ""
}

// podQOS is the QOS cell: the class abbreviated as k9s does it.
func podQOS(class corev1.PodQOSClass) string {
	switch class {
	case corev1.PodQOSGuaranteed:
		return "GA"
	case corev1.PodQOSBurstable:
		return "BU"
	}
	return "BE"
}

// workloadOr returns s, or blank when s is empty.
func workloadOr(s, blank string) string {
	if s == "" {
		return blank
	}
	return s
}

// podResources adds up the requests and the limits of the pod's
// containers and sidecars, as k9s does.
func podResources(spec *corev1.PodSpec) (req, lim workloadQuantities) {
	add := func(c *corev1.Container) {
		r, l := workloadResources(c)
		req.cpu += r.cpu
		req.mem += r.mem
		lim.cpu += l.cpu
		lim.mem += l.mem
	}
	for i := range spec.InitContainers {
		if podIsSidecar(&spec.InitContainers[i]) {
			add(&spec.InitContainers[i])
		}
	}
	for i := range spec.Containers {
		add(&spec.Containers[i])
	}
	return req, lim
}

// workloadQuantities is CPU in millicores and memory in bytes.
type workloadQuantities struct {
	cpu, mem int64
}

// workloadResources returns the requests and the limits of a container.
// A container without requests is counted with its limits as its
// requests, as k9s does.
func workloadResources(c *corev1.Container) (req, lim workloadQuantities) {
	requests := c.Resources.Requests
	if len(requests) == 0 {
		requests = c.Resources.Limits
	}
	req.cpu, req.mem = requests.Cpu().MilliValue(), requests.Memory().Value()
	lim.cpu, lim.mem = c.Resources.Limits.Cpu().MilliValue(), c.Resources.Limits.Memory().Value()
	return req, lim
}

// workloadMebi is a mebibyte.
const workloadMebi = 1024 * 1024

// workloadCPU prints CPU as k9s does: millicores without a unit, "0"
// when there is none.
func workloadCPU(q workloadQuantities) string {
	return strconv.FormatInt(q.cpu, 10)
}

// workloadMem prints memory as k9s does: whole mebibytes without a
// unit, "0" when there is none.
func workloadMem(q workloadQuantities) string {
	return strconv.FormatInt(q.mem/workloadMebi, 10)
}
