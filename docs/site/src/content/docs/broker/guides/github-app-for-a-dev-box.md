---
title: GitHub App for a development box
description: Register a GitHub App, store its key, and use its installation tokens from a development box.
sidebar:
  order: 10
---

# GitHub App for a development box

Use one GitHub App for one person. Store the private key of that App in the secret of the person.
The broker makes new installation tokens from the key each time a session reads the secret.

Replace each `<placeholder>` before you run a command.

## Register the App

Register the App on your own GitHub account, not on an organization. Sign in to GitHub, then open
this link:

```text
https://github.com/settings/apps/new?name=<person>-development-box&url=https://example.invalid&public=true&contents=write&pull_requests=write&issues=write&workflows=write&metadata=read
```

The link fills in the settings of the App. Make sure that the page shows the settings in this
reference list, then click **Create GitHub App**.

```yaml
name: <person>-development-box
url: https://example.invalid
public: true
permissions:
  contents: write
  pull_requests: write
  issues: write
  workflows: write
  metadata: read
webhook: off
```

GitHub sets webhooks off by default. Do not enable them. Do not add permissions.

The App must be public, because you install it on more than one account. GitHub lets other
accounts install a public App. Install it only on the accounts that the box must use.

## Generate a private key

1. Open the settings page of the App. GitHub opens it when you create the App.
2. Find the **App ID** on the **General** page. Record it.
3. Under **Private keys**, click **Generate a private key**.
4. GitHub downloads a `.pem` file. Move it to a folder that only you can read.

## Install the App

1. On the settings page of the App, open **Install App**.
2. Select an account that owns repositories that the box must use.
3. Select the repositories that the box must use.
4. Click **Install**.

Do these steps for each account that the box must use. If you do not own an organization, GitHub
sends your request to its owners. An owner must approve the request before the box can use the App.

## Tell the administrator which accounts the box uses

The broker makes tokens only for the accounts that the secret's `github-owners` tag lists, and
that the profile you ask for names. Anyone can install a public App. The tag stops a token for an
account that you did not choose.

You cannot change the tag. The deployment sets it when it creates your secret. Send the
administrator the account names where you installed the App. Ask them to add any account that the
deployment does not list.

The broker serves a `github-app` secret only through the profiles that the deployment's profiles
file declares for it, and refuses the secret by its own name when no profile names it. The
administrator declares your profiles with your secret: one of its own name, with the permissions
your App holds, and others that narrow it, such as `GITHUB_APP_<PERSON>_READ`, a read profile.

The broker returns one token for each installation that is not suspended, on an account that the
profile names. An installation on any other account gets no token.

## Store the App key

The deployment creates an empty secret named `GITHUB_APP_<PERSON>`, with the `type: github-app`
and `github-owners` tags. Your access lets you set its value.

Set the private key only in a secret that has the `type: github-app` tag. If the secret has no
`type` tag, a session that has a grant for it gets the stored value. That value is your private
key. To change a secret without a type to `github-app`, do these steps in this sequence:

1. Change the tags with `agent-secrets secret retag NAME --type github-app --github-owners "<owners>"`.
   The retag reads the value of the secret and checks it as an App key, because the broker reads
   it that way after the change. If your sign-in can read the value, the retag stops with exit 2
   and writes nothing when the value is not an App key. It also stops, with exit 1, when the
   secret has no value. Then ask the administrator for a new secret that has the
   `type: github-app` tag. If your sign-in cannot read the value, the retag writes the tags and
   tells you that it did not check the value.
2. Make sure that the broker uses the new tags. The retag shows `broker: serving NAME (type
   github-app)`, or it exits 1 with `reason=value-malformed`, because the old value is not an App
   key. If the retag exits 1 and tells you that the broker still sees the old tags, do the retag
   again. Do not set the private key until the broker uses the new tags. If the secret has no
   value, the retag exits 1 with `reason=no-current-value`, or `reason=pending-value` in the ten
   minutes after the secret was created. The broker gives that reason for a secret with no value,
   with the old tags and with the new tags, so the retag cannot show which
   tags the broker uses. Then set the private key (step 3): `secret set` checks it as an App key
   before it writes it, and shows `broker: serving NAME (type github-app)` when the broker uses the
   new tags.
3. Set the private key.

You can do this step on your own computer or on the box. The procedure is the same. The agents on
the box run as your user. They can read a file that you keep there, and they can use a Google
sign-in that you leave there. Thus, on the box, delete the key files and delete your sign-in when
you complete the step.

Sign in one time with `gcloud auth login`. This command does not change the credentials of the
box. `agent-secrets` writes with the credential of the first source it finds, in this order: an
access token in `CLOUDSDK_AUTH_ACCESS_TOKEN`, then a credentials file that
`GOOGLE_APPLICATION_CREDENTIALS` names, then the gcloud login that `CLOUDSDK_CORE_ACCOUNT` names,
else the gcloud configuration that `CLOUDSDK_ACTIVE_CONFIG_NAME` names (the account variable
outranks the account of any configuration), then the well-known credentials file, then the
active gcloud account, and last the credentials of the box. Thus, before the `set`, unset the
three variables that come before the configuration, so that your own login signs in and not a
token or a login you exported for an earlier step:

```sh
unset CLOUDSDK_AUTH_ACCESS_TOKEN GOOGLE_APPLICATION_CREDENTIALS CLOUDSDK_CORE_ACCOUNT
```

Then, on the box, name the configuration your `gcloud auth login` signed in to, as the `set`
command below does; the command's first line names the credential it used. For more
information, refer to [Before you start](/legion/broker/guides/manage-a-secret/#before-you-start)
in "Manage a secret". Then build the file and set the secret.

Build `app-key.json` from the App ID and the private key file that GitHub gave you. A PEM key
spans several lines, which JSON must escape, so let `jq` write the file. Do not paste the key into
the file.

```sh
(umask 077 && jq -n --arg app_id '<app-id>' --rawfile private_key '<private key file>.pem' \
  '{app_id: $app_id, private_key: $private_key}' > app-key.json)
```

Set the secret from standard input, then delete the two key files once the set succeeds. Do not
put the private key on a command line.

```sh
CLOUDSDK_ACTIVE_CONFIG_NAME='<your gcloud configuration>' agent-secrets secret set GITHUB_APP_<PERSON> < app-key.json \
  && rm app-key.json '<private key file>.pem'
```

`secret set` checks the value before it writes it, as the broker reads it: a JSON object with an
`app_id` of digits and a `private_key` that signs. If the check fails, the command exits 2, writes
nothing, and names the problem, for example `GitHub App secret needs an app_id and private_key`.
The key files stay, so fix `app-key.json` and set the secret again.

If the command shows `set a new value of GITHUB_APP_<PERSON> in …` and then exits 1, the key is
stored. The exit 1 means that the command could not ask the broker to read the secret again, or
that the broker refuses the secret. The `&&` does not delete the key files in this case, so delete
them yourself:

```sh
rm app-key.json '<private key file>.pem'
```

The line after `set a new value` tells you why the command exits 1.
[Troubleshooting](/legion/broker/guides/troubleshooting/#managing-a-secret) gives the fix for
each message.

If you do this step on the box, delete your Google sign-in from the box after you set the secret:

```sh
gcloud auth revoke '<your email>'
```

This command deletes your Google sign-in from the box, so the agents on the box cannot use it.

The broker returns a JSON value with one entry for each active installation. The account name of
each installation is the entry key.

```json
{
  "installations": {
    "<owner>": {
      "installation_id": 123,
      "token": "<token>",
      "expires_at": "2026-01-01T01:00:00Z"
    }
  }
}
```

The broker never returns the private key. Each time a session reads the secret, the broker makes a
new token for each installation. If GitHub refuses a token request, the broker returns
`MINT_FAILED` with the HTTP status code from GitHub.

## Keep each release until it expires

Each token is valid until its `expires_at` time, about one hour after the release. Keep the release
JSON and use it again until that time. Read the secret again only when the token expires.

Do not read the secret for each Git operation. Each read sends a request to GitHub for each
installation, and the secret store counts each read against its quota.

Keep the release in memory, or in a file that only your user can read.

## Configure Git

Your Git credential helper must get the release JSON with
`agent-secrets GITHUB_APP_<PERSON> -- <command>`, which runs `<command>` with the release JSON in
the `GITHUB_APP_<PERSON>` environment variable. The helper must read the repository owner from
the Git URL. Set `credential.useHttpPath` so that Git sends the URL path to the helper:

```sh
git config --global credential.https://github.com.useHttpPath true
```

The helper must select `.installations["<owner>"].token` from the release JSON that it keeps, and
write these two lines to Git:

```text
username=x-access-token
password=<token>
```

Do not put the token in the Git remote URL. Do not put the token in a command argument. Give the
token to Git on standard output.

## Use GitHub CLI

Get the release JSON through `agent-secrets`. Select the token for the owner that you will access.
Set `GH_TOKEN` only for the `gh` command that needs it.

```sh
agent-secrets --reason "GitHub CLI <owner>" GITHUB_APP_<PERSON> -- sh -ceu '
token=$(printf %s "$GITHUB_APP_<PERSON>" | jq -r ".installations[\"<owner>\"].token")
GH_TOKEN=$token gh repo view <owner>/<repository>
'
```

The token expires at the time in `expires_at`. Read the secret again after the token expires.

## Give a narrowed token to another machine

A token you give to another machine, such as a virtual machine that must only read one
repository, must be narrowed. Ask for a profile that narrows the App's permissions, such as
`GITHUB_APP_<PERSON>_READ`, and narrow it further on the `agent-secrets` command that reads it:

- `--target <owner>` gives a token for the installation on that account only. The account must be
  one that the profile names.
- `--target <owner>/<repository>` limits the token to that repository of the account. Use the
  flag one time for each repository, all of one account, at most 100 times. A repository must be
  one that the profile reaches.

`--target` never widens a profile, and it narrows a run of that one name.
`agent-secrets` checks it before it asks for the secret: a target of two accounts exits 2, and
nothing is sent.
A target that the profile does not hold is refused `400 TARGET_REFUSED`, and the message names the
profile's accounts and repositories.

```sh
agent-secrets --reason "read <repository> on <machine>" GITHUB_APP_<PERSON>_READ \
  --target <owner>/<repository> -- sh -ceu '
printf %s "$GITHUB_APP_<PERSON>_READ" | jq -r ".installations[\"<owner>\"].token"
'
```

In the `jq` key, write the account's login as GitHub writes it: the release names each
installation by that login, whatever case you type in `--target`.
A profile whose `github.value` is `token` puts that one account's token in the variable in place of
the JSON object of installations, so a command that sends it to GitHub in a header needs no `jq`.
Such a profile needs one account in effect: give `--target` when it names more than one.

The broker asks GitHub for the profile's permissions cut to what the App's installation holds, and
names each permission it left out in the release (`github_left_out`); `agent-secrets` writes one
line on stderr for each account, `the profile <NAME> left out <permissions> for <account>, since
the installation there does not hold them`, and runs the command.
A profile that asks for more than `metadata` and whose permissions the installation holds none of
but `metadata`, which every installation holds and which alone does nothing the profile is for,
or that names a repository the installation does not reach, is refused `422 PROFILE_MINT_REFUSED`;
an account the App is not installed on is refused with a message that says `has no installation
on` that account, `422 PROFILE_MINT_REFUSED` when the broker refuses the request, and
`502 MINT_FAILED` when the refusal comes as the broker reads the grant.
When a person approves the request, the broker makes these refusals before the approval request:
it makes the token that your read would get, reads GitHub's answer, and gives that token back to
GitHub at once, so no request waits and the person is not asked.
When GitHub does not answer the broker or refuses the App itself, or the check passes its time
limit, the request goes to the approver as before, and the refusal comes when the broker reads the
grant.
The broker gives out a token only when GitHub's answer shows that it holds no more than the profile
asked.

An `agent-secrets` of an earlier release took this narrowing as flags of its own, and sent it in the
request's `github_app` field. The broker refuses every body that carries that field with
`400 GITHUB_APP_FIELD_REMOVED` before it writes anything or asks anyone, so such a client runs
nothing. This release refuses those flags itself, with exit 2, and names `--target` and the
profile form.

The target applies to this read only. The next read of the profile without `--target` gives the
token for every account and repository that the profile names. Keep a narrowed token apart from
the other tokens, so that you do not use one in place of the other.
