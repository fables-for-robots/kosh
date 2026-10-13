package kube

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"sigs.k8s.io/yaml"
)

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

// imagePairs returns images as "name=image", sorted, for the log.
func imagePairs(images map[string]string) []string {
	out := make([]string, 0, len(images))
	for name, image := range images {
		out = append(out, name+"="+image)
	}
	sort.Strings(out)
	return out
}

// propagation returns the propagation of opts: background when it is
// not set.
func (opts DeleteOptions) propagation() Propagation {
	if opts.Propagation == "" {
		return PropagationBackground
	}
	return opts.Propagation
}

func (s *session) Delete(ctx context.Context, ref ObjectRef, opts DeleteOptions) (err error) {
	if err := s.begin(ctx, actDelete, ref); err != nil {
		return err
	}
	defer func() {
		logged := err
		if ref.Resource.Is("", "secrets") {
			// A webhook's denial may quote the Secret it was given.
			logged = loggableError(actDelete, err)
		}
		s.logAction(ctx, actDelete, ref, logged,
			slog.String("propagation", string(opts.propagation())), slog.Bool("force", opts.Force))
	}()
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

func (s *session) Kill(ctx context.Context, pod ObjectRef) (err error) {
	if err := s.begin(ctx, actKill, pod); err != nil {
		return err
	}
	defer func() { s.logAction(ctx, actKill, pod, err) }()
	if err := kindIs("kill", pod, podKinds...); err != nil {
		return err
	}
	return s.delete(ctx, pod, DeleteOptions{Propagation: PropagationBackground, Force: true})
}

func (s *session) Update(ctx context.Context, ref ObjectRef, text string) (err error) {
	if err := s.begin(ctx, actUpdate, ref); err != nil {
		return err
	}
	// The log gets what kind of error it was: its text may quote the edit.
	defer func() { s.logAction(ctx, actUpdate, ref, loggableError(actUpdate, err)) }()
	obj, err := parseObject(ref, text)
	if err != nil {
		return err
	}
	_, err = s.objectClient(ref).Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

// editRefusal is Update's refusal of a text before anything is sent.
// reason says nothing of the text and is what the log holds; detail is
// for the user who wrote it.
type editRefusal struct {
	reason, detail string
}

func (e *editRefusal) Error() string {
	if e.detail == "" {
		return e.reason
	}
	return e.reason + ": " + e.detail
}

// parseObject returns the object that text, YAML, says, when it is the
// object ref as it was read.
func parseObject(ref ObjectRef, text string) (*unstructured.Unstructured, error) {
	doc, err := oneDocument(text)
	if err != nil {
		return nil, err
	}
	js, err := yaml.YAMLToJSON(doc)
	if err != nil {
		return nil, &editRefusal{"the YAML does not parse", err.Error()}
	}
	var v any
	if err := utiljson.Unmarshal(js, &v); err != nil {
		return nil, &editRefusal{"the YAML does not parse", err.Error()}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &editRefusal{"the YAML is not an object", ""}
	}
	obj := &unstructured.Unstructured{Object: m}
	wantVersion := ref.Resource.GVR.GroupVersion().String()
	if obj.GetAPIVersion() != wantVersion || obj.GetKind() != ref.Resource.Kind ||
		obj.GetName() != ref.Name || obj.GetNamespace() != ref.Namespace {
		return nil, &editRefusal{"the YAML names another object",
			"it must be " + describeObject(wantVersion, ref.Resource.Kind, ref.Namespace, ref.Name)}
	}
	if obj.GetResourceVersion() == "" {
		return nil, &editRefusal{"the YAML has no resourceVersion", "it must be the object as it was read"}
	}
	return obj, nil
}

// oneDocument returns the one YAML document of text. Documents that hold
// nothing but comments and directives do not count; anything but those
// after a document's end marker "..." is another document.
func oneDocument(text string) ([]byte, error) {
	more := &editRefusal{"the YAML holds more than one document", ""}
	r := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(text)))
	var docs [][]byte
	for {
		doc, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, &editRefusal{"the YAML does not parse", err.Error()}
		}
		content, after := documentContent(doc)
		if after {
			return nil, more
		}
		if content {
			docs = append(docs, doc)
		}
	}
	switch len(docs) {
	case 0:
		return nil, &editRefusal{"the YAML is not an object", ""}
	case 1:
		return docs[0], nil
	}
	return nil, more
}

// documentContent reports whether a part of a YAML stream between two
// "---" holds more than blank lines, comments, directives and markers,
// and whether anything of that kind follows a document end marker "...".
// Both markers and directives are only those at the start of a line,
// where nothing else can be.
func documentContent(doc []byte) (content, afterEnd bool) {
	ended := false
	for _, line := range strings.Split(string(doc), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case isMarker(line, "..."):
			ended = true
			continue
		case isMarker(line, "---"), strings.HasPrefix(line, "%"):
			continue
		}
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if ended {
			return content, true
		}
		content = true
	}
	return content, false
}

// isMarker reports whether line is the marker, alone or before white
// space.
func isMarker(line, marker string) bool {
	rest, ok := strings.CutPrefix(line, marker)
	return ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}

// scaleClient is the scale subresource of one kind, in one namespace.
type scaleClient interface {
	GetScale(ctx context.Context, name string, opts metav1.GetOptions) (*autoscalingv1.Scale, error)
	UpdateScale(ctx context.Context, name string, scale *autoscalingv1.Scale, opts metav1.UpdateOptions) (*autoscalingv1.Scale, error)
}

func (s *session) Scale(ctx context.Context, ref ObjectRef, replicas int32) (err error) {
	if err := s.begin(ctx, actScale, ref); err != nil {
		return err
	}
	defer func() { s.logAction(ctx, actScale, ref, err, slog.Int("replicas", int(replicas))) }()
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

func (s *session) Restart(ctx context.Context, ref ObjectRef) (err error) {
	if err := s.begin(ctx, actRestart, ref); err != nil {
		return err
	}
	defer func() { s.logAction(ctx, actRestart, ref, err) }()
	if err := kindIs("restart", ref, workloadKinds...); err != nil {
		return err
	}
	return s.patchTemplate(ctx, ref, "", map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{restartedAtAnnotation: time.Now().Format(time.RFC3339)},
		},
	})
}

// patchTemplate sends a strategic merge patch of the pod template of
// ref: spec.template is template. A resourceVersion that is not empty
// goes into the patch too, so that the API answers a conflict when the
// object has changed since it was read.
func (s *session) patchTemplate(ctx context.Context, ref ObjectRef, resourceVersion string, template map[string]any) error {
	patch := map[string]any{"spec": map[string]any{"template": template}}
	if resourceVersion != "" {
		patch["metadata"] = map[string]any{"resourceVersion": resourceVersion}
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = s.objectClient(ref).Patch(ctx, ref.Name, types.StrategicMergePatchType, body, metav1.PatchOptions{})
	return err
}

func (s *session) SetImages(ctx context.Context, ref ObjectRef, images map[string]string) (err error) {
	if err := s.begin(ctx, actSetImages, ref); err != nil {
		return err
	}
	defer func() { s.logAction(ctx, actSetImages, ref, err, slog.Any("images", imagePairs(images))) }()
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
	// Without the object's resourceVersion, a container removed since it
	// was read would be added back with nothing but a name and an image.
	return s.patchTemplate(ctx, ref, obj.GetResourceVersion(), map[string]any{"spec": spec})
}

func (s *session) Rollback(ctx context.Context, replicaSet ObjectRef) (err error) {
	if err := s.begin(ctx, actRollback, replicaSet); err != nil {
		return err
	}
	defer func() { s.logAction(ctx, actRollback, replicaSet, err) }()
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
	// The deployment of that name must be the one the reference is to,
	// not another made since under the same name.
	deployment, err := s.c.typed.AppsV1().Deployments(rs.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || err == nil && deployment.UID != owner.UID {
		return fmt.Errorf("replicaset %s belongs to a deployment that no longer exists", name)
	}
	if err != nil {
		return err
	}
	rb, err := polymorphichelpers.RollbackerFor(schema.GroupKind{Group: "apps", Kind: "Deployment"}, s.c.typed)
	if err != nil {
		return err
	}
	result, err := rb.Rollback(deployment, nil, revision, cmdutil.DryRunNone)
	if err != nil {
		return err
	}
	if strings.HasPrefix(result, rollbackSkipped) {
		return fmt.Errorf("deployment %s/%s: %s", deployment.Namespace, deployment.Name, result)
	}
	return nil
}

// rollbackSkipped begins what kubectl's rollbacker says when the
// deployment is at that revision already, and nothing was done.
const rollbackSkipped = "skipped rollback"

// revisionAnnotation is the revision of a deployment that a replicaset
// is.
const revisionAnnotation = "deployment.kubernetes.io/revision"

func (s *session) Trigger(ctx context.Context, cronJob ObjectRef) (job string, err error) {
	if err := s.begin(ctx, actTrigger, cronJob); err != nil {
		return "", err
	}
	defer func() { s.logAction(ctx, actTrigger, cronJob, err, slog.String("job", job)) }()
	if err := kindIs("trigger", cronJob, cronJobKinds...); err != nil {
		return "", err
	}
	cj, err := s.c.typed.BatchV1().CronJobs(cronJob.Namespace).Get(ctx, cronJob.Name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	created, err := s.c.typed.BatchV1().Jobs(cj.Namespace).Create(ctx, jobFromCronJob(cj, manualJobName(cj.Name)), metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return created.Name, nil
}

// manualJobName returns the name of a job that is triggered by hand from
// the cronjob: "<cronjob>-manual-xxxxx", of at most 63 characters.
func manualJobName(cronJob string) string {
	const suffix, random = "-manual-", 5
	if max := 63 - len(suffix) - random; len(cronJob) > max {
		cronJob = cronJob[:max]
	}
	return cronJob + suffix + utilrand.String(random)
}

// jobFromCronJob returns the job named name that kubectl create job
// --from=cronjob/… makes of cj.
func jobFromCronJob(cj *batchv1.CronJob, name string) *batchv1.Job {
	annotations := map[string]string{"cronjob.kubernetes.io/instantiate": "manual"}
	for k, v := range cj.Spec.JobTemplate.Annotations {
		annotations[k] = v
	}
	controller := true
	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   cj.Namespace,
			Name:        name,
			Labels:      cj.Spec.JobTemplate.Labels,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "CronJob",
				Name:       cj.Name,
				UID:        cj.UID,
				Controller: &controller,
			}},
		},
		Spec: cj.Spec.JobTemplate.Spec,
	}
}

func (s *session) SetSuspended(ctx context.Context, cronJob ObjectRef, suspended bool) (err error) {
	action := actResume
	if suspended {
		action = actSuspend
	}
	if err := s.begin(ctx, action, cronJob); err != nil {
		return err
	}
	defer func() { s.logAction(ctx, action, cronJob, err) }()
	if err := kindIs(action, cronJob, cronJobKinds...); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"suspend": suspended}})
	if err != nil {
		return err
	}
	_, err = s.c.typed.BatchV1().CronJobs(cronJob.Namespace).Patch(ctx, cronJob.Name, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}

// describeObject names an object as "apps/v1 Deployment default/web".
func describeObject(apiVersion, kind, namespace, name string) string {
	if namespace != "" {
		name = namespace + "/" + name
	}
	return apiVersion + " " + kind + " " + name
}
