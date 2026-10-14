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
| `/connect/<instance>`, `/callback/<instance>`, `/signin` | A person's provider sign-in in the browser, with providers and `--enable-oauth`; otherwise a 503 "No provider configured" page (below) |
| `POST /token` | Token exchange (RFC 8693) for the installation's kagent client, with providers, `--enable-oauth` and `--token-exchange-client-id` (below) |
| `GET /.well-known/jwks.json` | The workspace grant's verification keys, unauthenticated, with providers, `--enable-oauth` and `--grant-signing-key` (below) |
| `:9464/metrics` | Prometheus metrics (`OTEL_METRICS_EXPORTER=prometheus`) |

## Provider sign-ins

A person connects each provider instance their workspaces need once. The MCP
tools `list_providers` (every instance, its kind and whether the caller is
connected), `connect_provider` (the provider's authorization URL) and
`disconnect_provider` (revokes the grant at the provider and forgets the
sign-in) act for the caller's Dex subject.

The sign-in is OAuth 2.0 authorization code with PKCE, the instance's client
secret read by Secret reference. Its `state` is sealed with the sign-in keys
(`--signin-keys-secret`, `--signin-current-key`): it binds the person, the
instance and the PKCE verifier for ten minutes, and the verifier never leaves
the manager in clear. The provider returns to `/callback/<instance>` (on
GitHub the `-agents` App's callback URL), which stores the token only for the
person the state was made for: the browser proves who it is by signing in to
Dex at `/signin`, so a connect link one person hands another connects nobody.
Dex must list `<base URL>/signin` as a redirect URI of the manager's client.
`/connect/<instance>` starts the same flow for a person in the browser: the
link other surfaces show.

The sign-in routes (`/connect/<instance>`, `/callback/<instance>`, `/signin`)
are served only with at least one configured provider instance and
`--enable-oauth`. Without them each answers `503 Service Unavailable` with a
short "No provider configured" page naming the missing configuration, never a
404.

## Token exchange

On every turn of a Session with a workspace, kagent exchanges the turn
caller's Dex token for the person's access token at a provider instance:
`POST /token`, OAuth 2.0 Token Exchange (RFC 8693). Only the installation's
kagent client is answered (`--token-exchange-client-id`, its secret by Secret
reference `--token-exchange-client-secret=<secret>/<key>`, read on every
exchange), authenticated with `client_secret_basic` or `client_secret_post`;
every other client gets `invalid_client`.

- `grant_type` `urn:ietf:params:oauth:grant-type:token-exchange`;
  `subject_token` the caller's Dex token, accepted on the MCP bearer's terms
  (`subject_token_type` `id_token`, `jwt` or `access_token`); `audience` one
  provider instance's name.
- The response is the stored access token, refreshed ahead of expiry:
  `issued_token_type` access token, `token_type` Bearer, `expires_in`; never a
  refresh token. While the stored token is valid beyond the refresh margin, no
  provider request is made.
- A person without a sign-in, or whose refresh was revoked, gets
  `invalid_target` with `error_uri` the instance's `/connect/<instance>`; an
  unknown audience gets `invalid_target` without one; an invalid subject
  token gets `invalid_request`.
- Every exchange is logged (person, instance, client, outcome) and counted
  (`workspace_manager.token_exchange.requests` by provider instance and
  outcome), never with a token.

Traces go to the OTLP collector named by `OTEL_EXPORTER_OTLP_ENDPOINT`.

## Access at a Session's start: `authorize_workspace` and the grant

Before a Session starts on a workspace, the surface calls the MCP tool
`authorize_workspace(workspace)` as the person. The manager reads the last
sync's manifest (the ConfigMap `status.manifest` names) and, with the person's
own sign-in to each provider of the workspace, checks per owner which of its
repositories the person reads, with the person's user-to-server token on
GitHub: up to 100 repositories of an owner each asked for on its own
(`GET /repos/<owner>/<name>`, eight at a time), more in one listing of the
owner's repositories, its pages fetched eight at a time; on GitLab, with the
person's OAuth token, likewise: up to 100 projects of a group or user each
asked for on its own (`GET /projects/<path>`), more in one listing of the
owner's projects as the person sees them. The whole check is
bounded by `--access-check-timeout` (20 seconds, within muster's default tool
timeout of 30, which the chart's `muster.mcpServer.timeout` raises); a check
past it is `refused`, saying so. The answer's `outcome`:

- `granted`: the person reads every repository. `grant` is a JWT signed by
  the manager, valid for `--grant-ttl` (5 minutes), to be passed to
  agent-manager's `start_session`, whose Session kagent admits only against
  this manager's keys. `changed` names the repositories pushed to since the
  sync, which the Session fetches before its first turn. When the last sync
  is older than the refresh threshold (`--sync-refresh-after`, the chart's
  `sync.refreshAfter`, 1h), the check also requests a sync, as
  `sync_workspace` does, and `syncRequested` says so; the Session does not
  wait for it. The requested cycle lists every owner anew, never from the
  listing cache, so a threshold below the cache's minute still reads the
  push. None is requested while a sync runs or one is requested
  already; a last cycle that only cleaned up session directories listed no
  provider, so it counts as no refresh. The mirrors are refreshed at Session
  start, never on a schedule.
- `refused`: `unreadable` names the repositories the person cannot read; or
  `reason` says why the workspace cannot be started on yet: it has not
  synced, its volume is not bound, or it is being deleted; or that a
  provider did not answer within the check's bound.
- `connect_required`: `connect` lists each provider the person has not
  connected, with its `/connect/<instance>` link.

The grant's claims are kagent's JWT admission's: `iss` the manager's base URL,
`sub` the person's email, `aud` `--grant-audience` (`kagent`), `exp`,
`nbf`, `iat`, `jti`; `workspace` {namespace, name}; `volume` {driver, handle}
of the workspace's CSI volume; `mounts` `sessions/${SESSION_ID}` read-write
and `mirrors` read-only; `changed` [`<owner>/<name>`]; and `providers`, one per
git and API host of each provider of the workspace {hostname, pathPrefix
(only on an API under a path of a git host, as GitLab's `/api/`), audience (the
instance), scheme, username, broker (`--grant-broker`, the name kagent knows
the token endpoint by)}. It carries no token.

The grant names the person by **email**, not by the Dex subject the sign-ins
are keyed by: kagent's admission compares `sub` with the Session's creator as
kagent's authenticator identifies the caller, which on the Agent Platform is
the `email` claim (the edge sets `x-user-id` from it, the controller's user-id
claim is `email`), so a grant naming the subject admits nobody. A caller whose
identity carries no email gets an error instead of a grant.

The signing key is `--grant-signing-key=<secret>/<key>`, a PEM private key (EC
P-256 or P-384, or RSA of at least 2048 bits) in the manager's namespace, read
at start. Every other entry of that Secret holding such a key is published
beside it at `/.well-known/jwks.json`, so a key rotates without a gap: add the
next key to the Secret, point the reference at it (a verifier fetches the
keys again for a key id it does not know), and remove the old one after its
last grant expired. The Secret is read again every 30 seconds.

## The Workspace API

A `Workspace` (`workspace-manager.giantswarm.io/v1alpha1`, namespaced, short
name `ws`) lives in the workspaces' namespace (`--namespace`, `kagent`) and
belongs to an Organization:

```yaml
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: platform, namespace: kagent}
spec:
  organization: acme          # immutable
  sources:                    # one owner on one provider instance each
  - provider: github          # a configured provider instance
    owner: acme
    repositories: [api, web]  # by name; counts even when archived or a fork
  - provider: github
    owner: acme-labs
    filters:
      languages: [Go]                         # primary language, any of
      topics: {match: all, names: [agent]}    # any (default) or all
    exclude: [legacy]
    includeArchived: false    # the default
    includeForks: false       # the default
  sizing: {headroom: 8Gi, minimum: 32Gi}   # optional, no cap
```

The status carries the conditions, the last sync's time and result, the
resolved repositories, the needed and the provisioned size, the volume (claim,
CSI driver and handle), the manifest's ConfigMap, the session directories
(Session id, name, creator and last turn, size, cleanup date) and the
observed generation.

**Validation.** The CRD's schema and CEL rules refuse what needs no context:
a source without a selector (no repository name, language or topic), a
negative headroom or minimum, and a change of Organization. A Workspace
written with the removed `spec.sync` schedule is accepted with the field
dropped (the API server prunes it), and so is a tool call still passing
`sync`. The manager refuses a Workspace naming a provider instance the
installation does not configure, naming the instance.

**Organization membership.** Every Workspace lives in one namespace, where no
person or group is bound to anything; the chart binds only the manager's
ServiceAccount, which writes the Workspaces (and their volume and Jobs). The
manager checks membership on every read and write (`internal/workspace`): the
caller's forwarded identity must carry one of the Organization's member
groups, configured per Organization (`--organization
<organization>=<group>[,<group>...]`, the chart's `organizations`). A listing
shows only the caller's Organizations' Workspaces; an Organization not
configured has no members.

## The workspace tools

Every tool acts for the caller muster forwards. A Workspace read or write first
passes the Organization check on that identity; the manager's ServiceAccount
then writes the object. A refusal is the tool's answer (`isError`), never a
server error: `permission denied` for a non-member or a call without a
caller, `invalid workspace` with the API server's or the manager's reason
(nothing is written), `not found`, `already exists`, and a stale
`resourceVersion`'s "changed since it was read".

| Tool | Input | Result |
|---|---|---|
| `list_workspaces` | `organization` (optional, a member's) | `workspaces`: each workspace's view (below); `organizations`: every Organization the caller is a member of, sorted, with or without a workspace and whatever the filter, where the caller may create one |
| `get_workspace` | `workspace` | `workspace` (the view); `mirrors`: the last sync's repositories (`provider`, `repository`, `defaultBranch`, `head`, `pushedAt`, measured `sizeBytes`, `droppedAt`); `storage` (`mirrorsBytes`, `sessionsBytes`, `neededSize`, `volumeSize`); `sessionDirectories` (`session`, `name`, `creator`, `lastTurn`, `unknown`, `sizeBytes`, `cleanupAfter`) |
| `create_workspace` | `name`, `organization`, `sources`, `sizing` (`headroom`, `minimum` as quantities) | the view |
| `update_workspace` | `name`, `sources` (replaced), `sizing` (replaced when given, kept otherwise; `sizing: {}` clears it), `resourceVersion` (optional: refused when the workspace changed since) | the view |
| `delete_workspace` | `workspace` | `workspace`, `sessions`: the Sessions with a directory on the volume; `alreadyDeleting` when the workspace was being deleted, which runs its deletion cycle now |
| `sync_workspace` | `workspace` | `workspace`, `requestedAt`, `requestedBy`; `activeSync` when a sync Job runs at the request, after which the requested cycle starts; `deleting` on a workspace being deleted, whose deletion cycle the request runs |
| `list_repositories` | `organization` (a member's), `provider`, `owner`, `search` (name, ignoring case), `languages` (any of), `topics` (any of), `archived` and `fork` (`true` only those, `false` none, unset both), `page`, `pageSize` (50, at most 200) | `total` matching, `ownerTotal`, `page`, `pageSize`, `repositories` (`name`, `language`, `topics`, `archived`, `fork`, `lastPush`, `sizeKiB`), and every `languages` and `topics` value of the owner for the picker's filters |
| `preview_workspace` | `workspace` (an existing one), or a draft's `organization`, `sources` and `sizing`; `search` (`owner/name`), `page`, `pageSize` | `total` selected now, `excluded`, `skippedArchived` and `skippedForks` (what a selector matched and those rules left out), `providerSizeKiB`, `estimatedVolumeSize`, `matching` the search, and the page's `repositories` (`provider`, `repository`, `language`, `topics`, `lastPush`, `sizeKiB`, `why`: `name`, `language <l>`, `topic <t>`) |

The view of a workspace: `name`, `organization`, `sources`,
`sizing`, `resourceVersion`, `createdAt`, `deleting`, `lastSync` (`time`,
`result`, `trigger`, `job`, `message`), `activeSync` (the sync
Job running now: `job`, `startedAt`, `trigger`), `syncRequest` (a sync
request, from `sync_workspace` or `authorize_workspace`, the controller has
not taken up yet: `requestedAt`, `requestedBy`), the number of `repositories` and `sessions`, `volume`
(`claimName`, and the CSI `driver` and `handle` read from the bound
PersistentVolume, which a Session's volume reference names; else `pending`
says why), `neededSize`, `volumeSize`, `storedBytes` (what the volume stores,
measured at the last sync: the mirrors and the Session directories that
`get_workspace`'s `storage` splits; absent before the first sync), `grewFrom`
(the provisioned share's size before the last sync grew it; empty when it did
not) and the `conditions`. `list_workspaces` carries both from the status, so a
list needs no `get_workspace` per row.

`sync_workspace` asks for a cycle now: it records the
request and the caller as annotations on the Workspace
(`workspace-manager.giantswarm.io/sync-requested`, `-by`), which the
controller serves and removes. A request made while a sync Job runs is served
after it; nothing changed since the last sync costs no fetch. On a workspace
being deleted the request runs its deletion cycle now (below).

`list_repositories` and `preview_workspace` read the owner's listing with the
provider instance's own sync credential (on GitHub the `-agents` App's
installation token), from the listing cache: an owner of hundreds of
repositories costs one paginated listing a minute, however many pages, searches
and filters the picker asks for. They write nothing and say nothing about the
caller's own access to a repository, which `authorize_workspace` checks at a
Session's start. The preview selects as the sync does: named repositories
count even when archived or a fork, the filters' matches unless archived or
forks (unless included), exclusions over everything; its
`estimatedVolumeSize` is the size a new volume is claimed with for the
providers' reported sizes (the sizing rule below, nothing mirrored yet),
where the StorageClass provisions one.

`delete_workspace` deletes the Workspace: `authorize_workspace` refuses it
from then on, so no new Session starts on it, and its volume claim goes once
its deletion cycles leave no session directory (below). `delete_workspace`
on a workspace already being deleted records a sync request, as
`sync_workspace` does: its deletion cycle runs now.

## The sync cycle

The controller (`internal/controller`, part of `serve`) watches the
Workspaces of the workspaces' namespace and runs each one's sync cycle. One
replica runs it, elected through the Lease `workspace-manager-controller` in
that namespace (`--leader-election`, on by default; the replica's name is
`--leader-election-identity`, the chart's `POD_NAME`); a replica that loses
the lease exits and competes again. A cycle starts, in this order of
precedence, for a `sync_workspace` request (trigger `request`), a changed
spec or a first sync (`change`), a volume claimed anew (`change`: every
repository is cloned again), a retry of a failed cycle once `--sync-cycle`
(the chart's `sync.cycle`, 10m) has passed (`retry`). There is no schedule:
a Session starting on a workspace whose last sync is older than the refresh
threshold requests one (`authorize_workspace`, above), and nothing else
wakes a cycle but the session directories' cleanup (below). Every Workspace
is reconciled every `--sync-cycle` besides, which only re-makes a claim
deleted out from under it. A last sync recorded before the schedule was
removed may still carry the trigger `schedule`.

A cycle lists each source's owner once with the provider instance's own sync
credential (one listing per owner within a minute, shared with the pickers;
a requested cycle, from `sync_workspace` or a Session's start, lists anew),
compares the selection with the last sync's manifest, and sizes and claims
or grows the volume (below). **The no-op check:** a selection equal to the
manifest's live entries, none pushed to since and no default branch moved,
is recorded as `NoChange` and starts no Job: the listing is the cycle's whole
cost, and no mirror's origin is contacted. When the volume holds session
directories or a dropped mirror, the cycle still starts the Job, which
fetches nothing (`activeSync.cleanupOnly`) and only cleans up (below): its
result is `NoChange` when the Job removed nothing, else `Succeeded` with what
it removed in `lastSync.message` and the `Synced` condition (`removed 3
session directories, dropped 1 mirror`), which the Job writes beside its
manifest. Otherwise the controller writes
the selection (the ConfigMap `workspace-<name>-selection`: per repository
its `pushedAt` as the provider lists it, which the sync compares with the
manifest's to fetch only what moved), the credentials (the Secret
`workspace-<name>-sync-credentials`, server-side applied, one entry per
provider and owner, emptied once the Job ended; the manager never reads a
Secret of the namespace) and starts the Job `workspace-<name>-sync-<id>`
from the chart's template (the ConfigMap `<fullname>-sync-job`, passed as
`--sync-job-template`: this image as the Job's own ServiceAccount
`<fullname>-sync`, the pod and container security contexts, resources,
`activeDeadlineSeconds` and `ttlSecondsAfterFinished`, the chart's
`sync.job`); everything is owned by the Workspace and goes with it. The
Job's end wakes the controller: its manifest (the ConfigMap
`workspace-<name>-manifest`, the Job's result) is the cycle's outcome, so a
Job gone before its end was seen (its TTL, a restart) still counts by its
manifest; a failed Job leaves the previous manifest and its error beside it,
which the status carries.

The status records it all, and nothing else writes it: `lastSync` (`time`,
`result` `Succeeded`, `NoChange` or `Failed`, `trigger`, `job`, `message`),
`activeSync` while a Job runs, `repositories`, `neededSize`,
`volumeSize`, `storedSize`, `grewFrom`, and the conditions `VolumeClaimed` and `Synced` (`True` with
reason `Synced` or `NoChange`; `Unknown` with `Syncing`; `False` with
`ListingFailed`, `SyncFailed`, `SizeCeiling` or `VolumeMissing`). A cycle
that failed before its Job (the listing, the sizing, the claim) is tried
again after `--sync-cycle`; a request or a change starts one sooner.

## Session directories: cleanup and inventory

Every Session on a workspace works in its own directory, `sessions/<session
id>/`, on the workspace's volume. The sync Job cleans them up, from kagent's
record of each Session's last turn:

- **kagent's Sessions.** Each cycle, the controller lists every creator's
  Sessions from kagent's API (`SessionService/ListSessions` with
  `all_creators`, walked to the end; `--kagent-address`, the chart's
  `kagent.address`), as its own ServiceAccount with a projected token for
  kagent's audience (`--kagent-token-file`), keeps those whose workspace
  volume is this workspace's (by CSI driver and handle) and hands them, with
  the window `--sessions-cleanup-after` (the chart's
  `sessions.cleanupAfter`, 720h), to the Job in its selection.
- **The window.** A directory is deleted once its Session's last turn
  (`last_turn_time`; its creation before the first turn) is older than the
  window; every turn moves the date. A Session kagent listed at the previous
  sync and no longer lists was deleted: its directory goes at once. A
  directory whose Session kagent does not know goes the window after the
  sync that first found it. A directory is renamed aside before it is
  removed, so no half-removed tree is ever seen as a Session's.
- **A deleted Session.** Between cycles, every `--sessions-poll` (the
  chart's `sessions.poll`, 30s) the controller lists kagent's Sessions once
  for every workspace; a live workspace whose status lists a directory of a
  Session kagent no longer lists gets a cleanup-only Job at once (trigger
  `sessionDeleted`, the result `Succeeded` with what it removed, `NoChange`
  when nothing): one Job for all its deleted
  Sessions, which fetches no mirror and deletes those directories and a
  dropped mirror they were the last to borrow from. Every sync cycle still
  cleans up everything else, and catches a directory this missed (kagent
  unreachable, the Job failed, the manager restarted).
- **The cleanup date.** A live workspace's directory goes at its own
  cleanup date (`status.sessions[].cleanupAfter`), never waiting for a
  sync: the controller wakes at the earliest date and starts a cleanup-only
  Job (trigger `cleanup`, the result `Succeeded` with what it removed,
  `NoChange` when nothing), which lists kagent's
  Sessions anew, fetches no mirror and deletes the directories past their
  window. A directory that Job leaves behind (its Session took a turn,
  which moves the date) is not looked after again before `--sync-cycle`.
- **Fails safe.** Without kagent, while kagent's listing fails (an
  incomplete listing is a failure) or while the volume's CSI handle is not
  known, no directory is deleted; the Job only measures them. The condition
  `SessionsCleanedUp` says which (`Listed`, `KagentNotConfigured`,
  `KagentUnavailable`, `VolumePending`).
- **Another uid's directory.** The runtime creates a Session's directory as
  the Session's user (Substrate: owned by its uid and gid, mode `0770`). A
  volume that keeps every creator's uid and mode (Azure Files over NFS, an
  NFS server) leaves it to that user alone; Amazon EFS's access point maps
  every uid to its own. The sync, which cannot enter it, keeps it with the
  size measured before and records its owner in the manifest (`owners`);
  the next cycle's Job runs a container of each owner's (`owner-<uid>-<gid>`,
  the sync container's image, resources and security context with the
  owner's uid and gid, the pod's `fsGroup` a supplementary group, the volume
  and the selection, no credential and no Kubernetes access) before the
  sync container: `sync --owner-report` measures and deletes that owner's
  directories alone and reports to the sync, which takes the report into
  the manifest. No container needs a capability or root: `sessions/` is
  the sync's, mode `2775`, so the fsGroup lets an owner's container rename
  its directory aside. A directory owned by root, or one the sync or its
  owner can neither measure nor delete, stays with the reason
  (`status.sessions[].error`, `SessionsCleanedUp` with `DirectoriesLeft`)
  and never fails the cycle; it waits for the next cycle, on a deleted
  workspace a day after the last.
- **Dropped mirrors.** After the cleanup, a mirror the sync marked dropped
  is deleted once no session directory borrows from it (its clones'
  `objects/info/alternates`); one still borrowed stays, the manifest naming
  its borrowers (`borrowedBy`). A directory the sync cannot read counts as
  a borrower of every mirror.
- **Inventory.** The manifest records each directory left (`sessions`: its
  Session's id, name, creator and last turn, the size measured by this sync,
  the cleanup date), and the status lists them (`status.sessions`), which
  `get_workspace` shows and the volume's sizing counts.
- **A deleted workspace.** The Workspace carries the finalizer
  `workspace-manager.giantswarm.io/volume`. Once deleted, its sync stops and
  no Session starts on it; its cycles (trigger `deletion`) only clean up the
  session directories — a directory whose Session kagent's listing does
  not name goes in the first one, a listed Session's after its window — at
  the earliest cleanup date or a day after the last cycle, whichever comes
  first, or at once for a `sync_workspace` request or a
  second `delete_workspace` (after a running Job): a directory cleared
  outside a cycle is found gone without waiting, while one still there is
  kept as by any cycle. When a cycle since the deletion leaves no
  directory, the controller deletes the volume's claim and removes the
  finalizer, and everything else the workspace owns goes with it.

## The workspace's volume and the sync Job

Every workspace has one read-write-many volume, shared by the sync and every
session on the workspace. The controller claims it as `workspace-<name>` in
the workspaces' namespace from the installation's StorageClass
(`--storage-class`, the chart's `storage.storageClassName`): access mode
`ReadWriteMany`, owned by the Workspace so it goes with it. The class must
keep POSIX semantics (file modes, symbolic links) for git, which a cluster's
default class rarely does, so there is no fall-back to it: without a class
nothing is claimed and the Workspace's `VolumeClaimed` condition is `False`
with reason `StorageClassMissing`, naming the flag and the chart value. The
status names the claim (`status.volume.claimName`); the condition is `True`
with reason `Claimed` once it exists.

**Sizing.** A class that provisions a size and allows expansion
(`allowVolumeExpansion`: Azure Files, an NFS server on block storage) is
sized by the rule below and grown; a class that does not (EFS, an NFS
export) is elastic: the claim requests a nominal 1Gi and nothing is sized.
The flags are the chart's `storage.sizing`.

```
need     = Σ measured size of the mirrors the manifest records
         + Σ factor × reported size of each selected repository not yet mirrored   (--sizing-factor, 1.5)
         + Σ size of the live session directories
headroom = max(--sizing-headroom (4Gi), --sizing-headroom-share (0.5) × need, spec.sizing.headroom)
size     = max(need + headroom, spec.sizing.minimum), rounded up to the GiB
```

A new claim requests `size`; before a Job that adds repositories the claim
is grown to it, so the clones have room, and after a Job it is grown to what
the measured mirrors need; a claim is never shrunk, since the volume holds
live session directories. `--sizing-max-size` (empty by default) is the
installation's ceiling: a cycle that would pass it fails with reason
`SizeCeiling`, naming the size it needs, and keeps the mirrors it has; a
Workspace's minimum never passes it. `preview_workspace` estimates a new
volume's size the same way from the providers' reported sizes.

| Path | Written by | Holds |
|---|---|---|
| `mirrors/<owner>/<name>.git` | the sync | a bare mirror of each selected repository (every branch and tag) |
| `sessions/` | the sync | the session directories, mode `2775` for the owners' containers; the runtime refuses a Session's mount while it is missing |
| `sessions/<session>/` | that session | the session's clones, borrowing their objects from the mirrors (git alternates), and its work |
| `manifest.json` | the sync | what the volume holds: each mirror's head commit, last push time and measured size |

`workspace-manager sync` is the Job the controller starts per cycle, with the
volume mounted at `--volume`:

- **Input:** the selection the controller resolved from the workspace's
  sources and filters, mounted from a ConfigMap as `--selection`: per
  repository its `provider` instance, `owner`, `name`, `cloneUrl` (http(s),
  without a credential), `defaultBranch` and `pushedAt` as the provider listed
  them.
- **Credential:** one directory `<provider>/<owner>` per provider instance
  and owner under `--credentials-dir`, projected from a Secret, with the
  files `username` and
  `token` (on GitHub `x-access-token` and the `-agents` App's installation
  token). git obtains it through the binary's own credential helper
  (`workspace-manager git-credential <dir> get`), so no token reaches a URL,
  a mirror's configuration, the volume or a log; an owner without a
  directory is fetched anonymously.
- **Work:** a repository without a mirror is cloned (`git clone --mirror`
  into a temporary directory, renamed into place when complete); one whose
  `pushedAt` is newer than the manifest's is fetched (`--prune`, so deleted
  branches go); any other is not contacted. A repository that left the
  selection keeps its mirror, marked `droppedAt` in the manifest, until no
  session directory borrows from it; the sync deletes it then.
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
  is written, so the previous manifest stays and the error goes beside it
  (the ConfigMap's `error` key, the Workspace's `Synced` message); an
  interrupted clone leaves only a temporary directory, which the next sync
  removes.

## Layout

- `cmd/`: the CLI (`serve`, `sync`, `version`); every flag also reads an environment variable.
- `internal/server`: the HTTP listener, the OAuth resource server and the caller's identity on each request; `routes.go` is the one place every route is registered.
- `internal/identity`: the caller and the caller's Dex token on the request context.
- `api/v1alpha1`: the Workspace API; `make generate` writes its deepcopy functions and the CRD the chart ships (`helm/workspace-manager/files/crds`).
- `internal/workspace`: the Organization check, the provider validation, the Workspace store every tool reads and writes through, the selection a workspace's sources resolve to from the providers' listings (with why each repository is in), the last sync's manifest, and the volume's CSI handle.
- `internal/controller`: the manager's controller, leader-elected among the replicas, watching the Workspaces of the workspaces' namespace and their sync Jobs: it runs each Workspace's sync cycle (the triggers, the no-op check, the selection and the credentials, the Job from the chart's template, the outcome from the manifest), sizes, claims and grows the volume from the installation's StorageClass, hands the Job kagent's Sessions for the session directories' cleanup, runs a deleted Workspace's cleanup to its last directory before its volume goes, and writes the Workspace's status, which nothing else writes.
- `internal/kagent`: kagent's Sessions for the cleanup (`SessionService/ListSessions` over every creator), from the stubs in `internal/kagent/gen` that `make generate-kagent` makes from the protos pinned in `hack/kagent-proto`; `internal/kagent/kagenttest` is a fake kagent for tests.
- `internal/kube`: the manager's Kubernetes clients.
- `internal/api`: the MCP server, its tools (`list_workspaces`, `get_workspace`, `create_workspace`, `update_workspace`, `delete_workspace`, `sync_workspace`, `list_repositories`, `preview_workspace`, `list_providers`, `connect_provider`, `disconnect_provider`, `authorize_workspace`) and its tracing and metrics middleware.
- `internal/authorize`: the access check at a Session's start: the manifest against what the person reads at each provider, the volume's CSI handle, and the grant.
- `internal/grant`: the workspace grant: its claims, the signing keys from their Secret and the JWKS.
- `internal/connect`: the person's connection to each provider instance: the OAuth 2.0 authorization code flow with PKCE (`golang.org/x/oauth2`), a sealed short-lived `state` binding the person, the instance and the verifier, the disconnect that revokes at the provider, and the browser pages `/connect/<instance>`, `/callback/<instance>` and `/signin`.
- `internal/provider`: the provider contract (listing, what a person reads, sync credential, sign-in, run-time hosts), the selection and change rules every provider shares, the listing cache (one listing per owner within a minute, shared by the workspaces resolved on the owner and by an editor's picker), and the provider instance list (`--providers-config`, the chart's `providers`).
- `internal/provider/<kind>`: one provider kind each (`github`; `fake` for tests only); `internal/provider/kinds` is the one list of kinds the binary serves and the only package that imports one. `internal/provider/providertest` is the contract suite every kind passes. The `github` kind lists a GitHub App installation's repositories (name, language, topics, archived, fork, last push, size in KiB) with the installation token it mints per owner and renews before expiry, and waits out a rate limit as GitHub asks (`Retry-After`, the primary limit's reset, else a minute).
- `internal/exchange`: the token exchange `POST /token` (RFC 8693) for the installation's kagent client.
- `internal/signin`: each person's provider sign-ins: one Secret per person and provider instance, sealed with AES-256-GCM under rotatable keys, named by a hash of the person; access tokens refreshed ahead of expiry, once across replicas (a Lease per sign-in), so a provider that rotates refresh tokens never sees one redeemed twice.
- `internal/mirror`: the sync: the volume's layout, the mirrors, the session directories' cleanup and inventory, the manifest, the credential helper and the borrowed-objects rule.
- `helm/workspace-manager`: the chart; see its [README](helm/workspace-manager/README.md).

See [docs/development.md](docs/development.md) for building and testing.
