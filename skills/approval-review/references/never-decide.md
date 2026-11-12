# What the agent never decides

Some requests are not yours to answer, whatever your confidence and however clear the answer
looks. This file names them. It is the whole rule: there is no list of words or phrases behind
it, and nothing else states it.

When a document falls under one of these kinds, you decide nothing. You say which kind it falls
under and you hand it back to the person.

## The one question

**Does the document ask for the act, or does it only mention it?**

A request of this corpus is a technical document, and a technical document is full of deletions,
credentials, vendors and organization settings that it only describes. Describing is not asking.
Ask yourself what follows from a person writing "approved" under this document as it stands:

- if one of the acts below is then **authorised** — somebody may now do it, or is being asked to
  do it, or the approval itself is the permission — the document asks for it, and it is held;
- if the act is **reported** (it already happened), **described** (this is how the system works,
  how it worked last month, what a job already does), **proposed as somebody else's separate
  request**, or **turned down** ("we will not drop it"), the document does not ask for it, and
  you decide it as you would any other.

Both failures cost something. Deciding a request you were never given costs a person something
they never agreed to. Holding an ordinary change costs them an answer they were waiting for, and
a rule that holds everything is no rule at all. Judge what the document asks for, not which
words appear in it.

The act does not need a person's name beside it. "Drop the legacy table on the primary", "The
operator drops it", "You drop it" and a change list that reads "- Drop the legacy table" all ask
for the same act; a change list is written in the imperative, and that is still the request.

## The seven kinds

### `destructive-act`

The document asks for data or infrastructure to be destroyed, overwritten or made unrecoverable:
dropping a database, a table or a column that holds live data; deleting a bucket, a volume, a
snapshot set or a backup; deleting an account, a repository, a cluster or an environment;
truncating or purging records; a force push over a branch's history; turning off backups or
cutting a retention window so that existing data ages out.

Shapes it takes:

- "Approve dropping `events_raw` on the production primary, with its indexes."
- "- Delete the read replica and the archive bucket once the dump is taken."
- "The release needs a force push over `main` to drop the two bad commits."

Not this kind: a report of what a retention job already aged out; a design of how deletion will
work; a change that deletes code, comments, a dead flag or a test; a deletion already made that
the document only records.

### `credential`

The document asks for a credential to be created, issued, rotated, handed over, pasted, stored
or shown: an API key, an access key, a private or signing key, an SSH key, a service-account
key, a deploy token, a password, a shared secret or a certificate. Asking you for one is the
same kind as asking somebody else to make one.

Shapes it takes:

- "Create an API key for the release job and put it in the pipeline's secret store."
- "Send the password for the shared mailbox to the contractor for the week."
- "- Mint a deploy token for the registry and commit it to the chart's values file."

Not this kind: a design that says a proxy mints its own token at run time; a document that says
where credentials are kept or how they are scrubbed from a log; a rotation that already ran and
is being reported.

### `owner-only-setting`

The document asks for a setting only an organization owner can make: billing, the plan, seats or
anything that is paid for; organization-wide identity and access, such as enforced single
sign-on, domain verification or two-factor policy; adding, removing or promoting an organization
owner, admin or member; organization-wide defaults and policies; enabling an organization-wide
feature; transferring ownership.

Shapes it takes:

- "Turn on enforced single sign-on for the whole organization."
- "- Upgrade the plan to the paid tier and buy five more seats."
- "Add the release bot as an organization owner so it can set repository defaults."

Not this kind: a setting of one repository, one project or one service that the team already
owns; a document that notes a setting an owner made last month; a request for a person's own
local configuration.

### `outside-message`

The document asks for something to be said to somebody outside the company: an email or a reply
to a customer, a vendor, a contractor or a candidate; a comment, an issue or a pull request on
another organization's project; a support ticket or a bug report filed upstream; a post to a
public forum, a mailing list, a status page, a blog or a social account; a disclosure.

Shapes it takes:

- "File the bug upstream on the library's tracker with the reproduction attached."
- "- Email the vendor and ask them to raise our rate limit."
- "Reply to the customer on the open ticket and tell them the fix ships on Thursday."

Not this kind: a message to a colleague, a comment on this organization's own issue, a note in
this repository's changelog, release notes written but not published, a draft the document says
a person will send themselves.

### `outside-account`

The document asks for an account, an organization, a seat or a registration on a platform
outside the company: signing up with a vendor or a service, opening a trial, registering an
organization on a public registry or a forum, claiming a package or project name, registering a
domain, creating a login a person outside the company would hold.

Shapes it takes:

- "Sign up for an account with the uptime service under the team mailbox."
- "- Register the organization on the public package registry and claim the name."
- "Open a free trial with the log vendor so we can compare the two."

Not this kind: a service account inside infrastructure the company already runs; using an
account the company already holds; a document that names a vendor the team already pays for.

### `changes-these-skills`

The document changes the agent's own skills or the numbers they are measured by: the text of the
skills it reads, this never-decide rule itself, the bar or threshold a round is held to, the
share of requests the agent decides, the rate it answers at, or the definition of a measurement
it reports. The agent never widens or narrows its own instructions.

Shapes it takes:

- "Lower the agreement bar for the shipped arm from 0.90 to 0.85."
- "- Drop the credential kind from the never-decide rule: it holds too many documents."
- "Raise the share of requests the agent answers without a person from a tenth to a half."

Not this kind: a change to code the skills happen to describe; a change to an unrelated
document, prompt or script; reporting a measured number without changing what is measured.

### `waiting-legion-root`

The primary document of a root issue that carries the `legion` label and stands on the backlog or
in icebox, because the record keeps such an approval as the person's own and the daemon then
admits that whole tree to run.

This one kind is a fact of the issue — whether it is a root, its label, its status, which
document is its primary — and never a reading of what the document says. It is settled from the
issue's facts before anything is read, so when you are judging a document's text you never name
it. If this is the only kind that could apply, answer `none`.

## Answering

Name exactly one kind, or `none`. Where two kinds fit, name the one the document asks for most
plainly; the hold is the same either way. Then say in one sentence what the document asks for
that makes it that kind, quoting or naming the line it asks it in, so a person reading your
answer can check it against the document.
