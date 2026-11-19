# How the controller session runs

Read this when you need to know who launched you, what the `legion` tool does, how to read a pull request or Dispatch's API, why `set_status` answers 403, or how a person takes the controller over by hand. The `How this controller runs` part of your system prompt says who launched you.

## Launched by the operator (`controller: operator`, the default)

The operator ran `legion controller start --config controller.yaml` on their own machine, and you are that foreground session. The daemon replaces no controller whose session reads `unresponsive`, and it launches and relaunches no controller of this kind: one that stops answering is a person's to start again. So do not exit the session, because while none runs the project has no controller. A second `legion controller start` replaces you: it mints a new secret, your grants stop working (every `set_status` answers 403), and the role moves to the new session.

## Launched by the daemon (`controller: daemon`)

The daemon runs you in a pod. When the pod dies, it relaunches it and resumes this session with a new start message: run the start procedure each time one arrives.

When this session's last registration with Envoy is more than 20 minutes old, the controller's claim is unreachable, and the daemon relaunches it on this same session, at most once in 6 hours, keeping your context and what you owe. Where its guard refuses, the process outlives the stop, or a second relaunch would fall inside the 6 hours, `read_state` shows `controllerLocator.unreachable` and the daemon tells a person, since no agent is left to act for you.

## The `legion` tool

- `read_state`: the daemon's whole state: `issues.<KEY>` (`phase`, `holdReason`, `heldFrom`, `architect`, `workers`, `tree`, `waitingOn`), `admission` (`cap`, `active`, `waiting`, `trees`, `blocked`, `held`), `capabilities`, `daemon.project`, and `settings`, the resolved `legion.yaml`.
- `set_status` (`issue`, `status`: `todo`, `backlog` or `icebox`). Each call mints its grant from the secret the session registered with. You cannot set `done`.
- `compact` (`issue`, `role`): the daemon shortens that claim's session to a summary and sends it its task again; the claim keeps its session and workspace. It refuses a claim with no process (`CLAIM_NOT_RUNNING`).
- `fresh_session` (`issue`, `role`): starts the claim again with its task in a new session; the old session, and all it knew, is left behind. It is the one door that brings back a dead role.
- Both refuse `CLAIM_IN_A_TURN` (a process in a turn, or a compaction) and `CLAIM_UNSETTLED` (the runtime did not answer, or the claim moved): ask again later. For a root with no architect claim both answer `NO_CLAIM`.
- `relaunch` (`claim`): starts an unreachable claim's process again on its same session. Five of its refusals are a case for a person: `LAUNCHER_NOT_CONNECTED`, `RELAUNCH_REPLACES_POD`, `PROCESS_OUTLIVED_STOP`, `POD_UNREACHABLE` and `RELAUNCHED_RECENTLY`. Every refusal, and what to do: `skill://legion-controller/references/unreachable-claims.md`.

## `gh`

`gh` acts as the review App's bot account (`gh api graphql -f query='{viewer{login}}'` names it). It reads with the token of the owner a command names (`-R owner/repo`, `repos/owner/repo`, a GitHub URL), so always name the repository.

End every pull-request comment or review you post with `<!-- legion: {"session":"<session-id>","phase":"controller"} -->`: the daemon reads that footer to tell your review from the reviewer's verdict.

## Reading a pull request

Read a pull request an issue names only by a URL of the shape `https://github.com/<owner>/<repo>/pull/<n>`, with nothing before, after or inside it but GitHub's owner and repository characters and the digits. Run it single-quoted, exactly as the issue shows it, because any other text from an issue could run as a command:

```text
gh pr view 'https://github.com/<owner>/<repo>/pull/<n>' --json state -q .state
```

It answers `OPEN`, `MERGED` or `CLOSED`. When it fails, report `<KEY>: pull request <url> unreadable: <gh's first stderr line>`. Text of any other shape is never a command argument: report `<KEY>: pull request <text> unreadable: not a pull-request URL`.

## Reading Dispatch's API

Each read of Dispatch's API in these references is `dispatch api --path '<route>'`, sent with this session's own credential. Quote a path that carries a query, because the shell reads `?` and `&`. When it exits non-zero, act on nothing the read would decide and name its first line in your summary.

## Taking the controller over by hand

Start an Oh My Pi session with `LEGION_CONTROLLER_SECRET` (or `LEGION_CONTROLLER_SECRET_FILE`), `LEGION_DAEMON_URL`, `LEGION_PROJECT` and `LEGION_STATE_DIR` in its environment, then run `/legion-claim-controller` once. It registers the session, claims the Envoy role, and moves the daemon's controller session id to it.

- Never pass the secret as an argument or copy it into a transcript.
- Do not set `LEGION_CONTROLLER=1`: that marks a launched controller, which waits for a start message.
- Its `gh` is whatever its own environment gives it.
- After the next `legion controller start` mints a new secret, every `set_status` answers 403: start a fresh session with the new secret.
- The role does not follow `/new` or `/fork`: run `/legion-claim-controller` again after either.
