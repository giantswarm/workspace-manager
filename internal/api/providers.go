package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"golang.org/x/oauth2"

	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/muster"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/workspace"
)

// ProviderStatus is one provider instance and whether the person granted its
// App in muster.
type ProviderStatus struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Connected bool   `json:"connected"`
	// Login is the account the person is signed in as on a connected
	// instance, as the provider answers for their token.
	Login string `json:"login,omitempty"`
	// SignIn says how the person grants the App, while they have not.
	SignIn string `json:"signIn,omitempty"`
}

// ProvidersResult is list_providers' result.
type ProvidersResult struct {
	Providers []ProviderStatus `json:"providers"`
}

// addProviderTools registers list_providers.
func addProviderTools(s *mcpserver.MCPServer, instances []provider.Instance, grants muster.Grants) {
	s.AddTool(mcp.NewTool("list_providers",
		tooldesc.Describe("List the installation's workspace providers, each with its kind and whether you granted its App in muster.",
			"With the account you are signed in as on each connected one. muster is the one place your provider sign-in lives; a sign-in the provider refuses shows as not connected."),
		readOnly(),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithOutputSchema[ProvidersResult](),
	), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if _, err := callerIdentity(ctx); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		list := make([]ProviderStatus, len(instances))
		var wg sync.WaitGroup
		for i, in := range instances {
			list[i] = ProviderStatus{Name: in.Name, Kind: in.KindName}
			token, ok := grants.Token(ctx, in.Name)
			if !ok {
				list[i].SignIn = grants.SignIn(in.Name)
				continue
			}
			list[i].Connected = true
			if in.Kind == nil {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				login, err := in.Person(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}))
				switch {
				case errors.Is(err, provider.ErrCredentialRefused):
					list[i].Connected, list[i].SignIn = false, grants.SignIn(in.Name)
				case err != nil:
					slog.WarnContext(ctx, "the provider did not name the signed-in account", "provider", in.Name, "error", err)
				default:
					list[i].Login = login
				}
			}()
		}
		wg.Wait()
		return mcp.NewToolResultStructured(ProvidersResult{Providers: list}, fmt.Sprintf("%d providers", len(list))), nil
	})
}

// OwnersInput is list_owners' input.
type OwnersInput struct {
	Organization string `json:"organization" jsonschema:"The Organization whose workspace you edit; you must be a member"`
	Provider     string `json:"provider" jsonschema:"The provider instance's name, as list_providers shows it"`
}

// Owner is one owner the person reaches on a provider instance.
type Owner struct {
	Name string `json:"name"`
	// Kind is organization, group or user.
	Kind string `json:"kind"`
}

// OwnersResult is list_owners' result.
type OwnersResult struct {
	Provider string  `json:"provider"`
	Owners   []Owner `json:"owners"`
	// Status is muster.StatusGrantRequired while the person has not granted
	// the provider's App, which Apps names for muster's sign-in link; SignIn
	// says how.
	Status string   `json:"status,omitempty"`
	Apps   []string `json:"apps,omitempty"`
	SignIn string   `json:"signIn,omitempty"`
}

// addOwnersTool registers list_owners.
func addOwnersTool(s *mcpserver.MCPServer, store *workspace.Store, instances []provider.Instance, grants muster.Grants) {
	s.AddTool(mcp.NewTool("list_owners",
		tooldesc.Describe("List the owners you reach on a provider instance, which a workspace's source picks repositories from.",
			"The organizations, groups and user account, fetched with your own sign-in and sorted by name; without your grant of the provider's App, how to grant it."),
		mcp.WithInputSchema[OwnersInput](),
		readOnly(),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithOutputSchema[OwnersResult](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in OwnersInput
		if err := req.BindArguments(&in); err != nil {
			return mcp.NewToolResultError("invalid arguments: " + err.Error()), nil
		}
		if err := store.CheckOrganization(ctx, in.Organization); err != nil {
			return toolError("list owners", err), nil
		}
		i := slices.IndexFunc(instances, func(p provider.Instance) bool { return p.Name == in.Provider })
		if i < 0 {
			return mcp.NewToolResultError(fmt.Sprintf("list owners: unknown provider %q", in.Provider)), nil
		}
		res := OwnersResult{Provider: in.Provider, Owners: []Owner{}}
		grantRequired := func() *mcp.CallToolResult {
			res.SignIn = grants.SignIn(in.Provider)
			if grants.App(in.Provider) {
				res.Status, res.Apps = muster.StatusGrantRequired, []string{in.Provider}
			}
			return mcp.NewToolResultStructured(res, res.SignIn)
		}
		token, ok := grants.Token(ctx, in.Provider)
		if !ok {
			return grantRequired(), nil
		}
		owners, err := instances[i].Owners(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}))
		switch {
		case errors.Is(err, provider.ErrCredentialRefused):
			return grantRequired(), nil
		case err != nil:
			return mcp.NewToolResultErrorFromErr("list owners", err), nil
		}
		seen := map[string]bool{}
		for _, o := range owners {
			if key := strings.ToLower(o.Name); !seen[key] {
				seen[key] = true
				res.Owners = append(res.Owners, Owner{Name: o.Name, Kind: string(o.Kind)})
			}
		}
		slices.SortFunc(res.Owners, func(a, b Owner) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
		names := make([]string, len(res.Owners))
		for j, o := range res.Owners {
			names[j] = o.Name
		}
		return mcp.NewToolResultStructured(res, fmt.Sprintf("%d owners on %s: %s", len(names), in.Provider, strings.Join(names, ", "))), nil
	})
}

// callerIdentity is the authenticated caller, without whom no tool acts.
func callerIdentity(ctx context.Context) (*identity.Identity, error) {
	id, ok := identity.FromContext(ctx)
	if !ok || id.Subject == "" {
		return nil, errors.New("no caller: this tool needs the server's OAuth")
	}
	return id, nil
}
