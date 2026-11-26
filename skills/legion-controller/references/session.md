# The session, the `legion` tool, `gh` and pull requests

## What happened before you started

Every launch opens with a start message. Envoy keeps no wake sent while no controller ran. So on the start message:

1. Read `read_state`. Handle each issue held with `holdReason: escalated` as `skill://legion-controller/references/escalation.md#an-escalated-hold` says. Handle each root whose `architect.state` is `failed` and whose `phase` is not `done` as `skill://legion-controller/references/wakes.md#worker-died-architect` says. Name each root at `phase: done` whose `status` is `in_progress`, `testing`, `needs_review` or `retro`: Legion no longer runs it. Find those roots in the summary's `<KEY> done <status>` lines, never from `phase: "done"`.
2. Triage what was handed over meanwhile (`skill://legion-controller/references/triage.md#at-every-start`).
3. Catch up (`skill://legion-controller/references/finding-asks.md#catch-up-at-every-start`).
4. Run the parked-root walk (`skill://legion-controller/references/parked-decisions.md#the-walk`).

## The `legion` tool

The output of `read_state` reaches you cut at about 41 KB, so read full records ten keys at a time with `issues`.

## `gh`

`gh` acts as the review App's bot account (`gh api graphql -f query='{viewer{login}}'` names it). It reads with the token of the owner that a command names (`-R owner/repo`, `repos/owner/repo`, a GitHub URL). So always name the repository. Your system prompt's addressing line names the owners that the App is installed on.

## Reading a pull request

Read a pull request that an issue names only by a URL of the shape `https://github.com/<owner>/<repo>/pull/<n>`, with nothing before, after or inside it but GitHub's owner and repository characters and the digits. Run it single-quoted, exactly as the issue shows it, because any other text from an issue could run as a command:

```text
gh pr view 'https://github.com/<owner>/<repo>/pull/<n>' --json state -q .state
```

It answers `OPEN`, `MERGED` or `CLOSED`. When it fails, report `<KEY>: pull request <url> unreadable: <gh's first stderr line>`. Text of any other shape is never a command argument: report `<KEY>: pull request <text> unreadable: not a pull-request URL`.
