package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// YAML reads the object with the dynamic client now and returns it as
// YAML, with its resourceVersion, which Update needs, and without
// managedFields unless they are asked for. A Secret is refused to a
// read-only grant before anything is read.
func (s *session) YAML(ctx context.Context, ref ObjectRef, managedFields bool) (string, error) {
	if err := checkRef(ref); err != nil {
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

// errNotAResource is what every reading method returns, whatever the
// grant, for a reference whose resource could not be a path segment of
// the API: the dynamic client joins it into the request's path, where
// "configmaps/../secrets" would be the secrets.
var errNotAResource = errors.New("not the name of a resource")

// checkRef returns errNotAResource for a reference whose resource is
// empty, or whose resource, group or version holds "/" or "..". Every
// reading method calls it before it reads anything.
func checkRef(ref ObjectRef) error {
	gvr := ref.Resource.GVR
	if gvr.Resource == "" {
		return fmt.Errorf("an empty resource: %w", errNotAResource)
	}
	for _, part := range []string{gvr.Group, gvr.Version, gvr.Resource} {
		if strings.Contains(part, "/") || strings.Contains(part, "..") {
			return fmt.Errorf("%q: %w", part, errNotAResource)
		}
	}
	return nil
}
