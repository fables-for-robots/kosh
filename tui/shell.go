package tui

import (
	"context"
	"errors"
	"fmt"
	"sync"

	tea "charm.land/bubbletea/v2"
	"sigs.k8s.io/yaml"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
)

// A shell in a pod (the shell's spec, §1 and §6): s on a pod, or on a
// container of a pod, reads the pod for its running containers, asks in
// a dialog which container and which command, and hands the session's
// terminal over to a function that runs the command there with Exec
// until it ends. The UI never touches the stream or the SSH session: it
// calls the session's Exec, and env.handover.
//
// The dialog is the one whose first enter does the thing: the focus
// starts in Command, and enter there presses Shell. Opening a shell
// changes nothing in the cluster, and the user asked to see the command
// before it starts, to change it when the image has no sh.
//
// The session's keys of actions are held from s until the return has
// been dealt with: by the reading, then by the dialog (given back when it
// closes with nothing done), then by the hand-over (given back when it
// returns, unless the command did not start and the dialog opens again,
// which then holds them).

// shellDefaultCommand is the Command field's text when the dialog opens:
// k9s's, bash when the image has it and sh otherwise.
const shellDefaultCommand = `sh -c "command -v bash >/dev/null && exec bash || exec sh"`

// shellAnnotation names the container chosen first.
const shellAnnotation = "kubectl.kubernetes.io/default-container"

// shellButton is the label of the button that starts the shell.
const shellButton = "Shell"

func init() {
	bind(binding{
		Key:      "s",
		Hint:     hint{"s", "Shell"},
		Order:    22, // after the logs' l and p
		Applies:  shellApplies,
		Run:      actionGuard(shellOpen),
		Handover: true,
	})
}

// shellApplies reports whether a table of res has s: of pods and of a
// pod's containers, for a full grant.
func shellApplies(res kube.Resource, grant keys.Grant) bool {
	return !grant.ReadOnly && (res.Is("", "pods") || logIsContainers(res))
}

// shellOpen reads the pod of the row, and opens the dialog when it has a
// container that runs. In a table of a pod's containers the pod is the
// table's and the container the row's.
func shellOpen(e env, q kube.Query, refs []kube.ObjectRef) tea.Cmd {
	if e.handover == nil {
		return nil
	}
	pod, container := refs[0], ""
	if logIsContainers(q.Resource) {
		if q.Of == nil {
			return nil
		}
		pod, container = *q.Of, refs[0].Name
	}
	sess := e.sess
	return actionRead(e, func(ctx context.Context, release func()) (tea.Msg, error) {
		text, err := sess.YAML(ctx, pod, false)
		if err != nil {
			return nil, err
		}
		names, at, err := shellRunning(text)
		if err != nil {
			return nil, fmt.Errorf("the pod %s/%s could not be read: %w", pod.Namespace, pod.Name, err)
		}
		if len(names) == 0 {
			release()
			return flashMsg{text: fmt.Sprintf("pod %s/%s has no running container", pod.Namespace, pod.Name)}, nil
		}
		s := &shell{e: e, pod: pod, names: names, at: at, release: release}
		if container != "" {
			s.names, s.at, s.fixed = []string{container}, 0, true
		}
		return openDialogMsg{s.dialog(shellDefaultCommand, "")}, nil
	})
}

// shellPod is what the dialog reads of a pod's YAML.
type shellPod struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Containers          []shellPodName `json:"containers"`
		InitContainers      []shellPodName `json:"initContainers"`
		EphemeralContainers []shellPodName `json:"ephemeralContainers"`
	} `json:"spec"`
	Status struct {
		ContainerStatuses          []shellPodStatus `json:"containerStatuses"`
		InitContainerStatuses      []shellPodStatus `json:"initContainerStatuses"`
		EphemeralContainerStatuses []shellPodStatus `json:"ephemeralContainerStatuses"`
	} `json:"status"`
}

// shellPodName is a container of a pod's spec.
type shellPodName struct {
	Name string `json:"name"`
}

// shellPodStatus is a container's status: whether it runs.
type shellPodStatus struct {
	Name  string `json:"name"`
	State struct {
		Running *struct{} `json:"running"`
	} `json:"state"`
}

// shellRunning returns the containers of a pod's YAML that run now, in
// the order its containers, its init containers, its ephemeral
// containers; and the index of the one named by the annotation, or 0.
// An error is a YAML that does not decode.
func shellRunning(text string) (names []string, at int, err error) {
	var p shellPod
	if err := yaml.Unmarshal([]byte(text), &p); err != nil {
		return nil, 0, err
	}
	add := func(spec []shellPodName, status []shellPodStatus) {
		runs := map[string]bool{}
		for _, s := range status {
			if s.State.Running != nil {
				runs[s.Name] = true
			}
		}
		for _, c := range spec {
			if runs[c.Name] {
				names = append(names, c.Name)
			}
		}
	}
	add(p.Spec.Containers, p.Status.ContainerStatuses)
	add(p.Spec.InitContainers, p.Status.InitContainerStatuses)
	add(p.Spec.EphemeralContainers, p.Status.EphemeralContainerStatuses)
	if def, ok := p.Metadata.Annotations[shellAnnotation]; ok {
		for i, n := range names {
			if n == def {
				at = i
				break
			}
		}
	}
	return names, at, nil
}

// shell is one s: the pod, the containers to choose from and the one
// chosen, and what gives the keys of actions back.
type shell struct {
	e   env
	pod kube.ObjectRef
	// names are the containers, as the API names them; at is the one
	// chosen. fixed: the container cannot be changed.
	names []string
	at    int
	fixed bool
	// release gives the session's keys of actions back; it does so once.
	release func()
}

// dialog returns the dialog with command in the Command field and why
// under the fields. It holds the keys of actions until it closes with
// nothing done, or Shell hands them on with the terminal.
func (s *shell) dialog(command, why string) *formDialog {
	var container *formField
	if s.fixed || len(s.names) == 1 {
		container = formFixedField("Container", cleanLine(s.names[s.at]))
	} else {
		shown := make([]string, len(s.names))
		for i, n := range s.names {
			shown[i] = cleanLine(n)
		}
		container = formPickField("Container", s.at, shown...)
	}
	cmd := formTextField("Command", command)
	cmd.presses, cmd.scrolls = shellButton, true
	d := dialogForm("A shell in pod "+s.pod.Namespace+"/"+s.pod.Name, nil, []*formField{container, cmd}, formButton{
		label: shellButton,
		after: true,
		press: func(*formDialog) (tea.Cmd, string) {
			text := cmd.value()
			words, err := commandWords(text)
			if err != nil {
				return nil, err.Error()
			}
			if container.kind == formIsChoice {
				s.at = container.at
			}
			return s.handover(text, words), ""
		},
	})
	d.title = "Shell"
	d.why = why
	d.onCancel = s.release
	d.setFocus(1)
	return d
}

// handover returns the command that hands the terminal over to a
// function that says where the shell is and runs words in the container
// chosen, until it ends; and what comes of it when the terminal is the
// UI's again. text is the Command field's, for the dialog when the
// command did not start.
func (s *shell) handover(text string, words []string) tea.Cmd {
	sess, pod, container, at := s.e.sess, s.pod, s.names[s.at], s.at
	var (
		mu     sync.Mutex
		ran    bool
		result kube.ExecResult
	)
	run := func(ctx context.Context, t Terminal) error {
		mu.Lock()
		ran = true
		mu.Unlock()
		// The terminal is raw: the line ends as the terminal needs it.
		fmt.Fprintf(t.Out, "kosh: a shell in pod %s/%s, container %s. It ends with exit or ctrl-d.\r\n",
			cleanLine(pod.Namespace), cleanLine(pod.Name), cleanLine(container))
		// No timeout: a shell lasts as long as the user wants, and the
		// context ends with the session.
		r, err := sess.Exec(ctx, pod, kube.ExecOptions{
			Container: container,
			Command:   words,
			Stdin:     t.In,
			Stdout:    t.Out,
			Size:      t.Size,
			Resized:   t.Resized,
		})
		mu.Lock()
		result = r
		mu.Unlock()
		return err
	}
	// done is called when the terminal is the UI's again, before any
	// message sent meanwhile is taken: the keys of actions are given back
	// here, and no key can reach a table before they are.
	done := func(err error) tea.Msg {
		mu.Lock()
		r, started := result, ran
		mu.Unlock()
		if started && errors.Is(err, kube.ErrExecNotStarted) {
			// The dialog again, which holds the keys from here on.
			s.at = at
			return tea.BatchMsg{openDialog(s.dialog(text, errorText(err.Error()))), refresh()}
		}
		s.release()
		switch {
		case err != nil:
			return tea.BatchMsg{flashErr(err), refresh()}
		case r.Status != 0:
			return tea.BatchMsg{flash(fmt.Sprintf("the shell ended with status %d", r.Status)), refresh()}
		}
		return refreshMsg{}
	}
	return s.e.handover(run, done)
}
