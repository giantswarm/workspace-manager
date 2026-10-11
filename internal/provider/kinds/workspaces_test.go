package kinds

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/api"
	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/mirror"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/fake"
	"github.com/giantswarm/workspace-manager/internal/provider/github"
	"github.com/giantswarm/workspace-manager/internal/provider/github/githubtest"
	"github.com/giantswarm/workspace-manager/internal/workspace"
)

const wsNamespace = "kagent"

// kubeClients is a kube.Client over given clients.
type kubeClients struct {
	dyn  dynamic.Interface
	core kubernetes.Interface
}

func (k kubeClients) Dynamic() dynamic.Interface              { return k.dyn }
func (k kubeClients) Typed() kubernetes.Interface             { return k.core }
func (k kubeClients) Discovery() discovery.DiscoveryInterface { return nil }

// secretsOf reads a ref from the first of several Secrets that has it.
type secretsOf []provider.Secrets

func (s secretsOf) Value(ctx context.Context, ref provider.SecretRef) ([]byte, error) {
	var last error
	for _, one := range s {
		v, err := one.Value(ctx, ref)
		if err == nil {
			return v, nil
		}
		last = err
	}
	return nil, last
}

// toolsFixture is the MCP server over two provider instances: GitHub (the
// stub API) with acme's 500 repositories, and the fake kind with umbrella's.
type toolsFixture struct {
	gh     *githubtest.Server
	cfg    api.Config
	member context.Context
	other  context.Context
}

func newToolsFixture(t *testing.T, kube kubeClients) *toolsFixture {
	t.Helper()
	gh := githubtest.NewServer(t)
	pushed := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := range 500 {
		it := provider.Item{Owner: "acme", Name: fmt.Sprintf("repo-%03d", i), Language: "TypeScript", SizeKiB: 1024,
			LastChange: pushed.Add(time.Duration(i) * time.Minute)}
		if i%2 == 0 {
			it.Language = "Go"
		}
		if i%10 == 0 {
			it.Topics = []string{"mcp"}
		}
		it.Archived, it.Fork = i%50 == 10, i%50 == 20
		gh.Put(it)
	}
	ghKind, err := github.Factory(github.Options{Clock: gh.Clock})(gh.Values())
	require.NoError(t, err)
	backend := fake.NewBackend()
	backend.Put(provider.Item{Owner: "umbrella", Name: "theirs", Language: "Rust", SizeKiB: 2048, LastChange: pushed})
	backend.Put(provider.Item{Owner: "umbrella", Name: "docs", LastChange: pushed})
	fkKind, err := fake.Factory(backend.Store)(backend.Values())
	require.NoError(t, err)

	return &toolsFixture{
		gh: gh,
		cfg: api.Config{
			Kube:          kube,
			Namespace:     wsNamespace,
			Organizations: workspace.Organizations{"acme": {"acme-devs"}, "umbrella": {"umbrella-devs"}},
			Providers: []provider.Instance{
				{Name: "github", KindName: github.KindName, Kind: ghKind},
				{Name: "code", KindName: fake.KindName, Kind: fkKind},
			},
			Secrets: secretsOf{gh.Secrets(), backend.Secrets()},
		},
		member: identity.ContextWith(context.Background(), &identity.Identity{Subject: "alice", Email: "alice@acme.test", Groups: []string{"acme-devs"}}),
		other:  identity.ContextWith(context.Background(), &identity.Identity{Subject: "bob", Email: "bob@umbrella.test", Groups: []string{"umbrella-devs"}}),
	}
}

func (f *toolsFixture) call(t *testing.T, ctx context.Context, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	msg, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	require.NoError(t, err)
	resp := api.NewMCPServer(f.cfg, "test").HandleMessage(ctx, msg)
	out, ok := resp.(mcp.JSONRPCResponse)
	require.True(t, ok, "response %#v", resp)
	res, ok := out.Result.(*mcp.CallToolResult)
	require.True(t, ok, "result %#v", out.Result)
	return res
}

// ok calls a tool that must succeed and decodes its structured result.
func ok[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	require.False(t, res.IsError, "tool error: %s", text(res))
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	var v T
	require.NoError(t, json.Unmarshal(raw, &v))
	return v
}

func text(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func refused(t *testing.T, res *mcp.CallToolResult, want string) {
	t.Helper()
	require.True(t, res.IsError, "want a refusal, got %v", res.StructuredContent)
	assert.Contains(t, text(res), want)
}

var platformSources = []any{
	map[string]any{"provider": "github", "owner": "acme",
		"filters": map[string]any{"languages": []string{"Go"}, "topics": map[string]any{"names": []string{"mcp"}}},
		"exclude": []string{"repo-000"}},
	map[string]any{"provider": "code", "owner": "umbrella", "repositories": []string{"theirs"}},
}

// workspaceToolsFlow is what a member and a non-member do through the tools,
// against the given API server.
func workspaceToolsFlow(t *testing.T, f *toolsFixture) {
	t.Run("a member creates, lists, gets, previews, updates and deletes", func(t *testing.T) {
		created := ok[api.Workspace](t, f.call(t, f.member, "create_workspace", map[string]any{
			"name": "platform", "organization": "acme", "sources": platformSources, "sizing": map[string]any{"minimum": "8Gi"}}))
		assert.Equal(t, "acme", created.Organization)
		assert.Equal(t, v1alpha1.ScheduleNightly, created.Sync.Schedule)
		assert.Equal(t, "8Gi", created.Sizing.Minimum)
		assert.False(t, created.NextSync.IsZero(), "a nightly workspace has a next sync")

		assert.Contains(t, workspaceNames(ok[api.WorkspacesResult](t, f.call(t, f.member, "list_workspaces", nil))), "platform")
		assert.NotContains(t, workspaceNames(ok[api.WorkspacesResult](t, f.call(t, f.other, "list_workspaces", nil))), "platform",
			"another Organization's workspace stays invisible")

		got := ok[api.WorkspaceDetail](t, f.call(t, f.member, "get_workspace", map[string]any{"workspace": "platform"}))
		assert.Equal(t, created.ResourceVersion, got.Workspace.ResourceVersion)
		assert.Empty(t, got.Mirrors, "no mirror before the first sync")

		// The stored workspace and the draft select the same: acme's Go
		// repositories with the topic mcp, but for the excluded repo-000
		// and the archived and forked ones, and umbrella/theirs by name.
		for _, args := range []map[string]any{
			{"workspace": "platform"},
			{"organization": "acme", "sources": platformSources, "sizing": map[string]any{"minimum": "8Gi"}},
		} {
			p := ok[api.PreviewResult](t, f.call(t, f.member, "preview_workspace", args))
			// repo-000…490 every tenth; repo-010/060/… are archived, 020/070/… forks.
			assert.Equal(t, 50-1-10-10+1, p.Total)
			assert.Equal(t, 1, p.Excluded)
			assert.Equal(t, 10, p.SkippedArchived)
			assert.Equal(t, 10, p.SkippedForks)
			assert.Equal(t, int64(29*1024+2048), p.ProviderSizeKiB)
			assert.Equal(t, "8Gi", p.EstimatedVolumeSize, "the minimum wins over 1.5 × 31 MiB plus the default headroom")
			require.NotEmpty(t, p.Repositories)
			byKey := map[string]api.PreviewRepository{}
			for _, r := range p.Repositories {
				byKey[r.Provider+"/"+r.Repository] = r
			}
			assert.Equal(t, []string{"name"}, byKey["code/umbrella/theirs"].Why)
			assert.Equal(t, []string{"language Go", "topic mcp"}, byKey["github/acme/repo-030"].Why)
			assert.NotContains(t, byKey, "github/acme/repo-000")
		}
		page := ok[api.PreviewResult](t, f.call(t, f.member, "preview_workspace", map[string]any{"workspace": "platform", "search": "repo-03", "pageSize": 1}))
		assert.Equal(t, 1, page.Matching)
		require.Len(t, page.Repositories, 1)
		assert.Equal(t, "acme/repo-030", page.Repositories[0].Repository)

		updated := ok[api.Workspace](t, f.call(t, f.member, "update_workspace", map[string]any{
			"name": "platform", "sources": platformSources[1:], "sync": map[string]any{"schedule": "custom", "weekdays": []string{"monday"}, "hour": 4}}))
		require.Len(t, updated.Sources, 1)
		assert.Equal(t, v1alpha1.ScheduleCustom, updated.Sync.Schedule)
		assert.Equal(t, time.Monday, updated.NextSync.Weekday())
		assert.Equal(t, 4, updated.NextSync.Hour())
		assert.Equal(t, "8Gi", updated.Sizing.Minimum, "sizing given nowhere is kept")

		deleted := ok[api.DeleteResult](t, f.call(t, f.member, "delete_workspace", map[string]any{"workspace": "platform"}))
		assert.Equal(t, "platform", deleted.Workspace)
		refused(t, f.call(t, f.member, "get_workspace", map[string]any{"workspace": "platform"}), "not found")
	})

	t.Run("a non-member is refused on every tool", func(t *testing.T) {
		ok[api.Workspace](t, f.call(t, f.member, "create_workspace", map[string]any{"name": "theirs", "organization": "acme", "sources": platformSources}))
		t.Cleanup(func() { f.call(t, f.member, "delete_workspace", map[string]any{"workspace": "theirs"}) })
		for name, args := range map[string]map[string]any{
			"list_workspaces":   {"organization": "acme"},
			"get_workspace":     {"workspace": "theirs"},
			"create_workspace":  {"name": "mine", "organization": "acme", "sources": platformSources},
			"update_workspace":  {"name": "theirs", "sources": platformSources},
			"delete_workspace":  {"workspace": "theirs"},
			"list_repositories": {"organization": "acme", "provider": "github", "owner": "acme"},
			"preview_workspace": {"workspace": "theirs"},
		} {
			t.Run(name, func(t *testing.T) {
				refused(t, f.call(t, f.other, name, args), "permission denied")
			})
		}
		refused(t, f.call(t, f.other, "preview_workspace", map[string]any{"organization": "acme", "sources": platformSources}), "permission denied")
		refused(t, f.call(t, context.Background(), "list_repositories", map[string]any{"organization": "acme", "provider": "github", "owner": "acme"}), "no authenticated caller")
		got := ok[api.WorkspaceDetail](t, f.call(t, f.member, "get_workspace", map[string]any{"workspace": "theirs"}))
		assert.Len(t, got.Workspace.Sources, 2, "the refused update changed nothing")
	})

	t.Run("an invalid spec is refused with its reason and nothing is written", func(t *testing.T) {
		for _, c := range []struct {
			args map[string]any
			want string
		}{
			{map[string]any{"name": "bad", "organization": "acme", "sources": []any{map[string]any{"provider": "gitlab", "owner": "acme", "repositories": []string{"a"}}}},
				`unknown provider instance "gitlab" (configured: code, github)`},
			{map[string]any{"name": "bad", "organization": "acme", "sources": platformSources, "sizing": map[string]any{"headroom": "lots"}},
				`sizing.headroom "lots"`},
		} {
			refused(t, f.call(t, f.member, "create_workspace", c.args), "invalid workspace: ")
			refused(t, f.call(t, f.member, "create_workspace", c.args), c.want)
		}
		refused(t, f.call(t, f.member, "get_workspace", map[string]any{"workspace": "bad"}), "not found")
	})
}

func TestWorkspaceTools(t *testing.T) {
	scheme := runtime.NewScheme()
	synced := syncedWorkspace(t)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{v1alpha1.WorkspaceResource: "WorkspaceList"}, synced)
	core := k8sfake.NewClientset(syncedObjects(t)...)
	f := newToolsFixture(t, kubeClients{dyn: dyn, core: core})

	workspaceToolsFlow(t, f)

	t.Run("get_workspace and list_workspaces after a sync", func(t *testing.T) {
		d := ok[api.WorkspaceDetail](t, f.call(t, f.member, "get_workspace", map[string]any{"workspace": "synced"}))
		w := d.Workspace
		require.NotNil(t, w.LastSync)
		assert.Equal(t, "NoChange", w.LastSync.Result)
		assert.Equal(t, &api.Volume{ClaimName: "workspace-synced", Driver: "efs.csi.aws.com", Handle: "fs-1::fsap-1"}, w.Volume)
		assert.Equal(t, 2, w.Repositories)
		require.Len(t, d.Mirrors, 2)
		assert.Equal(t, api.Mirror{Provider: "github", Repository: "acme/repo-002", DefaultBranch: "main", Head: "abc",
			PushedAt: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), SizeBytes: 3 << 20}, d.Mirrors[1])
		assert.Equal(t, api.Storage{MirrorsBytes: 4 << 20, SessionsBytes: 180 << 20, NeededSize: "6Gi", VolumeSize: "10Gi"}, d.Storage)
		require.Len(t, d.SessionDirectories, 1)
		assert.Equal(t, "fix-flaky-test", d.SessionDirectories[0].Session)
		require.NotNil(t, d.SessionDirectories[0].CleanupAfter)

		list := ok[api.WorkspacesResult](t, f.call(t, f.member, "list_workspaces", map[string]any{"organization": "acme"}))
		require.Len(t, list.Workspaces, 1)
		assert.Equal(t, "fs-1::fsap-1", list.Workspaces[0].Volume.Handle, "a Session's volume reference resolves to its workspace")
		assert.Equal(t, w.LastSync.Time, list.Workspaces[0].LastSync.Time)

		del := ok[api.DeleteResult](t, f.call(t, f.member, "delete_workspace", map[string]any{"workspace": "synced"}))
		assert.Equal(t, []string{"fix-flaky-test"}, del.Sessions)
	})
}

// TestListRepositories pages, searches and filters an owner of 500
// repositories from one cached listing.
func TestListRepositories(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{v1alpha1.WorkspaceResource: "WorkspaceList"})
	f := newToolsFixture(t, kubeClients{dyn: dyn, core: k8sfake.NewClientset()})
	list := func(args map[string]any) api.RepositoriesResult {
		args["organization"], args["provider"], args["owner"] = "acme", "github", "acme"
		return ok[api.RepositoriesResult](t, f.call(t, f.member, "list_repositories", args))
	}

	f.gh.PageSize = 100
	first := list(map[string]any{})
	assert.Equal(t, 500, first.Total)
	assert.Equal(t, 500, first.OwnerTotal)
	assert.Equal(t, api.DefaultPageSize, first.PageSize)
	require.Len(t, first.Repositories, api.DefaultPageSize)
	assert.Equal(t, "repo-000", first.Repositories[0].Name)
	assert.Equal(t, []string{"Go", "TypeScript"}, first.Languages)
	assert.Equal(t, []string{"mcp"}, first.Topics)
	listed := f.gh.Listed()
	assert.Equal(t, 5, listed, "one listing of 100 a page")

	third := list(map[string]any{"page": 3, "pageSize": 100})
	require.Len(t, third.Repositories, 100)
	assert.Equal(t, "repo-200", third.Repositories[0].Name)
	assert.Equal(t, "repo-299", third.Repositories[99].Name)
	assert.Empty(t, list(map[string]any{"page": 11}).Repositories, "past the last page")

	search := list(map[string]any{"search": "REPO-12"})
	assert.Equal(t, 10, search.Total)
	assert.Equal(t, 250, list(map[string]any{"languages": []string{"go"}}).Total)
	assert.Equal(t, 50, list(map[string]any{"topics": []string{"mcp"}}).Total)
	assert.Equal(t, 10, list(map[string]any{"archived": true}).Total)
	assert.Equal(t, 480, list(map[string]any{"archived": false, "fork": false}).Total)
	combined := list(map[string]any{"languages": []string{"Go"}, "topics": []string{"mcp"}, "archived": false, "fork": false, "search": "repo-0"})
	assert.Equal(t, []string{"repo-000", "repo-030", "repo-040", "repo-050", "repo-080", "repo-090"}, names(combined.Repositories))

	assert.Equal(t, listed, f.gh.Listed(), "every call after the first is served from the cache")

	refused(t, f.call(t, f.member, "list_repositories", map[string]any{"organization": "acme", "provider": "gitlab", "owner": "acme"}), `unknown provider instance "gitlab"`)
}

func workspaceNames(r api.WorkspacesResult) []string {
	out := make([]string, len(r.Workspaces))
	for i, w := range r.Workspaces {
		out[i] = w.Name
	}
	return out
}

func names(rs []api.Repository) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

// syncedWorkspace is acme's workspace after a sync: its manifest, its bound
// CSI volume and a Session directory.
func syncedWorkspace(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	need, size, dir := resource.MustParse("6Gi"), resource.MustParse("10Gi"), resource.NewQuantity(180<<20, resource.BinarySI)
	cleanup := metav1.NewTime(time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC))
	ws := &v1alpha1.Workspace{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Workspace"},
		ObjectMeta: metav1.ObjectMeta{Name: "synced", Namespace: wsNamespace, UID: "synced-uid"},
		Spec: v1alpha1.WorkspaceSpec{Organization: "acme", Sync: v1alpha1.Sync{Schedule: v1alpha1.ScheduleNightly},
			Sources: []v1alpha1.Source{{Provider: "github", Owner: "acme", Repositories: []string{"repo-001", "repo-002"}}}},
		Status: v1alpha1.WorkspaceStatus{
			LastSync:     &v1alpha1.LastSync{Time: metav1.NewTime(time.Date(2026, 10, 11, 3, 0, 0, 0, time.UTC)), Result: v1alpha1.SyncNoChange},
			Repositories: []string{"acme/repo-001", "acme/repo-002"},
			NeededSize:   &need, VolumeSize: &size,
			Volume:   &v1alpha1.VolumeRef{ClaimName: "workspace-synced"},
			Manifest: "workspace-synced-manifest",
			Sessions: []v1alpha1.SessionDirectory{{Session: "fix-flaky-test", Size: dir, CleanupAfter: &cleanup}},
		},
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ws)
	require.NoError(t, err)
	return &unstructured.Unstructured{Object: obj}
}

func syncedObjects(t *testing.T) []runtime.Object {
	t.Helper()
	pushed := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	m := mirror.Manifest{SyncedAt: pushed, Repositories: []mirror.Entry{
		{Provider: "github", Owner: "acme", Name: "repo-001", DefaultBranch: "main", Head: "def", PushedAt: pushed, SizeBytes: 1 << 20},
		{Provider: "github", Owner: "acme", Name: "repo-002", DefaultBranch: "main", Head: "abc", PushedAt: pushed, SizeBytes: 3 << 20},
	}}
	raw, err := m.JSON()
	require.NoError(t, err)
	return []runtime.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "workspace-synced-manifest", Namespace: wsNamespace},
			Data: map[string]string{mirror.ManifestFile: string(raw)}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "workspace-synced", Namespace: wsNamespace},
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-synced"}},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-synced"},
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "efs.csi.aws.com", VolumeHandle: "fs-1::fsap-1"}}}},
	}
}
