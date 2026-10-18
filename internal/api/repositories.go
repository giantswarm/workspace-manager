package api

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/controller"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/workspace"
)

const (
	// DefaultPageSize is a page's size when the call names none.
	DefaultPageSize = 50
	// MaxPageSize is the largest page a call gets.
	MaxPageSize = 200
)

// Page selects one page of a long answer.
type Page struct {
	Page     int `json:"page,omitempty" jsonschema:"The page, from 1 (the default)"`
	PageSize int `json:"pageSize,omitempty" jsonschema:"Repositories per page: 50 by default, at most 200"`
}

// RepositoriesInput is list_repositories' input.
type RepositoriesInput struct {
	Organization string   `json:"organization" jsonschema:"The Organization whose workspace you edit; you must be a member"`
	Provider     string   `json:"provider" jsonschema:"The provider instance's name, as list_providers shows it"`
	Owner        string   `json:"owner" jsonschema:"The owner on the provider: an organization or a user account"`
	Search       string   `json:"search,omitempty" jsonschema:"Only repositories whose name contains this, ignoring case"`
	Languages    []string `json:"languages,omitempty" jsonschema:"Only repositories whose primary language is any of these"`
	Topics       []string `json:"topics,omitempty" jsonschema:"Only repositories carrying any of these topics"`
	Archived     *bool    `json:"archived,omitempty" jsonschema:"true: only archived repositories; false: none; unset: both"`
	Fork         *bool    `json:"fork,omitempty" jsonschema:"true: only forks; false: none; unset: both"`
	Page
}

// Repository is one repository as its provider lists it.
type Repository struct {
	Name     string    `json:"name"`
	Language string    `json:"language,omitempty"`
	Topics   []string  `json:"topics"`
	Archived bool      `json:"archived,omitempty"`
	Fork     bool      `json:"fork,omitempty"`
	LastPush time.Time `json:"lastPush"`
	SizeKiB  int64     `json:"sizeKiB"`
}

// RepositoriesResult is list_repositories' result.
type RepositoriesResult struct {
	Provider string `json:"provider"`
	Owner    string `json:"owner"`
	// Total is the number of repositories matching the search and filters;
	// OwnerTotal the owner's repositories.
	Total        int          `json:"total"`
	OwnerTotal   int          `json:"ownerTotal"`
	Page         int          `json:"page"`
	PageSize     int          `json:"pageSize"`
	Repositories []Repository `json:"repositories"`
	// Languages and Topics are every value the owner's repositories carry,
	// for the picker's filters.
	Languages []string `json:"languages"`
	Topics    []string `json:"topics"`
}

// PreviewInput is preview_workspace's input: an existing workspace by its
// name, or a draft.
type PreviewInput struct {
	Workspace    string            `json:"workspace,omitempty" jsonschema:"An existing workspace to preview; or give organization and sources"`
	Organization string            `json:"organization,omitempty" jsonschema:"The draft's Organization; you must be a member"`
	Sources      []v1alpha1.Source `json:"sources,omitempty" jsonschema:"The draft's sources, as create_workspace takes them"`
	Sizing       *Sizing           `json:"sizing,omitempty" jsonschema:"The draft's sizing overrides"`
	Search       string            `json:"search,omitempty" jsonschema:"Only selected repositories whose owner/name contains this, ignoring case"`
	Page
}

// PreviewRepository is one repository a selection holds now, and why.
type PreviewRepository struct {
	Provider   string    `json:"provider"`
	Repository string    `json:"repository"`
	Language   string    `json:"language,omitempty"`
	Topics     []string  `json:"topics"`
	LastPush   time.Time `json:"lastPush"`
	SizeKiB    int64     `json:"sizeKiB"`
	// Why are the selectors that take it: name, language <l>, topic <t>.
	Why []string `json:"why"`
}

// PreviewResult is preview_workspace's result.
type PreviewResult struct {
	Workspace string `json:"workspace,omitempty"`
	// Total is the number of repositories the selection holds now.
	Total int `json:"total"`
	// Excluded, SkippedArchived and SkippedForks count what a selector
	// matched and the exclusions, archived or fork rule left out.
	Excluded        int `json:"excluded"`
	SkippedArchived int `json:"skippedArchived"`
	SkippedForks    int `json:"skippedForks"`
	// ProviderSizeKiB is the selection's size as its providers report it;
	// EstimatedVolumeSize the size a volume is claimed with for it, where
	// its StorageClass provisions one.
	ProviderSizeKiB     int64               `json:"providerSizeKiB"`
	EstimatedVolumeSize string              `json:"estimatedVolumeSize"`
	Matching            int                 `json:"matching"`
	Page                int                 `json:"page"`
	PageSize            int                 `json:"pageSize"`
	Repositories        []PreviewRepository `json:"repositories"`
}

// selectionTools serve list_repositories and preview_workspace: from the
// providers' cached listings with their own sync credentials, after the
// caller's Organization check; they write nothing and say nothing about the
// caller's own access at the provider.
type selectionTools struct {
	store    *workspace.Store
	listings *workspace.Listings
	sizing   controller.Sizing
}

func addSelectionTools(s *mcpserver.MCPServer, t *selectionTools) {
	s.AddTool(mcp.NewTool("list_repositories",
		tooldesc.Describe("List one owner's repositories on a provider instance for a workspace's picker: searched by name, filtered and paged.",
			"Filtered by language, topics, archived and forks; each with language, topics, last push and size, from the provider's cached listing; "+
				"it says nothing about your own access to them."),
		mcp.WithInputSchema[RepositoriesInput](),
		readOnly(),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithOutputSchema[RepositoriesResult](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in RepositoriesInput
		if err := req.BindArguments(&in); err != nil {
			return mcp.NewToolResultError("invalid arguments: " + err.Error()), nil
		}
		if err := t.store.CheckOrganization(ctx, in.Organization); err != nil {
			return toolError("list repositories", err), nil
		}
		if in.Provider == "" || in.Owner == "" {
			return mcp.NewToolResultError("list repositories: provider and owner are required"), nil
		}
		items, err := t.listings.List(ctx, in.Provider, in.Owner)
		if err != nil {
			return toolError("list repositories", err), nil
		}
		res := listRepositories(items, in)
		return mcp.NewToolResultStructured(res, fmt.Sprintf("%d of %d repositories of %s match; page %d of %d per page.",
			res.Total, res.OwnerTotal, in.Owner, res.Page, res.PageSize)), nil
	})

	s.AddTool(mcp.NewTool("preview_workspace",
		tooldesc.Describe("Preview the repositories a workspace selects now, an existing one or a draft, and its estimated volume size.",
			"Paged, each repository with language, topics, last push and why it is in; "+
				"also what the exclusions and the archived and fork rules left out. Writes nothing and says nothing about your own access to the repositories."),
		mcp.WithInputSchema[PreviewInput](),
		readOnly(),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithOutputSchema[PreviewResult](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in PreviewInput
		if err := req.BindArguments(&in); err != nil {
			return mcp.NewToolResultError("invalid arguments: " + err.Error()), nil
		}
		draft, err := t.draft(ctx, in)
		if err != nil {
			return toolError("preview workspace", err), nil
		}
		sel, err := t.listings.Resolve(ctx, draft.Spec.Sources)
		if err != nil {
			return toolError("preview workspace", err), nil
		}
		res := t.preview(sel, draft, in)
		return mcp.NewToolResultStructured(res, fmt.Sprintf("%d repositories selected now (%d excluded, %d archived and %d forks skipped); estimated volume %s.",
			res.Total, res.Excluded, res.SkippedArchived, res.SkippedForks, res.EstimatedVolumeSize)), nil
	})
}

// draft is the workspace to preview: the stored one, read after its
// Organization check, or the caller's draft after theirs.
func (t *selectionTools) draft(ctx context.Context, in PreviewInput) (*v1alpha1.Workspace, error) {
	if in.Workspace != "" {
		if len(in.Sources) > 0 || in.Organization != "" {
			return nil, fmt.Errorf("%w: give a workspace's name or a draft's organization and sources, not both", workspace.ErrInvalid)
		}
		ws, err := t.store.Get(ctx, in.Workspace)
		if err != nil {
			return nil, err
		}
		// A new claim's size, from what the providers report.
		return &v1alpha1.Workspace{ObjectMeta: ws.ObjectMeta, Spec: ws.Spec}, nil
	}
	if err := t.store.CheckOrganization(ctx, in.Organization); err != nil {
		return nil, err
	}
	if len(in.Sources) == 0 {
		return nil, fmt.Errorf("%w: a draft needs at least one source", workspace.ErrInvalid)
	}
	sizing, err := in.Sizing.spec()
	if err != nil {
		return nil, err
	}
	return &v1alpha1.Workspace{Spec: v1alpha1.WorkspaceSpec{Organization: in.Organization, Sources: in.Sources, Sizing: sizing}}, nil
}

func (t *selectionTools) preview(sel *workspace.Selection, ws *v1alpha1.Workspace, in PreviewInput) PreviewResult {
	res := PreviewResult{Workspace: in.Workspace, Total: len(sel.Items), Excluded: sel.Excluded,
		SkippedArchived: sel.SkippedArchived, SkippedForks: sel.SkippedForks, Repositories: []PreviewRepository{}}
	search := strings.ToLower(in.Search)
	var matching []PreviewRepository
	items := make([]provider.Item, 0, len(sel.Items))
	for _, it := range sel.Items {
		res.ProviderSizeKiB += it.SizeKiB
		items = append(items, it.Item)
		if search != "" && !strings.Contains(strings.ToLower(it.Key()), search) {
			continue
		}
		matching = append(matching, PreviewRepository{Provider: it.Provider, Repository: it.Key(), Language: it.Language,
			Topics: nonNil(it.Topics), LastPush: it.LastChange.UTC(), SizeKiB: it.SizeKiB, Why: nonNil(it.Why)})
	}
	// The size a volume gets for the selection as nothing of it is mirrored
	// yet: the estimate for every repository, the headroom, the overrides.
	size := t.sizing.Size(t.sizing.Need(nil, items, nil), ws.Spec.Sizing)
	res.EstimatedVolumeSize = size.String()
	res.Matching = len(matching)
	var page []PreviewRepository
	page, res.Page, res.PageSize = paginate(matching, in.Page)
	res.Repositories = append(res.Repositories, page...)
	return res
}

// listRepositories filters, sorts by name and pages an owner's listing.
func listRepositories(items []provider.Item, in RepositoriesInput) RepositoriesResult {
	res := RepositoriesResult{Provider: in.Provider, Owner: in.Owner, OwnerTotal: len(items), Repositories: []Repository{}}
	langs, topics := map[string]bool{}, map[string]bool{}
	wantLangs, wantTopics := lowerSet(in.Languages), lowerSet(in.Topics)
	search := strings.ToLower(in.Search)
	var matching []Repository
	for _, it := range items {
		if it.Language != "" {
			langs[it.Language] = true
		}
		for _, tp := range it.Topics {
			topics[tp] = true
		}
		switch {
		case search != "" && !strings.Contains(strings.ToLower(it.Name), search),
			len(wantLangs) > 0 && !wantLangs[strings.ToLower(it.Language)],
			len(wantTopics) > 0 && !slices.ContainsFunc(it.Topics, func(tp string) bool { return wantTopics[strings.ToLower(tp)] }),
			in.Archived != nil && it.Archived != *in.Archived,
			in.Fork != nil && it.Fork != *in.Fork:
			continue
		}
		matching = append(matching, Repository{Name: it.Name, Language: it.Language, Topics: nonNil(it.Topics),
			Archived: it.Archived, Fork: it.Fork, LastPush: it.LastChange.UTC(), SizeKiB: it.SizeKiB})
	}
	sort.Slice(matching, func(i, j int) bool { return strings.ToLower(matching[i].Name) < strings.ToLower(matching[j].Name) })
	res.Total = len(matching)
	var page []Repository
	page, res.Page, res.PageSize = paginate(matching, in.Page)
	res.Repositories = append(res.Repositories, page...)
	res.Languages, res.Topics = sortedKeys(langs), sortedKeys(topics)
	return res
}

// paginate is one page of all, with the page and size it used.
func paginate[T any](all []T, p Page) ([]T, int, int) {
	size := p.PageSize
	switch {
	case size <= 0:
		size = DefaultPageSize
	case size > MaxPageSize:
		size = MaxPageSize
	}
	page := max(p.Page, 1)
	start := min((page-1)*size, len(all))
	return all[start:min(start+size, len(all))], page, size
}

func lowerSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[strings.ToLower(s)] = true
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func nonNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}
