package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Owner reads the object's metadata with the dynamic client and returns
// its owner: the owner reference that is the controller, or else the
// first. ErrNoOwner when it has none; an error that names the kind when
// the owner's kind is not a resource of the cluster.
func (s *session) Owner(ctx context.Context, ref ObjectRef) (ObjectRef, error) {
	if err := checkRef(ref); err != nil {
		return ObjectRef{}, err
	}
	o, err := s.c.dyn.Resource(ref.Resource.GVR).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return ObjectRef{}, err
	}
	owners := o.GetOwnerReferences()
	if len(owners) == 0 {
		return ObjectRef{}, ErrNoOwner
	}
	owner := owners[0]
	for _, or := range owners {
		if or.Controller != nil && *or.Controller {
			owner = or
			break
		}
	}
	gv, err := schema.ParseGroupVersion(owner.APIVersion)
	if err != nil {
		return ObjectRef{}, fmt.Errorf("the owner %s %s of %s has an API version that does not parse: %w", owner.Kind, owner.Name, ref, err)
	}
	r, ok := s.c.resourceFor(gv.Group, owner.Kind)
	if !ok {
		return ObjectRef{}, fmt.Errorf("the owner of %s is a %s, which the cluster has no resource for", ref, owner.Kind)
	}
	out := ObjectRef{Resource: r, Name: owner.Name}
	if r.Namespaced {
		out.Namespace = o.GetNamespace()
	}
	return out, nil
}
