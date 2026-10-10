# workspace-manager

Manages the Agent Platform's workspaces, the sources agent sessions work on and their provider sign-ins, as one MCP server behind muster

**Homepage:** <https://github.com/giantswarm/workspace-manager>

## Source Code

* <https://github.com/giantswarm/workspace-manager>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| replicaCount | int | `1` | Number of replicas. The server keeps no MCP session state (a session begun on one replica continues on another), so two or more are fine. Tokens issued by its own OAuth flow (a client that signs in to it directly) live in one replica's memory; muster's forwarded Dex tokens are validated by every replica. |
| revisionHistoryLimit | int | `3` | Number of old ReplicaSets retained for rollback. |
| image.registry | string | `"gsoci.azurecr.io"` | Image registry. |
| image.repository | string | `"giantswarm/workspace-manager"` | Image repository. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.tag | string | `""` | Image tag. Defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets. |
| nameOverride | string | `""` | Override the chart name. |
| fullnameOverride | string | `""` | Override the fully qualified release name. |
| global | object | `{}` |  |
| workspaces.namespace | string | `"kagent"` | Namespace the workspaces and everything they own live in: the namespace kagent runs its sessions in. The manager's Role (`rbac.create`) is rendered here. |
| crds.install | bool | `true` | Install the `Workspace` CRD (`workspaces.workspace-manager.giantswarm.io`). Kept on uninstall, so removing the chart never deletes a workspace. |
| organizations | object | `{}` | The Organizations whose members manage workspaces, each with its member groups: a caller whose forwarded identity carries any of an Organization's groups reads and writes its workspaces, nobody else does. An Organization not listed has no members. Members are bound to nothing in `workspaces.namespace`; the manager writes as its ServiceAccount. For example `{giantswarm: {memberGroups: [giantswarm-ad:developers]}}`. |
| providers | list | `[]` | The installation's workspace providers, several at once: each a unique `name` (a DNS label: the name workspaces and sign-ins refer to), a `kind` and the kind's `values`. A duplicate name, an unknown kind or invalid values fail the server's start with a message naming the instance. Kind `github` (github.com or a GitHub Enterprise Server) takes `url` (default `https://github.com`), `apiURL` (derived from `url`), `app.id`, `app.privateKey` and `oauth.clientID`, `oauth.clientSecret`; a Secret reference is `{name, key}` of a Secret in the release namespace, e.g. `{name: github, kind: github, values: {app: {id: "123", privateKey: {name: github-agents-app, key: private-key}}, oauth: {clientID: Iv1.abc, clientSecret: {name: github-agents-app, key: client-secret}}}}`. |
| signin.keys.secretName | string | `""` | Secret in the release namespace with the keys that seal each person's provider sign-ins and the sign-in flow's state: each data entry is a key id (a lower-case DNS label of at most 32 characters) and its 32 random bytes. Required with `providers` and `oauth.enabled`; the chart never renders it. |
| signin.keys.current | string | `""` | The id of the key that seals. The others still open what they sealed, so a key rotates by adding a new one, making it current and dropping the old one once every sign-in was written again. |
| tokenExchange.clientID | string | `""` | The installation's kagent client: the only client the token exchange `POST /token` (RFC 8693) answers, with the person's access token for a provider instance. Empty serves no token exchange. Needs `providers` and `oauth.enabled`; kagent's namespace belongs in `networkPolicy.ingressNamespaces`. |
| tokenExchange.clientSecret.name | string | `""` | Secret in the release namespace with the kagent client's secret, read on every exchange; the chart never renders it. |
| tokenExchange.clientSecret.key | string | `"client-secret"` | Key of the client secret in that Secret. |
| mcp.path | string | `"/mcp"` | MCP endpoint path. |
| oauth.enabled | bool | `false` | Make workspace-manager an OAuth 2.1 resource server (mcp-oauth): the MCP endpoint requires a bearer token Dex issued, and every call carries the caller's identity and Dex token, from which the caller's Organization is checked. muster forwards the session's Dex id_token (its MCPServer registration with `auth.forwardToken`, unpinned like every manager); it is validated against Dex's JWKS when its audience is in `trustedAudiences`. Off: no caller, so nothing that needs an Organization works; only for a server nothing but a trusted proxy can reach. |
| oauth.baseURL | string | `""` | Public base URL of this server: the issuer of its own OAuth metadata (https, or http on loopback). Empty derives `https://<fullname>.<global.domain>` when `global.domain` is set. |
| oauth.dex.issuerURL | string | `""` | Dex issuer URL. Empty falls back to `global.identity.issuerUrl`. |
| oauth.dex.clientID | string | `""` | Dex OAuth client ID. Empty falls back to `global.identity.clientId`. |
| oauth.dex.clientSecret | string | `""` | Dex OAuth client secret (prefer `oauth.existingSecret`). |
| oauth.dex.allowPrivateURLs | bool | `false` | Let the issuer resolve to a private or loopback address (an in-cluster Dex). |
| oauth.dex.caSecret | object | `{"key":"ca.crt","name":""}` | Secret with the CA of a Dex that serves a private certificate; mounted and passed as `--dex-ca-file`. Empty name falls back to `global.identity.ca.secretName` / `global.identity.ca.key`. |
| oauth.existingSecret | string | `""` | Existing Secret with the Dex client secret under `dex-client-secret`. Empty falls back to `global.identity.existingSecret`; without that, the chart renders a Secret from `oauth.dex.clientSecret`. |
| oauth.trustedAudiences | list | `[]` | OAuth client IDs whose Dex id_tokens are accepted as bearer tokens (SSO token forwarding). Empty falls back to `[global.identity.clientId]`, the platform client MCP clients and the muster CLI log in with. The server trusts the union of this list and `muster.mcpServer.auth.requiredAudiences` (in that order, without duplicates): every token muster forwards carries the required audiences by construction and they are what the kube-apiserver trusts, so a portal session — whose id_token carries them but not the platform client — is accepted without listing its client here. |
| oauth.sso.allowPrivateIPs | bool | `false` | Let Dex's JWKS endpoint resolve to a private address when validating forwarded tokens (an in-cluster Dex). |
| oauth.allowPublicClientRegistration | bool | `false` | Accept unauthenticated dynamic client registration (labs only). |
| muster.mcpServer.enabled | bool | `false` | Register this server with muster by rendering an `mcpservers.muster.giantswarm.io` CR in the release namespace. Tools then appear as `x_<name>_<tool>`. |
| muster.mcpServer.name | string | `"workspace-manager"` | MCPServer CR name (drives the tool prefix). |
| muster.mcpServer.autoStart | bool | `true` | Start the server connection when muster initializes. |
| muster.mcpServer.description | string | `"Workspaces of this installation, the sources agent sessions work on and their provider sign-ins, for the Agent Platform"` | Human-readable description shown by muster. |
| muster.mcpServer.labels | object | `{}` | Extra labels on the MCPServer CR. |
| muster.mcpServer.auth | object | `{"forwardToken":true,"requiredAudiences":[]}` | How muster authenticates to this server; rendered only with `oauth.enabled`. `forwardToken` makes muster forward the session's Dex id_token byte-identical (the SSO path this chart trusts through its trusted audiences). `requiredAudiences` are extra audiences that token must carry — the Dex cross-client audience the kube-apiserver trusts (`dex-k8s-authenticator` on Giant Swarm clusters; agentlab's is `kubernetes`); muster requests them at login, so users re-login after a change. They are trusted as bearer audiences by construction, since the server's trusted audiences are `oauth.trustedAudiences` plus this list. The registration is unpinned — no authorization server and no GitHub App: a provider sign-in is the workspace-manager's own business. |
| networkPolicy.enabled | bool | `false` | Create a Kubernetes NetworkPolicy for the pod (off when an umbrella chart renders the policies). |
| networkPolicy.ingressNamespaces | list | `[]` | Namespaces allowed to reach the MCP endpoint (label kubernetes.io/metadata.name). Empty allows ingress from the release namespace only. |
| networkPolicy.allowKubeAPI | bool | `true` | Allow egress to the Kubernetes API server. |
| networkPolicy.allowInternet | bool | `true` | Allow egress on 443 to every destination: Dex (discovery and JWKS). Vanilla NetworkPolicy cannot select by name; narrow with egressCIDRs instead when the addresses are known. |
| networkPolicy.egressCIDRs | list | `[]` | Extra egress CIDRs. |
| networkPolicy.metricsNamespaces | list | `["kube-system"]` | Namespaces allowed to scrape the metrics port (label kubernetes.io/metadata.name), when `observability.metrics.enabled`. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount. |
| serviceAccount.annotations | object | `{}` | Annotations on the ServiceAccount. |
| serviceAccount.name | string | `""` | ServiceAccount name (generated when empty). |
| rbac.create | bool | `true` | Create the ServiceAccount's Role and RoleBinding in `workspaces.namespace` (the manager writes the Workspaces), and the sync Job's own ServiceAccount and Role there (`<fullname>-sync`, allowed to write its result ConfigMap). No person or group is bound to anything. |
| podAnnotations | object | `{}` | Annotations on the pod. |
| podLabels | object | `{}` | Labels on the pod. |
| podSecurityContext | object | `{"fsGroup":1000,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Container security context. |
| service.type | string | `"ClusterIP"` | Service type. |
| service.port | int | `8080` | Service port (the container listens on 8080). |
| resources | object | `{"limits":{"cpu":"500m","ephemeral-storage":"100Mi","memory":"256Mi"},"requests":{"cpu":"50m","ephemeral-storage":"50Mi","memory":"64Mi"}}` | Container resources. |
| observability.otel.endpoint | string | `""` | OTLP collector URL for traces, e.g. `http://otlp-gateway.kube-system.svc:4317` (gRPC) or `https://tempo.example.com:4318` (with protocol `http/protobuf`). Empty exports nothing; inbound `traceparent` headers still propagate. With `networkPolicy.enabled` the policy opens egress to it: the Service's namespace for an in-cluster host, every address otherwise. |
| observability.otel.protocol | string | `"grpc"` | OTLP protocol: `grpc` or `http/protobuf`. |
| observability.otel.headers | string | `""` | OTLP headers (`OTEL_EXPORTER_OTLP_HEADERS`), e.g. `X-Scope-OrgID=giantswarm` for a multi-tenant collector. |
| observability.otel.resourceAttributes | string | `""` | Resource attributes appended to the downward-API `k8s.pod.name`, `k8s.namespace.name` and `k8s.node.name`. |
| observability.otel.sampler | string | `"parentbased_traceidratio"` | `OTEL_TRACES_SAMPLER`. The parent-based default follows the caller's sampling decision and samples `samplerArg` of the traces that start here. |
| observability.otel.samplerArg | string | `"0.1"` | `OTEL_TRACES_SAMPLER_ARG`. |
| observability.metrics.enabled | bool | `true` | Serve Prometheus metrics on `GET /metrics` of the `metrics` port: `mcp_server_operation_duration_seconds` per tool and `http_server_request_duration_seconds` for the MCP endpoint. Off, `OTEL_METRICS_EXPORTER=none`: nothing is served. |
| observability.metrics.port | int | `9464` | Port of the metrics listener, apart from the MCP port so the OAuth guard never covers it. |
| serviceMonitor.enabled | bool | `false` | Render a ServiceMonitor for the `metrics` port. Also needs `observability.metrics.enabled`. |
| serviceMonitor.interval | string | `""` | Scrape interval; empty uses the Prometheus Operator default. |
| serviceMonitor.labels | object | `{}` | Labels on the ServiceMonitor, beside the chart's own. The Giant Swarm observability platform routes a scrape to a Mimir tenant by `observability.giantswarm.io/tenant`; a monitor without it writes to no tenant. |
| logging.verbose | bool | `false` | Enable debug logging. |
| extraArgs | list | `[]` | Extra container arguments. |
| extraEnv | list | `[]` | Extra environment variables. |
| nodeSelector | object | `{}` | Node selector. |
| tolerations | list | `[]` | Tolerations. |
| affinity | object | `{}` | Affinity. |
