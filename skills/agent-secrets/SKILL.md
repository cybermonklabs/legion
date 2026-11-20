---
name: agent-secrets
description: Use when a command needs a token, a key or a password, when you store a secret for agents, before you say that a service or an API is out of reach, and when an instruction names `agent-secrets`.
---

# agent-secrets

`agent-secrets` is the secrets broker's client. It asks the broker for a secret and gives it to one command, in that command's environment, so your shell and your transcript never hold the value.
`agent-secrets --help` lists every form, flag, environment variable and exit code.
Every message the client prints is explained at https://cybermonklabs.github.io/legion/broker/guides/troubleshooting/.

## The one form

```sh
agent-secrets NAME... [--reason TEXT] [--wait DURATION] [--ttl DURATION] -- <command> [args...]
```

- Each granted secret reaches `<command>`'s environment under its own name. Nothing runs unless every name is granted.
- `--reason` is necessary wherever a person or the model judge decides: such a request that carries none is denied whole. A request every name of which is granted at once needs none. Say what the credential is for, in a sentence the approver can decide on, and name in it the issue you work on by its key, an issue that is not done.
- Each key says who decides a request for it: `agent-secrets secret show NAME` prints it on its `decides:` line, beside the `guidance:` a model judge decides by. Meet that guidance in `--reason`. The judge grants a request or refuses it for good (77); one it cannot decide goes to the key's person. A request that also names a key a person decides goes to that person whole.
- A session that must not block passes `--wait 0s` and reads the decision later with `agent-secrets status <request_id>`.
  The same command run again (the same names and `--ttl`) uses the live grant or joins the waiting request, and asks nobody again.
- Read `skill://agent-secrets/references/github-app.md` before you use a GitHub App secret: what its name holds, the flags that narrow its token, and the refusals of a narrowing.

## The command reads the value, your shell does not

Your own shell expands `"$NAME"` inside double quotes before `agent-secrets` runs, and there the name is empty, so the API answers as if you sent no credential.
Put the command in single quotes inside `sh -c`, and give `curl` the header on standard input (`-H @-`), so the value is in no command's arguments:

```sh
agent-secrets LEGION_SLACK_BOT_TOKEN --reason 'check that the Slack token works' -- \
  sh -c 'printf "Authorization: Bearer %s" "$LEGION_SLACK_BOT_TOKEN" | curl -sS -H @- https://slack.com/api/auth.test'
```

Read the body, not the exit status alone: Slack and many HTTP APIs answer `200` with `"ok": false`.
Send the body to `jq` straight from `curl` or through `printf '%s'`, never through `echo`, which can turn a `\n` inside a JSON string into a control character.
Read `skill://agent-secrets/references/slack.md` before you read from Slack or post to it.

## The value never reaches your transcript

Your transcript keeps everything a command prints.

- Never print the value, and never run `curl -v`, `curl --trace` or `set -x` around it: each of them writes the header.
- Read `skill://agent-secrets/references/check-a-value.md` before you check a value's shape, or take a credential out of a JSON value.
- Compare two values only as equal or not equal. Never print, slice or diff their characters.

## A secret behind the airlock

Read `skill://agent-secrets/references/airlock.md` when a name holds `agent-secrets-placeholder-` and 32 letters and digits, and before you retry a call the broker's proxy refused (an HTTP error naming a rule, `407` or `503`).

## What the exit status means

After a grant the client writes `agent-secrets: <names> from <source>` to stderr, and the status is then the command's own.
One case breaks that: a line `agent-secrets: exec <command>: …` after it means the command could not start, and the status is 1.
Without the grant line the status is the client's:

- **1**: a refusal or a failure that the message names: a name the broker does not serve (`UNKNOWN_SECRET`), a secret that waits for its first value (`PENDING_VALUE`), a broker it cannot reach, a machine that is not logged in to the broker, a request cancelled or expired, `REQUEST_PENDING`, a grant that is no longer live, a command that is not on `PATH`, which is refused before the broker is asked anything, so it reaches no approver, or no broker identity at all.
  `UNKNOWN_SECRET` is final: do not ask again, and do not try a name of your own.
  `PENDING_VALUE` means its owner is storing the value now with `agent-secrets secret create`; no request was made and no approver asked. Ask again once a minute, for ten minutes at most: ten minutes after its making the broker stops calling it pending and refuses the name `UNKNOWN_SECRET`. Then tell its owner, who sees it as `versions: none` in `agent-secrets secret show NAME`.
  `REQUEST_PENDING`: any other run that would itself wait for a person and shares a name with a waiting request, with another `--ttl` or another set of names, is refused `REQUEST_PENDING`, which names the request already waiting.
- **2**: a usage error, such as no `AGENT_SECRETS_URL` or no `--`.
- **75**: the request still waits for a person or the model judge when `--wait` runs out, and nothing ran. The message gives the `agent-secrets status <request_id>` that reads it.
  Read `skill://agent-secrets/references/waiting.md` when a run exits 75, and before you end a grant with `agent-secrets grant revoke`.
- **77**: the request was denied: the approver or the model judge denied it, the policy refused a name (a service's secret, or one whose requesters list does not name this session), or the request named a secret a person must approve and carried no reason, which the detail reads `<NAME>: a reason is necessary; ask again with --reason`. Final: ask for none of it again, and try no name of your own. The one exception is a message that says `ask again with --reason`: ask again with `--reason`.

## Where a name comes from

The broker tells a session no names. A name comes from your instructions, your role prompt, a skill or the issue you work on; there is no list to browse.
Request a secret only to use it for the work in front of you: every request is recorded against your session with its reason, and can reach a person.
A secret that a service owns goes to that service's own pods and to nothing else: the broker grants it at once when the session's own service is the secret's owner, and denies every other session, with no person asked.
A shared agent secret, such as `LEGION_SLACK_BOT_TOKEN`, goes at once to every session, a pod's and a person's alike, unless its `requesters` tag lists who may ask for it.

## Before you say that a service is out of reach

1. Run the call through `agent-secrets`, with the secret your instructions name.
2. A GitHub `404` under your session's own token is that token's reach, not your access: GitHub answers `404` for a repository a token cannot see.
   Ask for the GitHub App secret your instructions name, narrowed with `--github-owner`, `--github-repo`, `--github-read-only` and `--github-token`, and make the call again.
3. Report a service unreachable only with the command you ran and the output you saw.
4. When no instruction names a secret for that service, report that, with the output of `agent-secrets identity --source` and the instructions you read. Request no name you guessed.

When the client exits 1 with `no broker identity for this process: …`, report that line: each source in it says why it had nothing.
Read `skill://agent-secrets/references/identity.md` then, for what each reason means and its cure; read it too when a message names `agent-secrets-helper`, and on a person's own machine.

## Store a secret

Read `skill://agent-secrets/references/store-a-secret.md` before you create a secret or replace its value.
