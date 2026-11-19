#!/usr/bin/env bash
# The release workflows keep every credential away from the code the releases are built from. Each
# release is built by a build job, which runs the dependencies' code (install, pack, build, test,
# smoke) and holds no write scope and no secret, and published by a publish job, which holds the
# credentials and runs no dependency code: only release-push.sh, git, gh, jq, sha256sum, npm view
# and npm publish and a handful of pinned actions, with no cache restored and the deploy key only
# in the env of the steps that push. (release.yaml's one publish job publishes the packages of its
# four build jobs, one after another.)
# This check pins that shape, so a later edit cannot undo it quietly:
#
# 1. A credentialed job — one whose `permissions` grants contents, id-token, attestations or
#    actions: write (the workflow's `permissions` when the job declares none, as GitHub gives it;
#    actions: write can start, re-run and cancel runs), or whose text anywhere, the workflow's
#    `env:` included, names the deploy key, `secrets.DEPLOY_KEY` or by index
#    `secrets['DEPLOY_KEY']`, in any casing (GitHub resolves `secrets.deploy_key` too) —
#    (a) runs no dependency code, (b) uses only the actions on CREDENTIALED_USES below, (c) sets
#    `cache-mode: none`, and (d) names the deploy key only as `DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}`
#    (the dot form) in the step-level `env:` of a step whose `run:` calls the one push command
#    (PUSH_COMMAND), `release-push.sh push-tag`, as a command and through the checkout's own copy
#    (behind `$GITHUB_WORKSPACE/`, as below), and nothing else; the workflow's `env:` reaches every
#    job, so the key there is never allowed.
# 2. A build job — one that runs dependency code — holds none of those four write scopes and no
#    `secrets` reference but `secrets.GITHUB_TOKEN`, in its own text or the workflow's `env:` (so
#    no deploy key, by dot or by index, no other secret and no `toJSON(secrets)`), declares
#    `permissions` of its own when the workflow declares none (a job with neither runs under
#    whatever the repository grants, which this check cannot read), carries no
#    `continue-on-error:` (GitHub reports such a job as a success to the jobs that need it, failed
#    tripwire and all), and its last step is exactly `run: .github/scripts/release-push.sh
#    no-credential`, with no `if:` and no `continue-on-error:`, under bash or sh (its `shell:`,
#    else the job's `defaults.run.shell`, else the workflow's; a `cat {0}` there would print the
#    tripwire instead of running it): no condition or error tolerance, on the step or on the job,
#    can gate the tripwire away or let its failure pass, and a shell is what runs it. (A build
#    that fails before it reaches the tripwire publishes nothing anyway: its publish job needs it.)
# 3. Every `actions/checkout` (its owner and repository compared lower-cased, as GitHub resolves
#    them) sets `persist-credentials: false` (the boolean), so no token is left in the checkout for
#    a later step.
# 4. release-envoy-listener.yaml's release_publish runs in the concurrency group
#    `release-envoy-listener` with `cancel-in-progress: false` and `queue: max`: a pending job that
#    GitHub cancelled for the next one would leave its commit in no release. That file's
#    workflow-level group does not stand in for this one and does not weaken it: it coalesces runs
#    before they start a job, so a waiting build is replaced by a newer push that still holds the
#    commit and the replaced commit loses an image and a version number of its own, never its
#    change (that file's header, above its `concurrency:` block, is the one copy of why); a run
#    that reached release_publish has already built and pushed its image, and a run
#    release-push.sh restart started is a workflow_dispatch in a group of its own, which no push
#    replaces.
# 5. A job's `if:` that calls `always()` tests `needs.<X>.result` for every X whose
#    `needs.<X>.outputs` it reads; one that calls no `always()` belongs to a job with no skippable
#    ancestor — no job upstream of it through `needs`, at any distance, carrying an `if:` of its
#    own. GitHub gives an `if:` that calls no status function an implicit `success()`, false
#    whenever any job upstream through `needs`, at any distance, was skipped (actions/runner#491),
#    so with a skippable ancestor that implicit `success()` decides, whatever the condition says
#    (an explicit `success()` is the implicit one); `always()` turns that gating off, and then a
#    failed or skipped X is read through its empty outputs (`'' != 'true'` is true) unless its
#    result is tested. A job with no `if:` is not checked: skipped with its upstream is then
#    GitHub's plain, visible behaviour. The job-level `if:` only: a step's implicit `success()` is
#    over the job's own earlier steps, not over `needs`. A string literal is text (`'always()'`
#    calls nothing), the context and functions are case-insensitive, and `needs['x'].outputs` is
#    `needs.x.outputs`. This is what skipped legion_publish, a job release.yaml had then, in run
#    38822866387, the first on the build/publish split: its `if:` read legion_build's outputs
#    behind an implicit `success()` that envoy_plugin_publish, four jobs upstream, had already
#    made false.
# 6. A job with a step whose `run:` calls `release-push.sh`, at any path or none, has an
#    `actions/checkout` before that step. The script is in the checkout and nowhere else, so
#    without one the step exits 127 and the job carries on: that is what left every legion
#    release's "## Worker image" section unwritten until release.yaml's worker_image_release,
#    a job that checked nothing out, got its checkout.
# 7. A job that calls a reusable workflow (`uses:` and no `steps:`) names no secret but
#    GITHUB_TOKEN in its text, and carries no `secrets: inherit`, which hands it every secret,
#    unless its `uses:` is `./` and the path of a workflow on WORKFLOWS: the called workflow's jobs
#    are what run the secret, and this check reads only those. So a names job pointed at
#    worker-image.yaml, which runs a docker build, or a `secrets:` block added to worker_image, is
#    refused here rather than passing unread. A call to a listed workflow carries no
#    `secrets: inherit` either: it would hide from rule 8 which secret the call passes.
# 8. A step that starts publishing a build job's output waits for that output's private-name check.
#    A names job is a job whose text names secrets.PRIVATE_NAME_TERMS; it checks the output of
#    each job it needs. In any other job, a step whose `if:` tests `needs.<build>.result ==
#    'success'` for such a build, as a conjunction (no `||`), is the first step of that package's
#    chain, since every later step of it reads the step before it: that `if:` also tests
#    `needs.<names job>.result == 'success'`. Else a finding of the names job publishes anyway.
#    An `if:` that joins packages with `||`, such as the setup-node step that serves every npm
#    publish, is no step of one package and is not read. A names job also brings in, through its
#    steps and the local composite actions they use at any depth, only the remote actions on
#    CREDENTIALED_USES, so no third-party action runs beside the term list.
#
# "Runs dependency code" is a tripwire over named patterns, not a proof that no other code runs: a
# step runs dependency code when its `uses:` starts with one of DEPENDENCY_USES, when the shell its
# `run:` runs under — its `shell:`, else the job's `defaults.run.shell`, else the workflow's, else
# bash — is not bash or sh (a body under python, pwsh, node or cmd is a program this check cannot
# read), or when a word in command position of its `run:` is one of DEPENDENCY_COMMANDS, is `npm`
# with any subcommand but those on NPM_ALLOWED (view, and publish of a `.tgz` named right after
# it), is `docker` with one of DOCKER_DEPENDENCY (build, run, compose, exec) or a long form on
# DOCKER_DEPENDENCY_LONG (`docker container run`, `docker image build`, `docker buildx build`, …),
# past docker's global options (`--config /tmp/c`, `-H tcp://x`, `-D`), is a path with a
# `scripts/` or `out/` component (`./scripts/x`, `../../scripts/x`, `out/x`,
# `"$GITHUB_WORKSPACE/out/x"`), other than one of the two repository shell scripts
# NOT_DEPENDENCY_SCRIPTS names (.github/scripts/release-push.sh and
# .github/scripts/check-private-names.sh) run from the checkout, or is `release-push.sh
# fold-notes`, at any path or none (`.github/scripts/release-push.sh fold-notes`), which runs
# changie: release-push.sh's other commands are not dependency code, and fold-notes is. A command
# runs one of the two from the checkout only when its word is the script's path behind
# `$GITHUB_WORKSPACE/` (or `${GITHUB_WORKSPACE}/`): a bare or `./` path, a `../` in front, or a
# path from another directory names whatever file sits there once a working-directory or a `cd`
# earlier in the `run:` has moved the shell, which this check cannot follow, so it is dependency
# code. check-private-names.sh is not dependency code: it sources nothing, and runs bash builtins
# and dirname, mktemp, rm, git (ls-files), grep, find, sort, xargs, env, sh, tr, cat and wc, all
# the runner's own; it is the one script a job holding the deployment's term list runs
# (release.yaml's claude_envoy_names and release-notes-names.yaml's job, through
# .github/actions/private-names-packed-plugin and .github/actions/private-names-release-notes).
# The
# classification is of a command a step runs, never of a word a comment or another command's
# argument holds: a script named only that way is no exemption. A step whose `uses:` is a local
# action and no DEPENDENCY_USES entry is read through that action's action.yml: a composite
# action's steps are read as a job's steps are (each `run:` under its own `shell:`), and any other
# kind of action, or one this check cannot read, runs dependency code. Command position is found
# by `.github/scripts/shell_commands.py` (`commands_of`), whose docstring gives the rules it reads a
# `run:` by, as bash does: comments, quotes, line joins, substitutions, redirections, the
# assignments and keywords before a command, and an interpreter, `-c` or `eval` looked through. A
# variable in command position (`"$tool" build`), a command behind `xargs` or `nohup`, or a
# program not on the list passes here; the job-level tripwire (`release-push.sh no-credential`)
# and review hold the rest.
#
# What this check is for, so it is held to the right bar: it pins the release workflows' shape
# against drift — a later well-meant edit that puts dependency code, a cache or a credential in the
# wrong job, or loosens a checkout — not against someone editing a workflow file to hide a command
# from it, who could as well remove this check from ci.yaml. The build/publish split is
# the boundary; this check is a tripwire on that boundary, and the pass-through class above (a
# variable in command position, `xargs`, `nohup`, an unlisted program, a `bun` behind `docker -Dl
# debug run`) is accepted on that ground.
# Scope: exactly the four workflows WORKFLOWS lists: release.yaml, release-envoy-listener.yaml,
# release-envoy-infra.yaml, and release-notes-names.yaml, the reusable workflow that release.yaml's
# and the listener's notes names jobs call, whose job holds the deployment's term list. Rules 1-3
# and 6 read every job that has `steps:`; a job that calls a reusable workflow (`uses:` and no
# steps: those names jobs, and release.yaml's worker_image) is out of their scope, since what it
# runs is that workflow's own jobs, which this check reads when the workflow is on WORKFLOWS
# (release-notes-names.yaml is; worker-image.yaml, which builds the worker image, is not). Rule 7
# reads every such job. Rule 4 reads the job it names. Rule 5 reads every job's `if:`, steps or
# not.
#
# Run from anywhere: .github/scripts/check-release-credentials.sh
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
import json
import re
import sys
from pathlib import Path

import yaml

from shell_commands import commands_of


class LineLoader(yaml.SafeLoader):
    """SafeLoader that records each mapping's line, so a problem names where to edit."""


class Mapping(dict):
    """A YAML mapping that remembers where each key was written."""

    lines: dict[str, int]


def _mapping_with_line(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.lines = {
        key.value: key.start_mark.line + 1 for key, _ in node.value if hasattr(key, "value")
    }
    return mapping


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_line)

RELEASE = Path(".github/workflows/release.yaml")
LISTENER = Path(".github/workflows/release-envoy-listener.yaml")
INFRA = Path(".github/workflows/release-envoy-infra.yaml")
NOTES_NAMES = Path(".github/workflows/release-notes-names.yaml")

# What makes a job credentialed: one of these scopes, or the deploy key's name anywhere in it, by
# dot or by index (`secrets.DEPLOY_KEY`, `secrets['DEPLOY_KEY']`, `secrets["DEPLOY_KEY"]`), in any
# casing, as GitHub resolves it. The env key `DEPLOY_KEY` the allowed site carries stays exact:
# release-push.sh reads `$DEPLOY_KEY`.
WRITE_SCOPES = ("contents", "id-token", "attestations", "actions")
DEPLOY_KEY_REF = re.compile(
    r"secrets\s*(\.\s*DEPLOY_KEY\b|\[\s*['\"]DEPLOY_KEY['\"]\s*\])", re.IGNORECASE
)
DEPLOY_KEY_VALUE = re.compile(r"\$\{\{\s*secrets\.DEPLOY_KEY\s*\}\}", re.IGNORECASE)
RELEASE_PUSH = ".github/scripts/release-push.sh"
PRIVATE_NAMES_CHECK = ".github/scripts/check-private-names.sh"
# The repository shell scripts under a scripts/ directory that are no dependency code, each at
# this exact path (the header says what each runs).
NOT_DEPENDENCY_SCRIPTS = (RELEASE_PUSH, PRIVATE_NAMES_CHECK)
# The workspace-root prefixes a command word must carry to run one of NOT_DEPENDENCY_SCRIPTS from
# the checkout, wherever its step runs.
WORKSPACE_ANCHORS = ("$GITHUB_WORKSPACE/", "${GITHUB_WORKSPACE}/")
TRIPWIRE = f"{RELEASE_PUSH} no-credential"
# The one release-push.sh command a step holding the deploy key may run: a release pushes its
# tag, and nothing else pushes.
PUSH_COMMAND = "release-push.sh push-tag"
# The workflows this check reads, in this order: a workflow joins the check here.
WORKFLOWS = [RELEASE, LISTENER, INFRA, NOTES_NAMES]
# A secret other than GITHUB_TOKEN, which a build job must not hold: `secrets.X` or `secrets['X']`
# for any X but GITHUB_TOKEN, `toJSON(secrets)` (the context and functions are case-insensitive,
# as are secret names), or a bare `secrets` inside a `${{ … }}` expression (`${{ secrets }}`,
# `${{ fromJSON(secrets) }}`), matched whole. A `secrets` right after a word character, `.`, `/` or
# `-` is part of another name (`agent-secrets/bin`), not the context.
OTHER_SECRET = re.compile(
    r"(?<![\w./-])secrets\s*(?:\.\s*(?!GITHUB_TOKEN\b)\w+|\[\s*(?!['\"]GITHUB_TOKEN['\"]\s*\])[^\]]*\])"
    r"|\btojson\s*\(\s*secrets\s*\)"
    r"|\$\{\{(?:(?!\}\}).)*?(?<![\w./-])secrets\b(?!\s*[.\[])(?:(?!\}\}).)*\}\}",
    re.IGNORECASE,
)

# What makes a step run dependency code. changie is a third-party binary, which `release-push.sh
# fold-notes` runs in each release's build job to render that release's changelog entries.
DEPENDENCY_COMMANDS = {
    "bun", "bunx", "go", "node", "npx", "pulumi", "mise", "make",
    "python", "python3", "pip", "uv", "cargo", "sudo", "changie",
}
# npm runs a package's scripts under every subcommand (install, ci, exec, run, test, …) but these
# two: view reads the registry, and publish of a tarball (the word right after `publish` ending in
# `.tgz`, where the release jobs put it) uploads the tarball it is handed and runs no package
# script. A publish of a directory runs the package's prepack, so it is dependency code, and a
# `.tgz` behind a flag is one (`npm publish --tag x.tgz` publishes the directory under the tag
# x.tgz): NO_TARBALL says why.
NPM_ALLOWED = {"view", "publish"}
NO_TARBALL = "without a tarball right after it (the tarball must be the word after `publish`; a directory publish runs the package's prepack)"
# docker runs a container's code under these, and under the long forms of the same on
# DOCKER_DEPENDENCY_LONG; `docker login`, `docker buildx imagetools`, `docker image ls` and
# `docker container ls` move only credentials, manifests and listings. The subcommand is the first
# word past docker's global options: every leading `-` word, and the value after one on
# DOCKER_GLOBAL_VALUES (`--config /tmp/c`, `-H tcp://x`; `--config=/tmp/c` is one word already).
DOCKER_DEPENDENCY = {"build", "run", "compose", "exec"}
DOCKER_DEPENDENCY_LONG = {
    ("container", "run"), ("container", "exec"), ("image", "build"), ("buildx", "build"),
    ("buildx", "bake"),
}
DOCKER_GLOBAL_VALUES = {
    "--config", "-c", "--context", "-H", "--host", "-l", "--log-level", "--tlscacert",
    "--tlscert", "--tlskey",
}
# The shells whose `run:` the tokeniser below reads, by the first word of the shell the step runs
# under: its `shell:`, else the job's `defaults.run.shell`, else the workflow's, else bash
# (GitHub's `bash`, `sh` or a template such as `bash -e {0}`); any other is an interpreter whose
# body it cannot read.
SHELLS = {"bash", "sh"}
DEPENDENCY_USES = (
    "actions/setup-go",
    "./.github/actions/setup-bun",
    "oven-sh/setup-bun",
    "./.github/actions/install-",
    "docker/build-push-action",
    "docker/setup-qemu-action",
)
DEPENDENCY_PATH = re.compile(r"(^|/)(scripts|out)/")
# What a word sheds to become a path relative to the checkout: $GITHUB_WORKSPACE/, ./ and ../.
PATH_PREFIX = re.compile(r"^(\$\{?GITHUB_WORKSPACE\}?/)?(\.\.?/)*")

# The only actions a credentialed job may use, compared before the `@` and lower-cased (GitHub
# resolves `Actions/Checkout@…` too).
CREDENTIALED_USES = {
    "actions/checkout",
    "actions/download-artifact",
    "actions/setup-node",
    "actions/attest-build-provenance",
    "docker/login-action",
}


def is_release_push(command: str) -> bool:
    """Whether the command word names release-push.sh, at any path: what it is left with after a
    path relative to the checkout sheds its prefix, so `.github/scripts/release-push.sh`,
    `./release-push.sh` and a bare `release-push.sh` all read as the one script. dependency_command
    asks it of a `fold-notes`, and rule 6 of a step that needs a checkout before it."""
    return PATH_PREFIX.sub("", command).rsplit("/", 1)[-1] == "release-push.sh"


def from_checkout(command: str) -> bool:
    """Whether the command word runs one of NOT_DEPENDENCY_SCRIPTS from the checkout: its path
    behind a WORKSPACE_ANCHORS prefix. A bare or `./` path, a `../` or any other prefix anchors
    nothing, since a working-directory or a `cd` may have moved the shell."""
    for anchor in WORKSPACE_ANCHORS:
        if command.startswith(anchor):
            return command[len(anchor):] in NOT_DEPENDENCY_SCRIPTS
    return False


def dependency_command(words: list[str]) -> str | None:
    """How the message names the command when it is dependency code (`'bun'`, `'npm ci'`, `'docker
    container run'`, `'npm publish' without a tarball right after it (…)`), else None."""
    command = words[0]
    if command in DEPENDENCY_COMMANDS:
        return f"'{command}'"
    if command == "npm":
        if len(words) < 2 or words[1].startswith("-"):
            return f"'{command}'"
        if words[1] not in NPM_ALLOWED:
            return f"'npm {words[1]}'"
        if words[1] == "publish":
            tarball = words[2] if len(words) > 2 else ""
            if tarball.startswith("-") or not tarball.endswith(".tgz"):
                return f"'npm publish' {NO_TARBALL}"
        return None
    if command == "docker":
        rest = words[1:]
        while rest and rest[0].startswith("-"):
            if rest.pop(0) in DOCKER_GLOBAL_VALUES and rest:
                rest.pop(0)
        if rest and rest[0] in DOCKER_DEPENDENCY:
            return f"'docker {rest[0]}'"
        if tuple(rest[:2]) in DOCKER_DEPENDENCY_LONG:
            return f"'docker {rest[0]} {rest[1]}'"
        return None
    path = PATH_PREFIX.sub("", command)
    if is_release_push(command) and words[1:2] == ["fold-notes"]:
        return f"'{command} fold-notes' (it runs changie)"
    # The exemptions are the checkout's own scripts, run from the checkout (from_checkout), not a
    # name counted at any path or under any directory: release-push.sh's other commands and
    # check-private-names.sh are no dependency code, and a script elsewhere is not one of them,
    # whatever it is called.
    if DEPENDENCY_PATH.search(path) and not from_checkout(command):
        return f"'{command}'"
    return None


def shell_of(step: Mapping, job: Mapping, workflow: Mapping) -> tuple[str, int | None]:
    """The first word of the shell the step's `run:` runs under, and the line of the `shell:` that
    sets it: the step's own, else the job's `defaults.run.shell`, else the workflow's, else bash,
    GitHub's default on Linux, set nowhere."""
    scopes = [step]
    for scope in (job, workflow):
        defaults = scope.get("defaults")
        run = defaults.get("run") if isinstance(defaults, Mapping) else None
        if isinstance(run, Mapping):
            scopes.append(run)
    for scope in scopes:
        shell = scope.get("shell")
        if isinstance(shell, str):
            return (shell.split() or [""])[0], scope.lines.get("shell")
    return "bash", None


# The job and the workflow a composite action's step is read under: none of either's defaults
# reaches it.
NO_SCOPE = Mapping()
NO_SCOPE.lines = {}


def local_action_code(uses: str, seen: tuple) -> str | None:
    """How a local action runs dependency code, read off its action.yml in the checkout: each step
    of a composite action as dependency_code reads a job's step, under no job or workflow default;
    any other kind of action, or one this check cannot read, as dependency code itself."""
    if uses in seen:
        return None
    directory = Path(uses)
    files = [directory / name for name in ("action.yml", "action.yaml") if (directory / name).is_file()]
    if not files:
        return "has no action.yml in the checkout for this check to read"
    try:
        action = yaml.load(files[0].read_text(), Loader=LineLoader)
    except yaml.YAMLError:
        return f"has a {files[0].name} that is not valid YAML"
    runs = action.get("runs") if isinstance(action, Mapping) else None
    using = runs.get("using") if isinstance(runs, Mapping) else None
    if using != "composite":
        return f"runs '{using}', a program this check cannot read"
    found = []
    for index, step in enumerate(runs.get("steps") or []):
        if isinstance(step, Mapping):
            if what := dependency_code(step, NO_SCOPE, NO_SCOPE, seen + (uses,)):
                found.append(f"{describe(step, index)} {what}")
    return "; ".join(found) or None


def action_uses(uses: str, seen: tuple = ()) -> list[str]:
    """The remote actions a step's `uses:` brings into its job: itself, or, for a local composite
    action, every remote `uses:` of its steps at any depth. A local action this check cannot read
    brings one entry that names it as such, which no allow-list holds."""
    if not uses.startswith("./"):
        return [uses]
    if uses in seen:
        return []
    directory = Path(uses)
    files = [directory / name for name in ("action.yml", "action.yaml") if (directory / name).is_file()]
    if not files:
        return [f"{uses} (no action.yml in the checkout to read)"]
    try:
        action = yaml.load(files[0].read_text(), Loader=LineLoader)
    except yaml.YAMLError:
        return [f"{uses} (an action.yml this check cannot parse)"]
    runs = action.get("runs") if isinstance(action, Mapping) else None
    if not isinstance(runs, Mapping) or runs.get("using") != "composite":
        return [f"{uses} (not a composite action)"]
    found = []
    for step in runs.get("steps") or []:
        if isinstance(step, Mapping) and isinstance(step.get("uses"), str):
            found += action_uses(step["uses"], seen + (uses,))
    return found


def dependency_code(step: Mapping, job: Mapping, workflow: Mapping, seen: tuple = ()) -> str | None:
    """How the step runs dependency code (`runs 'bun'`, `uses 'actions/setup-go@…'`, `runs its body
    under shell 'python', not bash or sh`, `uses './.github/actions/x', whose step …`), or None."""
    found = []
    uses = step.get("uses")
    if isinstance(uses, str) and uses.lower().startswith(DEPENDENCY_USES):
        found.append(f"uses '{uses}'")
    elif isinstance(uses, str) and uses.startswith("./"):
        if what := local_action_code(uses, seen):
            joined = "whose" if what.startswith("step") else "which"
            found.append(f"uses '{uses}', {joined} {what}")
    run = step.get("run")
    if isinstance(run, str):
        interpreter, _ = shell_of(step, job, workflow)
        if interpreter not in SHELLS:
            found.append(f"runs its body under shell '{interpreter}', not bash or sh")
        else:
            commands = []
            for words in commands_of(run):
                what = dependency_command(words)
                if what and what not in commands:
                    commands.append(what)
            if commands:
                found.append("runs " + ", ".join(commands))
    return " and ".join(found) or None


def write_scopes(job: Mapping, workflow: Mapping) -> list[str]:
    """The write scopes the job holds: its own `permissions`, or, when it declares none, the
    workflow's, which GitHub gives a job that has no block of its own."""
    permissions = job["permissions"] if "permissions" in job else workflow.get("permissions")
    if permissions == "write-all":
        return list(WRITE_SCOPES)
    if not isinstance(permissions, Mapping):
        return []
    return [scope for scope in WRITE_SCOPES if permissions.get(scope) == "write"]


def needs_of(job: Mapping) -> list:
    """The jobs a job needs, as GitHub reads `needs:`: one name, or a list of them."""
    needs = job.get("needs")
    return [needs] if isinstance(needs, str) else needs if isinstance(needs, list) else []


# The path of the workflow-level `env:` texts, which every job's scan takes in with its own: GitHub
# gives those variables to every step of every job.
WORKFLOW_ENV = ("workflow", "env")


def texts_of(node, path=(), line=0) -> list[tuple[tuple, int, str]]:
    """Every key and every string value under node, with where it is: (path, line, text)."""
    texts = []
    if isinstance(node, Mapping):
        for key, value in node.items():
            key_line = node.lines.get(key, line)
            texts.append((path + (key,), key_line, str(key)))
            texts += texts_of(value, path + (key,), key_line)
    elif isinstance(node, list):
        for index, item in enumerate(node):
            texts += texts_of(item, path + (index,), line)
    elif isinstance(node, str):
        texts.append((path, line, node))
    return texts


def deploy_key_allowed(steps: list, path: tuple, text: str) -> bool:
    """`DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}` in the env of a step whose `run:` runs PUSH_COMMAND,
    the checkout's release-push.sh (from_checkout) with `push-tag`, and nothing else: the dot form,
    exactly, as the one value of that key."""
    if len(path) != 4 or path[0] != "steps" or path[2:] != ("env", "DEPLOY_KEY"):
        return False
    if not DEPLOY_KEY_VALUE.fullmatch(text.strip()):
        return False
    run = steps[path[1]].get("run")
    return isinstance(run, str) and any(
        from_checkout(words[0]) and is_release_push(words[0]) and words[1:2] == ["push-tag"]
        for words in commands_of(run)
    )


def describe(step: Mapping, index: int) -> str:
    if isinstance(step.get("name"), str):
        return f"step '{step['name']}'"
    if isinstance(step.get("uses"), str):
        return f"step '{step['uses']}'"
    return f"step {index + 1}"


def describe_site(steps: list, path: tuple) -> str:
    """Where a text sits: `step 'Push tag' env.OTHER_KEY`, `env.DEPLOY_KEY` of the job, or `the
    workflow's env.DEPLOY_KEY` (WORKFLOW_ENV's texts, which every job inherits)."""
    if len(path) >= 2 and path[0] == "steps" and isinstance(path[1], int):
        rest = ".".join(str(part) for part in path[2:])
        return f"{describe(steps[path[1]], path[1])} {rest}".rstrip()
    if path[:2] == WORKFLOW_ENV:
        return "the workflow's " + ".".join(str(part) for part in path[1:])
    return ".".join(str(part) for part in path)


def line_of(step: Mapping, fallback: int) -> int:
    return min(step.lines.values(), default=fallback)


def is_checkout(step: Mapping) -> bool:
    """Whether the step checks the repository out: its `uses:` names actions/checkout, owner and
    repository compared lower-cased, as GitHub resolves them. Rules 3 and 6 ask it."""
    uses = step.get("uses")
    return isinstance(uses, str) and uses.lower().startswith("actions/checkout@")


problems: list[str] = []
credentialed_jobs = 0
build_jobs = 0


def problem(path: Path, line: int, message: str) -> None:
    problems.append(f"{path}:{line}: {message}")


def check_job(
    path: Path, name: str, job: Mapping, job_line: int, steps: list, workflow: Mapping
) -> None:
    global credentialed_jobs, build_jobs
    if not all(isinstance(step, Mapping) for step in steps):
        problem(
            path, job.lines.get("steps", job_line), f"job '{name}' has a step that is not a mapping"
        )
        return
    scopes = write_scopes(job, workflow)
    texts = texts_of(job, line=job_line) + texts_of(
        workflow.get("env"), WORKFLOW_ENV, workflow.lines.get("env", 1)
    )
    key_sites = [(site, line, text) for site, line, text in texts if DEPLOY_KEY_REF.search(text)]
    dependency = [
        (index, step, found)
        for index, step in enumerate(steps)
        if (found := dependency_code(step, job, workflow))
    ]

    # 3. Every checkout leaves no token behind, whatever the job holds.
    for index, step in enumerate(steps):
        if not is_checkout(step):
            continue
        with_ = step.get("with")
        if isinstance(with_, Mapping) and "persist-credentials" in with_:
            value = with_["persist-credentials"]
            if value is False:
                continue
            line = with_.lines.get("persist-credentials", line_of(step, job_line))
            found = f"found {json.dumps(value)}"
            if isinstance(value, str):
                found += ", a string"
        else:
            line = line_of(step, job_line)
            found = "it is not set"
        problem(
            path, line,
            f"job '{name}' {describe(step, index)} must set 'persist-credentials: false', the "
            f"boolean, so no token is left in the checkout ({found})",
        )

    # 6. release-push.sh is in the checkout, so a checkout comes before the step that runs it:
    # the steps before the job's first checkout are the ones that would run it without one. Of
    # those, only the steps the tokeniser reads: another shell's body is not its to parse.
    for index, step in enumerate(steps):
        if is_checkout(step):
            break
        run = step.get("run")
        if not isinstance(run, str):
            continue
        if shell_of(step, job, workflow)[0] not in SHELLS:
            continue
        if any(is_release_push(words[0]) for words in commands_of(run)):
            problem(
                path, line_of(step, job_line),
                f"job '{name}' {describe(step, index)} runs {RELEASE_PUSH}, but no "
                f"actions/checkout before it puts the script in the workspace, so the step would "
                f"exit 127 and the job would carry on",
            )

    if scopes or key_sites:
        credentialed_jobs += 1
        # 1a. No dependency code.
        for index, step, found in dependency:
            problem(
                path, line_of(step, job_line),
                f"job '{name}' holds a credential, but {describe(step, index)} {found}; "
                f"dependency code runs only in a job that holds none",
            )
        # 1b. Only the allow-listed actions.
        for index, step in enumerate(steps):
            uses = step.get("uses")
            if isinstance(uses, str) and uses.split("@", 1)[0].lower() not in CREDENTIALED_USES:
                problem(
                    path, step.lines.get("uses", line_of(step, job_line)),
                    f"job '{name}' holds a credential, but {describe(step, index)} uses '{uses}', "
                    f"which is not on the checker's allow-list for a job that holds a credential; "
                    f"add it there deliberately",
                )
        # 1c. No cache restored.
        if job.get("cache-mode") != "none":
            found = (
                f"found {json.dumps(job['cache-mode'])}" if "cache-mode" in job else "it is not set"
            )
            problem(
                path, job.lines.get("cache-mode", job_line),
                f"job '{name}' holds a credential, so it must set 'cache-mode: none' to restore "
                f"no cache ({found})",
            )
        # 1d. The deploy key reaches only the steps that run the push command.
        for key_path, line, text in key_sites:
            if not deploy_key_allowed(steps, key_path, text):
                problem(
                    path, line,
                    f"job '{name}' names secrets.DEPLOY_KEY at {describe_site(steps, key_path)}; "
                    f"the deploy key may appear only as 'DEPLOY_KEY: ${{{{ secrets.DEPLOY_KEY }}}}' "
                    f"in the env of a step whose run calls {PUSH_COMMAND}",
                )

    # A names job holds the deployment's term list: only the allow-listed actions run beside it, so
    # no third-party code reads the list from its step's environment or the runner's memory. A
    # local composite action is read through, at any depth (action_uses).
    if not (scopes or key_sites) and any(NAMES_SECRET.search(text) for _, _, text in texts):
        for index, step in enumerate(steps):
            uses = step.get("uses")
            if not isinstance(uses, str):
                continue
            for remote in action_uses(uses):
                if remote.split("@", 1)[0].lower() in CREDENTIALED_USES:
                    continue
                brought = "" if remote == uses else f", which brings in '{remote}',"
                problem(
                    path, step.lines.get("uses", line_of(step, job_line)),
                    f"job '{name}' holds the deployment's term list (secrets.PRIVATE_NAME_TERMS), "
                    f"but {describe(step, index)} uses '{uses}'{brought} which is not on the "
                    f"checker's allow-list for a job that holds a secret; add it there deliberately",
                )
    if dependency:
        build_jobs += 1
        index, step, found = dependency[0]
        what = f"{describe(step, index)} {found}"
        # 2. No credential, a known token, no error tolerance on the job, and the tripwire last,
        # under a shell, with nothing that could gate it away (`if:`) or let its failure pass
        # (`continue-on-error:`).
        if "permissions" not in job and "permissions" not in workflow:
            problem(
                path, job_line,
                f"job '{name}' runs dependency code ({what}), but declares no permissions and the "
                f"workflow declares none, so its token is whatever the repository grants a job "
                f"that declares no permissions, which this check cannot read; set 'permissions: "
                f"contents: read' on the job",
            )
        for scope in scopes:
            line = job.lines.get("permissions") or workflow.lines.get("permissions", job_line)
            held = f"holds '{scope}: write'"
            if "permissions" not in job:
                held += " through the workflow's permissions, having none of its own"
            problem(path, line, f"job '{name}' runs dependency code ({what}), but {held}")
        for _, line, text in texts:
            if secret := OTHER_SECRET.search(text):
                problem(
                    path, line,
                    f"job '{name}' runs dependency code ({what}), but its text names "
                    f"{secret.group(0)}; a build job holds no secret but GITHUB_TOKEN",
                )
        if "continue-on-error" in job:
            problem(
                path, job.lines.get("continue-on-error", job_line),
                f"job '{name}' runs dependency code ({what}), so it must carry no "
                f"'continue-on-error:' (a true or an expression there reports a failed tripwire "
                f"as success to the jobs that need it)",
            )
        last = steps[-1]
        run = last.get("run")
        if not (isinstance(run, str) and run.strip() == TRIPWIRE and "uses" not in last):
            problem(
                path, line_of(last, job_line),
                f"job '{name}' runs dependency code ({what}), so its last step must be the "
                f"tripwire 'run: {TRIPWIRE}'",
            )
        else:
            for key in ("if", "continue-on-error"):
                if key in last:
                    problem(
                        path, last.lines.get(key, line_of(last, job_line)),
                        f"job '{name}' runs dependency code ({what}), so its tripwire step must "
                        f"carry no '{key}:'",
                    )
            shell, shell_line = shell_of(last, job, workflow)
            if shell not in SHELLS:
                problem(
                    path, shell_line or line_of(last, job_line),
                    f"job '{name}' runs dependency code ({what}), so its tripwire step must run "
                    f"under bash or sh (found '{shell}'), which is what runs {TRIPWIRE}",
                )


# A job-level `if:`, read as GitHub does. A string literal is text, not a call or a context read
# (GitHub quotes with `'…'`, escaping a quote as `''`), so the strip removes each one first — but
# for the quoted job id of an index (`needs['x']`), STRING_LITERAL's first alternative, which the
# strip keeps for NEEDS_OUTPUTS and NEEDS_RESULT to read. Those take a job by dot
# (`needs.x.outputs`) or by index (`needs['x'].result`), a job id being letters, digits, `_` and
# `-`; the context and functions are case-insensitive, and whitespace may sit around `.`, `(`,
# `)`, `[` and `]`.
STRING_LITERAL = re.compile(r"(\bneeds\s*\[\s*'[\w-]+'\s*\])|'(?:[^']|'')*'", re.IGNORECASE)
ALWAYS = re.compile(r"\balways\s*\(\s*\)", re.IGNORECASE)
NEEDS_REF = r"\bneeds\s*(?:\.\s*([\w-]+)|\[\s*['\"]([\w-]+)['\"]\s*\])\s*\.\s*"
NEEDS_OUTPUTS = re.compile(NEEDS_REF + r"outputs\b", re.IGNORECASE)
NEEDS_RESULT = re.compile(NEEDS_REF + r"result\b", re.IGNORECASE)


def ancestors(jobs: Mapping, name: str) -> list[str]:
    """Every job upstream of the named one through `needs`, at any distance, in the file's job
    order. A visited set keeps a cycle from looping the walk; a `needs` entry that names no job of
    the file is ignored (GitHub refuses such a workflow at parse)."""
    seen = {name}
    pending = [name]
    while pending:
        for need in needs_of(jobs[pending.pop()]):
            if need not in seen and isinstance(jobs.get(need), Mapping):
                seen.add(need)
                pending.append(need)
    return [job for job in jobs if job != name and job in seen]


def check_conditions(path: Path, jobs: Mapping) -> None:
    """5. Each job-level `if:`, steps or not (a step's `if:` is never read: its implicit success()
    is over the job's own earlier steps, not over `needs`). A non-string `if:` (`if: true`) calls
    no always() and reads nothing."""
    for name, job in jobs.items():
        if not isinstance(job, Mapping) or "if" not in job:
            continue
        cond = job["if"]
        cond = STRING_LITERAL.sub(r"\1", cond) if isinstance(cond, str) else ""
        line = job.lines.get("if", jobs.lines.get(name, 1))
        if ALWAYS.search(cond):
            # 5a. always() turns GitHub's gating off, so every job whose outputs the condition
            # reads must have its result tested, else a failed or skipped one is read through its
            # empty outputs (`'' != 'true'` is true).
            tested = {
                (match.group(1) or match.group(2)).lower()
                for match in NEEDS_RESULT.finditer(cond)
            }
            missing: list[str] = []
            for match in NEEDS_OUTPUTS.finditer(cond):
                read = match.group(1) or match.group(2)
                if read not in missing and read.lower() not in tested:
                    missing.append(read)
            if missing:
                reads = ", ".join(f"needs.{read}.outputs" for read in missing)
                tests = ", ".join(f"needs.{read}.result" for read in missing)
                problem(
                    path, line,
                    f"job '{name}' calls always() in its if: and reads {reads}, but does not test "
                    f"{tests}; always() turns GitHub's own gating off, so a failed or skipped job "
                    f"is read through its empty outputs",
                )
            continue
        # 5b. Without always(), GitHub's implicit success() decides first (an explicit success()
        # is the implicit one), and it is false whenever any job upstream through `needs`, at any
        # distance, was skipped (actions/runner#491): a job upstream with an `if:` of its own can
        # be, so the condition never decides anything.
        skippable = [ancestor for ancestor in ancestors(jobs, name) if "if" in jobs[ancestor]]
        if skippable:
            upstream = ", ".join(f"'{ancestor}'" for ancestor in skippable)
            carry = (
                f"job {upstream} upstream of it through needs carries an if: of its own"
                if len(skippable) == 1
                else f"jobs {upstream} upstream of it through needs carry an if: of their own"
            )
            problem(
                path, line,
                f"job '{name}' has an if: that calls no always(), but {carry}; GitHub's implicit "
                f"success() is then false whenever any job upstream was skipped, whatever this "
                f"if: says, so the if: must call always() and test needs.<job>.result for each "
                f"job it reads",
            )

# 7. A job that calls a reusable workflow hands a secret only to one this check reads.
LOCAL_CALLS = {f"./{workflow}" for workflow in WORKFLOWS}


def check_call(path: Path, name: str, job: Mapping, job_line: int) -> None:
    uses = job.get("uses")
    if not isinstance(uses, str):
        return
    if uses in LOCAL_CALLS:
        # A listed workflow's own jobs are read, but `secrets: inherit` hands it every secret and
        # hides from rule 8 which one the call passes: a names job called so stops counting as one.
        if job.get("secrets") == "inherit":
            problem(
                path, job.lines.get("secrets", job_line),
                f"job '{name}' calls {uses} with 'secrets: inherit', which hands it every secret "
                f"and hides from rule 8 the one it needs; pass that one secret by name",
            )
        return
    named = [
        (line, secret.group(0))
        for _, line, text in texts_of(job, line=job_line)
        if (secret := OTHER_SECRET.search(text))
    ]
    if job.get("secrets") == "inherit":
        named.append((job.lines.get("secrets", job_line), "every secret ('secrets: inherit')"))
    for line, secret in named:
        problem(
            path, line,
            f"job '{name}' calls {uses}, a workflow this check does not read, but its text names "
            f"{secret}; a job that calls a reusable workflow hands a secret but GITHUB_TOKEN only "
            f"to a workflow on WORKFLOWS, called as './' and its path",
        )

# 8. A step that starts publishing a build's output waits for the names job that checks it.
NAMES_SECRET = re.compile(
    r"(?<![\w./-])secrets\s*(?:\.\s*PRIVATE_NAME_TERMS\b|\[\s*['\"]PRIVATE_NAME_TERMS['\"]\s*\])",
    re.IGNORECASE,
)
SUCCESS_TEST = re.compile(NEEDS_REF + r"result\s*==\s*'success'", re.IGNORECASE)


def check_names_gates(path: Path, jobs: Mapping) -> None:
    names_of: dict[str, list[str]] = {}
    for name, job in jobs.items():
        if isinstance(job, Mapping) and any(NAMES_SECRET.search(text) for _, _, text in texts_of(job)):
            for build in needs_of(job):
                names_of.setdefault(str(build).lower(), []).append(str(name))
    for name, job in jobs.items():
        if not isinstance(job, Mapping) or not isinstance(job.get("steps"), list):
            continue
        for index, step in enumerate(job["steps"]):
            cond = step.get("if") if isinstance(step, Mapping) else None
            if not isinstance(cond, str) or "||" in cond:
                continue
            tested = {
                (match.group(1) or match.group(2)).lower() for match in SUCCESS_TEST.finditer(cond)
            }
            for build, names in names_of.items():
                if build not in tested or str(name) in names:
                    continue
                for names_job in names:
                    if names_job.lower() in tested:
                        continue
                    problem(
                        path, step.lines.get("if", line_of(step, jobs.lines.get(name, 1))),
                        f"job '{name}' {describe(step, index)} tests needs.{build}.result == "
                        f"'success' but not needs.{names_job}.result == 'success'; {names_job} "
                        f"checks {build}'s output for the deployment's private names, so the step "
                        f"that starts publishing it waits for that check, else a finding there "
                        f"publishes anyway",
                    )




loaded: dict[Path, tuple[Mapping, Mapping]] = {}
for path in WORKFLOWS:
    try:
        text = path.read_text()
    except FileNotFoundError:
        problems.append(f"{path} is missing")
        continue
    try:
        doc = yaml.load(text, Loader=LineLoader)
    except yaml.YAMLError as error:
        problems.append(f"{path} is not valid YAML: {error}")
        continue
    jobs = doc.get("jobs") if isinstance(doc, Mapping) else None
    if not isinstance(jobs, Mapping):
        problems.append(f"{path}:1: has no jobs mapping")
        continue
    loaded[path] = (doc, jobs)
    for name, job in jobs.items():
        if not isinstance(job, Mapping):
            continue
        if not isinstance(job.get("steps"), list):
            check_call(path, str(name), job, jobs.lines.get(name, 1))
            continue
        check_job(path, str(name), job, jobs.lines.get(name, 1), job["steps"], doc)


def required_job(path: Path, name: str, must: str) -> tuple[Mapping, int] | None:
    """The named job and its line, or None after reporting that the workflow lacks it."""
    doc, jobs = loaded[path]
    job = jobs.get(name)
    if isinstance(job, Mapping):
        return job, jobs.lines.get(name, 1)
    problem(path, doc.lines.get("jobs", 1), f"no job '{name}', which {must}")
    return None


# 4. The listener's publish job moves `latest` and tags one run at a time, and finishes.
if LISTENER in loaded:
    path = LISTENER
    must = (
        "must run in concurrency group 'release-envoy-listener' with 'cancel-in-progress: false', "
        "the boolean, and 'queue: max'"
    )
    if found := required_job(path, "release_publish", must):
        job, job_line = found
        concurrency = job.get("concurrency")
        group = concurrency.get("group") if isinstance(concurrency, Mapping) else None
        cancel = concurrency.get("cancel-in-progress") if isinstance(concurrency, Mapping) else None
        queue = concurrency.get("queue") if isinstance(concurrency, Mapping) else None
        if group != "release-envoy-listener" or cancel is not False or queue != "max":
            problem(
                path, job.lines.get("concurrency", job_line),
                f"job 'release_publish' {must} (found concurrency: {json.dumps(concurrency)})",
            )

# 5. Every job-level `if:` either calls always() and tests the result of each job it reads, or
# belongs to a job with no skippable ancestor, where GitHub's implicit success() can decide.
for path, (_, jobs) in loaded.items():
    check_conditions(path, jobs)

# 8. A step that starts publishing a build's output waits for the names job that checks it.
for path, (_, jobs) in loaded.items():
    check_names_gates(path, jobs)

if problems:
    for entry in problems:
        print(f"::error::{entry}")
    print(f"{len(problems)} release credential problem(s)", file=sys.stderr)
    sys.exit(1)

print(
    f"check-release-credentials: {credentialed_jobs} credentialed job(s) and {build_jobs} build "
    f"job(s) across {len(WORKFLOWS)} workflows keep credentials out of jobs that run "
    f"dependency code"
)
PY
