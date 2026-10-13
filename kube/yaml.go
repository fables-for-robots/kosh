package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apipath "k8s.io/apimachinery/pkg/api/validation/path"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// YAML reads the object with the dynamic client now and returns it as
// YAML, with its resourceVersion, which Update needs, and without
// managedFields unless they are asked for. A Secret is refused to a
// read-only grant before anything is read, and the refusal is logged.
func (s *session) YAML(ctx context.Context, ref ObjectRef, managedFields bool) (string, error) {
	if err := s.c.checkRef(ref); err != nil {
		return "", err
	}
	if err := s.mayRead(ctx, readYAML, ref); err != nil {
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
// resource must pass checkResource, its namespace checkNamespace (and be
// empty exactly when the resource is not namespaced), and its name
// checkName.
//
// The clients join the GVR into the request's path and clean it, so a
// resource string of "." with the name "secrets" would read the list of
// a namespace's Secrets; and kubectl's describers are chosen by the kind.
// Only what discovery made can be read. Containers and Rules, which no
// object belongs to, are not among them.
func (c *Cluster) checkRef(ref ObjectRef) error {
	if err := c.checkResource(ref.Resource); err != nil {
		return err
	}
	if ref.Resource.Namespaced && ref.Namespace == "" {
		return fmt.Errorf("%s is namespaced, and the reference names no namespace: %w", ref.Resource.GVR.Resource, ErrInvalidNamespace)
	}
	if err := checkNamespace(ref.Resource, ref.Namespace); err != nil {
		return err
	}
	return checkName(ref.Name)
}

// checkResource refuses a resource that is not one the Cluster knows:
// its resource, group and version must be what the API's names can be,
// and its GVR must be one discovery found, with that resource's kind and
// scope. The error wraps ErrUnknownResource.
func (c *Cluster) checkResource(r Resource) error {
	if err := checkGVR(r.GVR); err != nil {
		return err
	}
	known, ok := c.knownResource(r.GVR)
	if !ok {
		return fmt.Errorf("%s: %w", r.GVR, ErrUnknownResource)
	}
	if r.Kind != known.Kind || r.Namespaced != known.Namespaced {
		return fmt.Errorf("%s is of the kind %s and namespaced %v, not %s and %v: %w",
			r.GVR, known.Kind, known.Namespaced, r.Kind, r.Namespaced, ErrUnknownResource)
	}
	return nil
}

// checkGVR refuses a GVR whose strings are not what the API's names can
// be: a resource that is a DNS-1123 label, a group that is empty or a
// DNS-1123 subdomain, a version that is a DNS-1035 label. None of them
// can then be "." or "..", or hold a slash or a percent sign. The error
// wraps ErrUnknownResource.
func checkGVR(gvr schema.GroupVersionResource) error {
	switch {
	case len(validation.IsDNS1123Label(gvr.Resource)) > 0:
		return fmt.Errorf("%q is not the name of a resource: %w", gvr.Resource, ErrUnknownResource)
	case gvr.Group != "" && len(validation.IsDNS1123Subdomain(gvr.Group)) > 0:
		return fmt.Errorf("%q is not the name of a group: %w", gvr.Group, ErrUnknownResource)
	case len(validation.IsDNS1035Label(gvr.Version)) > 0:
		return fmt.Errorf("%q is not the name of a version: %w", gvr.Version, ErrUnknownResource)
	}
	return nil
}

// checkNamespace refuses a namespace that is neither empty nor a
// DNS-1123 label, which is what a namespace's name is, and one that is
// given for a resource that is not namespaced. The error wraps
// ErrInvalidNamespace.
func checkNamespace(r Resource, namespace string) error {
	if namespace == "" {
		return nil
	}
	if len(validation.IsDNS1123Label(namespace)) > 0 {
		return fmt.Errorf("%q is not the name of a namespace: %w", namespace, ErrInvalidNamespace)
	}
	if !r.Namespaced {
		return fmt.Errorf("%s is not namespaced, and a namespace was given: %w", r.GVR.Resource, ErrInvalidNamespace)
	}
	return nil
}

// maxNameLength is the longest name an object can have: a DNS-1123
// subdomain's.
const maxNameLength = validation.DNS1123SubdomainMaxLength

// checkName refuses a name that is no object's: an empty one, one
// longer than maxNameLength, and one that is not a path segment by the
// API's own rule (".", "..", or one that holds "/" or "%").
func checkName(name string) error {
	if name == "" {
		return errors.New("the reference names no object")
	}
	if len(name) > maxNameLength {
		return fmt.Errorf("a name of %d bytes is no object's: at most %d", len(name), maxNameLength)
	}
	if msgs := apipath.IsValidPathSegmentName(name); len(msgs) > 0 {
		return fmt.Errorf("%q is no object's name: %s", name, strings.Join(msgs, "; "))
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
