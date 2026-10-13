package render

import "k8s.io/apimachinery/pkg/runtime/schema"

// The kinds kosh has a renderer for. Each is registered by the file of
// its renderer. Containers and rules are no resources and are not here:
// see Containers and Rules.
var (
	Pods                      = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	Services                  = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	Nodes                     = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"}
	Namespaces                = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	Events                    = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"}
	ConfigMaps                = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	Secrets                   = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	PersistentVolumeClaims    = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}
	PersistentVolumes         = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumes"}
	ServiceAccounts           = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"}
	Deployments               = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	ReplicaSets               = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	StatefulSets              = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	DaemonSets                = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}
	Jobs                      = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	CronJobs                  = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}
	Ingresses                 = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
	NetworkPolicies           = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}
	StorageClasses            = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	Roles                     = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
	RoleBindings              = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
	ClusterRoles              = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	ClusterRoleBindings       = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	HorizontalPodAutoscalers  = schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}
	PodDisruptionBudgets      = schema.GroupVersionResource{Group: "policy", Version: "v1", Resource: "poddisruptionbudgets"}
	CustomResourceDefinitions = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
)

// All is every kind above: the 26 that the finished program has a
// renderer for. A test of the whole program checks Kinds against it.
var All = []schema.GroupVersionResource{
	Pods, Services, Nodes, Namespaces, Events, ConfigMaps, Secrets,
	PersistentVolumeClaims, PersistentVolumes, ServiceAccounts,
	Deployments, ReplicaSets, StatefulSets, DaemonSets,
	Jobs, CronJobs,
	Ingresses, NetworkPolicies,
	StorageClasses,
	Roles, RoleBindings, ClusterRoles, ClusterRoleBindings,
	HorizontalPodAutoscalers,
	PodDisruptionBudgets,
	CustomResourceDefinitions,
}
