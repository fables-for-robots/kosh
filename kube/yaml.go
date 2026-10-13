package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// YAML reads the object with the dynamic client now and returns it as
// YAML, with its resourceVersion, which Update needs, and without
// managedFields unless they are asked for. A Secret is refused to a
// read-only grant before anything is read.
func (s *session) YAML(ctx context.Context, ref ObjectRef, managedFields bool) (string, error) {
	if err := s.c.checkRef(ref); err != nil {
		return "", err
	}
	if err := s.maySee(ref); err != nil {
		return "", err
	}
	o, err := s.c.dyn.Resource(ref.Resource.GVR).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if !managedFields {
		unstructured.RemoveNestedField(o.Object, "metadata", "managedFields")
	}
	b, err := yaml.Marshal(o.Object)
	if err != nil {
		return "", fmt.Errorf("%s cannot be written as YAML: %w", ref, err)
	}
	return string(b), nil
}

// checkRef refuses, whatever the grant, a reference that is not to an
// object of a resource the Cluster knows, before anything is read: its
// resource, group and version must be what the API's names can be, and
// its GVR must be one discovery found, with that resource's kind and
// scope. The error wraps ErrUnknownResource.
//
// The clients join the GVR into the request's path and clean it, so a
// resource string of "." with the name "secrets" would read the list of
// a namespace's Secrets; and kubectl's describers are chosen by the kind.
// Only what discovery made can be read. Containers and Rules, which no
// object belongs to, are not among them.
func (c *Cluster) checkRef(ref ObjectRef) error {
	r := ref.Resource
	gvr := r.GVR
	switch {
	case len(validation.IsDNS1123Label(gvr.Resource)) > 0:
		return fmt.Errorf("%q is not the name of a resource: %w", gvr.Resource, ErrUnknownResource)
	case gvr.Group != "" && len(validation.IsDNS1123Subdomain(gvr.Group)) > 0:
		return fmt.Errorf("%q is not the name of a group: %w", gvr.Group, ErrUnknownResource)
	case len(validation.IsDNS1035Label(gvr.Version)) > 0:
		return fmt.Errorf("%q is not the name of a version: %w", gvr.Version, ErrUnknownResource)
	}
	known, ok := c.knownResource(gvr)
	if !ok {
		return fmt.Errorf("%s: %w", gvr, ErrUnknownResource)
	}
	if r.Kind != known.Kind || r.Namespaced != known.Namespaced {
		return fmt.Errorf("%s is of the kind %s and namespaced %v, not %s and %v: %w",
			gvr, known.Kind, known.Namespaced, r.Kind, r.Namespaced, ErrUnknownResource)
	}
	return nil
}

// knownResource returns the resource of gvr as discovery last found it.
func (c *Cluster) knownResource(gvr schema.GroupVersionResource) (Resource, bool) {
	c.resMu.RLock()
	defer c.resMu.RUnlock()
	for _, r := range c.resources {
		if r.GVR == gvr {
			return r, true
		}
	}
	return Resource{}, false
}
