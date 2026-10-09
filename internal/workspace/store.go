package workspace

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
)

// Store reads and writes the Workspaces of one namespace for the caller in
// the context, as the manager's ServiceAccount, after Organizations.Check.
type Store struct {
	client    dynamic.ResourceInterface
	orgs      Organizations
	providers []string
}

// NewStore returns a Store on the Workspaces of namespace. providers are the
// configured provider instances' names.
func NewStore(client dynamic.Interface, namespace string, orgs Organizations, providers []string) *Store {
	return &Store{
		client:    client.Resource(v1alpha1.WorkspaceResource).Namespace(namespace),
		orgs:      orgs,
		providers: providers,
	}
}

// List returns the Workspaces of every Organization the caller is a member
// of; the others' stay invisible.
func (s *Store) List(ctx context.Context) ([]v1alpha1.Workspace, error) {
	list, err := s.client.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	out := []v1alpha1.Workspace{}
	for i := range list.Items {
		ws, err := fromUnstructured(&list.Items[i])
		if err != nil {
			return nil, err
		}
		if s.orgs.Check(ctx, ws.Spec.Organization) == nil {
			out = append(out, *ws)
		}
	}
	return out, nil
}

// Get returns a Workspace of an Organization the caller is a member of.
func (s *Store) Get(ctx context.Context, name string) (*v1alpha1.Workspace, error) {
	ws, err := s.get(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.Check(ctx, ws.Spec.Organization); err != nil {
		return nil, fmt.Errorf("workspace %q: %w", name, err)
	}
	return ws, nil
}

// Create writes a new Workspace for an Organization the caller is a member
// of, naming only configured provider instances.
func (s *Store) Create(ctx context.Context, ws *v1alpha1.Workspace) (*v1alpha1.Workspace, error) {
	if err := s.orgs.Check(ctx, ws.Spec.Organization); err != nil {
		return nil, fmt.Errorf("workspace %q: %w", ws.Name, err)
	}
	if err := Validate(ws, s.providers); err != nil {
		return nil, err
	}
	u, err := toUnstructured(ws)
	if err != nil {
		return nil, err
	}
	created, err := s.client.Create(ctx, u, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create workspace %q: %w", ws.Name, err)
	}
	return fromUnstructured(created)
}

// Update replaces a Workspace's spec. The caller must be a member of the
// Organization the stored Workspace belongs to, and of the one ws names (the
// API server refuses a change of Organization).
func (s *Store) Update(ctx context.Context, ws *v1alpha1.Workspace) (*v1alpha1.Workspace, error) {
	current, err := s.Get(ctx, ws.Name)
	if err != nil {
		return nil, err
	}
	if err := s.orgs.Check(ctx, ws.Spec.Organization); err != nil {
		return nil, fmt.Errorf("workspace %q: %w", ws.Name, err)
	}
	if err := Validate(ws, s.providers); err != nil {
		return nil, err
	}
	next := current.DeepCopy()
	next.Spec = ws.Spec
	if ws.ResourceVersion != "" {
		next.ResourceVersion = ws.ResourceVersion
	}
	u, err := toUnstructured(next)
	if err != nil {
		return nil, err
	}
	updated, err := s.client.Update(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("update workspace %q: %w", ws.Name, err)
	}
	return fromUnstructured(updated)
}

// Delete removes a Workspace of an Organization the caller is a member of.
func (s *Store) Delete(ctx context.Context, name string) error {
	ws, err := s.Get(ctx, name)
	if err != nil {
		return err
	}
	// The precondition makes sure what is deleted is what was checked.
	if err := s.client.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &ws.UID}}); err != nil {
		return fmt.Errorf("delete workspace %q: %w", name, err)
	}
	return nil
}

func (s *Store) get(ctx context.Context, name string) (*v1alpha1.Workspace, error) {
	u, err := s.client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get workspace %q: %w", name, err)
	}
	return fromUnstructured(u)
}

func toUnstructured(ws *v1alpha1.Workspace) (*unstructured.Unstructured, error) {
	ws = ws.DeepCopy()
	ws.APIVersion = v1alpha1.GroupVersion.String()
	ws.Kind = "Workspace"
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ws)
	if err != nil {
		return nil, fmt.Errorf("workspace %q: %w", ws.Name, err)
	}
	u := &unstructured.Unstructured{Object: obj}
	// The status is the manager's controller's, written through its own
	// subresource.
	unstructured.RemoveNestedField(u.Object, "status")
	return u, nil
}

func fromUnstructured(u *unstructured.Unstructured) (*v1alpha1.Workspace, error) {
	ws := &v1alpha1.Workspace{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, ws); err != nil {
		return nil, fmt.Errorf("workspace %q: %w", u.GetName(), err)
	}
	return ws, nil
}
