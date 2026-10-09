# Developing on workspace-manager

- `make test`: the Go tests, among them the MCP `initialize` against a fake Dex
  (`internal/server/oauth_test.go`).
- `make lint`: golangci-lint.
- `make helm-test`: `helm lint` and the helm-unittest suites in
  `helm/workspace-manager/tests/` (the `chart-test` CI job).
- `make docker-build`: a local dev image (`TAG=workspace-manager:dev`).

Run it against the current kube context without OAuth:

```sh
go run . serve --listen=:8080
curl -s localhost:8080/healthz
```

With OAuth against a Dex (the agentlab's, for one):

```sh
DEX_CLIENT_SECRET=... go run . serve --enable-oauth \
  --oauth-base-url=http://localhost:8080 \
  --dex-issuer-url=https://dex.example.com --dex-client-id=agent-platform \
  --oauth-trusted-audiences=agent-platform
```

`helm/workspace-manager/values.schema.json` and the chart README are
generated (`pre-commit run --all-files`); CI files under `.circleci/` other
than `custom.yml` and the root `Makefile` come from devctl.
