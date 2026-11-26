# Deploying the Legion daemon on a shared box

This runbook covers a shared-box deployment: the daemon, its tmux server, and every pane it
launches run as the same Unix user on one machine.
This provides no filesystem boundary between the daemon and its panes.
Kubernetes deployments (`docs/kubernetes.md`) get a pod boundary instead.

The daemon gets GitHub App installation tokens from the secrets broker; the App private keys
stay in the broker.
Raw key secrets are not used.
Configure each App with `app_id` and `token_command`, as [the App section](#the-legion-implementer-and-legion-reviewer-apps) shows.

`secrets` and `agent-secrets` are two CLIs over two stores, and a name can exist in both.
`secrets` is secretsd, shipped from `sjawhar/forward`: it reads its agent tier from local sops
files and its human tier through the local secretsd daemon, which asks for a YubiKey touch.
`agent-secrets` reads the secrets broker, which is backed by a cloud Secret Manager and releases
installation tokens for `github-app` secrets.
The App configuration below uses `agent-secrets`; `provider_keys` can still read secretsd keys.

## How the daemon launcher gets tokens

Start the daemon inside an `agent-secrets` session, so every `token_command` it runs inherits that session:

```sh
export AGENT_SECRETS_URL=<the broker URL>
agent-secrets register --wait 10 --exec -- legion start …
```

The `agent-secrets` helper must be running, with the machine login a person approves through `agent-secrets machine login` ([Log a machine in](site/src/content/docs/broker/guides/log-a-machine-in.md)).
That login must be allowed to request both Apps' broker secrets.
A daemon started from a plain shell is no session: each `token_command` then fails with `NOT_A_SESSION` ([Troubleshooting](site/src/content/docs/broker/guides/troubleshooting.md)).

## The `legion-implementer` and `legion-reviewer` Apps

The `sjawhar` account owns both Apps.
The secrets broker holds them as `github-app` secrets, which release installation tokens and never the App's private key:
`LEGION_IMPLEMENT_APP` (agent tier) and `LEGION_REVIEW_APP` (human tier: each new grant waits for a person's approval in Dispatch, and a later release reuses a live grant only for the same profile and the same target).
A caller asks the broker for a profile of such a secret, never for the secret itself: the broker's profiles file (`BROKER_PROFILES_FILE`, [the airlock](site/src/content/docs/broker/concepts.md#the-airlock)) names each profile, its source secret, its accounts, its permissions and who may ask for it, and the broker refuses a `github-app` secret that no profile names.
Each role's `token_command` names two profiles of its App's secret: the profile of the secret's own name for the write token, and the profile of that name with `_READ` after it for the read token.
The review App's command names `--ttl 12h`, because a human-tier request that names no lifetime gets 15 minutes, and the daemon renews a lease about 40 minutes after it mints one:
without the flag every renewal would wait for a person, and with it the approver reads the twelve hours on the card and decides them for that one profile and target.
The broker reuses a live grant, and joins a waiting request, only for the same profile and the same target, so a daemon of a human-tier App on `token_command` needs one decision per profile and owner:
one for the release of every account its boot reads (no target), and one each for the write and the read profile of each of its N owners, up to 2N+1 every twelve hours.
While one of those requests waits on a person, the broker refuses a request of the same profile for another owner `REQUEST_PENDING`, and the daemon reads that as it reads exit 75: no answer about the App, so it asks again once the first is decided.
A daemon that holds no App key uses both Apps through `token_command`, as the live proofs do for the implement App (`scripts/e2e/lib/github-apps.sh`, `app_token_command`):

```yaml
github_apps:
  implement:
    app_id: "5341240"
    token_command: "P=LEGION_IMPLEMENT_APP${LEGION_GITHUB_READ_ONLY:+_READ}; agent-secrets \"$P\" --reason=legion-daemon-github-app-token ${LEGION_GITHUB_OWNER:+--target \"$LEGION_GITHUB_OWNER\"} -- printenv \"$P\""
  review:
    app_id: "5341243"
    token_command: "P=LEGION_REVIEW_APP${LEGION_GITHUB_READ_ONLY:+_READ}; agent-secrets \"$P\" --ttl 12h --reason=legion-daemon-github-app-token ${LEGION_GITHUB_OWNER:+--target \"$LEGION_GITHUB_OWNER\"} -- printenv \"$P\""
```

The broker's profiles file declares the four profiles those commands name.
Each is `"value": "installations"`, the release the daemon reads, and has no `proxy` block, since the daemon hands its tokens to the panes' `gh` bundles.
The write profiles hold the role's write set and the read profiles the read set, the sets the daemon asks GitHub for when it mints with a key of its own (`writePermissions` and `readPermissions` in `packages/daemon/internal/appauth`), so a lease holds the same permissions either way.
`requesters` names the operators whose machine logins run the daemon, and no pod's service: a pod that held one of these releases would hold the App's whole write token.
The accounts are the accounts of each secret's `github-owners` tag the daemon works on:

```json
{"profiles": [
  {
    "name": "LEGION_IMPLEMENT_APP",
    "requesters": ["operator@example.com"],
    "github": {
      "accounts": ["example-org", "example-user"],
      "permissions": {
        "actions": "write", "attestations": "read", "checks": "read", "contents": "write", "issues": "write",
        "metadata": "read", "pull_requests": "write", "security_events": "read", "statuses": "write", "workflows": "write"
      },
      "value": "installations"
    }
  },
  {
    "name": "LEGION_IMPLEMENT_APP_READ",
    "source": "LEGION_IMPLEMENT_APP",
    "requesters": ["operator@example.com"],
    "github": {
      "accounts": ["example-org", "example-user"],
      "permissions": {
        "actions": "read", "attestations": "read", "checks": "read", "contents": "read",
        "issues": "read", "metadata": "read", "pull_requests": "read", "statuses": "read"
      },
      "value": "installations"
    }
  },
  {
    "name": "LEGION_REVIEW_APP",
    "requesters": ["operator@example.com"],
    "github": {
      "accounts": ["example-org", "example-user"],
      "permissions": {
        "actions": "write", "attestations": "read", "checks": "write", "contents": "write", "issues": "write",
        "metadata": "read", "pull_requests": "write", "security_events": "write", "statuses": "read"
      },
      "value": "installations"
    }
  },
  {
    "name": "LEGION_REVIEW_APP_READ",
    "source": "LEGION_REVIEW_APP",
    "requesters": ["operator@example.com"],
    "github": {
      "accounts": ["example-org", "example-user"],
      "permissions": {
        "actions": "read", "attestations": "read", "checks": "read", "contents": "read",
        "issues": "read", "metadata": "read", "pull_requests": "read", "statuses": "read"
      },
      "value": "installations"
    }
  }
]}
```

A profile is never looser than its source: its `requesters` are among the secret's own `requesters` tag, where the secret has one, and a profile of the human-tier `LEGION_REVIEW_APP` is human tier too.
`packages/daemon/internal/appauth/deployment_doc_test.go` runs both commands against a stand-in `agent-secrets` and holds these profiles' permissions to the daemon's sets.

The live proofs take the review App's tokens that way no longer.
They resolve that App's key on the driver, from the operator's `LEGION_E2E_REVIEW_APP_KEY_COMMAND`, into a key file of the run's own, and give the App a `private_key_command` that reads it, while the implement App keeps the `token_command` above (`scripts/e2e/lib/github-apps.sh`, `write_github_apps`).

The daemon runs the command with `sh -c` whenever it needs a lease: at boot, and again once fewer
than 20 minutes remain on a lease it holds.
At boot it runs each App's command once to read the App's login and once more to prove its key.
A login read that fails after the release, and is tried again, reads under the same token until
fewer than 20 minutes remain on it, and an App whose login was read is not read again.
The command's environment is the daemon's, less the NATS seeds and every variable whose name starts
`LEGION_GITHUB_`, with two variables the daemon sets:
`LEGION_GITHUB_OWNER`, the account whose installation token the daemon wants (empty for every
installation the command may release, which the boot's read of the App's login asks for), and
`LEGION_GITHUB_READ_ONLY`, `1` for a read-only token and empty otherwise.
The command maps the first onto `--target`, which narrows the profile to that one account, and the second onto the name of the read profile.
The profiles above name no repositories, so a token reaches every repository the account's
installation holds: the account chooses those when it installs the App, and the daemon asks for no
narrowing by repository.
Its stdout must be the broker's release of a profile whose `value` is `installations`,
`{"installations":{"<account login>":{"installation_id":<id>,"token":"ghs_…","expires_at":"<RFC 3339>"}}}`,
and the daemon takes the owner's entry, matched without regard to case.
An owner the command releases nothing for is an App not installed there: the broker refuses an account the App has no installation on, and the daemon reads a refusal for an account outside the profile the same way once a release of every account has not named it.
With no key, the daemon reads each App's bot login from GraphQL's `viewer` under one of its
tokens, refuses a token whose `GET /apps/<slug>` names an id other than `app_id`, and reads whether
the App is installed on a repository from that owner's installation's repository list.
A read-only token holds what the read profile names, cut to what the App's installation holds:
the broker asks GitHub for each permission the installation holds at the level it holds it, leaves
out one it holds none of, and `agent-secrets` names each one left out on its stderr.
The read profiles above name the daemon's own read set, `contents`, `metadata`, `pull_requests`,
`issues`, `actions`, `checks`, `statuses` and `attestations`, each at `read` (`readPermissions` in
`packages/daemon/internal/appauth`), which a configured key asks for too, cut the same way to what
the installation is granted before each mint, so a controller reads the same through either.
The daemon passes the command's stderr through as it runs, with every GitHub token in it redacted,
so `agent-secrets`' line naming the pending request appears in the daemon's log while the review
App waits for its grant.
A run is not cut off by the boot's 15-second attempt: the boot waits for it, logging that it waits,
and the next attempt joins the same run, so a grant that waits on a person's approval is one
request; `agent-secrets`' exit 75 (still waiting when its own wait ended) is retried, and so is
the broker's `REQUEST_PENDING` (another owner's request of the same profile still waiting).
`legion config check` never runs it, and `installations` cannot be set beside it, since
the release names every installation.

Both Apps are public GitHub Apps, so any GitHub account can install them.
What such an install gives depends on where the daemon gets its tokens.
A daemon that gets its tokens from the secrets broker through `token_command`, as this runbook configures it, is bounded by the broker: the broker releases a token only for the accounts the profile names, each an account of the secret's `github-owners` tag, so an install by any other account gives nothing.
A daemon that mints with its own App key counts every installation and keeps no list of owners.
There, an account that installs the implement App gets a directory with that App's write token in the gh bundle of every implementer and merger, and an install of the review App gives every other role that App's token the same way and the controller its read-only one.
The daemon reads the installations at every bundle it builds, so a running pane holds the new directory within a minute, and loses it within a minute of an uninstall.
The one exception is the account that holds the pane's own issue repository: that directory cannot be left out, and its token is that owner's cached lease, which stays in use until its expiry is no more than `refreshWindow` (20 minutes, `internal/appauth`) away.
Until then every bundle built for the pane succeeds and the refresh keeps writing all of its directories, that one holding a token GitHub would no longer mint.
The first build that mints that owner for real fails, naming the owner; from then every bundle built for the pane fails, no refresh writes any of its directories, and the pane keeps its last tokens until they expire.
The controller's default owner is refused at once instead, because `ControllerCredential` reads the installations before it mints anything.
The account's name is among the owners that each later launch of that App's roles is told in its `Legion addressing` line.
What an install changes for a running pod is in `docs/kubernetes.md`.

Each App needs its own `app_id` and `token_command`, with no second credential source.
See [Rotating broker-held App keys](#rotating-broker-held-app-keys) for the rotation procedure.

## What a pane's `gh`, `git` and `jj` are

A tmux pane's `PATH` is the daemon's own with the state directory's `bin/` put first: its `legion`
launcher, its `gh`, a script that runs `legion gh`, its `git`, a script that runs `legion git`, and
its `jj`, a script that runs `legion jj` (below). The daemon resolves at boot a
jj of 0.38 or later — the jj bound it refuses to boot below (`resolveTools`, naming
`LEGION_JJ_PATH`, its own override for the jj it runs) — and the real `gh` (`LEGION_GH_PATH`, or the
first `gh` on its `PATH`, refused when that is a script), which every pane is told as `LEGION_GH_PATH`. A pane's GitHub identity is
a set of files that binary reads, one directory per GitHub owner the role's App is installed on,
one more per owner a configured rename moved a repository away from while the project serves the
name it moved to, and one for the issue's own recorded owner: the
daemon writes gh's own `hosts.yml` (an App installation token, user `x-access-token`) and
`config.yml` under `<state_dir>/secrets/<claim>-gh/<dir>` (0700, files 0600) when it opens the
pane — in every directory the role's App token on that owner's installation, asked for with the
role's own permissions, the implement App's write set or the review App's review set, and in no
directory a token narrowed to reading, the one directory with another owner's token being that
of an owner a rename moved a repository away from where the App is not installed on it, which
holds the token of the owner the repository moved to (each token asks for what the App's
installation on that owner is granted of the set, and a permission it is granted none of is left
out, named in a warn record of the daemon's log) — names the directory the issue's repository
routes to as `GH_CONFIG_DIR`, its recorded owner's or, for a repository a configured rename
moved, that of the owner it moved to,
and the bundle's root as `LEGION_GH_OWNERS_DIR`, with `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` set
empty so nothing outranks the files, and rewrites each directory's `hosts.yml` in place every
minute from its cached leases (the log lines `tmux runtime: github credential written` and `github
credential refreshed`, with the claim, role, App, `expiresAt` and the directories written); a pane
it adopted after a restart is refreshed for the repository its claim's issue works, read from the
daemon's own state, never one read from the pane's directory. Each of those tokens reaches every
repository its own installation holds, which the account chose when it installed the App, and the
daemon asks GitHub to narrow none of them to a repository. The bundle's root also holds one file
of its own, `renames.txt`, the configured renames the project's pairs resolve to, which both
wrappers route a repository a rename moved by and read from there and from nowhere else
(`ghconfig.PairsFile`). `legion gh` runs the real gh with
`GH_CONFIG_DIR` at the one directory every repository the command targets is routed to: its
owner's, or for a repository a configured rename moved, the directory of the owner it moved to,
each repository routed on its own and an owner the command names alone routed to its own
directory, the targets read as gh reads them, with pflag's rules, from the real gh's own help of
each command down to its leaf (`-R`/`--repo` before or after the command, a repository or owner
argument, before `--` or as a clone's or fork's first after it, a `github.com` URL argument — for a
`pr` command only a pull request's, since gh reads any other as a branch of the command's own
repository — every one of a slot gh's usage takes several of, a `repos/<owner>/…` endpoint of
`gh api` or a search endpoint's `q`, a search
query's plain `repo:`/`owner:`, the options naming a repository, an owner, a team or an issue by
URL). The command's own repository counts too whenever gh resolves it, because no argument names
it, and for `repo set-default`, which reads every remote's network, whatever names it: `GH_REPO`
for a command that reads it (`gh api`'s `{owner}` and `:owner` placeholders among them), and
otherwise every `github.com` remote and `gh-resolved` setting of the working directory, read with
gh's own URL parser, whatever the scheme. One working directory counts `origin` alone: one whose
only `gh-resolved` line gh reads is `remote.origin.gh-resolved base`, on an `origin` whose fetch
URL gh keeps as `github.com` and is not ssh, for a command of the 14 groups gh resolves through
that marker (`agent-task`, `api`, `browse`, `cache`, `discussion`, `issue`, `label`, `org`, `pr`,
`release`, `repo`, `ruleset`, `run` and `workflow`, as gh's `pkg/cmd/root/root.go` builds them;
`repo set-default` only with `--view` or `--unset`), because gh itself acts on `origin` there. A
command naming no owner, in a directory whose remotes name none, runs at the issue owner's. A
command whose last `--help` is true, and `gh --version` and `gh -h`, each alone, only print. It fails closed: a
command it cannot read for certain (an option the leaf's
help does not list or names a repository with in a way it does not know, a subcommand gh does not
know, a first word gh's own help does not list, anything before the command but `-R` and
`--help`, an argument the leaf's usage has no place for or names in a way it does not know, a
search qualifier gh or GitHub could read otherwise, a gh alias or an extension, run or named as
`gh help`'s target, whose help it never reads since reading it runs them, `gh copilot` and `gh
extension exec`), one whose repositories route to two directories (two owners' repositories, an
option or a second repository naming one owner while gh acts on another's, a workspace whose
remotes name two but for the marked `origin` above, or a repository a configured rename moved
beside another of the owner it moved from, which run under one directory's token would answer for
the other's with less than GitHub holds and no error; a moved repository named by both of its
names is one directory), one naming an owner the bundle holds no directory for but for the read
below, one naming a
repository a configured rename moved to an owner the bundle holds no directory for, which names
the pair, every command where `PATH`'s first `gh` is not
Legion's wrapper (a script
that execs `legion gh` by its absolute path), and every command under a `GH_HOST` other than
`github.com` or a set `GH_TOKEN` or `GITHUB_TOKEN` are refused with exit 2, naming the argument,
and send nothing.
A read that names only owners the bundle holds no directory for runs, rather than being refused,
with the token of `GH_CONFIG_DIR`, the directory a command naming no owner runs with
(`selectors.dir`): a call `ghWritesNothing` admits, or `gh api graphql` in the one form
`ghGraphQLQueryRead` reads for certain (`api graphql` typed first, its query given with
`-f query=…` and holding the words `mutation` and `subscription` nowhere, in any case and inside a
string or a comment too, its other fields given with `-f` or with `-F` values that read no file
and none of them with a key that holds `query`, no `-H` naming a method override or
`Content-Length`, and no option but `-f`, `-F`, `-H`, `-q`, `-t`, `--paginate` and `--slurp`, by
either spelling, each value in its own word), where no repository it names is one a configured
rename moves and no `github.com` remote of the working directory was read for it.
The GraphQL rule reads no GraphQL syntax: GitHub's lexer (graphql-ruby's `BLOCK_STRING_REGEXP`)
ends a block string at other quotes than the GraphQL specification does, and a document runs a
mutation or a subscription only through that word, so one that holds neither word writes nothing
however a lexer reads it.
An installation's token reads public data and that installation's own repositories alone, and
GitHub answers 404 for another owner's private repository. Every other command naming such an
owner is refused as above.
It refuses the same way, before gh runs: `--editor`/`-e` on any command, because gh runs the
editor after the leak guard has read the call and posts what it wrote; `gh gist edit`, which opens
an editor unless a file is named; `gh agent-task`, which sends the token to the Copilot API host
GitHub names; `gh auth login`, `logout`, `refresh` and `switch`, which write the gh directory the
daemon alone writes; `gh auth setup-git`, which writes git's global configuration, a file the git
`legion gh` runs never reads; `--recurse-submodules` on any command (`gh pr checkout`'s), which
runs `git submodule update`, whose fetch reads each submodule's own configuration; and any git
flag `gh repo clone`, `gh repo fork` or `gh gist clone` would hand git clone after `--` but a
directory and these, spelled exactly: `--quiet`/`-q`, `--verbose`/`-v`, `--progress`,
`--no-progress`, `--depth`, `--shallow-since`, `--shallow-exclude`, `--single-branch`,
`--no-single-branch`, `--no-tags`, `--branch`/`-b`, `--origin`/`-o`, `--filter`, `--sparse`,
`--no-checkout`/`-n`, `--bare` and `--mirror`. A clone's other flags set its configuration
(`-c`/`--config`, which git also takes abbreviated, and a URL-scoped `sslVerify=false` given so
sent the token to a proxy), its hooks (`--template`), its submodules or where its objects come
from (`--reference`, `--separate-git-dir`, `--upload-pack`). It refuses as well a gh directory
whose `hosts.yml` or `config.yml` is no regular file
(a named pipe, a symbolic link, a directory) or is larger than any the daemon writes, which it
reads without following a link or waiting on a pipe, or names `api_host` or `http_unix_socket`
anywhere gh would read it as a key, a list's items among them; a `config.yml` that holds a `hosts`
block or an alias named `help`, which gh would run on `legion gh help`; and a working directory
whose repository configuration sets an `http` ssl or proxy key, generic or for a URL, but an
`sslVerify` that is true, or a credential helper other than `legion git-credential`, since git
reads it before the command line and a URL-scoped `sslVerify=false` there beats every entry the
command line forces; or any include (`include.path`, `includeIf.<condition>.path`, whatever its
condition and whether or not its file is there), or `extensions.worktreeConfig` on, since a git
gh runs later, after a checkout or in another worktree (`pr checkout --worktree`, `pr merge
--delete-branch`), would read a configuration the check did not. With no `git` on `PATH` that
check reads nothing, since gh then runs no git either.
`legion gh` starts the real gh for a command's help only the first time:
it keeps each help in `$XDG_STATE_HOME/legion-gh/help`, one file for each command path and each set of values of the `GH_*`, `CLICOLOR`, `CLICOLOR_FORCE`, `NO_COLOR`, `TERM` and `COLORTERM` variables gh is handed, which nothing removes.
It writes each copy as a file of mode 0600 in a directory of mode 0700, and reads a copy only where neither it nor its directory is a link and the role's user owns both, with no permission for their group or for others.
In a pod every role runs as one user, so this keeps out another user and not another role.
It reads the help from gh again when one of these changes: the gh binary, the file the `LEGION_GH_PATH` path's links end at (its path, device, inode, size, modification time, change time or mode); `config.yml` or `hosts.yml` in `GH_CONFIG_DIR` (gh lists its aliases in its help, and prints no help when either file does not parse); gh's extensions directory; or one of those variables.
Where the links end at a file not named `gh`, such as the `mise` binary a mise shim links to, which picks the gh it runs on each run, it keeps nothing and reads every help from gh.
The leak guard's own visibility reads and `gh api rate_limit` read no help at all.
Under a set `GH_TOKEN` or `GITHUB_TOKEN`, `legion gh` runs these calls and refuses every other one:
the leak guard's own two visibility reads, each exactly for the repository `PUBLIC_LEAK_CHECK_VISIBILITY_READ` names, with that token;
the shared check's own visibility read, which `legion gh` answers itself with the guard's read;
the exact `gh api rate_limit`;
and `gh auth git-credential`, which answers from the bundle.
The guard's reads use the caller's token because a push made with the token writes the repository they read,
and the bundle may hold no directory for that repository's owner.
Without such a token, the leak guard's own REST read of a repository whose owner the bundle holds no directory for is refused with the line that names that owner,
`legion gh: the role's GitHub App is not installed on "<owner>": <LEGION_GH_OWNERS_DIR> holds no gh directory for it`, exit 2,
and the guard then reads that repository's visibility with an unauthenticated `git ls-remote`, which makes no API request:
its exit 0 alone reads public, and any other outcome refuses the write as a setup error naming both reads.
The guard makes no unauthenticated REST read of it, because GitHub allows 60 of those an hour to one address.
The shared check's own visibility read of such a repository, which `legion gh` answers with the guard's read, reads it the same way, so the check counts a public one's advertised commits as published.
The refusal of a repository a configured rename moved to an owner the bundle holds no directory for names the pair around that line, and stays a setup error.
The real gh runs with `GH_PATH`
naming the pane's wrapper, so the git gh runs itself (a clone, the fetch of a fork's parent, `pr
checkout`) authenticates through the helper gh names after it, `legion gh auth git-credential`,
which answers as `legion git-credential` does, with `credential.useHttpPath` on for `github.com`;
and with gh's update check, prompts and telemetry off and its editor a program that fails
(`/bin/false`), whatever the caller names. Its environment is built from an allowlist of the
caller's (`ghChildEnviron` and `ghChildKeeps`, `packages/daemon/cmd/legion/gh.go`): the paths,
the terminal, the locale, the proxy variables, gh's state directories, `GH_REPO`, how gh prints,
git's identity, and Legion's wrapper variables, each kept for a read gh or go-gh makes of it. No
trust store, editor, browser or pager variable of the caller's reaches gh: a proxy sees only the
TLS tunnel gh opens through it, and cannot read the token. A deployment whose proxy inspects TLS
with a CA of its own must install that CA in the system's trust store, which gh, git and curl read
by default; naming it in `SSL_CERT_FILE` or `SSL_CERT_DIR` reaches a pane, but not the gh `legion
gh` runs, and gh's requests then fail certificate verification. The git gh runs reads no global
or system configuration file (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`), takes of
the caller's `GIT_CONFIG_*` pairs only those the runtime sets, and gets `http.sslVerify=true` and
submodule recursion off (`submodule.recurse=false`, `fetch.recurseSubmodules=false`,
`push.recurseSubmodules=no`): git's default fetch recurses on demand into a submodule whose
gitlink the fetched commits move, with that submodule's own configuration, which no check reads.
With the global and system files out, a clone of a Git LFS repository through `legion gh` gets no
`filter.lfs` (git-lfs registers it there) and leaves pointer files; no Legion caller clones one.
The saved help its tests read is the image's gh's,
with that gh's whole command reference, every argument and every repository-naming option of
which the wrapper's tables know (`packages/daemon/cmd/legion/testdata`, with the script that
regenerates it when that pin moves; `.github/scripts/check-gh-version.sh` fails the build until
the saved help is the pin's).
The shared clone's `credential.helper` is `!legion git-credential` with `credential.useHttpPath`
on, so a pane's `git` and `jj git push` read the hosts.yml of the owner of the repository in the
path git asks about — of the owner a configured rename moved that repository to where a pair
names it — and nothing else, but for a repository of an owner the bundle holds no directory
for. The helper reads such a repository from GitHub once, with the token of the directory
`GH_CONFIG_DIR` names where that is a directory of the bundle. Where GitHub answers that it is a
fork, the helper answers the token it would answer for the fork's `parent`, or else for its
`source`, where the bundle holds that owner's directory. That repository is usually the base of
the fork's pull requests, whose token GitHub takes for a maintainer's commit to the branch of such
a pull request from a fork in a personal account, and GitHub decides which branch of the fork
that token may update. Every other repository of such an owner gets nothing.
Every provision, a pane's and a pod's, also writes `remote.origin.gh-resolved base`, gh's own
default-repository marker, into the clone's own configuration where that configuration holds no
`gh-resolved` line, so a worker who adds an `upstream` remote keeps gh's choice, and `legion gh`'s,
on the issue's repository; a marker a person set, or one a renamed remote carries, stays.

In a pod, the sweep that knives runs after each write to a fork's shared ledger (`knives ledger
sweep`) pushes this machine's own ref, `refs/knives/<machine>`, to the fork's ledger repository
with plain git, from a directory that is no clone. The git configuration entries every role is
given (`gitConfigPairs`, [Anatomy of a Sandbox pod](kubernetes.md#anatomy-of-a-sandbox-pod)) name
`!legion git-credential` there too, so it answers that push as it answers the shared clone's, and
no person's token is used.

Oh My Pi holds a pane's `PATH` when `omp_invocation` runs it through mise. `mise x` rewrites the
`PATH` of the program it starts: it puts the tool's own directories and each `_.path` entry of the
mise config it reads ahead of the rest, and such an entry, a dotfiles `shims` directory for example,
can hold a `gh`, `git` or `jj` of its own. So the daemon has `mise x` start `/bin/sh` in place of
`omp`, and that shell puts the first entry of the `PATH` the launch ran under, the state directory's
`bin/`, back in front, then execs the tool's own `bin/omp`, so the process mise started is Oh My Pi
itself (`keepPathHead`, `packages/daemon/internal/omplaunch`). The rest of the `PATH` stays as mise
made it, with `bin/` in it a second time where mise left the daemon's own. Every boot probe runs Oh
My Pi the same way. So does `legion controller start`, whose launch `PATH` begins with the `bin/` of
its own state directory, and whose mise reads the operator's own config, since the controller keeps
the operator's `HOME` and `XDG_CONFIG_HOME`. `LEGION_OMP_PATH` names `omp` directly, with nothing
between the launch and Oh My Pi, so Oh My Pi holds the launch's `PATH` unchanged.

The `git` in `bin/` runs `legion git --after <state_dir>/bin` (`packages/daemon/cmd/legion/git.go`).
The daemon writes it at boot, unless the first `git` on the daemon's own `PATH` is the worker
image's wrapper, `/opt/legion/bin/git`, which hands a commit to `legion git` itself. In that case
the daemon writes no `git` and removes one that an earlier boot wrote.
For a `git commit`, `legion git` adds two trailers: `Omp-Session`, with the session id that the
`dispatch` command acts as, and `Legion-Role`, with the pane's `LEGION_ROLE`. It adds each line
only when its value is valid, and only when git alone would accept the commit's message and the
two lines change nothing else.
It adds none when the commit's caller asks git for an isolated configuration, as hermetic tests
do: `GIT_CONFIG_NOSYSTEM` set to a value git reads as true, or `GIT_CONFIG_GLOBAL=/dev/null`.
It then runs the next `git` on `PATH` after `bin/`, a host's own git wrapper included, and it runs
every other command through that `git` unchanged.
Where a host's git wrapper runs `bin/git` again, as a wrapper that searches `PATH` for the next
`git` can, that second run goes on past each `git` the first runs ran, a script or a compiled
program, and adds nothing, so the loop ends. Where the `git` programs on `PATH` only run one
another and `bin/git`, it stops them with exit 127 and says so.
The trailers need git 2.32 or later, the first git with `git commit --trailer`.
`legion git` reads the version of one `git`: the first `git` on `PATH` after `bin/` that is a
compiled program and not a script. When that `git` is older than 2.32, the commit gets neither
line.
A host's git wrapper script can run a different `git`, and `legion git` does not read the version
of that `git`. When that different `git` is older than 2.32, it refuses the two `--trailer`
options with its unknown-option error, so the commit fails, although the same commit succeeds
without `bin/git`.
Every `git` command of a pane runs through `legion git`, so `git` can die of a signal that its
caller ignores.
The Go runtime puts its own handler over an ignore that the caller set, for each signal that it
handles except SIGHUP and SIGINT.
It does not handle SIGTSTP, SIGTTIN or SIGTTOU.
The exec sets each handled signal back to its default action.
So `git` dies of an ignored SIGTERM, SIGPIPE or SIGQUIT, for example, but an ignored SIGHUP,
SIGINT, SIGTSTP, SIGTTIN or SIGTTOU stays ignored.
`legion controller start` writes the same `git` into the `bin/` of its own state directory, by the
same rule, for the operator's controller.
The two lines record which session and which role made a commit, and they guard nothing.

A release older than the first one that writes `bin/git` leaves `<state_dir>/bin/git` in place,
and that file names a `legion` that has no `git` command, so every `git` command in a pane fails.
So before you start the older daemon, remove `<state_dir>/bin/git`. Before you run an older
`legion controller start`, remove the `bin/git` of its state directory too.

The `jj` in `bin/` runs `legion jj --after <state_dir>/bin` (`packages/daemon/cmd/legion/jj.go`).
The daemon writes it at boot, unless the first `jj` on the daemon's own `PATH` is the worker
image's wrapper, `/opt/legion/bin/jj`, which expands each command as jj does and hands each one
that it expands into a `jj git push` to `legion jj` itself. In that case the daemon writes no
`jj` and removes one that an earlier boot wrote.
`legion jj` runs the next `jj` on `PATH` after `bin/`, a host's own jj wrapper included.
It reads a command as jj 0.45 and 0.46 do (`packages/daemon/cmd/legion/jj_expand.go`): an
alias's words in place of its command word, an alias of an alias the same way, and
`ui.default-command` in front of a command that names no command, or that an alias of options
alone leaves with none, each read from the configuration of the working directory, as jj reads
them.
A command word is a command's first word that is no option and no option's value.
These commands make no read of the aliases: one whose command word is a built-in jj command,
`jj git push` among them; `-h`, `--help`, `-V`, `--version` or a cluster of short flags that
starts with `-h` or `-V`, with no command word; any command while `COMPLETE` is set for shell
completion; any command of a process with no table, such as the controller; and a command that
comes back into `bin/jj` with the words `legion jj` ran it with (below).
Any other command, a bare `jj` included, first costs one `jj config list` process (below).
A push that the step runs on costs one read of the push's configuration as well, unless the read
of the aliases expanded it and it names no `-R` (below).
Every command that jj does not run as a `jj git push` runs through that `jj` with the role's own
words, byte for byte.
Where `legion jj` cannot expand a command, it prints one line,
`legion jj: could not expand <what>: <why>; the command runs as written`, and the command runs
with the role's own words and no step: an alias that is no list of strings, a table-form alias
with `enabled = false`, a cycle of aliases, a `ui.default-command` that is neither a string nor a
list of strings, a command word that is no bare name where the configuration names an alias by a
quoted name, and a read that fails or prints what is no name and JSON value.
That read, the read of a push's configuration (below), and each read of the worker image's
wrapper, print what jj prints on standard error but the warnings that jj prints on every command,
such as `Cannot define an alias that overrides the built-in command`, and jj's note of a key that
a read names and no configuration sets (`No matching config key for`), which only a read prints.
A read of the aliases or of `ui.default-command` that fails leaves out jj's error too, and the
line after it says that the read failed.
So a warning that jj prints once, such as `Your repo appears to have been copied`, prints once
where the read is the command that takes it, and a warning that jj prints on every command prints
as often as jj alone prints it for the command that runs after the read.
In a pod, jj itself prints each warning of its aliases, such as
`Cannot define an alias that overrides the built-in command`, twice for each command, and three
times for one with a `-R`: the wrapper puts its `--config` on every line it runs, and jj expands a
line, loading its aliases and printing their warnings, once, again for a `--config`, and a third
time for a `-R` (its `warn_if_args_mismatch`).
jj's other warnings of every command come from no such expansion: a deprecated setting's prints
once for each command, and that of a `trunk()` that does not resolve once for each load of the
workspace, in a pod as in a pane.
The expansion has four limits.
An alias that a `[[--scope]]` defines for `jj config` alone is expanded as the worker image's
wrapper expands it, so its push runs with the step where jj alone refuses it.
A table-form alias with `enabled = false` runs as written, in a pod as in a pane: jj 0.45.1-sami
pushes it with no step, and jj 0.46.0, which the worker image pins as 0.46.0-sami, refuses it with
exit 2 and pushes nothing.
jj 0.38 refuses every table-form alias, that one included; the pane expands one without
`enabled = false`, so its push runs with the step there.
jj 0.38 refuses an alias of options alone used twice, which the pane expands, so the step runs and
jj 0.38 then refuses the expanded line for its option given twice or, for an option it takes
twice, pushes it; and it refuses an alias of options alone that leaves no command word, which the
pane and the worker image's wrapper expand with `ui.default-command` in front, so where that is a
push, it runs with the step.
Before a `jj git push`, and before a command that jj expands into one, `legion jj` runs the push
step on the push as jj expands it.
In a pane, the step's first command reads the configuration of the repository that the push
loads, `jj --ignore-working-copy --color=never config list -T '""'` with the push's own `-R`
arguments and the `--config` and `--config-file` arguments that jj keeps for the push: it takes
no snapshot, adds no operation, and prints what jj prints once there, such as
`Your repo appears to have been copied`.
The step's other reads print nothing of what jj prints on standard error, so without that read
the first of them would take such a warning from the role.
In a pod, the worker image's wrapper's read of `git.executable-path`, with the same arguments, is
that command.
A push that the read of the aliases expanded makes no such read unless it names `-R`: the read of
the aliases loaded the same repository.
A read of the push's configuration that fails stops the push with jj's error and exit status, as
jj alone fails on the same configuration.
The step decides from reads with `--ignore-working-copy`.
Where the push takes a snapshot of its own, the step first compares what git did in the
workspace with what jj last imported, with reads that import nothing: git's `HEAD`
(`git rev-parse HEAD`) with the first parent of `@` (`jj log -r 'first_parent(@)'`), to which jj
sets git's `HEAD` after each of its commands, and git's local branches
(`git for-each-ref refs/heads`) with jj's `@git` bookmarks (`jj bookmark list --all-remotes`).
It compares no other ref of git's, such as a tag or a remote-tracking branch.
An unborn `HEAD` where the first parent of `@` is the root commit counts as equal, and a
conflicted `@git` bookmark as a difference.
A workspace whose root (`jj workspace root`) holds no `.git`, such as an issue workspace under jj
0.38, which colocates none, has no git working tree and compares nothing.
Each git read runs the first `git` on `PATH`, the worker image's wrapper in a pod and `bin/git` in
a pane, with `-C` and the workspace's root; one that fails, but for an unborn `HEAD`, counts as a
difference, since the snapshot that follows is the one the push takes anyway.
Where git moved neither `HEAD` nor a local branch and the step's first run finds nothing of the
push to change, no commit and no word, or where the push itself carries `--ignore-working-copy`,
every jj command of the step runs with `--ignore-working-copy`, so it takes no snapshot, and where
it found nothing to change it adds no operation; a role's own `--ignore-working-copy` goes to the
real push alone, and the step then compares nothing.
Otherwise the step takes the one snapshot of the working copy that the push would take itself,
with the push's own `-R` and `--quiet` arguments and the `--config` and `--config-file` arguments
that jj keeps for the push, and passes on what that snapshot prints but the warnings that jj
prints on every command, which the push prints itself.
It takes that snapshot before its first read where git moved `HEAD` or a local branch, so that
read sees what git did, and otherwise before it changes a commit or a word of the push, after
which it reads what the push sends again and acts on no commit id it read before the snapshot.
Each of its other commands runs with `--ignore-working-copy`, and so does the real push, which
then takes no second snapshot: every id the step reads after its snapshot, `@` and each
descendant of `@` included, stays valid through the push, a warning that a snapshot prints, such
as jj's `Refused to snapshot some files`, is printed once, and a write that another process makes
after the step's snapshot stays in the working copy for the next jj command.
Those ids stay valid unless another jj command, such as a `jj st` in the same workspace, rewrites
those commits between the step's read and the real push.
The daemon tells every workflow role's pane `LEGION_ROLE_IDENTITIES`, the git identity of each
role of the issue as one JSON object (`docs/kubernetes.md`, contract entry `20261125161436`).
The step reads what the push sends from a dry run of the same push, and reads the author and the
first `Legion-Role` trailer of each of those commits.
jj keeps each of `--config` and `--config-file` only from the deepest of the three places of the
command line that holds one, in front of `git push`, between its two words and after them, so
every command of the step carries only those.
The dry run's own `--config ui.quiet=false` goes at the end of the place that holds the push's
kept `--config`, or, where the push has none, right in front of the dry run's own flags, which go
in front of a `--`; so jj keeps both and reads it after each `--config-file` there or in front of
it; only a `--config-file` at a deeper place that sets `ui.quiet = true` silences the dry run,
which the step then cannot read.
Where a commit's author is not the identity that the table gives the trailer's role, the step
runs `jj metaedit` for each such role, under that role's identity, which sets the author.
It prints one line for each commit it rewrote,
`legion jj: <role>: <old commit id> -> <new commit id>`, and then runs the push with the role's
own arguments, but for a revision argument whose meaning the rewrite changed, and with
`--ignore-working-copy` in front of them where the step took its snapshot.
Before it rewrites anything, the step has jj resolve each argument of `--change`, `--named` and
`--revisions` to the whole set of commits it names, and reads which of its mutable commits that no
remote bookmark holds are hidden and which is the working-copy commit; a hidden commit whose change
has one visible commit stands for that commit, and for `@` where that commit is the working-copy
commit.
The set the push must send is that set with each commit the step rewrote or rebased, and each
hidden one that stands for another, replaced by the commit that stands for it.
Where the step rewrote nothing, every argument reaches the push as written, but for one that names
a hidden commit standing for another, which gets that set as below.
Where it rewrote something, every argument is held to its set, whether or not its own commits
moved: one reaches the push as written only where jj, resolving it again after the rewrite,
resolves it to that set, such as `@`, a bookmark or a change id of a rewritten commit.
Any other argument can name other commits once the step has rewritten some, as a change offset
`<change>/<n>`, `latest(...)`, `mine()` or `author(...)` can, even through another argument's
rewrite, so the push gets the set as ids joined by ` | `, `@` for the working-copy commit, while
that word is under 100,000 bytes.
Where it would not be, the push gets the primary form, `(<the argument as written>) [~ (<the old
ids>)] [~ (<the ids it now names and must not>)] [| <the new ids>] [| <the ids it must send and no
longer names>]`, with each empty part left out, where that word is under 100,000 bytes and jj,
resolving it once more, finds that it names the set.
Else a `--change` or `--revisions` argument becomes as many words of that same option as keep each
under 100,000 bytes, each with a part of the ids, since `jj git push` pushes the union of a
repeated option; `--named` names one commit.
Each argument that gets the ids, in one word or several, gets one `legion jj:` line that says so;
an argument that named no commit before the step gets `none()`, and its line says that.
No word the step writes passes 100,000 bytes, but for a role's own argument that is that long
already, so none passes Linux's 131,072-byte limit on one argument.
Only where the push would then pass Linux's 2,097,152-byte limit on a command's arguments and
environment together, with each revision argument counted at the larger of its own word and the
ids of its whole set, does the step decide, before any rewrite other than the fill of an empty
author that a dry run refused, and, where no such fill ran, before its snapshot, to set no other
author:
it prints one `legion jj: could not set the author:` line that names the argument, the count and
the limit, and the push runs as written, but where that fill ran, after which each revision
argument is held to its set as above and the push carries `--ignore-working-copy`.
The step's own `jj metaedit` runs in as many parts as keep each command under that limit, each
naming its commits by the ids the step followed them to after the part before it.
Each of the step's reads of a list of commit or change ids runs once for each part of the list
under 100,000 bytes, so a push of thousands of commits still runs.
It changes no commit that a remote bookmark holds and no immutable commit.
It sets the author of no commit that has no `Legion-Role` trailer or whose role has no identity in
the table.
jj rebases such a commit when it sits above a commit that the step rewrote, which gives it a new
commit id and the step's identity as committer, and it keeps its author.
A commit with an empty author whose trailer names a role of the table gets that role's author
too, and `jj git push` still refuses one with no such trailer.
The step adds no refusal of its own.
A jj command of the step that fails stops the push with jj's error and exit status, but for a dry
run that it cannot read (below), a read of a revision argument that jj refuses, and a rewrite
that jj refuses because its commit exists already, where that commit stands for one the push
names (below).
An argument whose set jj refuses to read goes to the push as written; one whose set the step has
read and that jj then refuses to read again goes as its ids, with its one printed line.
Where it cannot read the dry run, it prints one line, and the push runs as written, but where a
metaedit of the step ran first, such as the fill of an empty author, after which each revision
argument is held to its set as above, and with `--ignore-working-copy` where the step took its
snapshot.
A commit that the push reaches only through git's `HEAD` or a local branch that git moved in a
colocated workspace, which no jj command has imported yet, such as one that a `git commit`, a
`git commit --amend` or a `git rebase` made, is no exception: the step takes its snapshot before
its first read, which imports git's `HEAD` and refs in jj's own order and keeps the working copy
current, and the step reads that commit and sets its author by the rules above, as for any other.
A commit that only another ref of git's names, such as a tag, is outside that comparison: the step
takes no snapshot for it, and the commit keeps the author that jj alone leaves it.
`jj git import` is not that step: with `--ignore-working-copy` it imports no commit that git made
on jj's detached `HEAD`, and after a `git rebase` of the branch under `@` it rewrites `@` and
leaves the working copy that every role shares stale.
jj prints a commit id short, as its first 12 hex digits, and the step never has jj resolve such an
id over the whole repository, hidden commits included: it matches the id against the commits of
the repository's bookmarks and tags and of the push's revision arguments, or, for jj's refusal of
a commit with no author, against the mutable commits that no remote holds.
Two commits of that set whose ids start the same, another issue's under tmux included, make a dry
run that the step cannot read.
A commit's timestamps are whole seconds, so a rewrite in the same second as an earlier rewrite of
the same commit asks jj for a commit that exists already, which jj refuses.
In a retry of a refused push that names a hidden commit of a divergent change, which the first
push's rewrite replaced, the step takes the commit that the rewrite made, and the push sends it.
In any other case, such as a retry after a `jj undo` or a `jj op restore` in the same second, the
push stops with jj's error and exit status before the real push runs, and sends nothing.
A process with no table runs no step, such as a controller or a pane of an earlier release.
Where a host's jj wrapper runs `bin/jj` again, `legion jj` follows the loop rule of `legion git`
above to choose the `jj` it runs, and a command that comes back with the words `legion jj` ran it
with runs at once, with no second read of the aliases, no second line and no second step.
A push that a jj command starts, such as `jj util exec -- jj git push`, is a push of its own and
runs the step.
Every `jj` command of a pane starts one `legion` process first.
Measured in a gVisor pod, `jj --version` through `bin/jj` took 408 to 755 ms, and the real `jj`
took 95 to 161 ms.
Any command but those that make no read of the aliases (above), a bare `jj` included, also starts
one `jj --ignore-working-copy --color=never config list --include-defaults` through the next
`jj`, in the working directory and with none of the command's own options, which reads every
alias and `ui.default-command`, takes no snapshot and reads no commit.
Measured on a scratch clone of cybermonklabs/legion in an issue's pod, 15 runs each, that read
took 40 to 88 ms (median 56 ms) under jj 0.45.1-sami and 70 to 187 ms (median 115 ms) under jj
0.38.0; whole jj commands in that clone took 2.2 to 4.9 s with or without `bin/jj`, so the read is
inside their spread.
A push that the step runs on also starts the read of its configuration before the step (above),
`jj --ignore-working-copy --color=never config list -T '""'` with the push's `-R`, `--config` and
`--config-file` arguments, unless the read of the aliases expanded it and it names no `-R`; that
read takes no snapshot and reads no commit.
Measured on 2026-11-23 on a shallow scratch clone of cybermonklabs/legion in an issue's pod, 15
runs each, that read alone took 38 to 70 ms (median 47 ms) under jj 0.45.1-sami and 78 to 129 ms
(median 96 ms) under jj 0.38.0.
`legion controller start` writes the same `jj` into the `bin/` of its own state directory, by the
same rule, for the operator's controller.

A release older than the first one that writes `bin/jj` leaves `<state_dir>/bin/jj` in place,
and that file runs a `legion jj` command that the older `legion` does not have, so every `jj`
command in a pane fails until the operator deletes it.
So before you start the older daemon, remove `<state_dir>/bin/jj`. Before you run an older
`legion controller start`, remove the `bin/jj` of its state directory too.

No leak guard runs in a pane's `git` or `jj`.
Legion's leak-guard wrappers and its pre-push hook are the worker image's, root-owned under
`/opt/legion` in a filesystem no role can write ([Worker image](kubernetes.md#worker-image)), and
a shared box has no such boundary: the daemon, its panes and the operator are one Unix user, so a
wrapper put first on a pane's `PATH` would be one the pane can replace, and a `core.hooksPath`
pinned for it one the pane can override.
A copy of them here would read as a guard and hold nothing.
What a pane does run the guard in is `legion gh`, the same binary, which resolves the shared
public leak check from the pane's own `PUBLIC_LEAK_CHECK` and runs the deployment's private-name
rule file that the pane's launch hands it, or no private-name rule where the deployment names none
([What a pane's Dispatch credential is](#what-a-panes-dispatch-credential-is)).
A pane can find that check no other way: `PUBLIC_LEAK_CHECK` is not a name a pane reads from the
daemon's environment, a pane's `XDG_CONFIG_HOME` is under the state directory rather than the
operator's home, and the check's installer puts its files on no `PATH` a pane has.
So the daemon resolves it instead, once per launch and in its own environment — the machine's
own `PUBLIC_LEAK_CHECK`, a `public-leak-check.sh` on its `PATH`, or the pointer file under its
configuration home — and tells the pane where it is
(`resolveLeakCheck` and `panePairs`, `packages/daemon/internal/runtime/tmux`).
An operator who points one pane at another copy still wins: `runtime.tmux.pane_env` and a spec's
own `Env` are told after the resolved one, and the last value of a name is what the pane holds.
Legion installs no check on a shared box.
The pinned install is a pod's: the daemon reads the check at the tag the worker image carries and
pushes it to the pod's launcher, which installs it ([Worker image](kubernetes.md#worker-image)).
So a box that writes to public repositories carries the check through the check's own installer,
run on the machine the daemon runs on.
A daemon that resolves none tells that pane no `PUBLIC_LEAK_CHECK`.
What a pane's `legion gh` writes then meet is the deployment's leak check setting, which a tmux
daemon reads from its process environment as a Kubernetes daemon does:
`LEGION_LEAK_CHECK_REPOSITORY` and `LEGION_LEAK_CHECK_MANIFEST_SHA256`, or the `leak_check` block
of `legion.yaml` ([Worker image](kubernetes.md#worker-image)).
With the setting, every pane is told the check's manifest digest as
`LEGION_LEAK_CHECKER_MANIFEST_SHA256`, the daemon logs one line per launch naming a pane it
resolved no check for, and every write that pane's `legion gh` makes to a public repository is
refused with a setup error naming the missing check.
A pane that resolves a check whose manifest digest is anything but that one — another program, a
launcher script edited beside an unchanged program, a missing README or must-pass lines — has
every call of its `legion gh` refused, reads included, with a setup error naming the digest told
and the digest found, so the check the machine's installer put there must be the tag the setting
names, whole.

Under a checker digest, `legion gh` writes the four files it hashed into a new 0700 directory before it runs the check, and removes that directory when the call ends. A role pod puts that directory under `/var/run/legion/private`, a memory-backed path only that role's mount namespace reaches. A tmux pane has no such mount: its fallback is a 0700 directory under `/tmp`. That fallback keeps other Unix users out and is removed on every normal call exit, but it does not isolate same-user panes and it is not the `/var/run/legion/private` boundary.
A broken setup on the machine is told to the pane all the same, as the path it is broken at: a
stale `PUBLIC_LEAK_CHECK`, a `PUBLIC_LEAK_CHECK` or pointer file naming a directory or a file with
no execute bit, a pointer file that names nothing or cannot be read, or, where nothing else names
a check, a `public-leak-check.sh` on `PATH` that is a dangling link or has no execute bit. Its
public writes, pushes included, are then refused under either setting, and the daemon logs a line
per launch.
With no setting, the daemon logs once at boot that the deployment leak check is not configured, and
a pane's `legion gh` with no check named anywhere writes under the guard's visibility read and the
deployment's private-name rules alone, where the deployment names a rule file; where it names
none either, the call is cleared with nothing read
([The deployment's private-name rules](kubernetes.md#the-deployments-private-name-rules)).

The real gh is a compiled program, so when the first `gh` on the daemon's `PATH`
is a script (a `gh` wrapper of the box's own, as the devbox's login `PATH` heads with, or asdf's
shim), boot refuses it, naming it and `LEGION_GH_PATH` (`RefuseScriptGH`): run as the real gh, a
wrapper would answer in place of the role's App token. Set `LEGION_GH_PATH` to the real gh's
install path (`mise which gh` or `asdf which gh` prints it). The check reads scripts only: mise's
shim is compiled and passes, and where mise has no gh for the directory it runs the next `gh` on
`PATH`, which in a pane is the pane's own `legion gh`, so a shims directory is no real gh either.
`legion gh` refuses such a gh itself too: a `LEGION_GH_PATH` that is not an absolute path, that is
the pane's wrapper, or that is another `legion gh` wrapper script, exits 2 before anything runs, and
every real gh it runs carries `LEGION_GH_RUN_BY_REAL_GH`, under which a `legion gh` that gh leads
back to exits 2 and runs nothing.
The leak guard blanks that variable for every child it runs, so a push the real gh makes still
passes the pre-push hook's visibility read, whose own real gh carries the variable again. Its help
read of the real gh runs in a process group of its own, which a read past its timeout stops whole,
and a SIGINT, SIGTERM or SIGHUP that comes before the real gh starts stops that group and the leak
guard's children whole.

The daemon runs tmux itself: the first `tmux` on its `PATH`, with the pane environment, whose XDG
directories are under the state directory. A mise shim, or a wrapper script that hands the call to
one, then does not read the operator's mise config as its global config. Started from a directory
under the operator's `HOME`, it finds that config as a parent directory's config with no trust
record and exits 1, and boot refuses with `scrub the private tmux server's environment: … not
trusted`. Put a real tmux binary first on the daemon's `PATH`
(`mise which tmux` prints its install path), as the live proofs do with `real_bin_path`
(`scripts/e2e/README.md`).

The private server reads no tmux configuration file: the command that starts it carries
`-f /dev/null`, so the server's start does not run, or wait for, the operator's `~/.tmux.conf`
or its plugins. An operator who attaches with `tmux -L legion-<project> attach` gets tmux's
default keys and options. `source-file` in that client is not local to the client: tmux runs the
file's commands on the daemon's server, so the file's options, key bindings and plugins then
apply to that server and to the daemon's sessions and windows.

## What a pane's Dispatch credential is

A pane's Dispatch bearer is the daemon's own. The daemon reads `dispatch_token_file` once at boot
(`readBoot`, `packages/daemon/internal/daemon/bootreads.go`) and writes the value to
`<state_dir>/secrets/dispatch-token`, a 0600 file in a 0700 directory, with
`WriteDispatchTokenFile` (`packages/daemon/internal/runtime/secretfiles.go`, called from
`prepareTmux` in `packages/daemon/internal/daemon/daemon.go`). It then tells every pane
`DISPATCH_TOKEN_FILE` naming that file (`panePairs`,
`packages/daemon/internal/runtime/tmux/spawn.go`). The value never enters tmux's argv or a pane's
environment. So a pane holds the daemon's own credential, not one of its own.

The Envoy listener bearer reaches a pane the same way. It is a launch secret, `ENVOY_TOKEN`
(`launchSecrets`, `packages/daemon/internal/daemon/launchsecrets.go`), and the tmux runtime writes
every launch secret to a 0600 file of its own under `<state_dir>/secrets` and tells the pane the
matching `<NAME>_FILE` pointer, here `ENVOY_TOKEN_FILE` (`secretFiles`, `WriteSecretFiles` and
`panePairs`, `packages/daemon/internal/runtime/tmux/spawn.go`).

The deployment's private-name rule file is a launch secret too, `LEGION_PRIVATE_NAME_RULES`, which
the pane's `legion gh` reads through `LEGION_PRIVATE_NAME_RULES_FILE`. The daemon reads the file
once at boot, from `private_name_rules_file` in `legion.yaml` or else
`LEGION_PRIVATE_NAME_RULES_FILE` in its own environment, and hands its bytes to every launch. A
pane keeps the copy its launch was handed, so a changed file reaches a pane after the daemon
restarts and that pane launches again. Where neither setting names a file, no launch carries the
secret, and a pane's `legion gh` runs no private-name rule and prints nothing of it; the daemon
logs once, at boot, that the deployment names no rule file.
`legion controller start` passes the operator's own environment to its controller, so an operator
who wants that controller's guard to run the rules exports `LEGION_PRIVATE_NAME_RULES_FILE` there.
`docs/kubernetes.md`, "The deployment's private-name rules", has the rest.

When `dispatch_token_file` holds one of the deployment's shared agent tokens
(`DISPATCH_AGENT_TOKEN`), Dispatch matches the bearer against them (`MatchesSharedAgentToken`,
`packages/envoy/internal/dispatch/auth/agent_token.go`) and reads no person, no service and no
project from it. Whatever the bearer, the acting session is the one the request body names
(`bearerSessionActor`, `packages/envoy/internal/dispatch/api/server.go`). Dispatch takes any other
bearer as a service token or a personal token (`optionalActor`, the same file), and every pane's
session then carries that token's service or owner as well. `dispatch whoami` in a pane tells the
two apart: for a shared agent token it answers
`Session <id> runs under the shared token: no owner, …`
(`packages/envoy-client/src/dispatch-commands/session.ts`). A pod's side of this is in `docs/kubernetes.md`,
"Anatomy of a Sandbox pod". A person's own session on the same box uses a personal token instead,
minted from Dispatch's Settings → Agent tokens
(`docs/site/src/content/docs/dispatch/for-agents.md`).

The agent-secrets helper holds a credential of its own: the machine login a person approves for
`agent-secrets machine login`. The helper spends that credential on the broker's own routes
(`packages/envoy/internal/cipher/helper/broker.go`) and asks the broker for no Dispatch token. The
broker can exchange such a login's access token for one (`docs/kubernetes.md`, "The Dispatch
identity the broker can issue"), and nothing on this box does that at this release.
`dispatch://INFRA-267` is where the daemon and its workers get Dispatch tokens of their own, a dev
box session writing under an approved machine login included, and `dispatch://INFRA-264` is where
Dispatch accepts a broker token.

A job entry of the broker is how such a token names its writer's own account and no person.
`BROKER_EXCHANGE_JOB_ACCOUNTS` lists a pod's Kubernetes service account,
`BROKER_EXCHANGE_JOB_WORKFLOWS` a GitHub Actions workflow of one repository on one ref, and
`BROKER_VM_EXCHANGE_JOB_ACCOUNTS` a machine's Google service account, each with its projects (one
project key, or `public` and at most 16 private project keys) and, where it fixes one, a session.
The token the exchange mints for that identity names it as its service and no person, so Dispatch
records a daemon's or a workflow's write under its session and its own account, and under no
person. The token holds no OAuth scope, so a route that needs one refuses it with
`403 OAUTH_SCOPE_REQUIRED` before Dispatch's project check runs. A token whose entry names one
project writes that project, and the project check judges its write to another project as it
judges a person's `named` token's for the same project: `403 PROJECT_SCOPE` until
dispatch://INFRA-4799 changes that answer for both. A token whose entry names
`public` writes wherever a token that names no project may, and its list limits no write at
this release. A GitHub Actions job trades its run's token with
`AGENT_SECRETS_MACHINE_IDENTITY=github-actions` and sends its requests with `dispatch api`
(`packages/envoy-client/README.md`, "The `dispatch` command"). A machine's token that names a
service under the project scope `person`, of a machine nobody has signed in on and no job entry
lists, still reads and is refused every write `403 SIGN_IN_REQUIRED` (`docs/kubernetes.md`, "The
Dispatch identity the broker can issue").

## Phase workers on Claude Code

`worker_backends` in `legion.yaml` runs a role on another agent harness than Oh My Pi:

```yaml
worker_backends:
  implementer: claude
```

Each key is an issue role (`architect`, `planner`, `implementer`, `tester`, `reviewer`, `merger`)
and each value `omp` or `claude`; a role left out runs on Oh My Pi. What each harness may run is
one table, `runtime.Backends` (`packages/daemon/internal/runtime/backend.go`): Claude Code runs the
phase workers alone, since the architect's loop and the controller's are proven only on Oh My Pi,
and it runs them under either runtime — a tmux
pane here, and an issue pod under `runtime: kubernetes`, whose worker image carries the
claude-envoy plugin and Claude Code's pin, from which the pod installs and verifies the pinned
Claude Code at every launch (`docs/kubernetes.md`). The loader refuses a role on a harness that does
not run it, naming both, a role named twice, and any other value. The
controller always runs on Oh My Pi.

For such a role the pane's shim runs `legion claude-bridge -- claude -p --input-format stream-json
--output-format stream-json --verbose --replay-user-messages --thinking-display summarized
--permission-mode bypassPermissions --strict-mcp-config …`.
With `--replay-user-messages` Claude Code re-emits each user message it takes,
and the bridge answers an Envoy delivery only then,
so claude-envoy counts the delivery taken only once Claude Code has it.
With `--thinking-display summarized` it writes the text of its thinking rather than a thinking block with none,
so a worker's thinking reaches the pane and Dispatch's Agents page.
The claim's own Claude Code directory, `<state_dir>/claude/<claim token>`, is the pane's
`CLAUDE_CONFIG_DIR`: it holds the transcript, the MCP configuration the daemon writes (one server,
`legion`, running claude-envoy's `dist/envoy-channel.js` on bun) and that server's phase-reminder
state file, one per session,
and it outlives every launch of the claim. A fresh launch passes `--session-id <new id>`; a
relaunch passes `--resume <id>`, read from the transcript the claim registered, so a killed pane
comes back as the same Claude Code session. A role moved to Claude Code since its session was
recorded is refused rather than resumed, naming the Oh My Pi session file. A role moved back to Oh
My Pi gets its recorded Claude Code transcript passed to `omp --resume`, as every recorded session
file is; the pinned Oh My Pi refuses it, exiting 1 with `Cannot resume session "<file>": the
session header is missing or malformed. The file was not modified.`, and leaves its bytes as they
were, so the launch fails loudly rather than starting a fresh agent
(`packages/daemon/internal/runtime/tmux/resume_omp_test.go` holds the real binary to this).

tmux's boot resolves three things from the daemon's environment, and `legion config check`
makes the same refusals:

- `LEGION_CLAUDE_PATH`, the `claude` executable, else the first `claude` on `PATH`;
- `LEGION_BUN_PATH`, the `bun` the MCP server runs on, else the first `bun` on `PATH`;
- `LEGION_CLAUDE_PLUGIN_DIR`, the absolute path of a built `packages/claude-envoy`: it must hold
  `dist/envoy-channel.js` and the generated Legion plugin's `legion/.claude-plugin/plugin.json`.
  No checkout holds either: nothing commits them. An operator gets a copy two ways. Unpack the
  published package — `npm pack @sjawhar/claude-envoy` and `tar xzf` it, whose `package/`
  directory is the plugin, built from the commit that release was cut at — or, to run a tree that
  has not been released yet, build one with `bun scripts/build.ts --out <dir>` in that package,
  into a directory outside the checkout, and point `LEGION_CLAUDE_PLUGIN_DIR` at `<dir>`. The
  build leaves the checkout's tracked files as they are, so its next `git pull` fast-forwards.

A missing one refuses the boot, naming every gap in one line. The three are tmux's alone: a pod
resolves nothing. Its bun and its claude-envoy root are the worker image's, at fixed paths
(`/usr/local/bin/bun`, `/opt/legion/claude-envoy`), which the runtime treats as image-owned so no
operator mount can hide them. Its `claude` is not the image's: the image carries Claude Code's pin,
`/opt/legion/claude-code`, and no Claude Code, since its licence grants no redistribution and the
image is public, so the pod's start script installs the pinned release at every launch into the
generation's own directory on the issue's volume, holds it to the lockfile's integrity and the
pinned version, and fails the launch, loudly, when the fetch fails or does not verify. The launch
shapes both runtimes build from those paths — the agent command, the session id and transcript
rules, the MCP document and Claude Code's own variables — are one package,
`packages/daemon/internal/claudelaunch`; the pod's side of it is in `docs/kubernetes.md`.

The daemon names no model for Claude Code under either runtime. In a pane its key reaches it the
way an Oh My Pi pane's does: `provider_keys` (for example
`ANTHROPIC_API_KEY: <secretsd key>`) is exported by the shim into `legion claude-bridge`'s
environment, which Claude Code inherits, and into every Oh My Pi pane's as well. A key meant for the
Claude Code roles alone goes in the claim's directory instead, before the claim first launches: a
`settings.json` there whose `apiKeyHelper` prints the key, as `scripts/e2e/claude-backend-tmux.sh`,
the live proof, writes it. In a pod the route is the operator's the same way: `provider_keys`
carries `ANTHROPIC_API_KEY`, and `ANTHROPIC_BASE_URL` and `ANTHROPIC_MODEL`, which are not
credentials, go in `runtime.kubernetes.pod.env` (`docs/kubernetes.md`, "Configuration").

### How a harness plugs in

Every harness runs behind `legion worker-shim`, which speaks Oh My Pi's RPC frames to it
(`legion claude-bridge` translates them for Claude Code), so the worker stream and supervision do
not know which harness a process runs. A harness is four pieces in the daemon:

- its row in `runtime.Backends`: the roles it runs, the runtimes that launch it, the daemon prompt
  part its launches are told after their role's parts (Claude Code's is
  `internal/prompts/go/claude-mechanics.md`);
- a `Launcher` in `internal/runtime/tmux`: the command the shim runs, the extra `-e` pairs of its
  pane, and the marker its pane's command line holds;
- a boot resolver in `internal/daemon/launchers.go`, which finds what its launch needs on the host;
- the Kubernetes runtime's own branch in `internal/runtime/sandbox`, which builds the same launch
  from the worker image's paths instead: the pod's agent command, the files its launcher writes,
  the variables its start command tells the role, and the rule for the session a replacement pod
  resumes.

Boot resolves a launch for each harness `worker_backends` uses (`legion config check` makes
the same refusals), and the tmux runtime refuses a launch on a harness it has no launcher for. A
pane is its recorded process only while its root process's command line, `sh -c` on the whole pane
command, holds a configured harness's marker: `omp` for Oh My Pi, ` claude-bridge -- ` for Claude
Code. A daemon with no `worker_backends` has Oh My Pi's launcher alone and checks for `omp` alone.

## The `runtime.tmux` block

`runtime: tmux` (or no `runtime` key) runs every pane with what the runtime tells it and the
allow-listed part of the daemon's environment (`HOME`, `OMP_PROFILE`, `PATH`, locale, proxies). The
mapping form adds the host's own needs:

```yaml
runtime:
  tmux:
    pane_env:                       # plain variables every pane gets, after the runtime's own
      ELAN_HOME: /opt/elan
      DISPATCH_PUBLIC_URL: https://dispatch.example.com
      LEDGER_DSN_FILE: /etc/ledger/dsn   # a _FILE pointer names a file and is allowed
    pi_config_files:                # Oh My Pi overlays, in order, in every pane's PI_CONFIG_FILES
      - /etc/legion-operator/overlay.yml
    take_over: kubernetes           # only while moving a project onto tmux (below)
provider_keys:
  ANTHROPIC_API_KEY: file:/etc/legion/providers/ANTHROPIC_API_KEY
  OPENAI_API_KEY: OPENAI_API_KEY_PROD    # a secretsd key, as before
```

- **`pane_env`** refuses `PATH` (the runtime composes it), every name the runtime sets on a pane
  (its own `LEGION_*` names, the XDG directories, the gh variables, `PI_CONFIG_FILES`,
  `LEGION_CONTROLLER`, `LEGION_SLACK_CHANNELS`), the variables it sets on every Claude Code pane
  (`CLAUDE_CONFIG_DIR`, `CLAUDE_CODE_DISABLE_CRON`), the one it sets to `1` on every pane that
  resumes a recorded Oh My Pi session (`OMP_SESSION_EXCLUSIVE`, which holds that Oh My Pi to the
  session it resumes), the git
  identity a launch sets (`JJ_USER`, `GIT_AUTHOR_NAME`, …), the identities of the issue's roles that
  every workflow launch sets for the push step (`LEGION_ROLE_IDENTITIES`), the pointers to the
  secrets every launch gets as a file of its own (`ENVOY_TOKEN_FILE`,
  `LEGION_PRIVATE_NAME_RULES_FILE`, `NATS_NKEY_SEED_FILE`), the variables that place
  the session a resume reads (`OMP_SESSION_STORAGE`, `OMP_SESSION_SQL_DSN`,
  `OMP_SESSION_SQL_DSN_FILE`, `OMP_CONFIG_ROOT`, `PI_CODING_AGENT_DIR`, `PI_CONFIG_DIR`), a provider key or its
  `_FILE` pointer, any credential-shaped name (`…_TOKEN`, `…_KEY`, `…_SECRET`, `…_SEED`,
  `…_PASSWORD`, `…_PAT`, `…_GRANT`, `…_CREDENTIALS`, `PRIVATE_KEY`), and any value holding a URL
  with a password in it (`scheme://user:password@…`). A value on tmux's command line is readable by
  every process of the host, so a secret reaches a pane as a file. The loader refuses these at load
  and the runtime again at boot, naming the variable and never its value. A launch whose own
  variable or secret pointer collides with a `pane_env` name is refused.
  `OTEL_RESOURCE_ATTRIBUTES` is the one composed name `pane_env` accepts: the runtime composes
  `PI_CONFIG_FILES` from `pi_config_files` and so refuses that name in the list above, while the
  attributes are the operator's to begin and the runtime's to finish.
  The operator sets the attributes this deployment's spend is billed
  under, and each pane carries them with its own `legion.issue` and `legion.role` after them,
  so a pane's own attributes name the issue it serves. A pane of a unit drawn into arms of the
  project's experiments carries `legion.arms` after those, one `<experiment>:<variant>` for each
  arm it runs in (`legion.arms=memory:memory` once decoded), so each model call says which arm's
  files it ran with. The agent keeps the last value a key is
  given, so the daemon's pairs come last. A spend row names an issue, a role or an arm only
  where the spend collector keeps that key. A deployment's collector may keep a fixed list of
  attributes; include all three in that list to group spend by them. A controller pane serves no issue and carries the
  role alone, and the boot gate's probe runs no role, so it carries the operator's value with its
  trailing commas and whitespace dropped and nothing added.
  The loader reads this one value for its content as well as its name: it
  refuses a member the agent's OpenTelemetry detector would throw on — one that is no single
  `key=value`, one with an empty key, one whose key or value does not percent-decode or is longer
  than 255 characters once decoded, and one naming `legion.issue`, `legion.role` or
  `legion.arms`, which the runtime writes itself — and names that member and what is wrong with it, never the rest of the
  value. One such member costs the pane every attribute of the variable, the operator's own among
  them, because the detector's caller catches the throw with an empty hand.
- **`pi_config_files`** paths resolve against `legion.yaml`'s directory and may not hold `:`.
  Legion adds no overlay of its own to the list: it writes none, so the operator's are the whole of
  it, with a role's own (`role_config_files`) after them. The
  boot plugin gate probes Oh My Pi under the same overlays and variables.
- **`role_config_files`**, a top-level key beside the block, gives one role's panes overlays no
  other role's get (`role_config_files: {implementer: [/etc/legion-operator/fallback.yml]}`, the
  controller's pane under `controller: daemon` too): such a pane's `PI_CONFIG_FILES` is
  `pi_config_files`, then the role's own in order, and nothing after them. Each
  path must be absolute; boot and `legion config check` refuse one the host does not hold
  as a file the daemon's user can read, naming the role and the path, and a pane of a role the key
  leaves out is told what it was told without it. After the plugin gate passes, the boot gate runs
  `omp config get retry.modelFallback --json` under each named role's list and on each of its files
  alone, and refuses the boot when Oh My Pi exits non-zero (`boot gate: role_config_files.<role>:
  the <role>'s Oh My Pi would not start: Oh My Pi refuses <file>: error: …`), rather than letting
  every launch of that role fail. `docs/kubernetes.md` ("Operator configuration") has the rest of
  the key's rules. Every pane runs as the daemon's Unix user, so each role can read every role's
  overlay file; only loading is per role, so never put a secret in one.
- **A role's system prompt is a file.** Each launch's composed system prompt — the role's prompt
  files, the sentence that says which claim it is, an owner's `plan_guidance` where the deployment
  sets one, the deployment's `instructions`, then each drawn arm's — is written to
  `<state_dir>/secrets/<claim token>-system-prompt.md`, kept beside the
  claim's other secret files and pruned with them, and Oh My Pi is passed that file's path as
  `--append-system-prompt <path>`, Claude Code as `--append-system-prompt-file <path>`, so no
  prompt travels as one argument and none is bounded by the 131,072 bytes Linux allows one.
- **An edited instructions file needs no restart.** The daemon reads the deployment's
  `instructions` and each declared arm's `instructions` at boot, and again before every launch,
  into its own copy under the state directory, so the next launch carries the edit. A role already
  running keeps the prompt it launched with until its next launch, and an edit replaces no pod.
  A file an edit leaves missing, unreadable or blank, which boot refuses, stops no launch of a
  running daemon: each launch carries the text the daemon last read, and the daemon logs one WARN
  line for each refusal, naming the file and the refusal (`legion daemon launches with the
  instructions it last read: …`; a copy it cannot replace is the same line at ERROR), then one INFO
  line once the file reads again (`legion daemon read an edited instructions file: …`, or
  `legion daemon reads an instructions file again …` when it holds the text the daemon kept). Under
  Kubernetes the daemon must read the file from a directory mount: the kubelet never refreshes a
  file mounted by `subPath`. Naming another file, or a file for the first time, is a change of
  `legion.yaml`, which still needs a restart.
- **`projects.<KEY>.memory`**, the project's own block, is one of the things that append to a
  pane's `PI_CONFIG_FILES`
  (`memory: {mode: random, config_files: [/etc/legion-memory/memory.yml], instructions: /etc/legion-daemon/memory-instructions.md}`).
  The block **is** the experiment named `memory` (`projects.<KEY>.experiments` below): the daemon
  reads it as an experiment of unit `tree` whose arms are `memory`, carrying the block's
  `config_files` and `instructions`, and `none`, carrying neither, at equal weight, with
  `on_variant: memory`.
  A project writes the block **or** `experiments.memory`, never both: the two are one experiment
  spelled twice, and a project that writes both is refused.
  The block itself is accepted and refused in no new way, so a file that passed before passes
  now, unchanged: every rule below is the rule it has always had.
  `mode` is `off`, `on` or `random`: `off` gives no tree a memory group and is what a project with
  no block gets, `on` gives every new tree one, and `random` draws a fair coin for each new tree.
  `config_files` are the Oh My Pi overlays a memory-group tree's panes name, and `instructions` the
  one markdown file the daemon reads at boot, and again at each launch, and appends to their
  prompt; both are absolute paths.
  The draw is made once, when the tree's lifecycle row is first created, and is kept on the tree's
  root issue record with the mode it was drawn under and the time of the draw.
  A re-admission, a new generation and the next epoch after a cleanup keep it, and a child its root
  hands over as a tree of its own takes the answer of the tree it leaves at the hand-over: that
  tree's draw verbatim where it had one, and no draw at all where that tree had been opened with
  none.
  A tree first opened before this release has no group and runs without memory, nothing draws for
  it later, and a change of mode reaches only the trees opened after it.
  A child out of a tree the daemon never opened has run under nothing and the hand-over gives it
  nothing, so its own first open draws it like any other new tree.
  Every role of a memory-group tree names the memory overlays after `pi_config_files` and the
  role's own, and before any other experiment's arms, since the block's experiment comes first
  among the project's declared experiments, and ends its system prompt with the memory
  instructions, after the deployment's own.
  The controller and every role of a no-memory tree get neither, and a block with no
  `instructions` appends nothing.
  The launch follows the record, not the mode:
  a tree the daemon drew into the memory group keeps its overlays and its instructions while the
  block still names them, whatever the mode is now, so `mode: off` gives every new tree no arm and
  takes memory from no tree already running with it.
  Taking the files away is removing them from the block.
  The loader refuses a `mode` that is no word of the three, and a mode other than `off` that names
  no `config_files`.
  It refuses a block that names `config_files` while `worker_backends` puts an issue role on
  Claude Code, which reads no `PI_CONFIG_FILES`, whatever the mode, since a drawn tree's role
  would be given overlays Claude Code never reads.
  Boot refuses an overlay no pane could read, naming the key and the file, as it does one of
  `role_config_files`, and an `instructions` file it cannot read or that holds nothing; neither
  reads the mode either.
  `legion config check` makes every one of these refusals and starts nothing.
  `docs/kubernetes.md` ("Operator configuration") has what the block does under `runtime:
  kubernetes`.
- **`projects.<KEY>.experiments`**, the project's own mapping, declares the experiments it draws
  its trees and its issues into, by the name each draw records
  (`experiments: {plan-length: {mode: random, unit: issue, variants: [{name: long, config_files: [/etc/legion-experiments/plan-length.yml]}, {name: control}]}}`).
  An experiment's name is 1 to 32 characters of `a-z`, `0-9` and `-`, and the project names it
  once.
  `mode` is `off`, `on` or `random`, and `off` is the default.
  `off` gives every new unit no arm, and the daemon records that unit at the empty arm, with the
  mode `off` and the instant.
  The record keeps that arm as `""`, and `legion state --json` shows `""` too.
  The plain `legion state` line prints it as `none`.
  `on` gives every new unit the arm `on_variant` names, and `random` draws among the arms by their
  weights.
  `unit` is `tree`, the default, which draws once for a whole tree and gives every issue of it the
  same arm, or `issue`, which draws for each issue on its own.
  `roles` lists the issue roles whose launches load the experiment's arms, each once, and every
  issue role where the block names none; a pane of a role the list leaves out loads none of this
  experiment's overlays or instructions while its unit keeps the arm it drew.
  `variants` are the arms it compares, and a mode other than `off` needs at least two of them,
  since a draw among one arm measures nothing.
  Each arm takes a `name` (the same characters, named once in its experiment), a `weight` (a whole
  number above zero, `1` where the arm names none), `config_files`, the Oh My Pi overlays a pane
  of a unit drawn into it names, `instructions`, one markdown file the daemon reads at boot, and
  again at each launch, and appends to that pane's prompt, and `plan`, how the issues drawn into it
  are done (below).
  Both file keys are absolute paths, and neither is required: the arm that names no file at all is
  the control the others are measured against.
  **The order a pane's `PI_CONFIG_FILES` names them in** is the operator's files
  (`runtime.tmux.pi_config_files`), then the pane's own role's (`role_config_files`), then the
  files of each arm the pane's unit was drawn into, in the order the project's file declares the
  experiments, so an arm's file outranks both.
  The `memory` block's own arm comes first among them, since the block's experiment comes before
  the `experiments` mapping's own.
  The system prompt ends with each such arm's `instructions` in that same order, after the
  deployment's own.
  The draw is made once, when the unit is opened — a tree's when its lifecycle row is first
  created, an issue's when the daemon first records that issue — and is recorded with the arm, its
  `plan` where it names one, the mode in force and the instant.
  The launch follows the record, not the mode: a unit drawn into an arm keeps that arm's files
  while the project's file still names the arm, whatever the mode is now, and taking the files
  away is taking the arm out of the file.
  The controller runs in no tree and no issue, so it is in no experiment and gets no arm's files.
  The loader refuses a `mode` that is no word of the three, a `unit` that is neither word, a block
  that lists no variant, a mode other than `off` over one variant, a `mode: on` that names no
  `on_variant`, an `on_variant` that is no arm of its experiment, a name or an arm name outside
  those characters, a name or an arm name written twice, a `weight` that is not a whole number
  above zero, and an arm that names `config_files` while `worker_backends` puts an issue role on
  Claude Code, which reads no `PI_CONFIG_FILES`.
  Boot refuses an arm's overlay no pane could read and an arm's `instructions` file it cannot read
  or that holds nothing.
  `legion config check` makes every one of these refusals and starts nothing.
  `legion config reference --json` prints every key of the block with its default and what it
  does.
  `docs/kubernetes.md` ("Operator configuration") has what the block does under `runtime:
  kubernetes`, where an arm's newly declared overlay file replaces every live issue pod.
- **An arm's `plan`** says how the issues drawn into it are done: `owner`, whose issues take the
  plan their owner gives with `register_plan`, or `default`, whose issues run the project's
  `default_steps` and take no plan from their owner: once the daemon records such an issue, its
  owner's `register_plan` and `stop_task` are answered `409 PLAN_NOT_TAKEN`, and a plan its owner
  gave before is passed over at its first step and told in a `plan-not-taken` notice.
  The draw records the arm's `plan` beside the arm, and the issue keeps it for its whole life,
  whatever the file says later.
  An experiment that names `plan` in one arm names it in every arm and is `unit: issue`, and at
  most one experiment of a project sets it.
  The loader refuses a `plan` that is neither `owner` nor `default` (`owner-alone`, which this
  release does not carry out, among them), an arm that names no `plan` beside one that does, an
  experiment that sets `plan` at `unit: tree`, and a second experiment of one project that sets
  `plan`; `legion config check` makes each of these refusals.
- **`projects.<KEY>.commit_issue_trailer_repos`**, the project's own list, names the repositories
  whose commits carry their issue key
  (`commit_issue_trailer_repos: [acme/widgets]`).
  A pane working one of them is told that key, and its jj appends a `Dispatch: <KEY>` line to
  every commit the pane makes in its own workspace, after the `Omp-Session:` and `Legion-Role:`
  lines every pane's commits carry, so a squash landing that branch carries the issue key in its
  message. A `git commit` gets the session and role lines from the pane's `git`, and no issue line.
  A pane stamps the commit when its launch's repository is one of the names.
  The daemon compares the list with nothing at boot, so a misspelled name stamps no commit and no
  line says so: compare the list with the repositories `legion state` lists.
  A pane of a repository the key does not name is told no key and its commits carry the session
  and role lines alone, as does a commit the pane's shell makes in any other jj repository.
  The variable the pane is told is the runtime's own, so a spec, a `pane_env` and a
  `runtime.kubernetes.pod.env` are each refused for setting it.
  The loader refuses a name that is no `owner/name`, and a repository the list names twice,
  compared without case and through `repository_renames`.
- **`take_over: kubernetes`** lets this daemon take over the claims a daemon running the project
  under `runtime: kubernetes` recorded on the same store ([Moving a project onto
  tmux](#moving-a-project-onto-tmux)). `kubernetes` is the only accepted value. Without the key, a
  claim whose recorded process is a pod is refused, naming the claim and the key, and the boot does
  not start: two daemons must never drive one store's claims at once.
- **`provider_keys`** values of the form `file:/abs/path` are read once at boot, as the daemon,
  into the same daemon-held 0600 files a secretsd key lands in. The file is read by the rules every
  other secret file the daemon reads follows: one that cannot be read, is not a regular file, holds
  nothing, or is readable by its group or others refuses the boot, naming the variable and the
  file. Kubernetes refuses the form: a file on the daemon's disk reaches no pod.
  Under tmux the loader, and so `legion config check`, refuses with the reason `pane_env` gives a
  key named for a variable that places the session a resume reads, one the runtime tells a Claude
  Code pane or a resumed Oh My Pi pane after the operator's variables, a launch secret's pointer,
  or `LEGION_ROLE_IDENTITIES`; the boot refuses those again, and also a key named for a variable
  every pane carries and a key whose `_FILE` pointer every pane carries.
  The shim exports every provider key into the environment of the agent it starts (Oh My Pi, or
  `legion claude-bridge` on a Claude Code pane), where it acts as the pane variable of that name.
- **`controller: daemon`** works under tmux too: the daemon launches the controller in a pane of
  its own window, `controller`, working in `<state_dir>/controller`, told `LEGION_CONTROLLER=1` and
  an empty `LEGION_SLACK_CHANNELS`, and supervises it like a root architect. It needs
  `dispatch_url`. A project with no GitHub owner to mint for runs it with gh acting as nobody, its
  gh directory empty.

## How workspaces are provisioned on the host

Each repository has one shared jj clone at `<state_dir>/repos/github.com/<owner>/<repo>`, and each
issue a jj workspace of it at `<state_dir>/workspaces/<owner>/<repo>/<issue, lowercased>`, on the
bookmark `legion/<ISSUE>`. The first provision of a repository clones it with the implement App's
installation token, handed to git through a one-shot 0600 credential helper under the state
directory. Every launch first makes sure its issue's workspace is there: a claim resumed,
relaunched or recovered, as well as a new one, is provisioned when its directory is missing or its
checkout never finished (it holds no sparse pattern, read with `jj sparse list
--ignore-working-copy`; a workspace an agent narrowed keeps its patterns), checking out
`legion/<ISSUE>` when GitHub has it. A launch waits for a provision or removal of the same issue
that is under way.

Provisions of different issues run side by side. Everything a provision does to the shared clone
runs under the clone's one store lock: a fetch of the default branch and the issue's branch alone,
the workspace's registration with no files (`jj workspace add --sparse-patterns empty`), its
bookmark, and the clone's git configuration. The checkout of the working copy (`jj sparse reset`
in the new workspace) writes only that workspace and runs with the lock released; at most 16
checkouts run at once, and a provision waiting on its repository's store lock holds no place
among them. A workspace whose checkout never finished is checked out by its next provision, which
its next launch runs. A removal takes the issue's turn and holds the store lock only for its jj
commands on the clone and the move of the workspace aside to `<dir>.removing`; the directory is
deleted after the lock is released, and a `<dir>.removing` a crash left is cleared by the issue's
next removal.

A store may be made before the daemon's first boot, with the same jj the daemon uses, as the
daemon's user: a colocated `jj git clone https://github.com/<owner>/<repo>` into the clone path,
then `jj -R <clone> --ignore-working-copy config set --repo git.abandon-unreachable-commits false`.
A directory there without `.jj` is removed and cloned again; one without `.git` is not a store.
Boot rewrites every store's credential helper to `legion git-credential`.

## Moving a project onto tmux

A tmux daemon can take over the store of a daemon that ran the project under `runtime: kubernetes`,
when its `legion.yaml` sets `runtime.tmux.take_over: kubernetes`. Without that key it refuses every
claim whose recorded process is a pod, and does not start. Nothing else stops two daemons from
driving one store at once — the instance lock is a file on each machine, and claim records are
written by whichever daemon wrote last — so the order matters:

1. Stop the Kubernetes daemon. Its issue pods outlive it: push every issue's unpushed work, then
   delete the project's Sandboxes (`kubectl -n <namespace> delete sandbox -l
   'legion.dev/project=<project>'`), which takes each pod and each pod's disk, and wait until none
   of the project's Sandboxes or pods is left. A pod still running beside the tmux daemon is a
   second agent on the same claim and branch.
2. Put each claim's session file where its pane's Oh My Pi keeps sessions, in a folder one level
   down, under its recorded file name (`<timestamp>_<session id>.jsonl`): either at the recorded
   path itself, or in the pane's own sessions directory. That directory is
   `<HOME>/.omp/agent/sessions`, or `<HOME>/.omp/profiles/<profile>/agent/sessions` when the
   daemon's environment or `pane_env` sets `OMP_PROFILE` or `PI_PROFILE`, or
   `<state_dir>/home/.local/share/omp[/profiles/<profile>]/sessions` when that directory already
   exists; a profile named only inside `omp_launch_prefix` is not read. A session the Kubernetes
   runtime kept in its session database (`runtime.kubernetes.session_store`) is not carried: no
   command writes its row out as a file, so its claim starts a fresh agent from the issue's pushed
   branch and committed handoffs.
3. Set `runtime.tmux.take_over: kubernetes` and boot the tmux daemon with the same Postgres. Each
   claim whose recorded process is a pod is reported gone, displaced, and relaunched in a pane,
   charged nothing, on its session when step 2 put its file in one of those places, searched by
   file name or session id. A session found nowhere starts a fresh agent, uncharged, which
   registers a session of its own; the loss is posted on the issue and told to its architect, to
   the controller and to the new agent in its own prompt. That holds whether or not `take_over` is
   set — a session no store holds is gone whichever runtime last wrote it — so what this key buys
   is the takeover of the other runtime's locators: a claim whose pod this daemon cannot see is
   resumed without waiting that process out. Each workspace is provisioned before its first launch.
4. Retry what failed to launch meanwhile, once the cause is gone: `legion claims retry --failed`
   (an architect relaunches on its session, or on a new one once supervise's `forgetSession` has
   dropped it; a phase worker whose issue is held for its own role retries that phase).
5. Remove `take_over` once no claim can still wake on a session a pod recorded: every claim that
   was running has relaunched in a pane, and no claim the Kubernetes daemon suspended (a
   production check's checking role — the one the project's `production_check` names — waiting
   for the look its own `wake_at` asked for among them) is left to wake. From then on a resume
   over a locator of the other runtime is refused, and a tmux daemon pointed at a store a
   Kubernetes daemon still drives refuses to start rather than take its claims.

## Rotating broker-held App keys

Rotate the keys in the broker's `github-app` secrets, not on the daemon's machine:

1. In each GitHub App's settings, generate a new private key and leave the old key active.

2. Update the matching broker secret, `LEGION_IMPLEMENT_APP` or `LEGION_REVIEW_APP`, as [Store the App key](site/src/content/docs/broker/guides/github-app-for-a-dev-box.md) shows:
   its value is the JSON document `{"app_id": …, "private_key": "<the new PEM>"}`, set with `agent-secrets secret set <NAME> < app-key.json`, never the bare PEM.
   Keep the secret's `github-app` type and its GitHub owners.

3. Open a registered shell of your own (`agent-secrets register --wait 10 --exec -- sh`; the daemon's session cannot be entered from another terminal), then run each role's `token_command` from [the App section](#the-legion-implementer-and-legion-reviewer-apps) there, with `LEGION_GITHUB_OWNER` and `LEGION_GITHUB_READ_ONLY` unset, and confirm it prints the installations' tokens (`LEGION_REVIEW_APP` waits for a person's approval).
   Then confirm a fresh worker pane's `gh auth status` succeeds with account `x-access-token`.
   The daemon needs no restart: `token_command` gets fresh tokens as its cached leases near expiry.

4. Only then revoke the old private keys in GitHub App settings.
   Revoking them before the broker can mint with the new keys would break token refresh and,
   once existing tokens expire, the panes' `gh` and `jj git push` credentials.

## Saying when the daemon is serving

`GET /readyz`, at the API port legion.yaml's `port` names (13370 by default, `defaultPort` in
`packages/daemon/internal/config/config.go`), is the daemon saying whether it serves. It carries no
token and reads no database, and what a caller sees follows the daemon's life:

- While the boot runs, no answer at all. The daemon binds its port early in the boot but serves
  HTTP only once the boot is done, so a request waits in the kernel's queue until the caller's own
  timeout runs out.

- `200 {"status":"ok"}` once the daemon serves: its runtime built, its launcher acceptor installed
  and its API serving. `503 {"status":"starting","waitingFor":"the daemon to serve"}` answers only a
  request that reaches the instant between the server starting and the latch being set.

- `503 {"status":"stopping"}` from the moment the daemon's stop begins, on a connection the server
  still holds through its drain. A new connection gets no answer, because the drain closes the
  listener at once.

It reports nothing beyond that boot's own progress, so the answer is the same under any load and
while every connection of the pool is busy. `/healthz` answers 200 on every request the API takes,
the drain's included, and says nothing about the boot, so a start script or a supervisor on this box
waits on `/readyz` instead, with a bound on each try, since a try during the boot gets no answer:
`until curl -fsS --max-time 2 http://127.0.0.1:13370/readyz; do sleep 1; done`.

A Kubernetes deployment declares it as the daemon container's readiness probe. Those manifests are
in another repository, not this one; the probe to declare on that container, and why it is a
readiness probe and never a liveness probe, are in the Kubernetes guide's Configuration section
(`docs/kubernetes.md`).

## Serving on a socket the harness holds

`legion start` binds legion.yaml's `bind` and `port` for its API. With `LEGION_LISTEN_FD=<n>` in
its environment, it serves the API on descriptor `<n>` instead, a listening TCP socket it inherited
from whatever started it, and binds nothing. A test harness opens that socket on port 0, so the
kernel chooses the port, and holds it until the daemon serves on it; no other program can take the
port in between. A shared-box deployment leaves the variable unset.

- The file's `bind` and `port` must name the socket's address: `bind` an IP literal equal to the
  socket's IP (`0.0.0.0` matches only a socket on every IPv4 interface, and a host name matches
  nothing), `port` its port. The registry entry, the default `daemon_url` and `legion state` dial
  them. A socket they do not name, or a descriptor that is not a listening TCP socket, refuses the
  start before it takes the team or opens the store, naming `LEGION_LISTEN_FD` and the setting:
  `legion start: legion.yaml: port=8081 does not match LEGION_LISTEN_FD=3, a socket listening on 127.0.0.1:8080`.
- The daemon takes the socket over before it runs anything, the configuration's token commands
  included, and serves on a close-on-exec copy, then unsets the variable: no process it starts, its
  panes and tmux server among them, holds the socket or inherits the variable.
- The boot line `legion daemon started` names the address the daemon serves on, as it does when it
  binds.
- `legion config check` binds nothing and never reads the variable.
