package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/dynamic"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
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

// scaleClient is the scale subresource of one kind, in one namespace.
type scaleClient interface {
	GetScale(ctx context.Context, name string, opts metav1.GetOptions) (*autoscalingv1.Scale, error)
	UpdateScale(ctx context.Context, name string, scale *autoscalingv1.Scale, opts metav1.UpdateOptions) (*autoscalingv1.Scale, error)
}

func (s *session) Scale(ctx context.Context, ref ObjectRef, replicas int32) error {
	if err := s.begin(ctx, actScale, ref); err != nil {
		return err
	}
	if err := kindIs("scale", ref, scaleKinds...); err != nil {
		return err
	}
	if replicas < 0 {
		return fmt.Errorf("cannot scale %s to %d replicas", ref, replicas)
	}
	var sc scaleClient
	apps := s.c.typed.AppsV1()
	switch {
	case ref.Resource.Is("apps", "deployments"):
		sc = apps.Deployments(ref.Namespace)
	case ref.Resource.Is("apps", "statefulsets"):
		sc = apps.StatefulSets(ref.Namespace)
	default:
		sc = apps.ReplicaSets(ref.Namespace)
	}
	scale, err := sc.GetScale(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	scale.Spec.Replicas = replicas
	_, err = sc.UpdateScale(ctx, ref.Name, scale, metav1.UpdateOptions{})
	return err
}

// restartedAtAnnotation is what kubectl rollout restart sets on the pod
// template.
const restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

func (s *session) Restart(ctx context.Context, ref ObjectRef) error {
	if err := s.begin(ctx, actRestart, ref); err != nil {
		return err
	}
	if err := kindIs("restart", ref, workloadKinds...); err != nil {
		return err
	}
	return s.patchTemplate(ctx, ref, map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{restartedAtAnnotation: time.Now().Format(time.RFC3339)},
		},
	})
}

// patchTemplate sends a strategic merge patch of the pod template of
// ref: spec.template is template.
func (s *session) patchTemplate(ctx context.Context, ref ObjectRef, template map[string]any) error {
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"template": template}})
	if err != nil {
		return err
	}
	_, err = s.objectClient(ref).Patch(ctx, ref.Name, types.StrategicMergePatchType, body, metav1.PatchOptions{})
	return err
}

func (s *session) SetImages(ctx context.Context, ref ObjectRef, images map[string]string) error {
	if err := s.begin(ctx, actSetImages, ref); err != nil {
		return err
	}
	if err := kindIs("set the images of", ref, workloadKinds...); err != nil {
		return err
	}
	if len(images) == 0 {
		return nil
	}
	obj, err := s.objectClient(ref).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	init := map[string]bool{} // of every container of the template
	for _, field := range []string{"initContainers", "containers"} {
		cc, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", field)
		for _, c := range cc {
			if m, ok := c.(map[string]any); ok {
				if name, ok := m["name"].(string); ok {
					init[name] = field == "initContainers"
				}
			}
		}
	}
	names := make([]string, 0, len(images))
	for name := range images {
		names = append(names, name)
	}
	sort.Strings(names)
	var containers, inits []any
	for _, name := range names {
		isInit, ok := init[name]
		if !ok {
			return fmt.Errorf("%s has no container %q", ref, name)
		}
		c := map[string]any{"name": name, "image": images[name]}
		if isInit {
			inits = append(inits, c)
		} else {
			containers = append(containers, c)
		}
	}
	spec := map[string]any{}
	if containers != nil {
		spec["containers"] = containers
	}
	if inits != nil {
		spec["initContainers"] = inits
	}
	return s.patchTemplate(ctx, ref, map[string]any{"spec": spec})
}

func (s *session) Rollback(ctx context.Context, replicaSet ObjectRef) error {
	if err := s.begin(ctx, actRollback, replicaSet); err != nil {
		return err
	}
	if err := kindIs("roll back to", replicaSet, rsKinds...); err != nil {
		return err
	}
	rs, err := s.c.typed.AppsV1().ReplicaSets(replicaSet.Namespace).Get(ctx, replicaSet.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	name := rs.Namespace + "/" + rs.Name
	owner := metav1.GetControllerOf(rs)
	if owner == nil || owner.Kind != "Deployment" || !strings.HasPrefix(owner.APIVersion, "apps/") {
		return fmt.Errorf("replicaset %s belongs to no deployment", name)
	}
	text, ok := rs.Annotations[revisionAnnotation]
	if !ok {
		return fmt.Errorf("replicaset %s has no revision", name)
	}
	revision, err := strconv.ParseInt(text, 10, 64)
	if err != nil || revision < 1 {
		return fmt.Errorf("replicaset %s has the revision %q, which is not one", name, text)
	}
	rb, err := polymorphichelpers.RollbackerFor(schema.GroupKind{Group: "apps", Kind: "Deployment"}, s.c.typed)
	if err != nil {
		return err
	}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: rs.Namespace, Name: owner.Name}}
	_, err = rb.Rollback(deployment, nil, revision, cmdutil.DryRunNone)
	return err
}

// revisionAnnotation is the revision of a deployment that a replicaset
// is.
const revisionAnnotation = "deployment.kubernetes.io/revision"

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
