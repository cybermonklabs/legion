---
title: Approve a request
description: Find a pending secret request in your Dispatch Inbox, open its page on the secrets broker, check what it asks for, and approve or deny it there.
sidebar:
  order: 10
---

When an agent asks for a secret a person must approve, that person decides it on the secrets
broker's own page, signed in to the broker. Only the approver the request names can decide it: the
secret's owner, for a person's secret; any member of the group, for a secret a Google group owns,
whose request waits in every member's Inbox until the first member to decide it decides it for
all of them; or anyone who signs in to the broker, for a shared secret. A
shared secret's request waits in the Inbox of the person responsible for the agent that asked: the
person whose session it is (their own `agent-secrets login`, the machine login they approved, or
[their pod](/legion/broker/concepts/#a-persons-pod), when the broker's `BROKER_POD_OPERATORS` maps
its account to them as the pod asks), the person a fleet machine's `BROKER_VM_EXCHANGE_ACCOUNTS`
entry names, or the assignee of the Dispatch issue a Legion worker pod works on. Where none of these
names a person, it waits in every person's Inbox, so no request waits where nobody sees it. Where
that person may not see the request, because it belongs to a private project they are not on, it
waits in the Inbox of everyone who may see it, for as long as Dispatch answers within the list's
two seconds for such requests. Anyone signed in who may see it may still decide it from its page
whether or not their Inbox lists it, and the first person to decide it decides it for everyone: it
then leaves every Inbox.
[Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret) explains which
requests need whom.

A group's members are the people the broker read in the group from Google Workspace within the
last 2 hours. A person added to the group may decide from the broker's next hourly read of it, and
while the broker cannot read the group for longer than 2 hours, nobody may decide its requests and
no Inbox lists them ([A group's secret](/legion/broker/concepts/#a-groups-secret)). The record
names the group as the approver and you as the person who decided, and the grant is the group's:
every member the broker's last good read of the group lists sees it on their Settings page and may
end it, however old that read is. A member may approve a request their own session made, as the
one owner of a person's secret may: a group owner is not a second person.

Some requests never reach you. Where the deployment configures a model judge, a request for keys
whose profile's `proxy` block in the profiles file (`BROKER_PROFILES_FILE`) gives the judge seconds,
none of them an administrator's or an Anthropic federation key, is the judge's to decide: it grants
or refuses it, each key against the guidance its owner gave in that block, and while it decides the
request is on no list. A request the judge cannot decide comes to you once,
as an ordinary card. If you open such a request's page while the judge holds it, the page is
read-only and says the judge is deciding it. A judged grant's **State** line reads
`approved by the model judge judge:<model id> at <time>`, with no person beside it.
The grant is still yours to end: it records you as its
approver, or anyone for a shared secret, so it is on your Settings page, where the row reads
**Approved by** `judge:<model id>`, and `agent-secrets grant revoke` ends it
([revoke a session or a grant](/legion/broker/guides/revoke-a-session/)). Ending it ends that grant
alone and withholds nothing, so the session may ask again, and the judge may approve that request
as it approved this one.
[Concepts](/legion/broker/concepts/#the-model-judge) says what the judge checks before it asks a
model anything.

## Find it

Pending requests you are asked to decide appear at the top of your Dispatch **Inbox**, under **Credential
requests**, one row per request: **Secret request** with the secrets' names, or **Machine login**
with a machine's host name ([log a machine in](/legion/broker/guides/log-a-machine-in/) covers
those). The section is absent when nothing is listed for you, and hidden entirely when the
deployment has no broker connected. The agent's own output names the same page: `waiting for <you>
to approve it on the secrets broker: <url>`.

A **Secret request** row is a link to the request's page on the broker,
`<broker>/approve/<record>`. Click it. When you signed in to the broker within the last day (a
sign-in lasts a day unless the deployment set it shorter) the page opens at once. Otherwise the
broker sends you to sign in with your Workspace account and brings you back to the same request.
Signed in to that one Google account, you are asked nothing; signed in to several, Google shows
its account chooser, and you choose the Workspace account.
A **Secret request** row that is not a link, with nothing to open, comes from a broker older than
that page; ask whoever runs that broker to upgrade it.

A row marked **Expired** is a request or a machine login whose deadline passed before anybody
decided it. Nobody can approve or deny it now, so it is not a link and it is not counted in
**Needs you**; the line under it says when its deadline passed. It stays until you click
**Dismiss**, which takes it out of the Inbox for good, and out of everyone's Inbox for a shared
request. The agent asks again with a new request if it still needs the secret, and that request is
a row of its own.

## Check it

The page prints the broker's own record of the request, not anything Dispatch or the agent passed
along:

- **Secrets**: the secrets asked for. A grant covers all of them or none.
- **Project of NAME**: shown for each secret that belongs to a Dispatch project (its
  `dispatch-project` tag): the project's key, whether it is public or private, and, for a private
  one, whether the session or pod that asked names it. A private project's secret goes only to a
  session or pod that names the project; the broker refuses the rest before anyone is asked.
- **Projects the session names**: the projects the session or pod that asked names, shown when it
  names any. A person's session names the projects it started with (`AGENT_SECRETS_PROJECTS`). A
  person's pod names the projects it enrolled with, as its person's session does; any other pod
  names the one its operator's `BROKER_EXCHANGE_ACCOUNTS` entry maps its service account to, or the
  list of its `BROKER_EXCHANGE_JOB_ACCOUNTS` entry, while that entry names the issuer whose token
  enrolled the pod, and a machine's own session the one `BROKER_VM_EXCHANGE_ACCOUNTS` maps its
  fleet's Google account to, or the list of its `BROKER_VM_EXCHANGE_JOB_ACCOUNTS` entry.
  A session that moved to another box since it asked shows the projects named on either box,
  and the request is hidden from, and refused to, anyone either list would hide it from.
- **Requested by (verified by the broker)**: the enrollment asking, as kind, operator and runtime
  id. A `session` started on a machine has a runtime id that starts with the machine's host name; a
  `pod` enrollment also shows its worker slot when it has one.
- **Session the request names (its own unsigned claim, not verified)**: shown only when the request
  names a session other than its enrollment's. The enrollment above is the one the broker checked.
- **Reason**: the `--reason` the agent gave, shown as plain text. Every request you see here has
  one: a request with no reason is denied before anyone is asked to approve it.
- **Lasts once approved**: how long the grant would last from the moment you approve: the `--ttl`
  the agent asked for; when it named none, **15 minutes** if any secret is human tier and
  **12 hours** otherwise, or **until the session ends** for a secret tagged
  `default-grant=session`.
- **Requested** and **Expires**: when it was made, and when it
  [expires](/legion/broker/concepts/#approvals) if nobody decides it.
- **Approver**: who may decide it: an email, `a member of the group <address>` for a secret a
  group owns, or anyone signed in for a shared secret.
- **State**: pending, or how it was decided, by whom and when.

The page header names who you are signed in as. Signed in as someone the request does not name,
or for a group's request as someone the broker's last read of the group does not list,
the page says who may decide it and offers no buttons, only **Sign in as someone else**, which
sends you through the sign-in again with an account chooser and back to the same request.

A request of a private project is the project's own: a request for a secret of the project, a
request from a session or pod that names the project (as **Projects the session names** above
reads), or a request whose reason names one of the project's issues. Signed in as someone who may
not see the project, its page says **No such request**, as for a request that does not exist, it
is in no list of yours, and you cannot decide it, whoever the approver line names.

To tell whose a request is, a broker set up to ask Dispatch about projects
(`BROKER_PROJECT_ACCESS_URL`) asks it, for you, about each project the request touches, the project
of each issue key its reason names included: `INFRA-12: rotate the deploy key` asks about `INFRA`,
even when no secret is tagged. Such a broker refuses a request whose reason names the issues of
more than 16 projects when it is made. While Dispatch cannot answer about a
request, that request is in no list of yours, and its page answers with its record id and `The
secrets broker could not ask
Dispatch whether you may see this request, so it shows nothing of it and cannot decide it until
Dispatch answers. Try again shortly.`, with no names, no reason and no buttons. Every other
request is listed and decided as before. Open it again once Dispatch answers.

Approve only what the reason and the session justify. The agent's command is waiting; a denial runs
nothing.

## Decide it

Click **Approve** or **Deny**. The broker records the decision under the email you signed in to it
as, so a shared secret's record names whoever decided it, and the agent's waiting command runs, or
exits 77. Every other request waiting on you is listed below the first, each with its own buttons,
and **Approve all N shown** approves every request the page printed, and only those.

The page then shows, under **What happened**, what became of each request it decided:

| Line | Why |
| --- | --- |
| `granted (approved by <you> at <time>)` | The grant is made; the agent's command collects it. |
| `denied by <you> at <time>` | You denied it; the agent's command exits 77. |
| `recorded as denied because the requesting session ended` | You approved it, but the session that asked had ended, so nothing was granted. |
| `already decided: …` | Someone else, another tab of your own, or the model judge decided it first. The rest of the line says who and when: `already decided: granted (approved by judge:<model id> at <time>)` for the judge. |
| `cancelled at <time>: the requesting session withdrew it or ended` | Nobody decided it: the session that asked ended first. |
| `expired before anyone decided it` | It waited past its expiry. The agent asks again if it still needs the secret. |
| `not yours to decide; only <approver> may` | The request names someone else, or a group the broker's last read of it does not list you in. |
| `still pending; it was not decided` | The broker refused your approval though the request names you: the secret's tags now name someone else to approve it ([what an owner change does](/legion/broker/concepts/#approvals)), the session's operator withheld one of its secrets, which only that secret's owner may then approve, and no one while the broker does not serve that secret to that session (it is out of the policy, or a service's), or the secret's `max-grant` became shorter than the lifetime the request asked for, which no one may then approve. You can still deny it. |
| `still pending; it was not decided: who may ask for it names a Google group the broker has not read in the last two hours. Approve it again once the group is read.` | The secret's `requesters` name a Google group the broker has not read for 2 hours, so it cannot say whether the session's person is on the list ([A group's secret](/legion/broker/concepts/#a-groups-secret)). The request stays pending. Approve it again once the group is read: the broker reads every group within the hour, and at the next five-minute policy read for one whose first read failed. |
| `unavailable: the secrets broker could not ask Dispatch whether you may see this request, …` | Dispatch did not answer whether you may see the request, so nothing was decided and it stays pending. The line names the request by its record id, not by its secrets. Decide it once Dispatch answers. |
| `still pending; it was not decided: Dispatch called you a restricted member, who decides no secret request.` | Dispatch called you a restricted member when you decided: someone an access list admitted from outside the deployment's sign-in domain, who is no approver of a secret ([Machine login](/legion/broker/concepts/#machine-login)). Nothing was decided. |

The page shows no request at all to a person Dispatch calls a restricted member: it answers `A
person Dispatch calls a restricted member is no approver of a secret, and Dispatch calls you one,
so this page shows you no secret request.` (`403`), whatever request it names. While the broker
cannot learn from Dispatch within 2 seconds what kind of member you are, it answers `The secrets
broker could not ask Dispatch whether you may decide secret requests, so this page shows you none,
nor what became of a decision you made. Try again shortly.` (`503`): a decision you made before
stands, and the same address shows what became of it once Dispatch answers. Either page lets you
sign in as someone else.

A decision is accepted only from the page itself, within an hour of opening it, while your sign-in
lasts, and only for the requests that page printed: a page more than an hour old, or a form from
anywhere else, decides nothing and says so. Open the request again. Dispatch cannot decide a secret
request for you: its Approve, from a tab opened before its Inbox linked this page, gets the broker's
`DECIDE_ON_BROKER_PAGE` refusal naming the page.

## Without Dispatch

On the [local stack](/legion/broker/guides/run-locally/) there is no Dispatch Inbox: open the page
`agent-secrets` prints in a browser on the same machine. The stack's stand-in identity provider
signs you in as the request's approver with no page to fill in, and the broker brings you back to
the request. `agent-secrets-devrelay` stands in for Dispatch only for a machine login
(`machine-approve`, with its code): the broker refuses every route a service calls to decide a
secret request.

The list Dispatch reads names each request's page as `approve_url`, and each row's `state`,
`pending` or `expired`, with its deadline as `expires_at`. `curl -H @-` reads the header from
standard input, so the token is in no command line another process can read:

```sh
printf 'Authorization: Bearer %s' "$(cat "$AGENT_SECRETS_ACCOUNT_TOKEN_FILE")" |
  curl -s -H @- "$AGENT_SECRETS_URL/v1/pending?approver=$AGENT_SECRETS_APPROVER&state=pending&state=expired" |
  jq '.pending[] | {record_id, kind, state, expires_at, approve_url}'
```

The [HTTP API reference](/legion/broker/reference/api/) documents the page's routes and that list.

A request has two ids. The request id is the one `agent-secrets` prints and takes (`self`,
`status`, `cancel`, and the exit-75 message); the record id is the one the approver's page and list
name, the last part of `approve_url`. `agent-secrets request NAME --json` prints both.
