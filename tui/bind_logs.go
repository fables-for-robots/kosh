package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
)

// The keys of the logs: l the log, p the log of the previous run. Both
// need the right to read the logs of pods, whatever the table is of: the
// logs of a workload or a service are its pods'.
func init() {
	bind(
		binding{
			Key:     "l",
			Hint:    hint{"l", "Logs"},
			Order:   20,
			Applies: logHasLogs,
			Needs:   logNeeds,
			Run:     logRun(false),
		},
		binding{
			Key:     "p",
			Hint:    hint{"p", "Logs Previous"},
			Order:   21,
			Applies: logHasPrevious,
			Needs:   logNeeds,
			Run:     logRun(true),
		},
	)
}

// logNeeds are the needs of l and p: get on the pods' log.
var logNeeds = []need{{Verb: "get", Sub: "log", Resource: needPods}}

// logIsContainers reports whether res is the containers of a pod.
func logIsContainers(res kube.Resource) bool {
	return res.Is(kube.Containers.GVR.Group, kube.Containers.GVR.Resource)
}

// logHasPrevious reports whether a table of res has p: of pods and of
// containers.
func logHasPrevious(res kube.Resource, _ keys.Grant) bool {
	return res.Is("", "pods") || logIsContainers(res)
}

// logHasLogs reports whether a table of res has l: of pods, containers,
// and the workloads and services whose pods' logs are read.
func logHasLogs(res kube.Resource, g keys.Grant) bool {
	switch {
	case logHasPrevious(res, g),
		res.Is("apps", "deployments"), res.Is("apps", "replicasets"),
		res.Is("apps", "statefulsets"), res.Is("apps", "daemonsets"),
		res.Is("batch", "jobs"), res.Is("", "services"):
		return true
	}
	return false
}

// logRun opens the log of the first of refs: in a table of containers
// the log of the table's pod and of the row's container, and elsewhere
// the log of the row.
func logRun(previous bool) func(e env, q kube.Query, refs []kube.ObjectRef) tea.Cmd {
	return func(e env, q kube.Query, refs []kube.ObjectRef) tea.Cmd {
		if len(refs) == 0 {
			return nil
		}
		ref, opts := refs[0], kube.LogOptions{Previous: previous}
		if logIsContainers(q.Resource) {
			if q.Of == nil {
				return nil
			}
			ref, opts.Container = *q.Of, refs[0].Name
		}
		return logOpen(e, ref, opts)
	}
}
