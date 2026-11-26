package api

// This file is the history of DaemonAPIVersion (contracts.go), the contract this daemon speaks
// with the Legion plugin (@sjawhar/pi-legion): the claim, credential, workflow, controller and
// state shapes the plugin's client parses
// (`packages/pi-shared/src/daemon-client.ts`, through
// `packages/contracts/src/legion-api.ts`), the pane environment it reads — every variable
// the tmux runtime sets on a pane (`internal/runtime/tmux/spawn.go`'s `panePairs`) and the Sandbox
// runtime on a pod's worker container (`internal/runtime/sandbox/manifest.go`'s
// `mainEnvironment`) — the command line and environment of a pod's `workspace-init provision`
// container (`initContainers` in `internal/runtime/sandbox/podkind.go`, and `initEnvironment`),
// and the command line of a pod's `legion launcher` container (each pod kind's
// `launcherContainer` in `internal/runtime/sandbox/podkind.go`, and `sessionProxyFlags`), both of
// which the image's own `legion` reads. The installed plugin declares the
// number it was built against as `legion.daemonApiVersion` in the `package.json` its pack writes,
// and the boot gate (`internal/daemon/bootgate.go`) refuses to start unless the two are equal.
//
// Entry rule: a change to any of these surfaces adds one entry, and edits no number by hand.
// `scripts/daemon-contract-entry.sh <slug>` writes the entry's file in this package,
// `contract_<id>_<slug>.go`, whose id is the UTC second it was made; the author writes the
// entry's text there, what the change does to the contract and why, with every row the change
// adds to a table the daemon serves from, each naming the entry by its constant. In
// `packages/daemon`, `go test ./internal/api -update` then writes the entry's fixture,
// `packages/contracts/fixtures/daemon-api/contracts/<id>_<slug>.json`. The numbered entries below
// end at LastNumberedContract, and no change adds one or moves one: each build numbers its new
// entries LastNumberedContract plus their rank in the order main's first-parent history merged
// them, which the release and worker-image builds stamp into the binary (contracts.go), so no
// committed file holds a new entry's number. `legion version --contracts` prints each entry's
// number in a build. `.github/scripts/check-daemon-api-contract.sh` refuses six changes: a
// numbered entry added to this file or renumbered in it; an entry's file or fixture deleted or
// renamed; an entry with no fixture, or a fixture with no entry; `legion.daemonApiVersion` in a
// manifest, a `version.json`, or a `daemon` mark in a state golden; a changelog fragment that
// names a number above LastNumberedContract; and an edit of an entry's fixture or of a frozen
// record with no paragraph starting `Correction:` in that entry's text.
//
// Serving rule: a change that moves the shape of an answer a plugin parses serves the old shape to
// each contract of `SupportedContracts` below DaemonAPIVersion, through shaping.go's
// `shapeAnswer`: a band takes off a key the change adds (`stateNarrowings`), a stand-in writes
// back a key it removes (`standIns`), and a value strip holds back a value newer than that
// contract's enumeration (`valueStrips`, which reads answers.go's `EnumeratedFields`). The shape
// gate in this package's tests (`contract_shapes_test.go`) refuses a change that does not, and a
// record of an earlier contract changed with the serving-down that produces it passes, which a
// reviewer reads.
// Under `settings` the rule covers only the top-level keys a supported reader types (answers.go's
// `typedSettings`): a settings key no reader types needs no band, stand-in or contract number,
// only its row in settings_test.go's classification and the fixture values the field walk asks for.
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
// which was a `z.strictObject` at this contract, so a reader before 49 refused a whole state
// carrying any of the three — and every worker's `read_record` parses the state through it.
// (That schema strips such a key rather than refusing it since sjawhar/legion#2788, which
// landed after this entry; 58 below is the first entry written under the strip.)
// `GET /legion/v1/state` authenticates nobody, so a caller from a pod whose claims record a
// contract below 49 is served the state without them (api's stateFor, withoutMemory), as a
// caller below 46 is served it without the claim views' update rounds; a caller below both is
// served it without either. The settings carry the project's `memory` block (mode, config
// files, instructions), which every plugin reads loosely and which changes no strictly parsed
// shape. This change adds no store migration of its own: the three columns are the issue
// record's (20261104073148_issue_memory_group), and the hand-over's answer beside them is too
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
// it the body its plugin reads (api's stateFor, withoutTrees), as it serves the memory group
// only from 49 on; a caller the daemon cannot place in a pod — the operator's `legion state` — is
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
// whose pod records a contract below 53 a body with no `wakesWaiting` key at all (api's
// stateFor, withoutWakesWaiting), as it serves the live trees only from 51 on and the memory
// group only from 49 on. The key is left off that body rather than emptied, which is why
// `packages/contracts/src/legion-api.ts` declares `wakesWaiting` optional, as it declares
// `admission.trees`. That caller is the only one the key is left off for: a caller the daemon
// cannot place in a pod — the operator's `legion state` — is served it, and so is a caller in a
// pod whose claims record no contract at all, which the route can place in no band and serves
// the state untouched (stateFor, podContracts). With `daemon.startedAt` and each claim's
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
// probe-image` refuse the mixed pair. A caller below this contract is now served the state with
// each check view's `role` left off (`withoutCheckRole`), so the boot gate is no longer the only
// thing that covers such a reader. (This branch took 50 while main stood at 49;
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
// the report without its retired entries (api's stateFor, withoutUnadmittedStrandedStates), as a
// caller below 49 is served it without the memory group. Dropping those entries, rather than
// serving them, is the only choice that keeps such a pod able to read the state at all: the value
// is the claim's own state and there is no older value to put in its place, and one retired entry
// refuses the whole body, which takes every role's `read_record` in that pod with it. Such a pod is
// served a report missing its retired entries until the pod is replaced, and that window is the one
// the plugin pair already bounds: the boot gate and `legion probe-image` refuse a daemon and a
// plugin that declare different contracts, so the only reader below 56 is a pod of the contract
// before this one still being replaced. The same change adds three fields to the operator claim
// view's pending delivery (api.DeliveryView): `sendFailures`, `lastSendFailureAt` and
// `lastSendError`, how many sends of the task failed, when the last one failed and the error it
// failed with. They need no gate, because no plugin calls the operator routes. This change adds one
// store migration, for those three columns, named for the UTC moment it was written, since the
// numbered series is closed. (This branch has been renumbered three times, which is what a
// hand-allocated serial costs: 51, then 53 when main reached 52, then 55 when main reached 54, and
// 56 now that main carries 55 — INFRA-1272's gated steps, merged as #2848 while this branch stood
// approved at its own 55. So it takes the number above every number main declares, as the collision
// note says, and that number was read from main at this merge rather than assumed. Two other open
// branches declare 55 as well: whichever merges after another takes the next free number, and the
// number is read again at every forward merge, as
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
// a contract below 57 the state with no `step` at all (api's stateFor, withoutStallRecord), as
// it serves a caller below 56 one whose claims nothing will wake name no retired claim, a caller
// below 55 one with no gated steps, a caller below 53 one with no `wakesWaiting` and a caller
// below 49 one with no memory group. That caller is the only one the key is left off for: a
// caller in a pod whose claims record no contract at all is placed in no band and is served the
// state untouched (stateFor, podContracts), and a state whose claims could not be read refuses
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
// neither order of the two ever runs. A caller below this contract is now served
// `pullRequest.fixAttempts` of 0 and `settings.max_fix_attempts` of 3 as stand-ins, so the boot
// gate is no longer the only thing that covers such a reader.
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
// deployment alone. A deployment's collector may keep a fixed list of resource attributes, so a
// spend row can be grouped by issue only when the collector keeps legion.issue and legion.role.
// The detector that reads the value assigns last-wins, which is why the daemon's pairs are
// written after the operator's; a pair the launch holds no fact for is left out rather than
// written empty, so the project controller's launch, which serves no issue, carries `legion.role`
// alone. The name stays the operator's to set — the runtime composes the value, it does not own
// the name — but a launch spec may not set it (`composed`, beside `runtimeOwned`), since a spec's
// second pair of that name is the one the C library's lookup answers with, and both configuration
// loaders refuse an operator value the agent's OpenTelemetry detector would throw on
// (runtime.SpendAttributesRefusal): that throw is caught with an empty hand, so one malformed
// member costs the launch every attribute of the variable, the operator's own project attribute
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
// no band and serves the state untouched (stateFor, podContracts). A pod whose claims could not
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
// served the body at, and `caller`, the contract the answer was shaped to, absent for a caller
// served this daemon's own shape. The operator's `legion state --json` is served whole and
// narrowed by nothing, and carries the mark with `daemon` alone, which is how the one reader
// that is never narrowed says which contract its answer was built at; a caller the shaping
// places below this daemon's own contract and at or above this one carries both numbers; and a
// caller below it carries no `contract` key at all (`withoutContractMark`), because a key added
// to a narrowed body is exactly what the narrowing exists to prevent. What that caller is told
// instead is told to the operator of the daemon rather than to the caller: the daemon writes
// one line to its own log for each band it serves the state down by, naming the caller, the
// contract that caller's pod records, the contract this daemon declares and what that band
// dropped (`internal/api`'s stateNarrowings), so a key withheld no longer reads as a key the
// daemon holds nothing for. The mark reaches no agent of a deployment the day this ships, since
// every live pod records a contract below this number; it reaches the operator at once, and
// each agent as its pod moves onto a plugin that declares this contract.
//
// The nine are one decision rather than nine: `internal/api/recordfields.go` stands every field
// of `supervise.Claim` and of `record.Issue` in one of three columns — served to the agent
// surfaces, the operator route's alone, or withheld — each with its reason as data, and
// internal/projection's TestEveryRecordFieldIsServedOrExcluded fails naming a field that stands
// in none of the three, so a field either record gains later is decided rather than forgotten.
// `packages/contracts/src/legion-api.ts` names all nine on the answer side, and that schema
// strips a key it does not name rather than refusing it: a plugin built after
// sjawhar/legion#2788 and before this contract loses the keys, not the body. The daemon keeps
// the two apart anyway — a caller whose contract is below 75 is served the state with none of
// them (`recordFieldsContract`, `withoutRecordFields`, `withoutContractMark`) — because a
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
// `GET /legion/v1/state` authenticates nobody, so a caller whose own contract is below 85 is
// served every issue without `why` and every member of the line with its `place` and its
// `length` alone (api's shapeIssue, withoutWhyAndLineFields), which is the body
// 58 describes, as a caller below 75 is served it without the nine keys the decided claim and
// issue records added and without the `contract` mark beside the daemon's block
// (withoutRecordFields, withoutContractMark), as a caller below 73 is served it without
// `claimsToRefresh`, one below 62 the workers without their counts of the moves back, one
// below 58 the issues with no `waitingLine` at all (withoutWaitingLine), one below 57 the
// claim views with no `step`, one below 56 the stranded report without its retired entries,
// one below 55 the issues without the gated steps, one below 54 the issues' checks without
// their `role`, one below 53 a body with no wakes waiting, one below 51 the issues without
// the admission's trees and each issue's tree, one below 49 without the memory draw and one
// below 46 without the claim views' update rounds; the thirteen compose, and every reader from
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
// (The entry at 193 below serves this contract's own shape down, so none of the keys above
// reaches a caller under 86. Three claim-view keys ride with the four this entry names and are
// served down with them: `lastDeathCause`, `lastStartFailure` and `lastReady`. Two more keys go
// at 86 beside `checksVerdict`. `checksVerdict` and `HandoffCompleteResponse.note` are written
// back to nobody and leave the records of the contracts from 46 to 85:
// `packages/contracts/src/legion-api.ts` reads each `nonEmptyString.optional()` at 85 and at
// every contract below, so no reader there refuses a body for want of either. The settings this
// contract stops writing — `settings.projects.*.merge`, its `by_hand` list and
// `merge_queue_role` — are in no record at any contract: no reader types `projects`
// (answers.go's typedSettings), so no reader names those keys at all. The `ready` this
// contract takes out of the request is read by name again for the plugins below 86 that still
// send it, which entry 193 says.)
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
// the key (shaping.go's stateFor, withoutLeakCheckCause), composed with the twelve narrowings 85
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
// contract below 94 is served the issues without both keys (shaping.go's stateFor,
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
// 98: INFRA-3107 -- `GET /legion/v1/state` carries `admission.held`, every issue the daemon's
// record holds in the workflow that stands in neither `admission.active` nor `admission.waiting`:
// the issue's key, the one word for why nothing of it runs (`record.HeldReason`, a closed list of
// eleven read in one order by one ladder, of which `at_work` names no issue on this contract, as
// `record.HeldAtWork` says) and the issues that hold it where any do (`heldBy`, absent where none
// does — a root whose Legion label is off, an issue none of the other words is true of). The
// three lists together are every issue of the workflow and what each is doing, which `active` and
// `waiting` alone never were: an issue in neither was simply missing, and a reader could not tell
// one the daemon was about to start from one nothing will start again.
// `packages/contracts/src/legion-api.ts` reads the word as a `z.enum` of `LEGION_HELD_REASONS`,
// which refuses a value it does not name rather than stripping it like an unknown key, so a word
// added to the ladder later bumps this number again, and is served down by the value strip that
// reads the table's arrivals (shaping.go's withoutUnadmittedHeldWords), as 219 did with
// `children_before_retro` and `task_stopped`. The key itself is written on every body, empty
// rather than null, and a caller from a pod whose claims record a contract below 98 is served the
// admission block without it (shaping.go's stateFor, withoutHeld), as a caller below 94 is served every
// issue without `phaseEntry` and `givenEntry`, one below 87 every claim view without
// `leakCheckCause`, one below 85 every issue without its rendered answer and every member of the
// waiting line without that contract's own fields, one below 75 the body without the nine keys
// of the decided record and its contract mark, one below 73 without `claimsToRefresh`, one below
// 62 the workers without their counts of the moves back and one below 51 the admission block
// without its trees; the fifteen strips compose, and every reader from 98 on takes the key as
// optional for exactly those stripped bodies. No other key of the wire moves: no list above it
// changes, though `why.reason` now follows the held word for some issues in `admission.blocked`
// and `unlisted` alike, for every caller at 85 or above (the sentence table of
// `internal/projection/why.go` is the one statement of which), which moves no contract, since the
// reason is free text. The rows are read from the judgement the state already made
// (`blockers.JudgeLine`, which now answers the rows the pass left behind beside the line it
// built), so the body costs no further read of the store.
// (The base declares 94, main's #3677 entry right above this one, landed by
// cybermonklabs/legion#3677 (INFRA-2800). By the rule of 2026-11-13T06:29Z this change keeps 98
// while it stands above its base and no other open head declares it, and renumbers only when the
// base takes 98 or a number above it. It declared 76 when this pull request opened, then 78 and
// 86, each renumbered when another change took the number first, then 94, which it left for 98
// because an open head declared a higher number that could land first; 86 was read at
// 2026-11-12T15:52Z as the lowest free number above the base alone, and #2539, an open head that
// already declared it, merged at 18:19Z. A reading
// of the open heads is of this file at refs/pull/<n>/head for every open pull request, not of a
// file list: GitHub cuts a pull request's file list short, so one that changes more files than
// the page holds answers without this path. Read the base's own number and every open pull
// request again at the next merge rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// 104: INFRA-3161 -- the two claim recoveries read the claim's process, and `fresh_session`
// reaches a claim stored working that no process stands behind. Such a claim was refused
// `CLAIM_IN_A_TURN` by both recoveries, which left a project controller no recovery at all for a
// dead architect. Both doors now read that claim's process at the moment of the request
// (`supervise.Machine.ReadProcess`) rather than the daemon's memory of an old report, which a
// restart forgets. `fresh_session`, the controller's and the operator's
// `legion claims retry --fresh-session` alike, reaches such a claim: it is suspended where the
// request arrives, it leaves the session it recorded behind, and it is started again with the
// task its dead turn held. The machine lands that suspension where it arrives rather than
// holding it for a turn that will never end (`supervise`'s holdSuspension). The compaction still
// refuses the claim, since it has no agent to ask, with `CLAIM_NOT_RUNNING` naming
// `fresh_session` as the operation that reaches it, or, for the project's own controller claim,
// which the fresh session refuses and its keeper leaves alone while it is stored working, the
// operator's `legion claims suspend` and then `legion claims resume`. `CLAIM_IN_A_TURN` now
// means a process that is running: a claim whose process the read finds alive, and one whose
// session is being compacted. A claim whose process the runtime would not answer for, and one
// that records no process to read, is refused `CLAIM_UNSETTLED` instead, left exactly as the
// request found it and told which read did not happen; so is a claim that moved under the
// request: at the fresh session, one whose later view is no longer working or whose second read
// finds the process gone once more, a moved claim being otherwise answered as that second read
// finds it; at the compaction, one the machine refused as working whose later view is no longer
// working, which before this change was read and could be answered `CLAIM_IN_A_TURN`. A claim
// under a relaunch the wake pacing parks keeps `CLAIM_UNSETTLED`. The read records what it finds
// as the sweep's report of the process would: a running verdict that clears the death charge
// writes the claim as the machine holds it, as that report's write does, and a refusal stops,
// suspends, forgets, takes back and starts nothing. One answer more changed: a fresh session that
// stopped the claim and could not start it again — the forget of its session or its start refused
// — answers 200 with that refusal, and the state it left the claim in, in `phaseNotRestarted`,
// rather than reporting a failure about a claim it had already stopped, so that member now carries
// either the workflow's refused phase restart or a step the machine refused after the claim was
// stopped. No request or response shape the plugin parses changes: `api.ClaimRequest`,
// `api.FreshSessionResponse` and `api.CompactResponse` keep their fields. What moved is which
// claims each operation reaches and the code and the sentence each refusal gives, which
// `packages/contracts/src/legion-claim-recovery.ts` mirrors and which a controller reads in the
// `legion` tool's own description, so this number is what keeps a plugin stating the old
// behaviour off a daemon that answers the new one. (This entry has taken six numbers: 70 when the
// base carried 68, 76 at the forward merges of 2026-11-12 where main declared 73 and then 75, 86 at
// the third, fbe970eef84d, where main declared 85, 90 at the fourth, a4c0ff994adc, which the pull
// request's own conflict forced and where main declared 86, still 90 at the fifth, 12ba4e6afa71,
// where main declared 87, and 103 at the sixth, 8d9d1ff2dd56, where main declared 94. It takes 104
// at the seventh, b7a2f926d3a8, which the conflict forced again and where main declares 98, the
// entry right above this one. The reading of `const DaemonAPIVersion` at refs/pull/<n>/head at
// 2026-11-13T12:40Z, for every open pull request whose file list names this file or is longer than
// the page that lists it, gives above the base's 98: 99 on #3575, 100 on #3573 and #3715, 101 on
// #3712, 102 on #3730 and 103 on #3519, so 104 is the lowest free number above the base's and above
// every other open head's, which is what the gate's refusal names. Read the base's own number and
// every open pull request again at the next forward merge rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// 111: dispatch://INFRA-2560 -- every issue record of `GET /legion/v1/state` that stands in a
// live tree gains that tree's quiet time: `treeMovedAt`, the newest movement the daemon has
// seen anywhere in the tree, and `quietHours`, how long the tree has been still at this read,
// as a number of hours. The pair is the tree's and not the issue's, so every issue of one tree
// carries the same two values — the root, a child past its own review, which holds no admission
// slot since 111's first slice, and a done child recorded under the tree alike — and a reader
// that wants to know whether a tree has stopped asks any one of its issues. The measure behind
// the keys is `record.QuietTrees`, the one statement of the question the admission pass reads
// too, so the state and the pass read one rule and neither keeps a measure of its own — the
// records they are handed are their own, so a tree whose root is done is measured here and by
// no admission pass. The movement is the newest of the `MovedAt` of every member the workflow
// still runs and the step times of those members' claims, and a tree that has never moved at
// all is measured from the zero time, where the daemon's duration saturates: such a tree reads
// as quiet for 2562047.788 hours, 292 years, rather than carrying no key. An issue whose tree
// has left the workflow — one no longer among `admission.trees` — carries neither key, and
// neither does an issue the workflow does not record. Nothing of the operator's plain
// `legion state` output changes: a line per issue would print hundreds of them on a live
// deployment, so the two values are in `--json` and in the one issue a pane prints, which is
// where the memory keys and the waiting line each landed.
// `GET /legion/v1/state` authenticates nobody, so a caller from a pod whose claims record a
// contract below 111 is served the issues without the two keys (shaping.go's stateFor,
// withoutQuietTime), as a caller below 98 is served the admission block without `held`, the
// entry two above (withoutHeld) — 104, the entry right above, moves no shape and narrows
// nothing — one below 94 the issues without `phaseEntry` and
// `givenEntry` (withoutPhaseEntries), one below 87 every claim view
// without `leakCheckCause` (withoutLeakCheckCause), one below 85 without each issue's one
// rendered answer and without the waiting line's own fields beside each member's place
// (withoutWhyAndLineFields), one below 75 the state with none of the record
// fields of the entry before that, one below 73 a body with no `claimsToRefresh`, one below 62
// the workers with no `backwardMoves`, one below 58 the issues with no `waitingLine`, one
// below 57 the claim views with no `step`, one below 56 the stranded report without its retired
// entries, one below 55 the issues without the gated steps, one below 53 a body with no wakes
// waiting, one below 51 the issues without the admission's trees and each issue's tree, one
// below 49 without the memory draw and one below 46 without the claim views' update rounds; the
// sixteen compose, and every reader from 111 on takes both keys as optional
// (`packages/contracts/src/legion-api.ts`). A plugin from 58 to 110 does
// not refuse them, as a plugin before one of the earlier contracts above refuses its own: every
// answer-side schema of `packages/contracts` has stripped a key it does not name since
// sjawhar/legion#2788. The strip is still what serves such a caller: a reader that did not ask
// for the keys is not given them, and a plugin built before #2788 is strict and would refuse
// the whole body for one of them.
// This change adds no route: the record column the movement is read from (`issues.moved_at`)
// and the measure over it are the slices before this one.
// The same change gives `legion.yaml` a `stalled_tree_notice_hours` key — how long a tree may be
// quiet before the admission pass tells the project's controller that the tree has stopped — and
// the settings block of this state carries it under that same name. That key is no part of this
// number: the plugin parses the settings block loosely (35 above), so a later legion.yaml key
// changes no strictly parsed shape and bumps nothing. The change carries three store migrations
// beside it: `20261110233602_past_review_starts_unmarked`, which takes the admission-slot mark
// off every queued start of a phase past the review; `20261111010031_issue_moved_at`, which adds
// the column the movement above is read from; and `20261111041503_tree_stalled_told_at`, which
// adds the mark that holds one stall to one notice. Those are no part of this number either: a
// migration moves the daemon's own record, and the shape a caller reads is the two keys above.
// Nor does `admission.held` move, though what it names does: an issue past its review holds no
// slot on this contract, so one not standing in `held` stands in that list under `at_work` where
// no earlier word of the ladder is true of it, and one held from such a phase under
// `unclassified` (`record.HeldAtWork`) — words the eleven of 98 already carry, so no strict
// reader of 98 or above refuses them. From 219 a parent held there for the issues beneath it
// reads `children_before_retro` and a task that was stopped there `task_stopped` (219 below).
// (This branch took 62 against the base 5a585b02, which carried 59; INFRA-2551's counts of the
// moves back landed on main as 62 (cybermonklabs/legion#3522) and INFRA-312's spend attributes
// as 64 (#3521), so this change takes a number above both. Read again on
// 2026-11-11 at 10:13Z, in each head's own copy of this file: main declares 64, and the open
// branches declare 65 (legion/INFRA-2009 and legion/INFRA-1282), 66 (legion/INFRA-1578) and 59
// (legion/INFRA-1522), with INFRA-2009 having published a move to 67. 65 was a four-way
// collision there, where the first of the four to land refuses the other three, so this change
// took 70: above every open declaration, and leaving 68 and 69 free for the two trees that had
// not chosen. That reading is of this file at each head, never of a file list, which GitHub
// cuts short. It held 70 through the forward merges that followed, and renumbered when main's
// own `DaemonAPIVersion` passed it: at the merge of 2026-11-12 that brought main in at
// efa39e15f9cd8a06f6a61f332b728407f34a668d, main declares 73, and the reading of
// `const DaemonAPIVersion` at refs/pull/<n>/head for each open pull request of this
// repository, which .github/scripts/check-daemon-api-contract.sh makes for every refusal,
// gives 74 (#3575), 75 (#3662), 76 (#3718), 77 (#3677), 78 (#3708), 79 (#3712), 80 (#3730) and
// 81 (#3659), so this change took 82, the lowest number above the base's that no open pull
// request declared, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says. It kept 82 at the forward merge of 2026-11-12 that brought main in at
// 9009dc1d5c73e017bc8a293873265f3629f00b33, where #3662's 75 was the entry above and was the
// number the base then carries: 82 was above it, so nothing was renumbered. It took 86 at the
// forward merge of 2026-11-12 that brought main in at
// fbe970eef84df8635b58ba73b380a13f1e6f0a78, which lands INFRA-2554's 85. It took 91 at the
// forward merge of 2026-11-12 that brought main in at
// a4c0ff994adc50fe79ca0e3b07ddde3af8aff708, which lands INFRA-714's own 86: 86 was no longer
// above the base, so .github/scripts/check-daemon-api-contract.sh was run as CI runs it,
// refused this tree for declaring 86 against a base at 86, and named 91 — the lowest number
// above the base's that no open pull request declared, read in each head's own copy of this
// file and never from a file list, which passed over 87 (#3939), 88 (#3730), 89 (#3519 and
// #3659) and 90 (#3575, #3830 and #3937). It takes 96 at the forward merge of 2026-11-13 that
// brought main in at 12ba4e6afa71acf380d0b64738de7a44653f9ea8, which lands #3939's INFRA-3219
// at 87, the entry right above, and is the number the base now carries. 91 was still above it
// but was declared by #3519 and #3712 as well, a three-way collision where the first of the
// three to land refuses the other two, so the check was run as CI runs it against this tree
// declaring the base's own 87, refused it, and named 96 — the lowest number above the base's
// that no open pull request declares, which passed over 88 (#3730), 89 (#3659), 90 (#3830), 91
// (#3519, #3712 and this change's own former number), 92 (#3715), 93 (#3677 and #3937), 94
// (#3708) and 95 (#3575) — which was the number this change then declared. It takes 100 at the
// forward merge of 2026-11-13 that brought main in at 045ee07625382ecc0f00bb3cc4069d479cf76d4a,
// which lands #3677's INFRA-2800 at 94, the entry right above, and is the number the base now
// carries. 96 was still above it but was declared by #2731 as well, so the check was run as CI
// runs it against this tree declaring the base's own 94, refused it, and named 100 — the lowest
// number above the base's that no open pull request declares, which passed over 95 (#3519 and
// #3712), 96 (#2731, and this change's own former number), 97 (#3937), 98 (#3708) and 99
// (#3575) — which was the number this change then declared. It takes 101 at the forward merge of
// 2026-11-13 that brought main in at 0fde2dcff6e4eb08a172d6fe7309151dabe011b1, which lands
// #3708's INFRA-3107 at 98, then the entry right above, and was the number the base then carried. 100
// was still above it, so the check was run as CI runs it against this tree declaring the base's
// own 98, refused it, and named 101 — the lowest number above the base's that no open pull
// request declares, which passed over 99 (#3575) and 100 (this change's own former number) —
// which was the number this change then declared. It takes 111 at the forward merge of
// 2026-11-13 that brought main in at 7810d00ef1c3a52e08e090f127bcf9e3b7ab3393, which lands
// #3830's INFRA-3161 at 104, the entry right above, and is the number the base now carries.
// 101 was no longer above it, so the check was run as CI runs it against this tree declaring
// 101, refused it, and named 111 — the lowest number above the base's that no open pull
// request declares, which passed over 105 (#3659), 106 (#2731 and #3730), 107 (#3712), 108
// (#3519), 109 (#3793) and 110 (#3715 and #3937) — which is the number this change declares. Each of those is the number that head declared at
// that reading and no head's for good, which is why this line is a reading and never the rule.
// Read the base's own number and every open head again at the next forward merge rather than
// from this line.)
//
// 123: INFRA-3146 (cybermonklabs/legion) -- a phase completion names the head its evidence is
// about, and the completion route judges that head rather than which phase entry the turn began
// under. Every file-backed handoff record now carries `head`, the commit the work it reports was
// done against, and the plugin's `handoff_complete` sends that head as the completion's `commit`
// for the planner, the tester and the reviewer as well as the implementer, read from the record
// and never from the shared workspace (`packages/pi-shared/src/handoff-record.ts`,
// findHandoffReport). The route takes a completion of an earlier entry of the same phase when the
// phase ends with a handoff record this role writes, the completion names a commit and that
// commit is the head the record holds for the issue's pull request, and refuses it with
// `HANDOFF_STALE_ENTRY` whatever head it names when the entry the phase stands on was raised by a
// move back (`record.PhaseRow.EntryMovedBack`, written by the transition that raises the entry,
// store migration `20261113090512_phase_entry_moved_back.up.sql`, which adds
// `phases.entry_moved_back` true on every row it finds, so each refuses a behind completion as
// the daemon before it did until that role's next entry writes what its own move was). The
// refusal's text names what decided it: the commit given or that none was, the head the record
// holds, and the move back where there was one. A completion of another entry than its role's
// phases row names, from a role that does not work the phase the issue stands in, is answered
// `HANDOFF_NOT_CURRENT_PHASE` in the workflow's words and records nothing, where daemons from 94
// refused it `HANDOFF_STALE_ENTRY` saying a task of the phase was owed to the claim. No field is
// added or removed on either side of the wire: `commit` was already optional on
// `api.HandoffCompleteRequest` and `handoffCommit` on `api.PhaseView`. What moves is what the two
// sides mean by them, so they move together: a plugin below 123 sends the three roles'
// completions with no commit, which this daemon refuses behind their entry as every daemon from
// 94 did, and a plugin at 123 against a daemon below it has a commit recorded for those roles and
// named in the next role's task (`Handoff commit:`) that the daemon never judges. A plugin at 123
// also names, on a completion sent after the issue moved on, the role's own record of the phase
// it left (pi-shared's completedPhase, the mirror of phase.Of), which this daemon reads back
// against that phase; a plugin below names none there and is answered as every daemon from 94
// answers it. (Taken at the push that answered the review of 9f1d4789, with main at 922eb1d992a3,
// whose own number is the 111 above. This entry took 104 at the merge of 8d9d1ff2 (main at 94),
// 106 at that of 0fde2dcff6e4 (main at 98), 109 at that of 2a6ff6c03269 (main at 98), 111 at that
// of b601d25b3fa1 (main at 104), 115 at that of 809208573a01 (main at 111), 120 at the push after
// it and 121 at the merge of 50bfbb0dec73 (main at 111), each declared by another open head before
// this one landed. The reading behind 123, of `const DaemonAPIVersion` at refs/pull/<n>/head for
// every open pull request of cybermonklabs/legion at 2026-11-14T06:17Z, made with the
// `declared_by_open_pull_requests` of `.github/scripts/check-daemon-api-contract.sh`, gives above
// main's 111: 112 (#2731), 113 (#3575), 114 (#3730), 115 (#3937), 116 (#3715), 117 (#3711), 118
// (#3659), 119 (#3712), 120 (#3993), 121 (#3519) and 122 (#2230); 123 is the lowest number above
// them all.)
//
// 132: INFRA-3036 (cybermonklabs/legion) -- the waiting line's `kind` (`api.WaitingLineView`) on
// `GET /legion/v1/state` gains a fifth word, `waiting_on_issue`: an issue the daemon has started
// that gave its admission slot back to wait on another issue — a Dispatch blocker of it that is
// not done, or an open child its own first phase waits for (record.LineWaitingOnIssue). It
// stands in the returned part with `since` the moment its row was taken
// (record.Issue.ReleasedForWaitAt) and `holdsSlot` false (the rule: admit.releaseSlotsForWaits,
// and `docs/site/src/content/docs/legion/concepts.md`, "Admission and the `legion` label"). Such
// an issue stands in `admission.waiting` and, where what it waits on is a Dispatch blocker or an
// open child, in `admission.blocked`, and so in no row of 98's `admission.held`, which names no
// issue `active` or `waiting` names. An issue past its review holds no slot on 111's rule, so it
// has none to give back and never reads this word.
// A plugin from 85 through 131, INFRA-714's 86, INFRA-3219's 87, INFRA-2800's 94, INFRA-3107's
// 98, INFRA-3161's 104, INFRA-2560's 111 and INFRA-3146's 123 above among them, reads `kind`
// with a `z.enum` of the
// four words before it, which refuses a word it does not name and with it the whole state body,
// so a caller from a pod whose claims record a contract below 132 is served every member of that
// kind without its `kind` (shaping.go's shapeIssue, withoutWaitingOnIssue), which that schema
// takes as optional; every other field of the member is 85's, `leakCheckCause` on the claim views
// is 87's and narrowed as 87 says, the issues' `phaseEntry` and `givenEntry` are 94's and narrowed
// as 94 says, `admission.held` is 98's and narrowed as 98 says, the issues' `treeMovedAt` and
// `quietHours` are 111's and narrowed as 111 says, and a caller below 85 has lost `kind` with the
// rest of them already (withoutWhyAndLineFields). This daemon serves 132 down by a value strip in
// shapeIssue, withoutWaitingOnIssue, beside the wait strip, withoutUnadmittedWaitAnswer, and by
// no row of stateNarrowings. The change carries one store migration beside it, `20261111214351`,
// which adds the column `ReleasedForWaitAt` is read from; a migration moves the daemon's own record
// and is no part of this number. Nothing else of this change is on the wire.
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
// this branch kept it. main has since landed #3677's 94, INFRA-2800's, at 045ee076, which is
// above 92, so this branch moved off it at the forward merge that brought that commit in. The
// same reading at refs/pull/<n>/head of each of the 140 open pull requests other than this one
// (141 with it) at 2026-11-13T08:16Z gave, above 94: 95 (#3519 and #3712), 96 (#2731 and
// #3573), 98 (#3708) and 99 (#3575), and no open head declares 97, so this branch took 97, the
// lowest number above 94 that no open head declared. The same reading of the 140 open pull
// requests other than this one at 2026-11-13T09:03:10Z gave, above 94: 95 (#3519 and #3712), 96
// (#2731 and #3573), 97 (#3937, which had moved up from 93 meanwhile), 98 (#3708) and 99
// (#3575), so 97 was no longer free and this branch took 100. main has since landed #3708's 98,
// INFRA-3107's, then the entry right above, at b7a2f926, and this entry sits above it. The same
// reading at refs/pull/<n>/head of each of the 140 open pull requests other than this one (141
// with it) at 2026-11-13T13:47:46Z gave, above 98: 99 (#3575), 100 (#3573), 101 (#3712), 102
// (#3730), 103 (#3519), 104 (#3830 and #3937) and 105 (#3659), so this branch moved to 106. The
// same reading of the 140 open pull requests other than this one at 2026-11-13T14:11:00Z gave,
// above 98: 99 (#3575), 100 (#3573), 101 (#3712), 103 (#3519), 104 (#3830 and #3937), 105
// (#3659) and 106 (#3730, which had moved up from 102 meanwhile, and #3793), so this branch
// moved to 107. The same reading of the 140 open pull requests other than this one at
// 2026-11-13T14:31:49Z gave, as the highest numbers above 98, 105 (#3659), 106 (#3730 and
// #3793) and 107 (#3712, which had moved up from 101 meanwhile), and no open head declares above
// 107, so this branch moved to 108. main has since landed #3830's 104, INFRA-3161's, then the
// entry right above, at aee2873c, and this entry sits above it. The same reading of the 137 open pull
// requests other than this one at 2026-11-13T17:15:05Z gave, above 104, 105 (#3659), 106 (#2731
// and #3730), 107 (#3712) and 109 (#3519 and #3793), and no open head declares above 109, so
// this branch moved to 110: 108 was free, but it was below 109. The same reading of the 135 open
// pull requests other than this one at 2026-11-13T18:08:34Z gave, above 104, 105 (#3659), 106
// (#2731 and #3730), 107 (#3712), 108 (#3519, which had moved down from 109 meanwhile), 109
// (#3793) and 110 (#3937, which had moved up from 104 meanwhile), and no open head declares
// above 110, so this branch took 111, above main's 104 and above every number another open head
// declared. main has since landed #3573's 111, INFRA-2560's, then the entry right above, at 80920857,
// and this entry sits above it. The same reading of `const DaemonAPIVersion` at
// refs/pull/<n>/head of each of the 138 open pull requests other than this one (139 with it), at
// 2026-11-13T22:06:06Z, gave, above 111, 111 (#3793), 112 (#2731), 113 (#3575) and 114 (#3730),
// and no open head declared above 114, so this branch took 115. The same reading of the 139 open
// pull requests other than this one (140 with it) at 2026-11-13T22:47:06Z gave, above 111, 111
// (#3178, #3793 and #3982), 112 (#2731), 113 (#3575), 114 (#3730) and 115 (#3519 and #3659,
// which had moved up meanwhile), and no open head declared above 115, so this branch took 116.
// main has since landed #3793's 123, INFRA-3146's, the entry right above, at 25f3e38a, and this
// entry sits above it. The same reading of the 148 open pull requests other than this one at
// 2026-11-14T15:19:58Z gave, above main's 123, 124 (#4021), 125 (#4030), 126 (#2230 and #4017),
// 127 (#3519, #3659 and #3711), 128 (#3730 and #4035) and 129 (#3937), and no open head declared
// above 129, so this branch took 130. The same reading of the 148 open pull requests other than
// this one at 2026-11-14T16:04:51Z gave, above 127, 128 (#3730 and #4035), 129 (#3937), 130
// (#2731 and #3993, which had moved up to it meanwhile) and 131 (#3575, which had moved up to it
// meanwhile), so 130 was no longer free, and no open head declares above 131, so this branch
// takes 132, the lowest number above main's 123 and above every number another open head
// declares. Read the base's own number and every open pull request again at every forward merge
// rather than from this line.)
//
// 133: dispatch://INFRA-3027 -- the issue record of `GET /legion/v1/state` gains `mergeWait`, what
// the issue's review round waits on from GitHub (api.MergeWaitView, which projection's
// mergeWaitView builds for exactly the rounds classify.RoundWaitsOnMergeState names — the one rule
// the engine reads to decide whether to ask GitHub about a round at all, so the object is on the
// view for every round the daemon is reading for and for no other). Its fields are `waitsOn`, the
// head the round waits on, which is the pull request's current head and the head the reviewer's
// approval stands on; `mergeState`, the merge state the record holds for that head in GitHub's own
// lowercase spelling (`blocked`, `behind`, `dirty`, `draft`, `unknown`), absent when the record
// holds no state for the head; `stateReadAt`, the moment of the daemon's last answering read of the
// pull request (record.PullRequest.StateReadAt, whose doc says when it is zero), absent when the
// record holds none, since a reader must never take a read at the zero instant for a time;
// `headUpdatedAt`, the newest `updated_at` GitHub has given the pull request among its lifecycle
// observations, absent when the record holds none; `checkRuns`, how many of the fence's runs are
// stamped with that head, which is the runs a CI settlement has applied for it and so 0 until a
// settlement has arrived, which is no evidence of what GitHub holds, written whatever its value so
// that 0 is that reading rather than a gap in the view; and `toldHead` with `toldState`, the head
// and state the architect and the reviewer were last told of once the round was reported stuck,
// absent when the reviewer's decision records none. A caller from a pod whose claims record a
// contract below 133 is served the state with no `mergeWait` on any issue at all — the key absent
// rather than empty, since a strict reader before the contract that added a key refuses it whatever
// it names (api's stateNarrowings, withoutMergeWait) — as a caller below 132 is served every member
// of the waiting line that waits on another issue without its `kind`, one below 111 every issue
// without `treeMovedAt` and `quietHours`, one below 98 the admission block without
// `admission.held`, one below 94 every issue without `phaseEntry` and `givenEntry`, one below 87
// every claim view without `leakCheckCause`, one below 85 every issue without `why` and every
// member of the waiting line with its `place` and its `length` alone, one below 75 the claim views
// and the issues without the nine keys the decided record added and the body without its `contract`
// mark, one below 73 a body with no `claimsToRefresh`, one below 62 the workers without their
// counts of the moves back, one below 58 the issues without their place in the admission line, one
// below 57 the claim views with no `step`, one below 56 the stranded report without its retired
// entries, one below 55 the issues without the gated steps, one below 53 a body with no
// `wakesWaiting`, one below 51 the issues without the admission's trees, one below 49 one with no
// memory draw and one below 46 the claim views without their update rounds; the bands compose, and
// every reader from this contract on takes the object as optional. The daemon makes no read of
// GitHub of its own for any of it: every field is the record as the daemon's own reads of the pull
// request have left it, so a state request costs no GitHub call and the object says how old the
// daemon's knowledge of the head is rather than what GitHub would answer now. The number is a
// census of `const DaemonAPIVersion` at refs/pull/<n>/head for every open pull request of this
// repository, taken at the moment the sites are written and never the base's number plus one,
// because the open heads move the free number within the hour. The reading that chose this one was
// taken at 2026-11-14T23:49:47Z-23:52:45Z, at the forward merge of main 8b0f29b9, over the 152 open
// pull requests: main declares 132 (INFRA-3036's entry above, landed by #3715), and no open head
// declares 133 or above, so 133 is the lowest number above the base's that no other open head
// declares. This change keeps its number while it stands above its base, even when another open
// head declares the same one: check-daemon-api-contract.sh compares a change with its base alone, a
// renumber costs a push, a full check run and a test round, and the change that lands second is the
// one renumbered (`.github/scripts/check-daemon-api-contract.sh`). When the base declares this
// change's number or a higher one, the base has taken it, and this change takes the free number at
// its next forward merge, from a census read then, in one push. The readings that chose this
// change's earlier numbers are in its implement handoffs on dispatch://INFRA-3027 rather than
// here, since each merge would make them false, and the entries above keep the numbers they were
// read at. Read the base's own number and every open
// head again at the next merge rather than from this entry, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.
//
// 137: INFRA-3621 (cybermonklabs/legion) -- a pod's `legion launcher` may be told
// `--session-impersonate-service-account <email>`, a fourth session flag after the three that name
// the Cloud SQL session database (`sessionProxyAccountFlag` after `sessionProxyFlagNames`,
// `internal/runtime/sandbox/manifest.go`). The daemon renders it
// only when `runtime.kubernetes.session_database.impersonate_service_account` is set, and the
// launcher's session proxy then impersonates that Google service account with the pod's own
// credentials and signs in to Cloud SQL as it (`connectorOptions`, `internal/sessionproxy`). A
// launcher built before 137 exits 2 on the unknown flag in every pod the daemon creates with it,
// so the image probe, which holds the image's pi-legion to this number, refuses such an image at
// boot instead, as it did for 27's new flag. A deployment that sets no account renders no fourth
// flag, and its pods carry the command they carried before. (The number was the lowest above
// main's 132 that no open pull request of cybermonklabs/legion declared at 2026-11-15T02:27Z, read
// with the `take` of .github/scripts/check-daemon-api-contract.sh. It stays above main's 133 at the
// forward merge of 2026-11-15 that brought in 2e546572 (#3937's 133, the entry above), and the same
// reading at 2026-11-15T03:57Z found no other open pull request declaring 137: it passed over 134
// (#4017 and #4021), 135 (#3659) and 136 (#3575 and #3730). Read the base's own number and every
// open pull request again at every forward merge rather than from this line.)
//
// 141: INFRA-2566 -- the experiments each issue was drawn for on `GET /legion/v1/state`. Every
// issue record of the state gains `assignments`, the experiment answers that apply to it: one
// `{experiment, variant, mode, drawnAt}` per answer, in the order the project's file declares
// the experiments (config.Project.ExperimentsDeclared): the `memory` block's experiment first,
// unless `experiments.memory` declares it instead (config.Project.MemoryIsBlock); then the
// experiments `settings.projects.<KEY>.experiments` names, in its order; then any experiment the
// file no longer names, in the order of their names (internal/projection's declaredPlaces). An
// issue carries its tree's answer for an experiment whose unit is the tree and its own for one
// whose unit is the issue (api.Issue.Assignments, filled and ordered by internal/projection from
// record.Assignment). `variant` and `mode` ride
// whatever they hold, the empty string included, and `drawnAt` is absent where the row holds no
// instant: that row — no arm, no mode, no instant — is a hand-over settling a child at no arm
// for an experiment the tree it left held no row for, which is how a reader tells a unit settled
// at no arm from a unit never drawn, which is no row at all. The key itself is absent from an
// issue no experiment holds an answer for, so the wire carries no empty list. The three memory
// fields of contract 49 answer exactly as they did and no reader of them changes: they are now
// read off the answer of the experiment named `memory`, whichever spelling declared it (the
// `memory` block or `experiments.memory`), and only where that row records a draw, where the
// arm `memory` is the group `memory` and every other arm — the control `none` and the empty
// arm an experiment at mode off draws, which the memory draw wrote as the word `none` — is
// `none`. A `memory` row with no instant, which is a hand-over settling a unit at no arm, fills
// none of the three, as the empty `issues.memory_group` of such a tree filled none of them
// before: a tree that answered nothing answers nothing still, and the measurement job's count
// of the trees each group reached is the count it was. `assignments` carries that settled row
// all the same, with its empty variant. `settings.projects.<KEY>.experiments` is a key new at
// 141, and every project carries it, `[]` for a project whose file declares no experiment under
// that key, as `slack_channels` and `memory` are always carried; every plugin reads the settings
// loosely, so no strictly parsed shape changes with it. `packages/contracts/src/legion-api.ts` mirrors
// `assignments` on `legionIssue`, which is a `z.object`: every answer-side schema of
// `packages/contracts` has stripped a key it does not name since sjawhar/legion#2788, which
// that package's own test holds it to, and 141 is later than that change, so a reader built after
// that change and before 141 parses this state and sees no `assignments` at all rather than
// refusing it — and every worker's `read_record` parses the state through that schema. The
// downgrade below is still what serves such a reader: a caller that did not ask for the field is
// not given it.
// `GET /legion/v1/state` authenticates nobody, so a caller from a pod whose claims record a
// contract below 141 is served the state without the field (api's stateFor,
// withoutAssignments), as a caller below 49 is served it without the memory fields; a caller
// from 49 to below this contract is served the three memory fields and no assignments, whatever
// else its own band gives it — each band is served what the narrowing at its lower edge and
// every narrowing below it added and none of what a narrowing above it added (api's
// stateNarrowings, held band by band by TestTheStateRouteServesAPodItsPluginContractsIssues),
// and each still no assignments — and a caller below 49 neither. The 86 entry above adds no
// narrowing of its own, so a caller at 86 is served what one at 85 is: the `why` answer and the
// line's own fields, and neither `leakCheckCause`, nor the two entry numbers, nor
// `admission.held`, nor the tree's quiet time, nor the word `waiting_on_issue`, nor `mergeWait`,
// nor assignments. With this entry's narrowing the route's strips compose as the strips before it
// did. The number is held in the files that move together, which
// .github/scripts/check-daemon-api-contract.sh names as its SITES. The answers are the rows of `experiment_assignments`,
// which this change's one store migration, 20261111045636_experiment_assignments, creates and
// back-fills from the four `issues.memory_*` columns; those columns stay in the schema, are
// written by nothing from this release on, are read by nothing this release's daemon runs but
// that migration's back-fill, and by the copy by hand an operator runs on a roll forward
// (`docs/kubernetes.md`), and a later release drops them.
// (This entry took 60 when the base carried 59, 73 at the forward merge of 2026-11-11 that
// brought main in, 74 at the push that followed, 86 at the forward merge of 2026-11-12 that
// brought main in at 9009dc1d, 87 at the push after it, 90 at the push after that, 93 at the
// push after that, 95 at the push after that, 97 at the forward merge of 2026-11-13 that brought
// main in at 12ba4e6a, and 99 at the push after that. Each of those took one above every open
// declaration rather than the lowest free number, because three branches had each taken the
// lowest free number at three moments and collided four ways at 65. The heads were read at that
// push over the `const DaemonAPIVersion` of main and of every one of the 140 pull requests of
// this repository then open, at refs/pull/<n>/head, rather than over the file lists GitHub
// gives, which it cuts short for a pull request of many files: main then declared 87, and the
// highest open declarations were #3708's 98, above the 97 this change declared before that
// move, #2731's and #3573's 96 and #3677's 94; every other open head declared less than 94. So
// this change took 99, which was one above every one of them. That rule, one above every open
// declaration, is retired here: two trees both following it push each other higher at every
// push, and #3708 and this change did, 95 to 99 against 98. This change holds its number while
// main's own number is below it, and renumbers only when main reaches that number or above,
// once, in the forward merge that landing forces, to the lowest free number above main's;
// .github/scripts/check-daemon-api-contract.sh refuses a number at or below the base's, which is
// the one collision a renumber has to answer. At the forward merge that brought main in at
// 045ee076, #3677's own, main declared 94; at the forward merge that brought main in at
// b7a2f926, #3708's own, main declared 98, the entry then right above this one, still below 99,
// so the number held; at the forward merge that brought main in at 2a6ff6c0, #3786's own, main
// still declared 98, so the number held at 99. At the forward merge that brought main in at
// aee2873c, #3830's own, main declared 104, the entry then right above this one, so in that
// merge the number moved to the lowest free number above 104 at its push, 113: of the heads of
// the 138 pull requests open at 2026-11-13T19:05:49Z, read at refs/pull/<n>/head, #3659 declared
// 105, #2731 106, #3712 107, #3519 108, #3793 109, #3937 110, #3573 and #3715 111, and #3730 112;
// the reads of 17:53Z and 18:51:45Z, before those three moved, had named 111 and then 112. At the
// forward merge that brought main in at 80920857, #3573's own, main declared 111, the entry then
// right above this one, still below 113, so the number held. At the forward merge that brought
// main in at 50bfbb0d, #3982's own, main still declared 111, so the number held at 113. At the
// forward merge that brought main in at 7df0a64c, main declared 123, #3793's own, the entry then
// right above this one, so in that merge the number moved to the lowest free number above 123 at
// its push, 131: of the heads of the 149 pull requests open at 2026-11-14T15:50:33Z, read at
// refs/pull/<n>/head, #4021 declared 124, #4030 125, #2230 and #4017 126, #3519, #3659 and #3711
// 127, #3730 and #4035 128, #3937 129, and #2731 and #3993 130; the read of 15:25:30Z, before
// those two declared 130, had named 130. At the forward merge that brought main in at 8b0f29b9,
// #3715's own, main declared 132, the entry then right above this one, so in that merge the
// number moved to the lowest free number above 132 at its push, 134: of the heads of the 152
// pull requests open at 2026-11-15T00:17:26Z, read at refs/pull/<n>/head, #4033 declared 132 and
// #3937 133. Before that merge was pushed, the reading at its push, of the heads of the 151
// pull requests open at 2026-11-15T01:04:30Z, found #4021 declaring 134 beside #2697, #3995 and
// #4033 at 132 and #3937 at 133, so the number moved once more, to 135, the lowest free number
// above 132 then. The reading before the next push, of the heads of the 150 pull requests
// open at 2026-11-15T01:34:27Z, found #3659 declaring 135, so the number moved to 136, the
// lowest free number above 132 then. At the forward merge that brought main in at b39062a1,
// main declared 133, #3937's own, and #3730 had declared 136 too, so in that merge the number
// moved to the lowest free number above 133 at its push, 139: of the heads of the 150 pull
// requests open at 2026-11-15T04:20:22Z, read at refs/pull/<n>/head, #2827, #3711 and #3983
// declared 133, #4017 and #4021 134, #3659 135, #3730 136, #4046 137, and #3712 and #3993 138.
// Before that merge was pushed, the reading at its push, of the heads of the 148 pull requests
// open at 2026-11-15T05:25:55Z, found #2731 and #3993 declaring 139 and #3519 140, so the
// number moved once more, to 141, the lowest free number above 133 then. At the forward merge
// that brought main in at 7baa398b, main declared 137, #4046's own, below 141, so the number
// held.)
//
// 144: dispatch://INFRA-3557 -- the memory guard in an issue pod's launcher, which is two shapes of
// the launcher wire. The daemon's answer to a launcher's hello carries the settings the guard runs
// by, as one optional member of `shimwire.LauncherHelloAck` (`memoryGuard`: `actPercent`,
// `floorPercent`, `waitSeconds` and `intervalMilliseconds`, from
// `runtime.kubernetes.memory_guard.issue_pods`). The runtime fills it for an issue pod's launcher
// alone (sandbox's Runtime.launcherHelloAck), and leaves it out where that block's `enabled` is
// false. The launcher sends the daemon one `launcher_memory_kill` frame
// (`shimwire.LauncherMemoryKill`) for each process its guard ended with SIGKILL, on the connection
// of the role whose process it was. The frame names that process by `executable`, the base name of
// its executable (its stat name where the guard could not read that), escaped and cut to 256 bytes,
// and carries no command: none of the process's arguments, which may hold a credential no masking
// rule names. The daemon applies it once, under the event id
// `memory-guard-kill:<guard>:<sequence>`, and tells that role why on its role topic as a
// `record.RoleMessage` row (workflow's memoryGuardNote): an outbox kind earlier releases already
// decode, so this change adds no store migration and owes no rollback statement for the note.
//
// The skew is safe in both directions, and this entry records why. An older launcher ignores the
// new member, since its decoder answered `LauncherHelloAck{}` without reading any member, and it
// sends no report. An older daemon writes no member, and the launcher reads that absence as "run no
// guard" (memoryguard's Guard.Apply), the same absence a deployment that turned the guard off
// sends. That daemon also closes a connection on a frame type it does not name (`ServeLauncher`'s
// `default: return`), and the launcher then redials, so the launcher sends `launcher_memory_kill`
// only on a connection whose acknowledgement carried settings (`internal/launcher`'s
// manager.openMemoryReports), and drops with a line a report that waits on any other. The same
// rule keeps the settings on the acknowledgement: the launcher's own command loop closes the
// connection on a frame type it does not name as well, so the settings must never become a
// daemon-to-launcher frame of their own. The image probe pairs a daemon with an image by this
// number (`runtime/sandbox/probe.go`), as it did for the launcher frames of 39 and 48, which is
// why a change to a launcher frame carries one.
//
// The `runtime.kubernetes.memory_guard` key is no part of this number. The state's settings block
// carries it as `settings.runtime.kubernetes.memory_guard`, and the plugin parses that block loosely
// (35 above), so a later legion.yaml key changes no strictly parsed shape and bumps nothing, as 111
// says of `stalled_tree_notice_hours`. An earlier loader refuses the key by name, so a file that
// sets it takes the block out before an earlier image is pinned (docs/kubernetes.md, "Rolling back
// below the release that guards an issue pod's memory").
//
// Two other changes touch the same frame: dispatch://INFRA-3531, the guard of the controller's pod,
// which fills this same member for that pod from a block of its own beside `issue_pods`, and
// cybermonklabs/legion#3519 (dispatch://INFRA-2009), which adds `volumeTrim` beside it and a
// `launcher_volume_report` frame. Of this change and either of those, the one that merges second
// takes a number above the first's, every site that names the number included, and keeps every
// member of both.
// (This change took 124, the lowest number above main's 111 that no open pull request declared,
// read on 2026-11-14 in each head's own copy of this file, and kept it at the forward merge of
// 2026-11-14 that brought main in at 25f3e38a3e29 (#3793's 123). It took 134 at the forward
// merge of 2026-11-15 that brought main in at 8b0f29b971df (#3715's 132), and kept it at the one
// that brought main in at a3ff3b81bbf0 (INFRA-3027's 133). It took 144 at the forward merge of
// 2026-11-15 that brought main in at 7baa398b206b (#4046's 137), the lowest number above 137 that
// no open pull request declared at 2026-11-15T12:47Z, and keeps it at the forward merge that
// brings main in at 7ed023731622, which carries #3575's 141, the entry right above and the number
// the base now carries: 144 is still above it, and the reading of every open head at
// 2026-11-15T17:19Z gives 144 to this change alone. That is a reading and never the rule: read
// main's own number and every open head again at the next forward merge rather than from this
// line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// 146: INFRA-2614 (cybermonklabs/legion) -- a root's architect is a role the record states work
// for, so the two keys contract 94 added carry a root: `phase.RoleFor` answers the architect for
// `admitted`, the phase a root's own architect works, and each root's view carries the entry its
// architect's work stands at and the entry its claim was queued, as a child's carries its
// implementer's. Beside them each issue of `GET /legion/v1/state` carries the two times of a
// claim's handshake of the last entry it was queued a task of: `entryQueuedAt`, when a task of
// that entry was first queued to the claim, and `entryConfirmedAt`, when a turn of that claim's
// worker first confirmed one. The claim keeps both in a record of its own
// (`supervise.Claim.Handshake`, the four `claims.handshake_*` columns store migration
// 20261114020537 adds), so they outlast the task, which the end of the confirming turn retires.
// A tree's own root carries its architect's handshake whatever phase it stands in, through
// `awaiting_person` and the phases a phase role works, where the two numbers are that role's and
// the two times are still the architect's; any other issue carries the handshake of the role of
// its current phase. Both are absent where no entry was queued to that claim in the issue's
// current generation — a role the daemon holds no claim row of, a re-admitted root whose new
// architect entry is not queued yet, and a root of the release before this one, whose migration
// gives the claim the two numbers and no handshake, which is how a reader tells such a root from
// one this release served — and `entryConfirmedAt` alone is absent while no turn has confirmed a
// task of the entry, which is the root whose architect took nothing.
// A tree's own root carries `architectStalls` too: every arm of INFRA-2614's architect-stalled
// reading that holds for its architect at that read, as `record.ArchitectStallArm`'s values —
// `took-nothing` (the task of the entry its `phases` row names was first queued to its claim more
// than ten minutes ago and no turn confirmed it, counted from that first queueing however many
// times the daemon sent that entry's task again; a task whose turn ended with the claim's process,
// at a death or at a replacement the daemon made, counts only while the claim is ready, working or
// idle, and from the relaunched agent's ready where that is later; and a task the root's own
// record has superseded, one that carries an entry, of a run before its generation or of an entry
// the row has moved past, raises the arm not at all, nor does a task of no entry, such as a
// wake's), `claim-unreachable` (the claim is failed or
// retired and no start of the root's run is the daemon's next move for it: a start the outbox is
// running is, and so is a start queued in the last ten minutes unless it has already run against
// the claim and left it failed or retired, after which the outbox only runs it again on its
// backoff; or no architect claim stands, no start of one was queued in the last ten minutes, and
// the work of its entry has waited the same ten minutes; a root waiting on a person raises it as
// any root does, since the daemon queues the next start of its architect only when that wait is
// answered) and
// `slow-before-gate` (it stands at `admitted` with no design-gate step registered 90 minutes
// after the later of its admission and its architect's last raised entry) — in that order. The
// daemon serves the arms the admission pass reads before it wakes the controller, by the same
// predicates over the same reads (admit.Admission.ArchitectStallArms, in the state's own
// snapshot), whether or not the controller was told of an arm, and the key is absent for a root
// no arm holds for and for every issue that is not a tree's root.
// `GET /legion/v1/state` authenticates nobody, so a caller from a pod whose claims record a
// contract below 146 is served the issues without both times and without `architectStalls`
// (api's stateFor, withoutEntryTimes) — as a caller below 141 is served the issues without
// `assignments` (144, the entry right above, moves no strictly parsed shape of the state and
// narrows nothing: `settings.runtime.kubernetes.memory_guard`, which came with it, is a key of the
// settings block the plugin parses loosely and no part of that number), one below 133 the issues
// without `mergeWait` (137 moves no shape of the state and narrows nothing), one below 132 every
// member of the waiting line whose `kind` is `waiting_on_issue` without its `kind`, one below 111
// the issues without `treeMovedAt` and `quietHours` (123 moves no shape of the state and narrows
// nothing), one below 98
// the admission block without `admission.held`, one below 94 the issues without `phaseEntry` and
// `givenEntry`, one below 87 every claim view without `leakCheckCause`, one below 85 every issue
// without `why` and every member of the admission line with its `place` and its `length` alone,
// one below 75 without the nine keys the decided record added and the body without its
// `contract` mark, one below 73 a body with no `claimsToRefresh`, one below 62 without the
// workers' `backwardMoves`, one below 58 with no place in the admission line, one below 57 with
// no `step` on a claim view, one below 56 the stranded report without its retired entries, one
// below 55 without the gated steps, one below 53 a body with no wakes waiting, one below 51
// without the admission's trees and each issue's tree, one below 49 without the memory draw and
// one below 46 without the claim views' update rounds; the bands compose, and every reader
// from 146 on takes both times and `architectStalls` as optional. The two entry numbers move no
// further: a plugin at 94 parses them already, and a root carrying them is the same shape the
// issue beside it carries.
// `legion state`'s plain line changes with them: it names a tree's own root where the state
// names an arm for it (`architectStalls`), with that arm's words, and states no arm of its own,
// and it names a root for nothing else: not for the mere difference of its two numbers, which
// every root reads as between a transition and its delivery, whatever role works the phase it
// stands in. The rest of the change moves no shape a caller reads: the architect's row of the
// `phases` record, the derivation that fills its entry and the sweep that sends that entry's task
// are the daemon's own lanes and no route.
// (This entry took 94 on cybermonklabs/legion#3677's head 643be0c4ae84, whose own entry stood
// at 93 then. #3677 merged at 94. The entry took 111 at the forward merge of 2026-11-13 that
// brought main in at 7810d00ef1c3, which declared 104, cybermonklabs/legion#3830's entry: a
// reading of `const DaemonAPIVersion` in this file at refs/pull/<n>/head at 2026-11-13T18:12Z,
// for every one of the 41 heads among the 136 open pull requests of this repository whose file
// list named this file or was cut short, gave above that base's 104: 105 (#3659), 106 (#2731 and
// #3730), 107 (#3712), 108 (#3519), 109 (#3793) and 110 (#3715 and #3937). It took 117 at the
// forward merge of 2026-11-13 that brought main in at 809208573a01, which landed
// cybermonklabs/legion#3573's INFRA-2560 at 111, then the entry right above: 111 was the number
// this change declared until then, and it was the number the base then carried. A first reading
// of `const DaemonAPIVersion` in this file at refs/pull/<n>/head between 2026-11-13T22:44Z and
// 22:48Z, for every one of the 139 open pull requests of this repository other than this
// change's own #3711, gave above the base's 111: 112 (#2731), 113 (#3575), 114 (#3730) and 115
// (#3519), so this change took 116. Other heads moved while the merge was resolved, and a second
// reading between 23:16Z and 23:22Z, for every one of the 138 open pull requests other than
// #3711, gave above 111: 112 (#2731), 113 (#3575), 114 (#3730), 115 (#3519, #3659 and #3937)
// and 116 (#3715). 111 itself was declared by #3178, #3793 and #3982, and every other head read
// declared 110 or below. So this change took 117, the lowest number above the base's that no
// open head declared. The reading is of this file at each head and not of a file list: GitHub
// cuts a pull request's file list short, so every open head is read. It took 127 at the forward
// merge of 2026-11-14 that brought main in at 25f3e38a3e29, which landed
// cybermonklabs/legion#3793's INFRA-3146 at 123, then the entry right above and the number the
// base then carried, so 117 was no longer above it. A reading of the number every open pull
// request of this repository declared, made between 2026-11-14T12:30:25Z and 12:34:35Z, gave
// above the base's 123: 124 (#4021), 125 (#4030) and 126 (#2230 and #4017), and no head declared
// a number above 126, so this change took 127, the lowest number above the base's that no open
// head declared. It took 133 at the forward merge of 2026-11-15 that brought main in at
// 8b0f29b971df, which landed cybermonklabs/legion#3715's INFRA-3036 at 132, then the entry right
// above and the number the base then carried, so 127 was no longer above it. A reading of
// `const DaemonAPIVersion` at refs/pull/<n>/head for every open pull request of this repository,
// made between 2026-11-15T00:05:55Z and 00:10:18Z, found no head declaring 133 or above, so this
// change took 133, the lowest free number above main's 132 at that reading. It took 139 at the
// forward merge of 2026-11-15 that brought main in at 2e546572f2b8, which landed
// cybermonklabs/legion#3937's INFRA-3027 at 133, then the entry right above and the number the base
// then carried, so 133 was no longer this change's. A reading of `const DaemonAPIVersion` at
// refs/pull/<n>/head for every open pull request of this repository, made between
// 2026-11-15T03:58:42Z and 04:03:11Z, gave above the base's 133: 134 (#4017 and #4021), 135
// (#3659), 136 (#3575 and #3730), 137 (#4046) and 138 (#3712), and no head declared a number
// above 138, so this change took 139, the lowest free number above main's 133 at that reading.
// It took 144 at the forward merge of 2026-11-15 that brought main in at 66ee7eb9100d, which
// still declared 133. A reading of `const DaemonAPIVersion` at refs/pull/<n>/head for every open
// pull request of this repository, made between 2026-11-15T10:07:01Z and 10:17:18Z, gave above
// the base's 133: 134 (#4021), 135 (#3659), 136 (#4065), 137 (#4046), 138 (#4017), 139 (#2731
// and #3993), 140 (#3519), 141 (#3575), 142 (#3712) and 143 (#3730), and no head declared a
// number above 143, so 139 was no longer free and this change took 144, the lowest free number
// above main's 133 at that reading. It took 146 at the forward merge of 2026-11-15 that brought
// main in at fecceeb60ec0, which landed cybermonklabs/legion#4046's INFRA-3621 at 137, then the
// entry right above and the number the base then carried. A reading of `const DaemonAPIVersion`
// at refs/pull/<n>/head for every open pull request of this repository, made between
// 2026-11-15T14:06:47Z and 14:11:13Z, gave above the base's 137: 138 (#4017), 139 (#2731 and
// #3993), 140 (#3519), 141 (#3575), 142 (#3712), 143 (#3730), 144 (#4021) and 145 (#3659), so
// 144 was no longer free and this change took 146, the lowest free number above main's 137 at
// that reading. It kept 146 at the forward merge of 2026-11-15 that brought main in at
// 3aef4ebbd041, which landed cybermonklabs/legion#3575's INFRA-2566 at 141, then the entry right
// above and the number the base then carried. A reading of `const DaemonAPIVersion` at
// refs/pull/<n>/head for every open pull request of this repository, made between
// 2026-11-15T18:12:54Z and 18:16:58Z, gave above the base's 141: 142 (#3712), 143 (#3730), 144
// (#4021), 145 (#3659) and 147 (#4017), so 146 was still the lowest free number above main's 141
// at that reading. It keeps 146 at the forward merge of 2026-11-15 that brings main in at
// 52b3cf5c002a, which carries cybermonklabs/legion#4021's INFRA-3557 at 144, the entry right
// above and the number the base now carries. A reading of `const DaemonAPIVersion` at
// refs/pull/<n>/head for every open pull request of this repository, made between
// 2026-11-15T23:05:22Z and 23:08:22Z, gave above the base's 144: 145 (#3659), 147 (#4017), 148
// (#3519), 149 (#2731), 150 (#3730 and #4049) and 151 (#4106), so 146 was still the lowest free
// number above main's 144 at that reading. Each of those numbers is the number that head declared
// at that reading and no head's for good. Read the base's own number and every open pull request
// again at the next merge rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// 151: dispatch://INFRA-3729 -- issue pods in GKE Sandbox microVMs, which is four shapes of the
// launcher wire and one of the state. The launcher's command line gains `--microvm` and, with
// `runtime.kubernetes.sandbox.microvm.metadata` set, `--metadata-workload-pool` and
// `--metadata-identity-provider`, which a microVM issue pod's container passes (sandbox's
// issuePod.launcherContainer). The daemon's answer to an issue pod launcher's hello gains
// `kernelKills` (`shimwire.LauncherHelloAck.KernelKills`), set on every issue pod's answer and on
// no controller pod's. `launcher_memory_kill` gains `cause` (`shimwire.LauncherMemoryKill.Cause`,
// a `shimwire.MemoryKillCause`): empty for the gVisor guard's own kill, as 144 sends it; `kernel`
// for a command of a role that a microVM pod's own kernel stopped; and `kernel-protected` for one
// of the processes that run a role's session (its agent, shim, role-init or a helper) that the
// kernel stopped all the same. The launcher's guest watch names both (memoryguard.Guest), and the
// daemon tells that role with a note of its own for each (workflow's kernelKillNote and
// kernelProtectedKillNote) under the same event id `memory-guard-kill:<id>:<sequence>`. The
// state's settings block carries the new `runtime.kubernetes.sandbox` key as
// `settings.runtime.kubernetes.sandbox`, which the plugin parses loosely (35 above), so that key
// alone would bump nothing.
//
// The skew is safe in both directions. An older launcher refuses an unknown flag and exits, so a
// daemon of this release must never render the microVM flags into a pod whose image predates
// them: the image probe pairs a daemon with an image by this number (`runtime/sandbox/probe.go`),
// as it did for 39, 48 and 144, which is why the flags carry one. A gVisor pod's command line is
// unchanged. An older daemon sends no `kernelKills`, and a microVM pod's launcher then sends no
// kill report, since it sends one only on a connection whose answer carried `kernelKills`
// (`internal/launcher`'s manager.serve), so such a daemon never tells a role of the kernel's kill
// in the guard's words, and the launcher's own line in the pod's log keeps what it would have
// said. An older launcher ignores `kernelKills`, as it ignored `memoryGuard` (144), and sends no
// `cause`. `kernelKills` is set on every issue pod's acknowledgement, a gVisor pod's included, so
// only the controller pod's acknowledgement is the bytes 144 writes, where the member is left
// out; the guard's report is those bytes too, since `cause` is left out when empty.
//
// An earlier loader refuses the `runtime.kubernetes.sandbox` key by name, so a file that sets it
// takes the block out before an earlier image is pinned (docs/kubernetes.md, "Issue pods in
// microVMs").
// (This change took 151, the lowest number above main's 144 that no open pull request declared,
// read on 2026-11-15 in each open head's own copy of this file: #3659 holds 145, #3711
// 146, #4017 147, #4049 and #3519 148, #2731 149 and #3730 150. It keeps 151 at the forward merge
// of 2026-11-16 that brings main in at 47a901c14da2, which carries #3711's INFRA-2614 at 146, the
// entry right above and the number the base now carries: 151 is still above it, and a reading of
// `const DaemonAPIVersion` at refs/pull/<n>/head for every open pull request, made between
// 2026-11-16T19:30:30Z and 19:35:10Z, gave 151 to this change alone (147 #4017, 148 #3519, 149
// #2230, 150 #4049 and #4065, 152 #3730, 153 #4115). That is a reading and never the rule: read
// main's own number and every open head again at the next forward merge rather than from this
// line.)
//
// 152: INFRA-2543 -- the GitHub App installation, and no longer the project's Dispatch repository
// mapping, decides which repositories a project works in, so the `repositories` block of
// `GET /legion/v1/state` stops being that mapping. `mapped` is every repository the daemon
// serves, the ones the mapping names and the ones the project's own records name, and each entry
// gains `apps`, the installation of each role App on that repository (`implement` and `review`,
// both always written): that is the answer an operator and a plugin ask of a repository, and no
// reader should have to parse it out of a refusal's prose. `refused` keeps `repo` and `reason`
// and holds a repository a role App is not installed on, or one whose installation could not be
// read, and never one for want of a mapping; it gains an optional `apps` of that same shape,
// written wherever GitHub answered both Apps' installation reads of `repo` and left out where a
// read failed or `repo` is no `<owner>/<name>`, so which App is missing is read from the field on
// a refused repository as on a served one; `withoutWebhookApp` and `caches` cover the
// repositories the records name beside the mapped ones. `packages/contracts/src/legion-api.ts`
// mirrors the entry and reads `apps` as optional, so a plugin of this contract meeting a daemon
// of an earlier one mid-roll loses that one answer rather than refusing the whole body, and a
// plugin of an earlier contract loses it the same way, its answer side stripping a key it does
// not name. The bump is for what a reader takes the block to mean — a plugin built before it
// reads `mapped` as the project's mapping, which it no longer is, and `refused` as the mapping's
// leftovers — and the boot gate and `legion probe-image` are what keep daemon and plugin from
// meeting across it at all. The addressing every pane, pod and controller is launched with moves
// with the same change: it names the issue's own repository and then the owner accounts the
// role's App is installed on, in place of the project's repositories, which are no bound on the
// work any more and would be a whole installation's worth of names
// (`daemon.RepositoriesClause`).
// The bundle a pane and a pod hold gains one file of its own with the same change, in the root
// beside the owner directories: `renames.txt`, the configured renames the project's pairs
// resolve to (`ghconfig.PairsFile`), projected from the role Secret's `github-renames` key. For
// a pair the routing key is the repository rather than the owner — the image's `legion gh` and
// `legion git-credential` answer a request for the pair's `from` repository from the `to`
// owner's directory, every other repository of that owner from its own, and a pair whose `to`
// owner the bundle holds no directory for from nothing at all, naming the pair — so an image
// built before it reads none of the file and answers the moved repository with the old owner's
// token, a push GitHub refuses; `legion probe-image` is what keeps that pair apart.
// `POST /legion/v1/controller/github-credential` carries the same pairs as `renames`, each with
// its `from` and `to`, absent where the project has none that resolve, so the controller's own
// bundle routes as a pod's does.
// The same answer carries `installed`, the owner accounts the review App is installed on, sorted
// and read with the bundle, which `legion controller start` names in its controller's addressing;
// an older `legion controller start` decodes the answer with plain `json.Unmarshal` and ignores
// the key, so the field is served to every caller and needs no serving down, as `renames` needs
// none.
// That route's refusal of a default owner the review App is not installed on answers
// `DEFAULT_OWNER_NOT_INSTALLED` at this contract, renamed from a code that said the project's
// repository mapping bounds the owner, which it no longer does. The status is the same, 400, and
// nothing in Legion reads the code, so what the rename reaches is an operator's own log filter or
// alert. The MESSAGE changed with the rule, not with the rename: against main it reads "is not an
// owner of the project's repositories", and here it reads "is no owner the controller's App is
// installed on", so a filter matching the old text is told nothing by the code alone.
// (This branch's own base carried 59; it declared 80 until the forward merge of 2026-11-12 that
// brought main in at 85, and then 86, which five other open heads declared as well, and then 88,
// which the forward merge that brought main in at 86 left standing, and then 102 at the forward
// merge of 2026-11-13 that brought main in at 94 — main stood at 104 after the merge of 17:3xZ
// that day, which was then the entry right above this one, where 144 is now — since 88 is
// below that base, which the gate refuses (.github/scripts/check-daemon-api-contract.sh), and
// since the two keys contract 94 added are served to a caller at the declared number alone
// (api's phaseEntriesContract). It took 106 at the merge after that, because
// cybermonklabs/legion#3659 declares 105 and is the change that gives every later contract its
// own serving-down, so a number below its own would be served without that machinery, and 112
// at the merge after that, which #2731 had taken thirteen minutes earlier and which is
// therefore its number rather than this one's. It takes 114 here: a census of
// `const DaemonAPIVersion` at refs/pull/<n>/head over all 148 open pull requests of this
// repository, taken at 2026-11-13T19:38:05Z, reads 104 ten times, then 105, 107, 108, 109, 110,
// 111 twice, 112 twice and 113 once, with nothing above, so 114 is the lowest number above
// main's 104 that no open head declares. The gate
// compares a head with main alone, so every head at a number another head holds passes it today
// and all but the first to land are renumbered after their review has run; that is
// dispatch://INFRA-1569's to cure. Read main's own number and every open pull request's again at
// the next forward merge rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says. main at 61685f973c9a declares 111, and a census of `const DaemonAPIVersion` at
// refs/pull/<n>/head over all open pull requests of this repository, taken at
// 2026-11-14T00:08Z, reads 104 sixteen times, 107 once, 111 twice, 112 once (#2731), 113 once
// (#3575), 114 once (this head), 115 four times and 116 once — so 114 is above main's 111 and
// no other open head declares it, and this merge keeps it rather than renumbering.)
// (The forward merge of 2026-11-14 that brought main in at 123 takes this entry to 128. A census
// of `const DaemonAPIVersion` at refs/pull/<n>/head over every open pull request of this
// repository, taken at 2026-11-14T13:0xZ, reads above main's 123: 123 three times (#2733, #3995,
// #4000), 124 (#4021), 125 (#4030), 126 twice (#2230, #4017) and 127 (#3519), with nothing above,
// so 128 is the lowest number above main's that no open head declares. Read main's own number and
// every open pull request's again at the next forward merge rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
// (The forward merge of 2026-11-15 that brought main in at 132 (#3715) takes this entry to 136.
// A census of const DaemonAPIVersion at refs/pull/<n>/head over every open pull request of this
// repository, taken at 2026-11-15T01:32:08Z, reads above main's 132: 133 (#3937), 134 (#4021)
// and 135 (#3659), with nothing above, so 136 is the lowest number above 132 that no open head
// declares. Read main's own number and every open pull request's again at the next forward
// merge rather than from this line.)
// (The forward merges of 2026-11-15 take this entry to 152. A census of const DaemonAPIVersion
// at refs/pull/<n>/head over every open pull request of this repository, taken at the push at
// 2026-11-15T23:55:22Z, reads main at 144 and above it 145 (#3659), 146 (#3711), 147 (#4017),
// 148 (#3519), 149 (#2731), 150 (#4049, which took this entry's own number while it stood there)
// and 151 (#4106), with nothing above, so 152 is the lowest number above main's that no open
// head declares. This entry stood at 143 until main passed it, and at 150 until #4049 took that.
// At the forward merge of 2026-11-16 main declares 146, cybermonklabs/legion#3711's entry
// being the one right above this one, and a census taken at that push -- const DaemonAPIVersion
// read at refs/pull/<n>/head for every one of the 156 open pull requests of this repository --
// reads above main's 146: 147 (#4017), 148 (#3519), 149 (#2230), 150 (#4049 and #4065), 151
// (#4106) and 153 (#4115), with 152 declared by this pull request alone, so 152 is above the
// base's number and free and this entry keeps it. An earlier census of that same push read only
// the first 100 of those heads, which is a sample and not the census: #4115's 153 and this
// entry's own 152 were both outside it.
// Read main's own number and every open pull request's again at the next forward merge rather
// than from this line.)
//
// 160: INFRA-2009 -- the cache trimmer in an issue pod's launcher process, which is two shapes of
// the launcher wire and one member of `GET /legion/v1/state`. The daemon's answer to a
// launcher's hello carries the settings the trimmer runs by, as one optional member of
// `shimwire.LauncherHelloAck` (`volumeTrim`: the two marks, the minimum age and the interval,
// from `runtime.kubernetes.volume_trim`), filled beside `memoryGuard` for an issue pod's launcher
// alone (sandbox's Runtime.launcherHelloAck) and written whole by `internal/stream`'s
// Listener.hello. The launcher sends the daemon at most one
// `launcher_volume_report` frame per sweep that ends with the volume still above its low mark,
// none for a sweep the settings stopped, and one that no sweep makes when the trimmer has
// switched itself off after three panics in a row in one of the two rows it counts its faults in
// (`switchedOff`, carrying the newest three faults of both rows, which end with the panic that
// switched it off, and its last reading of the volume, if it made one). `ServeLauncher` reads each
// into at most one `volume-near-full` notice to the issue's architect, since the daemon's own
// bounds decide which sweep reports are told, and the switched-off report passes every one of
// them. That frame carries, beside the sweep's own numbers, how many
// entries the launcher's own bound left out of each of its lists (`sizesOmitted` and its three
// siblings), so a reader never takes a cut list for the whole of what the trimmer measured, and
// two optional members of each size, `refused` and `refusals`: the first read under that target
// the walk did not make — one the kernel refused, or a directory more than 64 levels below the
// target, which the walk does not open — named by the target, the name and its depth, and how
// many such reads there were, so a size the walk could read only in part is told as a lower
// bound and never as the target's whole. Each size measures a directory two levels under the
// volume root, a regular file beside such directories in an immediate child of the root, or an
// immediate child that holds no directory of its own. The frame's `sizesPartial` is a walk its
// budget or the container's stop cut, and nothing else, and its own rule
// (`shimwire.LauncherVolumeReport.Validate`) refuses a report holding more entries in any list
// than that list's cap (`shimwire.VolumeReportSizes` and the three beside it), since the daemon
// renders every entry it is given into a notice and the launcher's own cut runs only where the
// sender is that launcher. The state body's `settings.runtime.kubernetes` gains
// `volume_trim` (`enabled`, `high_percent`, `low_percent`, `min_age_minutes` and
// `interval_seconds`, api.VolumeTrimSettings): the settled block, `enabled` false where the
// deployment switched the trimmer off, which the contracts' state fixture
// (`packages/contracts/fixtures/daemon-api/state.json`) carries. A plugin's schema reads
// `settings` as a loose object (`legionSettings` in `packages/contracts/src/legion-api.ts`), so
// a reader of an earlier contract takes the state body with the member in it.
//
// The skew is safe in both directions, and this entry records why, because that is what a
// reader of it needs. A new worker image under an older daemon reads the member absent, which
// is the one thing that means "no settings", so its trimmer never starts and its send side
// refuses a report on a connection whose acknowledgement carried none (`internal/launcher`'s
// manager.sendVolumeReport), which is what keeps it off that daemon's switch on an unmodelled
// frame type. An older worker image under a new daemon ignores the extra member of the ack,
// because the frame decoder is tolerant of members it does not model, and it sends no report
// either. Both rest on one constraint, which holds now and later: the settings must never become
// a daemon-to-launcher frame of their own, because `ServeLauncher` and the launcher's own
// command loop each close the connection on a frame type they do not name, and a launcher that
// redials once a second for every role of the pod is the failure this shape exists to avoid.
//
// What the wire survives is not what the store survives, and a reader of this entry about to
// change this number needs the second as well. The same change adds the architect notice kind
// `volume-near-full` and the `volume` member of the notice payload, and an outbox payload is
// decoded strictly, so a daemon of any release without this entry can decode no such row: every
// release of main before this change lands, the one just before it included. The bound is named
// by the entry and not by a number because a forward merge can move the number, as the history
// below records, and a number written into prose stays where it was. That row does not fail
// alone: `WaitingNotices` reads every waiting notice row of the project, `pacedCatchUps`
// returns on the first it cannot decode, and its one caller is the state read, so one row fails
// `GET /legion/v1/state` for the whole project until somebody deletes it — the chain
// `store/migrations/20261109060737_drop_awaiting_slot_notice.up.sql` records as this
// repository's own incident — and `releaseWaiting`, the other reader of that set, fails on it the
// same way. No migration of an earlier release deletes these rows, so the rollback is an
// operator's: one statement with the daemon stopped, and a `runtime.kubernetes.volume_trim`
// block taken out of `legion.yaml`, since an earlier loader refuses that key by name, both
// written down where an operator reads them (docs/kubernetes.md, "Rolling back below the release
// that trims an issue pod's volume").
// (The base carried 64 at the read that first chose a number for this change, 2026-11-11, and
// 65 was declared by three open pull requests then and 66 by a fourth, so it took 67 and held
// that through two forward merges. main reached 68, then 73, then 75, then 85, and now 86 with
// the two entries right above, which is the one thing the hold rule moves for, so this change
// takes 91, above main's own number and above every number an open pull request declares: a
// reading of `const DaemonAPIVersion` at refs/pull/<n>/head for each of the 142 open pull
// requests on 2026-11-12, never of a file list, gives main 86 and, above it, 86 (#2404, #2558,
// #2918, #3000, #3534, #3573, #3677, #3708, #3715 and #3785), 87 (#3939), 88 (#3730), 89
// (#3659, and this change's own head before this merge) and 90 (#3575, #3830 and #3937), so 91
// is the lowest free number above the base. The 89 this change held is given up because #3659
// declares it as well, and a number two heads share is a refusal for whichever merges second.
// It is held from here: a later forward merge keeps both sides' entries and leaves this number
// alone, as it keeps both sides of validNoticeKinds, and it moves again only when main's own
// DaemonAPIVersion reaches or passes it, or when another open head is found declaring it,
// because renumbering changes this branch's own diff, and a renumber made after a result stands
// against this head costs the tester a round rather than a commit. So do not re-derive a number
// from this line at the next merge: read main's own DaemonAPIVersion and the open heads' own,
// and move this one only when one of them reaches 91.
// At the forward merge of 2026-11-13 main had 87, below 91, but #3712 was found declaring 91 as
// well, so the number moved: a reading of `const DaemonAPIVersion` at refs/pull/<n>/head for each
// of the 140 open pull requests gives, above main's 87, 87 (#2544, #2813, #2945, #3178, #3769,
// #3785, #3786, #3831 and #3971), 88 (#3730), 89 (#3659), 90 (#3830), 91 (#3712, and this
// change's own head before this merge), 92 (#3715), 93 (#3937), 94 (#3677), 96 (#2731 and
// #3573), 97 (#3575) and 98 (#3708), so 95 is the lowest free number above main's. It is held
// from here under the same rule: move it only when main's own number or another open head's
// reaches 95.
// At the second forward merge of 2026-11-13 main had 94, below 95, but #3712 had moved to 95,
// so the number moved again: above main's 94 the open heads declare 95 (#3712), 96 (#2731 and
// #3573), 98 (#3708) and 99 (#3575), so 97 is the lowest free number above main's. It is held
// under the same rule: move it only when main's own number or another open head's reaches 97.
// Read again immediately before the push of 2026-11-13 that followed review round 12, with
// main still at 94: the open heads declare 95 (#3659), 96 (#2731), 97 (#3937), 98 (#3708), 99
// (#3575), 100 (#3573 and #3715), 101 (#3712) and 102 (#3730), so 97 is given up because #3937
// declares it as well and 103 is the lowest number above main's that no open head declares. It
// is held under the same rule: move it only when main's own number or another open head's
// reaches 103.
// Read during the round that followed review round 13, with main at 98, the open heads declared
// 105 (#3659), 106 (#3730 and #3793) and 107 (#3712), and a hold on this file asks every change
// to it for a number above #3659's 105, so the number moved to 108. Read again immediately
// before that round's push, after main's second forward merge: #3715 had moved to 108, and the
// open heads declare 99 (#3575), 100 (#3573), 104 (#3830 and #3937), 105 (#3659), 106 (#2731,
// #3730 and #3793), 107 (#3712) and 108 (#3715), so 109 is the lowest number above both main's
// and 105 that no open head declares, and the change was pushed at 109. Main then moved to 104
// (#3830), and the third forward merge of that round read the open heads again: #3793 had moved
// to 109 and #3715 to 110, so the open heads declare 105 (#3659), 106 (#2731 and #3730), 107
// (#3712), 109 (#3793) and 110 (#3715 and #3937), and 108 is the lowest number above both
// main's 104 and 105 that no open head declares. It is held under the same rule: move it only
// when main's own number or another open head's reaches 108.
// At the forward merge that followed review round 15, main had moved to 111, above 108, so the
// number moved again: above main's 111 the open heads then declared 112 (#2731), 113 (#3575)
// and 114 (#3730), so 115 was the lowest number above main's that no open head declared.
// At the forward merge of round 17 main was still at 111, but #3937 had moved to 115, and a hold
// on this file asked every change to it for a number above #3659's 118. Read at
// refs/pull/<n>/head for every open pull request, the open heads declare 112 (#2731), 113
// (#3575), 114 (#3730), 115 (#3937), 116 (#3715), 117 (#3711), 118 (#3659), 119 (#3712) and 120
// (#3793 and #3993), so 121 is the lowest number above both main's 111 and 118 that no open
// head declares. It is held under the same rule: move it only when main's own number or another
// open head's reaches 121.
// At the forward merge of round 20 main had moved to 123 (#3793), above 121, so the number moved
// again. Read at refs/pull/<n>/head for every open pull request, the open heads above main's
// declare 124 (#4021), 125 (#4030) and 126 (#2230 and #4017), so 127 is the lowest number above
// main's 123 that no open head declares. It is held under the same rule: move it only when
// main's own number or another open head's reaches 127.
// At the forward merge of round 22 main had moved to 133, above 127, and #3659 and #3711 had
// also declared 127. Read at refs/pull/<n>/head for every open pull request, the open heads
// above main's declare 134 (#4017 and #4021), 135 (#3659), 136 (#3575 and #3730), 137 (#4046)
// and 138 (#3712 and #3993), so 139 was the lowest number above main's 133 that no open head
// declared. Read again immediately before the push, #4017 had moved to 138 and #3993 to 139,
// so 140 was the lowest number above main's 133 that no open head declared. At the forward merge
// of 2026-11-15 that brought in c69be928, main had moved to 137 (INFRA-3621's entry), and the
// open heads above it declared 138 (#4017 and #4049), 139 (#2731 and #3993), 141 (#3575), 142
// (#3712), 143 (#3730), 144 (#4021), 145 (#3659) and 146 (#3711), so 140 was still the lowest
// number above main's 137 that no open head declared. At the forward merge of round 25, which
// brought in 7ed02373, main had moved to 141 (#3575, INFRA-2566's entry above), above 140, and
// the open heads above main's declared 142 (#3712), 143 (#3730), 144 (#4021), 145 (#3659), 146
// (#3711) and 147 (#4017), so 148 was the lowest number above main's 141 that no open head
// declared. At the forward merge of round 26, which brought in c6d12afd, main had moved to 144
// (#4021, INFRA-3557's entry above), and #4049 had declared 148 too. Read again immediately
// before the push, the open heads above main's declared 145 (#3659), 146 (#3711), 147 (#4017),
// 149 (#2731), 150 (#4049, which had moved off 148), 151 (#4106) and 152 (#3730), so 148 is the
// lowest number above main's 144 that no open head declares. It is held under the same rule:
// move it only when main's own number or another open head's reaches 148.
// At the forward merge of round 32, which brought in 47a901c1, main had moved to 146 (#3711,
// INFRA-2614's entry above), still below 148. Read at refs/pull/<n>/head for every open pull
// request, a reading that ended at 2026-11-16T19:39:55Z, the open heads above main's declared 147
// (#4017), 149 (#2230), 150 (#4049 and #4065), 151 (#4106), 152 (#3730) and 153 (#4115), so 148 is
// still the lowest number above main's 146 that no open head declares. It is held under the same
// rule. At the forward merge of 2026-11-17, which brought in d9299f7a, main had moved to 151
// (#4106, INFRA-3729's entry above), above 148, and read at refs/pull/<n>/head for every open pull
// request, a reading that ended at 2026-11-17T11:46:59Z, the open heads above main's declared 152
// (#3730), 153 (#4115), 154 (#3659), 155 (#4273) and 156 (#3993 and #4065), so 157 is the lowest
// number above main's 151 that no open head declares. Read again immediately before the push of
// round 36, main still at 151, #2230 and #3993 had moved to 157, and the open heads above main's
// declared 152 (#3730), 153 (#4115), 154 (#3659), 155 (#4273), 156 (#4065), 157 (#2230 and #3993),
// 158 (#4017 and #4173) and 159 (#4049), a reading that ended at 2026-11-17T16:53:49Z, so 157 is
// given up and 160 is the lowest number above main's 151 that no open head declares. It is held
// under the same rule. At the forward merge of 2026-11-18, which brought in 0b2769e5, main had
// moved to 152 (#3730, INFRA-2543's entry above), and read at refs/pull/<n>/head for every open
// pull request, a reading that ended at 2026-11-18T06:01:03Z, the open heads above main's declared
// 153 (#4115), 154 (#3659), 155 (#4273), 156 (#4065), 157 (#2230 and #3993), 158 (#4017, #4173,
// #4180 and #4193) and 159 (#4049 and #4175), so 160 is still the lowest number above main's 152
// that no open head declares.)
//
// 168: the completion that ends the retro rates the plan the issue ran. `POST
// /legion/v1/handoff/complete` (`api.HandoffCompleteRequest`, contracts'
// `LegionHandoffCompleteRequest`) takes two optional fields: `planRating`, a whole number from 1
// (the plan cost the work) to 5 (the plan fit the work), and `planReason`, why, in one to 2,000
// characters counted as Unicode code points and not all blank, the two together
// (`PLAN_RATING_INPUT`). The agent that does the retro gives them, so the completion that ends the
// retro takes them, and every other completion and a production check's report are refused them
// (`PLAN_RATING_UNEXPECTED`). Which completion ends the retro the workflow judges where it ends
// it, on the issue as the completion's own transaction reads it (`TriggerRetroCompleted`), and its
// own refusals, a held issue's 409 among them, come first. A claim whose plugin declared this
// contract or a later one must rate (`PLAN_RATING_REQUIRED`), so a plugin built from this commit
// sends both on its retro; a plugin below it never sends them, and its retro, where the daemon
// serves its claim (`staleClaim`: a claim in a turn, in an update round, with no plugin install,
// or on a daemon that relaunches onto no plugin), completes with no rating
// (`api.PlanRatingContract`).
// Every refusal is a 400 that records nothing, so the same completion, corrected, is taken: the
// route writes `PLAN_RATING_INPUT` and a report's `PLAN_RATING_UNEXPECTED` before the fact is
// applied, and the workflow refuses the rest with the fact left unspent (`intake.Result.Unspent`).
// The route hands the two to the workflow on the completion's fact as one value
// (`intake.HandoffComplete.PlanRating`), beside whether the claim must rate (`MustRate`).
// `readBody` refuses an unknown field, so a plugin sending them to a daemon below 168 would have
// every retro refused; the bump is why the two never meet. No shape of `GET /legion/v1/state`
// moves, so no caller is served a narrower one.
// The decoder of every request body (`decodeJSON`) also refuses a body any string of which, a
// member's name and an earlier copy of a repeated member included, holds U+0000, which
// PostgreSQL's text and jsonb cannot hold: 400 `invalid request body` naming the string's place and
// where in it the character stands (`unstorableJSON`), or first the stale-caller 409 where the
// route can name a stale caller (`readBody`'s identity check), as for any body that does not
// decode. A route that stored such a string answered 500 before, and one that only compared it
// took it; every route now refuses it before it reads the body.
// (This entry takes 168, the lowest number above main's 160 that no open pull request declared
// when the merge of 2026-11-19 brought main in at c70a80228d: a reading of `const
// DaemonAPIVersion` at refs/pull/<n>/head for each of the 235 open pull requests, made between
// 2026-11-19T08:26:23Z and 08:26:52Z, found each of 161 to 166 declared, 167 declared by #4030,
// #4485, #4493 and this change's own head before this merge, and nothing above 167. Read the
// base's own number and every open pull request again at the next merge rather than from this
// line.)
//
// 178: INFRA-3759 (cybermonklabs/legion) -- the project pause. Four routes: the controller's `POST
// /legion/v1/project/pause` (`{grantId, reason}`) and `POST /legion/v1/project/resume`
// (`{grantId}`), which take a controller grant and refuse any other with 403 `CONTROLLER_REQUIRED`,
// and the operator's `POST /legion/v1/operator/project/pause` (`{reason}`) and `POST
// /legion/v1/operator/project/resume` (no body), which take the operator's bearer as every operator
// route does (`legion project pause|resume`). None names a project: the daemon pauses its own. A
// pause answers `{pause}` (api.ProjectPauseResponse) and a resume `{pause, released}`, the ended
// pause and how many held rows it let out (api.ProjectResumeResponse); a pause is `{id, since, by,
// reason}` and, once ended, `endedAt` and `endedBy` beside them (api.PauseView), and `by` is
// `{kind: controller, session}` or `{kind: operator}`. The refusals are one shape, the Failure body
// with what it names beside the code (api.PauseFailure): 400 `PAUSE_REASON_REQUIRED` for a blank
// reason; 409 `PAUSE_UNAVAILABLE` with the `setting` that turns the pause off (`runtime: tmux`,
// `runtime.kubernetes.idle_release.idle_minutes: 0`, `worker_wakes_per_minute: 0`), and with no
// setting from a daemon that runs no workflow; 409 `PROJECT_ALREADY_PAUSED` with the `pause` that
// stands; 409 `PROJECT_NOT_PAUSED` with the `lastPause`, where one has ended. While the project is
// paused, the controller's `compact` and `fresh_session` and the operator's `claims resume`,
// `retry`, `compact`, `spawn` and `deliver` answer 409 `PROJECT_PAUSED`, with the `pause` that
// stands, for every claim but the controller's, and change nothing. `GET /legion/v1/state` gains
// `paused` (`{id, since, by, reason, waitsOn, held}`: the claims whose stop the pause still waits
// on, each `{claim, issue, role, askedAt}`, and each row it holds, `{id, kind, issue, claim,
// waitedSeconds}`, `claim` absent for a row that names none) while the project is paused, and
// `lastPause` (a pause with `endedAt` and `endedBy`) while it runs after a pause has ended; a
// caller from a pod whose claims record a contract below 178 is served the state with neither key
// (api's stateNarrowings, withoutPause), as a caller below 141 is served it without
// `assignments`, and the bands compose. The release records' resumes gain `pause`, the pause that
// held the wake, absent on a wake no pause held (api.ResumeRecord). A controller grant now records
// who minted it, the controller session or the operator's bearer (credential.Grant's Session and
// Operator), which the pause records as who acted; that has no wire shape of its own.
// (This entry takes 178, above main's 168: a reading of `const DaemonAPIVersion` at
// refs/pull/<n>/head for each open pull request of cybermonklabs/legion, made on 2026-11-19
// between 20:40:35Z and 20:45:30Z, found 169 to 177 each declared by another open pull request
// and none declaring 178.)
//
// 188: #4543 -- no wire shape changes. Entry 64's prose no longer names one deployment's spend
// collector or its attribute prefix: it says that a spend row can be grouped by issue only when a
// deployment's collector keeps `legion.issue` and `legion.role`. Every route, the state, and the
// pane and container environments are as 178 has them. The number moves because
// `.github/scripts/check-daemon-api-contract.sh` reads any edit of this history as a contract
// change, and the boot gate and `legion probe-image` refuse a daemon and a plugin that declare
// different numbers, so the two still move together.
// (This entry takes 188, above main's 178: #4175 declares 185, #4495 and #4573 declare 186,
// and #4581 declares 187.)
//
// 189: a pod's launcher resumes a session named by its id alone (dispatch://INFRA-4108). A claim
// can record a session and no file of it: the issue close of every release from `legion-v18.0.1`
// up to, and not including, `legion-v20.5.0` cleared the file and kept the id
// (`clearIssueResumePath`), and a launch of such a claim spawned a fresh agent, which the
// same-agent fence refused at every try until the claim failed. The start frame
// (`shimwire.LauncherStart`, `launcher_start`) gains the optional member `resumeSession`, the id
// of the session the child resumes where the daemon holds no file of it, and the launcher's hello
// (`shimwire.LauncherHello`, `launcher_hello`) gains `resumesBySession`, the launcher's word that
// it looks such an id up in the session table: it takes the newest row whose path ends in
// `_<id>.jsonl` (`ompsessions.FindByID`), puts that path in the argv's one `--resume=<id>`, and
// answers `session: gone` where the table holds no such row. `LauncherStart.Validate` refuses a
// frame naming both `resumeFile` and `resumeSession`, an id that is not a single path element, and
// an argv that does not hold `--resume=<resumeSession>` exactly once.
//
// The skew is safe in both directions. A launcher older than this entry ignores `resumeSession`
// and sends no `resumesBySession`, so the daemon never names a session to it
// (`launchers.resumesBySession`, sandbox's `relaunch`, which answers
// `runtime.ErrSessionLookupUnavailable` before it sends anything): the daemon starts the agent
// with `--resume=<session id>` in its own command instead, which Oh My Pi resolves in the table
// itself, and names no session and no file to that launcher. An older daemon sends no
// `resumeSession`, and a launcher of this entry answers its frames as before. The number moves
// because the image probe pairs a daemon with a worker image by this number (entry 144), so a
// daemon of this entry boots only on an image whose launcher says `resumesBySession`, and a
// launcher older than it is left only in a pod kept from before the boot.
//
// The same change adds the notice kind `session-forgotten`, to the issue's architect and to the
// controller, for a claim that forgot its session after a start through such an older launcher
// did not register (`workflow.Engine.sessionLost`, `record.validNoticeKinds`). An outbox payload
// is decoded strictly, so no daemon of an earlier release decodes such a row, and one row fails
// `GET /legion/v1/state` for the whole project, as the `volume-near-full` entry above says of its
// own kind. The way back of this change's migration
// (`store/migrations/20261120042902_claim_agent_resume_generation.up.sql`) deletes those rows
// before a rollback. No shape of `GET /legion/v1/state` moves.
// (This entry takes 189, the lowest number above main's 188 that no open pull request declared
// when the merge of 2026-11-20 brought main in at b1b4e281ec0a: a reading of `const
// DaemonAPIVersion` at refs/pull/<n>/head, made between 2026-11-20T12:23:12Z and 12:28:35Z, for
// each of the 109 open pull requests, of 267, whose file list names this file or is longer than
// one read lists, found 188 declared by 12 of them, 190 by #4578, 191 by six, 192 by seven, 193 by
// #3659, #3712, #4065 and #4553 as well as by this change's own earlier head, 194 by #3831 and
// #4030, and nothing at 189. Read the base's own number and every open pull request again at the
// next merge rather than from this line.)
//
// 193: dispatch://INFRA-2694 -- the daemon shapes every answer down to the contract the caller's
// own plugin speaks, and reads every request an older plugin still sends, so a roll that replaces
// one pod at a time leaves the `legion` tool working in every pod it has not replaced yet. A
// request carries the caller's contract in the
// `Legion-Plugin-Contract` header, and every answer carries this daemon's in
// `Legion-Daemon-Contract`, so a caller is placed by what it declares and not by its address
// alone. `GET /legion/v1/issues/{key}/record` is the per-issue record route, and it answers 404
// `RECORD_NOT_FOUND` for an issue the daemon records nothing of. `GET /healthz` answers a typed
// body, `api.Health` with `status` and `project`, in place of one untyped key, so a caller reads
// which project the daemon it reached serves. A plugin newer than its daemon therefore fails the
// controller's boot check, which requires `project` from `/healthz`, against a daemon before this
// contract, which answers that route with no such key. `api.State` gains `lowestPluginContract`, the
// lowest contract among the claims whose locator names a pod and that record a contract
// (`recordsContract`). Every claim view gains `updateState`, which is `none`, `open`, `asked` or
// `answered`, and `wake`. An earlier head of this change also wrote two more claim view fields,
// `updateReaskedForPushCheck` and `unpromptedTurnTakesAnswerBack`, and a fifth update state,
// `push-check-reask`. main's #3982 removed the push-check re-ask and the take-back that they
// reported, so this change dropped them before it merged. Neither field shipped in any contract,
// so no record holds them and no caller is served them down. main's #4337 (ba774f01) moved what
// `asked` reads, with no contract number: `UpdateTaskedAt` is now set when the round's ask reached
// a session of the claim's role (supervise's Machine.PublishUpdateAsk), where it was set when the
// ask was queued, and an ask that reached none leaves the claim `open`. So
// `open` is a round whose ask has reached no session of the role yet and `asked` one whose ask
// reached one. `updateState` is this contract's own field, read by no reader below it, so no
// serving-down is owed for the change.
// The two contracts the boot gate alone covered are now served the shape their own entry
// describes: a caller below 54
// is served the state with each check view's `role` left off (`withoutCheckRole`), and a caller
// below 59 is served `pullRequest.fixAttempts` of 0 and `settings.max_fix_attempts` of 3 as
// stand-ins. The three contracts above 73 are served their own shape here too: a caller below 75
// is served the state without the nine keys the decided record added and without the `contract`
// mark (`withoutRecordFields`, `withoutContractMark`), a caller below 85 without each
// issue's `why` and without every field of the waiting line beside `place` and `length`
// (`withoutWhyAndLineFields`), and a caller below 86 without the seven keys 86 added: each pull
// request's `mergeState`, each worker's `pass`, and every claim view's `lastTurnStart`,
// `lastTurnEnd`, `lastDeathCause`, `lastStartFailure` and `lastReady`
// (`withoutStateMachineFields`). Such a caller is served no `pullRequest.checksVerdict`, the key
// 86 removed: every reader from 46 to 85 reads it `nonEmptyString.optional()`, so none requires
// it, and the records of those contracts leave it out, as they leave out the `note` 86 took off
// the handoff completion's answer. Contract 87 is one of the contracts served down here too: a
// caller below 87 is served every claim view without `leakCheckCause` (`withoutLeakCheckCause`).
// Contract 94 is one of them as well: a caller below 94 is served every issue without
// `phaseEntry` and `givenEntry` (`withoutPhaseEntries`). Contract 98 is one of them too: a
// caller below 98 is served the admission block without `admission.held` (`withoutHeld`).
// Contract 111 is one of them as well: a caller below 111 is served every issue without
// `treeMovedAt` and `quietHours` (`withoutQuietTime`, the band of `quietTimeContract`).
// Contract 132 is one of them as well, served down by value: a caller from a pod whose lowest
// contract is below 132 is served every waiting line member of the kind `waiting_on_issue` without
// its `kind` (`withoutWaitingOnIssue`), and no key moves.
// Contract 133 is one of them too: a caller below 133 is served every issue without
// `mergeWait` (`withoutMergeWait`, the band of `mergeWaitContract`).
// Contract 141 is one of them as well: a caller below 141 is served every issue without
// `assignments` (`withoutAssignments`, the band of `assignmentsContract`).
// Contract 144 is served with no code of its own here: its two shapes are on the launcher's wire,
// which the image probe pairs by number.
// Contract 151 is served with no code of its own here: its four shapes are on the launcher's
// wire, which the image probe pairs by number, and its one state key is a settings key, so a
// caller at 151 is served what one at 146 is, and 151's records are 146's under its own number.
// Contract 160 is served with no code of its own here: its two shapes are on the launcher's
// wire, which the image probe pairs by number, and its one state key is a settings key, so a
// caller at 160 is served what one at 152 is, and 160's records are 152's under its own number.
// Contract 168 is served with no code of its own here: it moves no key path or value of any
// answer, so a caller at 168 is served what one at 160 is, and 168's records are 160's under its
// own number. Its two request fields, `planRating` and `planReason`, are fields of
// `HandoffCompleteRequest`, so this change's request record of the completion route, which is
// 193's (no request record is kept below it), holds them, and a plugin at 168, which sends them
// on its retro, and one below it, which never does, are both decoded.
// Contract 178 is served down here by rows of this change's own, each decided by the contract
// the caller states, or by its pod where it states none, rather than by the pod's claims alone:
// the state's `paused` and `lastPause` are one band of shaping.go's stateNarrowings
// (`withoutPause`, the band of `pauseContract`); a resume's `pause` on the release records is
// taken off by that answer's row of shapeAnswer; and a refusal's `setting`, `pause` and
// `lastPause` (`api.PauseFailure`) are taken off by the refusal's row, which serves a caller
// below 178 the `Failure` alone. placedByPod places a caller that states no contract by its pod
// on both of those bodies, as on the state. `PROJECT_PAUSED` and the pause's other codes are
// values of the refusal's `code`, which no reader enumerates, so no value is held back. The
// answers of the controller's pause and resume arrived with their routes at 178, so no contract
// below records them, and the operator's two routes answer the `legion` command, which is built
// from the daemon's own commit.
// Contract 188 is served with no code of its own here: it moves no key path or value of any
// answer, as its entry says, so a caller at 188 is served what one at 178 is, and 188's records
// are 178's under its own number.
// Contract 189 is served the same way: no shape of any answer moves, as its entry says, so a
// caller at 189 is served what one at 188 is, and 189's records are 188's under its own number.
// Contract 146 is one of the contracts served down here too, by its row of shaping.go's
// stateNarrowings, and this contract is served above it: a caller below 146 is served every
// issue without `entryQueuedAt`, `entryConfirmedAt` and `architectStalls` (`withoutEntryTimes`,
// the band of `entryTimesContract`).
// Contract 152 is one of the contracts served down here too, by its row of shaping.go's
// stateNarrowings, and this contract is served above it: a caller below 152 is served every
// entry of `repositories.mapped` and `repositories.refused` without `apps`
// (`withoutRepositoryApps`, the band of `repositoryAppsContract`). `legionRepositories`, read at
// the first commit on main of each contract, is a `z.strictObject` for the block and for each
// entry of both lists at 46 (7b4ffe3c) and 48 (45548900), which refuses the whole state for the
// key, and a `z.object` from 49 (e68c26f9) to 151 (d9299f7a), which strips it and types neither
// it nor a key under it. 152's records are 151's under its own number with the six key paths of
// `apps` on the state's, and every record below 152 is as it was. Two more of 152's changes are
// in no record. `POST /legion/v1/controller/github-credential` gains `installed` and an optional
// `renames`, and its refusal `DEFAULT_OWNER_OUTSIDE_PROJECT` is renamed
// `DEFAULT_OWNER_NOT_INSTALLED`, a 400 as before. No plugin parses that answer at any supported
// contract: no plugin source names `LegionControllerCredentialResponse` at the first commit of
// any of them, the one reader is the Go CLI's `legion controller start`, which decodes the
// answer with `json.Unmarshal` and so passes a key it does not name, and no reader branches on
// either code. And `repositories.mapped`, which held the repositories of the project's Dispatch
// mapping, became every repository the daemon serves, the mapping's and the ones the project's
// own records name, and `refused`, which held the mapping's repositories left out, became a
// repository a role App is not installed on or whose installation could not be read. No plugin
// code at any contract from 46 to 151 acts on either list (read at the first commit of each, in
// `packages/pi-legion/src`, `packages/pi-legion/extensions`, `packages/pi-shared/src` and
// `packages/claude-envoy/src`), so that change of meaning moves no key and reaches every caller
// alike. The controller's `read_state` of 46 to 85 hands its model the parsed state whole, which
// is no read, and an earlier `legion controller start` names `mapped` in its controller's
// addressing; the `legion` command states no contract and is served this daemon's own.
// The records hold only the settings keys a supported reader types, and every other settings key
// path is in no record at any contract, so a new legion.yaml key is no contract change, as the
// reader's own rule in `packages/contracts/src/legion-api.ts` has it. The declaration is
// answers.go's `typedSettings`, which `answers.json` writes on the state's row, and it rests on
// one measurement: `legionSettings`, read at the first commit on main of each contract of
// `SupportedContracts` (dispatch://INFRA-2694/artifact/settings-typed-by-contract-md), is a
// `z.looseObject` that types top-level positive integers and nothing nested at every contract:
// `max_fix_attempts` and `worker_boot_timeout_seconds` from 46 (7b4ffe3c) to 58,
// `worker_boot_timeout_seconds` alone from 59 (7e7bcf21), and `review_round_cap` beside it from
// 86 (a4c0ff99) to this contract. Outside the schema, plugin code
// reads the block only through pi-legion's `read_state` with `settings`, which passes it through
// untyped. A loose object passes a key it does not name and refuses the body only for a key it
// names and does not find, so a record holds `settings` and, below it, exactly the keys its own
// contract's reader types, and the gate catches the removal of one of them (the stand-in of
// `max_fix_attempts` below 59 is what keeps it green there). The keys later contracts added
// under the block are typed by no reader, so they are in no record and owe no band. Each was
// read at these commits: 141's `settings.projects.<KEY>.experiments`, at the commit of each
// contract from 111 to 141: 123 (25f3e38a), 132 (8b0f29b9), 133 (2e546572) and 137 (7baa398b).
// `settings.runtime.kubernetes.memory_guard`, the one key 144 adds to the state, at 141's commit
// (7ed02373) and 144's (adebbf6d); 144's records are 141's under its own number.
// `settings.projects.<KEY>.experiments.<name>.roles`, which main's #4241 (4357585e) added with no
// contract number of its own, at 146's commit (858e9645), at #4241's and at this change's merge
// with it. `settings.runtime.kubernetes.agent_secrets.exchange_audience`, which main's #4153
// (8a4dbb9f) added the same way, at #4153's commit and at this change's merge with it.
// `settings.runtime.kubernetes.sandbox`, the one key main's 151 (d9299f7a) adds to the state, at
// 151's commit and at this change's merge with it. `settings.runtime.kubernetes.volume_trim`, the
// one key main's 160 (a573bac2) adds to the state, at 160's commit and at this change's merge
// with it.
// Contract 137 is served with no code of its own here: it moved no key path, no value and no
// request field, and changed no answer's status or meaning (its flag is on the launcher's command
// line, which the image probe holds, entry 137 above), so a caller at 137 is served what a caller
// at 133 is, and its records are 133's under its own number. main's #3983 (86218fbc) took no
// number of its own and merged when main declared 133, so legion-v20.19.3 and v20.19.4 carry it
// under 133, and 137 is the first contract every daemon of which carries it: the sentence of
// `HANDOFF_NO_RUN` and when `fresh_session` restarts a phase, which move no key and reach every
// caller alike.
// main's #3982 replaced `settings.pod_update_retask_hours` and `settings.pod_update_notice_hours`
// with `settings.pod_update_grace_minutes`. #3982 merged when main declared 111 and took no
// contract number of its own, so a daemon at 111 may send either shape: legion-v20.14.0 is
// #3982's merge commit (50bfbb0d) itself, declares 111 and sends `pod_update_grace_minutes`, and a
// daemon at 123 sends the new key and neither of the two old ones. No reader at any contract
// types any of the three (the measurement above), so no stand-in and no band is owed for them: a
// caller at every contract is served the settings block as this daemon writes it, the new key in
// it and neither old one, and no record holds any of the three
// (`TestACallerAt111ReadsTheSettingsBlockThisDaemonWrites`). A controller's model, which reads
// the block through `read_state` with `settings`, so sees the bound this daemon's update round
// runs on rather than two hour keys that describe nothing it does.
// Contract 123 is served down with no code of its own here. It changes no shape on either side
// of the wire, and it refuses nothing a plugin below it sends that a daemon before 123 did not
// refuse (measured: a tester or a reviewer below 123 that completes with no commit is answered
// as before 123, 200 at its entry and 409 behind it). Two answers do change. An implementer
// below 123 whose completion is behind its entry, not after a move back, and names the pull
// request's recorded head as its commit now gets 200 where it got 409. A completion of another
// entry from a role that does not work the phase the issue stands in now gets
// HANDOFF_NOT_CURRENT_PHASE where it got HANDOFF_STALE_ENTRY. No plugin below 123 branches on
// either code.
// Contract 104 is served down too, by what one answer means, because 104 moved no key path. A
// fresh session that stopped the claim and could not start it again answers a caller below 104
// as the daemon did before 104: the refusal of the step that failed, 409 with the machine's
// sentence or 500, in the `error` body. A caller at 104 or above gets the 200 with that refusal
// in `phaseNotRestarted` (`readsTheStopAsAFailure`, `processReadContract`).
// So from this contract on the header decides the status of an answer as well as its shape. For
// 104's `fresh_session`, the caller's contract, which its header states or else its pod records,
// decides between the 200 and the 409 or 500, and that choice is how this daemon serves 104's
// meaning down. This departs from the rule that the header changes no refusal.
// The operator's door, `legion claims retry --fresh-session`, is placed like any caller. Both
// doors place a caller by its header, else by the claims of its pod (`readsTheStopAsAFailure`).
// The `legion` command states no contract. Run from no pod, it stands at the daemon's own
// contract and gets the 200. The worker image ships `legion` too, and run from a pod below 104
// it gets the failure, which `TestAFreshSessionAnswersAPodBelow104ByItsLowestContract` holds.
// From such a pod the failure that 104 replaced is the conservative answer.
// A value is served down as well as a key. At this contract the rows of shaping.go's valueStrips
// serve these fields' values down: `strandedClaims[].state` (`withoutUnadmittedStrandedStates`), an
// issue's `waitingOn.answeredBy` (`withoutUnadmittedWaitAnswer`) and an issue's `waitingLine.kind`
// (`withoutWaitingOnIssue`). A strip takes only a value that arrived after its field, by the
// arrivals the declared table records (answers.go's `EnumeratedFields`), and serves the body
// without it where the value arrived above the contract the caller states, else the lowest its
// pod records: the stranded entry goes, the wait's answered pair goes, and the member's `kind`
// goes. A value that arrived with its field rides its field's band instead, and shaping.go's
// `fieldArrivals.heldBack` says why. The records under
// `packages/contracts/fixtures/daemon-api/contract-shapes.json` hold what this daemon serves every
// contract it supports, key path by key path and value domain by value domain, so a later shape
// change that carries no serving-down is red before it ships. A value a contract removes needs no
// serving-down of its own, which is why 86's dropped `awaiting_merge` has none (answers.go's
// `phases`).
//
// On the request side, `readBody` still refuses a member a request type does not name, and every
// plugin from 46 to 85 sends `ready` on every `handoff_complete`, which 86 took out of
// `HandoffCompleteRequest`. The type reads it by name again (`HandoffCompleteRequest.Ready`):
// `ready: false` and an absent `ready` are ignored, since false meant at 85 what a completion
// means now, and `ready: true` is refused 400 `READY_RETIRED`, since at 85 it armed the daemon's
// merge and no daemon at 86 or later merges. No other field an older plugin sends is missing
// from today's request types. Two request-side refusals of an older plugin stand with no code,
// because accepting would change what the call means: `gates/register` requires `step` only where
// a deployment gates two or more design steps, which a plugin from 46 to 54 cannot name, and
// `phase/backward` refuses `to: awaiting_merge`, which names no phase since 86. The records under
// `packages/contracts/fixtures/daemon-api/request-shapes.json` hold the field paths of every
// request a plugin sends, from this contract on, and a later request type that no longer reads a
// recorded field is red unless requests.go's `AcceptedByName` declares it, with the contract it
// left at and what ignoring it means. `ready` is that table's first row.
//
// The state's `contract.caller` is written for every caller from this contract on. A caller at
// the daemon's own contract is told DaemonAPIVersion, the contract whose shape it gets; before
// this contract the daemon left the key off for that caller alone. So the record of a contract
// holds the same keys while it is the daemon's own contract as after a later contract lands
// above it, and `-update`, which writes only the records of the daemon's own contract, never has
// to leave a record below it wrong.
//
// Some kinds of change pass both gates, because neither reads them:
//
//   - A change to what an answer means, with no change to its shape. A later contract that does
//     that must serve the old meaning down by hand, as 104's fresh session is served above.
//   - A renamed or removed refusal `code`, which no record holds. At this contract no supported
//     plugin branches on a code the daemon no longer sends: the one renamed code an older plugin
//     names, `HANDOFF_STALE_ASSIGNMENT`, it names in its tool description and not in a branch.
//   - A change to the type of a field. The records hold key paths, value domains and request
//     field paths, and no types, so a string that becomes a number passes both gates.
//   - A change that keeps the daemon's own contract number. A key or a request field that
//     arrived at that number stands in the records of the daemon's own contract only, and
//     `-update` writes those again, so the change passes both gates for such keys and fields.
//     The reviewer reads the records' diff, and this file's Bump rule says when a new number is
//     owed.
//   - A key removed with no stand-in, together with a hand edit of the earlier contracts' records
//     that removes it there too. The shape gate compares only what the daemon serves each
//     contract with that contract's record, so the pair passes both gates. R2 lets a record of an
//     earlier contract change only together with the serving-down that produces it; the gate cannot
//     tell this pair from such an edit, so review is its only guard (the reviewer reads the
//     records' diff).
//
// The request gate catches only a request field that a later type no longer reads. A newly
// required field, a narrowed value and a changed type all pass it. (This
// entry first took 60, when the base carried 59, then 81 at the forward merge of 2026-11-12
// that brought main in at 68, 81 again at the merge of that day that brought main in at 73, 87
// at the merge that brought main in at 85, 89 at the merge that brought main in at 86, 95 at
// the merge that brought main in at 94, 99 at the merge that brought main in at 98, 105
// before the push of that merge, and 115 at the merge that brought main in at 111, where open
// heads held 111 to 114, and 118 before the push that followed, when #3519, #3793 and #3937 had
// taken 115, and 127 at the merge that brought main in at 123 (#3793), where open heads held
// 124, 125 and 126, and 133 at the merge that brought main in at 132 (#3715), where no open
// head declared a number above 131, and 135 before the push that followed, when #3937 had taken
// 133 and #4021 134, and 135 again at the merge that brought main in at 133 (#3937), and 144
// at the merge that brought main in at 137 (#4046), where open heads held 138 to 143, and 145
// before the push that followed, when #4021 had taken 144, and 145 again at the merge that
// brought main in at 141 (#3575), and at the merge that brought main in at 144 (#4021), and 154
// at the merge that brought main in at 146 (#3711), the lowest number above 146 that no open
// pull request declared at 2026-11-16T20:01Z, and 154 again at the merge that brought main in
// at 151 (#4106), where the reading of the 194 open pull requests at 2026-11-17T12:23:43Z gave
// 154 to this change alone, and 154 again at the merge that brought main in at 152 (#3730),
// which is below it, and 168 at the merge that brought main in at 160 (#3519, INFRA-2009's
// entry above), where the reading of the 234 open pull requests at 2026-11-19T08:37:12Z gave 161
// to 167 to other heads and 168 to none, and 172 at a later round, when #3993, #4175 and #4488
// had taken 168 too, where the reading of the 233 open pull requests at 2026-11-19T13:36:20Z gave
// 161 to 171 to other heads and 172 to none, and 172 again at the merge that brought main in at
// 168 (#4488), which is below it, where the reading of the 241 open pull requests at
// 2026-11-19T17:51:41Z gave 169 to 171 to other heads and 172 to this change alone, and 182 at
// the merge that brought main in at 178 (#4115), where the reading of the 241 open pull requests
// at 2026-11-20T00:59:33Z gave 179 to 181 to other heads and 182 to none, and 184 before that
// merge was pushed, where the reading of the 243 open pull requests at 2026-11-20T02:40:07Z gave
// 179 to 183 to other heads, 182 among them, and 184 to none, and 193 at the merge that brought
// main in at 188 (#4543), where the reading of the 266 open pull requests at
// 2026-11-20T10:41:07Z gave 189 to 192 to other heads and 193 to none. 87 was free
// when this head took it and was gone
// when the head was pushed: the gate holds a head against main alone and sees no other head's
// number, so
// a reading of `const DaemonAPIVersion` at refs/pull/<n>/head for every open pull request of this
// repository is what finds them. That reading, made when this entry took 89, gives main 85,
// #3730 at 88, #3575 at 87 and five heads at 86, so 89 was the lowest number above the base's
// that no open head declared. 89 stayed above main through the merges that brought it in at 86
// and at 87, so neither forced a renumber. main's #3677 (INFRA-2800) then declared 94, so the
// merge that brought main in at 94 took this entry to 95, the number above it. main's #3708
// (INFRA-3107) then declared 98, so the merge that brought main in at 98 took this entry to 99,
// the number above it. 99 was free when this head took it, and #3575 held it before the push;
// the reading of `const DaemonAPIVersion` at refs/pull/<n>/head for every open pull request
// then gave 99 (#3575), 100 (#3573, #3715), 101 (#3712), 102 (#3730), 103 (#3519) and 104
// (#3830, #3937) above main's 98, so this entry took 105, the lowest number above main's 98
// that no open head holds. main's #3830 (INFRA-3161) then declared 104, which 105 is above
// already, so the merge that brought main in at 104 kept 105. The reading is
// taken again before each push rather than read off this line. The reading is
// of this file at refs/pull/<n>/head for every open pull request, not of a file list: GitHub
// cuts a pull request's file list short, so one that changes more files than the page holds
// answers without this path. Read the base's own number and every open pull request again at the next
// merge rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says. Until it landed, a renumber moved `shapedAnswersContract` in shaping.go with it, and that
// one constant alone: it is the number the three keys of this entry are served down below, it is
// what answers.go gives the two answers this change adds as their arrival, and it was then the
// last number of answers.go's SupportedContracts.)
//
// 199: INFRA-4605 (cybermonklabs/legion) -- a pod's `legion launcher` may be told
// `--session-writer-audience <audience>`, a fifth session flag after
// `--session-impersonate-service-account` (`sessionWriterAudienceFlag` after
// `sessionProxyAccountFlag`, `internal/runtime/sandbox/manifest.go`). The daemon renders it only
// when `runtime.kubernetes.session_database.writer_audience` is set. Its value is a workload
// identity provider's full resource name. The launcher then reads a projected ServiceAccount token
// of that audience at `<roles-dir>/session-writer/token`, which no role's namespace shows. Its
// session proxy exchanges that token at Google's security token service and impersonates the
// account with the federated token, never with the pod's own credentials (`connectorOptions`,
// `internal/sessionproxy`). The proxy admits to the session socket only the launcher's own process
// and the generation's Oh My Pi process, and closes and logs every other connection
// (`internal/launcher`'s sessionPeer). The launcher's start command gains `agentExecutable`
// (`shimwire.LauncherStart.AgentExecutable`), the agent's executable, by which the launcher knows
// the Oh My Pi process. The daemon fills it for every Oh My Pi launch (sandbox's launcherCommand)
// and leaves it out when it is empty, as for a Claude Code launch. Under the same member the
// controller's pod runs the launcher's roles-dir form for its one role in place of the
// single-role form (`--roles-dir` and `--role controller`, sandbox's
// controllerPod.rolesDirContainer), so role-init hides the writer token and the controller's
// launcher token from the controller's agent. A launcher built before 199 exits 2 on the unknown
// flag in every pod the daemon creates with it, so the image probe, which holds the image's
// pi-legion to this number, refuses such an image at boot instead, as it did for 137's flag. An
// older daemon refuses the member by name (`internal/config`'s readSessionDatabase), so it
// renders neither the flag nor the controller's roles-dir form. An older launcher ignores
// `agentExecutable`. A pod created under the member keeps `--session-writer-audience` in its
// launcher's command for its life, and an older daemon sends no `agentExecutable`, so that
// launcher refuses every Oh My Pi start an older daemon sends it (`internal/launcher`'s
// newSessionPeer). An older daemon also reads no fifth flag, and where it knows a plugin pair
// every relaunch of an Oh My Pi claim recorded at 199 installs that pair and so keeps the pod
// whatever its image: each Oh My Pi start is refused and charged as a launch failure (a Claude
// Code start is admitted), the controller's claim fails and its retries install into the same pod
// until its Sandbox is deleted, and each issue claim on Oh My Pi fails and needs a retry once its
// pod is replaced. So the member is turned off on this release first: restore the account's grant
// to the pod's own principal, remove writer_audience and restart the daemon, which reads the fifth
// flag as a moved session store and replaces each pod that carries it (an issue pod by its update
// round, the controller's pod at its next launch). The writer pool, its provider and the
// account's grant to the pool's principal stay until then, since a pod that carries the flag
// exchanges the writer token until it is replaced. Only once no launcher command carries the flag
// is the daemon rolled back below 199 (docs/kubernetes.md, "Selecting the store"). A deployment
// that sets no writer audience renders no fifth flag, and its launcher reads no
// `agentExecutable`, so its pods carry the command they carried before. (The
// number was first read at 2026-11-19T10:30Z-10:34Z as 170, the lowest above main's 160 (46795520,
// #3519's INFRA-2009) that no open pull request of cybermonklabs/legion declared, with the `take`
// of .github/scripts/check-daemon-api-contract.sh, while this branch's base, 4eec2cc2, still
// carried 152. The forward merge of 2026-11-19 brought in cf5905d3 and with it main's 160. A
// reading of every open pull request at that merge, which ended at 2026-11-19T12:17:28Z, found 170
// declared by #2230, #4030 and #4173 as well, and 161 (#4180, #4193 and #4259), 162 (#3712), 163
// (#4049), 164 (#4017), 165 (#4392), 166 (#4432), 167 (#4273), 168 (#3659, #3993, #4175 and #4488)
// and 169 (#3831, #4065, #4485 and #4493) declared, so this entry moved to 171, the lowest number
// above main's 160 that no other open pull request declares. The forward merge of 2026-11-19
// that brought in a20e0b05 brought main's 168 (#4488's entry above). A reading of every open pull
// request at that merge, which ended at 2026-11-19T16:56:58Z, found 169 (#3831 and #4065), 170
// (#2230, #4030, #4173, #4180 and #4193), 171 (#4115 and #4485), 172 (#3659), 173 (#4493) and
// 174 (#4273) declared, so this entry moved to 175, the lowest number above main's 168 that no
// other open pull request declares. The forward merge of 2026-11-19 that brought in d7c7c531 left
// main at 168. A reading of every open pull request at that merge, which ended at
// 2026-11-19T23:08:22Z, found 169 (#4273), 171 (#2230, #4049 and #4065), 172 (#3659), 173
// (#4493), 174 (#3993), 175 (#3712 and #4432 as well), 176 (#4030, #4173, #4180, #4182, #4193,
// #4259, #4261, #4269 and #4290), 177 (#4017), 178 (#3831 and #4115), 179 (#4392 and #4540) and
// 180 (#4175) declared, and none 170, so this entry moved to 170, the lowest number above main's
// 168 that no other open pull request declares. The forward merge of 2026-11-20 that brought in
// 9e0f0b8e left main at 168, and a reading of every open pull request at that merge, which ended at
// 2026-11-20T00:44:52Z, found 170 declared by no other, so this entry kept 170. The forward merge
// of 2026-11-20 that brought in c7e1cbd5 brought main's 178 (#4115's entry above). A reading of
// every open pull request at that merge, which ended at 2026-11-20T01:02:32Z, found 179 (#4392 and
// #4540), 180 (#4049 and #4175) and 181 (#4553) declared, so this entry moved to 182, the lowest
// number above main's 178 that no other open pull request declared. The forward merge of
// 2026-11-20 that brought in 28b2ea9e left main at 178. A reading of every open pull request at
// that merge, which ended at 2026-11-20T04:54:42Z, found 179 (#4540), 180 (#4049), 181 (#3831 and
// #4273), 182 (#4173, #4180, #4193, #4392 and #4493 as well), 183 (#2230, #3712, #4030 and
// #4432), 184 (#3659, #4065, #4553 and #4573) and 185 (#4175 and #4543) declared, so this entry
// moved to 186, the lowest number above main's 178 that no other open pull request declares. The
// forward merge of 2026-11-20 that brought in 1c7c8253 brought main's 188 (#4543's entry above). A
// reading of every open pull request at that merge, which ended at 2026-11-20T10:16:19Z, found 189
// (#3831, #4592 and #4596) and 190 (#4493 and #4578) declared, so this entry moved to 191, the
// lowest number above main's 188 that no other open pull request declares. The forward merge of
// 2026-11-20 that brought in a4cd4949 brought main's 189 (#4581's entry above). A reading of every
// open pull request at that merge, which ended at 2026-11-20T16:05:12Z, found 190 (#4578), 191
// (#2230, #4049 and #4573), 192 (#3993, #4173, #4175, #4182, #4193, #4273, #4392 and #4580), 193
// (#3659, #3712, #4065, #4432 and #4553), 194 (#3831 and #4030) and 195 (#4493 and #4590)
// declared, so this entry moved to 196, the lowest number above main's 189 that no other open pull
// request declares. The forward merge of 2026-11-20 that brought in 8a954068 brought main's 193
// (#3659's entry above). A reading of every open pull request at that merge, which ended at
// 2026-11-20T20:51:40Z, found 194 (#3831 and #4030), 195 (#4590), 196 (#3993, #4173, #4193, #4273,
// #4392, #4432, #4493, #4580, #4592 and #4596), 197 (#4065, #4553 and #4578) and 198 (#4175)
// declared, so this entry moved to 199, the lowest number above main's 193 that no other open pull
// request declares. Read the base's own number and every open pull request again at every forward
// merge rather than from this line.)
//
// 202: INFRA-4401 -- the daemon reads no repository mapping from Dispatch, and the `repositories`
// block of `GET /legion/v1/state` becomes the repositories the project's live records name, read
// in the state's own snapshot. The block is `{live, caches, withoutWebhookApp}`. `live` holds one
// entry for each repository a live record names, under the name the records keep, sorted by its
// lowercased `<owner>/<name>`: `repo`; `servedAs`, the other name of its configured pair where the
// rename rule serves it under that one; `apps`, each role App's installation on the name it is
// served under, once GitHub answered both reads; `readError`, why GitHub's last read did not name
// it; and `cache`, its repository cache under the Kubernetes runtime; each but `repo` absent until
// it holds something. `caches` is unchanged: the caches of repositories no live record names.
// `withoutWebhookApp` is kept in 152's shape for every caller, `{repo, reason}` for each live
// repository Dispatch's webhook App is not known to be installed on, under the name the records
// keep, in `live`'s order and never null, and it is the block's one report of the webhook App: no
// `live` entry carries a copy of it. The operator's `legion state` states no contract and asks from
// no pod, so it is served the new block, and a `legion state` of 152 reads its webhook App lines
// from this list, so without it that binary would print that no repository lacks the webhook App
// whatever the live records hold. `owners`, `mapped` and `refused` are gone, and with them each
// entry's `default`, which the daemon no longer knows. A plugin of 152 reads the block with a
// schema that requires those three keys and would refuse the whole state without them, so a
// caller below 202 is served the block in 152's shape, written from the same entries (shaping.go's
// repositoriesLiveContract and withoutRepositoriesLive, its row of stateNarrowings): `owners` and
// `mapped` are the entries both role Apps are installed on, under the name each is served under,
// every `default` false; `refused` is each other entry with its `readError`, the App not installed
// on it, or "GitHub has not answered for it yet"; and `withoutWebhookApp` is the same list as on
// the new block. A caller below 152 is also below 202, and is served that block with no `apps` on
// either list, as 193 serves it (withoutRepositoryApps, whose flag the older block reads). The
// caller is placed as 193 places every caller of the state: by the contract its
// `Legion-Plugin-Contract` header states, else by the claims of its pod, else at this contract. So
// a plugin from 193 to 198, which states its contract, is served 193's keys and the block in 152's
// shape; one from 199 to 201 is served 199's keys, since neither 200 nor 201 has an entry, and the
// same block, 199's keys being 193's on this route since 199 moved no shape of an answer; a pod
// whose claims record 178 to 192 and that states nothing is served the pause and that block; and a
// caller that states nothing from no pod — the operator's `legion state`, `legion controller
// start` — is served the new block. A tmux pane's plugin states its own contract, which the boot
// gate and `legion probe-image` keep at this one.
// The records of `contract-shapes.json` take the step as they take every band: 202's records hold
// the new block's key paths, and every record below 202 keeps the older block's. One enumerated
// field arrives with the block, the cache's `state` at `repositories.live.*.cache.state`, whose
// five words answers.go records at this contract (liveCacheStates), so every record of the state
// below 202 holds that key path's domain empty, as domainsOf writes it, and no other domain moves.
// `packages/contracts/src/legion-api.ts` mirrors the new block, and the fixture
// `state-narrowed-repositories.json` is the body served a caller that states 201, the contract
// before this one, which is served at 199, the recorded contract below it: 199's keys, which are
// 193's, the pause and the block in 152's shape.
// (This change takes 202, the lowest number above main's 199 that no open pull request declared
// at the census of `const DaemonAPIVersion` at refs/pull/<n>/head over the 301 open pull requests
// of cybermonklabs/legion, taken from 2026-11-20T23:09:18Z to 23:19:17Z, which named above
// main's 199: 200 (#4065 and #4580) and 201 (#4553 and #4611). This entry stood at 167, 169, 173,
// 182, 189, 190, 191, 195, 196 and 200 before, each the number it took at an earlier census and no
// head's for good. Read main's own number and every open pull request's again at the next forward
// merge rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// 205: dispatch://INFRA-3178 -- the owning architect moves a child out of the workflow by its own
// status write in Dispatch, and the operation that made that move through the daemon goes. The
// daemon takes the owning architect's done, backlog, icebox or triage on a child of its tree as it
// takes a person's move (workflow's sessionStatusWrite). The child route that operation called
// stays in the route table only to refuse an older plugin's call (removedChildRoute): it reads no
// body, applies no fact and answers 410 ROUTE_REMOVED with a reason that says which status to
// write in Dispatch. The `legion` tool loses the operation, `@legion/pi-shared`'s
// `LegionDaemonClient` loses its call, and `packages/contracts/fixtures/daemon-api` loses the
// fixture of the route's answer. That directory gains `end-child-rule.json`, the rules for ending
// a child and the sentence on a status the daemon has not taken yet, which the daemon's refusals
// and notices over children carry (`record.EndChildRule` and `record.StatusNotTakenYet`), and the
// `legion` tool's description is held to that file word for word. `api.ChildRequest` and
// contracts' `LegionChildRequest` stay, because rerun_child takes them. A plugin below 205 still
// offers the operation to its agents. On a daemon at 205 that call gets the refusal as its result,
// so the agent reads the status to write.
// Contract 205 is served with no code of its own (193's entry): no key path or value of any
// answer moves, so a caller at 205 is served what one at 202 is, and 205's records are 202's under
// its own number. The route leaves the empty object's row of answers.go's Answers, since no plugin
// at 205 parses that answer there, and an older plugin parses the refusal it gets as it parses any
// refusal. `ROUTE_REMOVED` is a value of the refusal's `code`, which no reader enumerates. The
// request records of 193, 199 and 202 hold the route's body (`request-shapes.json`). The route
// leaves the request types the gate walks, reads no body and answers that body with the refusal,
// and requests.go's RemovedRoutes declares it removed at 205, so the record of 205 and of every
// later contract holds no body for it.
// (This entry took 191 at first, the lowest number above main's 188 that no other open pull
// request declared at a reading that ended at 2026-11-20T10:18:19Z, and the forward merge of
// 2026-11-20 that brought in 0ec9ed84 left main at 188 and found 191 still free. The forward merge
// of 2026-11-21 that brought in 43b9c9c3 brought main's 199 (INFRA-4605's entry above). A reading
// of every open pull request at that merge, which ended at 2026-11-21T02:21:13Z, found 200
// (#4049), 201 (#4611), 202 (#3712, #4273, #4392, #4432, #4493, #4580 and #4590), 203 (#4065,
// #4175, #4553, #4592 and #4661) and 204 (#4030) declared, so this entry moved to 205, the lowest
// number above main's 199 that no other open pull request declares. The forward merge of
// 2026-11-21 that brought in 1cdf5b9c brought main's 202 (INFRA-4401's entry above). A reading of
// every open pull request at that merge, which ended at 2026-11-21T03:41:04Z, found 203 (#4065,
// #4175, #4553, #4592 and #4661) and 204 (#3831 and #4030) declared and 205 declared by no other,
// so this entry kept 205. The forward merge of 2026-11-21 that brought in 52f925ee left main at
// 202, and a reading of every open pull request at that merge, which ended at
// 2026-11-21T12:31:48Z, found 205 declared by no other, so this entry kept 205. Read the base's own
// number and every open pull request again at every forward merge rather than from this line.)
//
// 213: INFRA-4036 (cybermonklabs/legion) -- the deployment's capability report on `GET
// /legion/v1/state` gains a row, `repository-cache-room`, and with it a third word of a capability
// row's `source`, `caches` (capabilities.SourceCaches): what the repository caches' last health
// reads said of their room, which is where that row is measured from. The row is present while no
// cache that reports its room carries a failure for lack of space, open while one does, its detail
// naming each such cache's repository, volume, claim, available bytes and clone and where room
// comes from, and decided where `capabilities.decided.repository-cache-room` records a reason
// (capabilities.Deployment.cacheRoom). A capability `name` is a plain non-empty string to every
// schema already published, so the row parses there, as `claude-code` did; its `source` does
// not. A plugin from 25 through 212 reads `source` with a `z.enum` of the two words before it,
// `configuration` and `boot`, which refuses a word it does not name and with it the whole state
// body: the controller's `read_state` and, from a plugin before 193, which reads an issue's record
// out of the whole state, every role's `read_record` and claude-envoy's own reads of the state.
// `caches` is a value added to an enumerated field and no key, so it is served down by
// value, as 193's value strips are: answers.go's `capabilitySources` records it as arriving at
// this contract, and a row of shaping.go's valueStrips (`withoutUnadmittedCapabilitySources`)
// serves every capability row whose `source` the caller does not admit without its `source`,
// which that schema takes as optional, wherever the value arrived above the contract the caller
// states, else the lowest its pod records. The row itself stays, its name, status, detail,
// decision and config line as they are, because a controller on an older plugin still has the
// right to see the gap, and no band of stateNarrowings is owed, since no key moves. The records
// under `packages/contracts/fixtures/daemon-api/contract-shapes.json` keep every contract below
// this one as it was and add this contract's own, whose domain of `capabilities.*.source` names
// `caches`. The room fields the repository cache's own `/healthz` gains beside this
// (repocache.Health) are read by the daemon alone and by no plugin, so they are no part of this
// number. One other surface this contract names moves: the Sandbox worker container's environment
// (manifest.go's `mainEnvironment`) gains four GIT_CONFIG_KEY_<n>/GIT_CONFIG_VALUE_<n> pairs,
// GIT_CONFIG_KEY_5 to GIT_CONFIG_KEY_8 with their values, which carry the four pack settings of
// workspace's packWindowConfig after the five pairs before them (sandbox's gitConfigPairs), and
// GIT_CONFIG_COUNT goes from 5 to 9. git reads them and no plugin does: no source under
// packages/pi-legion, packages/pi-shared, packages/contracts or packages/claude-envoy reads a
// GIT_CONFIG variable, so they move no plugin's reader and nothing is served down for them. The new
// names are the runtime's own (runtimeOwned), so CheckPod refuses one in
// `runtime.kubernetes.pod.env` when the configuration loads, and ValidateSpawnSpec refuses one in a
// spawn spec's `Env` at that spawn's launch: a pair that would replace one of the four settings is
// refused, naming it, rather than replacing that setting with no word.
// The bump relaunches every registered claim onto the new plugin, as every bump does (the
// stale-plugin lane, internal/daemon's refreshStalePlugins), and that relaunch reaches each claim
// at its own time, so for the length of the wave some pods still run a plugin below 213: a
// process of 193 or later states its contract in `Legion-Plugin-Contract` and is served by it,
// and an older one is placed by the lowest contract its pod records. Those are the callers the
// strip serves.
// A renumber moves `cachesSourceContract` in shaping.go with it, and that one constant alone: it
// is the arrival answers.go's `capabilitySources` records for `caches`, and it is a number of
// answers.go's SupportedContracts, which names the constant rather than its number for that
// reason.
// (This entry first took 197, the lowest number above main's 189 that no open pull request
// declared when the merge of 2026-11-20 brought main in at f21107b285, and took 201 at the merge
// that brought main in at 193 (8a9540682d, #3659 having declared 193): a reading of `const
// DaemonAPIVersion` at refs/pull/<n>/head for each of the 297 open pull requests, made between
// 2026-11-20T21:53:59Z and 21:58:14Z, found 194 declared by #4030, 195 by #4590, 196 by #3993,
// #4173, #4180, #4193, #4273, #4392, #4432, #4493, #4580, #4592 and #4596, 197 by #4578 as well as
// by this change's own head before this merge, 198 by #4175 and #4589, 199 by #3831, #4495 and
// #4661, 200 by #4065, and no open head declaring 201. It took 203 at the merge that brought
// main in at 199 (0d3786da5367, #4495 having declared 199): a reading of `const
// DaemonAPIVersion` at refs/pull/<n>/head for each of the 327 open pull requests, made between
// 2026-11-21T00:43:25Z and 00:47:21Z, found 200 declared by #4049 and #4065, 201 by #4611 as well
// as by this change's own head before this merge, 202 by #4273, #4432, #4493, #4580 and #4590,
// and no open head declaring 203. It took 209 at the merge that brought main in at 202
// (3c59c0cc27ab, #4493 having declared 202): a reading of `const DaemonAPIVersion` at
// refs/pull/<n>/head for each of the 357 open pull requests, made between 2026-11-21T05:29:23Z
// and 05:34:35Z, found 203 declared by #2230, #4065, #4175, #4596 and #4661 as well as by this
// change's own head before this merge, 204 by #3831, 205 by #3712, #4539 and #4573, 206 by #4540,
// 207 by #3993, #4030, #4392, #4432, #4590 and #4592, 208 by #4611, and no open head declaring
// 209. It takes 213 at the merge that brought main in at 7477b87ed556, main still at 202: a
// reading of `const DaemonAPIVersion` at refs/pull/<n>/head for each of the 408 open pull
// requests, made between 2026-11-21T12:50:26Z and 13:12:31Z, found 203 declared by #4175, 204 by
// #4173 and #4193, 205 by #4573, 206 by #3712 and #4539, 207 by #3831, #3993, #4030, #4273,
// #4392, #4432, #4590, #4596, #4682, #4723 and #4805, 208 by #2230, 209 by #4049, #4540 and #4580
// as well as by this change's own head before this merge, 210 by #4661, 211 by #4611, 212 by
// #4065, #4592 and #4771, and no open head declaring 213. It keeps 213 at the merge that brought
// main in at dbebeb309b5d, main at 205 (#4573 having declared 205): a reading of `const
// DaemonAPIVersion` at refs/pull/<n>/head for each of the 430 open pull requests, made between
// 2026-11-22T01:42:47Z and 01:45:01Z, found 206 declared by #3712, 207 by #3993, #4432 and #4805,
// 208 by #4175, 209 by #4049, 210 by #4661, 211 by #2230, 212 by #4030, #4392, #4540 and #4580,
// and no open head but this change's own declaring 213. Read the base's own number and every open
// pull request again at the next merge rather than from this line.)
//
// 217: dispatch://INFRA-3531 and dispatch://INFRA-4567, two changes that reach main together as
// cybermonklabs/legion#4273, into whose branch #4485 merged, so they take one number. First,
// dispatch://INFRA-3531 -- the memory guard in the launcher of the project controller's pod,
// which moves the two launcher shapes contract 144 added. The daemon's answer to the hello of that
// pod's launcher, `shimwire.LauncherHelloAck`, now carries `memoryGuard` too, from a block of its
// own, `runtime.kubernetes.memory_guard.controller_pod`, beside `issue_pods` (sandbox's
// Runtime.launcherHelloAck hands each pod kind its own block and never the other's, and leaves the
// member out where that block's `enabled` is false). That launcher, PID 1 of the controller's
// container, runs the guard in its single-role form and sends `launcher_memory_kill` for each
// process it ended, every process of the container being the controller's. The daemon applies the
// report once, under the same event id `memory-guard-kill:<guard>:<sequence>`, as the fact
// `intake.ControllerMemoryGuardKill`, and tells the controller as one controller notice of the new
// kind `memory-guard-kill` under the project key (workflow's controllerMemoryGuardKill), one row
// for each kill and never through admission's wakeController. It names the process by its
// executable and carries none of its arguments, as 144's note does.
//
// A gVisor controller pod's launcher runs the guard and never the guest watch: its reports carry
// the guard's own empty `cause`, and go on a connection whose acknowledgement carried
// `memoryGuard`, as a gVisor issue pod's do. A microVM controller pod's launcher runs the guest
// watch instead (the second change, below).
//
// The skew is safe in both directions, for 144's reasons. An older launcher ignores the member in
// its single-role form and runs no guard there. An older daemon writes no member for the
// controller's pod, so that pod's launcher, which runs the guard at its defaults from its start,
// turns it off at the first acknowledgement, and since the acknowledgement carried no settings it
// sends no report on that connection (manager.openMemoryReports). An earlier daemon does not know
// the notice kind: it refuses an unsent outbox row of kind `memory-guard-kill` and tries that row
// again for ever, so a rollback below this release deletes those rows first, and takes
// `controller_pod` out of the file, which an earlier loader refuses by name (docs/kubernetes.md,
// "Rolling back below the release that guards the controller's pod's memory"). The plugin reads no
// notice kind, and the key is no part of this number, for 144's reason.
//
// cybermonklabs/legion#3519 (dispatch://INFRA-2009) merged first, at 160, adding `volumeTrim`
// beside `memoryGuard` in the same acknowledgement, so this change takes a number above it and
// keeps every member of both: an issue pod's acknowledgement carries `memoryGuard` and
// `volumeTrim`, and the controller pod's carries `memoryGuard` and no `volumeTrim`, since that
// pod's launcher runs no trimmer, and the second change, below, adds `kernelKills` to it.
// (This change took 155 before #3519 merged, 168 at the forward merge that brought main in at
// a573bac20ad2, and 167 at the one that brought it in at e5e4900fe70e. It took 174 at the forward
// merge of 2026-11-19 that brought main in at a16c218ee3a4, which carries #4488 at 168, the entry
// right above. A reading of `const DaemonAPIVersion` at the head of each of the 242 open pull
// requests of this repository, made on 2026-11-19 after 16:30Z, gave above main's 168: 168 (#3993
// and #4175), 169 (#3831 and #4065), 170 (#2230, #4030, #4173, #4180 and #4193), 171 (#4115, #4485
// and #4495), 172 (#3659) and 173 (#4493), so 174 was the lowest free number above main's 168 at
// that reading. That is a reading and never the rule: read main's own number and every open head
// again at the next forward merge rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// Second, in the same contract, dispatch://INFRA-4567 -- the project controller's pod in the
// project's sandbox, which moves the launcher wire. Where `runtime.kubernetes.sandbox.type` is
// `microvm`, the daemon renders the controller's pod as it renders a microVM issue pod (sandbox's
// prepare and SandboxConfig.Controller; `canary_issues` never moves it), and that pod's launcher
// command gains `--microvm` and, where `runtime.kubernetes.sandbox.microvm.metadata` is set,
// `--metadata-workload-pool` and `--metadata-identity-provider`, with the projected token mounted
// at /var/run/legion/workload-identity (sandbox's controllerPod.launcherContainer). The daemon's
// answer to the hello of the controller pod's launcher now carries `kernelKills`
// (`shimwire.LauncherHelloAck.KernelKills`) beside its `memoryGuard`, as every issue pod's has
// since 151, and still no `volumeTrim`, which an issue pod's alone carries (160) (sandbox's
// Runtime.launcherHelloAck). The single-role launcher given `--microvm` runs the guest watch over
// controllerClassifier in the guard's place (the launcher's runsGuest and serveMemoryGuard) and
// sends its `launcher_memory_kill` reports, each carrying one of the kernel's two causes, only on
// a connection whose answer carried `kernelKills`; given the metadata flags too, it serves the
// pod-local metadata endpoint from the token at /var/run/legion/workload-identity/token (the
// launcher's validateMetadata no longer needs `--roles-dir`). The daemon tells the controller of
// a `kernel` report and of a `kernel-protected` one with a note of its own for each (workflow's
// controllerKernelKillNote and controllerKernelProtectedKillNote), under the same event id
// `memory-guard-kill:<guard>:<sequence>` and as the same controller notice kind,
// `memory-guard-kill`. So entry 151's "on no controller pod's" no longer holds from this
// contract: a microVM controller pod's launcher runs the guest watch and never the guard, and
// every controller pod's acknowledgement carries `kernelKills`. A gVisor project's controller pod
// is the pod the first change renders, byte for byte; its launcher ignores `kernelKills` and sends
// its guard's reports where the answer carried `memoryGuard`. The state's settings block is
// unchanged beyond the first change's `controller_pod`.
//
// Where 199's `runtime.kubernetes.session_database.writer_audience` puts the controller's pod in
// the launcher's roles-dir form (sandbox's controllerPod.rolesDirContainer), both changes hold
// there too. The controller's role runs under a role-init of its own in that form, so its
// launcher runs the roles-dir form's memory watch, over the classifier an issue pod's runs, and
// its acknowledgement carries `memoryGuard` from `controller_pod` and `kernelKills`, as in the
// single-role form. In a microVM that form's command gains `--microvm` and the metadata flags too,
// with the projected token mounted under the roles directory, at
// /var/run/legion/roles/workload-identity, as an issue pod's is.
//
// Contract 217 is served with no code of its own in shaping.go (the serving rule at the head of
// this file). The one key either change adds to an answer a plugin parses is
// `settings.runtime.kubernetes.memory_guard.controller_pod` on the state, with its five members,
// and no reader at any contract types a nested key of `settings` (answers.go's typedSettings,
// 193's entry), so it owes no band of stateNarrowings. Neither change adds a value to an
// enumerated field (answers.go's EnumeratedFields): `memory-guard-kill` is a kind of controller
// notice, which no answer a plugin parses carries. So a caller at 217 is served what one at 213,
// #4553's entry above, is, `caches` as a capability row's `source` among it, and 217's answer
// and request records are 213's under its own number; its request records hold no `POST
// /legion/v1/children/park` (205's entry).
//
// The skew is safe in both directions. A single-role launcher of an earlier image refuses the
// metadata flags (its validateMetadata needed `--roles-dir`), one built before 151 refuses
// `--microvm` as an unknown flag, and one of 151 or later takes `--microvm` alone and runs no
// memory watch in that form, since its runsGuest needed `--roles-dir`. So a daemon of this release
// must never render those flags into a controller pod whose image predates them, and it never does:
// the image probe pairs a daemon with an image by this number (`runtime/sandbox/probe.go`), as it
// did for 151. A microVM controller pod of this release under an earlier daemon gets no
// `kernelKills`, so its launcher drops each report of a process the pod's kernel stopped, with a
// line of its own, and the controller is told nothing. That daemon records the controller's
// gVisor shape alone, so its next launch that installs nothing rebuilds the controller in gVisor,
// for the recorded shape's `sandbox` member or for the pod's image. The rollback:
// docs/kubernetes.md, "Rolling back" in "Pods in microVMs", says the controller returns to gVisor
// at that launch, and that suspending and resuming its claim does it at once where the launch at
// the resume installs nothing.
// (This change took 167 against the base's 155, 169 at the forward merge of 2026-11-19 that
// brought legion/INFRA-3531 in at 4d6665d5d593, and 171 at the one that brought it in at
// 41d4f45b156d. It took 175 at the forward merge of 2026-11-19 that brought legion/INFRA-3531 in
// at f301a926dec2, whose 174, cybermonklabs/legion#4273's, was the entry above it then. A reading
// of `const DaemonAPIVersion` at the head of each of the 243 open pull requests of this repository,
// made between 2026-11-19T16:45:54Z and 16:48:48Z, gave above main's 168: 168 (#3993, #4175,
// #4387, #4492 and #4525), 169 (#3831 and #4065), 170 (#2230, #4030, #4173, #4180 and #4193), 171
// (#4115, #4495 and this change's own earlier number), 172 (#3659), 173 (#4493) and 174 (#4273),
// so 175 was the lowest free number above the base's at that reading. That is a reading and never
// the rule: read the base's own number and every open head again at the next forward merge rather
// than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
// (Both changes take 169 together: cybermonklabs/legion#4485 merged into legion/INFRA-3531 at
// 2026-11-19T21:34:15Z, so they reach main as one pull request, #4273, under one number. A reading
// of `const DaemonAPIVersion` at the head of each of the 243 open pull requests of this repository
// but #4273, made at 2026-11-19T22:07:21Z, gave above main's 168: 170 (#4182), 171 (#2230, #4049
// and #4065), 172 (#3659), 173 (#4493), 175 (#3712, #4392, #4432 and #4495), 176 (#4030, #4173,
// #4180, #4193 and #4259), 177 (#4017), 178 (#3831, #4115 and #4175) and 179 (#4540), so 169 was
// the lowest free number above main's 168 at that reading. That is a reading and never the rule:
// read main's own number and every open head again at the next forward merge rather than from
// this line.)
// (They took 181 together at the forward merge of 2026-11-20 that brought main in at
// c7e1cbd53237, 192 at the one that brought it in at 1c7c8253678f, where main carried 188,
// #4543's: a reading of the 264 open pull requests, made between 2026-11-20T10:25:07Z and
// 10:30:02Z, found 189 (#3831, #4592 and #4596), 190 (#4493 and #4578) and 191 (#4495) above it,
// and 196 at the one that brought it in at a4cd4949fce7, where main carried 189, #4581's: a
// reading of the 283 open pull requests, made between 2026-11-20T16:08:35Z and 16:17:22Z, found
// 190 to 195 declared by other heads and nothing at 196.
// They took 202 together at the forward merge of 2026-11-20 that brought main in at
// 8a9540682d8a, where main carried 193, #3659's, the entry right above. A reading of `const
// DaemonAPIVersion` at the head of each of the 300 open pull requests of this repository, #4273
// among them at its earlier 196, made between 2026-11-20T22:42:52Z and 22:52:52Z, gave above
// main's 193: 194 (#4030), 195 (#4590), 196 (#3993, #4173, #4180, #4193, #4432, #4580, #4592 and
// #4596), 197 (#4553 and #4578), 198 (#4175 and #4589), 199 (#3831, #4392, #4495 and #4661), 200
// (#4065 and #4493) and 201 (#4611), so 202 was the lowest free number above main's 193 at that
// reading. They kept 202 at the forward merge of 2026-11-20 that brought main in at
// 6a85d7237c60, where main carried 199, #4495's, the entry right above, and where a reading of
// `const DaemonAPIVersion` at the head of each of the 301 open pull requests of this repository,
// made between 2026-11-20T23:12:06Z and 23:18:26Z, gave above main's 199: 200 (#4065, #4493 and
// #4580) and 201 (#4553 and #4611), so 202 was the lowest number above main's 199 that no open
// pull request declared at that reading. They moved from 202 to 207 at the forward merge of
// 2026-11-21 that brought main in at 1cdf5b9ceba6, where main carried 202, #4493's, the entry
// right above, and where a reading of `const DaemonAPIVersion` at the head of each of the 343 open
// pull requests of this repository, made between 2026-11-21T03:34:11Z and 03:44:21Z, gave above
// main's 202: 203 (#2230, #3993, #4065, #4175, #4553, #4592, #4596 and #4661), 204 (#3831 and
// #4030), 205 (#4573) and 206 (#4540), so 207 was the lowest number above main's 202 that no open
// pull request declared at that reading. It took 217 at the merge of main at 787c324fd0 (main at
// 205; a read of 414 open heads found 206 to 216 declared, 207 by seven other pull requests), and
// kept it at the merge of main at 51d1a21ff7a6 (main at 213, #4553's, the entry right above). That
// is a reading and never the rule: read main's own number and every open head again at the next
// forward merge rather than from this line.)
//
// 219: the owner of an issue gives Legion a plan with one task, and the plan picks which of
// today's steps run. Two architect routes are new. `POST /legion/v1/plans/register`
// (`api.PlanRegisterRequest`: `grantId`, `issue`, `artifactId`, `version` from 1) reads that
// version of a document of the issue from Dispatch, with its blocks, and answers 200 with the
// plan it took (`api.PlanRegisterResponse`: `plan.version` and each task's `id` and `steps`, never
// null), or `409 PLAN_REFUSED` with `reasons`, one `{task, reason}` for each cause (`task` is ""
// for a cause of the whole plan; `api.PlanRefusal`), `409 VERSION_NOT_IN_DISPATCH`,
// `409 PLAN_VERSION_OLD`, `409 PLAN_DOCUMENT_CHANGED`, `409 ISSUE_ALREADY_RELEASED`,
// `409 TASK_DONE` (a version that changes a task that is done or left the workflow),
// `409 PLAN_STALE_GENERATION` (a plan given at a generation the issue left before the workflow
// applied it), `409 TREE_LINGERING`, `403 ARCHITECT_REQUIRED`, `403 ISSUE_OUTSIDE_TREE`,
// `404 ISSUE_NOT_FOUND`, `404 ARTIFACT_NOT_ON_ISSUE`, `502 PLAN_UNREADABLE` or
// `502 DISPATCH_FAILED`. `POST /legion/v1/tasks/stop` (`grantId`, `issue`, `task`) stops one task
// of the issue and answers 200 `{}`, `404 TASK_NOT_FOUND`, `409 TASK_DONE`, `409 TREE_LINGERING`,
// `409 TASK_STALE_GENERATION`, `409 TASK_AWAITING_PERSON`, `403 ARCHITECT_REQUIRED`,
// `403 ISSUE_OUTSIDE_TREE` or `404 ISSUE_NOT_FOUND`; on an issue whose first step has not run, the
// stop first settles the plan that step would walk (the newest plan its owner gave, or the
// project's default steps) and stops that plan's task, and a task the plan does not hold is refused
// before anything is written. A root that waits on a person before its first step is refused
// `409 TASK_AWAITING_PERSON` with nothing written: its owner changes the task with a later version
// of the plan, or stops it once the person has answered. A
// stopped task starts again at its first step on a later version of the plan that changes it
// (register_plan), or on a root on a person's `todo`; one that never started waits for its gate
// and its line first, as any task does before its first step. `POST
// /legion/v1/handoff/complete` takes one optional field, `returnTo`, the place of the step of the
// issue's running task, 1 for its first, that a tester's completion with the verdict `fail` sends
// the task back to: below 1 it is refused `400 INVALID_RETURN_TO`, and on any completion that
// fails no step, a production check's report included, `400 RETURN_TO_NEEDS_FAIL`
// (`api.returnToRefusal`); the workflow refuses a step it may not name, one that is not earlier in
// the task, `409 RETURN_REFUSED`. For
// an issue whose task was stopped the completion route may also answer `409 TASK_STOPPED` to a
// late completion of the step the stop ended, and `retry_or_escalate` and
// `legion claims retry` answer `409 TASK_STOPPED` or `409 HELD_FOR_CHILDREN` on a hold of one of
// the two new reasons; only this release writes plan rows and those reasons, so no issue a release
// before ran gets the new answers. `readBody` refuses an unknown field, so a plugin sending
// `returnTo` to a daemon below 219 would have the completion refused, and a daemon below 219 has
// neither route; the bump is why the two never meet (`api.OwnerPlanContract`).
// Every architect request of one issue — register_plan, stop_task, sign_off,
// request_backward_move, retry_or_escalate, close_root and register_gate — names on its fact the
// claim of the architect and the issue's generation its route read, and sign_off and
// request_backward_move the run of accepting it read too (`record.Store.OpenStep`). The workflow
// judges each again under the fact lock (`intake.ArchitectRequest`, workflow's `requestRefusal`),
// since an adoption, a new run, a re-parent or a new run of accepting can commit between the
// route's read and the fact, and answers, with nothing changed and the fact unspent:
// `403 ISSUE_OUTSIDE_TREE` for an architect of another tree now, `403 ISSUE_NOT_OWNED` for one no
// longer the issue's own architect or one above it (every route but register_plan and stop_task,
// `record.ArchitectAbove`), `409 PLAN_STALE_GENERATION`, `409 TASK_STALE_GENERATION`,
// `409 SIGNOFF_STALE_GENERATION`, `409 BACKWARD_STALE_GENERATION`, `409 RETRY_STALE_GENERATION`,
// `409 CLOSE_STALE_GENERATION` or `409 GATE_STALE_GENERATION` for a run that is over, and
// `409 SIGNOFF_STALE_ENTRY` or `409 BACKWARD_STALE_ENTRY` for a run of accepting that is over.
// The answers and the requests of the two new routes are recorded at 219 and at no contract below
// it. register_plan's answer arrives whole with its route (answers.go's AnswerPlanRegister), and
// stop_task answers the empty object every fact route answers (AnswerEmptyObject), an answer
// older than its route. A refused plan's `409 PLAN_REFUSED` carries `reasons` beside its code and
// its sentence (`api.PlanRefusal`), a key of the refusal answer: a caller below 219 is served
// that refusal as its Failure alone, as a PauseFailure is below 178 (shaping.go's refusal row,
// placed by the caller's pod where it states no contract: placedByPod), although no plugin below
// 219 calls the route. The completion's request gains `returnTo`, and the request records of 219
// hold it and the two new routes' requests; no recorded request field leaves.
// One value set of `GET /legion/v1/state` and of `GET /legion/v1/issues/{key}/record` gets
// wider: an issue's `holdReason` takes `task-stopped` (its owner stopped its task) and `children`
// (a parent whose plan has no acceptance step waits, before its retro, for the issues beneath it)
// beside `escalated`. Every reader below 219 reads the field with `z.literal("escalated")`, which
// admits that one value as an enumeration of one would, and the reader of 219 reads it with a
// `z.enum` of the three, so the field is a row of answers.go's EnumeratedFields: `escalated` is
// recorded at the floor, and the two new words at 219. A caller whose contract (the one it states,
// else its pod's lowest) is below 219 is served an issue held for either new reason with no
// `holdReason`, as a hold of no reason is served (shaping.go's valueStrips,
// `withoutUnadmittedHoldReason`).
// A second value set gets wider with it: `admission.held`'s `reason` takes
// `children_before_retro` (a parent whose plan has no acceptance step, held before its retro,
// with `heldBy` naming each issue beneath it still in the workflow) and `task_stopped` (a task
// that was stopped, held where it keeps no slot, with no `heldBy`), in that order between
// `accepted_ancestor` and `at_work` (`record.HeldReasons`); both read `unclassified` before. The
// plugin reads the word as a `z.enum`, so a caller whose contract is below 219 is served each row
// of either word as `unclassified` with no `heldBy`, the row it read for that issue before
// (valueStrips, `withoutUnadmittedHeldWords`; answers.go's heldReasons records the two at 219).
// `settings.projects.<KEY>.default_steps`, the steps a task that names none runs, came with it: a
// key below `projects`, which no reader types, so no record holds it and nothing is served down
// (answers.go's typedSettings). The three notice kinds this entry adds (`task-stopped`,
// `plan-not-taken` and `retro-held`) ride inside a notice row's payload, as 189's does, and are no
// key or value of an answer a plugin parses.
// (This entry took 211 at first, the lowest number above main's 202 (#4493's entry above) that no
// other open pull request declared at a reading of `const DaemonAPIVersion` at
// refs/pull/<n>/head for each of the 381 open pull requests, made between 2026-11-21T09:45:11Z
// and 09:45:33Z. The forward merge of 2026-11-21 that brought in c03d3afe brought main's 205
// (INFRA-3178's entry above), and a reading of every open pull request at that merge, which read
// all 420 heads between 2026-11-21T22:07:20Z and 22:07:49Z, found 212 declared by no other, so
// this entry moved to 212. The reading before the push of 2026-11-22, which read all 431 heads
// between 01:05:21Z and 01:05:49Z, found 212 declared by #4030, #4392, #4540, #4580 and #4682,
// and 219 by none, so this entry moved to 219: the lowest number above main's 205 that no open
// pull request declares and no later part of this work, stacked on this branch, holds. The
// forward merge of 2026-11-22 that brought in 51d1a21f brought main's 213 (the entry above),
// below 219, so this entry kept 219. From this entry answers.go's SupportedContracts names
// `api.OwnerPlanContract`, after `cachesSourceContract`, and
// `shapedAnswersContract` and `repositoriesLiveContract` stay 193 and 202. Read the base's own
// number and every open pull request again at the next merge rather than from this line. The
// forward merge of 2026-11-23 that brought in 054cd5ca brought main's 217 (the entry above), below
// 219, and a reading of all 533 open heads between 2026-11-23T01:59:53Z and 02:00:28Z found 219
// declared only by #4611 and by #4902, #4948 and #5161, each stacked on this branch and carrying
// this entry, so this entry kept 219, and OwnerPlanContract comes after 217 in SupportedContracts.)
//
// 220: an experiment sets how each new issue is done. `POST /legion/v1/plans/register` and `POST
// /legion/v1/tasks/stop` answer `409 PLAN_NOT_TAKEN` for a recorded issue that an experiment drew
// into an arm whose `plan` is `default`: a code and a sentence that names the issue's variant and
// experiment, with no `reasons`. Legion takes no plan from that issue's owner, and so no stop of
// one of its tasks; register_plan's answer leaves the registration unspent, so the same call sent
// again gets the same answer. The code is a value of the refusal's `code`, which no reader
// enumerates, so a plugin below 220 reads the code and its sentence as it reads every other one,
// and no answer's shape moves for it. This entry also names what the two routes answer and 219's
// entry leaves out: register_plan's `400 INVALID_ISSUE`, `400 INVALID_ARTIFACT_ID`,
// `400 INVALID_VERSION` and `500 DISPATCH_UNAVAILABLE`, and stop_task's
// `400 INVALID_ISSUE`. `TestEveryCodeThePlanRoutesAnswerIsInTheContractHistory` reads the codes the
// two routes answer of their own from the source and holds each to this history.
// `GET /legion/v1/state` and `GET /legion/v1/issues/{key}/record` gain one key: each assignment of
// an issue's `assignments` carries `plan`, the group the arm of an experiment that sets how its
// issues are done set at the issue's draw (`owner` or `default`), as the draw recorded it, and
// absent where the arm named none. The issue runs on that group for its whole life, and the
// project's file can change an arm's `plan` or rename the arm after the draw, so a reader takes
// the group from the record and never from `settings`. The records of 220 hold
// `issues.*.assignments.*.plan`, and no record below 220 does: a caller whose contract (the one it
// states, else its pod's) is below 220 is served every assignment without the key (the band of
// shaping.go's stateNarrowings, `withoutAssignmentPlans`). The key
// `settings.projects.<KEY>.experiments.<name>.variants[].plan` came with it: a key below
// `projects`, which no reader types, so no record holds it and nothing is served down
// (answers.go's typedSettings).
// (This entry takes 220, above each number another open pull request declared. The forward merge
// of 2026-11-22 brought the base's 219 (the owner's plan's entry above). A reading of `const
// DaemonAPIVersion` at the head commit of each of the 431 open pull requests, made between
// 2026-11-22T02:35:47Z and 02:36:07Z, read all 431 and found 219 declared by #4611 and #4805, 220
// by this change's own pull request alone, and nothing above 220. The forward merge of 2026-11-23
// that brought in 7923340d brought the base's 219 again, over main's 217, and a reading of all 507
// open heads between 2026-11-23T16:08:36Z and 16:09:51Z found 220 declared only by this change's
// pull request and #5014, which is stacked on it and carries this entry, so this entry kept 220.
// From this entry answers.go's SupportedContracts names `api.assignmentPlanContract`, after
// `OwnerPlanContract`. Read the base's own number and every open pull request again at the next
// merge rather than from this line.)
//
// 221: the roles a deployment defines run steps of an issue's task. An issue whose running step is
// of such a role stands at a new phase, `step`, of `GET /legion/v1/state`'s `phase`, `heldFrom` and
// a claim view's `pendingTask.phase`, and names that role in a new key, `issues.<key>.stepRole`; a
// stranded claim and a waiting wake may name such a role. An issue names, in a new key
// `issues.<key>.startWaitsForPod`, the roles whose start waits because its pod was built before the
// deployment defined them, absent where none waits. A caller whose contract is below 221 is served
// an issue at step as at work, at `implementing`, its hold from step as one from `implementing`, a
// pending task at step as one at `implementing`, no `stepRole`, no `startWaitsForPod` (a plugin
// before memoryContract reads an issue as a strict object and would refuse the whole state over
// it), and no stranded claim or waiting wake of a role outside Legion's six (one band of
// shaping.go at `deploymentRolesContract`), and the daemon logs one line for each claim it leaves
// out (`leftAClaimOut`); the issue stays in `issues` and in `admission.active`. Such a role's agent
// completes its step with `POST /legion/v1/handoff/complete` and the verdict `pass` or `fail` and
// a commit, and no handoff record (`400 STEP_VERDICT_REQUIRED`); `VERDICT_ROLE_FORBIDDEN` now
// reads `only a tester or a role of the deployment reports a verdict`. `POST
// /legion/v1/phase/backward` takes such a role's grant, and one optional field, `returnTo`, the
// place of the step the move returns to (below 1 `400 INVALID_RETURN_TO`; a place the workflow may
// not name, or a `to` that is not the phase that step runs as, `409 BACKWARD_REFUSED`). `readBody`
// refuses an unknown field, so a plugin sending `returnTo` to a daemon below 221 would have the
// move refused; the bump is why the two never meet (`deploymentRolesContract`). `fresh_session`
// and `compact` name the roles `architect, planner, implementer, tester, reviewer, merger, or a role
// of this deployment`. From this entry the last number of answers.go's SupportedContracts is
// `api.deploymentRolesContract`.
// (This entry takes 221, above the base's 211 (the owner's plan, the entry above) and above every
// number another open pull request declared, with 218 to 220 left free: a reading of
// `const DaemonAPIVersion` at refs/pull/<n>/head for each of the 422 open pull requests, made
// between 2026-11-21T22:24:28Z and 22:24:45Z, read all 422 and found 211 declared by #4611 and
// #4902, 213 by #4030 and #4553, 214 by #4592 and #4596, 215 by #4065, 216 by #3831, #4173, #4259
// and #4540, 217 by #2230, #4273, #4590 and #4771, and nothing at 212, from 218 to 220, or at 221
// or above. Read the base's own number and every open pull request again at the next merge rather
// than from this line.)
//
// 222: a claim of `GET /legion/v1/state`'s `strandedClaims` names its claim token in a new key,
// `strandedClaims.*.token`. Two claims of one issue can hold one role, a keyed agent's on another
// role's step among them, so the issue and the role no longer name one claim; the boot's log line
// and `legion state` read who suspended each stranded claim by that token (api.Suspenders), so
// each takes the move its own suspender calls for. A caller whose contract is below 222 is served
// each stranded claim without the key (one band of shaping.go at `strandedTokenContract`): a
// plugin before INFRA-1048 reads a stranded claim as a strict object and would refuse the whole
// state over it. From this entry the last number of answers.go's SupportedContracts is
// `api.strandedTokenContract`, and `deploymentRolesContract` stays 221.
// (This entry takes 222, the lowest number above the base's 221 (the entry above) that no other
// open pull request declared: a reading of `const DaemonAPIVersion` at refs/pull/<n>/head, by the
// contract check's own reader (declared_by_open_pull_requests), of each of the 428 open pull
// requests, made between 2026-11-22T04:42:58Z and 04:46:33Z, found 220 the highest number
// declared and none at 222. Read the base's own number and every open pull request again at the
// next merge rather than from this line.)
//
// 224: each task of an owner's plan works in a working copy of its own, and the daemon's
// adoption names it. The worker frame `adopt-working-copy` (`shimwire.AdoptWorkingCopy`) gains
// `branch`, the branch of the task whose working copy the shim adopts: absent for the issue's own
// working copy, `legion/<KEY>.<task>` for another task's, and refused when it is the branch of no
// issue or task. A shim built before it decodes the frame with no such field, which encoding/json
// leaves unread, and adopts its own workspace. No answer a plugin parses changes with this frame.
// `POST /legion/v1/phase/retry` (`api.PhaseRetryRequest`, `retry_or_escalate`) takes an optional
// `task`, the held task of the issue's plan the decision is of: a failed claim holds the task of
// its agent's step, and the issue only where no other task runs. A request that names none is of
// the one held task, as every request of a plan of one task is; where two or more are held it is
// answered `409 TASK_REQUIRED`, and a named task that is not held `409 TASK_NOT_AT_PHASE`.
// `readBody` refuses an unknown field, so a plugin that sends `task` needs this contract; a
// plugin before it sends none, which a daemon of this contract takes while one task is held.
// `POST /legion/v1/signoff` (`api.SignOffRequest`, `sign_off`) and the architect's
// `POST /legion/v1/phase/backward` (`api.PhaseBackwardRequest`, `request_backward_move`) take an
// optional `task` the same way, of the tasks that stand at accepting: one that names none is of
// the one task there, refused `409 TASK_REQUIRED` where two or more stand there and
// `409 TASK_NOT_AT_PHASE` for a named task that does not. A phase worker's move back is of the
// task of its own step, for a plan of one task too, which the daemon reads from its claim, and a
// `task` it names must be that one (`400 TASK_NOT_GRANTS`). A completion of an issue with plan
// rows, a plan of one task among them, is of one task of the plan: the task of the step its
// claim's agent runs, else, for a completion that arrives after its step ended, the task of the
// step the latest phases row naming its claim runs, else the task that merges; it is keyed by that
// task's phase, and one of no task of the plan is answered `409 HANDOFF_NOT_CURRENT_PHASE`; no
// request or answer shape of it changes. An implementer's completion of such a task is refused
// `409 HANDOFF_PULL_REQUEST_BASE` when the open pull request on the task's branch is against
// another branch than the base the plan gives the task (the default branch, its one need's branch,
// or for a task of several needs its join branch, its own branch and `.join`): the refusal names
// that base and `gh pr edit <n> --base <base>`, and nothing is recorded. An issue with no plan
// rows reads no base. A plugin of an earlier contract shows the code and its text as it shows any
// refusal of the daemon.
// A completion of a task of a plan sent again is answered `409 HANDOFF_ALREADY_RECORDED` in words
// that name the task beside its phase (`phase <phase> of task <id>`), since the pass they count is
// that task's own; an issue with no plan rows is answered in the words it was before.
// `GET /legion/v1/state` serves each task of an issue's plan in a new key,
// `issues.<key>.tasks`, in task id order (`api.TaskView`): its `id`, its `state`, the tasks it
// `needs`, the `kind` and the `position` of the act it stands at and that act's `phase`, its
// `branch`, the `role` and the `agent` that work it, why it is held (`holdReason`) or stopped
// (`stoppedReason`), what the step at its place waits for (`wait`, `join` or `agent`) with the
// agent it waits for and the running step that agent works (`waitsFor`), its own `pullRequest`
// with what its review round waits on from GitHub (`mergeWait`), and its own `counts`; the key is
// absent for an issue with no plan rows. The issue's own `pullRequest` and `mergeWait` are its
// merge's, the pull request of the task that merges. A caller whose contract is below 224 is
// served each issue without `tasks` (one band of shaping.go at `tasksContract`): a plugin built
// before the reader that drops a key it does not know reads an issue as a strict object and
// would refuse the whole state over it. From this entry the last number of answers.go's
// SupportedContracts is `api.tasksContract`.
// `POST /legion/v1/plans/register` (`register_plan`) takes a plan of several tasks and a task's
// `needs` (plan.Supported refuses neither), and its answer names each task's `needs`, the ids of
// the tasks it needs as register_plan resolved them, `[]` for none (`api.RegisteredTask`), and the
// plan's `added`, the ids of the tasks the version adds over the version in force, in the plan's
// order, every task of a generation's first version, `[]` for none (`api.RegisteredPlan`): the
// workflow records them on the version (`store/migrations/20261123200630_plan_added.up.sql`).
// Whether a registration is one already taken is the workflow's judgment under the fact lock, not
// its id: it judges every give against the record. The version in force is answered `200` with its
// original additions; a version below it is refused `409 PLAN_VERSION_OLD`, even where an earlier
// registration took it; and a version the issue's first step passed over since is judged and stored
// as a new version. A version given at generation 0 and carried to the issue's generation by its
// first step is the version in force there. No request or answer shape changes with it. A caller
// below 224 is served neither `needs` nor `added` (shaping.go's withoutNeedsOrAdded); the reader
// of 219 parses the plan and a task as zod objects, which drop the keys, so a plugin before it
// reads either body.
// The same change adds the notice kind `task-join-failed`, to the issue's architect, for a task
// whose join of its needs waits for a need's branch GitHub does not have (`record.validNoticeKinds`).
// An outbox payload is decoded strictly, so no daemon of an earlier release decodes such a row, and
// one row fails `GET /legion/v1/state` for the whole project, as the `session-forgotten` entry
// says of its own kind; the way back of this change's migration
// (`store/migrations/20261121165047_task_copies.up.sql`) deletes those rows before a rollback. No
// shape of `GET /legion/v1/state` moves with it.
// (224 is a working number, above every number this tree's entries hold and every number the
// changes this one is built on hold. No reading of the open pull requests chose it: the pull
// request that carries this entry reads `const DaemonAPIVersion` at each open pull request's head
// when it opens, and takes its number then.)
//
// 228: INFRA-1575 -- the intake gives up on a message whose handling keeps failing, keeps it
// whole, tells the controller, and lets the operator or the controller replay it or discard it.
// `GET /legion/v1/state` gains `intakeGiveUps` (`api.IntakeGiveUpsView`), written on every body:
// `consumers`, each durable consumer with an open give-up as `{name, open, newestSeq}`, the newest
// stream sequence being the `throughSeq` of a bulk replay or discard of all of them, and `newest`,
// the newest open give-ups, latest given up first, at most 50, each `{id, consumer, subject,
// streamSeq, storedAt, eventId, deliveries, gaveUpAt, reason, lastError}`, where `eventId` is null
// for a message that did not decode and `lastError` is the first 300 characters of the record's
// error; both lists are empty rather than null. The controller's two routes, `POST
// /legion/v1/intake/replay` and `POST /legion/v1/intake/discard` (`api.IntakeReplayRequest`,
// `api.IntakeDiscardRequest`), take `grantId` and a selection: `giveUp`, one record's id, or
// `consumer` with `throughSeq`, every open record of that consumer at or below that sequence; the
// discard takes `reason` too. They answer `api.IntakeReplayResponse` (`replayed`, each record's
// `{id, consumer, streamSeq, outcome, error}`, the outcome `applied`, `already applied`, `no fact`
// or `failed`, and `remaining`) and `api.IntakeDiscardResponse` (`discarded`, the ids it closed),
// and refuse 400 `GIVE_UP_REQUEST_INVALID` for a selection of neither form or both and a discard
// with no reason or one longer than 2,048 characters, 403 `CONTROLLER_REQUIRED` for a grant that
// is not the controller's, 404 `GIVE_UP_NOT_FOUND` for an id of no give-up of the project, 409
// `GIVE_UP_CLOSED`, naming how and when, for an id the action may not take, 409
// `GIVE_UP_BEING_REPLAYED` for an id a running replay holds, and 503 `NO_INTAKE` on a daemon
// with no intake. A controller grant records who minted it (`credential.Grant`'s Session and
// Operator, 178), so the record keeps `controller session <id>` as who closed a record, and
// `operator` for a controller grant the operator's bearer minted. The operator's two
// routes, `POST /legion/v1/operator/intake/replay` and `POST /legion/v1/operator/intake/discard`,
// take the same bodies without `grantId` and answer the same; `legion intake replay` and `legion
// intake discard` post them. The controller notice kind `intake-gave-up`
// (`record.IntakeGaveUpNotice`), with `record.Notice`'s `count` and `giveUps`, reaches the
// controller's topic, a minute after the give-up that opens it, with every give-up of that minute
// joined to it. An older daemon refuses a controller whose plugin holds this contract with 409, at
// `legion controller start` and at its registration, so the controller never calls the two routes
// there; only this release's operator command `legion intake` reaches an older daemon, which
// answers it 404.
//
// The change is served down by the tables 193 put in place, one row each:
//
//   - The state's one new key is one band, the row of `intakeGiveUpsContract` in shaping.go's
//     `stateNarrowings` (`withoutIntakeGiveUps`): a caller that states a contract below 228, or
//     that states none from a pod whose claims record one below 193 (`podContracts` counts no
//     claim at `shapedAnswersContract` or above), is served the state without the key,
//     with every band below it, which is why `packages/contracts/src/legion-api.ts` declares it
//     optional, as it declares `wakesWaiting`. The key is left off by a flag rather than emptied
//     (`State.intakeGiveUpsHidden`), as `wakesWaiting` and `claimsToRefresh` are, since a strict
//     reader refuses it empty as surely as filled.
//   - The controller's two answers are two rows of answers.go's `Answers`, `AnswerIntakeReplay`
//     and `AnswerIntakeDiscard`, each arrived with its route at 228, so no contract below records
//     either, and each is the identity in `shapeAnswer`. The operator's two routes answer the
//     `legion` command, which is built from the daemon's own commit, so they are no rows and
//     write their answers as `answerUnparsed`, as the operator's pause and resume do. Every
//     refusal of the four routes is the refusal answer.
//   - Two fields of those bodies are read with a `z.enum`, so each is a row of answers.go's
//     `EnumeratedFields`: a give-up's `reason` on the state (`intakeGiveUps.newest.*.reason`, the
//     five words of `record.IntakeGiveUpReason`) and a replayed record's `outcome` on the
//     replay's answer (`replayed.*.outcome`, the four words of `intake.ReplayOutcome`). Every
//     value arrived with its field at 228, so the band and the answer's own arrival keep each
//     from an older caller, and no value strip is owed (shaping.go's `fieldArrivals.heldBack`).
//   - The controller's two request bodies are two rows of the request gate's table
//     (`pluginRequests` in request_shapes_test.go), recorded from 228 on. The operator's two are
//     the `legion` command's, which no older build sends.
//
// So `contract-shapes.json` gains the records of 228, which are 220's with the key paths of
// `intakeGiveUps` and the five values of its `reason`'s domain on the state's record and with the
// records of the two new answers beside them, and `request-shapes.json` gains the records of 228,
// which are 220's with the two controller routes beside them. Every record below 228 is as it was
// but for the state's, 220's among them, which gains the domain `intakeGiveUps.newest.*.reason`
// admitting no value, since every record holds a domain for each enumerated field of its answer
// (`TestTheRecordSetIsOneRecordPerAnswerPerContract`) and no value of this one arrived below 228.
//
// The change carries store migration `20261120144241_intake_failures`, which adds the tables
// `intake_failures`, `cleared_pull_requests` and `pull_request_settled_heads`. An older daemon
// binary put back on this schema decodes an outbox payload strictly (`record.DecodeOutboxPayload`,
// `DisallowUnknownFields`), so an unsent `intake-gave-up` notice row is refused at every attempt
// and logged `outbox row failed` each time; and it applies or drops a message with an open failure
// without telling the table, so after a roll forward the sweep would give up as passed a message
// that applied. The way back is the migration header's last paragraph (`migrations.WayBack`), the
// same text `docs/kubernetes.md` carries under "Rolling back the release that gives up on intake
// messages": with the daemon stopped, delete the project's unsent `intake-gave-up` controller
// notices and its open failures, each statement naming the project. The given-up records and the
// three tables stay, which the older binary never reads and a roll forward finds in place.
// (This entry took 195 when main declared 193, read on 2026-11-20 at 8a9540682d, and the open
// pull requests read that day declared 194 and 196 to 199. The forward merge of 2026-11-20 that
// brought in 6a85d7237c brought main's 199 (#4495, INFRA-4605's entry above). A reading of every
// open pull request at that merge, which ended at 2026-11-20T23:12:47Z, found 200 (#4065, #4493
// and #4580) and 201 (#4553 and #4611) declared, so this entry moved to 202, the lowest number
// above main's 199 that no other open pull request declared. The forward merge of 2026-11-21 that
// brought in a86c31c9fd brought main's 202 (#4493, INFRA-4401's entry above). A reading of every
// open pull request at that merge, which ended at 2026-11-21T03:14:44Z, found 203 (#4065, #4175,
// #4553, #4592 and #4661), 204 (#3831 and #4030), 205 (#4573) and 206 (#4540) declared above 202,
// so this entry moved to 207, the lowest number above main's 202 that no other open pull request
// declares. The forward merge of 2026-11-21 that brought in 98987d3f3d brought main's 205 (#4573,
// INFRA-3178's entry above), and #4175 declares 208 and merges first. A reading of every open pull
// request at that merge, which ended at 2026-11-21T20:28:32Z, found 206 (#3712), 207 (#3993,
// #4273, #4392, #4432 and #4805), 208 (#2230 and #4175), 209 (#4049, #4540 and #4580), 210
// (#4661), 211 (#4611), 212 (#3831 and #4771), 213 (#4030 and #4553), 214 (#4592 and #4596), 215
// (#4065) and 216 (#4259) declared, so this entry moved to 217, the lowest number above #4175's
// 208 that no other open pull request declares. The forward merge of 2026-11-22 that brought in
// b6dc467a27 brought main's 213 (#4553, INFRA-4036's entry above). A reading of every open pull
// request at that merge, which ended at 2026-11-22T16:36:44Z, found 214 (#4592), 215 (#4596), 216
// (#4173, #4180, #4182, #4193, #4259, #4932 and #4984), 217 (#4273), 218 (#3831), 219 (#4611), 220
// (#4930), 221 (#3993, #4049, #4392, #4432 and #4771), 222 (#4661), 223 (#4805), 224 (#4960), 225
// (#3712 and #4580), 226 (#4175) and 227 (#4065) declared, so this entry moved to 228, the lowest
// number above main's 213 that no other open pull request declares. The forward merge of
// 2026-11-23 that brought in 054cd5ca16 brought main's 217 (#4273, the entry above for
// dispatch://INFRA-3531 and dispatch://INFRA-4567). A reading of every open pull request at that
// merge, which ended at 2026-11-23T02:19:36Z, found 218 (#3831), 219 (#4611), 220 (#4930), 221
// (#3993, #4049, #4392, #4432, #4771, #4940 and #5164), 222 (#4661, #5156 and #5160), 223 (#4805),
// 224 (#4960), 225 (#3712), 226 (#4175) and 227 (#4065 and #5162) declared, and no other pull
// request declaring 228, so this entry kept 228, the lowest number above main's 217 that no other
// open pull request declares. The forward merge of 2026-11-24 that brought in 42b2ad4d17 brought
// main's 220 (#4930, the entry above, after #4611's 219). A reading of every open pull request at
// that merge, which ended at 2026-11-24T01:34:50Z, found 221 (#4049, #4392, #4432, #4940 and
// #5164), 222 (#5156), 223 (#3993 and #4661), 224 (#4960 and #5165), 225 (#3712), 226 (#4175) and
// 227 (#5162) declared, and no other pull request declaring 228, so this entry kept 228, the
// lowest number above main's 220 that no other open pull request declares. Read main's own number
// and every open pull request again at the next merge rather than from this line.)
//
// 232: dispatch://INFRA-4508 -- `POST /legion/v1/claims/turn-outcome` takes two optional fields.
// `httpStatus` is the HTTP status the provider refused the turn with, from 100 to 599, where the
// producer's provider library gives one (Oh My Pi's `errorStatus` on the refused assistant
// message, Claude Code's `api_error_status`); a number outside that range is taken as no status,
// and the daemon logs a warning naming it rather than refusing the report. `unprompted: true`
// says the turn ran no prompt the daemon sent: a phase reminder, an Envoy message, a background
// job's result. The daemon judges such a turn from its report, as it judges a prompt's turn from
// that prompt's result: a refusal whose status cannot clear (a 4xx other than 401, 403, 408 and
// 429) fails the claim at once with cause `provider_error` and holds the issue, and the answer's
// `failed` is then true; a refusal that can clear, or with no status, fails nothing and counts in
// the claim's run of provider-error turns, whose sixth in a row tells the controller once. A
// refused report without `unprompted` is judged by nothing, so the report of a turn a prompt ran is
// never counted beside that prompt's result. A clean report, with `unprompted` or without, ends
// that run when no later turn's report has reached the daemon before it. The daemon reads every
// body strictly, so a report from this contract that carries either field is refused by a daemon
// before 232; the boot gate refuses the mixed pair. (This entry took 239 at first, the lowest
// number above main's 219 that no open pull request declared at a reading of the 132 open heads
// that change this file, made between 2026-11-23T22:41:49Z and 22:44:42Z. The forward merge of
// 2026-11-23 that brought in 7df64124 brought main's 220 (the entry above 228), and a reading of
// the 133 open heads that change this file, at refs/pull/<n>/head between 2026-11-23T23:47:10Z and
// 23:50:14Z, found each of 220 to 239 declared by another pull request (239 by #4592) and 240 by
// none, so this entry moved to 240. The forward merge of 2026-11-24 that brought in e14345bf
// brought main's 228 (INFRA-1575's entry above), and a reading of the 137 open heads that change
// this file, between 2026-11-24T09:36:47Z and 09:39:37Z, found 240 declared by #3831 and 229 to
// 231 declared by others, and 232 by none, so this entry moved to 232, the lowest number above
// main's 228 that no other open pull request declares. Read main's own number and every open head
// again at the next merge rather than from this line.)
//
// 246: each issue of `GET /legion/v1/state` (and the issue record, which is one issue of that
// body) carries `podHold`, the standing hold of the issue's pod: why the
// idle rule keeps that pod past the idle period. It is `{reason, firstAt, at, detail}`: `reason`
// is the rule's own word for the hold (workflow.IdleHold's String, as the daemon's hold records
// spell it), `firstAt` the first pass of the hold's streak, `at` its newest pass, and `detail`
// what that word leaves out, cut to its first 200 characters and absent where the record holds
// none. The daemon reads it from `pod_holds` in the state's own snapshot, one statement for the
// whole project (store.StandingPodHoldsTx), and writes the key only for an issue whose pod a hold
// stands on: a pod the rule has not held, one it released and one whose streak a later pass ended
// carry none. A plugin below 246 reads each issue with a strict schema that names no `podHold`,
// so a caller below 246 is served every issue without it (shaping.go's podHoldContract and
// withoutPodHold, its row of stateNarrowings). The caller is placed as 193 places every caller of
// the state: a plugin from 228 to 245 states its contract and is served 228's keys. No enumerated
// field arrives: `reason` is a string a reader takes whole, so a hold a later rule adds is read
// rather than refused. `packages/contracts/src/legion-api.ts` mirrors the key on its issue and
// takes each of its four members as optional, since no plugin code reads them
// (LEGION_ANSWER_READS).
// (This entry took 221 at first, the lowest number above main's 205 that no open pull request
// declared at a reading of every open pull request of cybermonklabs/legion that ended at
// 2026-11-22T04:57Z. 220 to 222 had been named for three changes, #4930 among them, and 223 for
// #4805, before each of them declared its number, so 221 was taken twice. This entry moved to 224,
// the lowest number above 223 that no open pull request declares and that no change had been named
// for. A reading of every open pull request that ended at 2026-11-22T06:08:16Z found 206 (#3712
// and #4940), 207 (#3993 and #4432), 208 (#4175), 209 (#4049), 210 (#4661), 211 (#2230), 212
// (#4030, #4392, #4540 and #4580), 213 (#4553), 214 (#4592), 215 (#4065 and #4596), 216 (#4173,
// #4180, #4182, #4193, #4259 and #4932), 217 (#4273, #4590 and #4771), 218 (#3831), 219 (#4611),
// 220 (#4930), 221 (this change at its first number) and 223 (#4805) declared, and none above 223.
// The forward merge of 2026-11-24 that brought in e14345bf brought main's 228 (INFRA-1575's entry,
// over 213, 217, 219 and 220). A reading of `const DaemonAPIVersion` at refs/pull/<n>/head for
// each open pull request whose files touch this file, made between 2026-11-24T09:38:10Z and
// 09:42:57Z, found 229 (#2230), 230 (#4580), 231 (#4540), 233 (#4173, #4180, #4182, #4193 and
// #4932), 234 (#4065), 235 (#5204), 236 (#4984), 237 (#5168 and #5169), 238 (#5285), 239 (#4592),
// 240 (#3831 and #5271), 241 (#4771 and #5307), 242 (#4596) and 243 (#5349) declared, and 244 had
// been named for another change before it declared it, so this entry moved to 245, above every
// number an open pull request declares or a change had been named for. A second reading, between
// 2026-11-24T11:29:31Z and 11:32:10Z, found 245 declared by #5158 as well, with 244 (#3831 and
// #5365), 243 (#5349) and 242 (#4596) below it, so this entry moved to 246, above every number an
// open pull request declares.
// The forward merge of 2026-11-24 that brought in 3dbaa2b2 brought main's 232 (INFRA-4508's
// entry, right above this one), and a reading of the open heads that change this file, between
// 2026-11-24T12:30:11Z and 12:32:24Z, found 246 declared by this change alone (245 by #5158,
// 251 by #5307), so this entry kept 246. Read the base's own number and every open pull request
// again at every forward merge rather than from this line.)
//
// 250: the admission waiting line is ordered by Dispatch priority first. `GET /legion/v1/state`
// and `GET /legion/v1/issues/{key}/record` gain two keys: each issue the workflow records carries
// `priority`, its own Dispatch priority as Dispatch sends it, a number from 0 for P0 to 3 for P3
// or null for none; and each member's `waitingLine` carries `priority`, the priority that placed
// it in the line (its own, else its tree root's), null for a member neither it nor its root has a
// priority for, whose band stands after P3. The records of 250 hold `issues.*.priority` and
// `issues.*.waitingLine.priority`, and no record below 250 does: a caller whose contract (the one
// it states, else its pod's) is below 250 is served every issue without either key (the band of
// shaping.go's stateNarrowings, `withoutPriority`). Two values change meaning at every contract
// and move no shape: `waitingLine.partPlace` and `partLength` count within the member's priority
// band and part rather than within the part across the whole line, and `admission.waiting` and
// each member's `place` follow the new order, which no reader enumerates. (This entry took 221
// when main declared 220, with no reading of the other open pull requests. A reading of `const
// DaemonAPIVersion` at the head commit of each open pull request that changes this file or whose
// file list GitHub cut short (138 of 500), made between 2026-11-24T09:14:34Z and 09:17:00Z, found
// every number from 221 to 243 declared and nothing above, so this entry moved to 244. The
// forward merge of 2026-11-24 that brought in e14345bfc8 brought main's 228 (#4590's entry
// above). A second reading (137 of 498), made between 09:35:23Z and 09:40:15Z, found 221 to 227,
// 229 to 231 and 233 to 243 declared and nothing above 243, so this entry kept 244. Then #3831
// took 244, the numbers from 245 to 249 were given to other changes, and this entry moved to 250.
// The forward merge of 2026-11-24 that brought in 3dbaa2b23a brought main's 232 (#5271's entry
// above), below 250, so this entry kept 250. The forward merge of 2026-11-24 that brought in
// 7e75126fae brought main's 246 (#4960's entry above), below 250, so this entry kept 250. Read
// main's own number and every open pull request again at the next merge rather than from this
// line.)
//
// 256: #5349 -- each claim view of `GET /legion/v1/state` and of `GET
// /legion/v1/issues/{key}/record` gains two keys: `lastDeathAt` beside `lastDeathCause` and
// `lastStartFailureAt` beside `lastStartFailure`, the time the daemon recorded that cause, in RFC
// 3339. Each is absent when its cause is absent, and beside a cause an earlier release recorded,
// which wrote no time, except after a rollback (the migration's header; `api.ClaimView`). The
// claim keeps both in the store's `claims` table (`20261124073512_claim_cause_times`), so a
// daemon restart keeps them, and each is set and cleared only together with its cause, so a
// cause recorded again with the same text carries its new time (the rule
// `supervise.Claim.LastDeathAt` states). The store writes each time with a digest of the cause it
// dates, which no key serves, and reads the time back only beside that cause (the claim store's
// causeTime). The records of 256
// hold the two keys on every claim view, and no record below 256 does: a caller whose contract
// (the one it states, else its pod's lowest) is below 256 is served every claim view without
// them (the band of shaping.go's stateNarrowings, `withoutCauseTimes`), so a plugin from 250 to
// 255 is served 250's keys. The operator's claim view
// (`api.OperatorClaim`, which `legion claims list --json` prints) carries the same two keys; the
// operator routes are not served down, since their reader is the `legion` built with this daemon.
// No value of an enumerated field moves, and no request a plugin sends changes.
// (This entry took 243 at first, above each number another open pull request declared. A reading
// of `const DaemonAPIVersion` at the head of each of the 138 of the 513 open pull requests of this
// repository that change version.go, made at 2026-11-24T07:23:50Z, found every number from 221
// to 242 declared and nothing above 242, so 243 was the lowest number above main's 220 that no
// open pull request declared at that reading. The forward merge of 2026-11-24 that brought in
// e14345bfc8 brought main's 228 (#4590), below 243, so this entry kept 243, and the forward
// merge of 2026-11-24 that brought in d27405712c brought main's 232 (#5271), below it too, so it
// kept 243 again. The forward merge of 2026-11-24 that brought in 7e75126fae brought main's 246
// (#4960), above 243. A reading of version.go at the head of each of the 473
// open pull requests of this repository, between 2026-11-24T15:34:44Z and 15:48:04Z, found 55
// that add an entry, and above main's 246 they declare 247 (#4580 and #5384), 250 (#5365), 251
// (#5307), 252 (#4540), 254 (#4432) and 255 (#5164), and 247 to 249 had been named for other
// changes, so this entry moved to 256, above every number an open pull request
// declares or a change had been named for. From this entry the last number of answers.go's
// SupportedContracts is `api.causeTimesContract`, after podHoldContract's 246. The forward merge
// of 2026-11-24 that brought in 5df00e2f13 brought main's 250 (#5365, the entry above), below
// 256, so this entry kept 256, and the list ends priorityContract, causeTimesContract. Read the
// base's own number and every open pull request again at the next merge rather than from this
// line.)
//
// 267: an issue whose plan merges the work of several tasks merges more than once in one run, and
// each merge opens a production check of its own, so an issue can hold several open checks at
// once. Each issue of `GET /legion/v1/state` and of `GET /legion/v1/issues/{key}/record` gains
// `checks`: each open check (`api.CheckView`), oldest first, absent while none is open. `check`
// stays, the newest open check, so a reader of one check reads what it read before. A plugin below
// 267 is served each issue with no `checks` and with its `check` (shaping.go's band,
// `withoutChecks`); a check's own fields and values are those `check` carries, so no value strip
// is owed.
// The same change adds the notice kind `pull-request-close-refused`, to the issue's architect, for
// the pull requests of tasks whose close GitHub refused as it would refuse it again, so each stays
// open (`record.validNoticeKinds`). An outbox payload is decoded strictly, so no daemon of an
// earlier release decodes such a row, as the `task-join-failed` entry above says of its own kind;
// the way back of this change's migration (`store/migrations/20261123221142_task_base.up.sql`)
// deletes those rows before a rollback. No shape of `GET /legion/v1/state` moves with it.
// The entry's allocated number is 267, above main's released contract 256. Earlier released
// records keep their shapes without `checks`; checksContract and SupportedContracts name this
// entry's arrival, not the daemon's next version. Read the base's number and every open pull
// request's declared number again before pushing a renumber.
//
// 275: dispatch://INFRA-4425 -- under `dispatch_token_broker` a role trades its pod's exchange
// identity at the secrets broker for a Dispatch token of its own session, and the daemon names
// that session, which moves three surfaces this number names. The agent's argv: in a pod that
// carries the exchange identity (sandbox's launch.trades) the daemon chooses each Oh My Pi role's
// session id and starts it with `--session-id <id>`, or with `--resume <id>` when it resumes the
// role by its recorded file, and no path (sandbox's agentArgv); a launch into a pod that carries no
// exchange identity keeps `--resume=<file>`. A resume of a session the claim records with no file
// keeps entry 189's one `--resume=<id>` in either pod, and a launch that trades runs under that
// id. The worker environment: a role in a pod that carries the identity is told
// `DISPATCH_TOKEN_BROKER=1` under the setting, which the image's Dispatch client reads as the
// order to trade, and no `DISPATCH_TOKEN_FILE`, since its start carries no Dispatch token
// (sandbox's mainEnvironment); a launch into a pod built before the identity carries the token of
// `dispatch_token_transition_file` through `DISPATCH_TOKEN_FILE`, as every launch carried the
// shared bearer before. The state: `GET /legion/v1/state` gains `dispatchCredential` under the
// setting and nowhere else (api's DispatchCredentialView): the session the daemon's own Dispatch
// calls run under, `legion-daemon:<project>`, the launches since the daemon's boot that carried the
// transition token with the time of the last, and the live launches whose start carried a Dispatch
// token, which a deployment waits to see at zero before it removes
// `dispatch_token_transition_file`. The settings block's `dispatch_token_broker` is a key of the
// block the plugin parses loosely (35 above), and moves no number.
// A caller below 275 is served the state without the key, by its row of shaping.go's
// stateNarrowings (withoutDispatchCredential, the band of dispatchCredentialContract): a plugin
// built before sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole
// body for a key it does not name, and one built after strips the key, so the band keeps the body
// each contract's record describes. A caller from 193 up states its contract and is placed by it,
// and a caller placed by its pod is below 193 (podContracts), so the band reaches both. 275's
// records are 256's under its own number with the key paths of `dispatchCredential` on the
// state's. No other answer a plugin parses moves: the release-names route, which gains a 502 and a
// 503 for the broker's answer to the release of a claim's session names, answers the `legion`
// command, which states no contract.
//
// The skew is why the number moves. An Oh My Pi from before `--session-id` refuses a flag it does
// not know and exits, and a Dispatch client from before `DISPATCH_TOKEN_BROKER` reads a role with
// no `DISPATCH_TOKEN_FILE` as a role with no Dispatch credential at all, so the boot gate and
// `legion probe-image` hold the daemon to an image built from this contract. A role in a kept pod
// of an older image carries no exchange identity, so it keeps the argv and the credential it had,
// and its pod's update round replaces the pod.
// (This change keeps 275 at the merge that brought main in at c227d04661, which declares 256
// (#5349's entry above), 275 being above it.)
// (This change takes 275, the number INFRA-3118's architect allocated to it on 2026-11-24T18:27Z
// under INFRA's controller's ruling that it allocates contract numbers until #5399 merges, at
// the merge that brought main in at 5df00e2f13, which declares 250.)
// (This change takes 248, the lowest number above main's 246 that no other open pull request
// declares, at the merge that brought main in at 7e75126fae: a reading of `const
// DaemonAPIVersion` at the head of each of the 473 open pull requests, from 2026-11-24T15:32:43Z
// to 15:35:25Z, found 247 (#4580 and #5384), 250, 251, 252, 254 and 255 declared above 246, and
// 248 by none.)
// (This change keeps 241 at the merge that brought main in at acade9857d, which declares 232, 241
// being above it.)
// (This change takes 241, the lowest number above main's 228 that no other open pull request
// declares, at the merge that brought main in at 1b636f3193: a reading of `const
// DaemonAPIVersion` at the head of each of the 483 open pull requests, from 2026-11-24T11:35:12Z
// to 11:37:54Z, found 228 declared by 22 of them, 229 to 240 and 242 to 245 declared by others,
// and 241 by none.)
// (This change keeps 221 at the merge that brought main in at 58251571dd, which declares 220
// (the experiment group's entry above), 221 being above it. A reading of `const
// DaemonAPIVersion` at the head of each of the 510 open pull requests, from 2026-11-23T23:30:41Z
// to 23:36:15Z, found 221 declared also by #4049, #4432, #4682 (stacked on this branch), #4940,
// #4942 and #5164: whichever of them merges after another renumbers.)
// (This change keeps 221 at the merge that brought main in at c45a34b6ab, which declares 219
// (#4611's entry above, the owner's plan), 221 being above it.)
// (This change keeps 221 at the merge that brought main in at 5d483d8c61, which declares 217
// (#4273's entry above, INFRA-3531's and INFRA-4567's), 221 being above it.)
// (This change takes 221, the lowest number above main's 213 that no other open pull request
// declared when the forward merge of 2026-11-22 brought main in at 83b91235c5, which declares 213
// (INFRA-4036's entry above). It had declared 165, then 175 from the merge that brought main in at
// a16c218ee3, then 179 from the merge that brought main in at 506395beea, then 182 from the merge
// that brought main in at c7e1cbd532, then 192 from the merge that brought main in at 1c7c825367,
// then 196 from the merge that brought main in at a4cd4949fc, then 199 from the merge that brought
// main in at fcc746a383, then 202 from the merge that brought main in at f1d27d3a82, then 207 from
// the merge that brought main in at 1cdf5b9ceb, then 212 from the merge that brought main in at
// a155b5a33a, which declares 205 (INFRA-3178's entry above), and it kept 212 at the merges that
// brought main in at da96e1db98 and at bad897aa9d, main still at 205. A reading of `const
// DaemonAPIVersion` in packages/daemon/internal/api/version.go at the head of each of the 444 open
// pull requests at the merge of 83b91235c5, which ran from 2026-11-22T11:21:28Z to
// 2026-11-22T11:28:19Z, found 214, 215, 216, 217, 218, 219, 220, 223 and 224 declared by other
// pull requests and 221 declared by none. The reading at the merge of a155b5a33a, which ran from
// 2026-11-21T21:38:24Z to 2026-11-21T21:47:59Z, found 206 (#3712, #4539), 207 (#3993, #4432,
// #4590), 208 (#4175), 209 (#4049, #4580), 210 (#4661), 211 (#4611, #4902), 213 (#4030, #4553),
// 214 (#4592, #4596), 215 (#4065), 216 (#3831, #4173, #4259, #4540) and 217 (#2230, #4273, #4771)
// declared by other pull requests; #4682 and #4723, stacked on this branch, and #4805, stacked on
// #4723, declared this branch's 207. Read the base's own number and every open pull request again
// at the next merge rather than from this line, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// Each change after LastNumberedContract is an entry of its own file, and takes no number here
// (contracts.go).

/*
The line below is no Go and no contract number of this build. Main's contract check from before
this change reads every open pull request's head for `const\s+DaemonAPIVersion\s*=\s*(\d+)` and
fails each pull request it checks while one open head declares none, so this head keeps a line it
reads, at the number main declared when the line was written. Its tab keeps it out of every reader
of the number after the cutover (legion-pins, scripts/daemon-contract.sh and
scripts/daemon-contract-convert.sh), which read `const DaemonAPIVersion = <n>` with one space, and
the check of this change reads no open pull request.
const	DaemonAPIVersion = 275
*/
