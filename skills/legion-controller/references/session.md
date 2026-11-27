# How the controller session runs

Read this when you need to know who launched you, what the `legion` tool does, how to read a pull request or Dispatch's API, why `set_status` answers 403, or how a person takes the controller over by hand. The `How this controller runs` part of your system prompt says who launched you.

## Launched by the operator (`controller: operator`, the default)

The operator ran `legion controller start --config controller.yaml` on their own machine, and you are that foreground session. The daemon replaces no controller whose session reads `unresponsive`, and it launches and relaunches no controller of this kind: one that stops answering is a person's to start again. So do not exit the session, because while none runs the project has no controller. A second `legion controller start` replaces you: it mints a new secret, your grants stop working (every `set_status` answers 403), and the role moves to the new session.

## Launched by the daemon (`controller: daemon`)

The daemon runs you in a pod.

## What happened before you started

The start procedure (`skill://legion-controller/SKILL.md`, *What happened before you started*). Every launch opens with a start message. Envoy keeps no wake sent while no controller ran, so on it:

1. Read `read_state`. Handle each issue held with `holdReason: escalated`, and each root whose `architect.state` is `failed` and whose `phase` is not `done`, as its wake says. Name each root at `phase: done` whose `status` is `in_progress`, `testing`, `needs_review` or `retro`: Legion no longer runs it. Take them from the summary's `<KEY> done <status>` lines, ten keys an `issues` read, never from `phase: "done"`.
2. Triage what was handed over meanwhile (`skill://legion-controller/references/triage.md#at-every-start`).
3. Catch up (`skill://legion-controller/references/finding-asks.md#catch-up-at-every-start`).
4. Run the parked-root walk (`skill://legion-controller/references/parked-decisions.md#the-walk`).

## The `legion` tool

- `read_state` with no field: a summary of the daemon's whole state, one line per fact: the project, the pause that stands or the last one, admission (`cap`, `active`, `waiting`, `blocked`), the wakes the pacing holds, each capability, one line per recorded issue, and under each issue Legion still runs its slot, hold, gate and each claim's state with its last turn times.
  With `issue`, `issues` or `phase` it returns those issues' full records, `issues.<KEY>` (`phase`, `holdReason`, `heldFrom`, `architect`, `workers`, `otherClaims`, `tree`, `waitingOn`), and with `settings: true` the resolved `legion.yaml`; it takes one of these at a time. The tool's output reaches you cut at about 41 KB, so read records ten keys at a time.
- `set_status` (`issue`, `status`: `todo`, `backlog` or `icebox`). Each call mints its grant from the secret the session registered with. You cannot set `done`.
- `stop`, `retry`, `suspend`, `resume_agent`, `compact` and `fresh_session` (`issue`, `role`): the actions on an agent, each naming one agent by its issue and role, and `replace_pod` (`issue`): the update round over one issue's pod (`skill://legion-controller/references/operations.md`).
- `issue` and `role` reach the agent that works the role's step that started last: the step the task stands at, where it is that role's. Two claims of one issue can hold one role; when a notice names a claim by its token, pass that token to `compact` or `fresh_session`, which take it as `claim` (`issue`, `role`, optional `claim`), so the call reaches that claim. Both refuse a token of another issue (`INVALID_CLAIM`) and a claim that holds another role (`CLAIM_NOT_OF_ROLE`).

## `gh`

`gh` acts as the review App's bot account (`gh api graphql -f query='{viewer{login}}'` names it). It reads with the token of the owner a command names (`-R owner/repo`, `repos/owner/repo`, a GitHub URL), so always name the repository.

## Reading a pull request

Read a pull request an issue names only by a URL of the shape `https://github.com/<owner>/<repo>/pull/<n>`, with nothing before, after or inside it but GitHub's owner and repository characters and the digits. Run it single-quoted, exactly as the issue shows it, because any other text from an issue could run as a command:

```text
gh pr view 'https://github.com/<owner>/<repo>/pull/<n>' --json state -q .state
```

It answers `OPEN`, `MERGED` or `CLOSED`. When it fails, report `<KEY>: pull request <url> unreadable: <gh's first stderr line>`. Text of any other shape is never a command argument: report `<KEY>: pull request <text> unreadable: not a pull-request URL`.

## Reading Dispatch's API

Each read of Dispatch's API in these references is a `dispatch api` of a route under `/api/v1`, which reads it with this session's own credential and prints Dispatch's JSON answer. Run it in bash:

```text
dispatch api --path '<route>'
```

Quote a route that carries a query, because the shell reads `?` and `&`. When the read fails, act on nothing the read would decide and name its first line in your summary.

## Taking the controller over by hand

Start an Oh My Pi session with `LEGION_CONTROLLER_SECRET` (or `LEGION_CONTROLLER_SECRET_FILE`), `LEGION_DAEMON_URL`, `LEGION_PROJECT` and `LEGION_STATE_DIR` in its environment, then run `/legion-claim-controller` once. It registers the session, claims the Envoy role, and moves the daemon's controller session id to it.

- Never pass the secret as an argument or copy it into a transcript.
- Do not set `LEGION_CONTROLLER=1`: that marks a launched controller, which waits for a start message.
- Its `gh` is whatever its own environment gives it.
- After the next `legion controller start` mints a new secret, every `set_status` answers 403: start a fresh session with the new secret.
- The role does not follow `/new` or `/fork`: run `/legion-claim-controller` again after either.
