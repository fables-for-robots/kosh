// Derived from k9s's internal/render/pod.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.
// It is k9s's PodStatus, which follows kubectl's printPod.

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
	podStatusNotReady          = "NotReady"
	podStatusUnknown           = "Unknown"
	// podNodeLost is the reason of a pod whose node no longer answers.
	podNodeLost = "NodeLost"
)

// podStatus returns the pod's STATUS, as kubectl get pods prints it
// (printPod in kubernetes' pkg/printers/internalversion, which k9s's
// PodStatus follows): the phase, overridden in turn by the pod's reason,
// by a scheduling gate, by the first init container that has not done
// its part, by the first container that waits or ended, and last by
// Unknown or Terminating once the pod is being deleted, Terminating only
// while its phase is not Succeeded or Failed. It is empty for
// a pod with no phase and nothing to override it.
func podStatus(po *corev1.Pod) string {
	st := &po.Status
	reason := string(st.Phase)
	if st.Reason != "" {
		reason = st.Reason
	}
	for _, c := range st.Conditions {
		if c.Type == corev1.PodScheduled && c.Reason == corev1.PodReasonSchedulingGated {
			reason = corev1.PodReasonSchedulingGated
		}
	}

	initReason, initializing := podInitStatus(&po.Spec, st)
	if initializing {
		reason = initReason
	}
	if !initializing || podCondition(st, corev1.PodInitialized) {
		reason = podContainersStatus(st, reason)
	}

	// As kubectl, and unlike k9s's PodStatus: a pod that has already
	// succeeded or failed shows what it ended as, deleted or not.
	terminal := st.Phase == corev1.PodSucceeded || st.Phase == corev1.PodFailed
	switch {
	case po.DeletionTimestamp != nil && st.Reason == podNodeLost:
		reason = podStatusUnknown
	case po.DeletionTimestamp != nil && !terminal:
		reason = podStatusTerminating
	}
	return reason
}

// podInitStatus returns the status the first init container that has
// not done its part gives the pod, and false when every one has: it
// ended well, or it is a sidecar that has started.
func podInitStatus(spec *corev1.PodSpec, st *corev1.PodStatus) (string, bool) {
	sidecars := make(map[string]bool, len(spec.InitContainers))
	for i := range spec.InitContainers {
		if podIsSidecar(&spec.InitContainers[i]) {
			sidecars[spec.InitContainers[i].Name] = true
		}
	}
	for i := range st.InitContainerStatuses {
		cs := &st.InitContainerStatuses[i]
		switch {
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0:
			continue
		case sidecars[cs.Name] && cs.Started != nil && *cs.Started:
			continue
		case cs.State.Terminated != nil:
			return "Init:" + podTerminatedReason(cs.State.Terminated), true
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != podStatusPodInitializing:
			return "Init:" + cs.State.Waiting.Reason, true
		}
		return "Init:" + strconv.Itoa(i) + "/" + strconv.Itoa(len(spec.InitContainers)), true
	}
	return "", false
}

// podContainersStatus overrides reason by the first container, in the
// order of the pod, that waits or ended. A pod whose first container
// completed is Running when another runs and the pod is ready, has the
// reason of a container that failed when one did, and is NotReady when
// another runs and the pod is not ready.
func podContainersStatus(st *corev1.PodStatus, reason string) string {
	var running bool
	var failed string
	for i := len(st.ContainerStatuses) - 1; i >= 0; i-- {
		cs := &st.ContainerStatuses[i]
		switch {
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
			reason = cs.State.Waiting.Reason
		case cs.State.Terminated != nil:
			reason = podTerminatedReason(cs.State.Terminated)
			if cs.State.Terminated.ExitCode != 0 {
				failed = reason
			}
		case cs.Ready && cs.State.Running != nil:
			running = true
		}
	}
	if reason == podStatusCompleted {
		switch {
		case running && podCondition(st, corev1.PodReady):
			reason = podStatusRunning
		case failed != "":
			reason = failed
		case running:
			reason = podStatusNotReady
		}
	}
	return reason
}

// podTerminatedReason is why a container ended: its reason, or else the
// signal or the exit code, "Signal:9", "ExitCode:2".
func podTerminatedReason(t *corev1.ContainerStateTerminated) string {
	switch {
	case t.Reason != "":
		return t.Reason
	case t.Signal != 0:
		return "Signal:" + strconv.Itoa(int(t.Signal))
	}
	return "ExitCode:" + strconv.Itoa(int(t.ExitCode))
}

// podCondition says whether the pod's condition of type c is true.
func podCondition(st *corev1.PodStatus, c corev1.PodConditionType) bool {
	for _, cond := range st.Conditions {
		if cond.Type == c && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// podIsSidecar says whether an init container is a sidecar: one that is
// restarted always and runs beside the pod's containers.
func podIsSidecar(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// podHealth returns the health of a pod from its status, as k9s colours
// it: on its way while it is scheduled (or held from it), created,
// initialized or deleted, completed when it is done, and an error
// otherwise. A running pod is as its VALID cell says, which valid is.
func podHealth(status string, valid string) Health {
	switch {
	case status == podStatusPending, status == corev1.PodReasonSchedulingGated,
		status == podStatusContainerCreating,
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
