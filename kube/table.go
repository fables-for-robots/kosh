package kube

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// tableLister reads the API server's table of a resource.
type tableLister interface {
	listTable(ctx context.Context, r Resource, namespace string) (*metav1.Table, error)
}
