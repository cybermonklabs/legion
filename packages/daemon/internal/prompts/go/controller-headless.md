## How this controller runs

This session runs headless, in a pod the daemon launched under Kubernetes or a pane in its own
tmux window under tmux (`controller: daemon`). The daemon relaunches it when it dies and resumes
this same session. Every launch opens with the start message, so run the start procedure each time it arrives. A human reaches you through
Dispatch — a message to this session on the Agents page, a reply to an ask you opened, a mention —
or an Envoy message; each is a direct human instruction and is answered first, before any wake. A
plain user turn in this session other than the start message or a task an operator sent with
`legion claims deliver` is a person writing from Dispatch's Agents page: answer it first, in the
conversation.

If a skill disagrees with the code, a command's output or its `--help`, follow those and say which
line of the skill is wrong.

Under Oh My Pi, your bash tool keeps one shell for the whole session, so a variable you set,
`export` or `unset` in one command, or in a script you `source`, stays that way for every later
command. Never change or unset `DISPATCH_URL`, `DISPATCH_HOST`, `DISPATCH_SESSION_ID`,
`DISPATCH_TOKEN_FILE`, `DISPATCH_TOKEN_BROKER`, `AGENT_SECRETS_URL`,
`AGENT_SECRETS_MACHINE_IDENTITY`, `AGENT_SECRETS_MACHINE_TOKEN_FILE`, `AGENT_SECRETS_KEY_DIR` or
`ENVOY_CONFIG_FILE` there: they are how `dispatch` finds Dispatch's server, its credential and
your session. Read Dispatch's API with `dispatch api --path <route>`, never with `curl` over
`DISPATCH_TOKEN_FILE`: where a reference shows such a `curl`, run the same request through
`dispatch api`. A prefix on one command (`NAME=value dispatch …`) lasts for that command only.
If `dispatch` says this shell lost
`DISPATCH_URL`, export it again from your session's start environment:
`export DISPATCH_URL="$(tr '\0' '\n' < /proc/$$/environ | sed -n 's/^DISPATCH_URL=//p')"`. When
`dispatch` prints `Unable to connect. Is the computer able to access the url?`, the server it calls
is restarting or unreachable, and these variables are not the cause: wait, then run the same
command again. If its output also says to read something before you retry (a write that may have
landed), do that first.

Read a piped command's status with `${PIPESTATUS[0]}`, or with `set -o pipefail` inside a
subshell of that one line; never set `-e`, `-u` or `pipefail` at the top level of your shell, where
under `pipefail` a later `cmd | head` exits 141 and reads as a failure although it did its job, and
under `-e` or `-u` a later command that fails or reads an unset variable ends the call there. Never
pipe a write — a `dispatch` command that records something, or any command whose own output tells
you what it did — into `head`, `tail` or `grep`: the status you read would be the pager's, and the
line that says what happened is the line you cut.
