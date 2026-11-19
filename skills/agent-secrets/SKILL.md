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
agent-secrets NAME... --reason 'TEXT' [--wait DURATION] [--ttl DURATION] -- <command> [args...]
```

- Each granted secret reaches `<command>`'s environment under its own name. Nothing runs unless every name is granted.
- Always pass `--reason`: a request a person must decide that carries none is denied whole. Write one sentence the approver can decide on, and name in it the issue you work on.
- Each key says who decides a request for it: `agent-secrets secret show NAME` prints it on its `decides:` line, beside the `guidance:` a model judge decides by. Meet that guidance in `--reason`. The judge grants a request or refuses it for good (77); one it cannot decide goes to the key's person.
- A session that must not block passes `--wait 0s` and reads the decision later with `agent-secrets status <request_id>`.
  The same command run again (the same names and `--ttl`) uses the live grant or joins the waiting request, and asks nobody again.
- For a GitHub App secret, `--github-owner <account> --github-token` gives the command that one account's token, the string an `Authorization` header carries; `--github-repo` and `--github-read-only` narrow it further.
  Without `--github-token` the name holds the JSON object of installations, and GitHub answers it `401`.

## The command reads the value, your shell does not

Your own shell expands `"$NAME"` inside double quotes before `agent-secrets` runs, and there the name is empty, so the API answers as if you sent no credential.
Put the command in single quotes inside `sh -c`, and give `curl` the header on standard input (`-H @-`), so the value is in no command's arguments:

```sh
agent-secrets LEGION_SLACK_BOT_TOKEN --reason 'check that the Slack token works' -- \
  sh -c 'printf "Authorization: Bearer %s" "$LEGION_SLACK_BOT_TOKEN" | curl -sS -H @- https://slack.com/api/auth.test'
```

Read the body, not the exit status alone: Slack and many HTTP APIs answer `200` with `"ok": false`.
Send the body to `jq` straight from `curl` or through `printf '%s'`, never through `echo`, which can turn a `\n` inside a JSON string into a control character.

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

A deployment can keep a secret's value back. The name then holds `agent-secrets-placeholder-` and 32 letters and digits, and the broker's proxy, which the run's `HTTPS_PROXY` names, puts the real value into the call to the secret's service. A shape check reads only the placeholder.

- Make every call inside the `agent-secrets NAME -- <command>` run. A tool that caches a token across runs, or calls outside the run, stops working.
- A proxy refusal names the rule that refused it, and a retry does not fix it: report it with the command and its output. `407` means the grant ended: ask again. `503` means the broker could not read the grant: try the call again.
- A GitHub narrowing and an airlocked name cannot share a run: ask for the GitHub App secret in a run of its own.

## What the exit status means

After a grant the client writes `agent-secrets: <names> from <source>` to stderr, and the status is then the command's own. Without that line the status is the client's:

- **1**: a refusal or a failure that the message names.
  `UNKNOWN_SECRET` is final: do not ask again, and do not try a name of your own.
  `PENDING_VALUE` means its owner is storing the value now: ask again in a few minutes.
  `REQUEST_PENDING` names a waiting request of this session that shares a name with this one.
- **2**: a usage error, such as no `AGENT_SECRETS_URL` or no `--`.
- **75**: the request is still undecided when `--wait` runs out, and nothing ran. The message gives the `agent-secrets status <request_id>` that reads it.
- **77**: the request was denied; the message gives the broker's reason when it recorded one. Final: do not send the same request again.

## Where a name comes from

The broker tells a session no names. A name comes from your instructions, your role prompt, a skill or the issue you work on; there is no list to browse.
Request a secret only to use it for the work in front of you: every request is recorded against your session with its reason, and can reach a person.

## Before you say that a service is out of reach

1. Run the call through `agent-secrets`, with the secret your instructions name.
2. A GitHub `404` under your session's own token is that token's reach, not your access: GitHub answers `404` for a repository a token cannot see.
   Ask for the GitHub App secret your instructions name, narrowed with `--github-owner`, `--github-repo`, `--github-read-only` and `--github-token`, and make the call again.
3. Report a service unreachable only with the command you ran and the output you saw.
4. When no instruction names a secret for that service, report that, with the output of `agent-secrets identity --source` and the instructions you read. Request no name you guessed.

When the client exits 1 with `no broker identity for this process: …`, report that line: each source in it says why it had nothing.
On a person's own machine, the person signs in with `agent-secrets login` or logs the machine in with `agent-secrets machine login`.

## Store a secret

`agent-secrets secret create NAME --owner me|shared|<email> --tier agent|human` creates one, and `agent-secrets secret set NAME` replaces the value of one that exists.
Both read the value from standard input or from a prompt with echo off, never from an argument: type it at the prompt, so it reaches no shell history.
They run under the cloud credential the environment gives them and print it first (`agent-secrets secret <form>: using …`).
Never run one under a gcloud configuration or account that holds a person's login, because the write then runs as that person: ask the person to run it.
Tags, deletion and restore: https://cybermonklabs.github.io/legion/broker/guides/manage-a-secret/.
