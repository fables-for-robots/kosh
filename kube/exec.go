package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// Exec begins as every action does; checks the pod's reference, the
// container's name and the command; and runs the command on the API's
// exec stream with a terminal, over WebSocket alone: kubectl's fallback
// to SPDY would turn the API's typed refusals into plain text.
func (s *session) Exec(ctx context.Context, pod ObjectRef, opts ExecOptions) (res ExecResult, err error) {
	ctx, release, err := s.begin(ctx, actExec, pod)
	if err != nil {
		return ExecResult{}, err
	}
	defer release()
	if err := s.checkExec(pod, opts); err != nil {
		s.logAction(ctx, actExec, pod, err, slog.String("phase", "end"))
		return ExecResult{}, err
	}
	err = s.stream(ctx, pod, opts)
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Started: true}, nil
}

// checkExec refuses what Exec cannot ask the API for: a reference that
// is not a pod's, or not one the Cluster knows; a container's name that
// is not a DNS-1123 label; and a command of no words. The words are
// not looked at otherwise: they are the container's runtime's.
func (s *session) checkExec(pod ObjectRef, opts ExecOptions) error {
	if err := s.c.checkRef(pod); err != nil {
		return err
	}
	if err := kindIs("exec in", pod, podKinds...); err != nil {
		return err
	}
	if c := opts.Container; c != "" {
		if len(c) > validation.DNS1123LabelMaxLength {
			return fmt.Errorf("a name of %d bytes is no container's: at most %d", len(c), validation.DNS1123LabelMaxLength)
		}
		if len(validation.IsDNS1123Label(c)) > 0 {
			return fmt.Errorf("%q is not the name of a container", c)
		}
	}
	if len(opts.Command) == 0 {
		return errors.New("there is no command to run")
	}
	if s.c.cfg == nil {
		return errors.New("the cluster has no API to run a command through")
	}
	return nil
}

// stream asks the API for the pod's exec subresource, as kubectl exec
// does, and runs the stream until it ends.
func (s *session) stream(ctx context.Context, pod ObjectRef, opts ExecOptions) error {
	req := s.c.typed.CoreV1().RESTClient().Post().
		Namespace(pod.Namespace).Resource("pods").Name(pod.Name).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: opts.Container,
			Command:   opts.Command,
			Stdin:     true,
			Stdout:    true,
			TTY:       true,
		}, scheme.ParameterCodec)
	ex, err := remotecommand.NewWebSocketExecutor(s.c.cfg, "GET", req.URL().String())
	if err != nil {
		return err
	}
	return ex.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  opts.Stdin,
		Stdout: opts.Stdout,
		Tty:    true,
	})
}
