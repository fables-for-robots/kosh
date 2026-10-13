// Follows k9s's internal/render/container.go (Apache-2.0).

package render

import (
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var containerColumns = []Column{
	{Name: "IDX"},
	{Name: "NAME"},
	{Name: "IMAGE"},
	{Name: "READY"},
	{Name: "STATE"},
	{Name: "RESTARTS", Right: true, Numeric: true},
	{Name: "PROBES(L:R:S)"},
	{Name: "CPU/RL", Right: true, Numeric: true},
	{Name: "MEM/RL", Right: true, Numeric: true},
	{Name: "PORTS"},
	{Name: "VALID", Wide: true},
	{Name: "AGE", Numeric: true},
}

// ContainerColumns returns the columns of the table of a pod's
// containers.
func ContainerColumns() []Column {
	return append([]Column(nil), containerColumns...)
}

// ContainerRows returns one row for each container of pod: its init
// containers, its containers and its ephemeral containers, in that
// order. A row's Name is the container's and its Namespace the pod's.
func ContainerRows(pod *corev1.Pod, now time.Time) []Row {
	if pod == nil {
		return nil
	}
	spec, st := &pod.Spec, &pod.Status
	rows := make([]Row, 0, len(spec.InitContainers)+len(spec.Containers)+len(spec.EphemeralContainers))
	for i := range spec.InitContainers {
		c := &spec.InitContainers[i]
		rows = append(rows, containerRow(pod, "I"+strconv.Itoa(i+1), c, containerStatusOf(st.InitContainerStatuses, c.Name), now))
	}
	for i := range spec.Containers {
		c := &spec.Containers[i]
		rows = append(rows, containerRow(pod, "M"+strconv.Itoa(i+1), c, containerStatusOf(st.ContainerStatuses, c.Name), now))
	}
	for i := range spec.EphemeralContainers {
		c := corev1.Container(spec.EphemeralContainers[i].EphemeralContainerCommon)
		rows = append(rows, containerRow(pod, "E"+strconv.Itoa(i+1), &c, containerStatusOf(st.EphemeralContainerStatuses, c.Name), now))
	}
	return rows
}

// containerStatusOf returns the status of the container named name, or
// nil when it has none yet.
func containerStatusOf(ss []corev1.ContainerStatus, name string) *corev1.ContainerStatus {
	for i := range ss {
		if ss[i].Name == name {
			return &ss[i]
		}
	}
	return nil
}

// containerRow is the row of one container of pod; idx tells init (I),
// main (M) and ephemeral (E) containers apart.
func containerRow(pod *corev1.Pod, idx string, c *corev1.Container, cs *corev1.ContainerStatus, now time.Time) Row {
	ready, state, restarts := false, MissingValue, int32(0)
	if cs != nil {
		ready, state, restarts = cs.Ready, containerState(cs.State), cs.RestartCount
	}
	completed := cs != nil && cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0
	valid := ""
	if !completed && !ready {
		valid = "container is not ready"
	}
	req, lim := workloadResources(c)

	b := NewRow(&metav1.ObjectMeta{Namespace: pod.Namespace, Name: c.Name}).
		Text(idx).
		Text(c.Name).
		Text(Or(c.Image)).
		Text(strconv.FormatBool(ready)).
		Text(state).
		Int(int64(restarts)).
		Text(containerProbe(c.LivenessProbe)+":"+containerProbe(c.ReadinessProbe)+":"+containerProbe(c.StartupProbe)).
		Num(workloadCPU(req)+":"+workloadCPU(lim), float64(req.cpu)).
		Num(workloadMem(req)+":"+workloadMem(lim), float64(req.mem)).
		Text(containerPorts(c.Ports)).
		Valid(valid).
		Age(containerSince(pod, cs), now).
		Health(containerHealth(cs, completed, valid))
	return b.Row()
}

// containerState is the STATE cell: the reason the container waits or
// ended, or that it runs.
func containerState(s corev1.ContainerState) string {
	switch {
	case s.Waiting != nil:
		return workloadOr(s.Waiting.Reason, "Waiting")
	case s.Terminated != nil:
		return workloadOr(s.Terminated.Reason, "Terminating")
	case s.Running != nil:
		return "Running"
	}
	return MissingValue
}

// containerHealth is the health of a container: completed when it ended
// well, on its way while it is created or its pod initialized, and an
// error when it waits or ended otherwise or runs without being ready.
func containerHealth(cs *corev1.ContainerStatus, completed bool, valid string) Health {
	switch {
	case completed:
		return Completed
	case cs != nil && cs.State.Waiting != nil &&
		(cs.State.Waiting.Reason == podStatusContainerCreating || cs.State.Waiting.Reason == podStatusPodInitializing):
		return Pending
	case valid != "":
		return Error
	}
	return OK
}

// containerSince is when the container entered its state: started for a
// running one, finished for one that ended, and otherwise when the pod
// was created.
func containerSince(pod *corev1.Pod, cs *corev1.ContainerStatus) time.Time {
	if cs != nil {
		switch {
		case cs.State.Running != nil && !cs.State.Running.StartedAt.IsZero():
			return cs.State.Running.StartedAt.Time
		case cs.State.Terminated != nil && !cs.State.Terminated.FinishedAt.IsZero():
			return cs.State.Terminated.FinishedAt.Time
		}
	}
	return pod.CreationTimestamp.Time
}

// containerProbe is "on" for a probe there is, and "off" for none.
func containerProbe(p *corev1.Probe) string {
	if p == nil {
		return "off"
	}
	return "on"
}

// containerPorts is the PORTS cell: "http:8080,9090╱UDP", the protocol
// shown when it is not TCP, or MissingValue for no ports.
func containerPorts(pp []corev1.ContainerPort) string {
	if len(pp) == 0 {
		return MissingValue
	}
	var sb strings.Builder
	for i, p := range pp {
		if i > 0 {
			sb.WriteByte(',')
		}
		if p.Name != "" {
			sb.WriteString(p.Name)
			sb.WriteByte(':')
		}
		sb.WriteString(strconv.Itoa(int(p.ContainerPort)))
		if p.Protocol != "" && p.Protocol != corev1.ProtocolTCP {
			sb.WriteString("╱")
			sb.WriteString(string(p.Protocol))
		}
	}
	return sb.String()
}
