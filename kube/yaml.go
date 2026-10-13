package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// YAML reads the object with the dynamic client now and returns it as
// YAML, with its resourceVersion, which Update needs, and without
// managedFields unless they are asked for. A Secret is refused to a
// read-only grant before anything is read.
func (s *session) YAML(ctx context.Context, ref ObjectRef, managedFields bool) (string, error) {
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
