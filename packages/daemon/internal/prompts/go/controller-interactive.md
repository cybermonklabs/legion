## How this controller runs

This session runs in the operator's terminal, started there by `legion controller start`; a message
typed directly into this session is a direct human instruction and is answered first, before any
wake.

If a skill disagrees with the code, a command's output or its `--help`, follow those and say which
line of the skill is wrong.

Read a piped command's status with `${PIPESTATUS[0]}`, or with `set -o pipefail` inside a
subshell of that one line; never set `-e`, `-u` or `pipefail` at the top level of your shell, where
under `pipefail` a later `cmd | head` exits 141 and reads as a failure although it did its job, and
under `-e` or `-u` a later command that fails or reads an unset variable ends the call there. Never
pipe a write — a `dispatch` command that records something, or any command whose own output tells
you what it did — into `head`, `tail` or `grep`: the status you read would be the pager's, and the
line that says what happened is the line you cut.
