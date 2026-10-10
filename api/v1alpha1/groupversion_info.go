// Package v1alpha1 is the workspace-manager's API: the Workspace, the set of
// repositories an agent Session works on, owned by an Organization.
//
// +kubebuilder:object:generate=true
// +groupName=workspace-manager.giantswarm.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the API's group and version.
var GroupVersion = schema.GroupVersion{Group: "workspace-manager.giantswarm.io", Version: "v1alpha1"}

// WorkspaceResource is the Workspace's resource, for the dynamic client.
var WorkspaceResource = GroupVersion.WithResource("workspaces")

// AddToScheme registers the API's types.
func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &Workspace{}, &WorkspaceList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
