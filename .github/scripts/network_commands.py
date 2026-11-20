"""What one shell command fetches from the network, read from its words alone: the reader of
installers, package runners, docker, curl, wget, go and elan that check-ci-filters.sh's rule 22
(.github/scripts/ci_network_installs.py, the policy) asks of each command it finds.

network_install(words) names the install a command makes and its kind, network-install.sh's word
for it; docker_command reads a docker command's verb, image and pull policy; go_fetch, elan_fetch
and go_test_timeout read the go and elan commands the policy treats apart. A command's words come
from .github/scripts/shell_commands.py, which this module shares the shell's view of a word with
(literal_word). Nothing here reads a file, a workflow or what an earlier step did: the policy holds
that.
"""

import re
from dataclasses import dataclass

from shell_commands import literal_word

# apt's own verbs that read a mirror, and the options of apt and apt-get that take a value.
APT = {"apt", "apt-get", "aptitude"}
APT_VERBS = {"update", "install", "reinstall", "upgrade", "dist-upgrade", "full-upgrade", "build-dep", "source",
             "download"}
APT_VALUE_OPTIONS = {"-o", "-c", "-t", "--option", "--config-file", "--target-release"}
# The apt verbs that run dpkg after their download. Under NETWORK_INSTALL such a command may only
# download (`--download-only`), and the install from what it downloaded runs unwrapped with
# `--no-download`: a bound that ends an attempt while dpkg writes leaves the system "interrupted",
# and every later attempt then fails on `dpkg was interrupted, you must manually run 'dpkg
# --configure -a'`, as worker-image run 44765474341 did when its runtime-base RUN's 471 packages
# (442 MB) downloaded in 21 s and dpkg was still unpacking them when the 60 s bound ended the
# attempt. Unpacking is no wait on the network; the build step's own limit holds it.
APT_DPKG_VERBS = {"install", "reinstall", "upgrade", "dist-upgrade", "full-upgrade", "build-dep"}
APT_DOWNLOAD_ONLY = {"-d", "--download-only"}
# Every other installer, by its command name: the verbs of it that install from the network, and
# the options of it that take a value of their own and so may stand before the verb (`npm --prefix
# dir ci`, `bun --cwd dir install`); a `+toolchain` word (`cargo +nightly install`) is an option.
# npm's verbs hold its aliases (`clean-install`, `ic`, `cit`, …), and `bun ci` is Bun's frozen
# install (Bun 1.4.4's `bun ci --help` prints `Usage: bun install`). A yarn with no verb at all
# installs too (yarn's own documentation: "Running `yarn` with no command will run `yarn install`"),
# and so do its upgrades (`yarn upgrade`, Berry's `yarn up` and `yarn upgrade-interactive`) and
# `yarn create`, which installs create-<name> before it runs it; YARN_FETCHES_TWO holds the ones of
# two words.
PIP_VALUE_OPTIONS = {"--proxy", "--cert", "--client-cert", "--log", "--timeout", "--retries", "--python",
                     "--cache-dir", "--exists-action", "--trusted-host"}
INSTALLERS = {
    "pip": ({"install", "download"}, PIP_VALUE_OPTIONS),
    "pip3": ({"install", "download"}, PIP_VALUE_OPTIONS),
    "pipx": ({"install"}, set()),
    "npm": ({"install", "i", "ci", "add", "clean-install", "ic", "install-clean", "cit", "it",
             "install-test", "install-ci-test"},
            {"--prefix", "-C", "--workspace", "-w", "--registry", "--cache", "--userconfig", "--loglevel"}),
    "pnpm": ({"install", "i", "add"}, {"-C", "--dir", "--filter", "-F", "--reporter", "--loglevel"}),
    "yarn": ({"install", "add", "upgrade", "up", "upgrade-interactive", "create"},
             {"--cwd", "--cache-folder", "--modules-folder", "--registry"}),
    "bun": ({"install", "i", "add", "ci"}, {"--cwd", "-c", "--config", "--env-file", "-r", "--preload"}),
    "gem": ({"install"}, {"--config-file"}),
    "cargo": ({"install"}, {"-C", "--config", "-Z", "--color"}),
    "snap": ({"install"}, set()),
    "brew": ({"install"}, set()),
    "apk": ({"add", "update", "upgrade", "fetch"}, {"-X", "--repository", "-p", "--root", "--cache-dir",
                                                     "--keys-dir", "--arch", "--repositories-file"}),
}
# Yarn's fetches of two words: a global install or upgrade, Berry's download of a Yarn release
# (`yarn set version`) and its install of some workspaces alone (`yarn workspaces focus`).
YARN_FETCHES_TWO = {("global", "add"), ("global", "upgrade"), ("set", "version"), ("workspaces", "focus")}
# The kind network-install.sh bounds each installer's install under, where it is not the program's
# own name.
KIND_OF = {"pip3": "pip", "pipx": "pip"}
UV_VALUE_OPTIONS = {"--directory", "--project", "--cache-dir", "--python", "-p", "--config-file", "--color"}
UV_INSTALLS = (["pip", "install"], ["pip", "sync"], ["tool", "install"], ["tool", "run"], ["python", "install"],
               ["sync"], ["venv"], ["run"], ["add"], ["lock"])
DOCKER_VALUE_OPTIONS = {"--config", "-c", "--context", "-H", "--host", "-l", "--log-level", "--tlscacert",
                        "--tlscert", "--tlskey"}
# The options of `docker run` and `docker create` that take a value of their own, so the image is
# the first word after them that is no option.
DOCKER_RUN_VALUE_OPTIONS = {
    "-e", "--env", "--env-file", "-v", "--volume", "-w", "--workdir", "--name", "-p", "--publish", "--entrypoint",
    "--network", "--net", "-u", "--user", "--mount", "--cpus", "--memory", "-m", "--add-host", "--tmpfs", "--label",
    "-l", "--ipc", "--platform", "--pull", "--cidfile", "--device", "--dns", "--gpus", "-h", "--hostname",
    "--log-driver", "--log-opt", "--memory-swap", "--pid", "--restart", "--security-opt", "--shm-size",
    "--stop-signal", "--stop-timeout", "--ulimit", "--userns", "--uts", "--volumes-from", "-a", "--attach",
    "--cap-add", "--cap-drop", "--cgroupns", "--group-add", "--health-cmd", "--init-path", "--isolation",
    "--link", "--mac-address", "--runtime", "--sysctl", "--expose", "--cpu-shares", "-c", "--cpuset-cpus",
}
# The runners of a package's own command, with their options that take a value, the options that
# fetch a package the project does not hold (`npx --yes cowsay`), the options that forbid a fetch,
# the kind of each runner's fetch, and the packages whose `install` downloads Playwright's browsers.
# A runner fetches a package the project does not hold whether or not it is given `--yes`: npm's
# exec "assumes --yes when standard input is not a TTY or a CI environment is detected", and bunx
# installs a package it does not find; so a runner of a package no lockfile of this repository holds
# (by its name or a command name it installs), or of a version spec (`cowsay@1.5.0`), is a fetch.
# `pnpm dlx`, `yarn dlx` and `pipx run` fetch whatever they run.
PACKAGE_RUNNERS = {"bunx": {"-p", "--package"}, "npx": {"-p", "--package"}, "pnpx": set()}
RUNNER_FETCHES = {"--yes", "-y", "-p", "--package"}
RUNNER_OFFLINE = {"--no", "--no-install", "--offline"}
RUNNER_KIND = {"bunx": "bun", "npx": "npm", "pnpx": "pnpm"}
ALWAYS_FETCH = {("pnpm", "dlx"): "pnpm", ("yarn", "dlx"): "yarn", ("pipx", "run"): "pip"}
PLAYWRIGHT_PACKAGES = {"playwright", "@playwright/test", "playwright-core"}
# The hosts a curl or a wget may read with no bound of its own: this runner's own servers. A URL
# whose host is one of these (or 127.x.y.z), and a `file:` URL, is the one read the rule can tell
# is local; every other URL waits on the network, however it is written (a variable, a substitution
# such as the worker image's `"$(gh_raw_url …)"`), since its spelling does not make it local.
LOCAL_HOSTS = {"localhost", "0.0.0.0", "[::1]"}
# curl's options that take a value of their own (curl 8.5.0's `--help all`, and `-x`/`--proxy`,
# whose value it prints in brackets), so its URLs are the other words that are no option; a short
# cluster takes its value from the rest of its word, or from the next word where one of these ends
# it (`-fsSIo /dev/null`).
CURL_VALUE_OPTIONS = {
    "--abstract-unix-socket", "--alt-svc", "--aws-sigv4", "--cacert", "--capath", "--cert", "--cert-type",
    "--ciphers", "--config", "--connect-timeout", "--connect-to", "--continue-at", "--cookie", "--cookie-jar",
    "--create-file-mode", "--crlfile", "--curves", "--data", "--data-ascii", "--data-binary", "--data-raw",
    "--data-urlencode", "--delegation", "--dns-interface", "--dns-ipv4-addr", "--dns-ipv6-addr", "--dns-servers",
    "--doh-url", "--dump-header", "--egd-file", "--engine", "--etag-compare", "--etag-save", "--expect100-timeout",
    "--form", "--form-string", "--ftp-account", "--ftp-alternative-to-user", "--ftp-method", "--ftp-port",
    "--ftp-ssl-ccc-mode", "--happy-eyeballs-timeout-ms", "--header", "--help", "--hostpubmd5", "--hostpubsha256",
    "--hsts", "--interface", "--ipfs-gateway", "--json", "--keepalive-time", "--key", "--key-type", "--krb",
    "--libcurl", "--limit-rate", "--local-port", "--login-options", "--mail-auth", "--mail-from", "--mail-rcpt",
    "--max-filesize", "--max-redirs", "--max-time", "--netrc-file", "--noproxy", "--oauth2-bearer", "--output",
    "--output-dir", "--parallel-max", "--pass", "--pinnedpubkey", "--preproxy", "--proto", "--proto-default",
    "--proto-redir", "--proxy", "--proxy-cacert", "--proxy-capath", "--proxy-cert", "--proxy-cert-type",
    "--proxy-ciphers", "--proxy-crlfile", "--proxy-header", "--proxy-key", "--proxy-key-type", "--proxy-pass",
    "--proxy-pinnedpubkey", "--proxy-service-name", "--proxy-tls13-ciphers", "--proxy-tlsauthtype",
    "--proxy-tlspassword", "--proxy-tlsuser", "--proxy-user", "--proxy1", "--pubkey", "--quote", "--random-file",
    "--range", "--rate", "--referer", "--request", "--request-target", "--resolve", "--retry", "--retry-delay",
    "--retry-max-time", "--sasl-authzid", "--service-name", "--socks4", "--socks4a", "--socks5",
    "--socks5-gssapi-service", "--socks5-hostname", "--speed-limit", "--speed-time", "--stderr", "--telnet-option",
    "--tftp-blksize", "--time-cond", "--tls-max", "--tls13-ciphers", "--tlsauthtype", "--tlspassword", "--tlsuser",
    "--trace", "--trace-ascii", "--trace-config", "--unix-socket", "--upload-file", "--url-query", "--user",
    "--user-agent", "--variable", "--write-out",
}
CURL_SHORT_VALUES = set("ACDEFHKPQTUXYbcdehmortuwxyz")
# wget's options that take a value of their own, as separate words (wget's long options take theirs
# after a `=`, in the same word).
WGET_VALUE_OPTIONS = {"-O", "-o", "-a", "-P", "-e", "-U", "-T", "-t", "-w", "-i", "-B", "-D", "-I", "-X", "-A",
                      "-R", "-l", "-Q", "--output-document", "--output-file", "--append-output"}
# The Lean commands that download the toolchain lean-toolchain names when elan has not installed it.
LEAN_COMMANDS = {"lake", "lean", "leanc"}


def from_verb(rest: list, value_options: set) -> list:
    """The words from the first that is no option on, each option's own value taken with it."""
    index = 0
    while index < len(rest):
        if rest[index] in value_options:
            index += 2
        elif rest[index].startswith(("-", "+")):
            index += 1
        else:
            return rest[index:]
    return []


def _playwright_install(rest: list, value_options: set) -> bool:
    """Whether a package runner's words (`playwright install chromium`, `-p @playwright/test
    playwright install`) run Playwright's browser download."""
    words = from_verb(rest, value_options)
    package = words[0].rsplit("@", 1)[0] if words[:1] and "@" in words[0][1:] else (words[:1] or [""])[0]
    return package in PLAYWRIGHT_PACKAGES and from_verb(words[1:], set())[:1] == ["install"]


def _runner_fetch(runner: str, rest: list, held: frozenset) -> bool:
    """Whether a package runner's words fetch a package: an option that asks for one (`--yes`,
    `--package`), a version spec, or a package `held` does not name; never under an option that
    forbids a fetch (`--no`, `--no-install`, `--offline`)."""
    words = from_verb(rest, PACKAGE_RUNNERS[runner])
    leading = rest[:len(rest) - len(words)]
    if any(word in RUNNER_OFFLINE for word in leading):
        return False
    if any(word in RUNNER_FETCHES or word.startswith("--package=") for word in leading):
        return True
    package = (words or [""])[0]
    return bool(package) and ("@" in package[1:] or package not in held)


@dataclass
class Docker:
    """A docker command: its verb as the rule names it, the image it runs or pulls where it names
    one, and its `--pull` policy."""
    verb: str
    image: str = ""
    pull: str = ""


# The docker builds: `docker build` reads one Dockerfile the rule can follow; `docker compose build`
# and `docker buildx bake` read theirs from a compose or bake file, which the rule does not read.
DOCKER_BUILDS = {"docker build", "docker compose build", "docker buildx bake"}


def docker_command(name: str, rest: list) -> Docker | None:
    """The docker (or docker-compose) command the words make, where it can reach a registry: a pull
    (`docker pull`, `docker image pull`, `docker compose pull`), a container of an image (`docker
    run`, `docker create`, `docker container run|create`, `docker compose up|run|create`) or a
    build (`docker build`, `docker buildx build`, `docker image build`, `docker compose build`,
    `docker buildx bake`)."""
    command = from_verb(rest, DOCKER_VALUE_OPTIONS) if name == "docker" else ["compose"] + rest
    if command[:1] == ["pull"] or command[:2] == ["image", "pull"]:
        return Docker("docker pull")
    if command[:1] == ["compose"]:
        verb = from_verb(command[1:], {"-f", "--file", "-p", "--project-name", "--profile", "--env-file"})
        if verb[:1] == ["pull"]:
            return Docker("docker pull")
        if verb[:1] == ["build"]:
            return Docker("docker compose build")
        if verb[:1] in (["up"], ["run"], ["create"]):
            pull = next((w.split("=", 1)[1] for w in verb if w.startswith("--pull=")), "")
            pull = pull or next((verb[i + 1] for i, w in enumerate(verb[:-1]) if w == "--pull"), "")
            return Docker(f"docker compose {verb[0]}", pull=pull)
        return None
    if command[:1] in (["build"],) or command[:2] in (["buildx", "build"], ["image", "build"]):
        return Docker("docker build")
    if command[:2] == ["buildx", "bake"] or command[:1] == ["bake"]:
        return Docker("docker buildx bake")
    verb = command[1:2] if command[:1] == ["container"] else command[:1]
    if verb not in (["run"], ["create"]):
        return None
    words = command[2:] if command[:1] == ["container"] else command[1:]
    pull, index = "", 0
    while index < len(words) and words[index].startswith("-"):
        word = words[index]
        if word.startswith("--pull="):
            pull = word.split("=", 1)[1]
        elif word == "--pull" and index + 1 < len(words):
            pull = words[index + 1]
        if word in DOCKER_RUN_VALUE_OPTIONS:
            index += 1
        index += 1
    return Docker(f"docker {verb[0]}", words[index] if index < len(words) else "", pull)


def _url_words(rest: list, long_values: set, short_values: set, hiding: set) -> list | None:
    """The URLs a curl's or a wget's words name: each word that is no option and no option's value,
    and curl's `--url` values; None where an option of `hiding` reads more words from elsewhere
    (curl's `-K`/`--config`, wget's `-i` and `-e`), whose URLs the rule cannot read."""
    urls, index = [], 0
    while index < len(rest):
        word = rest[index]
        index += 1
        if word in hiding or word.split("=", 1)[0] in hiding:
            return None
        if word == "--url":
            urls += rest[index:index + 1]
            index += 1
        elif word in long_values:
            index += 1
        elif word.startswith("--") or word == "-":
            continue
        elif word.startswith("-") and len(word) > 1:
            cluster = word[1:]
            for position, letter in enumerate(cluster):
                if f"-{letter}" in hiding:
                    return None
                if letter in short_values:
                    index += position == len(cluster) - 1  # its value is the next word
                    break
        else:
            urls.append(word)
    return urls


def _local_url(word: str, local: frozenset) -> bool:
    """Whether a URL is one of this runner's own: a `file:` URL, a URL whose host is written out and
    is one of LOCAL_HOSTS or 127.x.y.z (curl reads a URL with no scheme as http://), or a URL that is
    a variable the text sets only to such URLs (`local`)."""
    if word.lower().startswith("file:"):
        return True
    match = re.fullmatch(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?(/.*)?", word)
    if match:
        return match.group(1) in local
    host = re.match(r"^(?:[A-Za-z][A-Za-z0-9+.-]*://)?(?:[^@/]*@)?(\[[^]]*\]|[^/:?#]*)", word).group(1).lower()
    return bool(host) and literal_word(host) and (host in LOCAL_HOSTS or host.startswith("127."))


def local_urls(assignments: list, known: dict) -> frozenset:
    """The variables a text, or its step's env, sets to this runner's own URLs alone: every value it
    gives the name is a local URL (_local_url), so a curl of `"$base/health"` reads that server."""
    values: dict = {}
    for name, value in list(known.items()) + list(assignments):
        values.setdefault(name, []).append(value)
    return frozenset(name for name, given in values.items()
                     if given and all(value and _local_url(value, frozenset()) for value in given))


def _curl_install(rest: list, local: frozenset = frozenset()) -> bool:
    """Whether a curl waits on the network with no bound of its own: it carries no --max-time
    (curl's own retries and its --connect-timeout leave a body that trickles unbounded) and names a
    URL that is not this runner's own, however that URL is written (_local_url)."""
    if any(w in ("-m", "--max-time") or w.startswith("--max-time=") or re.fullmatch(r"-[A-Za-z]*m[0-9.]*", w)
           for w in rest):
        return False
    urls = _url_words(rest, CURL_VALUE_OPTIONS | {"-x"}, CURL_SHORT_VALUES, {"-K", "--config"})
    return urls is None or any(not _local_url(url, local) for url in urls)


def _wget_install(rest: list, local: frozenset = frozenset()) -> bool:
    """Whether a wget reads the network: it names a URL that is not this runner's own. wget has no
    bound on a whole transfer (its --timeout is a wait for the next byte, which a trickle never
    ends), so every such wget is an install."""
    urls = _url_words(rest, WGET_VALUE_OPTIONS, set("OoaPeUTtwiBDIXARlQ"),
                      {"-i", "--input-file", "-e", "--execute"})
    return urls is None or any(not _local_url(url, local) for url in urls)


def network_install(words: list, local: frozenset = frozenset(), held: frozenset = frozenset()) -> tuple[str, str] | None:
    """The install a command makes from the network, as the words that say so (`apt-get update`,
    `bun install`) and its kind, network-install.sh's word for it (`apt`, `bun`, `docker`,
    `playwright`, …; an install that script has no kind for is of a kind named after its
    program), or None where the command makes none. `local` names the variables the text sets to
    this runner's own URLs alone (local_urls); `held`, the packages and command names this
    repository's lockfiles install (a package runner of one of them fetches nothing). A docker
    container of an image, a docker build and a Lean command are installs the job's reader decides
    on, since what an earlier step pulled or installed decides them."""
    name = words[0].rsplit("/", 1)[-1]
    rest = words[1:]
    if name in APT:
        verb = (from_verb(rest, APT_VALUE_OPTIONS) or [""])[0]
        if verb in APT_DPKG_VERBS and "--no-download" in rest:  # apt's own flag: no download at all
            return None
        return (f"{name} {verb}", "apt") if verb in APT_VERBS else None
    if name == "wget":
        return ("wget", "wget") if _wget_install(rest, local) else None
    if name == "curl":
        return ("curl", "curl") if _curl_install(rest, local) else None
    if name in ("docker", "docker-compose"):
        docker = docker_command(name, rest)
        if docker is None or docker.verb in DOCKER_BUILDS or docker.pull == "never":
            return None
        return (docker.verb, "docker")
    runner = name if name in PACKAGE_RUNNERS else ""
    command = from_verb(rest, INSTALLERS[name][1]) if name in INSTALLERS else []
    if (name, (command or [""])[0]) in ALWAYS_FETCH:
        return (f"{name} {command[0]}", ALWAYS_FETCH[(name, command[0])])
    if name == "bun" and command[:1] == ["x"]:  # `bun x` is bunx
        runner, rest = "bunx", command[1:]
    elif name == "npm" and command[:1] == ["exec"]:  # `npm exec` is npx
        runner, rest = "npx", [word for word in command[1:] if word != "--"]
    if runner:
        if _playwright_install(rest, PACKAGE_RUNNERS[runner]):
            return ("playwright install", "playwright")
        return (f"{name} {command[0]}" if runner != name else name, RUNNER_KIND[runner]) \
            if _runner_fetch(runner, rest, held) else None
    if name in ("uv", "uvx") and "--offline" in rest:  # uv's own flag: "Disable network access"
        return None
    if name == "uvx":
        return ("uvx", "uv") if from_verb(rest, UV_VALUE_OPTIONS | {"--from", "--with"}) else None
    if name == "uv":
        command = from_verb(rest, UV_VALUE_OPTIONS)
        for verbs in UV_INSTALLS:
            if command[:len(verbs)] == verbs:
                return (f"uv {' '.join(verbs)}", "uv")
        return None
    if name.startswith("python") and rest[:2] == ["-m", "pip"] and "install" in rest[2:]:
        return (f"{name} -m pip install", "pip")
    if name == "go":
        fetch = go_fetch(rest)
        return (fetch, "go") if fetch else None
    if name == "elan":
        fetch = elan_fetch(rest)
        return (fetch, "elan") if fetch else None
    if name == "gh":
        command = from_verb(rest, {"-R", "--repo"})
        return ("gh release download", "gh") if command[:2] == ["release", "download"] else None
    if name in INSTALLERS:
        verbs, value_options = INSTALLERS[name]
        command = from_verb(rest, value_options)
        verb = (command or [""])[0]
        if name == "yarn" and not command and not {"--version", "-v", "--help", "-h"} & set(rest):
            return ("yarn install", "yarn")
        if name == "yarn" and tuple(command[:2]) in YARN_FETCHES_TWO:
            return (f"yarn {command[0]} {command[1]}", "yarn")
        return (f"{name} {verb}", KIND_OF.get(name, name)) if verb in verbs else None
    return None


# The go fetches that compile nothing, the only go commands network-install.sh's kind 'go' may wrap:
# a compile under that bound is timed as a wait on the network (worker-image run 44772738928:
# gopls's `go install` took 80 s of the 120 s bound, 74 s of it compile after 6.1 s of downloads).
GO_DOWNLOADS = {"go get", "go mod download", "go mod tidy", "go mod vendor"}


def go_fetch(rest: list) -> str:
    """The fetch a go command names, as words (`go mod download`, `go install <module>@<version>`),
    or "" where it names none. A step that names a fetch is an install; the modules a `go build`,
    `go test`, `go vet` or `go run` of a package of this repository downloads by itself are not,
    since no step names them (pull request #4434 moves those into one fetch step per job, which
    this rule then reads)."""
    if rest[:1] == ["-C"]:
        rest = rest[2:]
    words = [word for word in rest if not word.startswith("-")]
    if words[:1] == ["mod"] and words[1:2] in (["download"], ["tidy"], ["vendor"]):
        return f"go mod {words[1]}"
    if words[:1] == ["get"]:
        return "go get"
    if words[:1] in (["install"], ["run"]) and any("@" in word for word in words[1:]):
        return f"go {words[0]} <module>@<version>"
    return ""


GO_DURATION = re.compile(r"(\d+(?:\.\d*)?|\.\d+)(h|ms|m|s|us|µs|ns)")


def go_test_timeout(words: list) -> int:
    """The seconds a `go test` command's `-timeout` gives its test binary (a Go duration such as
    `20m` or `1h30m`, rounded up), or 0 where the words are no `go test` or set none (go's own
    default is 10 minutes, but a step that leans on it states no bound), or set `-timeout 0`, which
    lifts the bound."""
    if words[0].rsplit("/", 1)[-1] != "go":
        return 0
    rest = words[3:] if words[1:2] == ["-C"] else words[1:]
    if rest[:1] != ["test"]:
        return 0
    value = ""
    for index, word in enumerate(rest):
        if word in ("-timeout", "--timeout") and index + 1 < len(rest):
            value = rest[index + 1]
        elif word.startswith(("-timeout=", "--timeout=")):
            value = word.split("=", 1)[1]
    parts = GO_DURATION.findall(value)
    if not parts or "".join(number + unit for number, unit in parts) != value:
        return 0
    scale = {"h": 3600, "m": 60, "s": 1, "ms": 0.001, "us": 1e-6, "µs": 1e-6, "ns": 1e-9}
    return -int(-sum(float(number) * scale[unit] for number, unit in parts) // 1)


def elan_fetch(rest: list) -> str:
    """The download an elan command makes of a Lean toolchain, as words (`elan toolchain install`,
    `elan default`, `elan override set`, `elan run`), or ""."""
    words = [word for word in rest if not word.startswith("-")]
    for verbs in (["toolchain", "install"], ["default"], ["override", "set"], ["run"]):
        if words[:len(verbs)] == verbs and len(words) > len(verbs):
            return f"elan {' '.join(verbs)}"
    return ""

