package kube

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

var errNotYet = errors.New("not implemented")

// objectClient returns the dynamic client of ref's resource, in
// ref.Namespace when the resource is namespaced.
func (s *session) objectClient(ref ObjectRef) dynamic.ResourceInterface {
	r := s.c.dyn.Resource(ref.Resource.GVR)
	if ref.Resource.Namespaced {
		return r.Namespace(ref.Namespace)
	}
	return r
}

// kindIs returns an error that says the action is not for ref, unless
// ref is of one of the resources, given as group and resource pairs.
func kindIs(verb string, ref ObjectRef, groupResources ...string) error {
	for i := 0; i+1 < len(groupResources); i += 2 {
		if ref.Resource.Is(groupResources[i], groupResources[i+1]) {
			return nil
		}
	}
	return fmt.Errorf("cannot %s %s", verb, ref)
}

// The kinds the actions are for.
var (
	podKinds      = []string{"", "pods"}
	scaleKinds    = []string{"apps", "deployments", "apps", "statefulsets", "apps", "replicasets"}
	workloadKinds = []string{"apps", "deployments", "apps", "statefulsets", "apps", "daemonsets"}
	rsKinds       = []string{"apps", "replicasets"}
	cronJobKinds  = []string{"batch", "cronjobs"}
	nodeKinds     = []string{"", "nodes"}
)

// propagation returns the propagation of opts: background when it is
// not set.
func (opts DeleteOptions) propagation() Propagation {
	if opts.Propagation == "" {
		return PropagationBackground
	}
	return opts.Propagation
}

func (s *session) Delete(ctx context.Context, ref ObjectRef, opts DeleteOptions) error {
	if err := s.begin(ctx, actDelete, ref); err != nil {
		return err
	}
	return s.delete(ctx, ref, opts)
}

// delete deletes the object through the dynamic client.
func (s *session) delete(ctx context.Context, ref ObjectRef, opts DeleteOptions) error {
	policy := metav1.DeletionPropagation(opts.propagation())
	del := metav1.DeleteOptions{PropagationPolicy: &policy}
	if opts.Force {
		zero := int64(0)
		del.GracePeriodSeconds = &zero
	}
	return s.objectClient(ref).Delete(ctx, ref.Name, del)
}

func (s *session) Kill(ctx context.Context, pod ObjectRef) error {
	if err := s.begin(ctx, actKill, pod); err != nil {
		return err
	}
	if err := kindIs("kill", pod, podKinds...); err != nil {
		return err
	}
	return s.delete(ctx, pod, DeleteOptions{Propagation: PropagationBackground, Force: true})
}

func (s *session) Update(ctx context.Context, ref ObjectRef, text string) error {
	if err := s.begin(ctx, actUpdate, ref); err != nil {
		return err
	}
	obj, err := parseObject(ref, text)
	if err != nil {
		return err
	}
	_, err = s.objectClient(ref).Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

// parseObject returns the object that text, YAML, says, when it is the
// object ref as it was read.
func parseObject(ref ObjectRef, text string) (*unstructured.Unstructured, error) {
	js, err := yaml.YAMLToJSON([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("the YAML does not parse: %w", err)
	}
	var v any
	if err := utiljson.Unmarshal(js, &v); err != nil {
		return nil, fmt.Errorf("the YAML does not parse: %w", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("the YAML is not an object")
	}
	obj := &unstructured.Unstructured{Object: m}
	wantVersion := ref.Resource.GVR.GroupVersion().String()
	if obj.GetAPIVersion() != wantVersion || obj.GetKind() != ref.Resource.Kind ||
		obj.GetName() != ref.Name || obj.GetNamespace() != ref.Namespace {
		return nil, fmt.Errorf("the YAML names another object: %s, not %s",
			describeObject(obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName()),
			describeObject(wantVersion, ref.Resource.Kind, ref.Namespace, ref.Name))
	}
	if obj.GetResourceVersion() == "" {
		return nil, errors.New("the YAML has no resourceVersion: it must be the object as it was read")
	}
	return obj, nil
}

func (s *session) Scale(ctx context.Context, ref ObjectRef, replicas int32) error {
	if err := s.begin(ctx, actScale, ref); err != nil {
		return err
	}
	return errNotYet
}

func (s *session) Restart(ctx context.Context, ref ObjectRef) error {
	if err := s.begin(ctx, actRestart, ref); err != nil {
		return err
	}
	return errNotYet
}

func (s *session) SetImages(ctx context.Context, ref ObjectRef, images map[string]string) error {
	if err := s.begin(ctx, actSetImages, ref); err != nil {
		return err
	}
	return errNotYet
}

func (s *session) Rollback(ctx context.Context, replicaSet ObjectRef) error {
	if err := s.begin(ctx, actRollback, replicaSet); err != nil {
		return err
	}
	return errNotYet
}

func (s *session) Trigger(ctx context.Context, cronJob ObjectRef) (string, error) {
	if err := s.begin(ctx, actTrigger, cronJob); err != nil {
		return "", err
	}
	return "", errNotYet
}

func (s *session) SetSuspended(ctx context.Context, cronJob ObjectRef, suspended bool) error {
	action := actResume
	if suspended {
		action = actSuspend
	}
	if err := s.begin(ctx, action, cronJob); err != nil {
		return err
	}
	return errNotYet
}

// describeObject names an object as "apps/v1 Deployment default/web".
func describeObject(apiVersion, kind, namespace, name string) string {
	if namespace != "" {
		name = namespace + "/" + name
	}
	return apiVersion + " " + kind + " " + name
}
