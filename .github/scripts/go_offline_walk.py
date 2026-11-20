"""Rule 2 of check-go-offline.sh: each job's steps, walked as bash runs them, through the scripts
and functions they run, to the offline go builds they reach; each such build is fetched by a step
before it. Also which workflows are required: ci.yaml and every local workflow it calls.

Imported with PYTHONPATH naming the directory this file lives in. Its test is
check-go-offline.test.sh, with the cases it sources.
"""
import os
import re
from pathlib import Path

import yaml

import source_scan as scan
from go_offline_argv import deps_args, fetch_calls, fetch_file_calls, go_args
from go_offline_sites import REASON, go_subcommand, goish_text, offline
from source_shell import shell_command_word, shell_env_elements, shell_inner_scripts, shell_script
from source_statements import Source


def required_workflows():
    """ci.yaml and every local workflow it calls, followed through each called file, as their
    text with comment lines taken out."""
    names, todo = set(), [".github/workflows/ci.yaml"]
    while todo:
        path = todo.pop()
        if path in names or not os.path.isfile(path):
            continue
        names.add(path)
        todo += re.findall(r"uses:\s*\./(\.github/workflows/[\w.-]+\.ya?ml)", Path(path).read_text())
    return {p: "\n".join(l for l in Path(p).read_text().splitlines() if not l.lstrip().startswith("#")) for p in names}


def named_by(path, workflows):
    word = re.compile(r"(?<![\w.-])" + re.escape(os.path.basename(path)) + r"(?![\w-])")
    return sorted(w for w, text in workflows.items() if path in text or word.search(text))


# --- What rule 2 reads: the jobs' steps, the scripts and functions they run, and the offline builds.
TESTBUILD = "packages/daemon/internal/testbuild/testbuild.go"
ENVOY_BINARY = re.compile(r'\btestbuild\.EnvoyBinary\(\s*\w+\s*,\s*"([^"]+)"\s*\)')
# The subcommands of an offline go command that build what they name. A go test or vet a script
# runs is that step's own test, not a build one of its tests makes.
BUILDS = {"build", "install", "run", "list", "generate"}
RUNNERS = (["bun", "test"], ["bunx", "playwright", "test"], ["npx", "playwright", "test"], ["bun", "x", "playwright", "test"])
TEST_PATH = re.compile(r"\.(?:test|spec|e2e)\.[cm]?[jt]sx?$")
REPO = "/@repo"
# A program a step hands its file to, and the files a step runs by their own path.
PROGRAMS = {"python", "python3", "node", "bun", "tsx", "ts-node"}
PROGRAM_FILES = (".py", ".ts", ".mts", ".cts", ".js", ".mjs", ".cjs")
# Filled by read_testbuild: each command a daemon test builds through testbuild.EnvoyBinary, and
# each directory such a test is in.
built, users = set(), set()


def read_testbuild():
    for directory, dirs, files in os.walk("packages/daemon"):
        dirs[:] = [d for d in dirs if d not in scan.SKIP_DIRS]
        for name in files:
            if name.endswith("_test.go"):
                calls = ENVOY_BINARY.findall(scan.read(os.path.join(directory, name)))
                if calls:
                    built.update(calls)
                    users.add(os.path.normpath(directory))


def norm(path):
    """A path made whole: inside the repository as /@repo/<relative>, a path outside it as itself,
    above the repository's root None."""
    if path is None:
        return None
    if path == REPO or path.startswith(REPO + "/"):
        relative = os.path.normpath(path[len(REPO):].lstrip("/") or ".")
        if relative == ".." or relative.startswith("../"):
            return None
        return REPO if relative == "." else f"{REPO}/{relative}"
    return os.path.normpath(path) if path.startswith("/") else None


def join(base, path):
    if path is None:
        return None
    if path.startswith("/"):
        return norm(path)
    return norm(f"{base}/{path}") if base else None


def in_repo(path):
    """A whole path as the repository names it (packages/envoy, or . for its root), else None."""
    if path is None or not (path == REPO or path.startswith(REPO + "/")):
        return None
    return path[len(REPO):].lstrip("/") or "."


class Need(tuple):
    """An offline build's need: its module as the repository names it (None when this reading
    cannot tell), the packages or tool pins it builds, and where."""

    __slots__ = ()

    def __new__(cls, module, packages, where):
        return super().__new__(cls, (module, frozenset(packages), where))

    module = property(lambda self: self[0])
    packages = property(lambda self: self[1])
    where = property(lambda self: self[2])


class Reach:
    """What one step reaches: the needs of the offline builds it runs, whether it runs tests built
    through testbuild, the fetches it makes, and the files it runs: each script it runs or
    sources, each program it hands to python3, node or bun, and the directory of each test run
    (trees), whose TypeScript the run loads."""

    def __init__(self):
        self.needs, self.testbuild, self.fetches, self.tools = [], False, {}, set()
        self.files, self.trees = set(), set()


# Filled by index, file by file, as rule 1 reads every file: every shell file, by name, and what
# each TypeScript file starts.
shell_files, ts_scripts, ts_go_needs = {}, {}, {}
SCRIPT_NAME = re.compile(r"[\w.-]+\.sh\b")


def index_typescript(path, text):
    """The shell scripts a TypeScript file names in its strings, and the packages each of its
    goCommand and goCommandSync calls builds, where its `packages:` is a list of strings."""
    if ".sh" in text:
        names = {name for token in scan.lex(text, "ts") if token.kind in ("string", "template")
                 for name in SCRIPT_NAME.findall(token.value)}
        if names:
            ts_scripts[path] = names
    if "goCommand" in text:
        src = Source(text, "ts")
        for call in src.calls():
            if call.name not in ("goCommand", "goCommandSync") or not call.args:
                continue
            a, b = call.args[0]
            if not (src.punct(a, "{") and src.partner.get(a) == b - 1):
                continue
            for pa, pb in src.split(a + 1, b - 1):
                if src.tokens[pa].value == "packages" and src.punct(pa + 1, ":") and src.punct(pa + 2, "[") \
                        and src.partner.get(pa + 2) == pb - 1:
                    packages = [src.string(*r) for r in src.split(pa + 3, pb - 1)]
                    if packages and None not in packages:
                        ts_go_needs.setdefault(path, []).append(
                            Need(None, packages, f"{path}:{src.line(call.at) + 1} (goCommand)"))


def index(path, lang, text):
    """Note what rule 2's walk needs to know of a file: a shell file by its name, and a TypeScript
    file's script names and goCommand packages."""
    if lang == "sh":
        shell_files.setdefault(os.path.basename(path), []).append(path)
    elif lang == "ts" and (SCRIPT_NAME.search(text) or "goCommand" in text):
        index_typescript(path, text)


class Walk:
    """Follows a step's shell the way bash runs it, far enough to see where each go command runs:
    the directory each `cd` moves to (a path, $(dirname "$0"), $(cd X && pwd), a variable set to
    one), the arguments a function or a script is called with, the environment an export or a
    prefix gives it, each script a command runs or sources and each function it calls, and the
    scripts a bun or Playwright test run's TypeScript names."""

    def __init__(self):
        self.parsed, self.memo = {}, {}

    def remembered(self, key, reach, walk, files=True):
        """Walk a script or a function once for each place, arguments and environment it is run
        with, into a Reach of its own, and add what that found to reach: the files it runs too,
        unless files is False. A walk under way counts as empty, so a function that calls itself
        ends."""
        found = self.memo.get(key)
        if found is None:
            found = self.memo[key] = Reach()
            walk(found)
        reach.needs += found.needs
        reach.testbuild |= found.testbuild
        for module, packages in found.fetches.items():
            reach.fetches.setdefault(module, set()).update(packages)
        reach.tools |= found.tools
        if files:
            reach.files |= found.files
        reach.trees |= found.trees

    def commands(self, path):
        if path not in self.parsed:
            self.parsed[path] = shell_script(scan.read(path)) if os.path.isfile(path) else []
        return self.parsed[path]

    def resolve(self, word, cwd, path, params, vars_):
        """A word's value with its expansions read, or None where one cannot be."""
        out = []
        for kind, text, nested in word.parts:
            if kind in ("lit", "sq", "dq"):
                out.append(text)
            elif kind == "param":
                if text.isdigit():
                    value = (REPO + "/" + path if text == "0" else
                             params[int(text) - 1] if int(text) <= len(params) else None)
                elif text == "BASH_SOURCE":
                    value = REPO + "/" + path
                elif text in ("GITHUB_WORKSPACE", "PWD"):
                    value = REPO if text == "GITHUB_WORKSPACE" else cwd
                else:
                    value = vars_.get(text)
                if value is None:
                    return None
                out.append(value)
            elif kind == "cmd":
                value = self.substitute(nested, cwd, path, params, vars_)
                if value is None:
                    return None
                out.append(value)
            else:
                return None
        return "".join(out)

    def substitute(self, nested, cwd, path, params, vars_):
        """The output of a $(...) that names a directory: dirname X, cd X && pwd, pwd, realpath X,
        readlink -f X and git rev-parse --show-toplevel."""
        if not nested:
            return None
        depth = min(len(c.frames) for c in nested)
        direct = [c for c in nested if len(c.frames) == depth]
        values = [[self.resolve(w, cwd, path, params, vars_) for w in c.words] for c in direct]
        if [v[:1] for v in values] == [["cd"], ["pwd"]]:
            target = [v for v in values[0][1:] if v not in ("--", "-P", "-L")]
            return join(cwd, target[0]) if target and target[0] is not None else None
        if len(values) != 1 or not values[0]:
            return None
        verb, args = values[0][0], [v for v in values[0][1:] if v not in ("--", "-f", "-P")]
        if verb == "pwd":
            return cwd
        if verb == "dirname" and args and args[0] is not None:
            whole = join(cwd, args[0])
            return os.path.dirname(whole) if whole else None
        if verb in ("realpath", "readlink") and args and args[0] is not None:
            return join(cwd, args[0])
        if values[0][:3] == ["git", "rev-parse", "--show-toplevel"]:
            return REPO
        return None

    def run(self, text, path, workdir, reach):
        """Walk a step's run text, its directory the step's."""
        self.walk(shell_script(text), path, join(REPO, workdir), [], [], {}, {}, reach, 0)

    def walk(self, commands, path, here, params, env, vars_, functions, reach, depth, body=False):
        """Walk commands in order. A function's body runs where it is called (body=True there), so a
        file's walk passes over it."""
        if depth > 8:
            return
        cwds, exported = {}, list(env)
        for name, lines in self.functions(commands).items():
            functions[name] = (path, lines)
        for command in commands:
            if command.function is not None and not body:
                continue
            cwd = here
            for size in range(len(command.frames), -1, -1):
                if command.frames[:size] in cwds:
                    cwd = cwds[command.frames[:size]]
                    break
            values, origin = [], []
            for index, word in enumerate(command.words):
                if [p[:2] for p in word.parts] == [("param", "@")]:
                    values += params
                    origin += [index] * len(params)
                else:
                    values.append(self.resolve(word, cwd, path, params, vars_))
                    origin.append(index)
            if not command.words:
                for name, value in command.assigns:
                    vars_[name] = self.resolve(value, cwd, path, params, vars_)
                continue
            k = shell_command_word(command)
            k = origin.index(k) if k is not None and k in origin else None
            verb = values[k] if k is not None else None
            own = shell_env_elements(command, len(command.words))
            if verb == "cd":
                target = [v for v in values[k + 1:] if v not in ("--", "-P", "-L")]
                cwds[command.frames] = join(cwd, target[0]) if target else None
            elif verb in ("local", "declare", "typeset", "readonly", "export"):
                for value in values[k + 1:]:
                    name, eq, rest = (value or "").partition("=")
                    if eq and re.fullmatch(r"[A-Za-z_]\w*", name):
                        vars_[name] = rest
                        if verb == "export" and name == "GOPROXY" and not command.forked:
                            exported.append(("set", name, rest))
            elif verb == "unset" and "GOPROXY" in values[k + 1:] and not command.forked:
                exported.append(("set", "GOPROXY", None))
            elif verb == "shift":
                count = values[k + 1] if k + 1 < len(values) and (values[k + 1] or "").isdigit() else "1"
                params = params[int(count):]
            self.go_commands(command, values, origin, cwd, path, exported, reach)
            if verb is None:
                continue
            inherited = [*exported, *own]
            for inner, line in shell_inner_scripts(command):
                self.walk(shell_script(inner, line), path, cwd, params, inherited, vars_, functions, reach,
                          depth + 1)
            state = offline(inherited)
            if verb in functions:
                where, lines = functions[verb]
                args = values[k + 1:]
                self.remembered(("function", where, verb, cwd, tuple(args), state), reach, lambda into: self.walk(
                    lines, where, cwd, args, inherited, vars_, functions, into, depth + 1, True))
            elif verb in (".", "source") and k + 1 < len(values):
                for script in self.scripts(values[k + 1], command.words[origin[k + 1]], cwd):
                    if values[k + 1] is not None:  # a path read, not one guessed from a variable's name
                        reach.files.add(script)
                    self.walk(self.commands(script), script, cwd, params, exported, vars_, functions, reach,
                              depth + 1)
            elif verb in ("bash", "sh") or (verb or "").endswith(".sh"):
                at = k if verb.endswith(".sh") else next((j for j in range(k + 1, len(values))
                                                         if not (values[j] or "-").startswith("-")), None)
                if at is not None:
                    args = values[at + 1:]
                    for script in self.scripts(values[at], command.words[origin[at]], cwd):
                        self.remembered(("script", script, cwd, tuple(args), state), reach, lambda into, s=script: (
                            self.walk(self.commands(s), s, cwd, args, inherited, {}, {}, into, depth + 1)))
                        reach.files.add(script)  # a script named by a word its walk reads, never None
            elif any(values[k:k + len(r)] == r for r in RUNNERS):
                runner = next(r for r in RUNNERS if values[k:k + len(r)] == r)
                # Playwright loads its config, and the harness that config starts, whatever specs it
                # is given: its whole directory is what it reaches. bun test reaches what it names.
                paths = [] if "playwright" in runner else values[k + len(runner):]
                self.runner(paths, cwd, inherited, reach, depth)
            elif verb in PROGRAMS or verb.endswith(PROGRAM_FILES):
                # A program a step runs by its path, or hands to python3, node or bun: a file it runs,
                # which rule 1's file reason may not cover. It is not walked: its language is no shell.
                at = k if verb.endswith(PROGRAM_FILES) else next(
                    (j for j in range(k + 1, len(values)) if values[j] and not values[j].startswith("-")
                     and values[j] != "run"), None)
                program = in_repo(join(cwd, values[at])) if at is not None else None
                if program and os.path.isfile(program):
                    reach.files.add(program)
            if any(v and v.endswith("go-fetch.sh") for v in values):
                self.fetch(values, cwd, reach)

    @staticmethod
    def functions(commands):
        bodies = {}
        for command in commands:
            if command.function:
                bodies.setdefault(command.function, []).append(command)
        return bodies

    def scripts(self, value, word, cwd):
        """The shell files a word names, read where it can be; else each $VAR in its text matches
        any name there."""
        if value is not None:
            whole = in_repo(join(cwd, value))
            return [whole] if whole and os.path.isfile(whole) else []
        text = word.source()
        found = []
        for base in (in_repo(cwd) or ".", "."):
            pattern = os.path.normpath(os.path.join(base, re.sub(r"\$\{?\w+\}?|\$\(\.\.\.\)", "*", text)))
            found += sorted(str(p) for p in Path(".").glob(pattern) if p.is_file() and p.suffix == ".sh")
        return sorted(set(found))

    def go_commands(self, command, values, origin, cwd, path, exported, reach):
        """The go commands of one simple command: a go test of testbuild's callers (rule 2's first
        half, offline or not, since testbuild builds offline inside it), and any offline go command
        that loads the modules of what it names. Each is read through go_offline_argv.go_args, the
        reading rule 7 shares; a package here is an operand from the module's directory (./x, .)
        or a path@version."""
        texts = [v if v is not None else "\x00" for v in values]
        for j in range(len(texts)):
            word = command.words[origin[j]]
            if not (goish_text(word.value()) and go_subcommand(texts, j)):
                continue
            go = go_args(texts, j)
            module = cwd if go.chdir is None else None if "\x00" in go.chdir else join(cwd, go.chdir)
            packages = [a for a in go.operands if a.startswith("./") or a == "." or ("@" in a and "\x00" not in a)]
            if go.sub == "test" and not (go.flag("-list") or (go.flag("-c") and ("-o", "/dev/null") in go.flags)):
                listed = not packages or "xargs" in texts[:j]
                self.testbuild_tests(in_repo(module), packages, listed, reach)
            if go.sub in BUILDS and offline([*exported, *shell_env_elements(command, origin[j])]):
                # A module this reading cannot place, or one outside the repository (a scratch copy),
                # is no module a fetch step names.
                if in_repo(module) is not None and "\x00" not in "".join(texts[j + 1:]):
                    reach.needs.append(Need(in_repo(module), packages or ["."], f"{path}:{word.line + 1}"))

    @staticmethod
    def testbuild_tests(module, packages, listed, reach):
        if module is None:
            return
        for user in users:
            if listed and module != "." and (user == module or user.startswith(module.rstrip("/") + "/")):
                reach.testbuild = True
            for arg in packages:
                pkg = os.path.normpath(os.path.join(module, arg))
                if pkg == user or (arg.endswith("/...") and user.startswith(pkg[:-3].rstrip("/") + "/")) \
                        or (arg == "./..." and user.startswith(module.rstrip("/") + "/")):
                    reach.testbuild = True

    def runner(self, args, cwd, env, reach, depth):
        """A bun or Playwright test run: each script a TypeScript file under its directory (or under
        the directories and test files it names; a config file is no such path) names in a string,
        walked from that script's own directory, and each goCommand of those files with its
        packages."""
        here = in_repo(cwd)
        if here is None:
            return
        roots = [os.path.normpath(os.path.join(here, v)) for v in args
                 if v and not v.startswith("-") and (os.path.isdir(os.path.join(here, v)) or (
                     os.path.isfile(os.path.join(here, v)) and TEST_PATH.search(v)))] or [here]
        reach.trees.update(roots)
        inside = [p for p in set(ts_scripts) | set(ts_go_needs)
                  if any(r == "." or p == r or p.startswith(r.rstrip("/") + "/") for r in roots)]
        for ts in sorted(inside):
            reach.needs += ts_go_needs.get(ts, [])
            package = "/".join(ts.split("/")[:2])
            for name in sorted(ts_scripts.get(ts, ())):
                candidates = shell_files.get(name, [])
                near = [s for s in candidates if s.startswith(package + "/")]
                for script in near or (candidates if len(candidates) == 1 else []):
                    here_ = join(REPO, os.path.dirname(script))
                    # A script a TypeScript file names in a string may be read as text, not run, so
                    # it is walked for its builds and is no file this step is known to run.
                    self.remembered(("script", script, here_, (), offline(env)), reach, lambda into, s=script, h=here_: (
                        self.walk(self.commands(s), s, h, [], env, {}, {}, into, depth + 1)), files=False)

    @staticmethod
    def fetch(values, cwd, reach):
        """The fetches a command makes through go-fetch.sh: deps DIR PACKAGE..., tools FILE, and
        file FILE, whose every deps or tools line is one of those, its paths from the repository's
        root (the GOOS, GOARCH or CGO_ENABLED setting before a deps aside). Each call and each line
        is read through go_offline_argv, as rule 7 reads them."""
        for call in fetch_calls(values):
            if call.mode == "file":
                listed = in_repo(join(cwd, call.words[0]))
                text = scan.read(listed) if listed and os.path.isfile(listed) else ""
                for line in fetch_file_calls(text):
                    if line.mode in ("deps", "tools") and line.words:
                        Walk.fetched(line.mode, join(REPO, line.words[0]), line.words[1:], reach)
            elif call.mode in ("deps", "tools"):
                Walk.fetched(call.mode, join(cwd, call.words[0]), call.words[1:], reach)

    @staticmethod
    def fetched(mode, path, rest, reach):
        """One fetch into reach: for deps, the module at path and the packages rest names
        (deps_args); for tools, each pin the file at path lists."""
        if mode == "deps":
            reach.fetches.setdefault(in_repo(path), set()).update(deps_args(rest).packages)
            return
        listed = in_repo(path)
        if listed and os.path.isfile(listed):
            reach.tools.update(l.split("#", 1)[0].strip() for l in scan.read(listed).splitlines()
                               if l.split("#", 1)[0].strip())


def covered(need, fetched, tools):
    pins = {p for p in need.packages if "@" in p}
    packages = need.packages - pins
    if not all(any(pin == tool or pin.split("@")[0] == tool.split("@")[0] for tool in tools) for pin in pins):
        return False
    if not packages:
        return True
    if need.module is not None:
        return packages <= fetched.get(need.module, set())
    return any(packages <= done for done in fetched.values())


def step_reasoned(raw_lines, step, run):
    """A `go-offline: <why>` in the step's run, or in a YAML comment within the 5 lines above the line
    that names the step."""
    if REASON.search(run):
        return True
    name = step.get("name")
    if not name:
        return False
    named = re.compile(r"^\s*(?:-\s+)?name:\s*[\"']?" + re.escape(str(name)) + r"[\"']?\s*$")
    return any(named.match(line) and any(re.match(r"\s*#", l) and REASON.search(l)
                                         for l in raw_lines[max(0, k - scan.REASON_WINDOW):k])
               for k, line in enumerate(raw_lines))


class Reached:
    """The files steps of the required workflows run, as rule 2's walk reaches them: each with the
    step that first reaches it; and each test run's directory, whose TypeScript it loads."""

    def __init__(self):
        self.files, self.trees = {}, {}

    def by(self, path, lang):
        """The step that reaches a file, or None."""
        if path in self.files:
            return self.files[path]
        if lang == "ts":
            return next((w for t, w in sorted(self.trees.items())
                         if t == "." or path == t or path.startswith(t.rstrip("/") + "/")), None)
        return None


def check_jobs(errors, workflows=()):
    """Rule 2 over every job of every workflow, its steps walked in order: each step that runs
    testbuild's tests with no fetch of the commands they build before it, and each that reaches an
    offline build no step before it fetches, adds an error. (The steps that run testbuild's tests,
    the steps that reach an offline build, those of them that say why they need none, and the
    files the steps of the workflows `workflows` names reach: a Reached.)"""
    walker = Walk()
    jobs_checked = reach_checked = reach_reasoned = 0
    reached = Reached()
    if os.path.isfile(TESTBUILD) and not users:
        scan.unknown(f"{TESTBUILD} exists and no daemon test calls testbuild.EnvoyBinary with a command, so the "
                     "jobs that run one are unseen")
    for workflow in sorted(Path(".github/workflows").glob("*.y*ml")):
        workflow_text = workflow.read_text()
        document = yaml.safe_load(workflow_text) or {}
        default_dir = ((document.get("defaults") or {}).get("run") or {}).get("working-directory", ".")
        for job_id, job in (document.get("jobs") or {}).items():
            if not isinstance(job, dict):
                continue
            job_dir = ((job.get("defaults") or {}).get("run") or {}).get("working-directory", default_dir)
            fetched, tools, testbuild_fetched = {}, set(), False
            for step in job.get("steps") or []:
                run = step.get("run")
                if not isinstance(run, str):
                    continue
                workdir = os.path.normpath(step.get("working-directory", job_dir))
                name = step.get("name") or run.strip().splitlines()[0]
                reach = Reach()
                # A step whose text names no go, no script, no program and no test run reaches none.
                if re.search(r"go|\.sh\b|\bbun|playwright|\bsource\b|(?:^|[;&|(]\s*)\.\s|\bpython|\bnode\b|\btsx\b"
                             r"|\.py\b|\.[cm]?[jt]s\b", run, re.M):
                    walker.run(run, str(workflow), workdir, reach)
                if str(workflow) in workflows:
                    where = f"{workflow} job {job_id}, step \"{name}\""
                    for path in reach.files:
                        reached.files.setdefault(path, where)
                    for tree in reach.trees:
                        reached.trees.setdefault(tree, where)
                if reach.testbuild and users:
                    jobs_checked += 1
                    if not testbuild_fetched:
                        errors.append((str(workflow), 1,
                                       f"job {job_id}, step \"{name}\", runs the tests of {', '.join(sorted(users))}, "
                                       f"whose testbuild.EnvoyBinary builds {' '.join(sorted(built))} with "
                                       f"GOPROXY=off, and no step before it fetches them: add `.github/scripts/go-fetch.sh "
                                       f"deps packages/envoy {' '.join(sorted(built))}` before it"))
                needs = sorted(set(reach.needs), key=lambda n: n.where)
                if needs:
                    reach_checked += 1
                    missing = [n for n in needs if not covered(n, fetched, tools)]
                    if missing and step_reasoned(workflow_text.split("\n"), step, run):
                        reach_reasoned += 1
                    elif missing:
                        errors.append((str(workflow), 1, f"job {job_id}, step \"{name}\", reaches "
                                       + "; ".join(f"{n.where}, an offline go command of {n.module or 'its module'}: "
                                                   f"{' '.join(sorted(n.packages))}" for n in missing)
                                       + ", and no step before it fetches that: add `.github/scripts/go-fetch.sh deps "
                                       + "; ".join(f"{n.module or 'MODULE'} {' '.join(sorted(n.packages))}"
                                                   for n in missing)
                                       + "` (or `go-fetch.sh tools` of a pin) before it, or say in a "
                                         "`# go-offline: <why>` line of its run why it builds none of it"))
                for module, packages in reach.fetches.items():
                    fetched.setdefault(module, set()).update(packages)
                tools |= reach.tools
                if "packages/envoy" in reach.fetches and built <= reach.fetches["packages/envoy"]:
                    testbuild_fetched = True
    # A tree where tests call testbuild and no step of any workflow runs them is one whose steps this
    # reading cannot follow (a go test, cd or script it does not see), not a clean one.
    if os.path.isfile(TESTBUILD) and jobs_checked == 0:
        scan.unknown(f"{', '.join(sorted(users))} call testbuild.EnvoyBinary and no step of any workflow was seen to "
                     "run their tests, so the jobs that run one are unseen")
    return jobs_checked, reach_checked, reach_reasoned, reached
