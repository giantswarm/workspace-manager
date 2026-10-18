package api

import (
	"context"
	"sync"
	"testing"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/giantswarm/mcp-toolkit/tooldesc/tooldesctest"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/workspace-manager/internal/authorize"
	"github.com/giantswarm/workspace-manager/internal/provider"
)

type fakeKube struct{}

func (fakeKube) Dynamic() dynamic.Interface {
	return dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
}
func (fakeKube) Typed() kubernetes.Interface             { return k8sfake.NewClientset() }
func (fakeKube) Discovery() discovery.DiscoveryInterface { return nil }

type noSecrets struct{}

func (noSecrets) Value(context.Context, provider.SecretRef) ([]byte, error) { return nil, nil }

// docsServer registers every tool; the descriptions need none of the
// clients.
var docsServer = sync.OnceValue(func() *mcpserver.MCPServer {
	return NewMCPServer(Config{
		Kube:       fakeKube{},
		Providers:  []provider.Instance{{Name: "github", KindName: "github"}},
		Secrets:    noSecrets{},
		Authorizer: &authorize.Authorizer{},
	}, "docs")
})

// TestToolsDocIsCurrent keeps docs/tools.md — every tool's description and
// input schema as a model reads them — equal to the registered tools.
// TOOLS_DOC_UPDATE=1 rewrites the file (`make tools-doc`).
func TestToolsDocIsCurrent(t *testing.T) {
	tooldesctest.AssertServerDoc(t, docsServer(),
		"# The tools\n\nGenerated from the registered tools (`make tools-doc`), with every tool registered; through muster every tool is `x_<server>_<name>`. "+
			"`list_providers` is registered only with provider instances, `list_repositories` and `preview_workspace` only with provider instances and their Secrets, "+
			"`authorize_workspace` only with a grant signing key.",
		"../../docs/tools.md")
}

// TestToolDescriptionBudgets holds the registered descriptions to the
// budget of giantswarm/giantswarm#38592.
func TestToolDescriptionBudgets(t *testing.T) {
	require.NoError(t, tooldesc.CheckServer(docsServer(), tooldesc.DefaultBudget))
}

// TestReadOnlyToolsAreNotDestructive: a read-only tool says so in full, or
// mcp-go's defaults report it destructive and not idempotent.
func TestReadOnlyToolsAreNotDestructive(t *testing.T) {
	for _, st := range docsServer().ListTools() {
		a := st.Tool.Annotations
		if a.ReadOnlyHint == nil || !*a.ReadOnlyHint {
			continue
		}
		require.Equal(t, mcp.ToBoolPtr(false), a.DestructiveHint, "%s is read-only but reported destructive", st.Tool.Name)
		require.Equal(t, mcp.ToBoolPtr(true), a.IdempotentHint, "%s is read-only but not reported idempotent", st.Tool.Name)
	}
}
