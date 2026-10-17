# Developing on workspace-manager

- `make test`: the Go tests, among them the MCP `initialize` against a fake Dex
  (`internal/server/oauth_test.go`) and the sync against a local git server
  over HTTP that wants a token and records every request
  (`internal/mirror/sync_test.go`; needs `git` on the path). Every provider
  kind passes the contract suite of `internal/provider/providertest` against
  its stub API (`githubtest`, `gitlabtest`) or the test-only `fake` kind
  (`internal/provider/kinds/kinds_test.go`).
- `make test-envtest`: the tests behind the `envtest` build tag, against a
  real kube-apiserver and etcd that setup-envtest fetches (the `envtest` CI
  job): the chart's Workspace CRD, its CEL
  rules and the Workspace store (`internal/workspace/envtest_test.go`), the
  workspace tools against the CRD
  (`internal/provider/kinds/workspaces_envtest_test.go`) and the controller's
  sync cycle with the GitHub stub as the provider: the first sync, the
  schedule, `sync_workspace`, the no-op check, a push, a repository joining,
  the volume sized and grown; a Job's end is played by the test, since
  envtest runs no Job controller
  (`internal/provider/kinds/sync_envtest_test.go`).
- `make generate`: the API's deepcopy functions and the CRD in
  `helm/workspace-manager/files/crds` from `api/` (controller-gen);
  `make verify-generate` fails when they are stale (in the `envtest` job).
- `make lint`: golangci-lint, the version CI's pre-commit job installs
  (read from `.github/workflows/zz_generated.pre-commit.yaml`, downloaded to
  `.bin/`); `make pre-commit` runs every hook with that same binary.
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

Pinned to a provider instance's App as muster connects on the platform, each
MCP call carries the person's App token as the bearer and the Dex ID token in
`X-Muster-Id-Token`; add `--providers-config=<file> --muster-provider=<instance>`.

Run a sync against a directory standing in for a workspace's volume, with a
selection file and a credential directory `<provider>/<owner>` per provider
instance and owner (`username`
and `token`; the token is a GitHub App installation token or a fine-grained
personal access token with read access to the repositories):

```sh
mkdir -p /tmp/ws-creds/github/giantswarm && printf 'x-access-token' > /tmp/ws-creds/github/giantswarm/username
# write the token into /tmp/ws-creds/github/giantswarm/token without echoing it
cat > /tmp/ws-selection.json <<'EOF'
{"repositories": [{"provider": "github", "owner": "giantswarm", "name": "workspace-manager",
  "cloneUrl": "https://github.com/giantswarm/workspace-manager.git", "defaultBranch": "main",
  "pushedAt": "2026-10-09T12:00:00Z"}]}
EOF
go run . sync --volume /tmp/ws-volume --selection /tmp/ws-selection.json --credentials-dir /tmp/ws-creds
cat /tmp/ws-volume/manifest.json
```

A second run with the same `pushedAt` fetches nothing; `--result-configmap`
also writes the manifest into a ConfigMap of `--namespace` (the kube flags
of `serve`).

`helm/workspace-manager/values.schema.json` and the chart README are
generated (`pre-commit run --all-files`); CI files under `.circleci/` other
than `custom.yml` and the root `Makefile` come from devctl.
