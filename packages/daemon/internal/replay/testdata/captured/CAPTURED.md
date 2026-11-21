# Captured samples for `legion replay`

Real daemon log lines, Dispatch events and one GitHub commits page, read once and neutralized for a public repository.
They live in `packages/daemon/internal/replay/testdata/captured/`, and every path below is relative to it: `log/<slug>.json` holds the daemon's lines, `events/<type-slug>.json` Dispatch's events and `github/commits.json` GitHub's page.
The daemon's lines sit under `log/` rather than `logs/` because the repository's `.gitignore` ignores every directory named `logs`.
`TestTheTraceReadsEveryCapturedShape` (`packages/daemon/internal/replay/trace_test.go`) reads every file here with the trace's readers (`ParseLogLine`, `ParseDispatchEvent` and `ParseMerges` in `trace.go`) and holds each to the fields read by hand from the file, so a file added here needs its row in that test.
Every issue key is `ACME-<n>` under one map across all files; the map is kept outside the repository.

Placeholders in the commands below:

- `<daemon filter>`: the Cloud Logging filter that selects the daemon's own container (resource type, namespace, container and pod app label of the deployment).
- `<project>`: the cloud project that holds the daemon's logs.
- `<daemon pod>`: the pod name of one daemon process.
- `<KEY>`: the source issue key; the file names the neutral key it became.
- `$DISPATCH_URL`, and the Dispatch token sent on stdin as the `Authorization: Bearer` header (`-H @-`).

Shape: each log file is a one-entry array of `{timestamp, jsonPayload}`, each event file a one-event array as the events route returns it, `github/commits.json` the page as GitHub returns it. Neutralizing changes values only; every key, type and array length is the captured one.

## Log lines

Command, one read at a time, 3 s apart:

```sh
gcloud logging read '<daemon filter> AND <clause>' --limit=1 --format=json --freshness=30d --project=<project> \
  | jq '[.[] | {timestamp, jsonPayload}]'
```

| File | `<clause>` | Entry time | Captured | Neutralizing changed |
|---|---|---|---|---|
| `log/phase-changed-gate-opened.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="gate_opened"` | 2026-11-21T00:19:37.742856009Z | 2026-11-21T05:06:28Z | issue key -> ACME-<n>: issue, tree |
| `log/phase-changed-planner-completed.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="planner_completed"` | 2026-11-21T03:45:12.790777954Z | 2026-11-21T05:06:34Z | issue key -> ACME-<n>: issue, tree |
| `log/phase-changed-implementation-ready.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="implementation_ready"` | 2026-11-21T05:06:10.042070913Z | 2026-11-21T05:06:41Z | issue key -> ACME-<n>: issue, tree |
| `log/phase-changed-tester-passed.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="tester_passed"` | 2026-11-21T05:01:51.066591297Z | 2026-11-21T05:06:48Z | issue key -> ACME-<n>: issue, tree |
| `log/phase-changed-tester-failed.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="tester_failed"` | 2026-11-21T04:52:53.433143999Z | 2026-11-21T05:06:56Z | issue key -> ACME-<n>: issue, tree |
| `log/phase-changed-review-approved.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="review_approved"` | 2026-11-21T04:55:47.676362087Z | 2026-11-21T05:07:03Z | issue key -> ACME-<n>: issue, tree |
| `log/phase-changed-review-rejected.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="review_rejected"` | 2026-11-21T05:06:12.912949789Z | 2026-11-21T05:07:10Z | issue key -> ACME-<n>: issue, tree |
| `log/phase-changed-retro-completed.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="retro_completed"` | 2026-11-21T04:43:39.028232818Z | 2026-11-21T05:07:17Z | issue key -> ACME-<n>: issue, tree |
| `log/phase-changed-pull-request-merged.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="pull_request_merged"` | 2026-11-21T03:16:06.296974173Z | 2026-11-21T05:07:24Z | issue key -> ACME-<n>: issue, tree |
| `log/phase-changed-sign-off.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="sign_off"` | 2026-11-21T04:33:31.534167356Z | 2026-11-21T05:07:33Z | issue key -> ACME-<n>: issue, tree |
| `log/phase-changed-backward.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="backward"` | 2026-11-21T05:01:32.767710711Z | 2026-11-21T05:07:41Z | issue key -> ACME-<n>: issue, tree |
| `log/phase-changed-conflict-window-one.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="conflict" AND timestamp>="2026-11-16T22:50:00Z" AND timestamp<"2026-11-20T20:54:00Z"` | 2026-11-20T20:47:28.110580884Z | 2026-11-21T05:07:47Z | issue key -> ACME-<n>: issue, tree |
| `log/phase-changed-ready-window-two.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="ready" AND timestamp>="2026-11-13T07:36:00Z" AND timestamp<"2026-11-16T22:50:00Z"` | 2026-11-16T22:31:54.321000497Z | 2026-11-21T05:07:53Z | issue key -> ACME-<n>: issue, tree |
| `log/phase-changed-checks-red-window-two.json` | `jsonPayload.msg="workflow: phase changed" AND jsonPayload.trigger="checks_red" AND timestamp>="2026-11-13T07:36:00Z" AND timestamp<"2026-11-16T22:50:00Z"` | 2026-11-16T22:23:47.448509334Z | 2026-11-21T05:08:00Z | issue key -> ACME-<n>: issue, tree |
| `log/admission-slot-taken.json` | `jsonPayload.msg="admission: slot taken"` | 2026-11-21T04:55:47.676470347Z | 2026-11-21T05:08:06Z | issue key -> ACME-<n>: issue |
| `log/admission-slot-released.json` | `jsonPayload.msg="admission: slot released"` | 2026-11-21T04:55:47.676437127Z | 2026-11-21T05:08:14Z | issue key -> ACME-<n>: issue |
| `log/admission-waits-on-blockers.json` | `jsonPayload.msg="admission waits for what this issue waits on"` | 2026-11-21T05:08:03.917843442Z | 2026-11-21T05:08:21Z | issue key -> ACME-<n>: blockers[], issue |
| `log/admission-slot-back-work-came-back.json` | `jsonPayload.msg="admission gives a slot back to an issue whose work came back"` | 2026-11-21T04:55:47.564982672Z | 2026-11-21T05:08:27Z | issue key -> ACME-<n>: issue |
| `log/admission-slot-back-wait-ended.json` | `jsonPayload.msg="admission gives a slot back to an issue whose wait ended"` | 2026-11-21T03:20:50.606630471Z | 2026-11-21T05:08:34Z | issue key -> ACME-<n>: issue |
| `log/admission-waits-for-children.json` | `jsonPayload.msg="admission: an issue waits for the Dispatch children under it"` | 2026-11-21T00:52:05.903914866Z | 2026-11-21T05:08:42Z | issue key -> ACME-<n>: issue, tree |
| `log/root-waits-on-person.json` | `jsonPayload.msg="workflow: root waits on a person and keeps its admission slot"` | 2026-11-21T02:22:11.479739820Z | 2026-11-21T05:08:49Z | issue key -> ACME-<n>: issue, tree |
| `log/design-gate-opened.json` | `jsonPayload.msg="workflow: design gate opened"` | 2026-11-21T03:40:14.324318680Z | 2026-11-21T05:08:58Z | id -> fresh uuid: artifact; issue key -> ACME-<n>: issue |
| `log/design-gate-closed.json` | `jsonPayload.msg="workflow: design gate closed"` | 2026-11-21T03:22:07.596379536Z | 2026-11-21T05:09:05Z | id -> fresh uuid: artifact; issue key -> ACME-<n>: issue |
| `log/held-phase-released.json` | `jsonPayload.msg="workflow: a held phase is released"` | 2026-11-20T00:10:02.000115273Z | 2026-11-21T05:09:12Z | issue key -> ACME-<n>: issue |
| `log/issue-left-workflow.json` | `jsonPayload.msg="workflow: issue left the workflow"` | 2026-11-21T04:17:17.416992528Z | 2026-11-21T05:09:19Z | issue key -> ACME-<n>: issue, tree |
| `log/tree-lingers.json` | `jsonPayload.msg="workflow: tree lingers"` | 2026-11-20T20:38:12.539322887Z | 2026-11-21T05:09:26Z | issue key -> ACME-<n>: tree |
| `log/linger-closed-tree.json` | `jsonPayload.msg="workflow: linger closed the tree"` | 2026-11-21T04:11:33.472823855Z | 2026-11-21T05:09:34Z | issue key -> ACME-<n>: tree |
| `log/supervise-turn-started.json` | `jsonPayload.msg="supervise: a turn started"` | 2026-11-21T05:09:23.249232332Z | 2026-11-21T05:09:42Z | claim token -> legion-acme-acme-<n>-<role>: claim |
| `log/supervise-turn-ended.json` | `jsonPayload.msg="supervise: a turn ended"` | 2026-11-21T05:09:40.804098485Z | 2026-11-21T05:09:49Z | claim token -> legion-acme-acme-<n>-<role>: claim |
| `log/supervise-launched.json` | `jsonPayload.msg="supervise: launched"` | 2026-11-21T05:08:53.047267599Z | 2026-11-21T05:09:56Z | claim token -> legion-acme-acme-<n>-<role>: claim; id -> fresh uuid: incarnation |
| `log/supervise-suspended.json` | `jsonPayload.msg="supervise: suspended"` | 2026-11-21T05:08:35.479508579Z | 2026-11-21T05:10:03Z | claim token -> legion-acme-acme-<n>-<role>: claim |
| `log/daemon-started.json` | `jsonPayload.msg="legion daemon started"` | 2026-11-21T02:01:27.895108090Z | 2026-11-21T05:10:31Z | project -> ACME: project |
| `log/merge-queue-left-nothing-read.json` | `jsonPayload.msg="workflow: a pull request left the merge queue where its issue does not merge at that head; nothing is read"` | 2026-11-21T04:45:24.375690722Z | 2026-11-21T05:10:38Z | issue key -> ACME-<n>: issue |
| `log/merge-queue-left-read-once.json` | `jsonPayload.msg="workflow: a pull request in merging left the merge queue; the daemon reads it from GitHub once"` | 2026-11-21T03:19:56.094538349Z | 2026-11-21T05:10:46Z | issue key -> ACME-<n>: issue |
| `log/design-gate-registered.json` | `jsonPayload.msg="workflow: design gate registered"` | 2026-11-21T07:59:33.665670117Z | 2026-11-21T08:06:11Z | issue key -> ACME-<n>: issue (the same key and fresh artifact id as `log/design-gate-closed.json`, whose issue and artifact it is); id -> fresh uuid: artifact |
| `log/supervise-claim-failed.json` | `jsonPayload.msg="supervise: claim failed"` | 2026-11-18T10:37:34.716843703Z | 2026-11-21T08:06:11Z | claim token -> legion-acme-acme-<n>-<role>: claim, and the same token inside why |
| `log/supervise-turn-ended-outcome.json` | `jsonPayload.msg="supervise: a turn ended" AND jsonPayload.sequence:*` | 2026-11-21T08:06:02.659823412Z | 2026-11-21T08:06:11Z | claim token -> legion-acme-acme-<n>-<role>: claim (the same issue key as `log/supervise-launched.json`, another role) |

Two extra files hold the first lines of the deployed release's last daemon process, which writes neither of the lines today's daemon starts a process with, the leak check line and `daemon: opening the store; …` (see "Kinds with no line in 30 days"):

```sh
gcloud logging read '<daemon filter> AND resource.labels.pod_name="<daemon pod>" AND timestamp>="2026-11-21T01:30:00Z" AND timestamp<="2026-11-21T01:55:00Z"' \
  --order=asc --limit=8 --format=json --freshness=30d --project=<project> | jq '[.[0] | {timestamp, jsonPayload}]'   # .[2] for the boot stage line
```

| File | msg | Entry time | Captured | Neutralizing changed |
|---|---|---|---|---|
| `log/daemon-first-line-stale-plugin.json` | `daemon: stale-plugin relaunches install` (entry 1) | 2026-11-21T01:54:55.884632109Z | 2026-11-21T05:12:03Z | nothing (no private value in the line) |
| `log/daemon-boot-stage-prompts.json` | `legion workflow boot stage`, stage `prompts` (entry 3) | 2026-11-21T01:54:55.884757308Z | 2026-11-21T05:12:03Z | nothing (no private value in the line) |

Kept as captured: the `reason` and `why` texts of the log lines. Each is the daemon's own literal or format string, not a person's text: `the operator retried this phase's worker` (workflow/engine.go:1873), `spec version %d is awaiting a person's review` (workflow/awaiting_reconcile.go:344), `the issue's pod has been idle for the release period` (daemon/idle.go:469), `the issue stands in %s, not merging` (workflow/pullrequest.go:661), and the error chain of `supervise: claim failed`, every link of which the daemon writes (`launch failures ran out: %v` supervise/machine.go:2864, `start through its authenticated launcher` runtime/sandbox/relaunch.go:446, `the shared public leak check could not be installed: ` leakguard/cause.go:15, `the served check could not be run: %w` leakguard/install.go:446) above the operating system's `waitid: no child processes`; only the claim token inside it changed. Also kept: commit SHAs of `cybermonklabs/legion`, artifact versions, seqs, slot counts and the cap, generations, the daemon's listen addresses (`[::]:<port>`), and plugin versions.

## Dispatch events

Command (page through with `after=<last seq>`), then the one event kept:

```sh
printf 'Authorization: Bearer %s\n' "$(cat <dispatch token file>)" \
  | curl -sS -H @- "$DISPATCH_URL/api/v1/issues/<KEY>/events?limit=200&after=0" | jq '[.[] | select(.seq==<seq>)]'
```

| File | type | `payload.changed` | Neutral key | seq | Captured | Neutralizing changed |
|---|---|---|---|---|---|---|
| `events/artifact-approved.json` | `artifact.approved` | — | ACME-25 | 58 | 2026-11-21T05:07:48Z | issue key -> ACME-<n>: issue_key; project -> ACME: project; email -> person@example.invalid: actor.id, payload.actor.id; id -> fresh uuid: payload.artifact_id, payload.ask_id; digest -> fresh sha256: payload.content_sha256 |
| `events/artifact-changes-requested.json` | `artifact.changes_requested` | — | ACME-25 | 41 | 2026-11-21T05:07:48Z | issue key -> ACME-<n>: issue_key; project -> ACME: project; email -> person@example.invalid: actor.id, payload.actor.id; id -> fresh uuid: payload.artifact_id, payload.ask_id; free text -> "Captured change request reason": payload.reason; digest -> fresh sha256: payload.content_sha256 |
| `events/ask-answered.json` | `ask.answered` | — | ACME-25 | 40 | 2026-11-21T05:07:48Z | issue key -> ACME-<n>: issue_key, payload.issue_key; project -> ACME: payload.approval_artifact.project, project; email -> person@example.invalid: actor.id, payload.answer.user, payload.thread_logins[]; free text -> "Captured change request reason": payload.answer.text; id -> fresh uuid: payload.approval.artifact_id, payload.approval.documents[].artifact_id, payload.author.id, payload.id; workspace path: key -> acme-<n>: payload.author.origin.cwd; machine -> legion-acme-daemon: payload.author.origin.machine; session title -> Legion <role> · ACME[-<n>]: payload.author.origin.session_title; free text -> "Captured approval question": payload.question |
| `events/ask-opened.json` | `ask.opened` | — | ACME-25 | 23 | 2026-11-21T05:07:48Z | issue key -> ACME-<n>: issue_key, payload.issue_key, payload.references_changed[].id; project -> ACME: project; id -> fresh uuid: actor.id, payload.author.id, payload.block_id, payload.id; machine -> legion-acme-daemon: actor.origin.machine, payload.author.origin.machine; workspace path: key -> acme-<n>: actor.origin.cwd, payload.author.origin.cwd; session title -> Legion <role> · ACME[-<n>]: actor.origin.session_title, payload.author.origin.session_title; free text -> "Captured option 1 description": payload.options[].description; free text -> "Captured option 1": payload.options[].label; free text -> "Captured option 2": payload.options[].label; free text -> "Captured question": payload.question |
| `events/handoff-recorded.json` | `handoff.recorded` | — | ACME-25 | 70 | 2026-11-21T05:07:48Z | issue key -> ACME-<n>: issue_key; project -> ACME: project; id -> fresh uuid: actor.id, payload.handoff_id; machine -> legion-acme-daemon: actor.origin.machine; workspace path: key -> acme-<n>: actor.origin.cwd; session title -> Legion <role> · ACME[-<n>]: actor.origin.session_title |
| `events/issue-closed.json` | `issue.closed` | `["status", "closed_at"]` | ACME-43 | 99 | 2026-11-21T05:07:48Z | issue key -> ACME-<n>: issue_key, payload.ancestors[], payload.key, payload.parent; project -> ACME: payload.project, project; daemon actor -> legion-daemon:ACME: actor.id; repository -> acme/widgets: payload.repo, payload.resolved_repo; free text -> "Captured workflow issue": payload.title; number follows the mapped key: payload.number; email -> person@example.invalid: payload.assignee; free text -> "Captured components reason": payload.components.reason; id -> fresh uuid: payload.created_by.id, payload.plan_artifact_id, payload.primary_artifact_id, payload.verification_artifact_id; machine -> legion-acme-daemon: payload.created_by.origin.machine; session title -> Legion <role> · ACME[-<n>]: payload.created_by.origin.session_title |
| `events/issue-created.json` | `issue.created` | `["title", "status", "rank", "priority", "assignee", "labels", "repo"]` | ACME-25 | 1 | 2026-11-21T05:07:48Z | issue key -> ACME-<n>: issue_key, payload.key, payload.references_changed[].id; project -> ACME: payload.project, project; id -> fresh uuid: actor.id, payload.created_by.id, payload.primary_artifact_id; machine -> legion-acme-daemon: actor.origin.machine, payload.created_by.origin.machine; session title -> Legion <role> · ACME[-<n>]: actor.origin.session_title, payload.created_by.origin.session_title; free text -> "Captured workflow issue": payload.title; number follows the mapped key: payload.number; email -> person@example.invalid: payload.assignee |
| `events/issue-updated-blocked-by.json` | `issue.updated` | `["components", "blocked_by"]` | ACME-51 | 3 | 2026-11-21T05:07:48Z | issue key -> ACME-<n>: issue_key, payload.ancestors[], payload.blocked_by[], payload.key, payload.parent, payload.references_changed[].id; project -> ACME: payload.project, project; id -> fresh uuid: actor.id, payload.created_by.id, payload.primary_artifact_id, payload.verification_artifact_id; machine -> legion-acme-daemon: actor.origin.machine, payload.created_by.origin.machine; workspace path: key -> acme-<n>: actor.origin.cwd; session title -> Legion <role> · ACME[-<n>]: actor.origin.session_title, payload.created_by.origin.session_title; repository -> acme/widgets: payload.repo, payload.resolved_repo; free text -> "Captured workflow issue": payload.title; number follows the mapped key: payload.number; priority id -> captured-priority: payload.advances[].id; email -> person@example.invalid: payload.assignee; free text -> "Captured components reason": payload.components.reason |
| `events/issue-updated-parent.json` | `issue.updated` | `["status", "parent", "external_links"]` | ACME-56 | 3 | 2026-11-21T05:07:48Z | issue key -> ACME-<n>: issue_key, payload.ancestors[], payload.key, payload.parent; project -> ACME: payload.project, project; id -> fresh uuid: actor.id, payload.claim.actor.id, payload.created_by.id, payload.primary_artifact_id; machine -> captured-machine: actor.origin.machine, payload.claim.actor.origin.machine, payload.created_by.origin.machine; session title -> Captured session: actor.origin.session_title, payload.claim.actor.origin.session_title, payload.created_by.origin.session_title; free text -> "Captured workflow issue": payload.title; number follows the mapped key: payload.number; email -> person@example.invalid: payload.assignee; pull request url: repository -> acme/widgets: payload.external_links[].url |
| `events/issue-updated-rank.json` | `issue.updated` | `["rank"]` | ACME-25 | 11 | 2026-11-21T05:07:48Z | issue key -> ACME-<n>: issue_key, payload.key; project -> ACME: payload.project, project; id -> fresh uuid: actor.id, payload.created_by.id, payload.primary_artifact_id; machine -> legion-acme-daemon: actor.origin.machine, payload.created_by.origin.machine; session title -> Legion <role> · ACME[-<n>]: actor.origin.session_title, payload.created_by.origin.session_title; free text -> "Captured workflow issue": payload.title; number follows the mapped key: payload.number; email -> person@example.invalid: payload.assignee |
| `events/issue-updated-status.json` | `issue.updated` | `["status"]` | ACME-25 | 2 | 2026-11-21T05:07:48Z | issue key -> ACME-<n>: issue_key, payload.key; project -> ACME: payload.project, project; daemon actor -> legion-daemon:ACME: actor.id; free text -> "Captured workflow issue": payload.title; number follows the mapped key: payload.number; email -> person@example.invalid: payload.assignee; id -> fresh uuid: payload.created_by.id, payload.primary_artifact_id; machine -> legion-acme-daemon: payload.created_by.origin.machine; session title -> Legion <role> · ACME[-<n>]: payload.created_by.origin.session_title |

Event-wide rules: the issue's own `project` and the daemon's actor `legion-daemon:<project>` take `ACME`; the daemon's machine name takes `legion-acme-daemon` and any other machine `captured-machine`; a Legion role's session title keeps its form `Legion <role> · ACME[-<n>]`, any other title becomes `Captured session`; every uuid (sessions, artifacts, asks, blocks, handoffs, incarnations) is a fresh uuid, the same fresh uuid wherever the captured one recurs; `payload.number` is the number of the mapped key. Kept: Dispatch's own option texts of an approval ask (`Approve`, `Request changes` and their descriptions), document names, ranks, event ids and seqs, labels, statuses and times.

## GitHub

```sh
gh api 'repos/cybermonklabs/legion/commits?sha=main&since=2026-11-20T00:00:00Z&until=2026-11-20T06:00:00Z&per_page=5'
```

`github/commits.json`, captured 2026-11-21T05:08:44Z, five commits. Neutralizing changed: each `commit.message` to `<type(scope)>: captured commit message (#<pr>)`, keeping the conventional prefix and the pull request number, and a `Co-authored-by` trailer to `person <person@example.invalid>` (the Legion implementer App's trailer is kept); a person's commit author name and email to `person`/`person@example.invalid`; the agent App named after a person to `person-agent[bot]` with `1+person-agent[bot]@users.noreply.github.com`; the GitHub user objects of those two (`login`, `id`, `node_id`, `avatar_url`, every user URL) to the neutral login with ids 1 and 2; the avatar of the Legion implementer App, which is its owner's user id, to user 2; `commit.verification.signature` to a neutral armour and `commit.verification.payload` to the neutral author and message. Kept: SHAs, tree and parent SHAs, dates, URLs of `cybermonklabs/legion`, `legion-implementer[bot]` and its id, and GitHub's own `web-flow`/`GitHub <noreply@github.com>` committer.

## Kinds with no line in 30 days

Read with `--freshness=30d` on 2026-11-21; each read returned an empty array, so no file holds them:

- `daemon: the deployment leak check is not configured (…)` / `daemon: the deployment leak check is configured: …` (clause `jsonPayload.msg:"daemon: the deployment leak check is"`).
- `daemon: opening the store; a connection error that the next attempt can clear is tried again`.

The deployed release predates both lines: its last process (boot at 2026-11-21T01:54:55Z, `legion daemon started` at 2026-11-21T02:01:27Z) begins with `daemon: stale-plugin relaunches install`, then `daemon: the image probe's attempt outlives slow_command_timeout_seconds`, then `legion workflow boot stage` with stage `prompts`, `agent-secrets: logged in …`, and stages `store`, `config`, `dispatch`. The first two of those lines that a trace can use as a process start are the two extra files above.

## Notes for the trace

- `log/phase-changed-ready-window-two.json` moves `merging` → `awaiting_merge`: window two's release had a phase `awaiting_merge`, which today's phase list does not hold.
- `log/phase-changed-checks-red-window-two.json` and `log/phase-changed-conflict-window-one.json` move to `implementing` (from `testing` and `merging`).
- `admission: slot taken` and `admission: slot released` carry `slots` and `cap` as numbers; `admission waits for what this issue waits on` carries `blockers` as an array of keys.
- `supervise:` lines name the claim token only (`claim`), never `issue`; the issue and role come from the token.
- `supervise: a turn ended` has two forms: `log/supervise-turn-ended.json` carries `claim` and `generation` (with `delivery` when the turn was a task's), while `log/supervise-turn-ended-outcome.json`, the turn-outcome line (supervise/turnoutcome.go:119), carries `generation`, `sequence`, `refused`, `refusedRun` and `limit`.
- `supervise: claim failed` is logged at `ERROR` with `cause`, `why` and the four budgets `launchFailures`, `deaths`, `promptFailures`, `promptRetires`.
- `handoff.recorded` carries `payload.phase`, `payload.version` and `payload.handoff_id` only.
