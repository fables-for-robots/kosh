package tui

import (
	"context"
	"slices"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
)

// The keys that change the cluster (spec §11.1). Each asks first, in a
// dialog that starts on Cancel, and none is a key of a read-only grant.

// actionBindings are the keys of the actions, which init binds.
var actionBindings = []binding{
	{Key: "ctrl+d", Hint: hint{"ctrl-d", "Delete"}, Order: 30, Applies: actionApplies(actionCan("delete")), Run: actionGuard(actionDelete)},
	{Key: "e", Hint: hint{"e", "Edit"}, Order: 31, Applies: actionApplies(actionCan("update")), Run: actionGuard(editorOpen)},
	{Key: "ctrl+k", Hint: hint{"ctrl-k", "Kill"}, Order: 32, Applies: actionApplies(actionIs("", "pods")), Run: actionGuard(actionKill)},
	{Key: "s", Hint: hint{"s", "Scale"}, Order: 33, Applies: actionApplies(actionIs("apps", "deployments", "statefulsets", "replicasets")), Run: actionGuard(actionScale)},
	{Key: "r", Hint: hint{"r", "Restart"}, Order: 34, Applies: actionApplies(actionIs("apps", "deployments", "statefulsets", "daemonsets")), Run: actionGuard(actionRestart)},
	{Key: "i", Hint: hint{"i", "Set Image"}, Order: 35, Applies: actionApplies(actionIs("apps", "deployments", "statefulsets", "daemonsets")), Run: actionGuard(actionSetImage)},
	{Key: "ctrl+l", Hint: hint{"ctrl-l", "Rollback"}, Order: 36, Applies: actionApplies(actionIs("apps", "replicasets")), Run: actionGuard(actionRollback)},
	{Key: "t", Hint: hint{"t", "Trigger"}, Order: 37, Applies: actionApplies(actionIs("batch", "cronjobs")), Run: actionGuard(actionTrigger)},
	{Key: "s", Hint: hint{"s", "Suspend/Resume"}, Order: 38, Applies: actionApplies(actionIs("batch", "cronjobs")), Run: actionGuard(actionSuspend)},
	{Key: "c", Hint: hint{"c", "Cordon"}, Order: 39, Applies: actionApplies(actionIs("", "nodes")), Run: actionGuard(actionCordon(true))},
	{Key: "u", Hint: hint{"u", "Uncordon"}, Order: 40, Applies: actionApplies(actionIs("", "nodes")), Run: actionGuard(actionCordon(false))},
	{Key: "r", Hint: hint{"r", "Drain"}, Order: 41, Applies: actionApplies(actionIs("", "nodes")), Run: actionGuard(actionDrain)},
}

func init() { bind(actionBindings...) }

// actionApplies is the one check of whether a key of an action applies:
// never for a read-only grant (spec §12), never to the containers or the
// rules, which are no objects of the API, and otherwise when kind says
// the resource has it.
func actionApplies(kind func(kube.Resource) bool) func(kube.Resource, keys.Grant) bool {
	return func(res kube.Resource, grant keys.Grant) bool {
		if grant.ReadOnly || res.GVR == kube.Containers.GVR || res.GVR == kube.Rules.GVR {
			return false
		}
		return kind(res)
	}
}

// actionIs returns whether a resource is one of the resources of group.
func actionIs(group string, resources ...string) func(kube.Resource) bool {
	return func(res kube.Resource) bool {
		return slices.ContainsFunc(resources, func(r string) bool { return res.Is(group, r) })
	}
}

// actionCan returns whether the API offers verb for a resource.
func actionCan(verb string) func(kube.Resource) bool {
	return func(res kube.Resource) bool { return res.Can(verb) }
}

// actionSimple returns the Run of an action that only asks, and then
// calls do for each row.
func actionSimple(w actionWords, do func(sess kube.Session, ctx context.Context, ref kube.ObjectRef) (string, error)) actionRunFunc {
	return func(e env, _ kube.Query, refs []kube.ObjectRef) tea.Cmd {
		return actionAsk(e, refs, w, apiTimeout, func(ctx context.Context, ref kube.ObjectRef) (string, error) {
			return do(e.sess, ctx, ref)
		})
	}
}

var (
	actionKill = actionSimple(actionWords{ask: "Kill", button: "Kill", done: "Killed"},
		func(sess kube.Session, ctx context.Context, ref kube.ObjectRef) (string, error) {
			return "", sess.Kill(ctx, ref)
		})
	actionRestart = actionSimple(actionWords{ask: "Restart", button: "Restart", done: "Restarted"},
		func(sess kube.Session, ctx context.Context, ref kube.ObjectRef) (string, error) {
			return "", sess.Restart(ctx, ref)
		})
	actionRollback = actionSimple(actionWords{ask: "Roll back to", button: "Roll Back", done: "Rolled back to"},
		func(sess kube.Session, ctx context.Context, ref kube.ObjectRef) (string, error) {
			return "", sess.Rollback(ctx, ref)
		})
	actionTrigger = actionSimple(actionWords{ask: "Trigger", button: "Trigger", done: "Triggered"},
		func(sess kube.Session, ctx context.Context, ref kube.ObjectRef) (string, error) {
			job, err := sess.Trigger(ctx, ref)
			if err != nil {
				return "", err
			}
			return " as job " + job, nil
		})
)

// actionCordon returns the Run of cordon (on) or uncordon.
func actionCordon(on bool) actionRunFunc {
	w := actionWords{ask: "Cordon", button: "Cordon", done: "Cordoned"}
	if !on {
		w = actionWords{ask: "Uncordon", button: "Uncordon", done: "Uncordoned"}
	}
	return actionSimple(w, func(sess kube.Session, ctx context.Context, ref kube.ObjectRef) (string, error) {
		return "", sess.Cordon(ctx, ref, on)
	})
}

// actionSuspend asks whether to suspend the cronjobs or resume them: the
// row says which they are now, and the key cannot see the row.
func actionSuspend(e env, _ kube.Query, refs []kube.ObjectRef) tea.Cmd {
	button := func(label, done string, suspend bool) formButton {
		return formButton{label: label, press: func(*formDialog) (tea.Cmd, string) {
			return actionStart(e, refs, done, apiTimeout, func(ctx context.Context, ref kube.ObjectRef) (string, error) {
				return "", e.sess.SetSuspended(ctx, ref, suspend)
			}), ""
		}}
	}
	return openDialog(newConfirmDialog(actionQuestion("Suspend or resume", refs), actionNames(refs),
		button("Suspend", "Suspended", true), button("Resume", "Resumed", false)))
}

// actionDelete asks how to delete, and deletes.
func actionDelete(e env, _ kube.Query, refs []kube.ObjectRef) tea.Cmd {
	prop := newChoiceField("Propagation", string(kube.PropagationBackground), string(kube.PropagationForeground), string(kube.PropagationOrphan))
	force := newCheckField("Force", false)
	return openDialog(newFormDialog(actionQuestion("Delete", refs), actionNames(refs), []*formField{prop, force}, formButton{
		label: "Delete",
		press: func(*formDialog) (tea.Cmd, string) {
			opts := kube.DeleteOptions{Propagation: kube.Propagation(prop.value()), Force: force.on}
			return actionStart(e, refs, "Deleted", apiTimeout, func(ctx context.Context, ref kube.ObjectRef) (string, error) {
				return "", e.sess.Delete(ctx, ref, opts)
			}), ""
		},
	}))
}

// actionScale reads the replicas of the first row in a command, and then
// asks for the number to scale the rows to.
func actionScale(e env, _ kube.Query, refs []kube.ObjectRef) tea.Cmd {
	sess := e.sess
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		defer cancel()
		n, err := sess.Replicas(ctx, refs[0])
		if err != nil {
			return flashMsg{text: err.Error(), err: true}
		}
		field := newNumberField("Replicas", strconv.Itoa(int(n)), false)
		return openDialogMsg{newFormDialog(actionQuestion("Scale", refs), actionNames(refs), []*formField{field}, formButton{
			label: "Scale",
			press: func(*formDialog) (tea.Cmd, string) {
				replicas, err := strconv.ParseInt(field.value(), 10, 32)
				if err != nil {
					return nil, "Replicas must be a number from 0 to 2147483647"
				}
				return actionStart(e, refs, "Scaled", apiTimeout, func(ctx context.Context, ref kube.ObjectRef) (string, error) {
					return "", sess.Scale(ctx, ref, int32(replicas))
				}), ""
			},
		})}
	}
}

// actionSetImage reads the containers of the first row in a command, and
// then asks for their images; it sends only those that were changed.
func actionSetImage(e env, _ kube.Query, refs []kube.ObjectRef) tea.Cmd {
	sess, ref := e.sess, refs[0]
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		defer cancel()
		containers, err := sess.Images(ctx, ref)
		if err != nil {
			return flashMsg{text: err.Error(), err: true}
		}
		fields := make([]*formField, len(containers))
		was := make([]string, len(containers))
		for i, c := range containers {
			label := cleanLine(c.Name)
			if c.Init {
				label += " (init)"
			}
			fields[i] = newTextField(label, cleanLine(c.Image))
			was[i] = fields[i].value()
		}
		return openDialogMsg{newFormDialog(actionQuestion("Set the images of", refs[:1]), nil, fields, formButton{
			label: "Set",
			press: func(*formDialog) (tea.Cmd, string) {
				images := map[string]string{}
				for i, c := range containers {
					if v := fields[i].value(); v != was[i] {
						images[c.Name] = v
					}
				}
				if len(images) == 0 {
					return flash(textNothingChanged), ""
				}
				return actionStart(e, refs[:1], "Set the images of", apiTimeout, func(ctx context.Context, ref kube.ObjectRef) (string, error) {
					return "", sess.SetImages(ctx, ref, images)
				}), ""
			},
		})}
	}
}

// actionDrain asks how to drain, starting from kubectl's defaults, and
// drains. A drain's call may take the dialog's timeout and apiTimeout
// besides; without a timeout it may take as long as it takes.
func actionDrain(e env, _ kube.Query, refs []kube.ObjectRef) tea.Cmd {
	def := kube.DefaultDrainOptions()
	grace := newNumberField("Grace Period", strconv.Itoa(def.GracePeriod), true)
	timeout := newTextField("Timeout", def.Timeout.String())
	ignore := newCheckField("Ignore DaemonSets", def.IgnoreDaemonSets)
	emptyDir := newCheckField("Delete EmptyDir Data", def.DeleteEmptyDirData)
	force := newCheckField("Force", def.Force)
	noEvict := newCheckField("Disable Eviction", def.DisableEviction)
	fields := []*formField{grace, timeout, ignore, emptyDir, force, noEvict}
	return openDialog(newFormDialog(actionQuestion("Drain", refs), actionNames(refs), fields, formButton{
		label: "Drain",
		press: func(*formDialog) (tea.Cmd, string) {
			g, err := strconv.Atoi(grace.value())
			if err != nil {
				return nil, "Grace Period must be a number of seconds, or -1 for each pod's own"
			}
			t, err := time.ParseDuration(timeout.value())
			if err != nil || t < 0 {
				return nil, "Timeout must be a duration such as 5s or 2m30s, or 0 for none"
			}
			opts := kube.DrainOptions{
				GracePeriod:        g,
				Timeout:            t,
				IgnoreDaemonSets:   ignore.on,
				DeleteEmptyDirData: emptyDir.on,
				Force:              force.on,
				DisableEviction:    noEvict.on,
			}
			limit := time.Duration(0)
			if t != 0 {
				limit = t + apiTimeout
			}
			return actionStart(e, refs, "Drained", limit, func(ctx context.Context, ref kube.ObjectRef) (string, error) {
				return "", e.sess.Drain(ctx, ref, opts)
			}), ""
		},
	}))
}
