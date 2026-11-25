---
name: agent-secrets
description: Use when a command needs a token, a key or a password, when you store a secret for agents, before you say that a service or an API is out of reach, and when an instruction names `agent-secrets`.
---

# agent-secrets

A command gets a secret in its environment, and your shell and your transcript never hold the value. `agent-secrets`, the secrets broker's client, asks the broker for it and gives it to that one command.
**Done when:** you read the body of each answer, not its exit status alone, and you report a service unreachable only with the command you ran and the output you saw.
`agent-secrets --help` lists every form, flag, environment variable and exit code.
Every message the client prints is explained at https://cybermonklabs.github.io/legion/broker/guides/troubleshooting/.

## The one form

```sh
agent-secrets NAME... [--reason 'TEXT'] [--wait DURATION] [--ttl DURATION] -- <command> [args...]
```

- Each granted secret reaches `<command>`'s environment under its own name. Nothing runs unless every name is granted.
- **Who decides a request for a key:** `agent-secrets secret show NAME` prints it on its `decides:` line, beside the `guidance:` a model judge decides by. The judge grants a request or refuses it for good (77). One it cannot decide goes to the key's person.
- **Pass `--reason` wherever a person or the model judge decides:** such a request that carries none is denied whole. A request every name of which is granted at once needs none.
- **Write `--reason` as one sentence the approver can decide on.** Name in it the issue you work on. Meet the key's `guidance:` in it.
- A session that must not block passes `--wait 0s` and reads the decision later with `agent-secrets status <request_id>`.
- The same command run again (the same names, `--ttl` and `--target`) uses the live grant or joins the waiting request, and asks nobody again.
- A request that waits for a person is already in that person's Dispatch Inbox, with a link to the broker's page that decides it (https://cybermonklabs.github.io/legion/broker/concepts/#approvals).
  Open no Dispatch ask, comment or message that asks them to approve it: each one is a duplicate of that Inbox row. Continue your work, and read the decision with `agent-secrets status <request_id>`.
- A GitHub App credential is a profile. `--target ACCOUNT`, or `--target ACCOUNT/REPOSITORY` for each repository of one account, narrows it in a run of that name alone.
- A profile of value `token` puts one token in the name.
- Any other profile puts the JSON object of installations in the name. GitHub answers that object `401`.
- A line `the profile NAME left out …` names permissions the token lacks. A call that needs them gets `403`.

## The command reads the value, your shell does not

Your own shell expands `"$NAME"` inside double quotes before `agent-secrets` runs, and there the name is empty, so the API answers as if you sent no credential.
Put the command in single quotes inside `sh -c`. Give `curl` the header on standard input (`-H @-`), so the value is in no command's arguments:

```sh
agent-secrets LEGION_SLACK_BOT_TOKEN --reason 'check that the Slack token works' -- \
  sh -c 'printf "Authorization: Bearer %s" "$LEGION_SLACK_BOT_TOKEN" | curl -sS -H @- https://slack.com/api/auth.test'
```

In a pod, `gh` refuses `GH_TOKEN`, so call GitHub with `curl` that way: `agent-secrets GITHUB_READ --target ACCOUNT -- sh -c '…"$GITHUB_READ" | curl -sS -H @- https://api.github.com/…'`.

Read the body, not the exit status alone: Slack and many HTTP APIs answer `200` with `"ok": false`.
Send the body to `jq` straight from `curl` or through `printf '%s'`, never through `echo`, which can turn a `\n` inside a JSON string into a control character.
Slack's read methods go through `curl` under this client. Post to Slack only with a `slack_post` call, which holds the reply to one thread, to 4,000 characters and to one send.

## The value never reaches your transcript

Your transcript keeps everything a command prints.

- Never print the value, and never run `curl -v`, `curl --trace` or `set -x` around it: each of them writes the header.
- Check a plain value's shape by its length and its prefix alone:

  ```sh
  agent-secrets NAME --reason 'check the shape of this credential' -- sh -c 'echo "${#NAME}"; case "$NAME" in ghp_*) echo prefix-ok;; *) echo prefix-no;; esac'
  ```

- Check a JSON value by its key paths and the length of each leaf, never by printing a leaf, masked or not. `try … catch` keeps jq's own parse error, which quotes the value, out of the output:

  ```sh
  agent-secrets NAME --reason 'check the shape of this credential' -- \
    jq -n 'env.NAME | try fromjson catch error("the value is not JSON") | [paths(type != "object" and type != "array") as $p | {path: ($p | join(".")), length: (getpath($p) | tostring | length)}]'
  ```

- Take a credential out of a JSON value inside the command that uses it. `jq -e` stops the command rather than send an empty credential:

  ```sh
  agent-secrets NAME --reason '…' -- sh -c '
    token=$(printf %s "$NAME" | jq -er ".path.to.token" 2>/dev/null) || { echo "no token at .path.to.token" >&2; exit 1; }
    printf "Authorization: Bearer %s" "$token" | curl -sS -H @- https://api.example.com/'
  ```

- Compare two values only as equal or not equal.

## A secret behind the airlock

A deployment can keep a secret's value back. The name then holds `agent-secrets-placeholder-` and 32 letters and digits. The broker's proxy, which the run's `HTTPS_PROXY` names, puts the real value into the call to the secret's service. A shape check reads only the placeholder.

- Make every call inside the `agent-secrets NAME -- <command>` run. A tool that caches a token across runs, or calls outside the run, stops working.

| Proxy answer | Means | Do |
| --- | --- | --- |
| A refusal | It names why it refused the call; a retry does not fix it | Change what the command sent as the refusal says, or report it with the command and its output |
| `407` | The grant ended | Ask again |
| `503` | The broker could not read the grant | Try the call again |

## What the exit status means

After a grant the client writes `agent-secrets: <names> from <source>` to stderr. The status is then the command's own. If a line `agent-secrets: exec <command>: …` follows the grant line, the status is 1. Without the grant line the status is the client's: read `skill://agent-secrets/references/exit-status.md` for what each status means and what to do.

## Where a name comes from

The broker tells a session no names. A name comes from your instructions, your role prompt, a skill or the issue you work on; there is no list to browse.
Request a secret only to use it for the work in front of you: every request is recorded against your session with its reason, and can reach a person.

## Before you say that a service is out of reach

1. **Run the call through `agent-secrets`**, with the secret your instructions name.
2. **On a GitHub `404` under your session's own token, ask for the GitHub profile your instructions name**, with `--target` for the call's account or repositories, and call again.
   It is that token's reach, not your access: GitHub answers `404` for a repository a token cannot see.
3. **Report a service unreachable only with the command you ran and the output you saw.**
4. **When no instruction names a secret for that service, report that**, with the output of `agent-secrets identity --source` and the instructions you read. Request no name you guessed.

When the client exits 1 with `no broker identity for this process: …`, report that line: each source in it says why it had nothing.
On a person's own machine, the person signs in with `agent-secrets login` or logs the machine in with `agent-secrets machine login`.

## Store a secret

Read `skill://agent-secrets/references/store-a-secret.md` before you create a secret or replace its value.
