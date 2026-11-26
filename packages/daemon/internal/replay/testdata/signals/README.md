# Captured samples of the signals' inputs

Real output of the systems that R11's signals are read from, captured once on 2026-11-25 and neutralized for a public repository.
Every path below is relative to `packages/daemon/internal/replay/testdata/signals/`.
`TestEachSignalInputReaderReadsItsCapturedShape` (`signal_inputs_test.go`) reads every file here with its reader and holds each to the reading written out by hand from the file, so a file added here needs its reading in that test.
`TestCollectWritesAWindowThatReadsBackAndSendsGetAlone` serves the provider's minutes and two of the timelines to a collect.

The samples were neutralized by hand: no Go program runs where they were captured, and a timeline of GitHub is no input of a window that `legion replay neutralize` reads.
Each section names what was renamed; every other value is the captured one.
The keys and uuids here are under a map of their own, so `ACME-1` here and `ACME-1` under `../captured/` can name two different issues.
The raw captures and the map are kept outside the repository.

Placeholders in the commands below:

- `<launcher filter>`: the Cloud Logging filter that selects the launcher's container of the project's issue pods: `resource.type="k8s_container"`, the namespace, `resource.labels.container_name="roles"` and a pod name that starts `legion-<project>-<project>-`.

- `<KEY>` and `<seq>`: the issue and the place in its event log of the event kept.

- `$DISPATCH_URL`, and the Dispatch token sent on stdin as the `Authorization: Bearer` header (`-H @-`).

## The launcher's memory guard lines

`launcher/memory-guard.json` holds three Cloud Logging entries, one `ended`, one `stopped for this rise` and one `started` line, each the first entry of a read of its kind, 3 s apart:

```sh
gcloud logging read '<launcher filter> AND textPayload=~"^launcher: memory guard: <kind> "' \
  --limit=2 --format=json --freshness=10d
```

Each entry is cut to the three members a collect reads: `timestamp`, `resource.labels.pod_name` and `textPayload`.
Neutralizing changed each pod's name, `legion-<project>-<project>-<n>`, to `legion-acme-acme-<m>`, numbered in the order of `n`.
The texts are the guard's own format and hold no name, so they are as captured.
The deployed launcher's `ended` line lacks the `rule`, `sizes`, `less_parent` and `new_read` fields of today's, and its `stopped for this rise` line lacks `unread_processes`, so the reader reads the fields a line carries.

## GitHub's timelines of pull requests with each merge queue shape

`github/timeline-<n>.json` holds the timeline of the pull request `n` of cybermonklabs/legion, one page, as GitHub's REST API serves it:

```sh
gh api 'repos/cybermonklabs/legion/issues/<n>/timeline?per_page=100' --paginate
```

| File | Shape |
| --- | --- |
| `github/timeline-5150.json` | Added to the merge queue at 02:27:46, then merged and removed by the merge queue in the same second, 18:25:55, the merge first. |
| `github/timeline-4922.json` | Added to the merge queue and removed from it, with no merge. |
| `github/timeline-4910.json` | Added, removed and added again two seconds later, then removed by the merge queue and merged three seconds after that. |

The plan named #5150.
The other two were found with a search of the pull requests created from 2026-11-16 to 2026-11-21 (809 of them) for their merge queue events, through GitHub's GraphQL search with the timeline item types `ADDED_TO_MERGE_QUEUE_EVENT`, `REMOVED_FROM_MERGE_QUEUE_EVENT` and `MERGED_EVENT`.
Neutralizing cut every event of another kind to its `event` member, since their bodies, commit messages and cross-references name private issues and people.
It renamed the agent App named after a person to `person-agent[bot]`, its user object as `../captured/CAPTURED.md` does: its `login`, its `id` to 1, its `node_id`, its avatar to user 1 and every user URL.
GitHub's own merge queue App keeps its name.

## Dispatch's settings events and pull request attribution events

`dispatch/settings-events.json` holds three of the project's settings events, one of each type the project had, read from the whole event log:

```sh
printf 'Authorization: Bearer %s\n' "$(cat <dispatch token file>)" \
  | curl -sS -H @- "$DISPATCH_URL/api/v1/events/log?after=<id>&limit=1000"
```

The read paged the log from `after=0` to its end: 1,148 pages and 1,147,325 events in 264 s.
It kept the project's events whose type starts `settings.`: 24 of them, 19 `settings.repo_project.updated`, 4 `settings.architecture_source.updated` and 1 `settings.project_default_assignee.updated`.
That one event moves the default assignee, and carries `project` and `assignee` alone, without the `thread_logins` that today's Dispatch writes.
Neutralizing changed the project to `ACME`, the person's email to `person-1@example.invalid`, the session uuids and the token's id to `00000000-0000-4000-8000-00000000000<n>`, the token's name to `token-1`, and the repository other than cybermonklabs/legion to `acme/repo-1`.

`dispatch/attribution-events.json` holds two `issue.pull_request_attribution_changed` events, one whose source is the pull request's branch and one an external link, each read from its issue's event log:

```sh
printf 'Authorization: Bearer %s\n' "$(cat <dispatch token file>)" \
  | curl -sS -H @- "$DISPATCH_URL/api/v1/issues/<KEY>/events?limit=200&after=<seq − 1>" | jq '[.[] | select(.seq==<seq>)]'
```

Neutralizing changed `issue_key`, `project` and `payload.to` to `ACME-1` and `ACME-2`; the pull request's repository, number and link are of cybermonklabs/legion, which is public, and are as captured.

## The provider's minutes

`provider/minutes.jsonl` holds ten lines of the deployment's record of the model provider, as t25's provider-record script (kept off the repository, the version whose sha256 starts `d31e01255591`) wrote it for the project from the export of its per-team series:

```sh
provider-record.py --from <time> --to <time> --prefix <local copy of the provider export> > provider.jsonl
```

The lines kept are three minutes of 2026-11-16 from 14:30 with refusals by the project's own limit, four of 2026-11-17 from 17:46 whose third minute's uncached tokens are below zero (the provider's series counted the cached tokens a minute after their input), and three of 2026-11-23 from 16:28 with refusals of both kinds.
The lines hold no name, and the script writes none.
One value was changed by hand: `limit_source` reads `deployment-log` where the script wrote the name of the deployment's own log, since the repository names no part of a deployment.

## Not captured here

- Dispatch's ask events and the issue events that change an issue's assignee, which the approvals signal reads, are captured with that signal's definition.

- The pods' memory series, which only the memory model reads, is read from t25's export of it.
