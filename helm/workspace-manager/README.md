# workspace-manager

Manages the Agent Platform's workspaces, the sources agent sessions work on and their provider sign-ins, as one MCP server behind muster

**Homepage:** <https://github.com/giantswarm/workspace-manager>

## Source Code

* <https://github.com/giantswarm/workspace-manager>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| replicaCount | int | `1` | Number of replicas. The server keeps no MCP session state (a session begun on one replica continues on another) and acts as the caller, so two or more are fine. Tokens issued by its own OAuth flow (a client that signs in to it directly) live in one replica's memory; muster's forwarded Dex tokens are validated by every replica. |
| revisionHistoryLimit | int | `3` | Number of old ReplicaSets retained for rollback. |
| image.registry | string | `"gsoci.azurecr.io"` | Image registry. |
| image.repository | string | `"giantswarm/workspace-manager"` | Image repository. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.tag | string | `""` | Image tag. Defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets. |
| nameOverride | string | `""` | Override the chart name. |
| fullnameOverride | string | `""` | Override the fully qualified release name. |
| global | object | `{}` |  |
| workspaces.namespace | string | `"kagent"` | Namespace the workspaces and everything they own live in: the namespace kagent runs its sessions in. The placeholder Role (`rbac.create`) is rendered here. |
| mcp.path | string | `"/mcp"` | MCP endpoint path. |
| oauth.enabled | bool | `false` | Make workspace-manager an OAuth 2.1 resource server (mcp-oauth): the MCP endpoint requires a bearer token Dex issued, and every call carries the caller's identity and Dex token, which every Kubernetes call of the request presents. muster forwards the session's Dex id_token (its MCPServer registration with `auth.forwardToken`, unpinned like every manager); it is validated against Dex's JWKS when its audience is in `trustedAudiences`. Off: no caller, so nothing that reaches Kubernetes works; only for a server nothing but a trusted proxy can reach. |
| oauth.baseURL | string | `""` | Public base URL of this server: the issuer of its own OAuth metadata (https, or http on loopback). Empty derives `https://<fullname>.<global.domain>` when `global.domain` is set. |
| oauth.dex.issuerURL | string | `""` | Dex issuer URL. Empty falls back to `global.identity.issuerUrl`. |
| oauth.dex.clientID | string | `""` | Dex OAuth client ID. Empty falls back to `global.identity.clientId`. |
| oauth.dex.clientSecret | string | `""` | Dex OAuth client secret (prefer `oauth.existingSecret`). |
| oauth.dex.allowPrivateURLs | bool | `false` | Let the issuer resolve to a private or loopback address (an in-cluster Dex). |
| oauth.dex.caSecret | object | `{"key":"ca.crt","name":""}` | Secret with the CA of a Dex that serves a private certificate; mounted and passed as `--dex-ca-file`. Empty name falls back to `global.identity.ca.secretName` / `global.identity.ca.key`. |
| oauth.existingSecret | string | `""` | Existing Secret with the Dex client secret under `dex-client-secret`. Empty falls back to `global.identity.existingSecret`; without that, the chart renders a Secret from `oauth.dex.clientSecret`. |
| oauth.trustedAudiences | list | `[]` | OAuth client IDs whose Dex id_tokens are accepted as bearer tokens: the platform client and the audiences muster requires at login (what the kube-apiserver trusts). Empty falls back to `[global.identity.clientId]`. |
| oauth.sso.allowPrivateIPs | bool | `false` | Let Dex's JWKS endpoint resolve to a private address when validating forwarded tokens (an in-cluster Dex). |
| oauth.allowPublicClientRegistration | bool | `false` | Accept unauthenticated dynamic client registration (labs only). |
| networkPolicy.enabled | bool | `false` | Create a Kubernetes NetworkPolicy for the pod (off when an umbrella chart renders the policies). |
| networkPolicy.ingressNamespaces | list | `[]` | Namespaces allowed to reach the MCP endpoint (label kubernetes.io/metadata.name). Empty allows ingress from the release namespace only. |
| networkPolicy.allowKubeAPI | bool | `true` | Allow egress to the Kubernetes API server. |
| networkPolicy.allowInternet | bool | `true` | Allow egress on 443 to every destination: Dex (discovery and JWKS). Vanilla NetworkPolicy cannot select by name; narrow with egressCIDRs instead when the addresses are known. |
| networkPolicy.egressCIDRs | list | `[]` | Extra egress CIDRs. |
| networkPolicy.metricsNamespaces | list | `["kube-system"]` | Namespaces allowed to scrape the metrics port (label kubernetes.io/metadata.name), when `observability.metrics.enabled`. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount. |
| serviceAccount.annotations | object | `{}` | Annotations on the ServiceAccount. |
| serviceAccount.name | string | `""` | ServiceAccount name (generated when empty). |
| rbac.create | bool | `true` | Create the ServiceAccount's Role and RoleBinding in `workspaces.namespace`. The Role carries no rules yet: requests act as the caller, and the server's own work brings its rules with it. |
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
