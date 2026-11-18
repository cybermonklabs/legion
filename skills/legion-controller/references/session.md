# How the controller session runs

Read this when you need to know who launched you, what `read_state` returns, how to read a pull
request, when `gh` or `curl` fails, when `set_status` answers 403, or when a person takes the
controller over by hand. The `How this controller runs` part of your system prompt says who
launched you.

## Launched by the operator (`controller: operator`, the default)

The operator ran `legion controller start --config controller.yaml [--daemon-url <url>]` on their
own machine, and you are that foreground session. The daemon reads your liveness from the Envoy
role registry: the holder of `legion-<project>-controller`, its state and its `last_seen`. It
replaces no controller whose session reads `unresponsive` — past the live window, inside the gone
horizon, still the holder — and acts only on one that reads `gone`. The daemon launches no
controller itself, and relaunches none either, whatever Envoy says of this session, because it
holds no launcher for a session an operator started: a controller of this kind that stops
answering is a person's to start again. So do not exit the session: while none runs, the project
has no controller. A second `legion controller start` replaces you: it mints a new secret, your
grants stop working (every `set_status` answers 403), and the role moves to the new session.

## Launched by the daemon (`controller: daemon`)

The daemon runs you in a pod and supervises you like a root architect. When the pod dies, the
daemon relaunches it and resumes this session with a new start message, so run the start procedure
each time one arrives. A person reaches you through Dispatch (a message on the Agents page, an
answer to your ask, a mention), Envoy or Slack. No `legion controller start` can replace you.

The daemon reads this session's liveness in Envoy's session list, as it reads every claim's.
When the last registration of this session is more than 20 minutes old, your claim is out of reach, and the daemon relaunches it on its same session, at most once in 6 hours: it stops this process through the launcher and starts it again, and it replaces the controller's pod where the launch needs that or the launcher is not connected, since no other role runs in that pod.
The relaunch keeps this session, its context and whatever it owes, and the start message that follows is the one you run the start procedure on.

That relaunch is refused while your worker stream has shown a step in the last 20 minutes, while you are in a turn and your session has sent an activity report in that turn in the last 20 minutes (it sends one every 5 minutes while a turn is open), while your open call is anything but a `wait`, and while your activity report since your launch shows a background job or a subagent running, which is the guard that keeps it off a turn still doing work.
Where it is refused, where the process outlives the stop, and where a second relaunch would fall inside the 6 hours, `read_state` names the controller's claim unreachable (`controllerLocator.unreachable`) and the daemon writes the line that tells a person in Slack's #fleet-alerts, because no agent is left to act for you.

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
- `op: "relaunch"` starts an unreachable claim's process again on the session it already runs: it takes the `claim` token the daemon's notice carries, stops the old process through that claim's launcher and starts the new one, and the claim keeps its session, its workspace and the task it owes.
  Nothing is charged to the claim, and the resumed session is told that its turn was cut.
  The daemon refuses a claim it no longer marks unreachable (`CLAIM_REACHABLE`), one between states (`CLAIM_UNSETTLED`: not ready, idle or working, under a held suspension, a waiting wake or a compaction, recording no process, or in a pod the runtime could not read), and the controller's own claim (`CLAIM_IS_CONTROLLER`); and it refuses `RELAUNCH_GUARD` a claim whose worker stream has shown a step in the last 20 minutes, one in a turn whose session has sent an activity report in that turn in the last 20 minutes, one whose open call is not a `wait`, one whose activity report since its launch shows a background job or a subagent running, and one that has sent no report since its launch.
  Five refusals are a case for a person instead: the role's launcher is not connected (`LAUNCHER_NOT_CONNECTED`), the launch would replace the pod (`RELAUNCH_REPLACES_POD`), the process outlived the stop (`PROCESS_OUTLIVED_STOP`), every claim of a pod of two or more claims is unreachable (`POD_UNREACHABLE`), and the claim was relaunched in the last 6 hours (`RELAUNCHED_RECENTLY`) (`skill://legion-controller/references/unreachable-claims.md`).

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
