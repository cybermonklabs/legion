---
title: Manage a secret
description: Create an agent secret, change its value or tags, and delete or restore it with agent-secrets secret under the cloud credential your environment gives it.
sidebar:
  order: 9
---

An agent secret is a secret in the broker's own store - AWS Secrets Manager or GCP Secret
Manager, whichever the broker's deployment uses - under the broker's namespace, encrypted with
its key, and tagged with owner, tier, optional type and optional max grant
([Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret)).
`agent-secrets secret` manages them. It reads and writes the store itself, under whatever cloud
credential its environment gives it, so that cloud's own IAM decides what it may do; the broker
never writes a secret. After every write the CLI asks the broker to reread that one secret, and the
broker that answers tells
every other broker process serving the same database to reread it too, so the change is served
across the whole deployment within seconds rather than at each process's next read of the
namespace. The examples on this page are AWS's; GCP's
backend differs only where this page calls it out.

## Before you start

You need three things:

- **`agent-secrets` on your `PATH`.** Each `legion-envoy-v*` GitHub release ships it for Linux in
  `agent-secrets-amd64.tar.gz` and `agent-secrets-arm64.tar.gz`, beside the helper, and for macOS
  in `agent-secrets-darwin-amd64.tar.gz` and `agent-secrets-darwin-arm64.tar.gz`, which hold the
  CLI alone. Managing secrets needs no helper, no machine login and no agent session.
- **`AGENT_SECRETS_URL`, the broker's address.** The CLI asks the broker for the namespace, the
  agent-secrets key, and that key's store and account/project (`GET /v1/settings`), so you
  configure none of them.
- **A cloud credential that may reach the broker's account or project.** The CLI takes it from
  the first source that is present, a person's or a machine's, for every form, and refuses none
  for what kind of identity it is: the store's IAM decides what each may read and write. It is not
  opinionated about where the credential comes from; it reads it from the environment, as the AWS
  CLI, boto3 and gcloud do. AWS: the AWS SDK's own order, in the broker's account: access keys in
  `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, then `AWS_WEB_IDENTITY_TOKEN_FILE`, then
  `AWS_PROFILE`, then `AWS_DEFAULT_PROFILE`, then the rest of the SDK's default chain. So with
  keys exported, `AWS_PROFILE` chooses nothing until you unset them, as in the AWS CLI, and a
  profile variable that names a profile your AWS config does not hold stops the command. GCP: the
  six sources below.

```sh
export AGENT_SECRETS_URL=https://secrets.internal.example
aws sso login --profile <profile>              # AWS
export AWS_PROFILE=<profile>
gcloud auth login                              # GCP, once
export CLOUDSDK_ACTIVE_CONFIG_NAME=<config>    # GCP, to name its gcloud configuration
```

On GCP a store command takes its credential from the first of these that is present:

1. `CLOUDSDK_AUTH_ACCESS_TOKEN`: an access token lent to the command.
2. `GOOGLE_APPLICATION_CREDENTIALS`: the credentials file it names.
3. `CLOUDSDK_ACTIVE_CONFIG_NAME` or `CLOUDSDK_CORE_ACCOUNT`: the gcloud configuration
   (`gcloud config configurations list` lists them) or the gcloud login it names.
4. The well-known credentials file, `application_default_credentials.json` in gcloud's own
   configuration directory (`CLOUDSDK_CONFIG`, else `~/.config/gcloud`), which
   `gcloud auth application-default login` writes.
5. The active gcloud account, where `gcloud` is on your `PATH` and holds a login.
6. The metadata server, on a Google Cloud VM or pod: the machine's own service account.

There is no `--profile` flag on either backend; a form given one is a usage error that names
these variables. A source that is absent is skipped. A source that is present but fails stops the
command with its error, and no later source is tried: a file that does not parse, a refresh gcloud
cannot make, an account gcloud holds no login for, a configuration that names no account. A
refusal of the store's is never tried again under another credential. With no source present, the
command names each of the six and why it had nothing.

So a login you choose, with a variable, always comes before the machine's own identity. On a
Google VM whose active gcloud configuration is the machine's own (here `box`), name the
configuration that holds your own `gcloud auth login` (here `ada`):

```sh
CLOUDSDK_ACTIVE_CONFIG_NAME=ada agent-secrets secret create DEMO_DEPLOY_TOKEN --owner me --tier human
```

The CLI asks the first `gcloud` on your `PATH` for a login's token (`gcloud auth list`,
`gcloud config list`, then `gcloud auth print-access-token`), so gcloud keeps it fresh, and its
own error says when you must run `gcloud auth login` again; the CLI reads none of gcloud's
credential files. gcloud runs in your own environment and reads `CLOUDSDK_ACTIVE_CONFIG_NAME` and
`CLOUDSDK_CORE_ACCOUNT` from it, so impersonation, an access token file and a credential file
override apply as gcloud's own settings say, as they do for `gcloud` itself. It always runs with
prompts and log files off and without `CLOUDSDK_AUTH_ACCESS_TOKEN`. The account of a configuration
you chose is the one that configuration names: one that names none is refused, never run as
whatever other login gcloud holds.

The login of a gcloud source is the one gcloud itself would use: the one `CLOUDSDK_CORE_ACCOUNT`
names, else the one a chosen configuration names, else the active one, whatever kind of account it
is. An account `CLOUDSDK_CORE_ACCOUNT` names that gcloud holds no login for is refused, never
replaced by another login. A command the CLI prints for running again keeps the variable that chose
the source (`GOOGLE_APPLICATION_CREDENTIALS`, `CLOUDSDK_ACTIVE_CONFIG_NAME`, `CLOUDSDK_CORE_ACCOUNT`):

```sh
CLOUDSDK_CORE_ACCOUNT=ada@example.com agent-secrets secret create DEMO_DEPLOY_TOKEN --owner me --tier agent
```

Every form prints one line on standard error, before its first call to the store, that names the
credential it uses: where it came from, and whom it acts as.

```console
$ CLOUDSDK_ACTIVE_CONFIG_NAME=ada agent-secrets secret show DEMO_DEPLOY_TOKEN
agent-secrets secret show: using the gcloud configuration ada (CLOUDSDK_ACTIVE_CONFIG_NAME): ada@example.com
```

On GCP the source is one of `the access token in CLOUDSDK_AUTH_ACCESS_TOKEN`,
`the credentials file GOOGLE_APPLICATION_CREDENTIALS names`,
`the gcloud configuration NAME (CLOUDSDK_ACTIVE_CONFIG_NAME)`,
`the gcloud account (CLOUDSDK_CORE_ACCOUNT)`,
`the well-known credentials file (application_default_credentials.json in the directory CLOUDSDK_CONFIG names)`
(or `in $HOME/.config/gcloud` where `CLOUDSDK_CONFIG` is unset),
`the active gcloud account (gcloud configuration NAME)` and `the metadata server`. No source is
named by a path: a credentials file's path is where a person who pasted the credential itself
puts it. The account
follows, with ` impersonating SA` where gcloud impersonates one, or `principal unknown` where the
credential does not say whose it is: a lent token, an `authorized_user` file, and a gcloud login
whose token comes from an access token file or a credential file override. The CLI asks for no
email scope to learn it. Whom a credential acts as is read best effort, and a read that is slow or
fails never stops the command: the line then names the metadata server's credential
`this machine's or this pod's identity`, and a gcloud login whose settings `gcloud config list`
did not read in 5 s `ACCOUNT (principal unknown: gcloud's auth settings could not be read)`. The
metadata server counts only when it yields a token in an answer that says it is Google's. On AWS
the line reads `using the AWS profile NAME (AWS_PROFILE): ARN`,
`using the AWS profile NAME (AWS_DEFAULT_PROFILE): ARN`,
`using the access keys in AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY: ARN`,
`using the web identity token file AWS_WEB_IDENTITY_TOKEN_FILE names: ARN`, or
`using the AWS SDK's default credential chain: ARN`; where keys or a token file are set beside a
profile variable, the line adds that they outrank it. Where the profile holds no credential of
its own, the SDK goes on to a container's credentials endpoint or the machine's instance role,
and the line adds `, which holds no credential, so the container credentials endpoint VARIABLE
names signed in` or `…, so this machine's instance role signed in`.

When you have made your writes, remove a sign-in you made for them from the machine, so nothing
else that runs as your user there can use it:

```sh
gcloud auth revoke '<your email>'
```

## Create a secret

```console
$ agent-secrets secret create DEMO_DEPLOY_TOKEN --owner me --tier agent
agent-secrets secret create: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
Value for DEMO_DEPLOY_TOKEN:
created DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1 (owner=ada@example.com, tier=agent)
broker: serving DEMO_DEPLOY_TOKEN from AWS Secrets Manager account 111122223333 in us-east-1
```

Every line names the store it wrote to or read, so a secret is never taken for one of the same
name in another store.

Every form below runs under the same credential, on GCP as on AWS, and a write the store's IAM
refuses names the identity it refused ([Which sign-in may do what](#which-sign-in-may-do-what)).

- **The name** is the one agents ask for, and the environment variable their command gets it in:
  uppercase letters, digits and single underscores, starting with a letter. In Secrets Manager it
  is `<namespace>demo-deploy-token`. Any other name is refused at once (exit 2).
- **`--owner me`** writes the email of the person your credential names, in lowercase: your
  Identity Center session name (AWS) or the email Google's userinfo endpoint answers for the token
  the command runs under (GCP). Where the credential names no person - a machine's role, a service
  account, or a token minted without an email scope, which Google will not name - the command
  refuses `me` before it reads the value, and **`--owner <email>`** names the owner outright,
  written in lowercase whatever case you give it. Either must be an email the broker accepts, or
  the command refuses before writing. The broker matches it against the email a person signs in to
  the broker's approval page with (and to Dispatch, for a machine login), so `me`
  names you where that session name or Google account is the same email. **`--owner shared`**
  makes the secret everyone's. GCP stores owner and tier as the secret's **annotations**, never
  labels, so they never collide with a label someone else applies.
- **`--tier agent`** lets the owner's own sessions use it without asking, and **`--tier human`**
  sends every use to a person for approval. Both flags are required.
- **`--type`** is optional. Use `github-app` for a GitHub App key, `google-login` for an
  authorized-user Google login, and `google-service-account` for one Google service account the
  broker mints an access token as, whose stored value names that account:
  `{"service_account": "<id>@<project>.iam.gserviceaccount.com"}`. That last one is an
  administrator's to write and to set a new value for, not a self-service step, even for the
  person who owns it: which account the broker mints for is the deployment's choice, bound to the
  secret's name in `BROKER_SERVICE_ACCOUNT_SECRETS`, and a secret the deployment has bound to
  nothing releases no token at all, however good its value. Ask whoever runs your deployment to
  create it and bind it.
  Use `none`, or omit the flag, for a stored secret. A type needs a broker that reads the `type`
  tag: against an earlier one, whose settings name no `secret_type_tag`, `create --type`, `set`
  on a type secret and a retag that leaves a type exit 1 before they write anything, naming the
  release to move the broker to, since such a broker would serve the secret's stored key as its
  value. A type also writes `mint-service-account: type-secret`, which only the brokers from
  legion-envoy-v13.19.0 to the release before v16.0.0 read, and refuse; it stops no later broker
  from before the `type` tag. So once a secret carries a type, the first release that reads
  `type` is the broker's rollback floor: a rollback past it serves the secret's stored key.
- **`--github-owners`** is required with `--type github-app`, and refused with any other type. It
  lists the GitHub accounts the broker mints installation tokens for, separated by spaces, so the
  shell quotes it: `--github-owners "acme-org ada"`. A comma is refused before anything is written.
- **`--requesters`** is optional. It lists who may ask for this secret at all — people by their
  lowercase sign-in email, services by the name the deployment registers them under — separated
  by spaces, so the shell quotes it: `--requesters "ada@example.com legion-daemon"`. Nobody else
  may ask, whatever the owner and tier allow. The list bounds no grant by itself: an ordinary
  secret keeps whatever `--max-grant` you give it, or none, so your own mail and Drive logins
  stay at their tier and are capped by nothing. A `google-service-account` secret is served to
  the one person who owns it, so the administrator who writes it gives it that person's own
  address: `--owner <their email> --requesters <their email>`. With `--tier human` it may name
  registered services beside that owner, so a service's pod can ask and the owner approves each
  request; with `--tier agent` it may not, and neither may a secret the deployment binds to a
  `dev-<name>` account, whose token is that one person's own. A comma, an email with a
  capital letter and a list that names nobody are refused
  before anything is written. Whether an entry naming a service is one your deployment registers
  is the broker's to say
  ([what the broker answers after a write](#what-the-broker-answers-after-a-write)).
- **`--scopes`** is optional, and read with `--type google-service-account` alone; with any other
  type it is refused before anything is written. It lists the OAuth scopes the minted token
  carries, each the part of a Google scope after `https://www.googleapis.com/auth/`, separated by
  spaces, so the shell quotes it: `--scopes "cloud-platform bigquery"`. Without it the token
  carries `cloud-platform` alone. The short names keep the list inside an AWS tag value's 256
  characters. A comma, a whole scope URL, a capital letter, and a scope named twice are each
  refused before anything is written.
- **`--admin true`** is optional. It marks a secret whose grant is an administrator's, which the
  broker cannot read off the type: an admin console password and a personal API key are both
  stored strings. Such a secret is `--tier human`, names its `--requesters`, and takes a
  `--max-grant` of an hour or less, so a person approves every release, only someone it names may
  ask, and the grant is short enough to review while it is live. A person approves every release
  of one even where a deployment runs a model judge: the judge refuses an administrator's
  secret, so no airlock rules entry can move one of its requests off the person who approves it
  ([The model judge](/legion/broker/concepts/#the-model-judge)). Any value but `true` is refused.
- **`--project`** is optional. It names the one Dispatch project the secret belongs to, by its
  key as Dispatch writes it: `--project PRIV`. A secret of a **private** project goes only to a
  session or pod whose credential names that project; every other one is denied
  `project-not-named`, whatever its owner and tier allow. A secret of a public project is decided
  by owner and tier as if it carried no project. A session names its projects when it starts,
  with `AGENT_SECRETS_PROJECTS` ([name a session's
  projects](/legion/broker/concepts/#a-private-projects-secrets)). Anyone who may write the
  store's prefix may set the tag, and that person can already read every value there; which of a
  project's secrets belong to it is the project owner's call. A key in lower case, two keys, and
  the word `none` are refused before anything is written. Only a broker that can ask Dispatch
  whether a project is private serves a secret carrying the tag: any other refuses it
  `project-unchecked`, which the create prints with its reason and exits 1 for. Beside `--project`
  or `--project-channels`, the CLI also writes `rollback-guard:project-tags` into the secret's
  `requesters` tag, after its list, so that a broker from before the project tags refuses the
  secret rather than serve it to every session; `--requesters none` keeps it, it goes with the
  last of the two tags, and `show` prints `rollback_guard: yes` and the list without it
  ([Rolling back past the project tags](/legion/broker/operate/#rolling-back-past-the-project-tags)).
- **`--project-channels slack`** is optional, and `slack` is its one value. It marks a Slack bot
  token, such as `LEGION_SLACK_BOT_TOKEN`, whose use a private project's owner limits. A session or
  pod whose credential names a private Dispatch project spends such a secret only through the
  airlock ([The airlock](/legion/broker/concepts/#the-airlock)), and only to post with
  `POST /api/chat.postMessage`, carrying `channel`, `thread_ts` and `text`, to a channel id the
  owner of every private project it names allows; the proxy refuses every other call
  `CHANNEL_NOT_ALLOWED`. Where the secret has no airlock entry, such a credential is denied it
  `project-channels-need-airlock`. A credential that names no private project gets the secret as
  before. Any other value, and on `create` the word `none`, is refused before anything is written.
- **`--max-grant`** is optional, except with `--admin true` and with
  `--type google-service-account`, where each needs it, of an hour or less. It is the longest
  `--ttl` a request for this secret may use.
  It must be a duration of at least one second.
- **`--default-grant session`** is optional. A request with no `--ttl` for secrets that all carry it
  gets a grant that lasts until its session ends, instead of the default: 15 minutes when any
  secret the request names is human tier, and 12 hours otherwise.
- **The value.** At a terminal the CLI reads one hidden line. Enter or Ctrl-D ends it; Backspace,
  Ctrl-W and the terminal's kill-line character edit it. An unhandled control byte, including
  Tab, Escape, Ctrl-S, Ctrl-Q, and Ctrl-C, Ctrl-\\ or Ctrl-Z inside a bracketed paste, refuses the
  whole value: the CLI never trims a value to store the rest. The
  prompt keeps discarding through Enter or Ctrl-D and the paste's end before reporting the error,
  which names the byte by its number and its name, says whether it came at the start of the
  value, at its end, inside it or as the whole value, and, for a Tab at either end, that a paste
  often adds one there, as a copy out of a web page's table does. It then says to enter the value
  again without the byte, or, where the byte is part of the value, names the pipe command. Anywhere
  else the CLI reads standard input to its end. The CLI then reads what it stores by the same rule
  the broker spends a value under: the whole trailing run of carriage returns and line feeds goes
  only where what is left is not empty and holds no carriage return and no line feed, so a
  one-line credential loses the ending a shell or an editor left behind and a PEM document keeps
  the one that ends it. Where that rule removes nothing, one final line feed goes, so `echo`
  and a file that ends in a newline store the value without one. `secret set` reads the secret's
  own type tag first, so it checks the value as that type's minter will. Empty values and
  invalid UTF-8 are refused (exit 2) and write nothing. A space is never trimmed: a value that
  starts or ends with one, typed or piped, is stored as given, and the command then prints one
  line on standard error saying the value starts or ends with a space and was stored as given.
  That line names the secret and never the value. A space inside the value prints nothing.
- **The value of a type.** `create --type` checks the value as the broker's minter for that type
  reads it, before it writes, and `set` does the same under the type tag the secret holds. A value
  that minter could not mint from is refused (exit 2) with the minter's own reason, such as
  `GitHub App secret needs an app_id and private_key`, and nothing is written. The broker reads
  every type secret's value with the same decoder, so when a value stored some other way, such as
  with `gcloud` or the AWS console, is one the decoder refuses, the broker refuses the secret as
  `value-malformed` ([the value read](/legion/broker/operate/#the-value-read)). `set` on a secret
  whose type tag names no type the broker mints is refused before it asks for the value.

**Keeping a value's final line ending.** The rule removes nothing from a value that holds a line
ending inside it, a PEM key or a JSON file for one; the CLI still removes one final line feed
there, so pipe one more line ending than the value needs. For a one-line value the rule trims, no
number of piped endings survives.

The CLI checks your sign-in before it asks for the value, so a refused sign-in never has you type a
secret for nothing. On GCP it then makes the secret, on the broker's key, with its tags and no
value yet, and only then asks for the value, so a store that refuses the create - a sign-in IAM
does not let create there, or a name already taken - ends the command before the prompt. The
create labels the secret `agent-secrets-create` with a random value of its own. The label stays on
the secret, so a create needs no `secretmanager.secrets.update`; the broker reads only whether a
secret carries it, never its value. Once the secret is made, the CLI asks IAM whether your sign-in
may add its value (`secretmanager.versions.add`, through `TestIamPermissions` on the new secret).
Where IAM says it may not, the CLI deletes the empty secret and exits 1 before the prompt, naming
the permission and your sign-in; where IAM cannot be asked, it goes on, and the add itself is the
check.

Where the store's answer to the create itself leaves open whether it made the secret - a lost
answer, an outage - the CLI reads the secret back before it exits 1, and touches only a secret
that carries its own label value. A read that finds no secret says the store did not show one then,
that a create the store has not finished may still land, and names `agent-secrets secret show NAME`
and the label value to look for. A secret without that value, there before or made by another
create of the name, is left as it is; one with it and no version is deleted again, and the broker
asked to reread the name; one with it and a version is left. A read that fails names the label
value and the `gcloud secrets describe` command that shows a secret's labels, and a read, list or
delete that fails names the `gcloud secrets delete` command that clears this create's empty
secret.

Every cleanup checks the creation label and lists versions, even when this form never added a
value, since another writer may have filled or replaced the secret during the prompt; a failed
read or list keeps the secret, and the delete carries the read's `etag`, so a secret replaced
since is not deleted. One case remains: Secret Manager offers no delete conditional on a secret
holding no version, so a version another writer adds between the version list and the delete is
deleted with the secret. A cleanup that deletes the empty secret says so: the failure the command
prints ends with `the empty secret … is deleted, so nothing stays in the store`. Where the store's
answer to the delete leaves open whether it deleted the secret - a lost answer, a deadline, an
outage - the CLI reads the secret back: none found is the delete done, said as above; the secret
still there has its versions listed again, since another writer may have added one meanwhile, and
stays: with no version it is said to stay, with the `gcloud secrets delete` command that clears it;
with a version it may hold a value, so no delete is named and the broker rereads the name; and a
list that fails names that command only for `versions: none`. A read back that fails says the store
may still hold it, naming `agent-secrets secret show NAME` and that command. Only a delete the store
refused, or a read back that found the secret, is said to leave it in the store.

While the secret waits for its value, for ten minutes from its making, the broker serves it to
nobody, logs that it waits, and refuses a request for it `PENDING_VALUE`, which says the secret
waits for its first value and to ask again once its owner stores it. On GCP the CLI reads the value
for the same ten minutes, at the prompt or from a pipe, and the prompt names the time the value must
come by. When no whole value has come by then, through the pipe or with nothing typed or pasted at
the prompt, it ends the read, deletes the empty secret, asks the broker to reread the name, and
exits 1, saying to run the command again. Where the ten minutes run out while you type or paste,
the prompt says so at once and keeps reading until you press Enter, and a paste until its end (or
until it falls quiet for ten seconds), so no part of the value is echoed or reaches your shell; a
value that ends only after the ten minutes is not stored. The create deletes the empty secret when
that entry ends: at Enter, or at a signal that ends the process, such as a hang-up, or Ctrl-C typed
outside a paste. The notice says the empty secret stays in the store until then and names what ends
the entry from where you are: Enter, with Ctrl-C deleting it at once where Ctrl-C ends the
process; inside a paste, where Ctrl-C is pasted text, finishing the paste first; and where Ctrl-C
does nothing (an interrupt the shell ignores, or a terminal that is not the prompt's own), ending
the process by closing the terminal or sending it SIGTERM. A prompt left waiting in the background
for `fg` gives up at the ten minutes. On AWS, where the create makes nothing before it has the
value, the read has no such limit. After the ten minutes the broker refuses a secret still without
a value `no-current-value`, and the broker deletes nothing: the create that made it deletes it when
its prompt ends, at the ten minutes or, with an entry under way then, when that entry ends. A
create that was killed or crashed deletes nothing, so the secret stays in the store with no value.
Its owner sees it as `versions: none` in `agent-secrets secret show NAME`, and frees the name with
`gcloud secrets delete <id> --project <project number>`, `<id>` being the part after `secrets/` of
the `secret_name` that `show` prints, or gives it its value with
`agent-secrets secret set NAME`. `agent-secrets secret delete NAME` only schedules the deletion
30 days out (see [delete and restore](#delete-and-restore)), and until then the name stays taken, so
a `secret create` of it is refused. A secret without the label, or one holding a version, is never
pending. If this create stores no value, it deletes its own empty secret before exit and asks the
broker to reread the name: a refused value exits 2 and a refused store call exits 1.

SIGHUP, Ctrl-C, Ctrl-\\ and SIGTERM are handled from before the initial create through recovery,
the value prompt and adding the value. A signal waits for a call in flight before cleanup; a signal
before the first call writes and deletes nothing. Each store call from the create until the value
is stored or the empty secret deleted has a deadline of 60 seconds, which is Secret Manager's Go
client's own default for those calls, and which AWS's `CreateSecret` takes too, since the AWS SDK
sets none; each `gcloud auth print-access-token` that refreshes a gcloud login's token has the same
deadline of its own. That bound is per call, not per signal: a signal waits out the call in
flight, and then its cleanup makes its own calls - a read back, a version list, a delete - each
with the same deadline, so a first signal against a store that answers nothing can take a few
minutes to return. The first signal's cleanup says what the create left, as the command would have:
a value the store stored meanwhile is said stored (`created NAME in …`) and the broker asked to
reread the name; a store call that failed is named with what the store may hold, such as an AWS
`CreateSecret` that ran out of time and may have made the secret. A second signal while the first
one still waits ends the command at once, by that second signal, with one line naming what the
store may be left holding and the command that clears or checks it: on GCP the empty secret, or the
secret empty or holding the value, with the `gcloud secrets delete` command, and `agent-secrets
secret show NAME` for the second; on AWS the secret with its value or nothing, with `agent-secrets
secret show NAME`.

Where the store's answer to the add of the value leaves open whether it stored it - a lost answer,
a deadline, an outage - the CLI never deletes the secret, since the add may have stored the value or
may still. It lists the secret's versions to say what it found: with a version there it says the
value may have been stored; with none it says the store showed no version then and a late add may
still land; where the list fails too it says whether the secret holds the value is unknown. Each
asks the broker to reread the name where the list answered, names `agent-secrets secret show NAME`
to settle it and the `gcloud secrets delete` command for a secret that proves empty, and exits 1,
as does a delete the store refuses, which names the empty secret that stays and that command. A
SIGKILL or a crash in that stretch leaves the empty secret, which
`gcloud secrets delete <id> --project <project number>` clears, `<id>` being the part after
`secrets/` of the `secret_name` that `agent-secrets secret show NAME` prints.

Every command the CLI prints for settling what a create left signs in as that create did: an
`agent-secrets` line carries, on GCP, the `GOOGLE_APPLICATION_CREDENTIALS`,
`CLOUDSDK_ACTIVE_CONFIG_NAME` or `CLOUDSDK_CORE_ACCOUNT` that chose the credential; a `gcloud` line
carries `CLOUDSDK_ACTIVE_CONFIG_NAME` for the chosen configuration, the variables that chose a
gcloud configuration or login, `CLOUDSDK_CORE_ACCOUNT`
for the active gcloud account's login, or `CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE` for a key file,
and names the credential after it, `(as …)`, so where gcloud cannot be pointed at it (the
metadata server, the well-known credentials file) you know whose sign-in to use.

On AWS, Secrets Manager takes the value with the tags, so the secret is made in one call once the
value is read. A `CreateSecret` whose every attempt was refused with an answer Secrets Manager
gives before it acts - a name taken, access denied, a request, a parameter or a limit refused, the
last of which the AWS SDK retries as a throttle - says nothing was stored. Any other failure says
that the store may hold the secret, with its value, or not, and names `agent-secrets secret show
NAME` to tell which: a deadline, an outage, and any create one of whose attempts ended another
way before the SDK retried it, since that attempt may have made the secret, which the store then
answers the retry with as a name already taken. Each exits 1. The broker serves
the secret from the next request. Secrets Manager refuses a name that is already taken, including
one scheduled for deletion: [restore](#delete-and-restore) that one instead.

The prompt takes one line. A bracketed paste of more than one line, such as a key, is read through
its closing mark and refused (exit 2), however long its writes take, as long as no more than 10
seconds of quiet pass between them: the bound counts the quiet since the last input, not the time
since the paste began. A paste that falls quiet for 10 seconds before its closing mark comes is
given up, and the command exits 1, storing nothing; input that arrives after it is given up reaches
your shell. Most terminal emulators and tmux support bracketed paste.

Without brackets the CLI drains input until 200 ms of quiet pass after the line, with no total
limit, and Ctrl-C ends the drain. A paste-start mark split so that only its first bytes arrive
before the line ends holds the drain for one second for the rest of the mark, so a late paste still
opens and is read through rather than reaching your shell. **That quiet window is not a guarantee
about an entire paste:** bytes arriving later can reach your shell. If those bytes are a second
line, the first line may already have been
accepted as the value. Pipe multi-line values in rather than pasting them, and use
`secret set NAME < FILE` to replace a value. The refusal names the command that pipes the value in:

```console
$ agent-secrets secret set DEMO_DEPLOY_TOKEN
agent-secrets secret set: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
Value for DEMO_DEPLOY_TOKEN:
agent-secrets secret set: a value of more than one line must be piped in: agent-secrets secret set DEMO_DEPLOY_TOKEN < FILE
$ agent-secrets secret set DEMO_DEPLOY_TOKEN < key.pem
agent-secrets secret set: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
set a new value of DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1
broker: serving DEMO_DEPLOY_TOKEN from AWS Secrets Manager account 111122223333 in us-east-1
```
Ctrl-C, Ctrl-\\, SIGTERM or SIGHUP at the prompt restores the foreground terminal and leaves the
store as it was, deleting first the empty secret a GCP `create` made, then ends the CLI by that
signal. A shell reports 130, 131, 143 or 129 respectively. While that delete waits on the store,
a second signal ends the CLI at once, by that second signal, naming the empty secret that may stay
and the `gcloud secrets delete` command that clears it.
Ctrl-C also stops a shell's `;` list. On Unix, core dumps are disabled before the value is read and
stay disabled for the rest of the process. On other platforms, crash-dump policy is OS-managed.

**Ctrl-Z discards the entry.** The CLI discards what it holds of the value and whatever the
terminal holds unread when it stops, which is everything that arrived with Ctrl-Z or before it, so
none of that reaches your shell, and it refuses the entry rather than storing a partial secret.
Keys typed after the stop has taken effect go to your shell, which then holds the terminal. After
`fg`, input stays hidden only while the remainder is discarded: press Enter, then run the command
shown in the message and type the whole value again.
The command exits 2 and stores nothing. After `bg`, the reader waits without touching the
terminal until `fg` gives it back, then discards the entry in the same way. SIGTERM or SIGHUP
while it waits there ends it at once by that signal, leaving the terminal to your shell. If the
shell that ran it exits first, it exits 2, naming the command that pipes the value in, and leaves
the terminal untouched.
A wrapper that ignores SIGTSTP keeps it ignored, without discarding the entry.

**A command started with `&` waits for `fg`.** It shows no prompt and reads nothing while your
shell holds the terminal. `fg` brings it forward, the label appears, and the value reads hidden.
When the command ends, the terminal is left the way `fg` handed it over. A stop before the label
appears discards nothing, since you have typed nothing for it yet. If no shell can ever bring it
forward, for example because it was started from a subshell that has since exited, it shows
nothing and exits 2, naming the command that pipes the value in.

The hidden prompt is available on Linux and macOS for amd64 and arm64. Builds for other targets
refuse an interactive value with exit 2, naming the command that pipes the value in. None of the
prompt's macOS-specific code has run on a macOS machine: that includes the stop and resume path,
the discard of unread input at Ctrl-Z, the two checks behind the refusal of a command started with
`&` (an orphaned process group, and a shell that has exited), keeping an inherited ignored signal
ignored, and disabling core dumps. It is built and checked for macOS, and read from its kernel
source, but not exercised.

On macOS, `poll(2)` may not report `/dev/tty` ready, so reading the value from it directly
(`agent-secrets secret set NAME < /dev/tty`) can leave the prompt waiting after you press Enter;
Ctrl-C ends it and restores the terminal. Run the command from the terminal's own standard input.

## Which sign-in may do what

Before anything is read or written, every form asks AWS whose sign-in it holds
(`sts:GetCallerIdentity`, which needs no permission). A sign-in in any account but the broker's is
refused, and the refusal names both accounts:

```console
$ agent-secrets secret list
agent-secrets secret list: your AWS sign-in arn:aws:sts::999999999999:assumed-role/AWSReservedSSO_User_abc123/Ada@Example.com is in account 999999999999, but the agent secrets are in account 111122223333: sign in to account 111122223333; choose another with AWS_PROFILE
```

That account check is the one check the CLI makes of a sign-in. A write (`create`, `set`, `retag`,
`delete`, `restore`) runs under any sign-in in that account - a person's Identity Center sign-in,
a machine's role, an assumed service role, an IAM user - and on GCP under any credential the CLI
found ([Before you start](#before-you-start)), since the store's IAM, not this CLI, decides what a
sign-in may change. Where IAM refuses one, the refusal names the credential it refused as the
`using` line does - its source and whom it acts as - and how to choose another from that source.
On GCP, under every source after `GOOGLE_APPLICATION_CREDENTIALS`: a gcloud configuration in
`CLOUDSDK_ACTIVE_CONFIG_NAME`, an account in `CLOUDSDK_CORE_ACCOUNT`, or a credentials file in
`GOOGLE_APPLICATION_CREDENTIALS`. Where `CLOUDSDK_CORE_ACCOUNT` is set, it outranks the account
of any configuration, so the refusal names the account first and a configuration only once that
variable is unset. Under a credentials file, another file, or unsetting the variable to reach the
gcloud variables; under a lent token, unsetting `CLOUDSDK_AUTH_ACCESS_TOKEN`, which outranks every
other source. On AWS, `AWS_PROFILE`, or unsetting the access keys or the web identity token file
that outrank it. A create that IAM lets make the secret but not add its value says the same.

```console
$ agent-secrets secret retag DEMO_DEPLOY_TOKEN --tier human
agent-secrets secret retag: using the active gcloud account (gcloud configuration box): robot@example-project.iam.gserviceaccount.com
agent-secrets secret retag: Secret Manager UpdateSecret answered PermissionDenied (missing permission: secretmanager.secrets.update; its message is not printed); the store refused the active gcloud account (gcloud configuration box): robot@example-project.iam.gserviceaccount.com; choose another with a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME, an account in CLOUDSDK_CORE_ACCOUNT, or a credentials file in GOOGLE_APPLICATION_CREDENTIALS (gcloud configurations here: ada, box); looked in GCP Secret Manager project 100000000001
```

`--owner me` is the one place a sign-in must name a person. Under one that names none, the command
refuses `me` (exit 2) before it reads the value, and `--owner <email>` names the owner instead:

```console
$ agent-secrets secret create DEMO_DEPLOY_TOKEN --owner me --tier agent
agent-secrets secret create: using the AWS SDK's default credential chain: arn:aws:sts::111122223333:assumed-role/example-instance-role/i-0abc
agent-secrets secret create: --owner me: arn:aws:sts::111122223333:assumed-role/example-instance-role/i-0abc is no IAM Identity Center sign-in, so it names no person: name the owner with --owner <email>; looked in AWS Secrets Manager account 111122223333 in us-east-1
```

GCP has no wrong-account check: every call already names `projects/<project number>/...`
explicitly in the resource, so IAM on that resource, not this CLI, is what decides whether a read
or a write succeeds. [Operating the broker](/legion/broker/operate/#people-who-manage-secrets)
lists the calls each form makes.

## See what is there

`list` shows every agent secret, including one scheduled for deletion:

```console
$ agent-secrets secret list
agent-secrets secret list: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
NAME               OWNER            TIER   TYPE          PROJECT  PROJECT_CHANNELS  MAX_GRANT  DEFAULT_GRANT  VALUE  DELETED  EARLIEST_PURGE
DEMO_DEPLOY_TOKEN  ada@example.com  agent  -             -        -                 -          -              yes    -        -
DEMO_GITHUB_APP    ada@example.com  agent  github-app    -        -                 -          -              yes    -        -
DEMO_LOGIN         ada@example.com  human  google-login  -        -                 1h         -              yes    -        -
Every row is a secret in AWS Secrets Manager account 111122223333 in us-east-1.
```

The last line goes to standard error, so the table stays one row per secret for a script.
`VALUE` says whether a version of the secret carries `AWSCURRENT`, the label Secrets Manager reads
a value from. The broker serves a secret only while one does, so a secret another tool created
without a value shows `no` until someone runs `secret set` on it. A missing tag shows as `-`.
`show` prints one secret's store, tags, dates, who decides a request for it and the guidance the
model judge decides it on, and its versions:

```console
$ agent-secrets secret show DEMO_DEPLOY_TOKEN
agent-secrets secret show: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
name: DEMO_DEPLOY_TOKEN
store: AWS Secrets Manager account 111122223333 in us-east-1
secret_name: example/agent-secrets/demo-deploy-token
owner: ada@example.com
tier: agent
type: -
github_owners: -
requesters: -
scopes: -
admin: -
project: -
project_channels: -
rollback_guard: -
max_grant: -
default_grant: -
created: 2026-10-06T20:31:08Z
last_changed: 2026-10-06T20:31:08Z
decides: nobody, for a session its tags allow; otherwise a person, on the broker page
guidance: -
version: 8ae2ea20-8e93-4f1a-b3d1-2e5396ecef72 AWSCURRENT
```

Neither ever prints a value, and both take `--json`.

## Change its value

```console
$ agent-secrets secret set DEMO_DEPLOY_TOKEN
agent-secrets secret set: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
Value for DEMO_DEPLOY_TOKEN:
set a new value of DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1
broker: serving DEMO_DEPLOY_TOKEN from AWS Secrets Manager account 111122223333 in us-east-1
```

`set` reads the value as `create` does: one line at a prompt with echo off, or all of standard
input, which is how a value of more than one line goes in. On a type secret it checks the value
under the secret's type tag before it writes. The broker never keeps a value; it reads
it from Secrets Manager each time a session reads a grant, so the next read of a live grant gets
the new one.

## Change its owner, tier, type, requesters or max grant

```console
$ agent-secrets secret retag DEMO_DEPLOY_TOKEN --tier human
agent-secrets secret retag: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
retagged DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1 (owner=ada@example.com, tier=human)
broker: serving DEMO_DEPLOY_TOKEN from AWS Secrets Manager account 111122223333 in us-east-1
```

Name `--owner`, `--tier`, `--type`, `--github-owners`, `--requesters`, `--scopes`, `--admin`, `--project`, `--project-channels`, `--max-grant`, `--default-grant` or a combination. A retag that
sets tags reads the owner and tier tags the secret holds and sends both in one request. On AWS,
this lets an IAM condition inspect both tags. Every other tag or annotation stays as it is. For a
secret missing an owner or tier tag, name that tag too.

`--type none` removes the type tag, the old `kind` tag and the `mint-service-account` tag written with it,
`--github-owners none` removes the GitHub owners, `--requesters none` removes the list of who may
ask, `--scopes none` removes the scopes, so the token carries `cloud-platform` alone again,
`--admin none` removes the administrator's mark, `--project none` removes the project, so the
secret belongs to no project again, `--project-channels none` removes the channel limit,
`--max-grant none` removes the max grant, and `--default-grant none` removes the opt-out. A
removal is a retag of its own: it changes no other tag, so the cloud refusing it leaves the secret
as it was. A retag that names `none` beside another change is refused before it writes anything.
So is a retag that would leave a `github-app` secret without `--github-owners`, as the broker would
then refuse to serve it, one that names `--github-owners` on a secret of another type, one that
names `--scopes` on a secret of another type, and one that would leave an `admin` or
`google-service-account` secret without a `--max-grant` of an hour or less.
Run the removal and the other change as two retags:

```console
$ agent-secrets secret retag DEMO_LOGIN --max-grant none
agent-secrets secret retag: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
retagged DEMO_LOGIN in AWS Secrets Manager account 111122223333 in us-east-1 (removed max-grant)
broker: serving DEMO_LOGIN from AWS Secrets Manager account 111122223333 in us-east-1
```

A retag that changes the type first reads the value
the secret holds and checks it as that type's minter will read it once the tags are written. It
never prints the value. A value the minter could not mint from refuses the retag (exit 2), and
nothing is written. A read the store refuses your sign-in does not stop the retag: it writes the
tags and prints one line on standard error that says the value was not checked and names the
permission the read needs. The broker then reads the value with its own access, and refuses the
secret as `value-malformed` when its minter could not mint from it. Any other failed read refuses
the retag and writes nothing, and a secret with no value is one: it has no value to read.

To make a stored secret a type, retag it first, and store the App key or the Google login with
`secret set` only once the broker reads the new tags. Where your sign-in may read the secret's
value, that retag is refused while the value is not yet of that type, and so is one of a secret
with no value: create the type secret under a new name instead, as below. Where your sign-in may
not read the value, the retag writes the tags, and the broker refuses the secret as
`value-malformed` until `set` stores a value of that type. The retag then exits 1 on
`reason=value-malformed`, which says the broker reads the new tags. A secret with no value is
refused `no-current-value` whichever type tag the broker reads (`pending-value` in the ten minutes
after `secret create` made it), so there the retag exits 1 on that reason, which says nothing of the tags:
`set` is the next step, since it
checks the value under the type the retag wrote, and its reread prints
`broker: serving NAME (type <type>)` once the broker reads the new tags. A reread can read the tags
stale (Secret Manager's metadata can lag a write by hours), and while the broker still serves the
secret as stored, a grant of it releases its stored value raw. So a retag whose reread answers
another type than the one it wrote exits 1: the broker still sees the old tags. Run the retag
again, and store no value until the broker serves the secret as that type
(`broker: serving NAME (type <type>)`) or refuses it as `value-malformed`. Only a retag run where
`agent-secrets` has a broker identity (`agent-secrets login`, or inside a session) prints that
line. Run with no identity, a retag exits 0 once the store holds the type tag and says in one line
that the broker's view was not checked: that exit 0 says only that the store holds the tag, and
is not the step that lets you store the key. A `create` or `set` whose reread answers another
type than the secret holds exits 1 the same way, with the value already stored: it names the
`secret retag NAME --type <type>` that asks the broker to read the tags again, and until that
retag prints `broker: serving NAME (type <type>)` a broker serving the secret as stored releases
its value raw. A release that finds the secret retagged while it reads the store releases nothing
and ends that grant.

Exit 0 is the answer of the one broker process that served the reread, not of every process. That
process tells the others over Postgres and they reread within seconds, so exit 0 is the usual case
and the one below is the exception. Five log lines say a process may be behind, and only two of
them name the secret:

| Log line | Names a secret? | What it means |
| --- | --- | --- |
| `broker: the other pods were not told to reread a secret secret=… error=…` | **yes**, `secret` | The answering process's own `NOTIFY` failed, so no peer was told at all. Its 200 is unchanged. |
| `broker: a notification's reread of a secret did not run secret=… sender=… error=…` | **yes**, `secret` and the peer that sent it | A peer was told and its reread failed. This is the strongest evidence available: a named process behind on a named secret. |
| `broker: a Postgres listener has no connection; redialing channels=… error=… attempt=… wait_ms=…` | no | That process is outside the fan-out **for every name** while it redials, and catches nothing up when it reconnects. |
| `broker: serving without the boot's catch-up policy load … error=…` | no | A booting process could not take its catch-up read, so a retag served while it was starting may not be in force there. |
| `broker: binding before the reread listener has had an answer waited=…` | no | A booting process bound without its `LISTEN`, so notifications sent in that gap reached it not at all. |

A process left behind serves the secret as stored, and a grant of it there releases the stored
value raw.

**The way to avoid the question is not to convert.** Create the secret with its type from the
start, under a name no agent holds a grant on, and store the App key or the Google login there.
A name that was never served as an ordinary secret has no grant that can release a raw value, so
none of what follows applies to it. Converting a name agents already hold grants on is what opens
the window, and it is worth a new name to close it.

Where you must convert, before you store an App key or a Google login:

- Read the broker's log since the retag. **If any of those five lines appears, do not store yet** —
  not only one naming your secret. Three of the five carry no secret at all, so a process that
  logged one may be behind on yours and nothing in the line will say so.
- Run the retag again and require another exit 0 with none of those lines after it, or, where the
  retag wrote the tags without checking the value, exit 1 on `reason=value-malformed` alone. That
  says **the answering process** has the new tags: its reread reads the one secret directly
  (`DescribeSecret`, `GetSecret`) rather than through a listing, and the retag exits 1 unless the
  type it reads back is the one it wrote. On Secrets Manager that read answers a write at once;
  on Secret Manager a single read is eventually consistent like the listing, which is why a retag
  there can need running more than once. A secret with no value answers `reason=no-current-value`
  (`reason=pending-value` in the ten minutes after `secret create` made it) whichever tags a process reads, so
  no retag of it says this. For such a secret, `secret set` is
  the next step, and the line its own reread prints, `broker: serving NAME (type <type>)`, says
  the answering process has the new tags.
- **It says nothing about the peers, and no log line will.** A peer's reread reads the one name
  too, so on Secret Manager it can succeed while still reading the old tags: the broker warns
  only when that read returns an error, and a successful read of stale tags is not one. The
  notification is asynchronous besides, so a healthy peer need not have applied the retag by the
  time exit 0 is printed. Neither a second exit 0 nor a clean log establishes the peers — that is
  the residual `dispatch://INFRA-375` tracks, and the reason the paragraph above recommends a new
  name instead.
- If you cannot read the log, wait at least five minutes after exit 0. That is the pod's own
  reload interval, and the wait is **the right action rather than a guarantee**: the reload is
  another listing read, and a store's listing lags a write by up to five minutes on Secrets
  Manager and up to six hours on Secret Manager, so on Secret Manager five minutes is not a bound
  at all. `dispatch://INFRA-375` tracks that
  lag and the two ways to close it.

Waiting costs nothing; storing early can hand the raw key to an agent through a process that still
serves the secret as stored.

A retag checks the tags it read. On GCP it writes against that read, so a retag another person
wrote in between refuses it, with `another write changed the secret after this retag read it`,
and nothing is written. AWS has no such check: a retag there
reads the tags back after its write, and when another writer's change left a type without the
GitHub owners it needs, it says so and puts nothing back, naming the tags it read, the tags it
found and the retag that sets the secret right, and exits 1. Both writes stand.

You can retag your own secret, to `--owner shared` too. Once a secret is shared, its owner and
tier are an administrator's to change: on AWS, by the IAM condition above; on GCP, because a
deployment that wants this protection grants `secretmanager.secrets.update` on a shared secret
only to administrators. When the cloud refuses a retag that sets the tags of a shared secret, the
CLI prints that refusal, then `a shared secret's owner and tier are an administrator's to change`,
and exits 1. It prints the same line when the cloud refuses a retag to `--owner shared`; for your
own secret, that refusal means your access does not include the retag. A refused removal prints
the cloud's refusal alone
([Troubleshooting](/legion/broker/guides/troubleshooting/#managing-a-secret)).

New tags decide what sessions get from the next request, and reach some grants already given:
[Approvals](/legion/broker/concepts/#approvals) says which.
[Revoke a session or a grant](/legion/broker/guides/revoke-a-session/#end-every-sessions-access-to-a-secret)
uses a retag to end every session's automatic access to a secret.

## Delete and restore

```console
$ agent-secrets secret delete DEMO_DEPLOY_TOKEN
agent-secrets secret delete: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
deleted DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1
broker: DEMO_DEPLOY_TOKEN is deleted in AWS Secrets Manager account 111122223333 in us-east-1 (restorable until 2026-11-05T20:22:33Z)
```

`delete` schedules the deletion with a 30-day recovery window, the longest Secrets Manager allows,
and never forces it. The date it prints is the one Secrets Manager answered for the deletion: the
end of that window. The broker stops serving the secret at once, so a grant that held it releases
nothing (`GRANT_NOT_LIVE`). Until you restore it, Secrets Manager keeps the secret but refuses to
read its value, to change its value or tags (`set`, `retag`), and to `create` another of the same
name. `delete` and `restore` are permissions of their own (`secretsmanager:DeleteSecret`,
`secretsmanager:RestoreSecret`): where your access does not include one, AWS refuses it with
`AccessDeniedException`, your own secret included. GCP has no separate recovery window: `delete`
sets the secret's `expire_time` to 30 days out in one `UpdateSecret` call and prints it; the secret
and every version stay readable and restorable until that moment, and GCP deletes them for good,
irreversibly, once it passes. `restore` clears `expire_time` the same way.

`list` and `show` keep showing a deleted secret, with when it was deleted (AWS) or GCP's
`expire_time`, the future moment it purges for good (GCP - no separate EARLIEST_PURGE there, since
that moment is both), and the **earliest** it can be purged:

```console
$ agent-secrets secret list
agent-secrets secret list: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
NAME               OWNER            TIER   TYPE  PROJECT  PROJECT_CHANNELS  MAX_GRANT  DEFAULT_GRANT  VALUE  DELETED               EARLIEST_PURGE
DEMO_DEPLOY_TOKEN  ada@example.com  human  -     -        -                 -          -              yes    2026-10-06T20:22:33Z  2026-10-13T20:22:33Z
DEMO_EMPTY_KEY     ada@example.com  human  -     -        -                 -          -              yes    -                     -
DEMO_SHARED_KEY    shared           human  -     -        -                 -          -              yes    -                     -
```

`EARLIEST_PURGE` is the deletion plus 7 days, Secrets Manager's shortest recovery window: Secrets
Manager answers when a secret was deleted, but not the window its delete chose. A delete through
this CLI stays restorable for 30 days, and one from the AWS console or another tool for between 7
and 30, so `restore` works at least until that date and perhaps well after it. The legend goes to
standard error, so standard output stays one row per secret. `show` prints the same two dates as
`deleted:` and `earliest_purge:`.

`restore` cancels the deletion, and the broker serves the secret again:

```console
$ agent-secrets secret restore DEMO_DEPLOY_TOKEN
agent-secrets secret restore: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
restored DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1
broker: serving DEMO_DEPLOY_TOKEN from AWS Secrets Manager account 111122223333 in us-east-1
```

## What the broker answers after a write

After each write, the CLI asks the broker to reread the secret (`POST /v1/secrets/{name}/reread`)
and prints its answer on a line of its own, naming the store the secret is in. The CLI signs that
call with a broker identity this process already holds — a key directory holding its key and
enrollment, your `agent-secrets login`, a helper that answers, or the machine's cached session —
because the broker says what it now serves only to a call a session signs. It waits on nothing
and trades no machine identity to find one. The held sign-in is
`AGENT_SECRETS_PERSON`'s, or the only local sign-in when that variable is unset, whatever cloud
credential wrote the secret. A sign-in record with no enrollment registers while the CLI looks up
its signer. A sign-in session that lapses after `BROKER_LEASE_SECONDS` (15 minutes by default)
registers again only when the broker refuses that existing session as not live, under the same box
key, and signs the reread with the new session:

| Line | What it means |
| --- | --- |
| `broker: serving NAME from <store>` | The broker serves the secret as just written, as a stored secret. |
| `broker: serving NAME from <store> (type <type>)` | The broker serves the secret as that type: it mints from the stored value and never returns it. |
| `broker: NAME is deleted in <store> (restorable until <date>)` | After a delete: the broker no longer serves it. |
| `broker: refusing NAME from <store> (reason=<reason>)` | The broker leaves the secret out: `absent` (it found no such secret, or one scheduled for deletion), `pending-value` (a secret `secret create` made and has not yet filled, which carries its `agent-secrets-create` label, holds no version and was made less than ten minutes ago: the broker waits for its value rather than refuses it), or one of the refusals [Operating the broker](/legion/broker/operate/#health-and-logs) lists, such as `no-current-value`. |
| `broker: rereading NAME from <store>` | The broker's view could not be checked: this process holds no broker identity, its identity could not sign, or the broker could not check or did not accept the proof. So the reread went with no proof. The broker took it and rereads the secret after answering, but tells such a call nothing of what it serves, and a reread that fails then reaches only the broker's log. |

After `broker: rereading`, one line on standard error says why the call carried no proof and what
the exit 0 then says:
`the broker at <host> took the reread of NAME, but how it serves NAME was not checked (…), so this exit 0 says only that the write stands`,
with the signed `curl` that checks it. The command exits 0: the write stands and nothing
contradicts it.

A write that leaves a type (`create --type`, `set` on a type secret, a `retag` that leaves one)
also reads the secret's type tag back from the store, under the sign-in it wrote with, whatever
the broker answers, a refused or unreachable reread included. When the store holds another type,
or none, it exits 1 with
`the write to <store> stands, but NAME's type tag there is <tag>, not <type>`, naming the retag
that writes it again: a type secret the store holds untyped is served as stored, its key or login
released as it is. A read back that fails exits 1 too: a sign-in that may write the tags and not
read them fails every typed write. When the tag is right and the reread went with no proof, it
prints one line instead of the one above,
`NAME carries the type tag <type> in <store>, but whether the broker at <host> serves it as <type> was not checked (…), so this exit 0 says only that the store holds the tag`,
which says to store no key or login in NAME until the retag it names, run where the command has a
broker identity, prints `broker: serving NAME from … (type <type>)`. It exits 0.

`<store>` is the store the broker's settings name: `AWS Secrets Manager account <id> in <region>`,
or `GCP Secret Manager project <number>`. The command exits 0 when the answer is what the write
meant. It exits 1 when the answer contradicts the write, and says so on standard error: `the
broker at <host> refuses NAME after the write to <store> (reason=…)` after a create, set, retag or
restore, or `the broker at <host> still serves NAME after its delete in <store>`. The write itself
stands in the store either way, so fix what the reason names rather than repeating the write:

```console
$ agent-secrets secret retag DEMO_EMPTY_KEY --tier human
agent-secrets secret retag: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
retagged DEMO_EMPTY_KEY in AWS Secrets Manager account 111122223333 in us-east-1 (owner=ada@example.com, tier=human)
broker: refusing DEMO_EMPTY_KEY from AWS Secrets Manager account 111122223333 in us-east-1 (reason=no-current-value)
agent-secrets secret retag: the broker at secrets.internal.example refuses DEMO_EMPTY_KEY after the write to AWS Secrets Manager account 111122223333 in us-east-1 (reason=no-current-value)
```

Here the secret has no value; `secret set DEMO_EMPTY_KEY` gives it one, and the broker serves it.

A `requesters` entry naming a service is answered the same way. This CLI holds no list of the
services a deployment registers, so it checks such an entry's shape and nothing more; the broker
is what knows the names, and it refuses a secret naming one it does not register with
`reason=requesters-malformed`, after the write, with the list already in the store. Retag the
secret with the name the deployment registers, and the broker serves it:

```console
$ agent-secrets secret retag DEMO_DEPLOY_TOKEN --requesters "ada@example.com legion-daemn"
agent-secrets secret retag: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
retagged DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1 (owner=ada@example.com, tier=agent)
broker: refusing DEMO_DEPLOY_TOKEN from AWS Secrets Manager account 111122223333 in us-east-1 (reason=requesters-malformed)
agent-secrets secret retag: the broker at secrets.internal.example refuses DEMO_DEPLOY_TOKEN after the write to AWS Secrets Manager account 111122223333 in us-east-1 (reason=requesters-malformed)
$ agent-secrets secret retag DEMO_DEPLOY_TOKEN --requesters "ada@example.com legion-daemon"
agent-secrets secret retag: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
retagged DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1 (owner=ada@example.com, tier=agent)
broker: serving DEMO_DEPLOY_TOKEN from AWS Secrets Manager account 111122223333 in us-east-1
```

When the broker cannot be asked at all (it is unreachable, or its
[reread limit](/legion/broker/operate/#network-and-trust) refuses the call), the write still stands
and the command exits 1:

```console
$ agent-secrets secret set DEMO_DEPLOY_TOKEN < token.txt
agent-secrets secret set: using the AWS profile ada (AWS_PROFILE): arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc123/ada@example.com
set a new value of DEMO_DEPLOY_TOKEN
agent-secrets secret set: the write to the secret store stands, but the broker could not be asked to reread DEMO_DEPLOY_TOKEN, so it serves the change only from its next reload: too many secret rereads; try again later (RATE_LIMITED)
```

Do not repeat the write. A new value is read at the next grant read anyway, and a create, a
restore, or a first value for a secret that had none is served on the first request naming it,
since the broker rereads a name it does not serve before refusing it. A retag or a delete of a
secret the broker serves takes effect at its next read of the namespace, within about ten minutes,
or at once when you ask for the reread yourself — one call, whichever process it reaches, since
that process tells the others. A call with no proof is answered `202` with `"accepted":true` and a
fixed `reason` at once, whatever the name, and the broker rereads the secret after that answer; to
read what the broker then serves, sign the call as this process's session:

```sh
curl -s -X POST "$AGENT_SECRETS_URL/v1/secrets/DEMO_DEPLOY_TOKEN/reread"
curl -s -X POST -H "Proof: $(agent-secrets sign --method POST --url "$AGENT_SECRETS_URL/v1/secrets/DEMO_DEPLOY_TOKEN/reread")" \
  "$AGENT_SECRETS_URL/v1/secrets/DEMO_DEPLOY_TOKEN/reread"
```

After a write whose outcome is open, where the broker could not be asked, the CLI prints the first
of these in a form of its own:
`u=${AGENT_SECRETS_URL:?…} && curl -sSf -o /dev/null -X POST "${u%"${u##*[!/]}"}/v1/secrets/NAME/reread"`.
It strips the slashes an address typed by hand may end in, so the path is the broker's own whatever
curl does with a doubled slash. It prints nothing of the broker's `202`, whose `reason` says the
write stands, and it exits non-zero on a refusal.

A write can also end with an error that is no refusal: a service fault, an outage, a lost
connection, or an AWS call one of whose attempts ended another way before the SDK retried it.
Such an error may follow a write the store carried out, so the form says
`the store's answer leaves open whether this write landed`, exits 1, and prints the broker's
answer to the reread without judging it, since the store may hold the write or not. It says what
to do next: after a `set`, run it again, since `secret show` reads no value; after a `delete`,
`restore` or `retag`, read the secret with the `secret show` it names.

[Troubleshooting](/legion/broker/guides/troubleshooting/#managing-a-secret) lists the messages the
`secret` forms print and what to do about each.
