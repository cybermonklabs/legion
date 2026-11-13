package api

// DaemonAPIVersion is the contract this daemon speaks with the Legion plugin (@sjawhar/pi-legion):
// the claim, credential, workflow, controller and state shapes the plugin's client parses
// (`packages/pi-shared/src/daemon-client.ts`, through
// `packages/contracts/src/legion-api.ts`), the pane environment it reads — every variable
// the tmux runtime sets on a pane (`internal/runtime/tmux/spawn.go`'s `panePairs`) and the Sandbox
// runtime on a pod's worker container (`internal/runtime/sandbox/manifest.go`'s
// `mainEnvironment`) — and the command line and environment of a pod's `workspace-init provision`
// container (`initContainers` in `internal/runtime/sandbox/podkind.go`, and `initEnvironment`),
// which the image's own `legion` reads. The installed plugin declares the
// number it was built against as `legion.daemonApiVersion` in its `package.json`, and the boot
// gate (`internal/daemon/bootgate.go`) refuses to start unless the two are equal.
//
// Bump rule: a change to any of these surfaces bumps this constant and the manifest field in the
// same commit; `packages/contracts/fixtures/daemon-api/version.json`, written by this package's
// golden test and read by the plugin's, holds the two together.
//
// 8: secrets broker enrollment -- AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR on an enrolled pod's worker
// container, and the shim's hello2.
//
// 9: secrets broker machine login -- the daemon's own agent-secrets machine login state
// (agentSecretsLogin) on GET /legion/v1/state.
//
// 10: LEGION-208 -- POST /legion/v1/roots/close, the Go legion tool's close_root.
//
// 11: LEGION-208 -- legionAppLogins on POST /legion/v1/gh-token, each Legion role App's login keyed by
// its App role.
//
// 12: LEGION-223 -- one daemon: the manifest field is renamed `legion.daemonApiVersion`, `legion
// probe-image` takes `--daemon-api-version` and its OK line ends `daemon-api-version=<N>`, and no
// pane or pod carries the variable that chose between two daemons' clients. `POST
// /legion/v1/controller/secret` takes the contract `legion controller start` held the controller's
// plugin to (`pluginContract`) and refuses another before it mints.
//
// 13: LEGION-583 -- the claim form of `POST /legion/v1/grants` takes an optional `push` bool,
// true only for a bash command the plugin judges to run `legion push`: such a grant lives
// `credential.pushTTL` (5 minutes) rather than the ordinary `ttl` (60 seconds), since jj's own
// working-copy snapshot before the network push can outrun the ordinary grant on a near-full tree
// volume. `readBody` refuses an unknown field, so a plugin built from this commit sending `push`
// against a daemon older than 13 fails every grant mint, blocking every bash command in every
// pane; the bump is why that never ships paired with an older daemon. 13 also brings
// `initEnvironment`'s `LEGION_REMOVABLE_WORKSPACES` (`runtime.RemovableWorkspacesPayload`) under
// this contract: `workspace-init` refuses a field it does not know and removes nothing, so a
// payload change bumps this number and the image probe pairs the daemon with an image that reads
// it, rather than removal stopping on every pod still running an older image.
//
// 14: LEGION-592 -- the daemon-launched controller (`controller: daemon`): a Sandbox pod's worker
// container may carry `LEGION_CONTROLLER=1` beside `LEGION_BOOT_TOKEN_FILE`, which the plugin must
// answer by registering on `POST /legion/v1/claims/register` with the launch's boot token and
// reporting ready on `claims/ready`. A plugin built before it reads that pod as the operator's
// controller, throws for want of `LEGION_CONTROLLER_SECRET`, and never registers, so the image probe
// must refuse such an image rather than leave the controller's keeper relaunching it forever.
//
// 15: LEGION-462 -- a Sandbox locator on GET /legion/v1/state addresses one role process in its
// issue's shared pod: the issue Sandbox's name, the pod's uid, the role container and the process
// generation, with the incarnation `<pod uid>/<generation>`.
//
// 16: LEGION-578 -- api.State gains `capabilities`, the deployment's capability report
// (capabilities.Deployment.Report): one row per capability of the table, each `present`,
// `installed`, `unchecked`, `live`, `withheld`, `decided` or `open`, an open row carrying the
// legion.yaml line that records a decision. Never null, so a plugin built before it refuses the
// state, and the bump is why the two never meet. (This branch first took 15; LEGION-462 landed at
// 15 first, and pi-legion 8.3.0 declares it without `capabilities`, so a daemon at 15 would pass
// the gate against a plugin whose strict reader refuses its state. Renumbered, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md says.)
//
// 17: LEGION-588 -- the role prompts the daemon embeds name the `dispatch` command an agent runs in
// its shell, which the Envoy plugin puts on the pane's PATH, rather than the `dispatch_*` tools it
// no longer registers: a daemon and a plugin from either side of that change would hand agents
// instructions for a surface they lack. (This branch first took 16; LEGION-578 landed at 16
// first, and pi-legion 8.4.0 declares it beside a pi-envoy 8.4.0 that still registers the native
// tools and puts no `dispatch` on the PATH, so a daemon at 16 would pass the gate against panes
// that lack the command its prompts name. Renumbered, as the same note says.)
//
// 18: LEGION-632 -- each issue's pod is independent, on a volume of its own, so the pod's
// `workspace-init provision` container no longer carries `LEGION_REMOVABLE_WORKSPACES` (the
// removable-workspaces pass is gone with the shared tree volume), nor
// `LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS` (no two provisions share a repository, so the flock
// and its wait are gone), nor the `LEGION_ROLE` and `LEGION_GENERATION` that seeded that pass's
// candidate rotation; and `LEGION_EXPECT_TREE_VOLUME` is now `LEGION_EXPECT_ISSUE_VOLUME`. An image
// built before 18 acts on a contract this daemon no longer speaks: it looks for the old name and so
// never learns the volume must already hold the clone, provisioning a lost volume afresh instead of
// exiting 3, and runs a lock and a removal pass against variables nothing sets; the image probe
// pairs the daemon with an image built from this contract instead. Each role's agent is told a
// state home of its own (`XDG_STATE_HOME=/home/legion/.local/state/<role>`), which the image's
// shim makes Oh My Pi's profile directory under, so the browser broker's lock is one name per
// container. (This branch first took 16, then 17; LEGION-578 landed at 16 and LEGION-588 at 17
// first, and pi-legion 8.4.1 and 8.5.0 declare those with the init container's old environment,
// so a daemon at either would pass the gate against an image whose `workspace-init` speaks the
// other contract. Renumbered twice, as the same learning says.)
//
// 19: LEGION-631 -- each role's GitHub App token is a file its plain `gh` and `git` read, never a
// grant the plugin redeems: the three credential routes, `POST /legion/v1/gh-token`,
// `POST /legion/v1/git-credential` and `POST /legion/v1/provisioning-credential`, and
// `GrantRequest.push` on `POST /legion/v1/grants` are deleted. The pane and pod environment gains
// `GH_CONFIG_DIR`, the role's directory of gh files (`hosts.yml` and `config.yml` rendered from
// its App token and rewritten as the lease turns over), with `GH_TOKEN`, `GITHUB_TOKEN` and
// `GH_HOST` set to the empty string so nothing in the environment outranks the file — all four
// runtime-set — and loses `LEGION_GH_PATH`, `LEGION_GIT_PATH`, `LEGION_JJ_PATH`,
// `LEGION_CREDENTIAL_HELPER` and `LEGION_GRANT_FILE`: a pane's gh, git and jj are its PATH's,
// every shared clone's helper is `gh auth git-credential`, and nothing in a pane runs `legion`
// from bash any more, since the `legion` tool mints its grants in-process for its own daemon calls
// (`handoff_complete`, `read_record`, the controller's `read_state` and `set_status`). No
// `worker-bin` directory leads PATH: nothing shims gh, and `PI_SHELL_PREFIX` puts the `legion`
// launcher directory first alone. `HandoffCompleteResponse` gains `note` (READY's three refusals
// run in the daemon on the pull request's head: `.legion/<issue>/` still carried, a conflict with
// the base, a required check not green or a required workflow without a passing run); the
// request's `commit` is the pushed commit carrying the phase's handoff, which the `legion` tool's
// `handoff_complete` finds in the pane as `legion handoff complete` did.
// `POST /legion/v1/threads/resolve` is deleted: the reviewer names the bot threads
// it accepted to the implementer, who resolves them with plain `gh` as the pull request's author. A
// plugin or image built before 19 would shim gh over a token file it never reads and mint a grant
// before every command, against a daemon on 19 that serves none of that, so the boot gate and
// `legion probe-image` refuse the mixed pair. (This branch first took 16, then 17, then 18;
// LEGION-578 landed at 16, LEGION-588 at 17 and LEGION-632 at 18 first, pi-legion 8.4.1, 8.6.0
// and 8.9.0 declaring them without the token file, so a daemon at any of them would pass the gate
// against them. Renumbered three times, as the collision note says.)
//
// 20: LEGION-668 -- the controller holds the review App's GitHub token like every role: `POST
// /legion/v1/controller/github-credential` is the route `legion controller start` fetches the
// controller's gh files from and refreshes them over, with the controller capability. The
// controller pod's agent environment gains `GH_CONFIG_DIR`, `GH_TOKEN`, `GITHUB_TOKEN` and
// `GH_HOST` exactly as an issue pod's role container has them, backed by a `gh-controller` volume
// of its role Secret. A `legion` built before 20 never fetches the credential, so its controller's
// gh acts as nobody against a daemon that expects it to, and a daemon before 20 serves no such
// route, so `legion controller start` from 20 is refused after the mint and cuts the incumbent
// controller off for nothing: the boot gate and `legion probe-image` refuse the mixed pair. (This
// branch first took 19; LEGION-631 landed at 19 first, pi-legion 8.10.1 declaring it without this
// route, so a daemon at 19 would pass the gate against it. Renumbered, as the collision note says.)
//
// 21: LEGION-663 -- every Legion session reports the six live capability rows with its ready, and
// the daemon renders them from those reports. `claim.RegisterResponse` gains `promptAgents`, the
// sorted names of the task agents the role's prompts dispatch, which the session's subagents check
// measures against; `claim.ReadyRequest` gains an optional `capabilities` report (`measuredAt`,
// `elapsedMs`, `rows[{name, ok, detail}]`), which the daemon normalises (capabilities.Normalize)
// and never refuses: a session runs and reports, whatever it reports. `api.ClaimView` gains
// `capabilities` (`measuredAt`, `incarnation`, `ok`, `open`), the claim's latest report as the
// state shows it, and `api.CapabilityState.status` loses `live`: a live row is `present`, `open` or
// `unchecked` from the sessions' reports, never a promise of a check to come. A 20 plugin's strict
// reader refuses a state whose claim views carry `capabilities`, and a 21 plugin's `capabilities`
// on `claims/ready` is an unknown field a 20 daemon's `readBody` refuses with 400, which exits the
// plugin, so the boot gate and `legion probe-image` refuse the mixed pair. (This branch first took
// 19, based on LEGION-631's branch while that branch declared 18; LEGION-631 landed at 19 first,
// pi-legion 8.11.0 declaring it without the report. Renumbered to 20 at the retarget to main; then
// LEGION-668 landed at 20 first, pi-legion 8.14.0 declaring it without the report, so a daemon at
// 20 would pass the gate against it. Renumbered twice, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// 22: LEGION-618 -- the daemon logs in to the secrets broker with its own Kubernetes ServiceAccount
// token, which no person approves, so GET /legion/v1/state's `agentSecretsLogin` loses `code` (no
// confirmation code exists any more) and its `state` is `none`, `issued` or `failed`. A 21 plugin's
// strict reader requires `code` and refuses a 22 daemon's state, so the boot gate and `legion
// probe-image` refuse the mixed pair.
//
// 23: LEGION-670 -- api.State gains `strandedClaims`, every claim nothing will wake
// (workflow.Stranded): a suspended or failed worker of its issue's phase, or tree architect, in a
// tree that runs, each named by its issue, role and state. Never null, so a plugin built before it
// refuses the state, and the bump is why the two never meet. (This branch first took 19, then 20,
// then 21, then 22; LEGION-631 landed at 19, LEGION-668 at 20, LEGION-663 at 21 and LEGION-618 at
// 22 first, so a daemon at any of them would pass the gate against a plugin whose strict reader
// lacks the field. Renumbered four times, as the collision note says.)
//
// 24: LEGION-164 -- pluginContract on the claim views (api.ClaimView, api.OperatorClaim) and the
// controller locator (api.ControllerLocator), always emitted, 0 meaning no contract recorded; the
// stale-caller refusals (409 naming both contracts). A 23 plugin's strict reader refuses a state
// whose views carry the field, so the boot gate and legion probe-image refuse the mixed pair.
//
// 25: LEGION-636 -- `source` on a deployment row of `api.State.capabilities`, "configuration"
// (what legion.yaml alone decides) or "boot" (what boot measured), always emitted there; absent on
// every other row. `installed` leaves the status set: no row has awaited a pod launch since
// LEGION-629 turned extension discovery on in the pod's launch, so no daemon has emitted it since,
// and the field that rendered it (capabilities.Capability.Awaits) is gone. The probe's OK line
// gains a third `model-fallback` mark, `unread`, when the probe could not read `retry.modelFallback`.
// A 24 plugin's strict reader refuses a state whose deployment rows carry `source`, so the boot
// gate and `legion probe-image` refuse the mixed pair.
//
// 26: reserved for sjawhar/legion#1999 (LEGION-694) by 27 below while that branch was open; no
// release declares it, since that branch landed at 28 once releases had declared 27 without its
// field. A gap, left so the history reads as it happened.
//
// 27: sjawhar/legion#1986 -- a pod's `workspace-init provision` command names the repository's
// default branch, `--default-branch <name>`, which it requires: an image whose `legion` was built
// before 27 exits 2 on the unknown flag in every new issue pod's init container, so the daemon's
// image probe, which holds the image's pi-legion to this number, refuses such an image at boot
// instead. (26 was sjawhar/legion#1999's, open when this was taken; a release declaring 26 would
// carry no `--default-branch`, so this branch took the number above it, as the collision note says.)
//
// 28: LEGION-694 (sjawhar/legion#1999) -- api.State gains `heldOutboxRows`, every due lifecycle
// outbox row held behind its issue's running lifecycle row (record.Store.HeldRows), each with its
// id, kind, issue, how long it has waited and the row it waits on. Never null, so a plugin built
// before it refuses the state, and the bump is why the two never meet. (This branch took 25, then
// 26 when LEGION-636 landed at 25, and 27 above reserved 26 for it; but pi-legion v10.3.5 and
// v10.3.6 were released from main declaring 27 without `heldOutboxRows`, so a daemon of this branch
// at 27 would pass the boot gate against a plugin whose strict reader then refuses the state. The
// release that first declares a number must carry your shapes: it does not, so this branch takes
// the number above it, as the collision note says.)
//
// 29: sjawhar/legion#1998 -- under runtime.kubernetes.agent_secrets the daemon-launched
// controller's pod carries AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR and its shim the three
// agent-secrets flags, as every workflow role's does, so its `slack_post` is granted the Legion
// daemon service's LEGION_SLACK_BOT_TOKEN; and a project's `slack_channels` (which only
// `controller: daemon` with the broker may set) reach that controller as LEGION_SLACK_CHANNELS,
// always set (empty for none), which the plugin requires there and subscribes to while the session
// holds the controller role and which the controller's generation records with its addresses, so a
// daemon restarted with another list relaunches it, and as the addressing's `Slack channels:` line,
// which cites the controller skill's Slack reference. A plugin from before 29 subscribes to no
// channel and ships neither `slack_post` nor the reference, so the boot gate and
// `legion probe-image` refuse the mixed pair. The controller secret's answer is unchanged.
//
// 30: sjawhar/legion#1986 (per-owner credentials) -- every pane and pod holds a gh bundle, one
// directory per GitHub owner of the project under LEGION_GH_OWNERS_DIR, GH_CONFIG_DIR its issue's
// owner's (write) and every other owner's read-only, with LEGION_GH_PATH the real gh the `gh`
// wrapper first on PATH (`legion gh`; `/opt/legion/bin/gh` in the image) runs; every shared
// clone's credential helper is `legion git-credential` with `credential.useHttpPath` on. A role
// Secret's gh keys are per owner (`github-hosts-<owner>`, `github-config-<owner>`,
// `github-default`), and `POST /legion/v1/controller/github-credential` takes an optional
// `default` and answers `owners` (each owner's `hosts` and `config`), `default`, `app` and
// `expiresAt` in place of one `hosts` and `config`. An image built before 30 has neither the
// wrapper nor the two commands, so the image probe refuses it. (This branch took 28, then 29 when
// sjawhar/legion#1999 landed at 28; main then landed sjawhar/legion#2003 at 29 and released
// pi-legion v10.5.0 declaring it without these shapes, so this branch takes the number above it,
// as the collision note says.)
//
// 31: sjawhar/legion#1986 (the issue's repository) -- an issue's record carries the repository it
// targets and that repository's default branch, taken from Dispatch's resolved_repo and GitHub
// when the daemon first records it: `GET /legion/v1/state` and `read_record` answer `repo` and
// `defaultBranch` on every issue and `repo` on its pull request, and the state answers
// `repositories`, the project's mapping as the daemon serves it (`owners`, `mapped` with each
// repository's `default`, and `refused`, each mapped repository left out with its `reason`). A
// plugin at 30 reads the state with a strict schema that knows none of these fields, so it
// refuses the state.
//
// 32: sjawhar/legion#1986 (the repository cache) -- the daemon runs one repository cache pod per
// served repository, whose one container is the image's `legion workspace-init cache`, and every
// issue pod's `workspace-init fetch` is told its repository's cache (`LEGION_REPOSITORY_CACHE_URL`)
// and that cache's bearer token (`LEGION_REPOSITORY_CACHE_TOKEN_FILE`), and fills a fresh feed
// from that cache alone, verified, fetching only the delta from GitHub; its `workspace-init
// provision` makes the issue's clone shared with the feed, which now lives on the issue's own
// volume. An image whose `legion` was built before 32 exits 2 on the unknown subcommand in every
// cache pod, and its fetch clones from GitHub, ignoring the cache, so the daemon's image probe,
// which holds the image's pi-legion to this number, refuses such an image at boot instead. The
// state's `repositories` gains `cache` on each mapped repository under the Kubernetes runtime, with
// its `state`, and `caches`, the caches of repositories the mapping no longer names, each with
// `retainedUntil`, which a plugin at 31 reads with a strict schema that knows neither.
//
// 33: LEGION-781 -- the architect accepts before the merge, the merge closes the issue, and the
// production check is work after the close. The phase enum gains `accepting` (between `reviewing`
// and `retro`, the architect's `sign_off` phase, a backward target too) and loses
// `production_check`; the issue record on GET /legion/v1/state gains `check` (api.CheckView), the
// issue's open production check with its repository, pull request, merge commit, looks, last wake
// and bound, absent once it closes; `HandoffCompleteRequest.commit` becomes optional, since the
// implementer's report on a look of that check (`handoff_complete` on a done issue whose check is
// open) pushes nothing, and its `verdict` gains `pending` beside `pass` and `fail`;
// `PhaseBackwardRequest` gains an optional `issue`, which the architect's grant, serving its whole
// tree, names the issue it returns from `accepting` with. A 32 plugin's strict reader refuses a
// state carrying `check` or the phase `accepting`, and its `handoff_complete` requires a commit the
// check report has none of, so the boot gate and `legion probe-image` refuse the mixed pair. (This
// branch took 30, then 31 when main landed sjawhar/legion#1986 at 30 and released pi-legion
// v10.8.3 declaring it; main then landed sjawhar/legion#2005 at 31 and released pi-legion v10.9.0
// declaring it, then sjawhar/legion#2009 at 32 and released pi-legion v10.10.0 declaring it, each
// without these shapes, so this branch takes the number above it, as the collision note says.)
//
// 34: cybermonklabs/legion INFRA-31 -- the roles of an issue share one scratch on the issue's volume,
// `/legion/scratch` (scratchRoot), and the values `mainEnvironment` sets name its directories:
// every role container of a pod, a workflow role's and the controller's, is told the same
// `TMPDIR`, `XDG_CACHE_HOME`, `XDG_DATA_HOME` and `BUN_INSTALL_CACHE_DIR` —
// `/legion/scratch/{tmp,cache,data,bun/install/cache}` — and the same `GOCACHE` and `GOMODCACHE`,
// `/legion/go/build` and `/legion/go/mod` beside the workspaces, as `UV_CACHE_DIR` already was.
// `TMPDIR`, `BUN_INSTALL_CACHE_DIR`, `GOCACHE` and `GOMODCACHE` are new names; `XDG_CACHE_HOME`
// and `XDG_DATA_HOME` move off the image's home directory. The Go pair is shared because both
// hold content addressed by its hash and the roles of one issue may fill them at once, and a pair
// is 751 MiB of build cache and 375 MiB of module cache for this repository, so the five
// redundant copies would be about 5.5 GiB spent on nothing. `XDG_STATE_HOME`, whose
// `/home/legion/.local/state/<role>` entry 18 declared, is the one directory of the scratch that
// stays one per role, `/legion/scratch/state/<role>`: Oh My Pi names its browser broker's lock
// from the state root, the name is an abstract unix socket, and an abstract socket name is one
// name across every container of the pod, so one state home would block every role's broker but
// the first. `XDG_CONFIG_HOME` is unchanged, the pod's shared in-memory volume `workspace-init`
// writes the agents' jj configuration in, and the issue pod's `workspace-init`, the one init
// container told any XDG home, keeps every value it had. These directories are on the volume
// rather than the gigabyte of ephemeral storage the container is admitted with, which the tools'
// caches overran: GKE Autopilot evicted the pod. The volume a project's issues get by default is
// 50Gi (`runtime.kubernetes.issue_volume`). The pane and pod environment is the surface this
// contract names, and four of these names are the runtime's alone (`runtimeOwned`), which a spec
// or an operator's pod env is now refused for setting, so the boot gate and `legion probe-image`
// hold the daemon and the plugin to the same list. (This branch took 33; main then landed
// LEGION-781 at 33 and released pi-legion v10.16.0 declaring it, without these shapes, so this
// branch takes the number above it, as the collision note says.)
//
// 35: LEGION-791 -- api.State gains `settings`, the daemon's resolved legion.yaml with every
// credential, credential source and credential file path left out and a URL's userinfo masked as
// `xxxxx@`, keyed as the file is. Required, so a plugin built before it refuses the state and the
// bump is why the two never meet; the plugin parses it loosely (two typed keys, the rest unknown),
// so a later legion.yaml key changes no strictly parsed shape and bumps nothing. (This branch took
// 30; main then landed sjawhar/legion#1986 at 30, 31 and 32, LEGION-781 at 33 and INFRA-31 at 34,
// each released without this shape, so this branch takes the number above them, as the collision
// note says.)
//
// 36: INFRA-229 (cybermonklabs/legion) -- `api.PullRequestView` gains `state`, the pull request's
// `open`, `merged` or `closed`, always emitted: an architect's acceptance is now refused while an
// issue beneath it is still in the workflow and a merged root's close waits for the rest of its
// tree, so a root stands in the phase that waits for its merge for days and an operator must see
// that its pull request merged rather than guess at it. A 35 plugin's strict reader refuses a
// state whose pull request views carry the field, so the boot gate and `legion probe-image`
// refuse the mixed pair.
//
// 37: cybermonklabs/legion INFRA-80 -- an issue's disk dies with its pod: the pod's one volume is a
// generic ephemeral volume the pod itself owns (issuePod.disk), which the kubelet creates
// with the pod and deletes with it, so an issue's Sandbox owns no volume claim template and a
// stopped issue's Sandbox is always deleted. Nothing carries a disk from one pod to the next, so
// every provision is the first and the `workspace-init provision` container's environment loses
// `LEGION_EXPECT_ISSUE_VOLUME`. With it go `workspace-init`'s exit code 3 for a volume holding
// neither the clone nor a retained session, the `LEGION_WORKSPACE_RECOVERED_FROM` a relaunch after
// that loss carried and the `.legion/<KEY>/workspace-recovered.json` marker `workspace-init` wrote
// from it, the daemon's probe for a retained session of the issue that decided whether to expect
// anything, and `legion sessions mark-lost`, the operator command that marked a claim whose
// workspace was lost. The controller's pod loses its init container with them, since the sessions
// directory it made is gone. `runtime.kubernetes.session_store` must name a store that keeps
// sessions in a database, `postgres` or `cloudsql-iam` — an omitted key is the same refusal as an
// explicit `pvc` — since a session kept as a file on the pod's disk would die with the pod and the
// next launch's resume would fail on the missing file, retiring the claim after three failures. A
// daemon before 37 sets `LEGION_EXPECT_ISSUE_VOLUME=true` on a pod whose `workspace-init` from
// this commit knows neither that name nor exit 3, so a lost disk is
// provisioned afresh and reported as provisioned, and that daemon resumes a role onto a session
// file the new disk does not hold; an image built before 37 runs the whole lost-disk machinery
// against a daemon that sets none of it. The image probe, which holds the image's pi-legion to
// this number, refuses either pair. (This branch took 36; main then landed INFRA-229 at 36 and
// released pi-legion declaring it, so this branch takes the number above it, as the collision
// note says.)
//
// 38: cybermonklabs/legion INFRA-225 -- Legion starts no issue that waits on an unfinished Dispatch
// issue, and says what it is waiting for. `api.Admission` gains `blocked`, required: every root
// and every child Legion is waiting to start, by issue key, each `{issue, blockedBy}` with the
// keys it waits on that are not done (`blockedBy` is never null, and a key the daemon has no
// mirror row of counts as unfinished). `waiting` is unchanged, the roots' waiting line.
// `api.WaveReleaseResponse` gains `started` and `held`, both required, `held` an array of the
// same `{issue, blockedBy}` row: a release accepts the whole wave whatever its children's links
// say, and these two say which children it started and which it left held. A 37 plugin's strict
// reader refuses a state carrying `blocked` and a release answer carrying `started` or `held`,
// so the boot gate and `legion probe-image` refuse the mixed pair. (This branch took 36, then 37;
// main landed INFRA-229 at 36 and INFRA-80 at 37 while it was open, so this change takes the
// number above them, as the collision note says.)
//
// 39: INFRA-208 -- an issue pod is one container, `roles`, whose PID 1 is one `legion launcher`
// serving every workflow role (`--roles-dir /var/run/legion/roles` and one `--role` per role), each
// role's generation started in namespaces of its own by `legion launcher role-init`; each role's
// own volumes are mounted under `/var/run/legion/roles/<role>/` and the role homes volume at
// `/var/run/legion/roles/.homes`. An image whose `legion launcher` lacks multi-role mode cannot run
// that pod, and the image probe refuses an image whose pi-legion declares another contract, so the
// bump keeps a daemon from building such pods for an older image. A sandbox locator of an issue
// pod's role names container `roles`, and store migration 0054 rewrites every stored one at boot;
// a running pod of the layout before it is StaleAddress for every role and replaced once by the
// next launch of any of them, uncharged. `settings.runtime.kubernetes.resources` is now
// `{issue, controller}`, the two pods' reservations, rather than one entry per role; the
// resource-limits capability row names those pods. (This branch took 36; main then landed
// INFRA-229 at 36, so this branch took 37; main then landed INFRA-80 at 37 with store migration
// 0051, so this branch took 38 and store migration 0052; main then landed INFRA-225 at 38 with
// store migrations 0052 and 0053, so this branch takes 39 and store migration 0054, as the
// collision note says. Other open branches may declare 39 too: whichever merges after another
// takes the next free contract number, every site that names it included. A store migration is
// not renumbered with it: one added after the numbered series takes the UTC moment it was
// created as its version, 14 digits YYYYMMDDHHMMSS, which sort above the numbered series, so two
// branches open at once cannot claim one version and the file keeps the name it was written
// with. A rollback that deletes the row a migration recorded, as the one in docs/kubernetes.md
// does with `delete from schema_version where version = 54`, names that version, which for a
// migration written since is its 14-digit timestamp.)
//
// 40: a root whose architect asked a person a question and handed the turn back gives its
// admission slot up in the new phase `awaiting_person`, and its strict state issue view gains
// `waitingOn`: `kind` `ask` with the question's `ask`, or `gate` with `gateVersion` and
// `artifactId` (in the contract now, so the next release's gate waits need no plugin bump; no
// daemon of this contract writes one), `since`, the hand-back the wait began at, and, once the
// wait was answered, `answeredAt` and `answeredBy` (`answeredBy` names how, not always by a
// person). A 39 plugin's strict reader refuses the phase
// and the field, so the boot gate and `legion probe-image` refuse the mixed pair. (39 is INFRA-208,
// on main before this change, so this change takes the number above it, as the collision note
// says.)
//
// 41: `legion controller start` passes `/legion-controller-start`
// (daemon.ControllerStartCommand) on the operator's Oh My Pi command line, and pi-legion sends the
// controller's start message from that extension command rather than from session_start: Oh My Pi
// drains every session_start send at the end of its startup, so the start turn ran inside the
// startup wait, with no TUI and no input, while a command-line word runs as a prompt after startup
// and once the TUI listens. Under this CLI a pi-legion before 41 sends from session_start and then
// gets the word as a prompt, and a pi-legion at 41 under an older CLI never sends the start, so the
// controller probe refuses either pair before the mint. The daemon-launched controller is unchanged
// on the wire. (This branch took 40; main then landed the awaiting_person change at 40 and released
// pi-legion v11.1.0 declaring it, without these shapes, so this branch takes the number above it,
// as the collision note says. Other open branches may declare 41 too: whichever merges after
// another takes the next free number, as the collision note says.)
//
// 42: a worker's environment gains the four LEGION_STALL_* numbers under both
// runtimes (`internal/daemon/specs.go` merges each one the daemon holds into `spec.Env`, which a
// pane's `-e` pairs and a pod launcher's start command carry verbatim), and a Claude Code role
// gains CLAUDE_CONFIG_DIR, CLAUDE_CODE_DISABLE_BACKGROUND_TASKS and CLAUDE_CODE_DISABLE_CRON in
// its start command's environment. Both plugins read the four numbers through
// `packages/pi-shared/src/stall-limits.ts`, and `@sjawhar/claude-envoy` reads the Claude
// directory, so the daemon and the image that carries them ship together. With 37's disk dying
// with its pod, a Claude Code role's session is kept in Oh My Pi's session table by the pod's
// own launcher: each Claude launch's start command carries LEGION_CLAUDE_SESSION_ID, the session
// it starts or resumes (`claudelaunch.SessionVariable`), and the image's `legion launcher` reads
// it, in the manager of that role in the one `roles` container, to keep the transcript and the
// phase-reminder record as rows of `omp_session_files` keyed by their paths, restoring both before
// a resume's child starts, and writes `<transcript>.legion-saved` once its first save of a fresh
// transcript has committed, which the image's `@sjawhar/claude-envoy` waits for before it
// registers the transcript. A launcher before 42 ignores the variable and keeps nothing, so the
// next pod's resume of that transcript is refused, and a claude-envoy before 42 registers a
// transcript the table may not hold yet; an image at 42 under a daemon before 42 is told no
// session, so its claude-envoy waits for no marker and its launcher keeps nothing. The image probe
// refuses either pair. This change adds no store migration. (This branch took 36 through 41 in
// turn; main landed each of those numbers first, the controller start command last at 41, so this
// branch takes the number above them, as the collision note says.)
//
// 43: api.State gains `consumers`, the lag of the daemon's three durable consumers
// (api.ConsumerLagView), read from NATS after the state's transaction commits: each consumer's
// `name`, `waiting` (JetStream's NumPending plus NumAckPending), `oldestAt` and
// `oldestAgeSeconds` (the stream time of the first message after its ack floor on one of its
// filter subjects, and that message's age in whole seconds; both null when nothing waits or the
// message could not be read), `lastHandledAt` (when the daemon last acknowledged one of its
// messages or terminated one as poison, null before the first; a nak is not handled), `stalled`
// (messages waiting and none handled for 60 seconds, counted from the consumer's open before the
// first) and `unavailable` (what NATS did not answer within the read's 2-second bound or refused,
// absent when every read answered: a failed CONSUMER.INFO leaves `waiting` 0 and `stalled` false,
// a failed STREAM.MSG.GET only the oldest null). Never null, so a 42 plugin's strict reader
// refuses the state, and the boot gate and `legion probe-image` refuse the mixed pair. The state's
// `repositories` gains `withoutWebhookApp` in the same contract: each served repository, as
// Dispatch maps it, that Dispatch's GitHub App (the one GitHub sends its webhook events through)
// is not known to be installed on, `{repo, reason}` sorted by repo, the reason naming whether the
// App is not installed, the check failed, or Dispatch does not answer the check; never null, and
// the repository is served all the same. (This branch took 41, then 42; 42 is declared by main's
// stalled-worker reminder, so this change takes the number above it, as the collision note says.)
//
// 44: a Claude Code role's start command and pane no longer carry
// CLAUDE_CODE_DISABLE_BACKGROUND_TASKS; CLAUDE_CONFIG_DIR and CLAUDE_CODE_DISABLE_CRON stay, and
// the name is no longer the runtime's, so an operator's `runtime.kubernetes.pod.env` and
// `runtime.tmux.pane_env` may set it as they may set Claude Code's other variables. A role may
// leave a background task running past the end of a turn: when it finishes, Claude Code starts a
// turn of its own, which the bridge reports to the daemon as a turn (`internal/claudebridge`), so
// the result is read in a turn the daemon sees rather than after one the daemon thinks has ended.
// The worker container's and the pane's environment is one of this contract's surfaces, so
// dropping a variable from it bumps the number, as adding the three at 42 did, and the image probe
// holds this daemon and the image that runs its roles together. This change adds no store
// migration. (This branch took 43; main then landed the consumer lag at 43, so this branch takes
// the number above it, as the collision note says.)
//
// 45: sjawhar/legion#2354 -- a phase's handoff is a record on its Dispatch issue, which the
// worker writes with `dispatch handoff-write` under its own session, rather than a committed
// `.legion/<KEY>/<phase>.json`. `api.HandoffCompleteRequest` gains `handoffId` and
// `handoffVersion`, the record a file-backed phase's completion (planning, implementing, testing,
// reviewing) names, and the daemon refuses such a completion without one
// (HANDOFF_RECORD_REQUIRED); it reads the record back from Dispatch and refuses one Dispatch has
// not (HANDOFF_RECORD_NOT_FOUND), cannot read (DISPATCH_READ_FAILED), of another issue, phase or
// version (HANDOFF_RECORD_MISMATCH), or written by another session than the claim's
// (HANDOFF_SESSION_MISMATCH), and `HANDOFF_NOT_NEW` and the completion's idempotency key name the
// record id in place of the commit. `commit` stays the implementer's (its pushed tip, and retro's
// commit) and the merger's; the planner, tester and reviewer send none, and the daemon no longer
// requires one of them. `api.PhaseView` gains `handoffRecord`, `{id, version}`, absent until the
// worker's completion of its current file-backed phase names one; store migration 20261101144900_handoff_records keeps it
// on the phase row. A 44 plugin posts a commit and no record, which this daemon refuses, and its
// strict reader refuses a state carrying `handoffRecord`, so the boot gate and `legion
// probe-image` refuse the mixed pair. (This branch took 40 and store migration 0055; main then
// landed `awaiting_person` at 40 with store migrations 0055 to 0058, and `issue_ancestry` as store
// migration 0059, so this change took 41 and store migration 0060; main then landed
// `pull_request_head_move_starts` as store migration 0060 at 40, so this change took store
// migration 0061; main then landed `pull_request_head_run` as store migration 0061, so this change
// took store migration 0062; main then landed the controller start command at 41 and the worker
// stall and Claude session environment at 42, so this change took 43 and kept store migration
// 0062; main then landed the lost-event pass at 43 with store migrations 0062 to 0064, so this
// change took 44 and store migration 0065; main then landed background work at 44 with store
// migrations 0065 to 0067, which closed the numbered series (sjawhar/legion#2411), so this change
// takes 45 and a store migration named for the UTC moment it was written, as the collision note
// says.)
//
// 46: INFRA-484 -- the update round, which replaces a pod only once the work on it is pushed.
// `POST /legion/v1/claims/ready-for-update` is a new claim route in the shape of `claims/ready`
// (`claim.ReadyForUpdateRequest`: `claimToken`, `sessionId`, `secret`, `generation`, `round`, and
// an optional `head`), answering 204: it is the worker's answer to the update task — everything
// it wants kept is pushed, and its process may now be replaced. `round` is the round the task it
// read named, and the daemon takes the answer only for the round that claim stands in, refusing
// any other with 409 and a sentence naming both rounds. `head` is the commit the worker reports
// as the head of its issue branch at `origin`, empty when the branch is not there. Both
// plugins gain the `ready_for_update` op that posts it, so a worker under either one can end its
// round. A claim whose update round is open is served on its contract of before the round, on
// every route: the claim routes, the grants route's claim form and every grant-bearing route,
// where a stale plugin contract otherwise refuses. Every claim view gains the round it is in and
// the head its worker reported when it answered — `updateRound` and `updateHead`, both absent
// when there is none — on the state's claims (`api.ClaimView`) and on the operator's
// (`api.OperatorClaim`), which `packages/contracts/src/legion-api.ts` mirrors; a strict reader
// before 46 refuses a state carrying either, so `GET /legion/v1/state`, which authenticates
// nobody, serves a caller from a pod whose claims record a contract below 46 the claim views
// without them (api's stateFor): the round serves that worker until its pod is replaced, and
// every reader from 46 on takes both as optional. `legion.yaml` gains `pod_update_retask_hours` and
// `pod_update_notice_hours`, which the settings carry as the two numbers they are. A plugin
// before 46 has no op to signal with, so its worker answers by ending the turn the task started
// (supervise.PodUpdateContract names this number), and a daemon before 46 answers that op's post
// a 404; the boot gate and `legion probe-image` refuse either pair. (This branch took 43 while
// main's consumer lag was unmerged, then 44, then 45, and its store
// migration 0065; main landed the consumer lag at 43, the Claude Code background-task variable at
// 44 and the handoff records at 45 first, with store migrations 0065, 0066 and 0067, so this
// change takes the number above them. Its migration is named for the moment it was written,
// 20261029195520, since main closed the numbered series at 0067.)
//
// 48: an idle issue pod is released, so a plugin tells the daemon what its session is doing and a
// released claim is a new suspension source. Each session reports its activity to the daemon on
// POST /legion/v1/claims/activity, authenticated by its claim as `claims/ready` is: whether it
// runs background work and of which kinds, and when the phase reminder armed in it is due, if
// one still is. The daemon releases an issue pod no claim of which has reported
// background work and none of whose phase workers is due a reminder inside the idle period
// (`runtime.kubernetes.idle_release`), suspending each of its claims with the new
// `suspendedBy` value `released`, which keeps the claim's phase and its pending delivery and
// which the operator routes now serve. A released pod's unpushed work travels as a git bundle,
// which the shim builds on the daemon's `bundle-workspace` frame and the next pod's launcher
// restores from the chunked `launcher_bundle` frames before any role starts; a launcher before 48
// knows neither frame, and a shim before 48 answers none, so the image probe holds this daemon
// and the image that runs its roles together. A claim may read the deployment's release, resume
// and hold records on POST /legion/v1/claims/release-records, which names its claim, that
// claim's secret and the window's start in its body, as the other claim routes do: the secret is
// a credential, and a query string reaches every proxy and access log between the pod and the
// daemon. No session-facing body carries the new `suspendedBy` value: the state's
// `strandedClaims` keeps the three keys it has had since 23, and an operator reads a released
// claim apart from a suspended one on the operator claim routes, which no plugin calls. This
// change adds ten store migrations. (This branch took 47 while main's #2538 was unmerged; main
// then landed 45 and, as sjawhar/legion#2538, 46, and sjawhar/legion#2539 takes 47, so this
// branch takes the number above them, as the collision note says.)
//
// 49: INFRA-1101 -- the memory group of an issue's tree on `GET /legion/v1/state`. Every issue
// record of the state gains `memoryGroup` (`memory` or `none`), `memoryDrawMode` (the project's
// `projects.<KEY>.memory.mode` in force at the draw) and `memoryDrawnAt`, the three absent
// together while nothing has drawn for the issue's tree; the draw is made once on the tree's root
// record, and every issue of the tree is shown its root's (api.Issue, filled by
// internal/projection). `packages/contracts/src/legion-api.ts` mirrors them on `legionIssue`,
// which is a `z.strictObject`, so a reader before 49 refuses a whole state carrying any of the
// three — and every worker's `read_record` parses the state through it. `GET /legion/v1/state`
// authenticates nobody, so a caller from a pod whose claims record a contract below 49 is served
// the state without them (api's stateFor, withoutMemory), as a caller below 46 is served it
// without the claim views' update rounds; a caller below both is served it without either. The
// settings carry the project's `memory` block (mode, config files, instructions), which every
// plugin reads loosely and which changes no strictly parsed shape. This change adds no store
// migration of its own: the three columns are the issue record's
// (20261104073148_issue_memory_group), and the hand-over's answer beside them is too
// (20261104153554_issue_memory_carried). (This branch took 47 while main's idle-release change
// was unmerged; main landed it as 48, so this branch takes the number above it, as the collision
// note says.)
//
// 51: dispatch://INFRA-980 -- the admission cap counts the issues at work rather than the trees,
// so `api.Admission` on GET /legion/v1/state changes shape with it. `active` and `waiting` name
// issues, a child as much as a root: a tree with three working children holds three slots, and a
// tree whose issues only wait holds none. `trees` is new, the root of every live tree
// (record.TreeLive), always emitted and never null, because a running tree may now hold no slot
// and `active` alone no longer says which trees run — the controller reads it where it read
// `active` as the running trees. Each issue record gains `tree`, the root of its tree, absent only
// on an issue the workflow does not record, so a reader tells a child from a root and finds the
// members of a tree that lingers. A plugin before 51 reads the state with a strict schema that
// knows neither key and refuses the whole body for either, so the route serves `admission.trees`
// and the issues' `tree` only to a caller whose pod records this contract, and serves a pod below
// it the body its plugin reads (api's stateFor, withoutTrees), as it serves the memory group only
// from 49 on; a caller the daemon cannot place in a pod — the operator's `legion state` — is
// served both. One change no gating can carry is the meaning `active` and `waiting` lost: they
// keep their names and name issues where they named trees, so a plugin below 51 reading a 51
// daemon's state reads a tree's working children as that many running trees and miscounts them,
// and its controller takes `active` for the trees at work. What protects against that is the
// pair, not the stripping: the boot gate and `legion probe-image` refuse a daemon and a plugin
// that declare different contracts, so no plugin below 51 is left reading a 51 daemon. The
// stripping is for the one window that pairing leaves open — a pod of the contract before this
// one still being replaced, whose roles go on reading the state through the plugin their image
// was built with. This change's store migrations are
// named for the UTC moments they were written, 20261103183833 and 20261104004500, since the
// numbered series is closed. (This branch took 47 while sjawhar/legion#2731 held that number
// open and unmerged; main has since reached 49 — 47 is sjawhar/legion#2539's, the idle release
// landed as 48 and the memory group as 49 — and sjawhar/legion#2731, dispatch://INFRA-1077, has
// since taken 50 on its own branch, so this change takes the number above them all, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// 52: INFRA-1307 -- a pane is told the issue key it stamps into its commits, LEGION_ISSUE_TRAILER:
// the claim's own issue when `projects.<KEY>.commit_issue_trailer_repos` names the repository the
// launch works, among the repositories Dispatch maps to the project, and set on no other pane, so
// a squash landing the branch of such a repository carries `Dispatch: <KEY>` in its message. Each
// runtime sets the name itself and owns it (tmux's `runtimeOwned`, the Sandbox's), so a spec, an
// operator's `runtime.tmux.pane_env` and a `runtime.kubernetes.pod.env` are all refused for
// setting it: no pane environment an operator writes can forge a key the daemon did not decide.
// Both plugins read that variable and never LEGION_ISSUE
// (`packages/pi-shared/src/jj-attribution.ts`, the one writer of the jj overlay they share), so a
// plugin built before 52 stamps nothing on a daemon that expects the landing message to carry the
// key, and a plugin from 52 under a daemon before it is told no key and stamps nothing either,
// while the pane environment that daemon builds may still carry an operator's own
// LEGION_ISSUE_TRAILER. The pane environment is one of this contract's surfaces, as it was at 34,
// 42 and 44, so the boot gate and `legion probe-image` refuse the mixed pair. The key is left out
// of the `settings` block on GET /legion/v1/state (api's settingsExcluded): that route
// authenticates nobody, and the value names a deployment's own repositories. This change adds no
// store migration.
// (This branch took 50 while main held 49, and main has since reached 51, so this change takes
// the number above it, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says. main held 51 at the forward merge that renumbered this entry, 71abec3650df; read main's
// own number again at the next merge rather than from this line.)
//
// 53: INFRA-976 -- the daemon spreads the wakes it causes at the pace its operator set
// (`legion.yaml`'s `worker_wakes_per_minute`, default 8, and `worker_wake_burst`, default 1, which
// the settings carry as the two numbers they are), and `api.State` gains `wakesWaiting`, the wakes
// that pace is holding until their turn: an array, never null, of `{claim, issue, role, kind,
// dueAt}`, beside `strandedClaims` and `heldOutboxRows`, one row per wake of the three lanes it
// paces — `launch`, a role process it is about to start, `plugin-task`, the update task a
// stale-plugin round queues to a session already running (73 below deletes that round and names
// the relaunch it became `plugin-refresh`, so no daemon from 73 on writes this kind), both held
// in the claim's machine, and
// `catch-up`, the notice a start writes to an architect already running, which waits on its own
// outbox row — in claim order, and empty at every moment outside a restart, a re-admission wave or
// a plugin wave. A strict reader before 53 refuses a state carrying the key, empty as surely as
// naming a wake, so the bump is why the two never meet; for the one window the pairing leaves
// open — a pod of the contract before this one still being replaced — the route serves a caller
// whose pod records a contract below 53 a body with no `wakesWaiting` key at all (api's stateFor,
// withoutWakesWaiting), as it serves the live trees only from 51 on and the memory group only
// from 49 on. The key is left off that body rather than emptied, which is why
// `packages/contracts/src/legion-api.ts` declares `wakesWaiting` optional, as it declares
// `admission.trees`. That caller is the only one the key is left off for: a caller the daemon
// cannot place in a pod — the operator's `legion state` — is served it, and so is a caller in a
// pod whose claims record no contract at all, which the route can place in no band and serves
// the state untouched (stateFor, podContract). With `daemon.startedAt` and each claim's
// `updateRound` it is what says whether a wave is still running. `legion state` prints a
// `wakes waiting:` line only when something waits.
// This change adds no store migration. (This branch took 49, then 50, then 52, while main's
// memory-group, admission-cap and issue-key-stamp changes were unmerged; main landed those as 49,
// 51 and 52, so this branch takes the number above them all, as the collision notes above say.
// main held 52 at the forward merge that renumbered this entry, bb24cc4d20d9; read main's own
// number again at the next merge rather than from this line.)
//
// 54: INFRA-809 -- the project decides whether a merge that closes an issue opens a production
// check and who works it (`projects.<KEY>.production_check`, `off` or `tester`), and the checking
// agent names its own next look. `api.CheckView` gains `role`, the role the check records as the
// one that works it, always emitted; its `nextWakeAt` is now the time the **next look** is due —
// the moment the checking agent named for itself on a pending report, the retry of a look nobody
// reported, or the check's own bound where neither stands — where before it was always the bound.
// The bound needs no field of its own: `nextWakeAt` is the bound whenever no agent's time is set,
// `openedAt` is on the view and `production_check_hours` is in the settings body.
// `api.HandoffCompleteRequest` gains `wakeAt`, which a pending check report on a look that is not
// the check's last must carry and no other report may (`CHECK_WAKE_REQUIRED`,
// `CHECK_WAKE_UNEXPECTED`, which the daemon also answers a phase completion that carries one),
// and a report by a role the check does not record is refused (`CHECK_ROLE_MISMATCH`). Which of
// the two a `handoff_complete` is, is the caller's own live assignment: a session whose task is a
// look of its issue's production check reports on that look, and every other session's completion
// is its phase's. A completion of a `done` issue, which runs no phase, from a session working no
// look is neither, and is refused `CHECK_LOOK_NOT_SERVED` rather than being read as a phase
// completion missing its commit. The settings body loses its `projects.<KEY>.deploys` block, with every
// path by which the daemon guessed that production carried a merge; a plugin reads the settings
// loosely, so that loss alone changes no strictly parsed shape. A plugin below this contract has
// a strict reader that refuses a state whose check views carry `role`, and its `handoff_complete`
// sends no `wakeAt`, which this daemon requires of a pending report, so the boot gate and `legion
// probe-image` refuse the mixed pair. (This branch took 50 while main stood at 49;
// sjawhar/legion#2731, dispatch://INFRA-1077, holds 50 on its own branch, and main has since
// landed the admission cap at 51, the pane issue key at 52 (sjawhar/legion#2839) and INFRA-976's
// wake pacing at 53 (sjawhar/legion#2805, merged as d43e0fe9 just before this one), so this
// branch takes the number above them all, as the collision note says. main held 53 at the
// forward merge that renumbered this entry, d43e0fe9bbb1; read main's own number again at the
// next merge rather than from this line.)
//
// 55: INFRA-1272 -- the design gate is a list of steps a deployment declares, and the issue
// record of `GET /legion/v1/state` gains `gatedSteps`, one entry for each step the deployment
// gates, in the order it gates them (`api.GatedStepView`): the `step` (`decisions`, `criteria` or
// `spec`), the document its architect registered for that step and that document's
// `currentVersion` (both absent until one is registered), the `approvedVersion` a person
// approved (null until one does), the `status` — `unregistered`, `waiting`, `approved` or
// `changes_requested` — and `changesReason`, what the reviewer asked for, on that last status
// alone. It is absent, as `designGate` is, from an issue that is not its tree's root, from every
// issue of a deployment that gates no step and from an issue the workflow does not record, so the
// wire carries no empty list. `designGate` is unchanged beside it, the spec step's row as it has
// always been, so a reader that asks one question of one gate reads what it read before.
// `GET /legion/v1/state` authenticates nobody, so a caller from a pod whose claims record a
// contract below 55 is served the issues without the field (api's stateFor, withoutGatedSteps),
// as a caller below 53 is served a body with no wakes waiting, one below 51 the issues without
// the admission's trees and each issue's tree, one below 49 without the memory draw and one
// below 46 without the claim views' update rounds; the five compose, and every reader from 55 on
// takes the field as optional.
// `POST /legion/v1/controller/secret` answers the same list as `designGateSteps` in place of the
// one word `designGate`, which 55 drops: `legion controller start` is built from this daemon's
// own tree, so the two always ship together, and a `legion` of this release refuses an answer
// that names no list at all.
// (This branch took 50 while main held 49, then 52 when main landed its admission cap as 51, then
// 53 when main landed the issue-key trailer as 52, then 54 when main landed the wake pacing as
// 53, and main has since landed the optional production check as 54, so this change takes the
// number above it, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says. main held 54 at the forward merge that renumbered this entry, b5482068c554; read main's
// own number again at the next merge rather than from this line.)
//
// 56: dispatch://INFRA-1480 -- `api.StrandedClaim.state` on `GET /legion/v1/state` may now be
// `retired` as well as `suspended` and `failed`, because `workflow.Stranded` now reports a retired
// claim that owns the phase its issue's worker works: only a start of that phase relaunches such a
// claim, and no rule the daemon runs queues that start by itself, so nothing will wake it. A plugin
// before 56 parses `strandedClaims[].state` with a strict enum of two values and refuses the whole
// state body for a third, and every worker's `read_record` goes through that parse. The route
// authenticates nobody, so a caller from a pod whose claims record a contract below 56 is served
// the report without its retired entries (api's stateFor, withoutRetiredStranded), as a caller
// below 49 is served it without the memory group. Dropping those entries, rather than serving
// them, is the only choice that keeps such a pod able to read the state at all: the value is the
// claim's own state and there is no older value to put in its place, and one retired entry refuses
// the whole body, which takes every role's `read_record` in that pod with it. Such a pod is served
// a report missing its retired entries until the pod is replaced, and that window is the one the
// plugin pair already bounds: the boot gate and `legion probe-image` refuse a daemon and a plugin
// that declare different contracts, so the only reader below 56 is a pod of the contract before
// this one still being replaced. The same change adds three fields to the operator claim view's
// pending delivery (api.DeliveryView): `sendFailures`, `lastSendFailureAt` and `lastSendError`,
// how many sends of the task failed, when the last one failed and the error it failed with. They
// need no gate, because no plugin calls the operator routes. This change adds one store migration,
// for those three columns, named for the UTC moment it was written, since the numbered series is
// closed. (This branch has been renumbered three times, which is what a hand-allocated serial
// costs: 51, then 53 when main reached 52, then 55 when main reached 54, and 56 now that main
// carries 55 — INFRA-1272's gated steps, merged as #2848 while this branch stood approved at
// its own 55. So it takes the number above every number main declares, as the collision note
// says, and that number was read from main at this merge rather than assumed. Two other open
// branches declare 55 as well: whichever merges after another takes the next free number, and
// the number is read again at every forward merge, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// 57: dispatch://INFRA-1274 -- every claim view carries `step`, what the daemon last saw that
// claim's agent do: `at`, when the daemon received that step, `turnStartedAt`, when the turn it
// belongs to began, the `tool` and `callId` of the call the agent is in, that call's `argsBytes`
// and `argsHash` — its arguments' length and an eight-byte fingerprint of them, never their text,
// since a shell command routinely holds a credential — and its `startedAt` and `endedAt`. It is on
// the claim views of `GET /legion/v1/state` and on the operator's `legion claims list` (api's
// StepView, ClaimView.Step and OperatorClaim.Step), absent for a claim whose agent has neither
// begun a turn nor made a call the daemon saw, and a call carrying `startedAt` and no `endedAt` is
// the shape a turn that stopped moving leaves behind. A strict reader before 57 knows no such key
// and refuses the whole state body for it, so the route serves a caller whose pod's claims record
// a contract below 57 the state with no `step` at all (api's stateFor, withoutStallRecord), as it
// serves a caller below 56 one whose claims nothing will wake name no retired claim, a caller
// below 55 one with no gated steps, a caller below 53 one with no `wakesWaiting` and a caller
// below 49 one with no memory group. That caller is the only one the key is left off for: a
// caller in a pod whose claims record no contract at all is placed in no band and is served the
// state untouched (stateFor, podContract), and a state whose claims could not be read refuses
// the whole request before the route cuts a body at all. 57 also brings the three numbers a
// deployment moves a session's watch on its own tool calls with — LEGION_STALL_CALL_BOUND_MS,
// LEGION_STALL_CALL_GRACE_MS and LEGION_STALL_STEP_MS — in the environment of every pane and of
// every pod's worker container, the controller's among them (`internal/daemon/specs.go`'s
// stallLimitVariables, merged into `spec.Env` after SpawnSpec's fork, which a pane's `-e` pairs
// and a pod launcher's start command carry verbatim): a session told none of them runs on the
// defaults its plugin holds. This change adds the store migration the record is kept in,
// `20261107052815_claim_step_record`. (53 is the contract this branch declared at its bump;
// main has since landed the optional production check at 54, INFRA-1272's design-gate steps at
// 55 and INFRA-1480's retired stranded state at 56, none of which carries the claim step record
// or the stall numbers, so this branch takes 57, above them all, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says. main held 56 at the forward merge that renumbered this entry, ff2a10ef3ef0; read main's
// own number again at the next merge rather than from this line.)
//
// 58: INFRA-1100 -- the issue record of `GET /legion/v1/state` gains `waitingLine`, where the
// issue stands in `admission.waiting`: its `place`, 1 being first in the line, and the `length`
// of that list at this read (`api.WaitingLineView`). It is present only for an issue the list
// names, so a slot holder and an issue the line does not carry have no such key, and the wire
// carries no place for an issue that stands in no line. It says where the issue stands, not what
// holds it: `admission.blocked` names what holds it, and a member a Dispatch link blocks stands
// in the line and has its place like every other member.
// `GET /legion/v1/state` authenticates nobody, so a caller from a pod whose claims record a
// contract below 58 is served the issues without the field (api's stateFor, withoutWaitingLine),
// as a caller below 57 is served the claim views with no `step`, one below 56 the stranded
// report without its retired entries, one below 55 the issues without the gated steps, one below
// 53 a body with no wakes waiting, one below 51 the issues without the admission's trees and
// each issue's tree, one below 49 without the memory draw and one below 46 without the claim
// views' update rounds; the eight compose, and every reader from 58 on takes the field as
// optional. A plugin at 57 does not refuse the field, as a plugin before one of the earlier
// contracts above refuses its own: every answer-side schema of `packages/contracts` has stripped
// a key it does not name since sjawhar/legion#2788, which that package's own test holds it to,
// and 57 is later than that change, so such a plugin reads this state and sees no `waitingLine`
// at all. The strip below is still what serves it: a caller that did not ask for the field is
// not given it, and a plugin built before #2788 is strict and would refuse the body.
// (This branch declared 56 while main held 55. main then landed INFRA-1480 at 56, and
// @sjawhar/pi-legion 18.32.1 was released declaring that 56 with none of these shapes, so this
// branch took 57; main has since landed INFRA-1274's claim step record at 57, which carries none
// of these shapes either, so this branch takes the number above it, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says. main held 57 at the forward merge that renumbered this entry, 0297a21a3b07; read main's
// own number again at the next merge rather than from this line.)
//
// 59: INFRA-956 (sjawhar/legion#2780) -- fix-attempt counting gone. `legion.yaml` no
// longer takes `max_fix_attempts`, and a file carrying it loads with a warning naming the
// removal, which boot logs and `legion start --check-config` prints, rather than a refusal, so
// the settings block of `GET /legion/v1/state` no longer carries `max_fix_attempts` and
// `api.Settings` no longer models it. Each issue's pull request in that state loses
// `fixAttempts` (`api.PullRequestView`), which counted the heads pushed onto a red verdict. A
// red CI verdict now sends the work back to the implementer every time it stands, however many
// came before, and the architect decides when to stop; no pull request is marked blocked and no
// `pr-blocked` notice is published for a count. `packages/contracts/src/legion-api.ts` mirrors both
// removals, and its reader of the settings block is loose, so a plugin before 59 reads a state
// without `max_fix_attempts` — but its strict pull-request reader refuses one without
// `fixAttempts`, and every worker's `read_record` parses the state through it, which is what
// this number is for: the boot gate and `legion probe-image` hold plugin and daemon equal, so
// neither order of the two ever runs.
// The issue's other half, the provider-error retry, adds two controller routes, one operator
// route and two tool operations. `POST /legion/v1/claims/fresh-session` (`api.ClaimRequest`:
// `grantId`, `issue`, `role`) takes a controller grant and runs, for the claim on that role of
// that issue, the effect `legion claims retry --fresh-session` runs for an operator: the claim
// leaves the session it recorded behind and starts again in a new one, which also puts its run
// of provider-error turns back to zero. One effect stands behind both doors (api's
// freshSession), and it reaches a claim whose agent is registered, ready or idle and one that
// is suspended, failed or retired, where the plain retry reaches only a claim that failed and
// one an operator stopped, which `legion claims stop` retires. Its answer
// (`api.FreshSessionResponse`), and the `api.OperatorClaim` the
// operator's retry answers, carry an optional `phaseNotRestarted`: the claim came back owing no
// task and the workflow refused the restart of its phase, which is the one thing the claim's
// state does not record, so the fresh session reports the refusal rather than giving it, and the
// claim is started either way.
// The controller's `legion` tool gains `fresh_session (issue, role)`, which posts it, and
// `packages/contracts/src/legion-api.ts` mirrors the request and that member.
// `POST /legion/v1/claims/compact`
// (the same `api.ClaimRequest`) is the recovery the controller reaches for
// first: it compacts the claim's Oh My Pi session and sends the claim whatever task it owed again,
// where a fresh session keeps nothing the old one held. It answers `api.CompactResponse`
// (`compacted`, and `reason` when it is false), and a compaction Oh My Pi refused or skipped is
// that answer rather than a failure, since the claim is back and whatever task it owed has gone
// again all the same. It reaches a claim whose agent is ready or idle, a root architect included,
// and refuses a suspended, failed or retired claim naming what starts it again
// (`CLAIM_NOT_RUNNING`): fresh_session for a workflow role, and the daemon's controller keeper
// for the project's own controller. It refuses a
// claim in a turn (`CLAIM_IN_A_TURN`), a claim between states — queued, with a launch in flight,
// one whose agent has registered and is not ready yet, or stored working under a launch the wake
// pacing parks (`CLAIM_UNSETTLED`), of which fresh_session refuses all but the registered one,
// which it reaches — a Claude Code claim, which runs no Oh My Pi session (`CLAIM_NOT_ON_OMP`),
// and the role `controller` (`INVALID_ROLE`). The controller's `legion` tool gains
// `compact (issue, role)` beside `fresh_session`, and
// `POST /legion/v1/operator/claims/{token}/compact`, which `legion claims compact --claim <token>`
// posts, is the operator's door onto the same effect. The compaction reaches Oh My Pi as the
// shim's own `compact` frame
// (`shimwire.Compact`, carried by `internal/stream`'s Conn.Compact and forwarded by the image's
// `legion shim` as a prompt is), so the pod's shim is one of this number's surfaces beside the
// three routes. None of the three routes needs a number of its
// own: the gate holds plugin and
// daemon equal, so a plugin whose tool posts these routes never meets a daemon that does not
// serve them. The rest of that half adds no route and changes no shape a plugin parses: a turn
// that ends in a provider error keeps its task, and the daemon reads that prompt's own result
// to decide whether the task goes to the same session again or the claim fails
// (`internal/supervise/providererror.go`). It ships two store migrations.
// `20261105004846_provider_error_retry.up.sql` adds the two columns a task a provider error
// took back is kept by (`pending_task_deliveries.provider_error_retries` and
// `provider_error_note`) and the claim's run of such turns (`claims.provider_error_run`), and
// `20261108165219_claim_death_charged.up.sql` adds what was charged for the death of the
// process a claim records (`claims.death_charged`, supervise.Claim.DeathCharged). The
// removal ships none; `fix_attempts`, `blocked_attempts` and `head_counted` stay as stopped
// columns that nothing reads, each given its zero on an insert (`record`'s PutPullRequest).
// (It took 47, then 48, then 50, then 52, then 54, then 55, then 56, then 57, then 58,
// as main's own changes took each number before it: main merged the idle-pod release at 48 while
// that branch was open, landed the memory group at 49, the admission cap over the issues at work
// at 51, the pane issue key at 52, INFRA-976's wake pacing at 53 and INFRA-809's production-check
// decision at 54 (sjawhar/legion#2855), each release declaring its number without these shapes,
// INFRA-1272's design-gate steps landed as 55 (cybermonklabs/legion#2848), INFRA-1480's retired
// stranded state landed as 56 (cybermonklabs/legion#2919), INFRA-1274's claim step record landed
// as 57 (cybermonklabs/legion#2922), and INFRA-1100's waiting line landed as 58
// (cybermonklabs/legion#2989), so it took the number above them all, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says. It landed on main as sjawhar/legion#2780's 59, which took this branch to 60.)
//
// 62: INFRA-2551 (cybermonklabs/legion) -- the daemon counts every move back of an issue by its
// `<from>-<to>` pair (internal/phase's BackwardPairs), and each worker of `GET /legion/v1/state`
// carries those counts as `backwardMoves`, the pair to its count (`api.PhaseView`). The key is
// absent, and never `{}`, for an issue that has had no move back, so a reader tells an issue
// carrying no counts at all from one whose counts the daemon holds. `rounds` beside it keeps its
// meaning exactly, rising on every move back, a conflict round included, which no pair counts
// since nobody sent the work back; and the counts start empty at the migration that added them
// (`20261110225246_backward_moves_and_cap_told`) and are never filled in from the past, so an
// issue already in flight reads counts that add up to less than its `rounds`.
// `GET /legion/v1/state` authenticates nobody, so a caller from a pod whose claims record a
// contract below 62 is served the workers without the field (api's stateFor,
// withoutBackwardMoves), as a caller below 58 is served the issues with no place in the
// admission line, one below 57 the claim views with no `step`, one below 56 the stranded report
// without its retired entries, one below 55 the issues without the gated steps, one below 53 a
// body with no wakes waiting, one below 51 the issues without the admission's trees and each
// issue's tree, one below 49 without the memory draw and one below 46 without the claim views'
// update rounds; the nine compose, and every reader from 62 on takes the field as optional. A
// plugin at 61 does not refuse the field, as a plugin before one of the earlier contracts above
// refuses its own: every answer-side schema of `packages/contracts` has stripped a key it does
// not name since sjawhar/legion#2788, which that package's own test holds it to. The strip is
// still what serves such a caller: one that did not ask for the field is not given it, and a
// plugin built before #2788 is strict and would refuse the body.
// The same change gives `legion.yaml` a `hooks:` key — each hook naming the event it stands on,
// the moves back it selects, how many of them fire it, the roles its words go in front of and
// those words, written inline or read from a `prompt_file` — and the settings block of that
// state carries every running hook as `hooks`: each one's `name`, `on`, `moves`, `every`,
// `roles` and `prompt_file`, and never its words, as this route serves the path of
// `instructions` and never its text. That key is no part of this number: the plugin parses the
// settings block loosely (35 above), so a later legion.yaml key changes no strictly parsed
// shape and bumps nothing. The one shape this number is for is the workers' field above.
// The rest of the change moves no shape a caller reads: the architect is told the
// `review_round_cap` message once for each value of the cap rather than on every round past it
// (`record.PhaseRow.CapTold`, the same migration), and a hook that fires publishes its prompt
// through the outbox to the roles it names (the `hook_prompt` payload), which is the daemon's
// own lane and no route.
// (The base carries 59, and no release declares this change's shapes. Two open pull requests
// declare the numbers between: cybermonklabs/legion#2731 declares 60 and #2230 declares 61,
// read from `const DaemonAPIVersion` at each head on 2026-11-11, so this change takes 62, the
// number above them all, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says. Read main's own number, and every open branch's, again at the next forward merge rather
// than from this line.)
// 64: INFRA-312 -- every Legion launch says which issue its dollars were spent on: the pane
// environment and a pod's worker container now carry `legion.issue`, the issue the launch serves,
// and `legion.role`, the role it runs, in `OTEL_RESOURCE_ATTRIBUTES`, written after the attributes
// the operator's own `runtime.kubernetes.pod.env` or `runtime.tmux.pane_env` sets (the one
// composer, `internal/runtime/spendattrs.go`'s SpendResourceAttributes, called from
// `internal/runtime/sandbox/manifest.go`'s roleOperatorEnv and `internal/runtime/tmux/spawn.go`'s
// panePairs), so every launch's resource attributes name the issue it serves rather than the
// deployment alone. Both keys reach the spend collector today, and its keep-list of six theorem.*
// resource attributes drops every other key, so no spend row names an issue yet. A row grouped by
// issue follows when the collector keeps them, which dispatch://INFRA-1835 adds to that list. The
// detector that reads the value assigns last-wins, which is why the daemon's pairs are
// written after the operator's; a pair the launch holds no fact for is left out rather than
// written empty, so the project controller's launch, which serves no issue, carries `legion.role`
// alone. The name stays the operator's to set — the runtime composes the value, it does not own
// the name — but a launch spec may not set it (`composed`, beside `runtimeOwned`), since a spec's
// second pair of that name is the one the C library's lookup answers with, and both configuration
// loaders refuse an operator value the agent's OpenTelemetry detector would throw on
// (runtime.SpendAttributesRefusal): that throw is caught with an empty hand, so one malformed
// member costs the launch every attribute of the variable, the operator's own theorem.project
// among them. The pane environment and the worker container's environment are surfaces of this
// contract, which is why an environment change takes a number at all: the boot gate refuses to
// start against an installed plugin declaring another number, and `legion probe-image` refuses an
// image whose pi-legion declares another, so no pane and no pod runs an agent of one contract
// under a daemon of the other. (Read again at the forward merge of 2026-11-11 that brought main
// in, 6c288b5434a86dad0d78930118888cddb9661949: main declares 62, which is INFRA-2551's entry
// above and which cybermonklabs/legion#3522 declared before it merged, and the open pull requests
// of this repository declare 60 (#2539, #2731, #3511 and #3519), 61 (#2230), 62 (#2733 and
// #3573) and 63 (#3534), none of them carrying these attributes, so this change keeps 64, the
// number above them all. That reading is of this file at refs/pull/<n>/head for every open pull
// request, not of a file list: GitHub cuts a pull request's file list short, so one that changes
// more files than the page holds answers without this path, which is how #2539 was missed when
// this entry was first written. As
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
// 68: INFRA-1282 -- a person may decline to answer an ask, and `answeredBy` on the `waitingOn` of
// an `awaiting_person` root in `GET /legion/v1/state` takes a ninth value, `declined`
// (`record.AnsweredByDecline`): the person closed an ask of the root — the one the wait names or
// any other — rather than answer it, so nobody will answer that question. The daemon then starts
// the root's architect again, as a person's answer starts it, with a task that names the declined
// ask and carries the four sentences Dispatch writes for the asking agent and the person's own
// note. The value is the whole of the wire change: no field is added, no field is removed, and a
// root nobody declined an ask of is served exactly as before.
// `packages/contracts/src/legion-api.ts` lists the nine values in `LEGION_WAIT_ANSWERS`, and
// that list is a `z.enum`, which refuses a value it does not name rather than stripping it like
// an unknown key: a plugin at any earlier contract reading a declined root's state would refuse
// the whole body, so the bump keeps the two apart from here on. It does not keep them apart on the way back: the
// migration is forward-only and the answer stands on the root's own row, so an older daemon
// binary put back on this schema serves `answeredBy: "declined"` from the rows this release
// wrote, and the plugin at the earlier contract it is gated to then refuses the whole
// `GET /legion/v1/state` body and not the one root that declined. The daemon ships store migration
// `20261111021610_ask_declined.up.sql` with it, which adds the three columns the decline is kept
// on (`root_asks.declined`, `declined_note`, `declined_words`) and widens the answer check to
// the ninth value; an older daemon binary reads that schema without them and resumes such a root
// through `resumeTask`'s default arm, so the root still starts and only Dispatch's words are
// lost. (This entry first took 60, when the base carried 59, then 65 at the forward merge of
// 2026-11-11 that brought main in, and 68 here. main declares 64, the entry right above this
// one, and a reading of `const DaemonAPIVersion` at refs/pull/<n>/head for each of the 126 open
// pull requests of this repository on 2026-11-11 gives 64 (#2404, #2461, #2603, #3577), 65
// (#2731, #3519 and this one), 66 (#3534) and 70 (#3573), so this change takes 68, which no open
// head declares and which this tree published as its own. That reading is of this file at
// refs/pull/<n>/head for every open pull request, not of a file list: GitHub cuts a pull
// request's file list short, so one that changes more files than the page holds answers without
// this path. Read the base's own number and every open pull request again at the next merge
// rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// 73: INFRA-2712 -- the stale-plugin lane asks its worker nothing, and what it has left to do is
// readable. `api.State` gains `claimsToRefresh`, how many claims the lane still has to relaunch
// onto this daemon's plugin: a claim with a recorded session, whose process holds a capability,
// whose recorded plugin contract is not this number, that is in no pod update round, and whose
// launch installs a plugin — the set the daemon's own pass walks (`internal/daemon`'s
// refreshStalePlugins), counted where that rule lives and carried through the body the route
// then cuts to its caller. It is always a number and never null, 0 when the lane has nothing
// left, which is every moment outside a wave; on a daemon that can relaunch onto no plugin at
// all it stands at what that lane cannot act on, beside the Error line such a daemon already
// writes for each of those claims. `wakesWaiting` alone could not say it: the lane now holds at
// most one minute of the pacing at a time (`supervise.RefreshGate`), so its waiting wakes are
// what it has been admitted, never what it has left — at the INFRA wave of 2026-11-10 one
// reading counted 395 live processes on the old contract against about 600 of the lane's wakes,
// and nothing in the body reconciled them. A strict reader before 73 refuses a state carrying
// the key, 0 as surely as a number, so for the one window the pairing leaves open — a pod of the
// contract before this one still being replaced — the route serves a caller whose pod records a
// contract below 73 a body with no `claimsToRefresh` key at all (api's stateFor,
// withoutClaimsToRefresh), as it serves a caller below 62 the workers without their counted
// moves back, one below 58 the issues without their place in the line, one below 57 the claim
// views with no `step`, one below 56 the stranded report without its retired entries, one below
// 55 the issues without the gated steps, one below 53 a body with no `wakesWaiting`, one below
// 51 the issues without the admission's trees and each issue's tree, one below 49 without the
// memory draw and one below 46 the claim views without their update rounds; the ten compose. The
// key is left off that body rather than zeroed, which is why
// `packages/contracts/src/legion-api.ts` declares `claimsToRefresh` optional, as it declares
// `wakesWaiting`. That caller is the only one the key is left off for. A caller the daemon
// cannot place in a pod — the operator's `legion state`, a tmux pane — is served the count, and
// so is a caller in a pod whose claims record no contract at all, which the route can place in
// no band and serves the state untouched (stateFor, podContract). A pod whose claims could not
// be read is no case here: the claims are the state's own snapshot, so a read that fails refuses
// the whole request before the route cuts the body.
// The same change moves the wire's wake kinds. `plugin-task`, the update task the stale-plugin
// round queued a running session, is gone with that round: the lane relaunches a stale process
// at one paced wake and asks its worker nothing, and no daemon from 73 on opens such a round or
// sends such a task. The relaunch the lane's own gate admits waits under `plugin-refresh`
// instead (`supervise.WakePluginRefresh`), a kind no other launch carries, so a reader of
// `wakesWaiting` tells the refresh of a fleet from the launches that start a tree's work; the
// relaunch a stale registration raises keeps `launch`, being one claim's own launch. That half
// needs no shaping of its own, because the schema leaves `kind` an unconstrained string, and
// every reader of the kinds — `packages/contracts/src/legion-api.ts`, `docs/kubernetes.md`, the
// two site guides and `scripts/wake-windows.py` — names the new one. This change adds no store
// migration. (This entry has taken four numbers, each read the same way and each recorded here
// rather than in a memory: 60 when the base carried 59; 69 at the forward merge of 2026-11-11
// that brought main in at 3956a7a66c10d7d512ad35ab866bd00018dd2109, where main declared 64 and
// the open heads declared 65 (#2731), 66 (#3534), 67 (#3519), 68 (#3511) and 70 (#3573); 69
// again at the second merge of that day, 287919e79112d2f1fbe85b74b2a5e37471685ea0, which brought
// in the 68 above and left 69 still free; and 73 at 16:40Z that day, because
// cybermonklabs/legion#3519 had moved onto 69 in a forward merge of its own and two heads
// declaring one number is a refusal for whichever merges second. This tree moved rather than
// that one because this tree had a round open, where a number taken at READY costs a tester
// round. The reading at 16:40Z, of `const DaemonAPIVersion` at refs/pull/<n>/head for each of
// the 124 open pull requests and never of a file list, gives main 68 and, above it, 69 (#2539,
// #3519 and this one before the move), 70 (#3573), 71 (#3577) and 72 (#3534), so 73 is the
// lowest free number above the base. Read the base's own number and every open head again at the
// next merge rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// 75: INFRA-3083 -- a claim says why it failed, and a held issue says which phase its hold
// stopped. A claim view of `GET /legion/v1/state` — a worker's, an architect's — carries
// `failure` once the daemon holds a cause and a sentence for that claim: `cause`, one of the
// seven words of `supervise.FailureCause` (`launch_refused`, `died`, `never_registered`,
// `stale_plugin`, `prompt_retires_exhausted`, `provider_error`, `unrecorded`); `detail`, the
// sentence the site that failed the claim wrote beside it, which is what the daemon kept until
// now; `at`, the moment it failed, absent for a claim whose record carries none; and
// `attempts`, the four counts it failed with, which the limits in `settings` are read against.
// The claim of the operator routes (`POST /legion/v1/operator/claims` and
// `…/{token}/deliver|suspend|resume|stop|retry|forget-session|close`, and the listing behind
// `legion claims list`) carries the same `failure`. Each issue of the state carries `heldFrom`,
// the phase the hold took it out of, beside the `holdReason` it already had; unlike the reason,
// which is suppressed while the issue's tree lingers, the phase is shown whatever the tree is
// doing, so every issue at `phase: held` names the phase that stopped. One sentence covered
// four events until now, so an agent reading a failed claim could not tell a dead process from
// an agent that never came up, and no surface said which phase a hold ended.
//
// Beside those two keys the claim view carries five more facts of the claim record, each absent
// where the record holds none: `servingGeneration`, the run of the last confirmed delivery the
// claim retired, which says whether the work it serves is of this generation; `updateReasons`,
// why the daemon must replace the pod, which the worker answering an update round is told to act
// on; `providerErrorRun`, the run of provider-error turns, which bounds nothing and which the
// project's controller is told of once at six, so the number behind that telling stood on no
// surface its reader could reach; `pendingTask`, the task the claim holds reduced to five facts
// — that one is held, its phase, when it was queued, how many sends failed and the last send
// error — and never its text, which the operator route goes on serving whole; and
// `pendingCause`, the cause of the death whose relaunch is in flight, which an architect reads
// while `failure` is still empty. Each issue carries two more: `lingerUntil`, when its linger
// runs out, so an absent `holdReason` stops meaning either no reason or a tree that lingers; and
// `ancestry`, the Dispatch parent chain a labelled root is held under, which reached no surface
// at all until now.
//
// A tenth key stands beside those nine, and it is the mark of the narrowing itself: `contract`,
// beside the state's `daemon` block, with `daemon`, the contract this binary declares and
// served the body at, and `caller`, the contract the claims of the caller's own pod record,
// absent for a caller the daemon places in no pod. The operator's `legion state --json` is
// served whole and narrowed by nothing, and carries the mark with `daemon` alone, which is how
// the one reader that is never narrowed says which contract its answer was built at; a caller
// at or above this contract carries both numbers; and a caller below it carries no `contract`
// key at all (`withoutRecordFields`), because a key added to a narrowed body is exactly what
// the narrowing exists to prevent. What that caller is told instead is told to the operator of
// the daemon rather than to the caller: the daemon writes one line to its own log for each
// narrowing it applies, naming the caller, the contract that caller's pod records, the contract
// this daemon declares and what that arm dropped (`internal/api`'s stateNarrowings), so a key
// withheld no longer reads as a key the daemon holds nothing for. The mark reaches no agent of
// a deployment the day this ships, since every live pod records a contract below this number;
// it reaches the operator at once, and each agent as its pod moves onto a plugin that declares
// this contract.
//
// The nine are one decision rather than nine: `internal/api/recordfields.go` stands every field
// of `supervise.Claim` and of `record.Issue` in one of three columns — served to the agent
// surfaces, the operator route's alone, or withheld — each with its reason as data, and
// internal/projection's TestEveryRecordFieldIsServedOrExcluded fails naming a field that stands
// in none of the three, so a field either record gains later is decided rather than forgotten.
// `packages/contracts/src/legion-api.ts` names all nine on the answer side, and that schema
// strips a key it does not name rather than refusing it: a plugin built after
// sjawhar/legion#2788 and before this contract loses the keys, not the body. The daemon keeps
// the two apart anyway — a caller in a pod whose plugin declares a contract below 75 is served
// the state with none of them (`recordFieldsContract`, `withoutRecordFields`) — because a
// plugin older than that strict-to-stripping change refuses a body carrying a key it does not
// name. The daemon ships store migration `20261111212544_claim_failure_cause.up.sql` with it,
// which adds the three columns the record is kept on (`claims.failure_cause`,
// `claims.failed_at`, `claims.pending_cause`) and backfills the cause of every claim the store
// holds a failure sentence for, whatever state it stands in, from the sentence it already
// wrote, `unrecorded` where that sentence names no one event and a null `failed_at` throughout,
// since the release this upgrades from recorded the moment nowhere.
//
// An older daemon binary put back on this schema names those columns in no statement and serves
// no `failure` and no `heldFrom`, so those keys go and every other key of the body stands; what
// it does not leave alone is the record and the outbox. It clears `failure` at every launch and
// leaves `failure_cause` and `failed_at` where they are, so a claim it relaunched, or failed
// again under a sentence of its own, holds a cause from one failure beside the sentence and the
// moment of another; `api.FailureViewOf` is keyed on the cause and the sentence together, which
// drops the first of those two states and cannot tell the second from an ordinary failure. And
// a `held` or `worker-died` outbox row carrying `record.Notice.Failure`, and an escalated `held`
// row carrying `record.Notice.Escalation`, is a payload that binary's decoder refuses
// (`record.DecodeOutboxPayload`, `DisallowUnknownFields`, which refuses a row for any key it does
// not know): no notice row's failure is permanent to its outbox, so the row is retried on its
// backoff, logged `outbox row failed` each time, and the hold it carries reaches neither the
// architect nor the controller while that binary runs — nothing else is held behind it, the
// board lane ordering no rows, and a daemon rolled forward decodes and delivers it. An operator
// rolling back lets the outbox drain first, or expects those notices to wait for the roll
// forward.
//
// (The number is a reading of `const DaemonAPIVersion` at refs/pull/<n>/head for every open
// pull request of this repository, not of a file list: GitHub cuts a pull request's file list
// short, so one that changes more files than the page holds answers without this path. The
// reading of 2026-11-11 gave the base 68, the entry two above this one, and the open heads 69
// through 74, so this change took 75, the lowest number no open head declared. It was read
// again at the forward merge of 2026-11-12 that brought main in at
// 54ca4973098de32bb76cda2fdfd2d5f75fed3733, which carries the 73 right above: the base declares
// 73 and the open heads above it declare 74 (#3575), 76 (#3718), 77 (#3677), 78 (#3708), 79
// (#3712) and 80 (#3730), so 75 is still the lowest free number above the base and this change
// keeps it. Read the base's own number and every open pull request again at the next merge
// rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// 85: INFRA-2554 (cybermonklabs/legion) -- every issue of `GET /legion/v1/state` that is not
// working now carries one rendered answer, `why` (`api.WhyView`): its `state` —
// `claim_stopped`, `waiting`, `blocked` or `unlisted` — and the `reason`, the sentence a person
// reads. It is present only for an issue the record holds in the workflow whose sentence
// renders something, so an issue at work carries no such key, and it sits on the issue beside
// `waitingLine` and never inside it, because an issue whose claim has stopped can stand in the
// line or outside it — such a member carries both keys, and reads `claim_stopped`, the state
// that wins where both hold — and the answer is the same answer for every issue that carries
// one. Beside it `api.WaitingLineView` gains the waiting line's own fields, for a program that
// acts on the line rather than reading the sentence: `part` (`returned` or `starting`) and
// `kind` (`returning`, `answered`, `queued_root` or `first_phase`), the member's `partPlace`
// and its part's `partLength`, `holdsSlot` — whether it already holds one of the cap's slots,
// written `false` for a member that does not — `since` for a member of the returned part,
// `rootRank`, `ownRank` and `rootKey` for a member of the starting part, and `reason`, that
// member's own rendered sentence. Each is absent where the member has no fact for it, so a
// returned member names no rank and a starting member no `since`. The two halves take one
// number because they are one answer read two ways and one pass of the projection renders both
// from the same facts (internal/projection), so no caller stands between them.
// `GET /legion/v1/state` authenticates nobody, so a caller from a pod whose claims record a
// contract below 85 is served every issue without `why` and every member of the line with its
// `place` and its `length` alone (api's stateFor, withoutWhyAndLineFields), which is the body
// 58 describes, as a caller below 75 is served it without the nine keys the decided claim and
// issue records added and without the `contract` mark beside the daemon's block
// (withoutRecordFields), as a caller below 73 is served it without `claimsToRefresh`, one
// below 62 the workers without their counts of the moves back, one below 58 the issues with no
// `waitingLine` at all (withoutWaitingLine), one below 57 the claim views with no `step`, one
// below 56 the stranded report without its retired entries, one below 55 the issues without
// the gated steps, one below 53 a body with no wakes waiting, one below 51 the issues without
// the admission's trees and each issue's tree, one below 49 without the memory draw and one
// below 46 without the claim views' update rounds; the twelve compose, and every reader from
// 85 on takes these fields as optional. A plugin from 58 through 84 refuses none of them, as a
// plugin before one of the earlier contracts above refuses its own: every answer-side schema
// of `packages/contracts` has stripped a key it does not name since sjawhar/legion#2788, which
// that package's own test holds it to, so such a plugin reads this state and sees neither the
// answer nor the line's fields. The strip is still what serves it: a caller that did not ask
// for these fields is not given them, and a plugin built before #2788 is strict and would
// refuse the whole body for one of them. Nothing else of this change is on the wire: the
// record and the projection fill the two shapes above and add no route, and the sentence each
// renders is text a person reads rather than a shape a reader parses.
// (This branch took 76 while main held 68, took 84 at the forward merge of 2026-11-12 that
// brought main in at e98fc56e, which declares 73, and takes 85 now at the second such merge,
// which brought main in at 9009dc1d, which declares the 75 right above. A reading of `const
// DaemonAPIVersion` at refs/pull/<n>/head for each of the 143 open pull requests of
// cybermonklabs/legion at 2026-11-12T13:00Z gives, above 75: 76 (#3830), 77 (#3677), 78
// (#3708), 79 (#3712), 80 (#3730), 81 (#3659), 82 (#3573), 83 (#2539) and 84 (#3534 and this
// one before the move). #3534 declares 84 as well, so this branch moves off it; 76 through 83
// are taken by the heads named above; and 85 is the lowest free number above the base and
// above every open head, which is the number to take, because a release declaring any of 76 to
// 84 without these shapes would land first and force the renumber then, which is what
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// records. That reading is of this file at refs/pull/<n>/head for every open pull request, not
// of a file list: GitHub cuts a pull request's file list short, so one that changes more files
// than the page holds answers without this path. Read the base's own number and every open
// pull request again at every forward merge rather than from this line.)
//
// 86: dispatch://INFRA-714 -- the daemon is an event-driven state machine, and its strictly parsed
// shapes change with it. The phase enum loses `awaiting_merge` (both places it is spelled: the
// issue's phase and a backward move's target), since the merger merges in `merging` and the daemon
// closes the issue when it reads that merge. `api.PullRequestView` loses `checksVerdict` — no CI
// verdict moves an issue at all on this contract: the daemon names every check it read to the
// tree's roles and judges none (dispatch://INFRA-714/spec R1), so it keeps no verdict of its own,
// the fix-attempt count having gone already at main's 59 — and gains `mergeState`, GitHub's
// own merge state for the head in GitHub's lowercase spelling (`clean`, `unstable`, `has_hooks`,
// `blocked`, `dirty`, `behind`, `draft`, `unknown`), absent until the daemon's read of the pull
// request has run. `api.PhaseView` gains
// `pass`, the passes the phase has made through its stretch of the workflow, beside the `rounds`
// only a review requesting changes counts. Every claim view, a phase worker's and the operator's,
// gains `lastTurnStart` and `lastTurnEnd`, each absent until the claim's agent has begun or ended
// a turn. `HandoffCompleteRequest`
// drops `ready` and `HandoffCompleteResponse` is an empty object: the flag and the `note` went
// with READY and the daemon's merge, since the merger merges and the daemon submits none.
// `mergeState`, `pass`, `lastTurnStart` and `lastTurnEnd` are keys a plugin of an older contract
// does not name, which one built before sjawhar/legion#2788 refuses the body for and one built
// after strips, so the boot gate and `legion probe-image` are what refuse the mixed pair.
// (This branch took 42, then 43 when main landed the consumer-lag state at 43;
// main landed the background-task contract at 44 and the Dispatch handoff records at 45, and
// INFRA-484's sjawhar/legion#2538 takes 46, this branch then took 47, and main's #2642 landed
// the idle release at 48 and INFRA-1101's memory group at 49; this branch then took 50, and
// main reached 51, 52, 53, 54 and, with INFRA-1272's gated steps, 55, which took this branch to
// 56, and INFRA-1480's retired stranded claim landed 56 on main at the next forward merge, so
// this branch takes 57, the number above every number main declares, read from main at that
// merge rather than assumed, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says. main's #2958 then landed INFRA-1274's claim step record at 57 as well, which took this
// branch to 58; main's #2989 landed INFRA-1100's waiting line at 58, which took it to 59; main's
// #2780 landed INFRA-956's end of fix-attempt counting at 59, which took it to 60; and main's
// #3522 has since landed INFRA-2551's counts of the moves back at 62, #3521 INFRA-312's launch
// spend attributes at 64, and #3511 INFRA-1282's declined ask at 68. The number has to be above
// every number main declares and clear of every number an open branch has claimed, and the
// entries below carry those releases' shapes with this one. main's #3536 has since landed
// INFRA-2712's count of the claims the stale-plugin lane has left to relaunch at 73, #3662
// INFRA-3083's decided claim and issue records at 75, and #3718's one answer for every issue that
// is not working now at 85, the entry right above this one. Read at the forward merge of
// 2026-11-12 that brought main fbe970eef8 in, at 15:53Z: main declares 85, and 86 is the number
// above it this change takes. That read missed an open head already declaring 86 (#3575, which
// has since moved to 87), so 86 was not free when it was taken, and other open heads may declare
// it now. A number two heads share is a refusal
// for whichever merges second, and a census taken before a merge rather than at the head it
// pushes cannot see the heads that moved meanwhile: 81 was lost that way on this branch, and
// every census since is taken after the merge, at the head being pushed. This branch lands
// second every time, so it
// takes the free number at the merge that lands it, written in each of the four sites that hold
// the number — this file, the contracts fixture and the
// `pi-legion` and `claude-envoy` manifests — since
// `.github/scripts/check-daemon-api-contract.sh` refuses a change that touches this file and
// declares a number its base already carries. Read main's own number, and the ledger, again at
// that merge rather than from this line.)
//
// Also at 86: dispatch://INFRA-714 -- every turn a session ends is reported to the daemon by the producer
// that ended it, over `POST /legion/v1/claims/turn-outcome`: `{claimToken, sessionId, secret,
// generation, sequence, outcome, message}`, authenticated by the registration's secret as the
// ready and the exit are, answering `{refusedRun, failed}`. `sequence` is the producer's own count
// of the turns it has ended at that generation, from 1, and `outcome` is `clean` or `refused`,
// `message` the provider's words for a refusal. The daemon keeps the newest reports per claim,
// keyed by (generation, sequence), derives the trailing run of refused turns from them on every
// report, and fails the claim once that run reaches `launch_failure_limit` — the fault that left an
// implementer's every turn refused by its provider with its claim never failing. A plugin built
// before 86 reports no turn, so its session's refusals are counted by nobody, and a plugin from
// this contract posting to a daemon before 86 is answered "no route" for every turn it ends; the
// boot gate and `legion probe-image` refuse the mixed pair. `legion claims retry` gains the
// optional body `{freshSession}` (`--fresh-session`), which starts the claim in a new session
// rather than resuming the recorded one; a claim the provider refused out of its budget takes one
// whoever retries it. That retry also reaches a phase worker an operator stopped, which is
// retired: the route starts the worker's phase again — restoring a held one, as the architect's
// retry_or_escalate does — so the session it starts, new or kept, is given that phase's task.
//
// 87: INFRA-3219 (cybermonklabs/legion) -- a claim whose launch the runtime refused because the
// shared public leak check could not be installed in the role's pod says so in a field of its
// own: `leakCheckCause` on `api.ClaimView` (every claim view of `GET /legion/v1/state`, and so
// `read_state` and `read_record`) and on `api.OperatorClaim` (`GET /legion/v1/operator/claims`
// and every operator route answering a claim, which is where the controller's claim is read),
// the one-line sentence the refusal carries, beginning `the shared public leak check could not be
// installed: ` (`leakguard.RefusalPrefix`). It is the outcome of the claim's latest launch try
// (`supervise.Claim.LeakCheckCause`): present from the first try the check refused and across the
// backoff after it, absent once a try starts its process or fails for another reason, and
// cleared with the failure record by the retry that revives a failed claim. It sits beside
// `failure` rather than in it: a claim that runs out of launches on it fails as before, with
// `failure.cause` `launch_refused` (or `died` where a death's relaunch was refused) and
// `failure.detail` `launch failures ran out: <the error>`, so the closed set of cause words and
// `failure-causes.json` do not change. The daemon ships store migration
// `20261112180548_claim_leak_check_cause.up.sql` with it, which adds `claims.leak_check_cause`
// with the empty default and backfills nothing. `GET /legion/v1/state` authenticates nobody, so a
// caller from a pod whose claims record a contract below 87 is served every claim view without
// the key (api's stateFor, withoutLeakCheckCause), composed with the twelve narrowings 85
// describes; a plugin from 85 on refuses none of it, since every answer-side schema of
// `packages/contracts` strips a key it does not name, and the strip is what keeps it from a
// caller that did not ask for it. The operator routes are not narrowed: their reader is the
// `legion` built with this daemon, whose `legion claims list` line ends with the sentence quoted,
// or `-` where there is none. Read the base's own number and every open pull request again at
// every forward merge rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.
//
// 94: INFRA-2800 (cybermonklabs/legion) -- a phase entry is a numbered fact of the record, and
// each issue of `GET /legion/v1/state` carries the two numbers that say whether its role holds
// the work that entry names: `phaseEntry`, the entry the role of the issue's current phase
// stands on (`record.PhaseRow.Entry`), and `givenEntry`, the entry of the last task that role's
// claim was queued (`supervise.Claim.GivenEntry`). The daemon sends the entry's own work to a
// role whose given number is below the entry its row names, on every admission pass and on a
// sweep of its own each minute, so the two numbers are equal for a role holding its work and
// differ for one the daemon has still to reach; `legion state` names every issue where they
// differ, with the role, in a line of its plain output. Both keys are absent together for an
// issue whose current phase names no phase worker role — a root at admitted whose design gate
// has never opened, which the sweep passes over — and each is absent where it is 0, so an issue
// whose role the daemon holds no claim row of carries `phaseEntry` alone. They are keys of the
// issue and not of `api.PhaseView` because a `phases` row exists for a role whose claim row does
// not, where `PhaseView.Claim` is a required value no reader parses empty.
// `GET /legion/v1/state` authenticates nobody, so a caller from a pod whose claims record a
// contract below 94 is served the issues without both keys (api's stateFor,
// withoutPhaseEntries), as a caller below 87 is served every claim view without
// `leakCheckCause` (withoutLeakCheckCause), one below 85 every issue without the one rendered
// answer `why` and every member of the admission line with its `place` and its `length` alone
// (withoutWhyAndLineFields), one below 75 the claim views and the issues without the nine keys
// the decided record added and the body without its `contract` mark, one below 73 a body with
// no `claimsToRefresh`, one below 62 the workers without `backwardMoves`, one below 58 the
// issues with no place in the admission line, one below 57 the claim views with no `step`, one
// below 56 the stranded report without its retired entries, one below 55 the issues without
// the gated steps, one below 53 a body with no wakes waiting, one below 51 the issues without
// the admission's trees and each issue's tree, one below 49 without the memory draw and one
// below 46 without the claim views' update rounds; the fourteen compose, and every reader
// from 94 on takes both keys as optional. A plugin from 58 through 93 does not refuse them, as
// a plugin before one of the earlier contracts above refuses its own: every answer-side schema
// of `packages/contracts` has stripped a key it does not name since sjawhar/legion#2788, which
// that package's own test holds it to. The strip is still what serves such a caller: one that
// did not ask for the numbers is not given them, and a plugin built before #2788 is strict and
// would refuse the body. The rest of the change moves no shape a caller reads: the entry and
// its task are columns of the `phases` and `claims` records, the start row carries the entry it
// hands, and the completion route compares the entry the worker took with the entry its row
// names, each of them the daemon's own lane and no route. The code that refusal carries is
// renamed in this same contract, from the minted assignment's code to `HANDOFF_STALE_ENTRY`,
// because what it refuses is now a completion of an entry behind the one the role's phases row
// names (or above it): the skills a worker reads, both plugins and this daemon's own docs name
// the new code, and the boot gate holds every plugin to this contract, so no worker is refused
// a code its tool's description does not name. The words that refusal tells the worker to act
// on are one text with the sentence the `legion` tool's description gives for that code,
// which `packages/contracts/fixtures/daemon-api/stale-entry.json` holds the two sides to.
// (This entry took 77 against a base that declared 68 and held 77 through three forward merges;
// it took 86 at the fourth such merge, 87 at the fifth and 93 at the push of that fifth merge,
// and 96 at the sixth, of 2026-11-13, which brought main in at 0b297211 —
// dispatch://INFRA-3219's own #3939, whose `const DaemonAPIVersion` is the 87 right above, so 87
// is the base's now. It takes 94 at the push of that sixth merge. The reading behind 96, of
// `const DaemonAPIVersion` at refs/pull/<n>/head for every one of the 139 open pull requests of
// cybermonklabs/legion at 2026-11-13T04:20Z, found 96 free; #2731 took it between that reading
// and the next. That next reading, of the same constant at refs/pull/<n>/head for every one of
// the 140 open pull requests at 2026-11-13T05:54Z, gives above the base's 87: 88 (#3730), 89
// (#3659), 90 (#3830), 91 (#3519, #3573 and #3712), 92 (#3715), 93 (#3937), 96 (#2731, and
// this branch's own pushed head), 97 (#3575) and 98 (#3708), and 87 itself at 7 heads; no head
// declares 94 or 95. So 94 is the lowest free number above the base, which is the number this
// entry takes: a release declaring any of 88 to 93 without these shapes would land first and
// force the renumber then, which is what
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// records. The reading of 2026-11-12T22:45Z found 93 free and #3937 took it in between, and the
// reading of 04:20Z found 96 free and #2731 took it in between, which is why the census is
// taken at the head being pushed and never before the merge. That reading is of this file at
// refs/pull/<n>/head for every open pull request, not of a file list: GitHub cuts a pull
// request's file list short, so one that changes more files than the page holds is read as
// well rather than passed over. Read the base's own number and every open pull request again
// at the next merge rather than from this line.)
//
// 100: INFRA-3036 (cybermonklabs/legion) -- the waiting line's `kind` (`api.WaitingLineView`) on
// `GET /legion/v1/state` gains a fifth word, `waiting_on_issue`: an issue the daemon has started
// that gave its admission slot back to wait on another issue — a Dispatch blocker of it that is
// not done, or an open child its own first phase waits for (record.LineWaitingOnIssue). It
// stands in the returned part with `since` the moment its row was taken
// (record.Issue.ReleasedForWaitAt) and `holdsSlot` false, the promotion passes over it while
// the wait lasts, and it takes a row again at the first pass after the wait ends. A plugin from
// 85 through 99, INFRA-714's 86, INFRA-3219's 87 and INFRA-2800's 94 above among them, reads
// `kind` with a `z.enum` of the four words before it, which refuses a word it does not name and
// with it the whole state body, so a caller from a pod whose claims record a contract below 100 is
// served every member of that kind without its `kind` (api's stateFor, withoutWaitingOnIssue),
// which that schema takes as optional; every other field of the member is 85's,
// `leakCheckCause` on the claim views is 87's and narrowed as 87 says, the issues' `phaseEntry`
// and `givenEntry` are 94's and narrowed as 94 says, and a caller below 85 has lost `kind` with
// the rest of them already (withoutWhyAndLineFields). It is the fifteenth narrowing and the last
// row of stateNarrowings, after 94's. Nothing else of this change is on the wire.
// (This branch took 86 at the forward merge of 2026-11-12 that brought main in at fbe970ee,
// which declared 85. main has since landed INFRA-714's 86 at a4c0ff99, so this branch moved off
// it at the forward merge that brought that commit in. A reading of
// `const DaemonAPIVersion` at refs/pull/<n>/head for each of the 144 open pull requests of
// cybermonklabs/legion at 2026-11-12T19:36Z gave, above 86: 87 (#3939), 88 (#3730), 89 (#3519
// and #3659) and 90 (#3575, #3830 and #3937), and this branch took 91. The same reading of the
// 141 open pull requests at 2026-11-12T20:41Z gave 87 (#3939), 88 (#3730), 89 (#3519 and
// #3659), 90 (#3575 and #3830) and 91 (#3937, which had moved up from 90 meanwhile), so this
// branch took 92. main has since landed #3939's 87, INFRA-3219's, at 12ba4e6a, and the forward
// merge that brought that commit in read the 139 open pull requests again at 2026-11-13T05:17Z:
// above 87 they declare 88 (#3730), 89 (#3659), 90 (#3830), 91 (#3519, #3573 and #3712), 93
// (#3937), 96 (#2731 and #3677), 97 (#3575) and 98 (#3708), and no open head declares 92, so
// this branch kept it. main has since landed #3677's 94, INFRA-2800's, the entry right above, at
// 045ee076, which is above 92, so this branch moved off it at the forward merge that brought that
// commit in. The same reading at refs/pull/<n>/head of each of the 140 open pull requests other
// than this one (141 with it) at 2026-11-13T08:16Z gave, above 94: 95 (#3519 and #3712), 96
// (#2731 and #3573), 98 (#3708) and 99 (#3575), and no open head declares 97, so this branch
// took 97, the lowest number above 94 that no open head declared. The same reading of the 140
// open pull requests other than this one at 2026-11-13T09:03:10Z gave, above 94: 95 (#3519 and
// #3712), 96 (#2731 and #3573), 97 (#3937, which had moved up from 93 meanwhile), 98 (#3708) and
// 99 (#3575), so 97 was no longer free and this branch takes 100, the lowest number above 94
// that no open head declares. Read the base's own number and every open pull request again at
// every forward merge rather than from this line.)
const DaemonAPIVersion = 100
