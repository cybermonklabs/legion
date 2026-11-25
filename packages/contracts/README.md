# @legion/contracts

Shared event contracts for the Legion monorepo, including the Envoy subsystem.

This package is the language-neutral contract layer for cross-runtime event payloads.

Current scope:

- envelope schema and subject helpers
- the Dispatch specs the `dispatch` command is built from, over `zodSchemaApi(hostZod)`
- Go and TypeScript generation (below)

This package has no release of its own, so nothing renders its changelog entries: each stays
readable as the fragment file that holds it, under `.changes/unreleased/`. `CHANGELOG.md` is the
history before that and stops where the fragments begin.

## Commands

Run each from this directory.

- `bun test`; one file: `bun test src/dispatch-tools.test.ts`.
- `bun run typecheck` and `bun run lint`.
- `bun run gen:go` rewrites the files `scripts/gen-go.ts` generates; `bun run check:go` names a
  stale one and writes nothing.
- `bun run gen:api` rewrites `src/dispatch-api.generated.ts`; `bun run check:api` compares it and
  writes nothing.

## Generated files

Change the source and regenerate; never edit these by hand.

- `scripts/gen-go.ts` writes, in `packages/envoy`, `listenerwire/envelope_generated.go` (the
  envelope), `internal/contracts/generated.go` (its aliases and every shared constant),
  `internal/contracts/dispatch_tools_generated.go` (from `src/dispatch-tools.ts`),
  `internal/dispatch/pmdoc/schema_generated.go` (from `schemas/block-schema.schema.json`) and
  `internal/dispatch/text/dashboard-pages.json` (a copy of `src/dashboard-pages.json`, which the
  server embeds), and here `src/block-schema.generated.ts`.
- `src/dashboard-pages.json`, the dashboard's pages file, is written from the dashboard's route
  table by `bun web/src/features/refs/dashboard-pages-file.ts --write` in `packages/dispatch`.
- `schemas/dispatch-openapi.json` is reflected from Dispatch's Go route table. In
  `packages/envoy`, `go run ./cmd/dispatch openapi --check` compares it with a fresh render and
  prints the command that rewrites it.
- `src/dispatch-api.generated.ts` is rendered from that document by `bun run gen:api`.

## GitHub Topic Hierarchy

The GitHub receiver publishes per-resource topics. Consumers subscribe at the
granularity they need using wildcard patterns.

### Base subjects

| Helper | Example output |
|--------|---------------|
| `githubSubject(owner, repo, kind)` | `notifications.github.acme.widgets.pr` |
| `githubResourceSubject(owner, repo, type, number)` | `notifications.github.acme.widgets.pr.42` |

### Published topic patterns

| Event type | Published topic |
|-----------|----------------|
| PR opened/closed/merged | `notifications.github.{owner}.{repo}.pr.{number}` |
| PR check settlement | `notifications.github.{owner}.{repo}.pr.{number}.checks` |
| PR comment | `notifications.github.{owner}.{repo}.pr.{number}.comment` |
| PR review | `notifications.github.{owner}.{repo}.pr.{number}.review` |
| Issue event | `notifications.github.{owner}.{repo}.issue.{number}` |
| Issue comment | `notifications.github.{owner}.{repo}.issue.{number}.comment` |
| Mention (repo-level) | `notifications.github.{owner}.{repo}.mention` |
| Mention (resource-level) | `notifications.github.{owner}.{repo}.{type}.{number}.mention` |
| Actions job finished on the default branch, of a run with no PR (`githubJobSubject`) | `notifications.github.{owner}.{repo}.job.{run_id}.completed` |

### Subscription granularity

| Want | Subscribe to |
|------|-------------|
| All events for PR #42 | `notifications.github.acme.widgets.pr.42`, `notifications.github.acme.widgets.pr.42.>` |
| All PR events in repo | `notifications.github.acme.widgets.pr.>` |
| All events in repo | `notifications.github.acme.widgets.>` |
| Exact resource topic | `notifications.github.acme.widgets.pr.42` |

The `>` wildcard matches one or more following levels.
The `*` wildcard matches exactly one level.

`{owner}` and `{repo}` are each one level: a dot in either is written `_`, as
`sanitizeSubjectSegment` writes one, so `sjawhar/.github` publishes under
`notifications.github.sjawhar._github`. The spelling is lossy (`a.b` and `a_b`
share one), so a consumer takes the repository from the payload's `repo`.

### CI check settlement

`notifications.github.{owner}.{repo}.pr.{number}.checks` publishes once a commit of the pull request has its recorded checks and suites settle after the quiet period, whether or not that commit is still the head. The payload's `sha` names the commit, and a consumer decides which commit a settlement stands for.

The payload's `check_runs` names the latest run of each check that has a run id, as `{name, id, conclusion}`, sorted by name. `conclusion` is GitHub's own word for that run (`success`, `failure`, `neutral`, `cancelled`, `skipped`, `timed_out`, `action_required`, `startup_failure` or `stale`), passed on as GitHub gave it, a word GitHub adds later included; `fixtures/checks-settlement/listener-rules.json` holds one, `a_conclusion_github_adds_later`. It is left out where the listener's record holds no word for the run, and a listener of an earlier release sends it for no run. A consumer shows a check by its word and decides by the groups, not by the word: the groups `failed`, `running`, `passed`, `queued`, `cancelled` and `skipped` are the listener's rule, so a word a consumer does not know still lands in one group, and a completed run whose word the listener does not know is in `failed`. `skipped` names each check whose run GitHub concluded `skipped`: that check ran nothing on the commit.

The payload's `statuses` names each commit status the record holds for that commit, as `{context, state, created_at}` in context order, with `state` one of `success`, `failure` and `error` and `created_at` GitHub's RFC 3339 text of when it created the status. The field is left out when the record holds no status. A commit status is no check run: `check_runs`, the status groups and the failing names hold check runs alone, and a status and a check run of one name are two entries. A status has no id, so `created_at` orders two statuses of one context, never `generation`: a settlement that names a context a consumer keeps no status of for that commit, or one with a later `created_at` than the status it keeps of that context, changes those statuses alone where its attempt set is older than the one the consumer keeps, or the same at a `generation` no higher than the kept one, and is newer, and applied whole, only where its attempt set is newer or mixed, or the same at a higher `generation`. A consumer merges the `statuses` of each settlement it applies into the statuses it keeps, one context at a time: the entry with the later `created_at` stands, on an equal one the settlement's where it is applied whole and the kept one where it changes those statuses alone, and a context the settlement leaves out stays as kept (the Legion daemon's `ClassifySettlement` and `mergedStatuses`, `packages/daemon/internal/classify/settlement.go`). Dispatch's CI reconcile records the statuses from GitHub's read of each open head, since GitHub's `status` webhook event names no pull request. `fixtures/checks-settlement/listener-rules.json` holds a settlement with statuses.

## Slack Topic Hierarchy

The Slack receiver publishes channel-level and thread-level topics. Consumers
subscribe at the granularity they need using wildcard patterns.

### Base subjects

| Helper | Example output |
|--------|---------------|
| `slackSubject(team, channel, kind)` | `notifications.slack.T01234567.C0A0DHVU8HE.message` |
| `slackThreadSubject(team, channel, threadTs, kind)` | `notifications.slack.T01234567.C0A0DHVU8HE.thread.1234567890_123456.message` |

### Published topic patterns

| Event type | Published topic(s) |
|-----------|-------------------|
| Channel message | `notifications.slack.{team}.{channel}.message` |
| Channel mention (`app_mention`) | `notifications.slack.{team}.{channel}.mention` |
| Thread reply (message) | Channel topic + `notifications.slack.{team}.{channel}.thread.{normalized_ts}.message` |
| Thread mention (`app_mention` in thread) | Channel topic + `notifications.slack.{team}.{channel}.thread.{normalized_ts}.mention` |
| Standalone message (no `thread_ts`) | Channel topic only (no thread envelope) |

**Thread timestamp normalization:** Slack `thread_ts` values contain dots
(e.g., `1234567890.123456`) which conflict with NATS segment separators. The
`slackThreadSubject` helper normalizes by replacing `.` with `_`
(→ `1234567890_123456`), making the thread identifier a single NATS segment.

### Subscription granularity

| Want | Subscribe to |
|------|-------------|
| All events in channel | `notifications.slack.T.C.>` |
| All messages in channel | `notifications.slack.T.C.message` |
| All mentions in channel | `notifications.slack.T.C.mention` |
| All events in specific thread | `notifications.slack.T.C.thread.1234567890_123456.>` |
| Only messages in thread | `notifications.slack.T.C.thread.1234567890_123456.message` |
| Only mentions in thread | `notifications.slack.T.C.thread.1234567890_123456.mention` |
| All threads in channel | `notifications.slack.T.C.thread.>` |

### Deduplication

A session subscribed to both `notifications.slack.T.C.>` and a specific thread
topic receives each event **once** (not duplicated), because both envelopes
share the same `dedupe_key` and the listener deduplicates by
`(dedupe_key, session_id)`.
