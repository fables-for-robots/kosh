package kube

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// filter says whether an object of a kind's cache is a row of a table.
type filter func(runtime.Object) bool

func everything(runtime.Object) bool { return true }

// filterOf returns the filter of a query of a typed resource. Without
// Of it is the namespace. With Of, the owner is read from the API now,
// once; a pair of resource and owner that has no relation is
// ErrNoRelation.
func (c *Cluster) filterOf(ctx context.Context, q Query) (filter, error) {
	if q.Of == nil {
		if q.Namespace == "" || !q.Resource.Namespaced {
			return everything, nil
		}
		return inNamespace(q.Namespace), nil
	}
	of := *q.Of
	switch {
	case q.Resource.Is("", "pods") && of.Resource.Is("", "nodes"):
		node, err := c.typed.CoreV1().Nodes().Get(ctx, of.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		name := node.Name
		return func(o runtime.Object) bool {
			p, ok := o.(*corev1.Pod)
			return ok && p.Spec.NodeName == name
		}, nil
	case q.Resource.Is("", "pods"):
		namespace, sel, err := c.podSelector(ctx, of)
		if err != nil {
			return nil, err
		}
		return func(o runtime.Object) bool {
			m, err := meta.Accessor(o)
			return err == nil && m.GetNamespace() == namespace && sel.Matches(labels.Set(m.GetLabels()))
		}, nil
	case q.Resource.Is("batch", "jobs") && of.Resource.Is("batch", "cronjobs"):
		cj, err := c.typed.BatchV1().CronJobs(of.Namespace).Get(ctx, of.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return ownedBy(cj.Namespace, cj.UID), nil
	}
	return nil, ErrNoRelation
}

func inNamespace(namespace string) filter {
	return func(o runtime.Object) bool {
		m, err := meta.Accessor(o)
		return err == nil && m.GetNamespace() == namespace
	}
}

func ownedBy(namespace string, uid types.UID) filter {
	return func(o runtime.Object) bool {
		m, err := meta.Accessor(o)
		if err != nil || m.GetNamespace() != namespace {
			return false
		}
		for _, ref := range m.GetOwnerReferences() {
			if ref.UID == uid {
				return true
			}
		}
		return false
	}
}

// podSelector returns the namespace and the selector of the pods that
// belong to a deployment, replicaset, statefulset, daemonset, job or
// service, read from the API now. ErrNoRelation for anything else. A
// service without a selector gives labels.Nothing().
func (c *Cluster) podSelector(ctx context.Context, ref ObjectRef) (string, labels.Selector, error) {
	get := metav1.GetOptions{}
	var (
		namespace string
		sel       *metav1.LabelSelector
	)
	switch {
	case ref.Resource.Is("apps", "deployments"):
		o, err := c.typed.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return "", nil, err
		}
		namespace, sel = o.Namespace, o.Spec.Selector
	case ref.Resource.Is("apps", "replicasets"):
		o, err := c.typed.AppsV1().ReplicaSets(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return "", nil, err
		}
		namespace, sel = o.Namespace, o.Spec.Selector
	case ref.Resource.Is("apps", "statefulsets"):
		o, err := c.typed.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return "", nil, err
		}
		namespace, sel = o.Namespace, o.Spec.Selector
	case ref.Resource.Is("apps", "daemonsets"):
		o, err := c.typed.AppsV1().DaemonSets(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return "", nil, err
		}
		namespace, sel = o.Namespace, o.Spec.Selector
	case ref.Resource.Is("batch", "jobs"):
		o, err := c.typed.BatchV1().Jobs(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return "", nil, err
		}
		namespace, sel = o.Namespace, o.Spec.Selector
	case ref.Resource.Is("", "services"):
		o, err := c.typed.CoreV1().Services(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return "", nil, err
		}
		if len(o.Spec.Selector) == 0 {
			return o.Namespace, labels.Nothing(), nil
		}
		return o.Namespace, labels.SelectorFromSet(o.Spec.Selector), nil
	default:
		return "", nil, ErrNoRelation
	}
	if sel == nil {
		return namespace, labels.Nothing(), nil
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return "", nil, err
	}
	return namespace, s, nil
}
