# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- Provider sign-ins: the MCP tools `list_providers`, `connect_provider` and `disconnect_provider`; OAuth 2.0 authorization code with PKCE per provider instance, its `state` sealed with the sign-in keys and binding the person, the instance and the verifier for ten minutes; the browser pages `/connect/<instance>`, `/callback/<instance>` (which stores the encrypted sign-in only for the person its state was made for, proven by a Dex sign-in at `/signin`) and the disconnect that revokes the grant at the provider (a GitHub App's grant, RFC 7009 elsewhere) and forgets it. The chart takes `signin.keys.secretName` and `signin.keys.current` and grants the ServiceAccount its sign-in Secrets and refresh Leases in the release namespace; Dex must list `<base URL>/signin` as a redirect URI of the manager's client.
- `sync`: the Job that keeps a workspace's repositories on its volume as bare mirrors under `mirrors/<owner>/<name>.git`: it clones added repositories, fetches only those pushed to since the manifest, marks the mirrors of repositories that left the selection and writes the manifest (head commits, push times, measured sizes) to the volume and, with `--result-configmap`, as the Job's result. While a session directory's clone borrows a mirror's objects, the sync's repack keeps every object (`gc.pruneExpire=never`, `--keep-unreachable`); the credential reaches git through the binary's own credential helper from a projected file, never through a URL, the volume or a log.
- The image carries git for the sync Job; the chart renders the Job's ServiceAccount and Role in the workspaces' namespace, allowed to write its result ConfigMap.
- The chart registers the server with muster through its own `MCPServer` (`muster.mcpServer.enabled`; its `name` sets the tool prefix `x_workspace-manager_<tool>`): `streamable-http` at the Service, the chart's labels plus `muster.giantswarm.io/type: workspace-manager` and `agent-platform.giantswarm.io/tool-group: agent-platform`, and with `oauth.enabled` the unpinned `auth` block (`forwardToken`, `requiredAudiences`) -- no authorization server and no GitHub App, since a provider sign-in is the workspace-manager's own business. The required audiences count as trusted bearer audiences next to `oauth.trustedAudiences`.



[Unreleased]: https://github.com/giantswarm/workspace-manager/tree/main
