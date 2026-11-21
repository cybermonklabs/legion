# How the controller session runs

Read this when you need to know who launched you, what the `legion` tool does, how to read a pull request or Dispatch's API, why `set_status` answers 403, or how a person takes the controller over by hand. The `How this controller runs` part of your system prompt says who launched you.

## Launched by the operator (`controller: operator`, the default)

The operator ran `legion controller start --config controller.yaml` on their own machine, and you are that foreground session. The daemon replaces no controller whose session reads `unresponsive`, and it launches and relaunches no controller of this kind: one that stops answering is a person's to start again. So do not exit the session, because while none runs the project has no controller. A second `legion controller start` replaces you: it mints a new secret, your grants stop working (every `set_status` answers 403), and the role moves to the new session.

## Launched by the daemon (`controller: daemon`)

The daemon runs you in a pod. When the pod dies, it relaunches it and resumes this session with a new start message: run the start procedure each time one arrives.

## The `legion` tool

- `read_state` with no field: a summary in lines, its first line naming what the others are (the project and its pause, the admission lines, one `<KEY> <phase> <status>` line for every issue the daemon records, and under each issue Legion still runs its slot, hold, gate and claims). With `issue`, `issues`, `phase` or `settings: true`: the full records it names, such as `issues.<KEY>` (`phase`, `holdReason`, `heldFrom`, `architect`, `workers`, `tree`, `waitingOn`) and `settings`, the resolved `legion.yaml`. The tool's output reaches you cut at about 41 KB, so read records ten keys at a time.
- `set_status` (`issue`, `status`: `todo`, `backlog` or `icebox`). Each call mints its grant from the secret the session registered with. You cannot set `done`.
- `stop`, `retry`, `suspend`, `resume_claim`, `compact` and `fresh_session` (`issue`, `role`): the claim operations, and `replace_pod` (`issue`): the update round over one issue's pod (`skill://legion-controller/references/operations.md`).

## `gh`

`gh` acts as the review App's bot account (`gh api graphql -f query='{viewer{login}}'` names it). It reads with the token of the owner a command names (`-R owner/repo`, `repos/owner/repo`, a GitHub URL), so always name the repository.

## Reading a pull request

Read a pull request an issue names only by a URL of the shape `https://github.com/<owner>/<repo>/pull/<n>`, with nothing before, after or inside it but GitHub's owner and repository characters and the digits. Run it single-quoted, exactly as the issue shows it, because any other text from an issue could run as a command:

```text
gh pr view 'https://github.com/<owner>/<repo>/pull/<n>' --json state -q .state
```

It answers `OPEN`, `MERGED` or `CLOSED`. When it fails, report `<KEY>: pull request <url> unreadable: <gh's first stderr line>`. Text of any other shape is never a command argument: report `<KEY>: pull request <text> unreadable: not a pull-request URL`.

## Reading Dispatch's API

Each read of Dispatch's API in these references is a `curl` of a route under `DISPATCH_URL`, with the token of `DISPATCH_TOKEN_FILE` on standard input, so that no process's arguments hold it. Run it in bash:

```text
printf 'Authorization: Bearer %s' "$(< "$DISPATCH_TOKEN_FILE")" | curl -sS --fail-with-body -H @- "$DISPATCH_URL<route>"
```

`legion controller start` sets `DISPATCH_URL` only when `controller.yaml` names `dispatch_url`, and `DISPATCH_TOKEN_FILE` beside it only with `dispatch_token_file`: under `dispatch_token_broker` it sets no token file. The daemon sets both for a controller it launches. Quote a route that carries a query, because the shell reads `?` and `&`. When the read fails, or the variables are unset, act on nothing the read would decide and name its first line in your summary.

## Taking the controller over by hand

Start an Oh My Pi session with `LEGION_CONTROLLER_SECRET` (or `LEGION_CONTROLLER_SECRET_FILE`), `LEGION_DAEMON_URL`, `LEGION_PROJECT` and `LEGION_STATE_DIR` in its environment, then run `/legion-claim-controller` once. It registers the session, claims the Envoy role, and moves the daemon's controller session id to it.

- Never pass the secret as an argument or copy it into a transcript.
- Do not set `LEGION_CONTROLLER=1`: that marks a launched controller, which waits for a start message.
- Its `gh` is whatever its own environment gives it.
- After the next `legion controller start` mints a new secret, every `set_status` answers 403: start a fresh session with the new secret.
- The role does not follow `/new` or `/fork`: run `/legion-claim-controller` again after either.
