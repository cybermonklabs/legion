"""Rule 22 of check-ci-filters.sh: a required job waits on the network only under a bound of its
own, and its limits leave room for each such wait to end by name.

Every job of ci.yaml, and of a workflow it calls, is on the path of ci-ok, the check main's
ruleset requires, or is pr-title, the other one, so an install there that waits without a bound
fails a pull request for a cause outside its change: on 2026-11-20 run 44665181416's daemon-go job
waited in `sudo apt-get update` from 03:05:59Z until the job's 70-minute limit, on a mirror that
had sent an answer's headers and then all but stopped, which apt's own timeout, a wait for the
next byte, never ends. The one way such a job may wait on the network is through
.github/scripts/network-install.sh, which bounds each attempt, tries it a fixed number of times
and fails by name (its header says how each kind's bound was measured; `--bounds` prints its
table, which this rule reads).

ci_filter_coverage.main() runs check_network_installs over ci.yaml and the workflows it calls.
Its docstring says what the rule reads, refuses, counts and leaves alone. This module is the
policy; the syntax and the reading of one command live apart, each read by it alone or with
another check: .github/scripts/shell_commands.py (a shell text), dockerfile_reader.py (a
Dockerfile, which check-image-trigger-paths.sh reads too), script_paths.py (the path of this
repository a command word names) and network_commands.py (what one command fetches).
"""

import hashlib
import json
import os
import posixpath
import re
import subprocess
from dataclasses import replace
from functools import cache

import dockerfile_reader
from ci_filter_coverage import REPO, error, load_repository_yaml, read_repository_text, squash, step_label
from network_commands import (APT, APT_DOWNLOAD_ONLY, APT_DPKG_VERBS, APT_VALUE_OPTIONS, DOCKER_VALUE_OPTIONS,
                              GO_DOWNLOADS, LEAN_COMMANDS, docker_command, elan_fetch, from_verb, go_fetch,
                              go_test_timeout, local_urls, network_install)
from script_paths import Site, environment, repository_script, resolved, unfound_script
from shell_commands import STDIN_SHELLS, WRAPPERS, command_of, literal_word, pipelines_of, read_shell, substitution

NETWORK_INSTALL = ".github/scripts/network-install.sh"
ACTIONS_PREFIX = "./.github/actions/"
BUILD_ACTION = "docker/build-push-action@"
# The action that pulls a job's test images before its tests, and, under `images:`, its build's FROM
# images.
PULL_ACTION = "./.github/actions/pull-test-images"
# The variables network-install.sh reads in place of its bounds for its own tests alone; this rule
# asks it for the real ones with them unset.
NETWORK_INSTALL_SEAMS = ("NETWORK_INSTALL_BOUND_SECONDS", "NETWORK_INSTALL_KILL_AFTER_SECONDS",
                         "NETWORK_INSTALL_PAUSE_SECONDS")
# GitHub's limit for a job or a step that sets no timeout-minutes.
DEFAULT_MINUTES = 360
# The installers no required job runs, so no run measures a bound for them: network-install.sh has
# no kind for them, and another installer's bound was measured on that installer's installs alone.
# Rule 22 refuses each of their installs by name, wrapped or not; the job that adds one measures its
# runs and adds its kind.
UNMEASURED = {"yarn": "no required job installs with Yarn, so no run measures a bound for it"}
# What a pull the runner or a builder makes costs a job's limit: the runner pulls each `services:`
# and `container:` image with `docker pull` and no timeout of its own (actions/runner retries it
# only after a non-zero exit), and a builder pulls each FROM no earlier step left on the runner, and
# no workflow can give either pull a bound, so a job's or a step's limit is theirs. The slowest of
# the 304 green "Initialize containers" steps of the 20 newest merge-group runs pulled its one image
# in 62.7 s (dispatch-browser's webkit-alone, job 135946192641, the 1 GB browser image; round 3's
# evidence, measure/initialize-containers.txt). 2 × 62.7 s is 125.4 s, so 180 s, rounded up to the
# next minute as network-install.sh's kinds are.
PULL_SECONDS = 180
# The Dockerfiles whose RUN installs rule 22 counts against their build step's own limit rather
# than a bound of their own, each only while it holds the bytes whose sha256 is given. The browser
# image's recipe: browser-image.yaml builds it only when the recipe changed, under its step's own
# 2-minute limit, from the context .github/docker, which holds no network-install.sh; and any edit
# to the recipe moves its hash, so check-ci-images.sh's rule 10 already needs the image built
# again and its .github/ci-images.yaml row moved. That edit wraps or time-bounds its installs (a
# --max-time on its curl), at no further cost: the hash below stops matching, and this rule then
# reads the recipe as it reads every other.
PINNED_RECIPES = {
    ".github/docker/browser.Dockerfile": "b44a4dabdf97d209de663fe05d4b5e21234511703fe4f4827da9c5960ddf8b99",
}


def _unmeasured(kind: str) -> str:
    """Why an install of an UNMEASURED kind has no bound, and what the job that adds one does."""
    return (f"no bound measured for {kind}: {UNMEASURED[kind]}, and npm's bound or any other kind's was measured "
            f"on that kind's installs alone. The job that adds a {kind} install measures that install's own runs "
            f"and adds a '{kind}' kind to {NETWORK_INSTALL}'s table")


@cache
def network_install_bounds() -> dict | str:
    """network-install.sh's table, read by running it with `--bounds` and its test seams unset:
    {kind: (bound s, attempts, s before SIGKILL, pause s)}, or why it could not be read."""
    script = REPO / NETWORK_INSTALL
    if not script.is_file():
        return f"{NETWORK_INSTALL} is not in the repository"
    env = {name: value for name, value in os.environ.items() if name not in NETWORK_INSTALL_SEAMS}
    result = subprocess.run(["sh", str(script), "--bounds"], capture_output=True, text=True, env=env,
                            timeout=60, check=False)
    try:
        if result.returncode != 0:
            raise ValueError(f"exit {result.returncode}")
        return {kind: tuple(int(field) for field in fields)
                for kind, *fields in (line.split() for line in result.stdout.splitlines())
                if len(fields) == 4}
    except ValueError as exc:
        return f"`{NETWORK_INSTALL} --bounds` did not print its table ({exc}): {squash(result.stderr)}"


def worst_case(bound: int, attempts: int, kill_after: int, pause: int) -> int:
    """The seconds a wrapped install can take before network-install.sh fails it by name: every
    attempt cut at its bound and killed after the wait for SIGTERM, with the pauses between them."""
    return attempts * (bound + kill_after) + (attempts - 1) * pause


def _minutes(holder: dict) -> int | None:
    """A job's or a step's timeout-minutes, GitHub's 360 where it sets none, or None where it is an
    expression this rule cannot read."""
    value = holder.get("timeout-minutes", DEFAULT_MINUTES)
    return value if isinstance(value, (int, float)) else None


def _fakes(shell, words: list) -> frozenset:
    """The commands a text puts first on the PATH it runs `words` under: each file it writes into
    the directory that PATH names first, where the PATH is set before that command alone
    (`PATH="$bin:$PATH" cmd`, `env -u X PATH=… cmd`) or for the whole text by an assignment
    (`export PATH="$bin:$PATH"`). autodeploy_test.sh writes its fake docker to "$bin/docker" (its
    line 55, `cat >"$bin/docker"`) and runs autodeploy.sh under `PATH="$bin:$PATH"` (its line 334),
    so in envoy-go-checks every docker call of autodeploy.sh reaches that fake and no registry. In
    the script such a command runs, and the scripts that one runs in turn, a command of such a name
    is the fake and no install; a script the same text runs without that PATH gets no fake, and
    every other command is read as anywhere else. A script the text sources (`source x`, `. x`) is
    read under every PATH the text sets for any of its commands: its functions run in the text,
    under the PATH of the command that calls them, which this reader does not follow.
    airlock-check.test.sh sources the k7 consumers and calls their curl only inside `env -u
    BASH_ENV PATH="$work/consumer-bin:$PATH" bash -c …`, where its fake curl is first. A directory
    a PATH names after the runner's own is no fake's: the real command comes first there."""
    own = shell.prefixes.get(tuple(words), {}).get("PATH")
    values = [value for _, value in shell.paths] if tuple(words) in shell.sourced else \
        [own] if own is not None else []
    values += [value for name, value in shell.assignments if name == "PATH"]
    firsts = {value.split(":", 1)[0] for value in values}
    return frozenset(posixpath.basename(target) for target in shell.writes
                     if posixpath.dirname(target) and posixpath.dirname(target) in firsts)


def _option_variables(assignments: list) -> dict:
    """The variables a text builds of options alone, as the words one expands to: every value it
    gives the name is empty, or, after the name itself where it starts with it (`forward="$forward
    -e $name"`), words of which the first is an option. An unquoted one before a docker run's image
    expands to those options, so the image is still the first word no option takes:
    packages/dispatch/e2e/visual-run.sh forwards its harness variables to the container so."""
    given: dict = {}
    for name, value in assignments:
        given.setdefault(name, []).append(value.split())
    found = {}
    for name, values in given.items():
        own = (f"${name}", f"${{{name}}}")
        tails = [words[1:] if words[:1] and words[0] in own else words for words in values]
        if any(tails) and all(not tail or tail[0].startswith("-") for tail in tails) and \
                not any(word in own for tail in tails for word in tail):
            found[name] = max(tails, key=len)
    return found


def _expanded(words: list, options: dict) -> list:
    """The words, each that names alone a variable of `options` (_option_variables) read as the
    options it expands to."""
    out: list = []
    for word in words:
        match = re.fullmatch(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?", word)
        out += options[match.group(1)] if match and match.group(1) in options else [word]
    return out


@cache
def held_packages() -> frozenset:
    """The packages this repository's lockfiles install, and the command names those packages put
    on a project's PATH (each `"bin"` a lockfile records): a package runner of one of these, after
    the frozen install that lockfile drives, runs it from node_modules and fetches nothing. Every
    bun.lock and package-lock.json the repository tracks is read, by `git ls-files`, or the root's
    alone where the tree is no git checkout."""
    try:
        listed = subprocess.run(["git", "ls-files", "-z", "--", "*bun.lock", "*package-lock.json"], cwd=REPO,
                                capture_output=True, text=True, check=True).stdout.split("\0")
    except (OSError, subprocess.CalledProcessError):
        listed = ["bun.lock", "package-lock.json"]
    held: set = set()
    for path in (path for path in listed if path and (REPO / path).is_file()):
        text = (REPO / path).read_text(encoding="utf-8")
        held.update(re.findall(r'^\s*"(?:node_modules/)?((?:@[^"/]+/)?[^"/@]+)": [\[{]', text, re.M))
        for bins in re.findall(r'"bin": \{([^}]*)\}', text):
            held.update(re.findall(r'"([^"]+)":', bins))
    return frozenset(held)


def _fetch_fed_to_shell(words: list) -> str | None:
    """`curl piped into a shell` (or wget's) where a shell runs what a fetch printed through a
    substitution: `bash <(curl …)`, `source <(curl …)`, `sh -c "$(curl …)"`, `eval "$(curl …)"`,
    each of which shell_commands leaves as a command whose first word is that substitution."""
    text = substitution(words[0])
    if text is None:
        return None
    for command in (c for pipeline in pipelines_of(text, WRAPPERS) for c in pipeline):
        name = command[0].rsplit("/", 1)[-1]
        if name in ("curl", "wget"):
            return f"{name} piped into a shell"
    return None


def _action_steps(uses: str, seen: set, inputs: dict) -> list:
    """The steps of the local composite action a `uses:` names, with each one's file and the inputs
    it gets (the caller's `with:` over the action's defaults), and those of the local actions they
    use in turn, each action once. An action this repository does not hold is check_table's to
    refuse, so it yields no steps here."""
    path = uses[2:].rstrip("/")
    if path in seen:
        return []
    seen.add(path)
    for name in ("action.yml", "action.yaml"):
        if (REPO / path / name).is_file():
            document = load_repository_yaml(REPO / path / name) or {}
            declared = {key: str((value or {}).get("default", "")) for key, value in
                        (document.get("inputs") or {}).items()}
            given = {**declared, **{str(k): _input_text(v, inputs) for k, v in inputs.items()}}
            steps = []
            for step in (document.get("runs") or {}).get("steps") or []:
                steps.append((f"{path}/{name}", step, given))
                inner = str(step.get("uses", ""))
                if inner.startswith(ACTIONS_PREFIX):
                    steps += _action_steps(inner, seen, {k: _input_text(v, given) for k, v in
                                                          (step.get("with") or {}).items()})
            return steps
    return []


def _input_text(value, inputs: dict) -> str | None:
    """A `with:` or `env:` value as text, an `${{ inputs.NAME }}` read from the inputs given, or
    None where it is another expression."""
    if value is None:
        return None
    text = str(value)
    match = re.fullmatch(r"\$\{\{\s*inputs\.([A-Za-z0-9_-]+)\s*\}\}", text.strip())
    if match:
        return inputs.get(match.group(1))
    return None if "${{" in text else text


def _loop_values(loop, known: dict) -> list | None:
    """The values a loop's variable takes, one per pass: the words of a `for` written out whole, or
    the first field of each non-empty line a `while` or `until` reads from a variable the step's env
    gives as text (`done <<<"$IMAGES"`, an input of the action its calling step's `with:` names);
    None where the rule cannot read them."""
    if loop.keyword == "for" and loop.words is not None and all(literal_word(w) for w in loop.words):
        return list(loop.words)
    if loop.feed and loop.feed[0] == "<<<":
        match = re.fullmatch(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?", loop.feed[1])
        text = known.get(match.group(1)) if match else None
        if text is not None:
            return [line.split()[0] for line in text.split("\n") if line.strip()]
    return None


def _iterations(loops: tuple, known: dict) -> int | None:
    """How many times a pipeline runs inside its loops, each loop's passes multiplied; None where a
    loop's passes cannot be read."""
    total = 1
    for loop in loops:
        values = _loop_values(loop, known)
        if values is None:
            return None
        total *= len(values)
    return total


def _values(word: str, loops: tuple, known: dict) -> list:
    """The values a command word takes: the word itself where it is written out whole, or each value
    of the innermost loop variable it names alone (`"$image"`); none where the rule cannot read it."""
    if literal_word(word):
        return [word]
    match = re.fullmatch(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?", word)
    for loop in reversed(loops):
        if match and loop.variable == match.group(1):
            return _loop_values(loop, known) or []
    return []


class NetworkInstalls:
    """Rule 22's reading of one job: each install its steps run, in the steps' own `run:`, in the
    local actions they use, in the repository scripts those run and in the Dockerfiles they build,
    refused unless wrapped; and what the job can spend before the last of its installs fails by
    name, held to the limits of each step and of the job."""

    def __init__(self, path: str, job_id: str, job: dict, problems: list, workflow_env: dict | None = None) -> None:
        self.path, self.job_id, self.job, self.problems = path, job_id, job, problems
        self.limit = _minutes(job)
        self.bounds = network_install_bounds()
        self.spent = 0  # the seconds the job can have spent before the step being read
        self.parts: list = []  # what self.spent counts, as (what, seconds, why)
        self.pulled: set = set()  # the images an earlier wrapped `docker pull` of the job pulled
        self.elan = False  # whether an earlier wrapped install of a Lean toolchain ran
        self.loaded: set = set()  # the tags an earlier build of the job loaded into the daemon
        # The images a pull-test-images step lists under `images:`, as (its label, the images, the
        # ones a later build's FROM named): the list must be the FROM images of the job's builds.
        self.listed: tuple | None = None
        # The variables the workflow's env and the job's set for every step of the job.
        self.env_names = {str(name) for name in {**(workflow_env or {}), **(job.get("env") or {})}}
        self.runner_pulls()

    def refuse(self, site: Site, message: str) -> None:
        self.problems.append(error(site.where, f"job {self.job_id} of {self.path}{site.via} {message}"))

    def runner_pulls(self) -> None:
        """The images the runner pulls before the job's first step: each `services:` image, the
        job's `container:`, and the image of each step that `uses: docker://<image>` (the runner's
        "Pull down action image", in its set-up of the job). The runner runs `docker pull` with no
        timeout of its own and tries it again only after a non-zero exit, and a workflow can give it
        none, so the job's limit is their one bound: each counts PULL_SECONDS, what a healthy pull of
        the runner takes."""
        images = [str((service or {}).get("image", "")) for service in (self.job.get("services") or {}).values()]
        container = self.job.get("container")
        images.append(str(container.get("image", "")) if isinstance(container, dict) else str(container or ""))
        images += [str(step["uses"])[len("docker://"):] for step in self.job.get("steps") or []
                   if isinstance(step, dict) and str(step.get("uses", "")).startswith("docker://")]
        images = [image for image in images if image]
        for image in images:
            self.spent += PULL_SECONDS
            self.parts.append((f"the runner's pull of {image}", PULL_SECONDS, "runner"))
            self.pulled.add(image)  # on the runner before the job's first step
        if images and self.limit is not None and self.spent > self.limit * 60:
            self.problems.append(error(self.path, f"job {self.job_id} of {self.path} has timeout-minutes {self.limit}, "
                                                  f"{self.limit * 60} s, and the runner's own pulls of its service and "
                                                  f"container images before its first step take {self.spent} s, "
                                                  f"{PULL_SECONDS} s each ({', '.join(images)}): the runner gives such a "
                                                  f"pull no bound of its own, so the job's limit must leave room for "
                                                  f"them and for what the job runs after them"))
            self.limit = None

    def read_step(self, step: dict) -> None:
        label = step_label(step)
        directory = step.get("working-directory") or (((self.job.get("defaults") or {}).get("run") or {})
                                                       .get("working-directory")) or ""
        directory = "" if "${{" in str(directory) else str(directory)
        own = Site(self.path, self.job_id, label, self.path, label=label, base=directory)
        located = [(own, step, {})]
        uses = str(step.get("uses", ""))
        if uses.rstrip("/") == PULL_ACTION:
            images = [line.strip() for line in str((step.get("with") or {}).get("images") or "").split("\n")]
            if any(images):
                self.listed = (label, [image for image in images if image], set())
        if uses.startswith(ACTIONS_PREFIX):
            given = {str(k): _input_text(v, {}) for k, v in (step.get("with") or {}).items()}
            for where, inner, inputs in _action_steps(uses, set(), given):
                located.append((Site(self.path, self.job_id, label, where,
                                     via=f", in {where}, which step \"{label}\" uses,", label=step_label(inner),
                                     action=where.rsplit("/", 1)[0]), inner, inputs))
        wrapped: list = []
        for site, inner, inputs in located:
            known = {str(k): _input_text(v, inputs) for k, v in (inner.get("env") or {}).items()}
            if isinstance(inner.get("run"), str):
                self.read_run(site, inner["run"], wrapped, set(), {k: v for k, v in known.items() if v is not None})
        if uses.startswith(BUILD_ACTION):
            options = step.get("with") or {}
            context = str(options.get("context") or ".")
            file = str(options.get("file") or posixpath.join(context, "Dockerfile"))
            self.read_build(own, step, file, context, str(options.get("build-args") or ""), wrapped)
            if str(options.get("load", "")).lower() == "true":  # the image it built is on the runner now
                tags = str(options.get("tags") or "").split("\n")
                self.pulled.update(tag.strip() for tag in tags if tag.strip())
                self.loaded.update(tag.strip() for tag in tags if tag.strip())
        self.hold_to_limits(step, label, wrapped, [] if "timeout-minutes" in step else self.own_bounds(step))

    def finish(self) -> None:
        """After the job's last step: each image a pull-test-images step lists under `images:` must
        be a FROM of a Dockerfile a later build of the job builds, so a pin changed in the Dockerfile
        alone, which leaves the list pulling the old image, fails by name."""
        if not self.listed:
            return
        label, images, named = self.listed
        for image in images:
            if image not in named:
                self.problems.append(error(self.path, f"job {self.job_id} of {self.path} pulls {image} in step "
                                                      f"\"{label}\", and no FROM of a Dockerfile the job builds after "
                                                      f"it names that image: the list is the build's FROM images, "
                                                      f"so write the Dockerfile's FROM pins there, the same bytes"))

    def own_bounds(self, step: dict) -> list:
        """The bounds a step that sets no timeout-minutes carries in its own `run:`: each `go test`
        given a `-timeout`, which ends the test binary that runs past it with the dump that names the
        hung test, times the passes of the loops it stands in, as (command, "go test", seconds,
        passes). A step's other commands carry no bound the rule reads, and count nothing."""
        if not isinstance(step.get("run"), str):
            return []
        known = {str(k): v for k, v in ((k, _input_text(v, {})) for k, v in (step.get("env") or {}).items())
                 if v is not None}
        shell = read_shell(step["run"], WRAPPERS)
        bounds = []
        for pipeline, loops in zip(shell.pipelines, shell.loops):
            for words in pipeline:
                seconds = go_test_timeout(words)
                if seconds:
                    bounds.append((" ".join(words), "go test", seconds, _iterations(loops, known) or 1))
        return bounds

    def read_run(self, site: Site, run: str, wrapped: list, seen: set, known: dict) -> None:
        """Each pipeline of a `run:`, a script or a RUN (shell_commands.read_shell, looked through
        WRAPPERS), with the loops it stands in: its installs refused, its wrapped installs kept in
        `wrapped` once per time it runs, each repository script it runs read the same way, once,
        and each script the resolver cannot place refused."""
        shell = read_shell(run, WRAPPERS)
        environment(shell.assignments, site)
        local = local_urls(shell.assignments, known)
        options = _option_variables(shell.assignments)
        found: list = []
        unfound: list = []
        for pipeline, loops, guards, cwd in zip(shell.pipelines, shell.loops, shell.guards, shell.cwds):
            set_by = [name for name in guards if name in self.env_names or name in known]
            if set_by:
                # The pipeline stands in an `if [ -z "$NAME" ]` branch, and NAME is set here (by the
                # job's env, its step's, or the command that runs this script), so the branch never
                # runs: its installs are no wait the job has, and are not counted.
                self.parts.append((f"left out: `{' '.join(pipeline[0])}` of {site.where}, which runs only "
                                   f"where {set_by[0]} is unset, and {set_by[0]} is set here,", 0, "left out"))
                continue
            # The directory a `cd` of a written-out path moved the text to (a subshell's own ends
            # with it), from which its relative paths are read: `(cd dir && ../x.sh)`.
            here = site if not cwd or cwd == "." else \
                replace(site, workdir=posixpath.normpath(posixpath.join(site.workdir, cwd))) if site.image else \
                replace(site, base=posixpath.normpath(posixpath.join(site.base, cwd)))
            for index, words in enumerate(pipeline):
                name = words[0].rsplit("/", 1)[-1]
                if name == NETWORK_INSTALL.rsplit("/", 1)[-1]:
                    self.read_wrapped(here, words, wrapped, loops, known)
                    continue
                install = None if name == "go" and tuple(words) in shell.offline else \
                    self.install_of(words, here, wrapped, local, options)
                if install:
                    found.append(install)
                if name in ("curl", "wget") and name not in site.fakes and \
                        any(later == [shell_name] for later in pipeline[index + 1:] for shell_name in STDIN_SHELLS):
                    found.append((f"{name} piped into a shell", name))
                fed = _fetch_fed_to_shell(words)
                if fed:
                    found.append((fed, fed.split()[0]))
                script = repository_script(words[0], here)
                if script and script not in seen:
                    seen.add(script)
                    parent = f"step \"{site.label}\"" if site.where == site.path else site.where
                    self.read_run(Site(site.path, site.job_id, site.step, script,
                                       via=f"{site.via}, in {script}, which {parent} runs,".replace(",,", ","),
                                       label=site.label, base=here.base, script=script, image=site.image,
                                       workdir=here.workdir, files=site.files, action=site.action,
                                       fakes=site.fakes | _fakes(shell, words)),
                                  read_repository_text(REPO / script), wrapped, seen,
                                  {**known, **shell.prefixes.get(tuple(words), {})})
                elif script is None and unfound_script(words[0], here):
                    unfound.append(words[0])
        for install, kind in dict.fromkeys(found):
            unmeasured = f", and {_unmeasured(kind)}" if kind in UNMEASURED else ""
            self.refuse(site, f"runs `{install}` in step \"{site.label}\": an install that waits on the network "
                              f"with no bound of its own{unmeasured}, so a mirror or a registry that stops answering "
                              f"holds the job until its limit and fails it for a cause outside the change under "
                              f"test. Run it as `{NETWORK_INSTALL} <kind> -- <command>` (from a composite action, "
                              f"\"$GITHUB_ACTION_PATH/../../scripts/network-install.sh\"), which bounds each "
                              f"attempt by that kind's measured bound, tries it three times and fails by name; a "
                              f"kind it lacks is a line of its table, with a bound measured from that install's "
                              f"own runs. A tool every run needs belongs in the runner image or in a cached, "
                              f"pinned download instead")
        for word in dict.fromkeys(unfound):
            self.refuse(site, f"runs `{word}` in step \"{site.label}\": a shell script rule 22 cannot place, so it "
                              f"cannot read the installs that script runs. Name it by its path from the step's "
                              f"directory, from $GITHUB_WORKSPACE or $GITHUB_ACTION_PATH, or from a variable the "
                              f"script sets to its own directory (`here=$(cd \"$(dirname \"$0\")\" && pwd)`)")

    def install_of(self, words: list, site: Site, wrapped: list, local: frozenset = frozenset(),
                   options: dict | None = None) -> tuple | None:
        """The install a command makes, as network_install names it (its words and its kind), with
        what the job did before it read in: a container of an image an earlier wrapped `docker pull`
        of the job pulled, and a Lean command after a wrapped install of its toolchain, pull
        nothing; a docker build reads its Dockerfile."""
        name = words[0].rsplit("/", 1)[-1]
        if name in site.fakes:  # the fake a test put first on the PATH it runs this script under
            return None
        if name in LEAN_COMMANDS:
            return None if self.elan else (f"{name} (elan's download of the Lean toolchain lean-toolchain names)",
                                           "elan")
        if name in ("docker", "docker-compose"):
            # A variable the text builds of options alone, before the image, is read as those options.
            words = _expanded(words, options or {})
            docker = docker_command(name, words[1:])
            if docker and docker.verb == "docker build":
                self.read_docker_build(site, words, wrapped)
                return None
            if docker and docker.verb in ("docker compose build", "docker buildx bake"):
                self.refuse(site, f"runs `{' '.join(words)}` in step \"{site.label}\": a build whose Dockerfiles a "
                                  f"compose or bake file names, which rule 22 does not read, so it cannot count the "
                                  f"pulls and installs the build makes. Build each image with "
                                  f"docker/build-push-action or `docker build -f <Dockerfile>`, which it reads")
                return None
            if docker and docker.verb in ("docker run", "docker create") and docker.pull != "always" and \
                    docker.image in self.pulled:
                return None  # its image is one an earlier step left on the runner
        return network_install(words, local, held_packages())

    def read_wrapped(self, site: Site, words: list, wrapped: list, loops: tuple, known: dict) -> None:
        """A call of a script named network-install.sh: the repository's own, as `<kind> --
        <command>`, with a kind its table bounds, wrapping an install of that kind, in a loop whose
        iterations the rule can count. An image a wrapped `docker pull` pulls is on the runner for
        the steps after it: a literal one, or each value of the loop variable it pulls."""
        count = _iterations(loops, known)
        call = " ".join(words)
        if resolved(words[0], site) != NETWORK_INSTALL:
            self.refuse(site, f"runs `{call}` in step \"{site.label}\": a network-install.sh that rule 22 cannot "
                              f"resolve to this repository's {NETWORK_INSTALL}, whose bounds are the measured ones")
            return
        if len(words) < 4 or words[2] != "--":
            self.refuse(site, f"runs `{call}` in step \"{site.label}\": {NETWORK_INSTALL} takes "
                              f"`<kind> -- <command>`, and refuses any other call when it runs")
            return
        kind, bounds = words[1], self.bounds
        if isinstance(bounds, str):
            self.refuse(site, f"runs `{call}` in step \"{site.label}\", and rule 22 cannot read the bounds that "
                              f"call waits under: {bounds}")
            return
        if kind not in bounds:
            unmeasured = f": {_unmeasured(kind)}" if kind in UNMEASURED else \
                ". A new kind is a line of its table, with a bound measured from that install's own runs"
            self.refuse(site, f"runs `{call}` in step \"{site.label}\": {NETWORK_INSTALL} bounds no kind "
                              f"'{kind}' (its kinds: {', '.join(sorted(bounds))}), and refuses it when it runs"
                              f"{unmeasured}")
            return
        for pipeline in command_of(list(words[3:]), WRAPPERS):
            for command in pipeline:
                install = network_install(command)
                if install and install[1] != kind and not kind.startswith(install[1] + "-"):
                    unmeasured = f"; {_unmeasured(install[1])}" if install[1] in UNMEASURED else ""
                    self.refuse(site, f"runs `{install[0]}` through {NETWORK_INSTALL}'s kind '{kind}' in step "
                                      f"\"{site.label}\": that bound was measured on {kind}'s installs, and "
                                      f"`{install[0]}` is of kind '{install[1]}'{unmeasured}")
                name = command[0].rsplit("/", 1)[-1]
                verb = (from_verb(command[1:], APT_VALUE_OPTIONS) or [""])[0] if name in APT else ""
                if verb in APT_DPKG_VERBS and not APT_DOWNLOAD_ONLY & set(command[1:]):
                    self.refuse(site, f"runs `{name} {verb}` through {NETWORK_INSTALL} in step \"{site.label}\" "
                                      f"without --download-only: the bound would end an attempt while dpkg "
                                      f"writes, and every later attempt then fails on `dpkg was interrupted`. "
                                      f"Wrap the download alone (`{name} {verb} --download-only …`), then install "
                                      f"what it downloaded unwrapped (`{name} {verb} --no-download …`), which "
                                      f"reads no network and is held by its step's limit")
                if kind == "go" and not (name == "go" and go_fetch(command[1:]) in GO_DOWNLOADS):
                    self.refuse(site, f"runs `{' '.join(command)}` through {NETWORK_INSTALL}'s kind 'go' in step "
                                      f"\"{site.label}\": that kind bounds a go download alone (`go get`, `go mod "
                                      f"download`, `go mod tidy`, `go mod vendor`), and a compile under it is timed as "
                                      f"a wait on the network (gopls's took 74 s of its 80 under the 120 s bound). "
                                      f"Fetch with .github/scripts/go-fetch.sh <package>@<version>, then build "
                                      f"unwrapped from the cache with the network off: "
                                      f"`GOPROXY=\"file://$(go env GOMODCACHE)/cache/download\" go install …`")
                docker = docker_command(name, command[1:]) if name == "docker" else None
                if docker and docker.verb == "docker pull" and command[-1:]:
                    self.pulled.update(_values(command[-1], loops, known))
                if name == "elan" and elan_fetch(command[1:]) == "elan toolchain install":
                    self.elan = True
        if count is None:
            self.refuse(site, f"runs `{call}` in step \"{site.label}\" inside a loop whose iterations rule 22 "
                              f"cannot count, so it cannot hold the job's limit to every attempt of every iteration. "
                              f"Loop over words written out whole, or over the lines of a variable the step's env "
                              f"gives as text, such as an action input its calling step names")
            return
        wrapped.append((" ".join(words[3:]), kind, worst_case(*bounds[kind]), count))

    def read_docker_build(self, site: Site, words: list, wrapped: list) -> None:
        """A `docker build` a step runs: its Dockerfile (`-f`, or the context's Dockerfile), read as
        read_build reads a docker/build-push-action step's."""
        rest = from_verb(words[1:], DOCKER_VALUE_OPTIONS)
        rest = rest[2:] if rest[:1] in (["buildx"], ["image"]) else rest[1:]
        file, context, args, index = "", "", [], 0
        while index < len(rest):
            word = rest[index]
            if word in ("-f", "--file") and index + 1 < len(rest):
                file, index = rest[index + 1], index + 2
            elif word.startswith("--file="):
                file, index = word.split("=", 1)[1], index + 1
            elif word == "--build-arg" and index + 1 < len(rest):
                args.append(rest[index + 1])
                index += 2
            elif word.startswith("-"):
                index += 2 if word in ("-t", "--tag", "--target", "--platform", "--build-context", "--cache-from",
                                       "--cache-to", "--output", "-o", "--secret", "--ssh", "--label") else 1
            else:
                context, index = word, index + 1
        context_path = resolved(context or ".", site)
        dockerfile = resolved(file, site) if file else (posixpath.join(context_path, "Dockerfile")
                                                         if context_path is not None else None)
        if context_path is None or dockerfile is None or not (REPO / dockerfile).is_file():
            self.refuse(site, f"runs `{' '.join(words)}` in step \"{site.label}\": a docker build whose Dockerfile "
                              f"and context rule 22 cannot place, so it cannot read the installs the build runs")
            return
        self.read_dockerfile(site, dockerfile, context_path, "\n".join(args), wrapped)

    def read_build(self, site: Site, step: dict, file: str, context: str, build_args: str, wrapped: list) -> None:
        """A docker/build-push-action step: the Dockerfile it builds, and that the step has a limit of
        its own for what the build waits on. A recipe PINNED_RECIPES names, while it holds the bytes
        it names, has its installs counted against that limit instead."""
        context_path = resolved(context, Site("", "", "", "")) if "${{" not in context else None
        dockerfile = resolved(file, Site("", "", "", "")) if "${{" not in file else None
        if context_path is None or dockerfile is None or not (REPO / dockerfile).is_file():
            self.refuse(site, f"builds `{file}` in step \"{site.label}\" with a context `{context}` rule 22 cannot "
                              f"place, so it cannot read the installs the build runs")
            return
        pinned = PINNED_RECIPES.get(dockerfile) == hashlib.sha256((REPO / dockerfile).read_bytes()).hexdigest()
        if pinned and "timeout-minutes" in step:
            return
        before = len(wrapped)
        self.read_dockerfile(site, dockerfile, context_path, build_args, wrapped)
        if len(wrapped) > before and "timeout-minutes" not in step:
            took = sum(seconds * count for _, _, seconds, count in wrapped[before:])
            self.refuse(site, f"builds {dockerfile} in step \"{site.label}\", whose builder pulls and wrapped "
                              f"installs can take {took} s before the last of them fails by name, with no "
                              f"timeout-minutes of its own: give the step a limit derived from them, so a pull the "
                              f"builder makes with no bound of its own ends in a cancellation that names this step")

    def read_dockerfile(self, site: Site, dockerfile: str, context: str, build_args: str, wrapped: list) -> None:
        """Each instruction of a Dockerfile a required job builds: every FROM of an image no earlier
        step left on the runner (a wrapped pull, or a build that loaded it) is a pull the builder
        makes with no bound of its own, which counts PULL_SECONDS, once per image; and every RUN is
        read as a step's `run:` is, in its stage's WORKDIR, with the files of the repository each
        COPY and each bind mount put in the image (a COPY from the context, or
        `--mount=type=bind,source=…`, so network-install.sh runs there by that path). BuildKit builds
        the stages one stage does not need from another side by side, so what the build can spend
        is the longest chain of stages the last one needs (its FROM, a COPY --from and a RUN
        --mount from= each make a stage need another): the installs and pulls of that chain alone
        go into `wrapped`."""
        values = dict(line.split("=", 1) for line in build_args.split("\n") if "=" in line)
        stages: dict = {}  # a stage's name: its files and WORKDIR so far
        index_of: dict = {}  # a stage's name or its number: its index
        costs: list = []  # each stage's own installs and pulls
        needs: list = []  # each stage's indexes of the stages it needs
        pulled: set = set()
        files: dict = {}
        workdir = "/"
        current = None
        for line, keyword, arguments in dockerfile_reader.instructions(read_repository_text(REPO / dockerfile)):
            flags, rest = dockerfile_reader.flags(arguments)
            if keyword == "ARG" and "=" in rest and not costs:
                name, _, value = rest.partition("=")
                values.setdefault(name.strip(), value.strip().strip('"'))
            elif keyword == "FROM":
                parts = rest.split()
                image = dockerfile_reader.substitute(parts[0], values) if parts else ""
                files = dict(stages.get(image, {}).get("files", {}))
                workdir = stages.get(image, {}).get("workdir", "/")
                costs.append([])
                needs.append({index_of[image]} if image in index_of else set())
                index_of[str(len(costs) - 1)] = len(costs) - 1
                if image not in stages and image not in ("scratch", "") and image not in self.loaded and self.listed:
                    if image in self.listed[1]:
                        self.listed[2].add(image)
                    else:
                        self.refuse(site, f"builds FROM {image} at {dockerfile}:{line}, and step \"{self.listed[0]}\" "
                                          f"pulls the job's FROM images but not that one: a pin changed in the "
                                          f"Dockerfile alone leaves that list behind. Write the Dockerfile's FROM "
                                          f"pins into its images:, the same bytes")
                if image not in stages and image not in self.pulled | pulled and image not in ("scratch", ""):
                    pulled.add(image)
                    costs[-1].append((f"FROM {image} (the builder's pull, at {dockerfile}:{line})", "pull",
                                      PULL_SECONDS, 1))
                if len(parts) >= 3 and parts[1].upper() == "AS":
                    stages[parts[2]] = {"files": files, "workdir": workdir}
                    index_of[parts[2]] = len(costs) - 1
                    current = parts[2]
                else:
                    current = None
            elif not costs:
                continue
            elif keyword == "WORKDIR":
                workdir = posixpath.normpath(posixpath.join(workdir, dockerfile_reader.substitute(rest, values)))
            elif keyword in ("COPY", "ADD") and any(f.startswith("--from=") for f in flags):
                source = dockerfile_reader.substitute(next(f for f in flags if f.startswith("--from=")).split("=", 1)[1],
                                                      values)
                if source in index_of:
                    needs[-1].add(index_of[source])
                elif source not in self.pulled | pulled:  # an image, which the builder pulls with no bound
                    pulled.add(source)
                    costs[-1].append((f"{keyword} --from={source} (the builder's pull, at {dockerfile}:{line})",
                                      "pull", PULL_SECONDS, 1))
            elif keyword in ("COPY", "ADD"):
                words = json.loads(rest) if rest.startswith("[") else rest.split()
                fetched = [source for source in words[:-1] if keyword == "ADD" and
                           (re.match(r"(https?|git|ssh)://|git@", source) or source.endswith(".git"))]
                for source in fetched:
                    self.refuse(site, f"builds {dockerfile} in step \"{site.label}\", whose ADD at line {line} fetches "
                                      f"{source} with no bound of its own (BuildKit's ADD gives a URL none): fetch it in "
                                      f"a RUN, with curl --max-time or through {NETWORK_INSTALL}")
                if len(words) >= 2:
                    target = posixpath.join(workdir, words[-1])
                    for source in (source for source in words[:-1] if source not in fetched):
                        repository = posixpath.normpath(posixpath.join(context, source))
                        into = posixpath.join(target, posixpath.basename(source)) \
                            if words[-1].endswith("/") or len(words) > 2 else target
                        files[posixpath.normpath(into)] = repository
            elif keyword == "RUN":
                mounts = {}
                for flag in flags:
                    options = dict(item.split("=", 1) for item in flag[len("--mount="):].split(",") if "=" in item) \
                        if flag.startswith("--mount=") else {}
                    if options.get("from") in index_of:
                        needs[-1].add(index_of[options["from"]])
                    elif options.get("from") and options["from"] not in self.pulled | pulled:
                        pulled.add(options["from"])  # an image, which the builder pulls with no bound
                        costs[-1].append((f"RUN --mount from={options['from']} (the builder's pull, at "
                                          f"{dockerfile}:{line})", "pull", PULL_SECONDS, 1))
                    elif options.get("type") == "bind" and "source" in options:
                        mounts[posixpath.normpath(posixpath.join(workdir, options.get("target", "")))] = \
                            posixpath.normpath(posixpath.join(context, options["source"]))
                run = " ".join(json.loads(rest)) if rest.startswith("[") else rest
                self.read_run(Site(site.path, site.job_id, site.step, dockerfile,
                                   via=f"{site.via}, in {dockerfile}, which step \"{site.label}\" builds,",
                                   label=f"RUN at line {line}", image=True, workdir=workdir,
                                   files={**files, **mounts}), run, costs[-1], set(), {})
            if keyword in ("WORKDIR", "COPY", "ADD") and current:
                stages[current] = {"files": files, "workdir": workdir}
        chain: dict = {}

        def longest(index: int) -> list:
            """The installs and pulls of the longest chain of stages ending at stage `index`."""
            if index not in chain:
                chain[index] = []  # a stage that needs itself is no chain
                before = max((longest(n) for n in needs[index] if n != index),
                             key=lambda entries: sum(s * c for _, _, s, c in entries), default=[])
                chain[index] = before + costs[index]
            return chain[index]

        if costs:
            wrapped += longest(len(costs) - 1)

    def hold_to_limits(self, step: dict, label: str, wrapped: list, bounded: list = ()) -> None:
        """Every install of the step must be able to fail by name inside the step's own limit, and,
        counted with what the job can spend before the step (the runner's pulls of its images, the
        limit of each step before it that sets one, and the installs of each that sets none), inside
        the job's: a limit that ends first ends the job in GitHub's cancellation, which names
        nothing. The step then adds its own limit, or its installs where it sets none, to what the
        steps after it count."""
        took = sum(seconds * count for _, _, seconds, count in wrapped)
        step_limit = _minutes(step) if "timeout-minutes" in step else None

        def each(installs: list) -> str:
            return "; ".join(f"`{command}` ({kind}) {seconds} s" if count == 1 else
                             f"`{command}` ({kind}) {count} × {seconds} s = {count * seconds} s"
                             for command, kind, seconds, count in installs)

        why = (f"each install through {NETWORK_INSTALL}'s attempts, each cut at its bound and killed after the wait "
               f"for SIGTERM, with the pauses between them, and each pull a runner or a builder makes at "
               f"{PULL_SECONDS} s, what a healthy pull takes")
        remedy = (f"A registry that stalls would end it in GitHub's cancellation, which names nothing, before "
                  f"{NETWORK_INSTALL} names the install. Give it a limit derived from what it runs, or fewer tries, "
                  f"with the reason in the notes of .github/ci-notes/")
        if wrapped and step_limit is not None and took > step_limit * 60:
            self.problems.append(error(self.path, f"job {self.job_id} of {self.path}: step \"{label}\" has "
                                                  f"timeout-minutes {step_limit}, {step_limit * 60} s, and {took} s is "
                                                  f"what the wrapped installs can take before the last of them fails "
                                                  f"by name, {why} ({each(wrapped)}). {remedy}"))
        tested = sum(seconds * count for _, _, seconds, count in bounded)
        at = self.spent + took + tested
        if wrapped and self.limit is not None and at > self.limit * 60:
            before = "; ".join(f"{what} {seconds} s" for what, seconds, _ in self.parts)
            own = f"; and the step's own test bounds ({each(bounded)})" if bounded else ""
            self.problems.append(error(self.path, f"job {self.job_id} of {self.path} has timeout-minutes {self.limit}, "
                                                  f"{self.limit * 60} s, and {at} s is what, up to step \"{label}\", the "
                                                  f"job can spend before the last of its installs fails by name: "
                                                  f"{before + ' before it; ' if before else ''}then {why} "
                                                  f"({each(wrapped)}){own}. {remedy}"))
            self.limit = None  # the job is named once
        cost = int(step_limit * 60) if step_limit is not None else took + tested
        if cost:
            self.spent += cost
            self.parts.append((f"step \"{label}\"'s limit" if step_limit is not None else
                               f"step \"{label}\"'s installs ({each(wrapped)})" if not bounded else
                               f"step \"{label}\"'s installs and test bounds ({each(wrapped + list(bounded))})",
                               cost, "step"))


def check_network_installs(documents: dict, problems: list) -> None:
    """No job of ci.yaml, nor of a workflow it calls, nor a local composite action one of them uses,
    nor a shell script of this repository one of those runs, nor a Dockerfile one of them builds,
    waits on the network except through NETWORK_INSTALL, and every such wait can end by name inside
    the limits of its step and its job (the module's docstring says why).

    What it reads: each pipeline of each step's `run:` (shell_commands.read_shell, looked through
    its WRAPPERS: sudo, timeout, nice, ionice, nohup, setsid, stdbuf, xargs and command, and the
    `NAME=value` words, env, interpreters, `-c` strings and `eval` each of them may put before the
    command, until none is left), with the loops it stands in; the steps of each local action a step
    uses, and of the actions those use, with the inputs the calling step gives; each shell script of
    this repository one of those commands runs, and the scripts those run, each once, found by the
    one resolver (script_paths.py: a path from the step's directory, $GITHUB_WORKSPACE, $GITHUB_ACTION_PATH,
    $0 and the script's own directory however written, and a variable the text sets to one of
    those); the Dockerfile each docker/build-push-action step or `docker build` builds, its FROM
    images and its RUN instructions, with the repository files its COPY and bind mounts put in the
    image; and each job's `services:` and `container:` images. A Python, TypeScript or Go program a
    step runs is out of its reach.

    What it refuses: apt, apt-get and aptitude with a verb that reads a mirror; apk adding,
    updating or fetching; pip, pip3, pipx and `python -m pip` installing; uv installing, syncing,
    making a virtual environment or running a tool (`uv pip install`, `uv sync`, `uv venv`, `uv
    run`, `uvx`, …) without its --offline; npm (with its aliases `ci`, `clean-install`, `cit`, …),
    pnpm and bun (`bun ci` too) installing or adding; yarn installing, adding or upgrading (a bare
    `yarn`, `yarn up`, `yarn global add`, `yarn create`, `yarn dlx`, …), wrapped or not, by name,
    since no bound is measured for it (UNMEASURED); npx, bunx, pnpx, `bun x` and `npm exec` running
    a package no lockfile of the repository holds, a version spec or under `--yes`/`--package`, and
    `pnpm dlx` and `pipx run`, which fetch whatever they run; gem, cargo, snap and brew
    installing; a docker pull (`docker pull`, `docker image pull`, `docker compose pull`), and a
    container of an image no earlier step left on the runner (`docker run`, `docker create`,
    `docker compose up`; an image is left there by a wrapped pull, by a build that loads it, and by
    the runner's pull of the job's `services:` and `container:` images), unless it carries
    `--pull=never`; Playwright's `install`, run by bunx, npx, pnpx or `bun x`; a go command that
    names a fetch (`go mod download`, `go mod tidy`, `go mod vendor`, `go get`, `go install` or `go
    run` of a <module>@<version>); elan installing a Lean toolchain (`elan toolchain install`,
    `elan default`, …), and lake or lean before such an install, since elan then checks github.com
    for a newer elan and downloads the toolchain lean-toolchain names, each a wait with no bound of
    its own; `gh release download`; a wget, and a curl with no --max-time, of a URL that is not
    this runner's own, however the URL is written (a variable or a substitution waits on the network
    as a literal does; network_commands.py reads a file: URL, localhost and a loopback address, written out or
    held by a variable the text sets only to such URLs, as local); and a curl or a wget piped into a
    shell, or handed to one by a substitution (`bash <(curl …)`, `sh -c "$(curl …)"`). timeout(1)
    alone is refused too: it bounds the wait,
    but neither tries it again nor names what failed. A call of NETWORK_INSTALL passes when the
    resolver places it at the repository's own script (never by the name's suffix alone), names a
    kind its table bounds (`--bounds`) and wraps no install of another installer, whose bound was
    not measured on it, and it stands in a loop whose iterations the rule can count. An apt install
    under NETWORK_INSTALL must carry --download-only, with the install from what it downloaded run
    unwrapped with --no-download, which is no install (APT_DPKG_VERBS says why). A shell script
    the resolver cannot place is refused by name, since the rule cannot read what it runs; one whose
    path after its first expansion names a shell script of the repository is read as that script.

    What it counts: for each job, the runner's pull of each `services:` and `container:` image at
    PULL_SECONDS (a healthy pull, measured; the runner gives the pull no bound of its own and a
    workflow can give it none, so the job's limit is its one bound); each wrapped install at
    network-install.sh's worst case, times the iterations of the loops it stands in; for a build,
    the FROM pulls and the wrapped installs of the longest chain of stages its last stage needs, a
    pull at PULL_SECONDS; and each step's own limit, or its installs where it sets none. A job or a
    step whose limit ends before its installs can all fail by name is refused, and so is a build
    step that sets no limit of its own.

    What it leaves alone, each for a reason: a `uses:` of an action this repository does not hold
    (actions/setup-go, actions/cache), whose fetches are that action's own; `mise install`, whose
    client ends a stalled wait by itself: mise 2026.8.12 bounds each request, its body included, by
    `http_timeout`'s 30 s, and against a server that sent the answer's headers and then one byte
    every 5 s, `mise install` of the jj install-jj installs ended by itself after four attempts, in
    125 s; a curl with --max-time, as .github/scripts/ci-tools.sh's ci_tools_curl makes each pinned
    download, and a curl or a wget of this runner's own server; a command a test put
    first on the PATH of the script it runs, which is that test's fake (_fakes); the RUN installs of
    a recipe PINNED_RECIPES names, while it holds the
    bytes named there, counted against its build step's own limit; and the modules a `go build`,
    `go test`, `go vet` or `go run` of this repository's own packages downloads by itself, which
    are no step's install: pull request #4434 moves them into one fetch step per job, which this
    rule then reads, so whichever of that pull request and this rule lands second wraps that step in
    NETWORK_INSTALL."""
    for path, document in documents.items():
        for job_id, job in ((document or {}).get("jobs") or {}).items():
            reader = NetworkInstalls(path, job_id, job or {}, problems, (document or {}).get("env"))
            for step in (job or {}).get("steps") or []:
                reader.read_step(step)
            reader.finish()
