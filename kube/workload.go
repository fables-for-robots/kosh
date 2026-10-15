package kube

import (
	"context"
	"fmt"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Images reads a deployment, statefulset or daemonset with the typed
// client and returns the containers of its pod template, init
// containers first. Any other kind is an error.
func (s *session) Images(ctx context.Context, ref ObjectRef) ([]ContainerImage, error) {
	ctx, release := s.bound(ctx)
	defer release()
	if err := s.c.checkRef(ref); err != nil {
		return nil, err
	}
	api, err := s.api()
	if err != nil {
		return nil, err
	}
	get := metav1.GetOptions{}
	var spec *corev1.PodSpec
	switch {
	case ref.Resource.Is("apps", "deployments"):
		o, err := api.typed.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return nil, err
		}
		spec = &o.Spec.Template.Spec
	case ref.Resource.Is("apps", "statefulsets"):
		o, err := api.typed.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return nil, err
		}
		spec = &o.Spec.Template.Spec
	case ref.Resource.Is("apps", "daemonsets"):
		o, err := api.typed.AppsV1().DaemonSets(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return nil, err
		}
		spec = &o.Spec.Template.Spec
	default:
		return nil, fmt.Errorf("%s has no images: only a deployment, statefulset or daemonset has", ref)
	}
	out := make([]ContainerImage, 0, len(spec.InitContainers)+len(spec.Containers))
	for _, c := range spec.InitContainers {
		out = append(out, ContainerImage{Name: c.Name, Image: c.Image, Init: true})
	}
	for _, c := range spec.Containers {
		out = append(out, ContainerImage{Name: c.Name, Image: c.Image})
	}
	return out, nil
}

// Replicas returns spec.replicas of the scale subresource of a
// deployment, statefulset or replicaset. Any other kind is an error.
func (s *session) Replicas(ctx context.Context, ref ObjectRef) (int32, error) {
	ctx, release := s.bound(ctx)
	defer release()
	if err := s.c.checkRef(ref); err != nil {
		return 0, err
	}
	api, err := s.api()
	if err != nil {
		return 0, err
	}
	get := metav1.GetOptions{}
	var scale *autoscalingv1.Scale
	switch {
	case ref.Resource.Is("apps", "deployments"):
		scale, err = api.typed.AppsV1().Deployments(ref.Namespace).GetScale(ctx, ref.Name, get)
	case ref.Resource.Is("apps", "statefulsets"):
		scale, err = api.typed.AppsV1().StatefulSets(ref.Namespace).GetScale(ctx, ref.Name, get)
	case ref.Resource.Is("apps", "replicasets"):
		scale, err = api.typed.AppsV1().ReplicaSets(ref.Namespace).GetScale(ctx, ref.Name, get)
	default:
		return 0, fmt.Errorf("%s has no replicas: only a deployment, statefulset or replicaset has", ref)
	}
	if err != nil {
		return 0, err
	}
	return scale.Spec.Replicas, nil
}
