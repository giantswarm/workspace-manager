package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
)

// CRDResource is the CustomResourceDefinitions' resource, for the dynamic
// client.
var CRDResource = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// WorkspaceCRD is the Workspace CRD's name.
var WorkspaceCRD = v1alpha1.WorkspaceResource.Resource + "." + v1alpha1.GroupVersion.Group

// requiredStatusFields are the status fields the API server must keep for
// the controller to work: a Workspace whose status drops activeSync never
// records its running sync Job, and the controller starts the next one.
var requiredStatusFields = []string{"activeSync"}

// CheckCRD reads the served Workspace CRD and fails when the version the
// controller writes prunes a status field it relies on: an older or
// hand-pruned CRD, which the manager refuses rather than syncing without end.
func CheckCRD(ctx context.Context, client dynamic.Interface) error {
	crd, err := client.Resource(CRDResource).Get(ctx, WorkspaceCRD, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("the Workspace CRD %s is not installed: install the CRD this version ships (the chart's crds.install)", WorkspaceCRD)
	}
	if err != nil {
		return fmt.Errorf("read the Workspace CRD %s: %w", WorkspaceCRD, err)
	}
	version := v1alpha1.GroupVersion.Version
	s, err := servedSchema(crd, version)
	if err != nil {
		return err
	}
	for _, field := range requiredStatusFields {
		if !keeps(s, "status", field) {
			return fmt.Errorf("the Workspace CRD %s (version %s) has no status.%s in its schema, so the API server drops it and the controller cannot record a running sync Job: install the CRD this version ships (the chart's crds.install)",
				WorkspaceCRD, version, field)
		}
	}
	return nil
}

// servedSchema is the openAPIV3Schema of the CRD's served version.
func servedSchema(crd *unstructured.Unstructured, version string) (map[string]any, error) {
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	var names []string
	for _, v := range versions {
		v, ok := v.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(v, "name")
		if served, _, _ := unstructured.NestedBool(v, "served"); !served {
			continue
		}
		names = append(names, name)
		if name != version {
			continue
		}
		s, _ := nestedMap(v, "schema", "openAPIV3Schema")
		return s, nil
	}
	return nil, fmt.Errorf("the Workspace CRD %s does not serve %s (served: %s): install the CRD this version ships (the chart's crds.install)",
		WorkspaceCRD, version, strings.Join(names, ", "))
}

// keeps reports whether a structural schema keeps the field at path: each
// step is a declared property, or the object above it keeps unknown fields.
func keeps(s map[string]any, path ...string) bool {
	node := s
	for _, step := range path {
		if preserve, _, _ := unstructured.NestedBool(node, "x-kubernetes-preserve-unknown-fields"); preserve {
			return true
		}
		next, found := nestedMap(node, "properties", step)
		if !found {
			return false
		}
		node = next
	}
	return true
}

// nestedMap is the object at path, without a copy.
func nestedMap(obj map[string]any, path ...string) (map[string]any, bool) {
	v, found, err := unstructured.NestedFieldNoCopy(obj, path...)
	if !found || err != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}
