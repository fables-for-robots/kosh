// Follows k9s's internal/render/pod.go (Apache-2.0).

package render

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// The pod's statuses that its health is read from.
const (
	podStatusRunning           = "Running"
	podStatusPending           = "Pending"
	podStatusCompleted         = "Completed"
	podStatusContainerCreating = "ContainerCreating"
	podStatusPodInitializing   = "PodInitializing"
	podStatusTerminating       = "Terminating"
	podStatusUnknown           = "Unknown"
	// podNodeLost is the reason of a pod whose node no longer answers.
	podNodeLost = "NodeLost"
)

// podStatus returns the pod's STATUS, as kubectl get pods prints it:
// the phase, overridden by the pod's reason, by an init container that
// has not finished, by a container that waits or ended with a reason,
// and by Terminating once the pod is being deleted. It is empty for a
// pod with no phase and nothing to override it.
func podStatus(po *corev1.Pod) string {
	st := &po.Status
	deleting := po.DeletionTimestamp != nil

	status := string(st.Phase)
	if st.Reason != "" {
		if deleting && st.Reason == podNodeLost {
			return podStatusUnknown
		}
		status = st.Reason
	}

	if s, ok := podInitStatus(&po.Spec, st); ok {
		return s
	}

	status, running := podContainersStatus(st, status)
	if running && status == podStatusCompleted {
		status = podStatusRunning
	}
	if deleting {
		return podStatusTerminating
	}
	return status
}

// podInitStatus returns the status the first init container that has
// not finished gives the pod, and false when every one has finished.
func podInitStatus(spec *corev1.PodSpec, st *corev1.PodStatus) (string, bool) {
	sidecars := make(map[string]bool, len(spec.InitContainers))
	for i := range spec.InitContainers {
		if podIsSidecar(&spec.InitContainers[i]) {
			sidecars[spec.InitContainers[i].Name] = true
		}
	}
	for i := range st.InitContainerStatuses {
		cs := &st.InitContainerStatuses[i]
		if s := podInitContainerStatus(cs, i, len(spec.InitContainers), sidecars[cs.Name]); s != "" {
			return s, true
		}
	}
	return "", false
}

// podInitContainerStatus returns the status the init container at idx
// of total gives the pod, or "" when it has done its part: it ended
// well, or it is a sidecar that ended or is started and ready.
func podInitContainerStatus(cs *corev1.ContainerStatus, idx, total int, sidecar bool) string {
	switch {
	case cs.State.Terminated != nil:
		t := cs.State.Terminated
		if t.ExitCode == 0 || sidecar {
			return ""
		}
		if t.Reason != "" {
			return "Init:" + t.Reason
		}
		if t.Signal != 0 {
			return "Init:Signal:" + strconv.Itoa(int(t.Signal))
		}
		return "Init:ExitCode:" + strconv.Itoa(int(t.ExitCode))
	case sidecar && cs.Started != nil && *cs.Started:
		if cs.Ready {
			return ""
		}
	case cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != podStatusPodInitializing:
		return "Init:" + cs.State.Waiting.Reason
	}
	return "Init:" + strconv.Itoa(idx) + "/" + strconv.Itoa(total)
}

// podContainersStatus overrides status by the first container, in the
// order of the pod, that waits or ended, and says whether a container is
// running and ready.
func podContainersStatus(st *corev1.PodStatus, status string) (string, bool) {
	var running bool
	for i := len(st.ContainerStatuses) - 1; i >= 0; i-- {
		cs := &st.ContainerStatuses[i]
		switch {
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
			status = cs.State.Waiting.Reason
		case cs.State.Terminated != nil && cs.State.Terminated.Reason != "":
			status = cs.State.Terminated.Reason
		case cs.State.Terminated != nil:
			if cs.State.Terminated.Signal != 0 {
				status = "Signal:" + strconv.Itoa(int(cs.State.Terminated.Signal))
			} else {
				status = "ExitCode:" + strconv.Itoa(int(cs.State.Terminated.ExitCode))
			}
		case cs.Ready && cs.State.Running != nil:
			running = true
		}
	}
	return status, running
}

// podIsSidecar says whether an init container is a sidecar: one that is
// restarted always and runs beside the pod's containers.
func podIsSidecar(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// podHealth returns the health of a pod from its status, as k9s colours
// it: on its way while it is scheduled, created, initialized or deleted,
// completed when it is done, and an error otherwise. A running pod is
// as its VALID cell says, which valid is.
func podHealth(status string, valid string) Health {
	switch {
	case status == podStatusPending, status == podStatusContainerCreating,
		status == podStatusPodInitializing, status == podStatusTerminating,
		podInitializing(status):
		return Pending
	case status == podStatusCompleted:
		return Completed
	case status == podStatusRunning && valid == "":
		return OK
	}
	return Error
}

// podInitializing says whether status is that of init containers still
// at work, "Init:1/3", and not that of one that failed.
func podInitializing(status string) bool {
	rest, ok := strings.CutPrefix(status, "Init:")
	if !ok {
		return false
	}
	done, total, ok := strings.Cut(rest, "/")
	if !ok {
		return false
	}
	_, err1 := strconv.Atoi(done)
	_, err2 := strconv.Atoi(total)
	return err1 == nil && err2 == nil
}
