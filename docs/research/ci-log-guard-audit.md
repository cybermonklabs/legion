# The steps the log-guard audit found printing a server's text on main

The audit behind cybermonklabs/legion#4991 (fix(ci): keep a server's text in the job log from becoming a workflow
command) listed 130 steps of main whose network client copies a server's reply into the job log. This table says
how the pull request routes each one. Its file and line are main's at the audit, so a line may have moved since;
`.github/scripts/check-log-guard.sh` is what holds the steps now, and `.github/log-guard-allow.tsv` says why each
unguarded command needs no guard.

80 routed, 3 routed in part, 47 left to #2918.
| Step | Audit's file:line on main | Client | How it is routed |
| --- | --- | --- | --- |
| Decide whether to publish | `workflows/browser-image.yaml:198` | crane | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Tag and record the image this run leaves | `workflows/browser-image.yaml:344` | crane | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Build what typecheck builds of the daemon module | `workflows/ci-caches.yaml:160` | go | `ci_log_guard` |
| Build what daemon-go builds of the daemon module | `workflows/ci-caches.yaml:175` | go | `ci_log_guard` |
| Write every Go release archive's third-party notices and cross-build the commands | `workflows/ci-caches.yaml:229` | go | in part: the cross-builds inside `go-release-notices.sh` (`ci_log_guard go`); `go-third-party-notices.sh`'s `"$go_cmd"` lines, which #2918 changes, stay **exposed** |
| Build the listener and natsd of the envoy module | `workflows/ci-caches.yaml:283` | go | `ci_log_guard` |
| Build the test binary of every package of the envoy module | `workflows/ci-caches.yaml:310` | go | `ci_log_guard` |
| Build what envoy-infra-go builds | `workflows/ci-caches.yaml:441` | go | inside `race-unit.sh` (`ci_log_guard_stderr go list`, `ci_log_guard go test`) |
| Delete this ref's go-envoy-infra entries the new one supersedes | `workflows/ci-caches.yaml:495` | gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Compare the Go airlock with the model over the seed corpus | `workflows/ci-job-airlock-lean.yaml:80` | go | `ci_log_guard` |
| Fuzz the Go airlock against the model | `workflows/ci-job-airlock-lean.yaml:83` | go | `ci_log_guard` |
| Two-process tests of the secrets broker | `workflows/ci-job-broker-multiprocess.yaml:58` | go | `ci_log_guard` |
| Copy every image the package does not hold | `workflows/ci-job-ci-images.yaml:42` | crane | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Check the package holds every row of .github/ci-images.yaml | `workflows/ci-job-ci-images.yaml:45` | Python urllib | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Test the Go daemon | `workflows/ci-job-daemon-go.yaml:196` | go | pipeline in one block: `{ …; } 2>&1 \| ci_log_guard cat` |
| Test the concurrent packages under the race detector | `workflows/ci-job-daemon-go.yaml:241` | go | `ci_log_guard` (now in `workflows/ci-job-daemon-go.yaml`, job `daemon-go-race`) |
| Test what the race build cannot run | `workflows/ci-job-daemon-go.yaml:251` | go | `ci_log_guard` |
| Stage 1 live proof | `workflows/ci-job-daemon-go.yaml:260` | docker,go | inside `stage1-skeleton.sh` (`ci_log_guard go build`, `ci_log_guard_stderr docker run`) |
| Size the service for this lane's Dispatch processes | `workflows/ci-job-dispatch-memory.yaml:58` | go | inside `pg-connection-ceiling.sh` (`ci_log_guard_stderr go run`) |
| Memory tests of the Dispatch server | `workflows/ci-job-dispatch-memory.yaml:60` | go | `ci_log_guard` |
| Size the service for this lane's Dispatch processes | `workflows/ci-job-dispatch-multiprocess.yaml:58` | go | inside `pg-connection-ceiling.sh` (`ci_log_guard_stderr go run`) |
| Two-process tests of the Dispatch server | `workflows/ci-job-dispatch-multiprocess.yaml:60` | go | `ci_log_guard` |
| Resolve the previous build's commit | `workflows/ci-job-dispatch-two-builds.yaml:119` | bun,go | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) (its script lines packages/envoy/scripts/build-dispatch-build.sh:141) |
| Build the previous build | `workflows/ci-job-dispatch-two-builds.yaml:135` | bun,go | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Resolve the first-rollout build's commit | `workflows/ci-job-dispatch-two-builds.yaml:147` | bun,go | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Build the first-rollout build | `workflows/ci-job-dispatch-two-builds.yaml:161` | bun,go | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Size the service for this lane's Dispatch processes | `workflows/ci-job-dispatch-two-builds.yaml:165` | go | inside `pg-connection-ceiling.sh` (`ci_log_guard_stderr go run`) |
| Two-build tests of the Dispatch server | `workflows/ci-job-dispatch-two-builds.yaml:172` | go | `ci_log_guard` |
| Build the Dispatch server | `workflows/ci-job-dispatch-two-builds.yaml:192` | go | `ci_log_guard` |
| Read the connections the two-build stack's Postgres must allow | `workflows/ci-job-dispatch-two-builds.yaml:213` | go | `ci_log_guard_stderr` (stdout captured) |
| Build this tree as the previous build | `workflows/ci-job-dispatch-two-builds.yaml:344` | bun,go | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Size the service for this lane's Dispatch processes | `workflows/ci-job-dispatch-two-builds.yaml:348` | go | inside `pg-connection-ceiling.sh` (`ci_log_guard_stderr go run`) |
| Two-build tests of the Dispatch server against its own build | `workflows/ci-job-dispatch-two-builds.yaml:351` | go | `ci_log_guard` |
| Local end-to-end acceptance | `workflows/ci-job-envoy-e2e-local.yaml:63` | curl,go | inside `e2e-local.sh` (`ci_log_guard go build`); its curl talks only to the listener it starts on 127.0.0.1 |
| Test the architecture-note and spec-file rules and their commands | `workflows/ci-job-envoy-go-checks.yaml:138` | go | `ci_log_guard` (now in `workflows/ci-job-envoy-go-build.yaml`, job `envoy-go-build`) |
| Test the listener's wire package, its clients and wirecheck | `workflows/ci-job-envoy-go-checks.yaml:146` | go | `ci_log_guard` (now in `workflows/ci-job-envoy-go-build.yaml`, job `envoy-go-build`) |
| Check the OpenAPI document is generated from the route table | `workflows/ci-job-envoy-go-checks.yaml:152` | go | `ci_log_guard` (now in `workflows/ci-job-envoy-go-build.yaml`, job `envoy-go-build`) |
| Check that no CI file of this repository compares the spec files | `workflows/ci-job-envoy-go-checks.yaml:161` | go | `ci_log_guard` (now in `workflows/ci-job-envoy-go-build.yaml`, job `envoy-go-build`) |
| Check the web app's display-tree fixture is up to date | `workflows/ci-job-envoy-go-checks.yaml:174` | go | `ci_log_guard` (now in `workflows/ci-job-envoy-go-build.yaml`, job `envoy-go-build`) |
| Build envoy services | `workflows/ci-job-envoy-go-checks.yaml:238` | go | `ci_log_guard` (now in `workflows/ci-job-envoy-go-build.yaml`, job `envoy-go-build`) |
| Check that no migration a database may have run is edited without its replaces line | `workflows/ci-job-envoy-go-checks.yaml:264` | git,go | `ci_log_guard`; inside `edited-migrations-base.sh` (`ci_log_guard git fetch`) (now in `workflows/ci-job-envoy-go-build.yaml`, job `envoy-go-build`) |
| Vet envoy for darwin, its tagged tests included | `workflows/ci-job-envoy-go-checks.yaml:279` | go | `ci_log_guard` (now in `workflows/ci-job-envoy-go-build.yaml`, job `envoy-go-build`) |
| Pack envoy binaries | `workflows/ci-job-envoy-go-checks.yaml:288` | go | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Search every order of the mention sender's claims | `workflows/ci-job-envoy-go-claim-model.yaml:86` | go | `ci_log_guard` |
| Size the service for this lane's Dispatch processes | `workflows/ci-job-envoy-go-docs-finalizers.yaml:89` | go | inside `pg-connection-ceiling.sh` (`ci_log_guard_stderr go run`) |
| Test the document service under the cleanup detector | `workflows/ci-job-envoy-go-docs-finalizers.yaml:96` | go | `ci_log_guard` |
| Size the service for this lane's Dispatch processes | `workflows/ci-job-envoy-go-race-api.yaml:67` | go | inside `pg-connection-ceiling.sh` (`ci_log_guard_stderr go run`) |
| Race-test the Dispatch API | `workflows/ci-job-envoy-go-race-api.yaml:76` | go | inside `database-skips.sh` (go's captured stderr printed through `ci_log_guard cat`) |
| Size the service for this lane's Dispatch processes | `workflows/ci-job-envoy-go-race-docs.yaml:76` | go | inside `pg-connection-ceiling.sh` (`ci_log_guard_stderr go run`) |
| Race-test the document service | `workflows/ci-job-envoy-go-race-docs.yaml:90` | go | `ci_log_guard` |
| Size the service for this lane's Dispatch processes | `workflows/ci-job-envoy-go-race.yaml:67` | go | inside `pg-connection-ceiling.sh` (`ci_log_guard_stderr go run`) |
| Race-test delivery, the store, the bus, the listener, the registry store, the session cache, kvwatch, the NATS credentials, the NATS auth service and the sign-in code flow | `workflows/ci-job-envoy-go-race.yaml:69` | go | `ci_log_guard` |
| Size the service for this lane's Dispatch processes | `workflows/ci-job-envoy-go.yaml:93` | go | inside `pg-connection-ceiling.sh` (`ci_log_guard_stderr go run`) |
| Test what the race build cannot run at all or at full size | `workflows/ci-job-envoy-go.yaml:140` | go | `ci_log_guard` |
| Unit test envoy Go package | `workflows/ci-job-envoy-go.yaml:174` | go | inside `database-skips.sh` (go's captured stderr printed through `ci_log_guard cat`) |
| Count the store-form rows on each backend | `workflows/ci-job-envoy-go.yaml:189` | go | inside `backend-count.sh` (go's captured stderr printed through `ci_log_guard cat`) |
| Count the database broker suites' skips | `workflows/ci-job-envoy-go.yaml:221` | go | inside `database-skips.sh` (go's captured stderr printed through `ci_log_guard cat`) |
| Vet agentloop's live proof (no model call) | `workflows/ci-job-envoy-go.yaml:226` | go | `ci_log_guard` |
| Vet agent-secrets' moto run (no moto) | `workflows/ci-job-envoy-go.yaml:231` | go | `ci_log_guard` |
| Test the container-backed NATS matrix (natslive) | `workflows/ci-job-envoy-go.yaml:262` | go | `ci_log_guard` |
| Integration test envoy (a real NATS server, in the test process) | `workflows/ci-job-envoy-go.yaml:276` | go | `ci_log_guard` |
| Vet envoy-infra, its live-test tags included | `workflows/ci-job-envoy-infra-go.yaml:86` | go | `ci_log_guard` (now in `workflows/ci-job-envoy-infra-go.yaml`, job `envoy-infra-build`) |
| Vet envoy-infra for darwin, its live-test tags included | `workflows/ci-job-envoy-infra-go.yaml:95` | go | `ci_log_guard` (now in `workflows/ci-job-envoy-infra-go.yaml`, job `envoy-infra-build`) |
| Unit test envoy-infra | `workflows/ci-job-envoy-infra-go.yaml:116` | go | inside `race-unit.sh` (`ci_log_guard_stderr go list`, `ci_log_guard go test`) (now in `workflows/ci-job-envoy-infra-go.yaml`, job `envoy-infra-build`) |
| Live tests of envoy-infra against nats-server (natslive) | `workflows/ci-job-envoy-infra-go.yaml:119` | go | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Size the service for this lane's Dispatch processes | `workflows/ci-job-envoy-session-stall.yaml:72` | go | inside `pg-connection-ceiling.sh` (`ci_log_guard_stderr go run`) |
| Container smoke test (testcontainers) | `workflows/ci-job-envoy-smoke.yaml:133` | go | `ci_log_guard` |
| Live tests of envoy-infra's host:Listener | `workflows/ci-job-envoy-smoke.yaml:168` | go | `ci_log_guard` |
| The envoy-infra plugin's schema carries the version and the download URL it was built with | `workflows/ci-job-envoy-smoke.yaml:186` | go | `ci_log_guard` |
| Check every Go release archive's third-party notices can be written and the cross-built commands compile | `workflows/ci-job-lint.yaml:236` | go | in part: the cross-builds inside `go-release-notices.sh` (`ci_log_guard go`); `go-third-party-notices.sh`'s `"$go_cmd"` lines, which #2918 changes, stay **exposed** |
| Server-change soak of the listener | `workflows/ci-job-listener-server-change.yaml:53` | go | `ci_log_guard` |
| Pack the plugin releases and run the prompt probes on the real Oh My Pi | `workflows/ci-job-plugin-pack.yaml:113` | go | `ci_log_guard` |
| Check the text packages/envoy pins | `workflows/ci-job-text-checks.yaml:90` | go | `ci_log_guard` |
| Check the text packages/envoy/infra pins | `workflows/ci-job-text-checks.yaml:93` | go | `ci_log_guard` |
| Check the text packages/daemon pins | `workflows/ci-job-text-checks.yaml:96` | go | `ci_log_guard` |
| Test the Dispatch API's source guards (no database) | `workflows/ci-job-text-checks.yaml:113` | go | `ci_log_guard` |
| Test the supervise table's two pure pins (no database) | `workflows/ci-job-text-checks.yaml:126` | go | `ci_log_guard` |
| Vet the Go daemon, its e2e-tagged live proofs included | `workflows/ci-job-typecheck.yaml:59` | go | `ci_log_guard` |
| Vet the Go daemon for darwin, its e2e-tagged live proofs included | `workflows/ci-job-typecheck.yaml:70` | go | `ci_log_guard` |
| Check the pull request's title, body, branch name, commits, comments and reviews | `workflows/ci.yaml:877` | gh,git | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| (unnamed) | `workflows/ci.yaml:900` | gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Judge every job of this run | `workflows/ci.yaml:986` | gh,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Set every other ci-ok on this head to this job's conclusion | `workflows/ci.yaml:998` | gh,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Cancel the removed group's ci.yaml run | `workflows/dequeued.yaml:69` | gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Build the Dispatch server | `workflows/dispatch-browser-nightly.yaml:145` | go | `ci_log_guard` |
| Build the binaries the browser suite runs | `workflows/dispatch-browser.yaml:56` | go | `ci_log_guard` |
| Read the connections each stack's Postgres must allow | `workflows/dispatch-browser.yaml:64` | go | `ci_log_guard_stderr` (stdout captured) |
| Build the Dispatch server | `workflows/dispatch-browser.yaml:311` | go | `ci_log_guard` |
| Read the connections the multiprocess stack's Postgres must allow | `workflows/dispatch-browser.yaml:323` | go | `ci_log_guard_stderr` (stdout captured) |
| Build the Dispatch server for the embedder lane | `workflows/dispatch-browser.yaml:388` | go | `ci_log_guard` |
| Read the connections the embedder stack's Postgres must allow | `workflows/dispatch-browser.yaml:401` | go | `ci_log_guard_stderr` (stdout captured) |
| Read the connections the visual stack's Postgres must allow | `workflows/dispatch-browser.yaml:469` | go | `ci_log_guard_stderr` (stdout captured) |
| Report a refused blob download as a refused read | `workflows/dispatch-browser.yaml:533` | gh,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Start ci.yaml when this push's head was never tested | `workflows/main-bypass-check.yaml:78` | gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Check the title, body, branch name, commits, comments and reviews | `workflows/pr-title.yaml:123` | gh,git | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Determine next version | `workflows/release-envoy-infra.yaml:104` | curl,go,npm | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Vet envoy-infra, its live-test tags included | `workflows/release-envoy-infra.yaml:167` | go | `ci_log_guard` |
| Unit test envoy-infra | `workflows/release-envoy-infra.yaml:170` | go | `ci_log_guard` |
| Build pulumi-resource-envoy-infra and envoy-infra | `workflows/release-envoy-infra.yaml:206` | go | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Tag release | `workflows/release-envoy-infra.yaml:337` | curl,git | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Create GitHub release | `workflows/release-envoy-infra.yaml:355` | curl,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Container smoke test (testcontainers) | `workflows/release-envoy-listener.yaml:306` | go | `ci_log_guard` |
| Build agent-secrets binaries | `workflows/release-envoy-listener.yaml:565` | go | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Determine latest | `workflows/release-envoy-listener.yaml:701` | docker,gh,git | inline `ci_log_guard_stderr` (a credentialed job runs no script of this repository) |
| Confirm the version | `workflows/release-envoy-listener.yaml:863` | curl,gh,git,npm | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Tag release | `workflows/release-envoy-listener.yaml:940` | curl,git | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Create or update GitHub release | `workflows/release-envoy-listener.yaml:963` | curl,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Write the changelog into the release body | `workflows/release-envoy-listener.yaml:992` | curl,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Build legion | `workflows/release.yaml:187` | go | in part: the build inside `release-build-legion.sh` (`ci_log_guard go`); `go-third-party-notices.sh`'s `"$go_cmd"` lines, which #2918 changes, stay **exposed** |
| pi-envoy: Publish to npm | `workflows/release.yaml:712` | npm | inline `ci_log_guard_stderr` (a credentialed job runs no script of this repository) |
| pi-envoy: Tag release | `workflows/release.yaml:726` | curl,git | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| pi-envoy: Push tag | `workflows/release.yaml:736` | curl,git | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| pi-envoy: Create GitHub release | `workflows/release.yaml:745` | curl,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| pi-envoy: Write the changelog into the release body | `workflows/release.yaml:767` | curl,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| pi-legion: Publish to npm | `workflows/release.yaml:799` | npm | inline `ci_log_guard_stderr` (a credentialed job runs no script of this repository) |
| pi-legion: Tag release | `workflows/release.yaml:813` | curl,git | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| pi-legion: Push tag | `workflows/release.yaml:823` | curl,git | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| pi-legion: Create GitHub release | `workflows/release.yaml:832` | curl,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| pi-legion: Write the changelog into the release body | `workflows/release.yaml:852` | curl,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| claude-envoy: Publish to npm | `workflows/release.yaml:885` | npm | inline `ci_log_guard_stderr` (a credentialed job runs no script of this repository) |
| claude-envoy: Tag release | `workflows/release.yaml:899` | curl,git | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| claude-envoy: Push tag | `workflows/release.yaml:909` | curl,git | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| claude-envoy: Create GitHub release | `workflows/release.yaml:918` | curl,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| claude-envoy: Write the changelog into the release body | `workflows/release.yaml:938` | curl,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| codex-envoy: Publish to npm | `workflows/release.yaml:971` | npm | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| legion: Tag release | `workflows/release.yaml:1092` | curl,git | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| legion: Create GitHub release | `workflows/release.yaml:1112` | curl,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| legion: Write the changelog into the release body | `workflows/release.yaml:1137` | curl,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Name the worker image on the legion release | `workflows/release.yaml:1238` | curl,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) |
| Post the failing ci-ok when this job's result decides a merge-group run | `actions/ci-ok-report/action.yml:34` | gh,gh | left to #2918, which changes this step; **exposed** on main until #2918 merges (the allow list says how #2918's head runs it) (its script lines .github/scripts/ci-ok-report.sh:191, .github/scripts/runner-shutdown.sh:153,169) |
