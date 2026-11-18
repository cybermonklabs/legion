---
name: agent-secrets
description: Use before a command that needs a token, a key or a password; before you say that a service or an API is out of reach; and when an instruction names `agent-secrets`.
---

# agent-secrets

The secrets broker holds the credentials this deployment gives its agents. `agent-secrets` is its
client. It asks the broker for a secret and gives it to one command, in that command's environment.
Your session never holds the value, and nothing of it reaches your transcript.

## The one form

```sh
agent-secrets NAME... [--reason TEXT] [--wait DURATION] [--ttl DURATION] -- <command> [args...]
```

- Each granted secret reaches `<command>`'s environment under its own name, as the value or, for
  a secret behind the airlock, as a placeholder standing for it (below). Nothing runs unless
  every name is granted: a denial of one name denies the whole request, so the command never starts
  with some of its secrets missing (`cmdExec`, `packages/envoy/cmd/agent-secrets/main.go`).
- `--reason` is necessary wherever a person approves: a request that names a secret someone must
  decide, and carries none, is denied whole. Say what the credential is for, in a sentence the
  approver can decide on, and name in it the issue you work on, by its key (below). A request
  every name of which is granted at once reaches nobody and needs none.
- `agent-secrets --help` holds the rest: the flags that narrow a GitHub App token
  (`--github-owner`, `--github-repo`, `--github-read-only`), the flag that hands the token over
  alone (`--github-token`), and how long a grant lasts. With `--github-owner` and
  `--github-token`, the secret's name holds that one account's token, the string an
  `Authorization` header carries. Without `--github-token` the name holds the JSON object of
  installations, and a command that sends that object to GitHub reads `401`.
- The broker checks a narrowing before the approval request, so a narrowing it cannot serve costs
  nobody an approval. It refuses the request, nothing runs, and the status is 1, in five cases:

  - an account the secret's `github-owners` tag does not list,
  - a run that names no GitHub App secret,
  - a name behind the airlock (the bullet below),
  - a narrowing GitHub itself refuses, such as a repository the App's installation does not
    reach, or read access the App does not hold,
  - an account the App is not installed on, which the message says in the words `has no
    installation on`. Install the App on that account, or narrow to an account it is on.

  The message says which of the five it is. Two cases keep the older behaviour, where the
  refusal comes when the grant is read instead: a request every name of which is granted at
  once, which no person decides, and a request on which the broker reaches no verdict, for
  example because GitHub is out of reach or refuses the App itself, or because the check passes
  its own time bounds.
- A narrowing and the airlock do not go together. Ask for a narrowing in a run whose grant holds
  a secret behind the airlock (below) and nothing runs: where a person decides the request,
  the broker refuses the request, naming that secret, and where the policy grants every name
  at once, the same refusal comes when the grant is read. Ask for the GitHub App secret in a
  run of its own, with no airlocked name beside it.

## The command reads the value, your shell does not

This form sends an empty value, and the API answers as if you had sent no credential:

```sh
# wrong
agent-secrets LEGION_SLACK_BOT_TOKEN -- \
  sh -c "printf 'Authorization: Bearer %s' \"$LEGION_SLACK_BOT_TOKEN\" | curl -sS -H @- https://slack.com/api/auth.test"
```

Your own shell expands `$LEGION_SLACK_BOT_TOKEN` inside the double quotes before `agent-secrets`
runs, and in your shell that name is empty. Slack then answers `{"ok":false,"error":"not_authed"}`.
What the client hands over — the value, or the placeholder of a secret behind the airlock — exists
only in the environment of the command the client starts, so that command must read it itself.
Put the command in single quotes, inside `sh -c`, where the quotes keep the expansion for the
child, and give `curl` the header on standard input (`-H @-`), so the value is in no command's
arguments:

```sh
# right
agent-secrets LEGION_SLACK_BOT_TOKEN -- \
  sh -c 'printf "Authorization: Bearer %s" "$LEGION_SLACK_BOT_TOKEN" | curl -sS -H @- https://slack.com/api/auth.test'
```

## A secret behind the airlock

Your deployment can hold a secret's value back and give your command a stand-in. You see it in the
command's environment: the name holds `agent-secrets-placeholder-` and 32 letters and digits, not
the value. A length or a prefix probe there reads the placeholder, and proves nothing about the
real value.

A call to that secret's service goes through the broker's own proxy, which `agent-secrets` names
in the run's environment (`HTTPS_PROXY`, a `NO_PROXY` that keeps the secret's hosts on that proxy,
and a trust bundle). The proxy swaps the placeholder for the real value in the headers the
secret's rule names. Three things follow for the command you write:

- Make every call inside the `agent-secrets NAME -- command` run, with the environment that run
  gives it. A tool that keeps a token in a cache file across runs, or that makes its calls outside
  the run, stops working. Ask for the grant in the run that spends it.

- Pass no value you obtained some other way. The proxy refuses a request whose headers already
  carry the real value.

- The placeholder is that one grant's, and works only while the grant lives.

A refusal comes from the proxy, not from the service:

- An HTTP error naming the rule that refused it: a TLS name or `Host` that is not the host the
  command asked for, a method or a path outside the entry's `allow`, a path with a `.` or `..`
  segment, an `X-HTTP-Method-Override`, `X-HTTP-Method` or `X-Method-Override` header where the
  entry lists methods, a `CONNECT` or an `h2c` upgrade inside the proxied connection, plain
  `http://` to a host an entry binds, or a private address. gRPC clients work through the proxy,
  and a gRPC refusal shows the same text in its error message.

- `407` once the grant ends, on every call to any host. A call still running at that moment (a
  WebSocket, a gRPC stream, a long poll) is ended with the grant. A tunnel to a host no entry binds
  carries no value, so it runs to its idle limit instead.

- `503` when the broker could not read the grant at all. The grant has not ended and the call
  can be made again.

A retry fixes neither of the first two, and neither does another name. A `407` needs a new
grant. A rule's refusal needs a change to the rules file in the deployment's own repository,
which a person reviews. Report the refusal as you report a service out of reach: the command you
ran and the output you saw.

Two calls are not refused by the proxy. A call to a host no entry binds goes out unread, and a
placeholder in a header the entry does not name goes out as it is, so the service refuses the
placeholder and its own error is what you see.

## The value never reaches your transcript

Your transcript keeps everything a command prints.

- Never echo or print the value, and never run `curl -v`, `curl --trace` or a shell with `set -x`
  around it: each of those writes the header, and so the value, to your output.
- Check a plain value's shape by its length and its prefix alone:

  ```sh
  agent-secrets NAME --reason 'check the shape of this credential' -- sh -c 'echo "${#NAME}"; case "$NAME" in ghp_*) echo prefix-ok;; *) echo prefix-no;; esac'
  ```

- A value may be a JSON document, with the credential one level down or deeper: a GitHub App
  secret asked for without `--github-token` is the JSON object of installations, and its tokens
  are inside it. Check a JSON value's shape by its key paths and the length of each leaf, and
  never by printing a leaf. A probe that masks one key prints every other key's value, the
  nested ones too, so print no value at all, masked or not. These print the key paths, then each
  leaf's path with its length. For a value that is not JSON, jq exits 5 and writes
  `jq: error (at <unknown>): the value is not JSON` to stderr (jaq writes
  `Error: "the value is not JSON"`), and nothing of the value: `try … catch` keeps jq's own parse
  error out, which quotes the text (`… (while parsing '<the value>')`):

  ```sh
  agent-secrets NAME --reason 'check the shape of this credential' -- \
    jq -n 'env.NAME | try fromjson catch error("the value is not JSON") | [paths(type != "object" and type != "array") | join(".")]'
  agent-secrets NAME --reason 'check the shape of this credential' -- \
    jq -n 'env.NAME | try fromjson catch error("the value is not JSON") | [paths(type != "object" and type != "array") as $p | {path: ($p | join(".")), length: (getpath($p) | tostring | length)}]'
  ```

- Take a credential out of a JSON value inside the command, and pass it on there without
  printing it, with the header on curl's standard input so it is in no command's arguments.
  `jq -e` exits 1 when the path holds nothing, so the command stops rather than sending an empty
  credential:

  ```sh
  agent-secrets NAME --reason '…' -- sh -c '
    token=$(printf %s "$NAME" | jq -er ".path.to.token" 2>/dev/null) || { echo "no token at .path.to.token" >&2; exit 1; }
    printf "Authorization: Bearer %s" "$token" | curl -sS -H @- https://api.example.com/'
  ```

  For a GitHub App secret, `--github-owner <account> --github-token` hands the command that one
  account's token under the secret's name, with no JSON to take apart.
- For a secret behind the airlock, every one of these reads the placeholder and not the value.
- Compare two values only as equal or not equal. Never print, slice or diff their characters.

## Read the answer, not the status alone

Slack and most HTTP APIs answer `200` with `"ok": false`, so a call that failed can still exit 0.
Read the body. Send it to `jq` straight from `curl`, or through `printf '%s'`, and never through
`echo`: the pod's `sh` turns a `\n` inside a JSON string into a control character, which `jq` then
refuses.

## What the exit status means

After a grant the client writes one line to stderr, `agent-secrets: <names> from <source>`, and the
status is then the command's own. One case breaks that: a line `agent-secrets: exec <command>: …`
after it means the command could not start, and the status is 1.

Without the grant line the status is the client's own:

- **1** — a refusal or a failure that the message names: a name the broker does not serve
  (`UNKNOWN_SECRET`), a secret that waits for its first value (`PENDING_VALUE`), a broker it cannot
  reach, a machine that is not logged in to the broker, a request cancelled or expired,
  `REQUEST_PENDING`, a grant that is no longer live, a command that is not on `PATH`, which is
  refused before the broker is asked anything, so it reaches no approver, or no broker identity at
  all: `no broker identity for this process: key-dir: …; sign-in: …; helper: …; machine: …`, each
  source with why it had nothing.
- **2** — a usage error: no `AGENT_SECRETS_URL`, no `--`, no `NAME`, or a flag value the client
  refuses.
- **75** — the request still waits for a person, or for the model judge, when `--wait` runs out.
  The message names who decides it, where, and the `agent-secrets status <request_id>` that reads
  it. A status poll that did not reach the broker (a reset, a timeout, a 502 or 503) does not end
  the wait: stderr says once that the command keeps waiting, and polling goes on. If the wait runs
  out while polls still fail, the exit is 75 too, and the message names the last failure;
  `agent-secrets status <request_id>` says whether the request was decided, and running the same
  command again (the same names and `--ttl`, from the same session) collects a grant it already
  has, asking no one again.
- **77** — the request was denied. The approver denied it, the model judge refused it, the policy
  refused a name (a service's secret, or one whose requesters list does not name this session), or
  the request named a secret a person must approve and carried no reason, which the detail reads
  `<NAME>: a reason is necessary; ask again with --reason`. A denial by the approver or the judge
  is final: do not send the same request again.

The other statuses come from forms this skill does not use: 76 from `agent-secrets renew`, a
refusal that is permanent; 126 and 127 from `agent-secrets register --exec`, whose command could not
run or was not found; and 129, 130, 131 and 143 from a signal at the value prompt of
`agent-secrets secret create` or `secret set`. `agent-secrets --help` lists them all.

## Who decides a request

Each key says who decides a request for it. Run `agent-secrets secret show NAME` before you ask,
and read its `decides:` and `guidance:` lines:

- **nobody** — an agent-tier key your session's tags allow is granted at once.
- **the model judge** — the deployment's rules let the judge decide the key. Nobody else sees the
  request while the judge decides it. Write `--reason` so it meets the `guidance:` line: say what
  you will do, and include what the guidance asks for, such as the Dispatch issue you work on or a
  `dispatch://KEY/ask/<id>` answer it names. The judge grants the request, or refuses it for good:
  the command exits 77 with `the model judge refused this request: <reason>. This request will not
  be granted.` Tell whoever gave you the work what the reason says; do not send the same request
  again. If the judge cannot decide, the request goes to the key's person once, as an ordinary
  card, and you wait as for any other.
- **a person, on the broker page** — the key's person decides it, as below. An administrator's key
  is always a person's, whatever the rules say.

No request has to cite anything. A request for several keys is the judge's when the judge decides
every key in it that needs a decision; a key your session gets at once may stand beside them, and
the judge's grant releases it too. Otherwise a person decides the request whole. Ask for every key
the work needs, at the lifetime it needs, from one session: each request is decided once.

## Waiting, and a grant you already hold

- `--wait` waits 30 minutes by default. A session that must not block passes `--wait 0s`, reads the
  request id from the message, and reads the decision later with
  `agent-secrets status <request_id>`.
- A later run with the same names and the same `--ttl` uses the live grant again and asks nobody;
  while the first request still waits, such a run joins it. Any other run that would itself wait
  for a person and shares a name with a waiting request, with another `--ttl` or another set of
  names, is refused `REQUEST_PENDING`, which names the request already waiting.
- `agent-secrets identity` exits 0 when this session has a launcher's key or a helper session, and
  1 otherwise, and it asks the broker nothing. `agent-secrets identity --source` prints the source
  the client would take its broker identity from (`key-dir`, `sign-in`, `helper` or `machine`).
  `agent-secrets self` prints this session's live grants.
- `agent-secrets grant revoke <grant_id>` ends a grant given to this session's broker identity,
  from inside the session, before its lifetime runs out. The broker checks the enrollment, not the
  session: a grant of another enrollment is refused `was given to another broker identity
  (enrollment)`, and every session of a person who ran `agent-secrets login` on this machine signs
  as that sign-in's one enrollment, so each of them can end the others' grants.

## Where a name comes from, and how you may use it

The broker answers no list of the names a session may ask for. Its settings name only the keys its
model judge decides, so that `agent-secrets secret show NAME` can say who decides a request for a
name you already have. A name comes from your deployment instructions, your role prompt, a skill,
or the issue you work on. There is no list to browse and nothing to guess.

- Request a secret only to use it for the work in front of you. A request made to learn whether a
  secret exists is recorded against your session with the reason you signed, and it reaches a
  person's Inbox, or the model judge where the key is one it decides.
- `UNKNOWN_SECRET` and a denial are final. Do not ask again, and do not try a name of your own.
- `PENDING_VALUE` is not final: the secret exists, and its owner is storing its value with
  `agent-secrets secret create` now. No request was made and no approver asked. Ask again in a few
  minutes. After ten minutes the broker stops calling it pending and refuses the name
  `UNKNOWN_SECRET`: the create that made it deletes it when its prompt ends, at ten minutes or,
  where its owner was still typing then, when that entry ends; a create that was killed deletes
  nothing, and the secret stays in the store with no value. Tell its
  owner, who sees it as `versions: none` in `agent-secrets secret show NAME`. The owner frees the
  name with `gcloud secrets delete <id> --project <project number>` (`<id>` is the part after
  `secrets/` of the `secret_name` that `show` prints), or stores the value with
  `agent-secrets secret set NAME`. `agent-secrets secret delete NAME` only schedules the deletion
  30 days out, and until then the name stays taken, so the next `secret create` of it is refused.
- A secret that a service owns goes to that service's own pods and VMs and to nothing else:
  `Evaluate` in `packages/envoy/internal/broker/policy/policy.go` grants a service-owned secret at
  once when the session's own service is the secret's owner, and denies every other session, with
  no person asked. A shared agent secret, such as `LEGION_SLACK_BOT_TOKEN`, goes at once to every
  session, a pod's and a person's alike.
- The `agent-secrets secret` forms (`list`, `show`, `create`, `set`, `retag`, `delete`, `restore`)
  call the broker's store under whatever cloud credential the environment gives them, and the
  store's IAM decides what each may read and write.
- Slack's read methods go through `curl` under this client. Every post to Slack stays a `slack_post`
  call, which holds the reply to one thread, to 4,000 characters and to one send.

## Before you say that a service is out of reach

1. Run the call through `agent-secrets`, with the secret your instructions name.
2. A GitHub `404` under your session's own token is that token's reach, not your access: GitHub
   answers `404` for a repository a token cannot see. Ask the broker for the GitHub App secret
   your instructions name, narrowed to that repository with `--github-owner`, `--github-repo`,
   `--github-read-only` and `--github-token`, and make the call again under it.
3. Report a service unreachable only with the command you ran and the output you saw.
4. When no instruction names a secret for that service, report that instead, with what you checked:
   `agent-secrets identity --source`, and which instructions you read. Request no name you guessed.

## On a person's own machine

The client takes its broker identity from the first of four sources that has one, and needs no
registered agent session: a launcher's key directory, the person's own `agent-secrets login`, the
helper for a process inside a session it registered, and the machine's own cloud identity, which it
finds with no setting once the broker names its exchange audience. `agent-secrets identity
--source` names the source it would use. When the command exits 1 naming no broker identity, each
source says why it had nothing:

- **`helper: this process is in no session the helper … registered`.** The process inherited the
  helper's socket but is not in a session the helper registered. That alone stops nothing: the
  client goes on to the machine.
- **`machine: the broker at … names no exchange audience`.** The broker accepts no machine identity
  yet. The person signs in (`agent-secrets login`), or restarts the agent as a session:
  `agent-secrets register --wait 10 --exec -- <the agent's command>`.
- **`this machine is not logged in to the secrets broker`.** The helper holds no machine credential.
  `agent-secrets machine login-status` prints `issued` only while it holds one. The person logs the
  machine in, as `/legion/broker/guides/log-a-machine-in/` in the documentation site shows.

A stopped helper is not a missing one. Where the helper is installed, or `AGENT_SECRETS_HELPER_SOCK`
names its socket, and the socket is gone, the command exits 1 with
`agent-secrets-helper at … so the helper is stopped`, and it does not act as the machine instead.
Report that line; the person starts the helper (`systemctl --user start agent-secrets-helper`).

On `UNKNOWN_SECRET` you stop, as above. You may tell the secret's owner how to find the cause:
`agent-secrets secret list` and `agent-secrets secret show NAME`, under a cloud sign-in that may
read the store (`gcloud auth login` against a broker on Google Cloud, an `AWS_PROFILE` against one
on AWS).

A write runs under the credential of the first source the command finds, and the store's IAM
decides whether it lands. On Google Cloud the order is `CLOUDSDK_AUTH_ACCESS_TOKEN`,
`GOOGLE_APPLICATION_CREDENTIALS`, `CLOUDSDK_ACTIVE_CONFIG_NAME` or
`CLOUDSDK_CORE_ACCOUNT`, the well-known credentials file, the active gcloud account and last the
metadata server; on AWS it is the AWS SDK's own: access keys in `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY`, then `AWS_WEB_IDENTITY_TOKEN_FILE`, then `AWS_PROFILE`, then
`AWS_DEFAULT_PROFILE`, then the rest of the SDK's default chain. Every form prints the credential
it uses (`agent-secrets secret <form>: using …`) before it calls the store, and a store refusal
names it and how to choose another. A source that is present but broken stops the command; it
never falls through to the machine's identity.
An agent never chooses a gcloud configuration that holds a person's login: the call then runs as
that person, so the person runs such a command in their own shell. Where the box's active gcloud
configuration holds a person's login, the active gcloud account, which comes before the metadata
server, is that person's as well. To write as the machine, name the machine's own gcloud login: set
`CLOUDSDK_CORE_ACCOUNT` to the box's service account (on a Google VM `gcloud auth list` lists it),
or `CLOUDSDK_ACTIVE_CONFIG_NAME` to the configuration that holds it. `CLOUDSDK_CORE_ACCOUNT`
outranks the account of any configuration, so unset it before you choose a configuration. Or ask
the person to run the command.
`agent-secrets secret set NAME` replaces the value of a secret that exists. A new one is
`agent-secrets secret create NAME --owner me|shared|<email> --tier agent|human`. `--owner me` is
the email of the person the credential names, and is refused where it names none, as a token
minted for `cloud-platform` alone does: give `--owner <email>` there. Both read the value from
standard input or from a prompt with echo off, never from an argument. Type it at the prompt, so it
reaches no shell history. On Google Cloud, `create` makes the secret before it asks for the value,
so a store that refuses the create asks for none. Until the value is added, a failure or a signal
deletes that empty secret again, except where the store's answer leaves open whether it stored the
value: then `create` keeps the secret, exits 1, and names `agent-secrets secret show NAME` to check.

## A worked example: one page of a Slack thread

```sh
agent-secrets LEGION_SLACK_BOT_TOKEN --reason 'read the thread of a Slack mention' --wait 0s -- sh -c '
  printf "Authorization: Bearer %s" "$LEGION_SLACK_BOT_TOKEN" |
  curl -sS -G https://slack.com/api/conversations.replies \
    -H @- \
    --data-urlencode channel=<channel_id> \
    --data-urlencode ts=<thread_ts> \
    --data-urlencode limit=200'
```

It exits 0 and answers `"ok": true` with the messages of the thread. A `ts` that names no message answers
`{"ok":false,"error":"thread_not_found"}`, also at exit 0, which is why the body decides and not the
status. A thread read needs the app's `channels:history` scope in a public channel and
`groups:history` in a private one; without them Slack answers `missing_scope`, which only a person
in the Slack workspace can fix. `LEGION_SLACK_BOT_TOKEN` is a shared agent secret, so every session
with a broker identity is granted it; a process with no broker identity exits 1.

Paging: Slack recommends a `limit` of at most 200. While the answer's `has_more` is true, pass its
`response_metadata.next_cursor` as `cursor` to the next call. Each page repeats the thread's root
message, so drop repeats by `ts` and sort by `ts`.

A message from the client that this skill does not cover is in the broker's troubleshooting guide,
`/legion/broker/guides/troubleshooting/` in the documentation site.
