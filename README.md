# workspace-manager

Manages the Agent Platform's workspaces, the sources agent sessions work on,
and their provider sign-ins, as one MCP server behind muster.

The chart registers it with muster like every other manager, through its own
`MCPServer` (`muster.mcpServer.enabled`; the tools appear as
`x_workspace-manager_<tool>`), unpinned with `forwardToken`: muster forwards
the person's Dex id_token, which workspace-manager validates as an OAuth 2.1
resource server ([mcp-oauth](https://github.com/giantswarm/mcp-oauth)).
The caller's identity and Dex token travel with every request, and a request
without one is refused with 401. The manager checks the caller's Organization
from that identity and writes with its own ServiceAccount.

## Endpoints

| Path | Purpose |
|---|---|
| `/mcp` | MCP over streamable HTTP; behind the OAuth guard when `--enable-oauth` |
| `/healthz`, `/readyz` | Probes, always open |
| `/.well-known/oauth-*`, `/oauth/*` | OAuth metadata and this server's own OAuth flow, with `--enable-oauth` |
| `:9464/metrics` | Prometheus metrics (`OTEL_METRICS_EXPORTER=prometheus`) |

Traces go to the OTLP collector named by `OTEL_EXPORTER_OTLP_ENDPOINT`.

## The workspace's volume and the sync Job

Every workspace has one read-write-many volume, shared by the sync and every
session on the workspace:

| Path | Written by | Holds |
|---|---|---|
| `mirrors/<owner>/<name>.git` | the sync | a bare mirror of each selected repository (every branch and tag) |
| `sessions/<session>/` | that session | the session's clones, borrowing their objects from the mirrors (git alternates), and its work |
| `manifest.json` | the sync | what the volume holds: each mirror's head commit, last push time and measured size |

`workspace-manager sync` is the Job the controller starts per cycle, with the
volume mounted at `--volume`:

- **Input:** the selection the controller resolved from the workspace's
  sources and filters, mounted from a ConfigMap as `--selection`: per
  repository its `provider` instance, `owner`, `name`, `cloneUrl` (http(s),
  without a credential), `defaultBranch` and `pushedAt` as the provider listed
  them.
- **Credential:** one directory per provider instance under
  `--credentials-dir`, projected from a Secret, with the files `username` and
  `token` (on GitHub `x-access-token` and the `-agents` App's installation
  token). git obtains it through the binary's own credential helper
  (`workspace-manager git-credential <dir> get`), so no token reaches a URL,
  a mirror's configuration, the volume or a log; a provider without a
  directory is fetched anonymously.
- **Work:** a repository without a mirror is cloned (`git clone --mirror`
  into a temporary directory, renamed into place when complete); one whose
  `pushedAt` is newer than the manifest's is fetched (`--prune`, so deleted
  branches go); any other is not contacted. A repository that left the
  selection keeps its mirror, marked `droppedAt` in the manifest, until no
  session directory borrows from it; the cleanup removes it then.
- **Borrowed objects:** a session's clone borrows the mirror's objects, so
  a mirror never loses an object such a clone may need, even after a
  force-push or a deleted branch upstream. Each mirror is cloned with
  `gc.auto=0` and `gc.pruneExpire=never`; after a fetch the sync repacks
  it with `--keep-unreachable` while any session directory's alternates name
  the mirror (matched by the tail `mirrors/<owner>/<name>.git/objects`, so a
  session's own mount path of the volume does not matter), and prunes the
  unreachable objects only when none does.
- **Result:** the manifest is written to the volume in one rename and, with
  `--result-configmap`, into that ConfigMap of `--namespace` under
  `manifest.json`, the Job's result; the chart renders the Job's
  ServiceAccount `<fullname>-sync` in the workspaces' namespace, allowed to
  write it. The controller sizes a provisioned share from the measured sizes.
- **Fails closed:** a clone or fetch error fails the Job before the manifest
  is written, so the previous manifest stays; an interrupted clone leaves
  only a temporary directory, which the next sync removes.

## Layout

- `cmd/`: the CLI (`serve`, `sync`, `version`); every flag also reads an environment variable.
- `internal/server`: the HTTP listener, the OAuth resource server and the caller's identity on each request.
- `internal/identity`: the caller and the caller's Dex token on the request context.
- `internal/kube`: the manager's Kubernetes clients.
- `internal/api`: the MCP server and its tracing and metrics middleware.
- `internal/provider`: the provider contract (listing, sync credential, sign-in, run-time hosts), the selection and change rules every provider shares, and the provider instance list (`--providers-config`, the chart's `providers`).
- `internal/provider/<kind>`: one provider kind each (`github`; `fake` for tests only); `internal/provider/kinds` is the one list of kinds the binary serves and the only package that imports one. `internal/provider/providertest` is the contract suite every kind passes.
- `internal/mirror`: the sync: the volume's layout, the mirrors, the manifest, the credential helper and the borrowed-objects rule.
- `helm/workspace-manager`: the chart; see its [README](helm/workspace-manager/README.md).

See [docs/development.md](docs/development.md) for building and testing.
