package render

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// ContainerColumns returns the columns of the table of a pod's
// containers.
func ContainerColumns() []Column {
	panic("not implemented")
}

// ContainerRows returns one row for each container of pod: its init
// containers, its containers and its ephemeral containers, in that
// order. A row's Name is the container's and its Namespace the pod's.
func ContainerRows(pod *corev1.Pod, now time.Time) []Row {
	panic("not implemented")
}
