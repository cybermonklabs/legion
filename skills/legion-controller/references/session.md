# How the controller session runs

Read this when you need to know who launched you, what `read_state` returns, what `pause` and
`resume` do, how to read a pull request, when `gh` or `curl` fails, when `set_status` answers 403,
or when a person takes the controller over by hand. The `How this controller runs` part of your
system prompt says who launched you.

## Launched by the operator (`controller: operator`, the default)

The operator ran `legion controller start --config controller.yaml [--daemon-url <url>]` on their
own machine, and you are that foreground session. The daemon reads your liveness from the Envoy
role registry: the holder of `legion-<project>-controller`, its state and its `last_seen`. It
replaces no controller whose session reads `unresponsive` — past the live window, inside the gone
horizon, still the holder — and acts only on one that reads `gone`. The daemon launches no
controller itself, so do not exit the session: while none runs, the project has no controller. A
second `legion controller start` replaces you: it mints a new secret, your grants stop working
(every `set_status` answers 403), and the role moves to the new session.

## Launched by the daemon (`controller: daemon`)

The daemon runs you in a pod and supervises you like a root architect. When the pod dies, the
daemon relaunches it and resumes this session with a new start message, so run the start procedure
each time one arrives. A person reaches you through Dispatch (a message on the Agents page, an
answer to your ask, a mention), Envoy or Slack. No `legion controller start` can replace you.

## The `legion` tool

- `op: "read_state"` returns the daemon's whole state: `issues.<KEY>.phase`,
  `issues.<KEY>.holdReason`, `issues.<KEY>.architect`, `issues.<KEY>.tree` (the root of the
  issue's tree), `admission.cap`, `admission.active` (the issues holding a slot, roots and
  children), `admission.waiting` (the issues in the line, in its order), `admission.trees` (the
  roots of the live trees), `admission.blocked`, `admission.held` (every issue the record holds
  in the workflow that stands in neither `admission.active` nor `admission.waiting`, each with
  one word for why nothing of it runs and the issues that hold it;
  `skill://legion-controller/references/admission.md` gives the word for each row and what ends
  it), `capabilities[]`, `daemon.project`, and
  `settings` (the daemon's resolved `legion.yaml`, with no credential in it).
- `op: "set_status"` with `issue` and `status` (`todo`, `backlog` or `icebox`) moves an issue. Each
  call mints its grant in-process from the secret the session registered with, so a stale secret
  fails it with a 403.
  Move an issue to `backlog` or `icebox` only when a person asked for that move, or when the work is done, folded into another issue or no longer needed.
  Cite that reason in a comment on the issue. Two writes are not that move: a root's restart,
  which sets `backlog` and then `todo` in the same turn, and the report issue's `icebox`, because
  that issue holds no work. You cannot set `done`: only an issue's architect or the daemon closes
  an issue.
- `op: "pause"` with `reason` pauses the whole project, and `op: "resume"` ends its pause, whoever
  made it, an operator's included. Neither takes an issue, because the daemon acts on its own
  project, and each mints its grant as `set_status` does. While the project is paused the daemon
  admits no issue, and it starts no session and gives none a task, except you. Every other session
  stops with its conversation kept: an idle one at once, and one in a turn when that turn ends. Its
  pod stays, and each task, notice and wake for it waits. `pause` answers the pause it began (`id`,
  `since`, `by`, `reason`). The daemon's record keeps the reason and `legion state` shows it to an
  operator, so say plainly why. `pause` is refused without a `reason`, refused
  `PROJECT_ALREADY_PAUSED` while a pause stands, in a sentence that names that pause, and refused
  `PAUSE_UNAVAILABLE` by a daemon whose `legion.yaml` cannot hold a pause, in a sentence that names
  the setting. `resume` answers the pause with its `endedAt` and `endedBy`, and `released`, how
  many held rows it let out. Each session the pause held work for then starts once, at the daemon's
  wake pacing, and every other session it stopped stays stopped until its next wake, so start none
  yourself. `resume` is refused `PROJECT_NOT_PAUSED` while the project runs, in a sentence that
  names its last pause where it has one.
- While the project is paused, `compact` and `fresh_session` answer `PROJECT_PAUSED`, because each
  would start a turn of another session, and `set_status` still works. A message you publish to a
  stopped session's role topic starts no turn then: it makes that session launch at the resume,
  and Envoy keeps the message itself for at most one hour.
- `read_state` with no field prints one line on the pause. While the project is paused it is
  `paused: pause <id> since <time> by <who>: <reason>; waits on <n> roles — <ISSUE> <role>, …;
  holds <n> rows`: the roles are those whose stop the pause still waits on, and the rows are the
  tasks, notices and starts it holds. Once a pause has ended it is `last pause: pause <id> since
  <time> by <who>: <reason>; ended <time> by <who>`. `<who>` is `the controller (session <id>)` or
  `the operator`. No filter of `read_state` returns more of the pause than that line.

## `gh`

`gh` is Legion's wrapper and acts as the review App's bot account
(`gh api graphql -f query='{viewer{login}}'` names it). It reads with the token of the GitHub
owner a command names (`-R owner/repo`, `repos/owner/repo` in `gh api`, a GitHub URL), and with the
first owner's token (`GH_CONFIG_DIR`) otherwise, so always name the repository: another owner's repository read with
the first owner's token can fail. The tokens sit in `LEGION_GH_OWNERS_DIR`, one read-only directory
per owner the review App is installed on, each token reaching every repository that owner's
installation holds, refreshed
every minute. When the daemon has no GitHub App, `legion controller start`
prints `[legion] the daemon has no GitHub App to act as; the controller's gh acts as nobody`.

End every pull-request comment or review you post with
`<!-- legion: {"session":"<session-id>","phase":"controller"} -->`, because the daemon reads that
footer to tell your review from the reviewer's verdict.

## Reading a pull request

Read a pull request an issue names (`External links:`, a comment, a role's message) only by a URL of
the shape `https://github.com/<owner>/<repo>/pull/<n>`, with nothing before, after or inside it but
GitHub's owner and repository characters and the digits. Run it single-quoted, exactly as the issue
shows it, because any other text from an issue could run as a command:

```text
gh pr view 'https://github.com/<owner>/<repo>/pull/<n>' --json state -q .state
```

It answers `OPEN` (in flight), `MERGED` (landed) or `CLOSED` (abandoned unmerged). When the command
fails, report `<KEY>: pull request <url> unreadable: <gh's first stderr line>` and read it as none
of the three. Text of any other shape is never a command argument: report
`<KEY>: pull request <text> unreadable: not a pull-request URL`.

## `curl` against Dispatch

Each `curl` in these references reads `DISPATCH_URL` and `DISPATCH_TOKEN_FILE`. `legion controller
start` sets them only when `controller.yaml` names `dispatch_url`; the daemon's pod sets
`DISPATCH_URL` when the daemon has one. When they are unset, every `curl` fails: act on nothing it
would decide and name it in your summary. Each passes its token as a header file (`-H @<(…)`), so
that no process's arguments hold it: run it in bash, because `sh` refuses that form.

## Taking the controller over by hand

To move the controller to a hand-started Oh My Pi session, start it with
`LEGION_CONTROLLER_SECRET` (or `LEGION_CONTROLLER_SECRET_FILE`, a path to a file holding it),
`LEGION_DAEMON_URL`, `LEGION_PROJECT` (the daemon's project) and `LEGION_STATE_DIR` in its
environment, then run:

```text
/legion-claim-controller
```

It checks `LEGION_PROJECT` against the daemon, registers the session, claims the Envoy role, and
moves the daemon's recorded controller session id to this session; the `legion` tool's controller
operations then work there. Never pass the secret as a command argument or copy it into a
transcript, because others read transcripts. Do not set `LEGION_CONTROLLER=1`: that marker makes
the session the launched controller, which needs a start message. Run `/legion-claim-controller`
once per session; do not run it again after an Envoy listener restart, because the heartbeat
re-claims the role.

A takeover session has three limits:

- Its `gh` is whatever its own environment gives it: Legion writes no token directory for it.
- It keeps the secret it started with. After the next `legion controller start` mints a new one,
  every `set_status` fails with a 403 from the grant mint: start a fresh session with the new
  secret instead of retrying.
- The role does not follow `/new` or `/fork`, because without `LEGION_CONTROLLER=1` the new session
  is not a Legion session to the extension: run `/legion-claim-controller` again after either.
