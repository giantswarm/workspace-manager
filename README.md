# workspace-manager

Manages the Agent Platform's workspaces, the sources agent sessions work on,
and their provider sign-ins, as one MCP server behind muster.

muster registers it like every other manager, unpinned with `forwardToken`: it
forwards the person's Dex id_token, which workspace-manager validates as an
OAuth 2.1 resource server ([mcp-oauth](https://github.com/giantswarm/mcp-oauth)).
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

## Layout

- `cmd/`: the CLI (`serve`, `version`); every flag also reads an environment variable.
- `internal/server`: the HTTP listener, the OAuth resource server and the caller's identity on each request.
- `internal/identity`: the caller and the caller's Dex token on the request context.
- `internal/kube`: the manager's Kubernetes clients.
- `internal/api`: the MCP server and its tracing and metrics middleware.
- `internal/provider`: the provider contract (listing, sync credential, sign-in, run-time hosts), the selection and change rules every provider shares, and the provider instance list (`--providers-config`, the chart's `providers`).
- `internal/provider/<kind>`: one provider kind each (`github`; `fake` for tests only); `internal/provider/kinds` is the one list of kinds the binary serves and the only package that imports one. `internal/provider/providertest` is the contract suite every kind passes.
- `helm/workspace-manager`: the chart; see its [README](helm/workspace-manager/README.md).

See [docs/development.md](docs/development.md) for building and testing.
