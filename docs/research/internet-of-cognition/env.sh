#!/usr/bin/env bash
#
# env.sh prepares what the recorded commands of claims.jsonl read and this tree cannot
# hold: a python interpreter with the libraries the similarity and the SCOPE rows need, a
# pinned model, a mas-lab installation, the SHACL shapes the scope rows validate against,
# the two Dispatch records under check, and the review corpus one search reads. It exports
# one variable per input, and check.sh runs every recorded command from the root of this
# repository, so that no recorded command names a directory of the pod this reading was
# made in; the three predicates below that record such a directory are named with it.
# check.sh sources it once before it runs the first recorded command; it can also be run
# on its own to build the cache and print what it exported:
#
#   bash docs/research/internet-of-cognition/env.sh
#
# It is idempotent. A second run reuses what the first built whole: it re-downloads
# nothing, re-installs nothing but an environment a recorded command has installed into
# since, and, run on its own, re-reads Dispatch only when a copy is missing. Nothing a run
# that was killed part-way left is reused: each entry below is kept whole in one of the
# three ways check.sh's header states ("Nothing a killed run leaves is trusted"), and the
# list after the exports names which. To build it all again from nothing, remove the cache
# directory, after making it writable, since go makes its module cache read-only:
#
#   chmod -R u+w "$IOC_CACHE" && rm -rf "$IOC_CACHE"
#
# It builds under one directory, IOC_CACHE, which defaults to $TMPDIR/infra3134, and it
# clones under a second, IOC_CLONES, which defaults to $TMPDIR/ioc-clones; it writes nothing
# into this repository. Every tool cache, its own and a recorded command's, is under
# IOC_CACHE, one variable per cache, which ioc_env_cache_table below names and
# ioc_env_caches exports. Where the gate writes, the empty TMPDIR each recorded command runs
# with, and what a tool still writes outside both roots are stated once, in check.sh's
# header ("Where it writes"). No recorded command and no script it runs names either
# default root or a cache of its own: each reads them through these variables, so a reader
# may move either root, and a fresh IOC_CACHE is a machine that never ran the replay.
#
# What it exports, one line per input:
#
#   IOC_CACHE       the cache root, everything below is under it
#   IOC_CLONES      the clone root: the three clones at the commits sources.json pins, and
#                   agent-infra, the private repository five rows read with git, fetched
#                   blobless at the commits its private_repositories entry pins
#   IOC_EVID        the absolute path of the committed evidence/ directory
#   IOC_DESIGN      the design under check, dispatch://INFRA-3134/artifact/ioc-design-2026-11-12-md
#   IOC_COMMENT     the reconciliation under check, dispatch://INFRA-3134/comment/326a044a-…
#                   check.sh --replay reads both records itself, live, before it sources
#                   env.sh, and names its own copies here; env.sh then reads neither
#   IOC_VENV        the virtual environment uv builds for the similarity and scope rows
#   IOC_PY          that environment's python: sentence-transformers, torch, rdflib, pyshacl
#                   and pypdf
#   IOC_MODEL_DIR   the pinned all-MiniLM-L6-v2 snapshot, its model.safetensors checksummed
#   IOC_MASLAB_VENV       mas-lab runtime, ctl and library-standard installed from the clone
#   IOC_MASLAB_WHEEL_VENV the second, empty environment the two schema rows install into
#   IOC_MASLAB_PY IOC_MASLAB_CTL  the python and the mas-ctl of IOC_MASLAB_VENV
#   IOC_MASLAB_WHEEL_CTL  the mas-ctl of IOC_MASLAB_WHEEL_VENV
#   IOC_SCOPE_SHAPES      the shapes graph the ts- rows validate against
#   IOC_SCOPE_MAS_SHAPES  where the shapes generator of the scope clone writes
#   IOC_TC_WORK     the working directory evidence/concord-build-run.sh runs in
#   IOC_TC_PY       the python of that directory's venv
#   IOC_CONCORD_WHEEL  the wheel of the clone's packages/ioc-concord, built once under
#                   evidence/build-constraints.txt, which the tc-run rows run with uv run
#   Each of IOC_PY, IOC_MASLAB_PY, IOC_MASLAB_CTL, IOC_MASLAB_WHEEL_CTL and IOC_TC_PY
#   names one program, and they are the only variables a recorded command may run.
#   IOC_REVIEWS     the review corpus of tc-corpus-1, fetched with curl from GitHub's REST
#                   API, sending the header file IOC_GITHUB_AUTH_LEGION names. A review a
#                   reader may not see is not in it: a PENDING review is visible to its
#                   author alone, and the row counts only the bodies submitted by its instant.
#   IOC_GITHUB_AUTH_LEGION IOC_GITHUB_AUTH_AGENT_INFRA  the header files of the token the
#                   machine's git credential helper gives for cybermonklabs/legion and for
#                   theorem-labs/agent-infra, each empty when it gives none
#                   (ioc_env_github_table); every recorded curl of GitHub's REST API sends
#                   the one of the repository it reads with -H @"$<variable>". check.sh
#                   --replay writes both under its run's scratch before it sources env.sh;
#                   run on its own, env.sh writes them under a scratch of its own, which it
#                   removes when it exits
#   IOC_OUR_TREE    our repository's go.work and its packages/daemon and packages/envoy
#                   as they stand at the commit sources.json pins, which every row that
#                   counts or tests our code reads; the working tree carries whatever the
#                   base branch merged since, and the claims are of the pinned commit
#   IOC_TM_OVERLAY  the go overlay of tm-our-count, written for IOC_OUR_TREE
#   ACCORD_SRC      the cognition-protocols clone, which similarity.py imports from
#   the variables of ioc_env_cache_table   every tool cache, each under IOC_CACHE
#   the variables of IOC_ENV_ENFORCED      the variables env.sh sets or clears in every
#                   row's environment, whatever the caller's shell held: it exports the
#                   three that enforce the locks, UV_NO_CONFIG=1 and
#                   PIP_CONFIG_FILE=/dev/null, and unsets the other eighteen: twelve, each
#                   a source of packages or a configuration file of uv or pip,
#                   UV_NO_VERIFY_HASHES, and five optional inputs of mas-lab's programs,
#                   which every row then reads as its default
#
# What it keeps, and the way each entry is kept whole. The rule, its three ways, and what
# a recorded command writes under IOC_CACHE are stated once, in check.sh's header ("Nothing
# a killed run leaves is trusted"):
#
#   entry                                   kept whole by
#   each clone, IOC_CLONES/<name>           a stamp, .git/ioc-complete, naming the pinned
#                                           commit, or for agent-infra the pinned commits
#                                           and paths; a clone without it is cloned again
#   ioc-design.md, comment-326a.md          a temporary name, <file>.part, renamed when the
#                                           read ended well; only when run on its own
#   IOC_VENV, IOC_MASLAB_VENV,              a stamp, .ioc-built-from, of what it was built
#   IOC_MASLAB_WHEEL_VENV, IOC_TC_WORK/venv from, removed before env.sh changes it and by
#                                           check.sh before a row installs into it; one
#                                           without it is built again from nothing
#   IOC_MODEL_DIR                           a stamp, model.built-from, of its revision and
#                                           checksum; huggingface writes each file under a
#                                           temporary name of its own
#   IOC_CONCORD_WHEEL                       a stamp, <wheel>.built-from, removed before the
#                                           build
#   IOC_SCOPE_MAS_SHAPES, IOC_SCOPE_SHAPES  a temporary name, <file>.part
#   IOC_REVIEWS                             a temporary name, reviews.jsonl.part
#   IOC_OUR_TREE                            a temporary name, <dir>.part, holding a stamp,
#                                           .complete
#   IOC_TM_OVERLAY                          written again at every run
#   the caches of ioc_env_cache_table       each tool, by its own means
#
# Every package env.sh installs comes from a committed, hashed file under evidence/,
# resolved as the package index stood at 2026-11-12T07:55:56Z, the instant sources.json
# records: IOC_VENV is synced exactly to ioc-venv.lock, the mas-lab environments read
# tm-constraints.txt, and every local build reads build-constraints.txt with -b. env.sh
# exports the variables uv and pip enforce those hashes by, and unsets every variable by
# which the caller's shell would point uv or pip at another source of packages or at a
# configuration file, or keep uv from checking a hash (IOC_ENV_ENFORCED); after it builds
# each environment of IOC_ENV_FROZEN it checks what the environment holds against that
# environment's own lock (ioc_env_frozen). The rule these serve, the one install written
# with the visible prefix UV_REQUIRE_HASHES=0, and what none of them can see are stated
# once, in check.sh's header above reader_py. The stamp of each environment and of the
# ioc-concord wheel is the sha256 of the files and the clone commit it was built from, so a
# venv or a wheel that predates the hashed files is never used.
#
# It needs uv, git, jq, curl, python3 and dispatch, and it needs the three clones and
# agent-infra's, which it clones at the commits sources.json pins when they are missing or
# not whole. It reads GitHub through git and the REST API and never through gh, which in a
# Legion pod is scoped to this project's owners alone. It refuses,
# with a line naming what it could not prepare and a non-zero status, rather than leaving a
# variable pointing at something that is not there.

ioc_env_refuse() {
    printf 'env.sh cannot prepare %s: %s\n' "$1" "$2" >&2
    return 1
}

ioc_env_need() {
    local tool missing=""
    for tool in "$@"; do
        command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
    done
    [ -z "$missing" ] || ioc_env_refuse "anything" "this machine has no$missing"
}

# One clone, at the commit sources.json pins, with the working tree at that commit:
# the shapes generator, the mas-lab install and similarity.py all read files and not
# git objects. The clone is reused only when its stamp, .git/ioc-complete, written after
# its checkout, names that commit and its HEAD is that commit; any other, one a fetch or a
# checkout that was killed part-way left among them, is removed and cloned again.
ioc_env_clone() {
    local name="$1" url commit dir stamp
    url="$(jq -r --arg n "$name" '.repositories[] | select(.name==$n) | .url' "$IOC_ENV_HERE/sources.json")"
    commit="$(jq -r --arg n "$name" '.repositories[] | select(.name==$n) | .commit' "$IOC_ENV_HERE/sources.json")"
    [ -n "$url" ] && [ -n "$commit" ] || ioc_env_refuse "the clone $name" "sources.json pins no such repository" || return 1
    dir="$IOC_CLONES/$name"
    stamp="$dir/.git/ioc-complete"
    if [ "$(cat "$stamp" 2>/dev/null)" = "$commit" ] && [ "$(git -C "$dir" rev-parse HEAD 2>/dev/null)" = "$commit" ]; then
        return 0
    fi
    # The clone is the one the triage read: one commit deep and with no tags. The triage
    # recorded that shape as a fact of its own reading, so a clone of the whole history
    # would answer those rows differently, and the maturity of a repository is read from
    # its own REST answers, which are committed under published/github-api/.
    rm -rf "$dir" && mkdir -p "$dir" && git -C "$dir" init --quiet >&2 ||
        ioc_env_refuse "the clone $name" "git init of $dir failed" || return 1
    git -C "$dir" remote add origin "$url" ||
        ioc_env_refuse "the clone $name" "git remote add of $url failed" || return 1
    git -C "$dir" fetch --quiet --depth 1 --no-tags origin "$commit" >&2 ||
        ioc_env_refuse "the clone $name" "git fetch of $commit from $url failed" || return 1
    git -C "$dir" checkout --quiet --detach "$commit" >&2 ||
        ioc_env_refuse "the clone $name" "it holds no commit $commit" || return 1
    printf '%s\n' "$commit" >"$stamp" ||
        ioc_env_refuse "the clone $name" "its stamp $stamp could not be written"
}

# ioc_env_git <git arguments>; git that never prompts. The private repository and every
# token of GitHub reach git through the machine's credential helper, which in a Legion pod
# is `legion git-credential` and answers for the repository's path (credential.useHttpPath).
# GIT_TERMINAL_PROMPT=0 keeps git from asking on the terminal, and an empty GIT_ASKPASS keeps
# it from running core.askPass or SSH_ASKPASS in its place, so a machine with no credential
# for a repository gets git's refusal and never a prompt.
ioc_env_git() { GIT_TERMINAL_PROMPT=0 GIT_ASKPASS='' git "$@"; }

# The private repository five rows read with git, theorem-labs/agent-infra, as a fourth
# clone under IOC_CLONES, by its entry of sources.json's private_repositories. It is fetched
# blobless (--filter=blob:none), with no checkout, at exactly the commits the entry pins;
# then the blobs of the paths the entry names, at each of those commits, are fetched by
# their ids in one request, so each row reads every file it reads from the clone and none
# fetches a blob of its own. The clone is reused only when its stamp, .git/ioc-complete,
# written last, names those commits and paths; any other, one a fetch that was killed
# part-way left among them, is removed and fetched again.
ioc_env_clone_private() {
    local name="$1" entry url commits paths dir stamp want commit
    entry="$(jq -c --arg n "$name" '.private_repositories[]? | select(.name==$n)' "$IOC_ENV_HERE/sources.json")"
    url="$(printf '%s' "$entry" | jq -r '.url // empty')"
    commits="$(printf '%s' "$entry" | jq -r '.commits[]?')"
    paths="$(printf '%s' "$entry" | jq -r '.paths[]?')"
    [ -n "$url" ] && [ -n "$commits" ] && [ -n "$paths" ] ||
        ioc_env_refuse "the clone $name" "sources.json pins no such private repository, with its commits and its paths" || return 1
    dir="$IOC_CLONES/$name"
    stamp="$dir/.git/ioc-complete"
    want="$(printf '%s\n' "$commits" "$paths")"
    [ "$(cat "$stamp" 2>/dev/null)" = "$want" ] && return 0
    rm -rf "$dir" && mkdir -p "$dir" && git -C "$dir" init --quiet >&2 ||
        ioc_env_refuse "the clone $name" "git init of $dir failed" || return 1
    git -C "$dir" remote add origin "$url" ||
        ioc_env_refuse "the clone $name" "git remote add of $url failed" || return 1
    # shellcheck disable=SC2086
    ioc_env_git -C "$dir" fetch --quiet --filter=blob:none --no-tags origin $commits >&2 ||
        ioc_env_refuse "the clone $name" "git fetch of the commits sources.json pins from $url failed, so git cannot read it here" || return 1
    : >"$dir/.git/ioc-blobs"
    for commit in $commits; do
        # shellcheck disable=SC2086
        git -C "$dir" ls-tree -r "$commit" -- $paths >>"$dir/.git/ioc-blobs" ||
            ioc_env_refuse "the clone $name" "it holds no tree of $commit" || return 1
    done
    # shellcheck disable=SC2086
    awk '$2 == "blob" { print $3 }' "$dir/.git/ioc-blobs" | sort -u | ioc_env_git -C "$dir" \
        -c fetch.negotiationAlgorithm=noop fetch --quiet --no-tags --no-write-fetch-head --filter=blob:none --stdin origin >&2 ||
        ioc_env_refuse "the clone $name" "git fetch of the files under $(printf '%s, ' $paths)at the pinned commits, from $url, failed" || return 1
    printf '%s\n' "$want" >"$stamp" ||
        ioc_env_refuse "the clone $name" "its stamp $stamp could not be written"
}

# The GitHub repositories whose REST API a recorded curl reads, one per line as
# <variable>|<owner>/<repo>: the variable names the header file of that repository's token,
# which the row sends with curl -H @"$<variable>", and env.sh's read of the review corpus
# sends the first. One file per repository, since a Legion pod's helper gives a token per
# repository.
ioc_env_github_table() {
    cat <<'TABLE'
IOC_GITHUB_AUTH_LEGION|cybermonklabs/legion
IOC_GITHUB_AUTH_AGENT_INFRA|theorem-labs/agent-infra
TABLE
}

# ioc_env_github_header <owner>/<repo> <file>; writes <file>, readable by its owner alone
# from before its first byte, holding one HTTP header, Authorization: Bearer <token>, with
# the token git credential fill gives for https://github.com/<owner>/<repo>.git, or nothing
# when it gives none, which curl then sends as no header: a read with no token, held to
# GitHub's sixty requests an hour. The token goes from git's output into the file alone: it
# stands on no command line, and nothing here prints it; git's own refusal is dropped, since
# a machine with no credential is no failure. It returns non-zero only when the file cannot
# be written.
ioc_env_github_header() {
    (
        umask 077
        rm -f -- "$2" || exit 1
        printf 'protocol=https\nhost=github.com\npath=%s.git\n\n' "$1" |
            ioc_env_git -c credential.interactive=false credential fill 2>/dev/null |
            sed -n 's/^password=\(..*\)$/Authorization: Bearer \1/p' >"$2"
    )
}

# ioc_env_github_auth; writes the header file of each repository of ioc_env_github_table
# under IOC_ENV_SCRATCH, the directory the caller removes when it ends, and exports the
# variable that names it. check.sh --replay calls it with its run's scratch before it
# sources env.sh; run on its own, env.sh calls it with a scratch of its own. A shell that
# sources env.sh otherwise writes none, and env.sh reads the files that shell names.
ioc_env_github_auth() {
    local name repo
    [ -n "${IOC_ENV_SCRATCH:-}" ] || ioc_env_refuse "the GitHub header files" "no scratch directory is named for them" || return 1
    while IFS='|' read -r name repo; do
        ioc_env_github_header "$repo" "$IOC_ENV_SCRATCH/$name.header" ||
            ioc_env_refuse "$name" "the header file of $repo could not be written under $IOC_ENV_SCRATCH" || return 1
        export "$name=$IOC_ENV_SCRATCH/$name.header"
    done < <(ioc_env_github_table)
}

# Every tool cache, one line each: the variable the tool reads, the directory under
# IOC_CACHE it names, and what the tool keeps there. This is the one list of them:
# ioc_env_caches exports each, check.sh exports them by it when its fixture builds nothing,
# and check.sh --help prints it.
ioc_env_cache_table() {
    cat <<'TABLE'
UV_CACHE_DIR|cache/uv|uv: every package it downloads or builds, and each environment uv run makes
UV_PYTHON_INSTALL_DIR|cache/uv-python|uv: the interpreter it downloads when no python of the version asked for is on PATH
PIP_CACHE_DIR|cache/pip|pip: every package it downloads or builds
HF_HOME|cache/hf|huggingface_hub: every snapshot it downloads, IOC_MODEL_DIR among them
XDG_CACHE_HOME|cache/xdg|every other tool that keeps its cache by the XDG base directories
GOMODCACHE|cache/go/mod|go: every module it downloads
GOCACHE|cache/go/build|go: what it builds
TORCHINDUCTOR_CACHE_DIR|cache/torchinductor|torch: the directory its compiler keeps, which importing torch makes under TMPDIR otherwise
BUN_INSTALL_CACHE_DIR|cache/bun|bun: every package it installs
npm_config_cache|cache/npm|npm and npx: every package they fetch
TABLE
}

# ioc_env_caches; exports IOC_CACHE, $TMPDIR/infra3134 unless it is set, and each variable
# of ioc_env_cache_table under it. It makes no directory, since each tool makes its own.
ioc_env_caches() {
    local name dir _what
    export IOC_CACHE="${IOC_CACHE:-${TMPDIR:-/tmp}/infra3134}"
    while IFS='|' read -r name dir _what; do
        export "$name=$IOC_CACHE/$dir"
    done < <(ioc_env_cache_table)
}

# The variables env.sh sets or clears in every row's environment, each with its value, or
# its removal, from the case of the one loop below, whatever the caller's shell held.
# UV_REQUIRE_HASHES, UV_BUILD_CONSTRAINT and PIP_REQUIRE_HASHES enforce the locks.
# UV_NO_CONFIG=1 keeps uv from reading a configuration file, a uv.toml or the settings of a
# pyproject.toml, in the directory it runs in, above it or in the user's configuration
# directory, and PIP_CONFIG_FILE=/dev/null keeps pip from reading any. The other eighteen
# are unset. Twelve point uv or pip at another source of packages, change which index a
# package is taken from, keep them from their index or name a configuration file, and one
# the caller's shell set reached every install, so that uv looked for a package first at an
# index the caller named and the replay failed as though the evidence were wrong (test
# round 31). An install that needs a source names it on its own command line, as
# ioc_env_python gives uv torch's page with --find-links. UV_NO_VERIFY_HASHES, when set,
# keeps uv from checking the hashes a lock, a constraints file or a build constraints file
# carries. The last five are optional inputs of mas-lab's programs, which no row sets and
# each row reads as its default, unset: MAS_MANIFEST_VALIDATE=0 turns mas-ctl validate into
# 'validation skipped', MAS_MANIFEST_RESOLVE_REFS=0 skips its check of each referenced file
# with the same OK line, MAS_MANIFEST_STRICT is read by validate_file when it is given no
# strict, OPENAI_API_KEY is the key their model clients read when a manifest names no
# other, and tm-load.py prints whether it and MAS_LLM_API_KEY are set (the side-by-side
# audit of round 32).
# check.sh reads this list for the names it reserves, so a name set or cleared here is a
# name it reserves.
IOC_ENV_ENFORCED="UV_REQUIRE_HASHES UV_BUILD_CONSTRAINT PIP_REQUIRE_HASHES UV_NO_CONFIG PIP_CONFIG_FILE UV_INDEX_URL UV_EXTRA_INDEX_URL UV_INDEX UV_DEFAULT_INDEX UV_FIND_LINKS UV_INDEX_STRATEGY UV_NO_INDEX UV_CONFIG_FILE PIP_INDEX_URL PIP_EXTRA_INDEX_URL PIP_FIND_LINKS PIP_NO_INDEX UV_NO_VERIFY_HASHES MAS_MANIFEST_VALIDATE MAS_MANIFEST_RESOLVE_REFS MAS_MANIFEST_STRICT OPENAI_API_KEY MAS_LLM_API_KEY"

ioc_env_enforce() {
    local name value
    for name in $IOC_ENV_ENFORCED; do
        case "$name" in
        UV_REQUIRE_HASHES | PIP_REQUIRE_HASHES | UV_NO_CONFIG) value=1 ;;
        UV_BUILD_CONSTRAINT) value="$IOC_EVID/build-constraints.txt" ;;
        PIP_CONFIG_FILE) value=/dev/null ;;
        UV_INDEX_URL | UV_EXTRA_INDEX_URL | UV_INDEX | UV_DEFAULT_INDEX | UV_FIND_LINKS | \
            UV_INDEX_STRATEGY | UV_NO_INDEX | UV_CONFIG_FILE | PIP_INDEX_URL | \
            PIP_EXTRA_INDEX_URL | PIP_FIND_LINKS | PIP_NO_INDEX | UV_NO_VERIFY_HASHES | \
            MAS_MANIFEST_VALIDATE | MAS_MANIFEST_RESOLVE_REFS | MAS_MANIFEST_STRICT | \
            OPENAI_API_KEY | MAS_LLM_API_KEY)
            unset "$name"
            continue
            ;;
        *) ioc_env_refuse "$name" "IOC_ENV_ENFORCED names it and ioc_env_enforce holds no value for it" || return 1 ;;
        esac
        export "$name=$value"
    done
    [ -s "$UV_BUILD_CONSTRAINT" ] ||
        ioc_env_refuse "UV_BUILD_CONSTRAINT" "$UV_BUILD_CONSTRAINT, the hashed build constraint, is missing"
}

# ioc_env_stamp <commit> <file>...; the sha256 of each file and the clone commit a cache
# was built from, one line, which the cache keeps beside it.
ioc_env_stamp() {
    local commit="$1"
    shift
    printf '%s %s\n' "$commit" "$(sha256sum "$@" | cut -d' ' -f1 | tr '\n' ' ')"
}

# ioc_env_claims; the rows of claims.jsonl, of which env.sh reads the values
# ioc_env_row_fields names. When check.sh --replay sources env.sh they are the rows its one
# parse read, which it names as IOC_ENV_CLAIMS in its own shell, so no line the parse read
# no row of reaches jq here; run on its own, env.sh reads the file.
ioc_env_claims() { cat "${IOC_ENV_CLAIMS:-$IOC_ENV_HERE/claims.jsonl}"; }

# ioc_env_row_fields; the values env.sh reads from rows of claims.jsonl, one per line as
# <row id>|<field of the row's evidence>|<what env.sh reads it as>. Each is a named field
# of the row's evidence, and none is read from the row's predicate, so a row recorded again
# with another predicate changes nothing env.sh reads. check.sh's one parse holds each field
# to its type, and --replay, before it sources env.sh, fails claim-form, naming the row and
# the field, when no row carries one or when the field is not a whole line of the value its
# row's predicate records, and then stops, so env.sh never builds from such a row
# (check.sh's header, "The one parse of claims.jsonl").
ioc_env_row_fields() {
    cat <<'FIELDS'
s-model-checksum|sha256|the checksum of the model.safetensors it downloads
tc-corpus-1|corpus_floor|the least number of review bodies the corpus it fetches holds
FIELDS
}

# ioc_env_row_field <row id> <field>; prints that field of the evidence of the row, or
# nothing when no row of that id carries it.
ioc_env_row_field() {
    ioc_env_claims | jq -r --arg id "$1" --arg field "$2" 'select(.id == $id) | .evidence[$field] // empty' | head -1
}

# ioc_env_lock_py prints the one reader of a committed hashed lock, Python source that
# ioc_env_frozen below and check.sh's reader_py both run, so the two read a lock alike.
ioc_env_lock_py() {
    cat <<'PY'
import os, re

def normalized(name):
    return re.sub(r"[-_.]+", "-", name).lower()

def read_lock(path):
    # The committed hashed lock at path, as ({normalized name: version}, {normalized
    # name: the names of the lock's packages its "# via" lines say require it}, or None
    # when the lock carries no such line), or None for a file that is missing, holds no
    # requirement, holds a line that is no name==version, or holds a requirement that
    # carries no hash.
    try:
        lines = open(path).read().splitlines()
    except OSError:
        return None
    pins, via, hashed, current, in_via, annotated = {}, {}, set(), None, False, False
    for line in lines:
        pin = re.match(r"^([A-Za-z0-9][A-Za-z0-9._-]*)(?:\[[^]]*\])?==([^ ;\\]+)", line)
        if pin:
            current, in_via = normalized(pin.group(1)), False
            pins[current] = pin.group(2)
            via[current] = set()
        elif re.match(r"^[A-Za-z0-9]", line):
            return None
        if "--hash=sha256:" in line and current:
            hashed.add(current)
        note = re.match(r"^\s+#\s+via\b(.*)$", line)
        item = re.match(r"^\s+#\s+(\S+)\s*$", line)
        if note and current:
            in_via = annotated = True
            item = re.match(r"^\s*(\S+)\s*$", note.group(1))
        elif not (in_via and item):
            in_via, item = False, None
        if item and current and not item.group(1).startswith("-"):
            via[current].add(normalized(item.group(1)))
    if not pins or hashed != set(pins):
        return None
    return pins, (via if annotated else None)
PY
}

# The environments env.sh builds, each bound to the one committed hashed lock under
# evidence/ that both checks read: the exemption UV_REQUIRE_HASHES=0, whose -c must be
# the target environment's lock, and the check of what the environment holds after every
# build and every row that installs into it. Each entry is <label>:<its directory,
# written as the rows write it>:<its lock>[:<the pinned clone whose own packages it may
# hold>]. The tc-work venv shares IOC_VENV's lock: it holds ioc-concord alone, which
# declares no dependency, so a lock of its own would hold nothing, and any dependency
# ioc-concord gained would be hash-checked if ioc-venv.lock holds it and refused if not.
# shellcheck disable=SC2016
IOC_ENV_FROZEN='IOC_VENV:$IOC_VENV:ioc-venv.lock IOC_MASLAB_VENV:$IOC_MASLAB_VENV:tm-constraints.txt:mas-lab IOC_MASLAB_WHEEL_VENV:$IOC_MASLAB_WHEEL_VENV:tm-constraints.txt:mas-lab IOC_TC_WORK:$IOC_TC_WORK/venv:ioc-venv.lock:cognition-protocols'

# ioc_env_frozen_labels; the label of each environment of IOC_ENV_FROZEN, one per line.
ioc_env_frozen_labels() {
    local entry
    for entry in $IOC_ENV_FROZEN; do
        printf '%s\n' "${entry%%:*}"
    done
}

# ioc_env_frozen_entry <label>; sets IOC_ENV_ENTRY_WRITTEN, IOC_ENV_ENTRY_LOCK and
# IOC_ENV_ENTRY_CLONE to the fields of the entry <label> of IOC_ENV_FROZEN, and
# IOC_ENV_ENTRY_DIR to its directory, empty when the variable it is written with is not
# set. It returns non-zero when no entry has that label.
ioc_env_frozen_entry() {
    local entry label variable
    for entry in $IOC_ENV_FROZEN; do
        IFS=: read -r label IOC_ENV_ENTRY_WRITTEN IOC_ENV_ENTRY_LOCK IOC_ENV_ENTRY_CLONE <<<"$entry"
        [ "$label" = "$1" ] || continue
        variable="${IOC_ENV_ENTRY_WRITTEN#\$}"
        variable="${variable%%/*}"
        IOC_ENV_ENTRY_DIR=""
        [ -n "${!variable:-}" ] && IOC_ENV_ENTRY_DIR="${!variable}${IOC_ENV_ENTRY_WRITTEN#\$"$variable"}"
        return 0
    done
    return 1
}

# The stamp of an environment of IOC_ENV_FROZEN is the file .ioc-built-from in its
# directory, holding the line ioc_env_stamp writes of what env.sh built it from. env.sh
# removes the stamp before it changes the environment and writes it last, once the
# environment is built and its check passed, and it reuses an environment only when the
# stamp is there and names what it would build it from now; any other, one an install that
# was killed part-way left among them, is built again from nothing. A row's install is no
# build of env.sh's, so check.sh removes the stamp of each environment a row installs into
# before the row runs, and the next run builds that environment again.
#
# ioc_env_built <label> <stamp>; true when the environment <label> carries <stamp> and has
# its python. ioc_env_stamp_write <label> <stamp> writes the stamp, and ioc_env_unstamp
# <label> removes it, doing nothing for an environment whose variable is not set.
ioc_env_built() {
    ioc_env_frozen_entry "$1" && [ -n "$IOC_ENV_ENTRY_DIR" ] || return 1
    [ "$(cat "$IOC_ENV_ENTRY_DIR/.ioc-built-from" 2>/dev/null)" = "$2" ] && [ -x "$IOC_ENV_ENTRY_DIR/bin/python" ]
}
ioc_env_stamp_write() {
    ioc_env_frozen_entry "$1" && [ -n "$IOC_ENV_ENTRY_DIR" ] || return 1
    printf '%s\n' "$2" >"$IOC_ENV_ENTRY_DIR/.ioc-built-from" ||
        ioc_env_refuse "$1" "its stamp $IOC_ENV_ENTRY_DIR/.ioc-built-from could not be written"
}
ioc_env_unstamp() {
    ioc_env_frozen_entry "$1" && [ -n "$IOC_ENV_ENTRY_DIR" ] || return 0
    rm -f "$IOC_ENV_ENTRY_DIR/.ioc-built-from"
}

# ioc_env_frozen <label> <locks directory>; checks the result of every install into the
# environment IOC_ENV_FROZEN names <label>. It lists each package installed there, with
# that environment's own python, and prints one line REFUSED<TAB><what> for each package
# that fails, naming <label>, the package, its version and the lock: a package installed
# from an index must be held at that version by the environment's own lock, under <locks
# directory>; a package installed from a path or a URL must be one the environment's
# pinned clone declares, by name and version, in a pyproject.toml at the clone's commit,
# and its direct_url.json must name a path inside that clone. A lock that is missing or
# carries a requirement with no hash, a dist-info with no name or version, and any error
# of the check itself are refusals too, each naming what it could not read; when the
# check ends non-zero with no refusal of its own, which only its crash leaves, it prints
# one naming that. A line CHECKED<TAB><what> says how many packages it read, and it
# returns non-zero exactly when it printed a refusal. env.sh reads it through
# ioc_env_check_frozen, and check.sh after each row that installs into the environment.
ioc_env_frozen() {
    local python out status
    if ! ioc_env_frozen_entry "$1"; then
        printf 'REFUSED\t%s is no environment env.sh builds\n' "$1"
        return 1
    fi
    if [ -z "$IOC_ENV_ENTRY_DIR" ]; then
        printf 'REFUSED\t%s cannot be checked: %s names no directory, since its variable is not set\n' "$1" "$IOC_ENV_ENTRY_WRITTEN"
        return 1
    fi
    python="$IOC_ENV_ENTRY_DIR/bin/python"
    if [ ! -x "$python" ]; then
        printf 'REFUSED\t%s has no python at %s, so what it holds cannot be listed\n' "$1" "$python"
        return 1
    fi
    if [ -n "$IOC_ENV_ENTRY_CLONE" ] && [ -z "${IOC_CLONES:-}" ]; then
        printf 'REFUSED\t%s cannot be checked: IOC_CLONES, under which its clone %s stands, is not set\n' "$1" "$IOC_ENV_ENTRY_CLONE"
        return 1
    fi
    out="$({
        ioc_env_lock_py
        cat <<'PY'
import json, subprocess, sys, tomllib
label, python, lock, clone = sys.argv[1:5]

LIST = ("import importlib.metadata as m, json; print(json.dumps([[d.metadata['Name'], d.version, "
        "d.read_text('direct_url.json'), str(getattr(d, '_path', '?'))] for d in m.distributions()]))")

def check():
    refused, declared = [], {}
    name_of_lock = os.path.basename(lock)
    found = read_lock(lock)
    if found is None:
        refused.append("%s was checked against %s, which is no committed hashed lock" % (label, lock))
    held = found[0] if found else {}
    root = os.path.realpath(clone) if clone else ""
    if root:
        paths = subprocess.run(["git", "-C", root, "ls-files", "*pyproject.toml"], capture_output=True, text=True, check=True).stdout.split()
        for path in paths:
            text = subprocess.run(["git", "-C", root, "show", "HEAD:" + path], capture_output=True, text=True, check=True).stdout
            try:
                project = tomllib.loads(text).get("project", {})
            except tomllib.TOMLDecodeError:
                continue
            if project.get("name") and project.get("version"):
                declared.setdefault(normalized(project["name"]), set()).add(project["version"])
    listing = subprocess.run([python, "-c", LIST], capture_output=True, text=True)
    if listing.returncode != 0:
        return ["%s could not be listed by its own python: %s" % (label, " ".join(listing.stderr.split())[:200])]
    installed = json.loads(listing.stdout)
    for name, version, direct, where in installed:
        if not name or not version:
            refused.append("%s holds %s, a dist-info with no name or version, which an interrupted install leaves" % (label, where))
            continue
        url = json.loads(direct).get("url", "") if direct else ""
        if not url:
            if held.get(normalized(name)) != version:
                refused.append("%s holds %s==%s, which %s does not hold at that version" % (label, name, version, name_of_lock))
            continue
        path = os.path.realpath(url[len("file://"):]) if url.startswith("file://") else ""
        inside = root and path and (path + "/").startswith(root + "/")
        if not (inside and version in declared.get(normalized(name), ())):
            refused.append("%s holds %s==%s, installed from %s, which is no path inside %s that declares it at that version" % (
                label, name, version, url, ("the pinned clone " + os.path.basename(root)) if root else "a pinned clone"))
    print("CHECKED\t%s: %d packages checked against %s%s, %d refused" % (
        label, len(installed), name_of_lock, (" and the clone " + os.path.basename(root)) if root else "", len(refused)))
    return refused

try:
    refused = check()
except Exception as why:
    refused = ["%s could not be checked: %s: %s" % (label, type(why).__name__, " ".join(str(why).split())[:200])]
for line in refused:
    print("REFUSED\t" + line)
sys.exit(1 if refused else 0)
PY
    } | python3 - "$1" "$python" "$2/$IOC_ENV_ENTRY_LOCK" "${IOC_ENV_ENTRY_CLONE:+$IOC_CLONES/$IOC_ENV_ENTRY_CLONE}")"
    status=$?
    [ -n "$out" ] && printf '%s\n' "$out"
    if [ "$status" -ne 0 ] && ! printf '%s\n' "$out" | grep -q '^REFUSED'; then
        printf 'REFUSED\t%s could not be checked: its check ended with status %s and no verdict\n' "$1" "$status"
    fi
    return "$status"
}

# ioc_env_check_frozen <label>; env.sh's own use of ioc_env_frozen, after it builds the
# environment: each refusal is env.sh's refusal, and it stops the build.
ioc_env_check_frozen() {
    local out kind text status
    out="$(ioc_env_frozen "$1" "$IOC_EVID")"
    status=$?
    while IFS=$'\t' read -r kind text; do
        case "$kind" in
        REFUSED) ioc_env_refuse "$1" "$text" ;;
        CHECKED) printf '%s\n' "$text" >&2 ;;
        esac
    done <<<"$out"
    return "$status"
}

# ioc_env_is_refusal <file>; true when what a dispatch read printed is Dispatch's refusal
# rather than a record. A refusal is printed on stdout, so its text is read as well as
# the status: "not found", a missing block, a missing document, an unreachable server, each
# with or without the refusal's code before it ("NOT_FOUND: not found"), as the CLI of the
# worker image prints it from 2026-11-17.
ioc_env_is_refusal() {
    head -1 "$1" | grep -qE '^(NOT_FOUND: )?(not found$|No block |document "[^"]*" not found|Dispatch at .* is unreachable|dispatch [a-z-]+ was not called)'
}

# ioc_env_record_command <ref>; the dispatch command that reads a record, an artifact by
# doc-read and anything else by read --ref, printing what dispatch prints. env.sh reads a
# record through ioc_env_read_record below; check.sh runs this same command through its
# live_read, which tells a Dispatch that does not answer from one that answers no.
ioc_env_record_command() {
    local ref="$1" rest="${1#dispatch://}"
    case "$ref" in
    *"/artifact/"*) dispatch doc-read --issue "${rest%%/*}" --artifact "${ref##*/artifact/}" ;;
    *) dispatch read --ref "$ref" ;;
    esac
}

# ioc_env_read_record <ref> <file>; env.sh's reader of a Dispatch record. It writes
# through <file>.part and keeps the copy only when dispatch exited 0 and what it printed is
# not a refusal; otherwise it removes the copy and refuses, naming the record, so no later
# run reads an error as the record.
ioc_env_read_record() {
    local ref="$1" file="$2" part="$2.part" status
    ioc_env_record_command "$ref" >"$part" 2>/dev/null
    status=$?
    if [ "$status" -ne 0 ] || [ ! -s "$part" ] || ioc_env_is_refusal "$part"; then
        ioc_env_refuse "the record $ref" "dispatch exited $status and printed '$(head -c 160 "$part" 2>/dev/null | tr '\n' ' ')'; is DISPATCH_TOKEN_FILE readable and DISPATCH_URL reachable?"
        rm -f "$part"
        return 1
    fi
    mv "$part" "$file"
}

# The two records under check, which the rows that count or quote a line of either one
# read. check.sh --replay reads both itself, live through its live_read, before it sources
# env.sh, exports IOC_DESIGN and IOC_COMMENT as its copies and sets IOC_ENV_RECORDS_READ=1
# in its own shell, and env.sh then reads neither. Run on its own, env.sh reads each from
# Dispatch into the cache, and a copy an earlier run kept is read again when it is a
# refusal.
ioc_env_records() {
    local label file ref
    if [ "${IOC_ENV_RECORDS_READ:-0}" = 1 ]; then
        [ -e "${IOC_DESIGN:-}" ] && [ -e "${IOC_COMMENT:-}" ] ||
            ioc_env_refuse "IOC_DESIGN and IOC_COMMENT" "check.sh read the records under check and named no copy of both"
        return
    fi
    for label in d r; do
        case "$label" in
        d) file="$IOC_CACHE/ioc-design.md" ;;
        r) file="$IOC_CACHE/comment-326a.md" ;;
        esac
        ref="$(jq -r --arg l "$label" '.records_under_check[] | select(.label==$l) | .ref' "$IOC_ENV_HERE/sources.json")"
        if [ -s "$file" ] && ! ioc_env_is_refusal "$file"; then
            continue
        fi
        rm -f "$file"
        ioc_env_read_record "$ref" "$file" || return 1
    done
    export IOC_DESIGN="$IOC_CACHE/ioc-design.md"
    export IOC_COMMENT="$IOC_CACHE/comment-326a.md"
}

# The interpreter the similarity table and the scope rows run on, synced to exactly the
# hashed lock whenever it is built, and reused only when its stamp names that lock, so a
# cache an earlier run left cannot carry another set:
# evidence/ioc-venv.in names its five requirements, sentence-transformers, torch and
# rdflib at the versions similarity/table.md records, pyshacl, which the scope rows run
# beside rdflib, and pypdf, which the rows that print the words of their study PDF read it
# with, and evidence/ioc-venv.lock pins all 53 packages with their hashes, torch at
# 2.15.0+cpu, its CPU build.
ioc_env_python_pinned() {
    "$IOC_PY" - "$IOC_EVID/ioc-venv.in" <<'PY' >/dev/null 2>&1
import sys
from importlib.metadata import version
import sentence_transformers, torch, rdflib, pyshacl, pypdf
for line in open(sys.argv[1]):
    line = line.split("#", 1)[0].strip()
    if line:
        name, want = line.split("==")
        if version(name) != want:
            sys.exit(1)
PY
}

# The page uv pip sync finds torch's file on. The lock holds torch 2.15.0+cpu, which PyPI
# does not serve: its torch 2.15.0 for linux x86_64 is the CUDA build, whose import made
# the replay's memory peak, 3.4 GB in one python (evidence/ioc-venv.lock's header). uv pip
# sync reads no source a lock names, so it is given this page with --find-links rather than
# the PyTorch index: the page lists torch's files and no other package's, so no other
# package can come from it, and every package, torch among them, is still checked against
# the lock's hash. Without it the sync finds no torch==2.15.0+cpu and fails.
IOC_ENV_TORCH_LINKS="https://download.pytorch.org/whl/cpu/torch/"

ioc_env_python() {
    export IOC_VENV="$IOC_CACHE/venv"
    export IOC_PY="$IOC_VENV/bin/python"
    local lock="" want
    ioc_env_frozen_entry IOC_VENV && lock="$IOC_EVID/$IOC_ENV_ENTRY_LOCK"
    [ -s "$lock" ] || ioc_env_refuse "IOC_PY" "$lock, the hashed lock of IOC_VENV, is missing" || return 1
    want="$(ioc_env_stamp - "$lock")"
    if ! ioc_env_built IOC_VENV "$want"; then
        ioc_env_unstamp IOC_VENV
        uv venv --clear --python 3.12 "$IOC_VENV" >&2 ||
            ioc_env_refuse "IOC_PY" "uv venv --clear --python 3.12 $IOC_VENV failed" || return 1
        uv pip sync --python "$IOC_PY" --require-hashes --find-links "$IOC_ENV_TORCH_LINKS" "$lock" >&2 ||
            ioc_env_refuse "IOC_PY" "uv pip sync --require-hashes --find-links $IOC_ENV_TORCH_LINKS $lock failed" || return 1
    fi
    ioc_env_python_pinned ||
        ioc_env_refuse "IOC_PY" "$IOC_PY does not import the five requirements of evidence/ioc-venv.in at those versions" || return 1
    ioc_env_check_frozen IOC_VENV || return 1
    ioc_env_stamp_write IOC_VENV "$want"
}

# The weights, pinned by revision and by the sha256 the evidence of the row
# s-model-checksum names (ioc_env_row_fields). The snapshot is reused only when its stamp,
# model.built-from under IOC_CACHE, written once the checksum matched, names this revision
# and checksum; any other is downloaded again, which fetches every file a killed download
# left out.
ioc_env_model() {
    local revision="1110a243fdf4706b3f48f1d95db1a4f5529b4d41" want got file stamp="$IOC_CACHE/model.built-from"
    export IOC_MODEL_DIR="$HF_HOME/hub/models--sentence-transformers--all-MiniLM-L6-v2/snapshots/$revision"
    file="$IOC_MODEL_DIR/model.safetensors"
    want="$(ioc_env_row_field s-model-checksum sha256)"
    [ -n "$want" ] ||
        ioc_env_refuse "IOC_MODEL_DIR" "claims.jsonl holds no row s-model-checksum whose evidence names a sha256" || return 1
    if [ "$(cat "$stamp" 2>/dev/null)" != "$revision $want" ] || [ ! -s "$file" ]; then
        rm -f "$stamp"
        "$IOC_PY" -c "
from huggingface_hub import snapshot_download
print(snapshot_download('sentence-transformers/all-MiniLM-L6-v2', revision='$revision',
      allow_patterns=['*.json', '*.txt', '*.safetensors', '1_Pooling/*', '*.md']))
" >&2 || ioc_env_refuse "IOC_MODEL_DIR" "the snapshot $revision did not download" || return 1
    fi
    [ -s "$file" ] ||
        ioc_env_refuse "IOC_MODEL_DIR" "$file is not there after the download" || return 1
    got="$(sha256sum "$file" | cut -d' ' -f1)"
    [ "$got" = "$want" ] ||
        ioc_env_refuse "IOC_MODEL_DIR" "$file has sha256 $got, and the row s-model-checksum pins $want" || return 1
    printf '%s %s\n' "$revision" "$want" >"$stamp" ||
        ioc_env_refuse "IOC_MODEL_DIR" "its stamp $stamp could not be written"
}

# mas-lab, installed from the clone the way evidence/tm-load.sh installed it: the three
# workspace members, then ctl and the lab core component editable, because a built wheel
# of ctl carries no schemas and its validator imports mas.lab. Its third-party
# dependencies are held to the versions evidence/tm-constraints.txt records, and the
# environment each member is built in to those evidence/build-constraints.txt records.
ioc_env_maslab() {
    export IOC_MASLAB_VENV="$IOC_CACHE/tm-venv"
    export IOC_MASLAB_WHEEL_VENV="$IOC_CACHE/tm-venv-wheel"
    export IOC_MASLAB_PY="$IOC_MASLAB_VENV/bin/python" IOC_MASLAB_CTL="$IOC_MASLAB_VENV/bin/mas-ctl"
    export IOC_MASLAB_WHEEL_CTL="$IOC_MASLAB_WHEEL_VENV/bin/mas-ctl"
    local clone="$IOC_CLONES/mas-lab" constraints="" build="$IOC_EVID/build-constraints.txt"
    ioc_env_frozen_entry IOC_MASLAB_VENV && constraints="$IOC_EVID/$IOC_ENV_ENTRY_LOCK"
    local want rebuilt=0
    [ -s "$constraints" ] ||
        ioc_env_refuse "IOC_MASLAB_VENV" "$constraints, the pinned dependencies of mas-lab, is missing" || return 1
    [ -s "$build" ] ||
        ioc_env_refuse "IOC_MASLAB_VENV" "$build, the pinned build environment, is missing" || return 1
    want="$(ioc_env_stamp "$(git -C "$clone" rev-parse HEAD)" "$constraints" "$build")"
    # A venv that imports mas.lab is reused only when its stamp says it was built from these
    # same files and this same clone commit; any other, one of evidence/tm-load.sh's or one
    # a row installed into since among them, is built again from nothing.
    if ! { ioc_env_built IOC_MASLAB_VENV "$want" && [ -x "$IOC_MASLAB_VENV/bin/mas-ctl" ] &&
        "$IOC_MASLAB_VENV/bin/python" -c 'import mas.lab' >/dev/null 2>&1; }; then
        ioc_env_unstamp IOC_MASLAB_VENV
        uv venv --clear "$IOC_MASLAB_VENV" >&2 ||
            ioc_env_refuse "IOC_MASLAB_VENV" "uv venv --clear $IOC_MASLAB_VENV failed" || return 1
        # The one install written with UV_REQUIRE_HASHES=0, as the header above reader_py in
        # check.sh states it, as a row writes it.
        (
            cd "$clone" || exit 1
            UV_REQUIRE_HASHES=0 VIRTUAL_ENV="$IOC_MASLAB_VENV" uv pip install -c "$constraints" -b "$build" --require-build-hashes ./runtime ./ctl ./library-standard >&2 &&
                UV_REQUIRE_HASHES=0 VIRTUAL_ENV="$IOC_MASLAB_VENV" uv pip install -c "$constraints" -b "$build" --require-build-hashes -e ./ctl -e ./lab/components/core >&2
        ) || ioc_env_refuse "IOC_MASLAB_VENV" "uv pip install from $clone failed" || return 1
        rebuilt=1
    fi
    { [ -x "$IOC_MASLAB_VENV/bin/mas-ctl" ] && "$IOC_MASLAB_VENV/bin/python" -c 'import mas.lab' >/dev/null 2>&1; } ||
        ioc_env_refuse "IOC_MASLAB_VENV" "$IOC_MASLAB_VENV holds no mas-ctl that imports mas.lab" || return 1
    ioc_env_check_frozen IOC_MASLAB_VENV || return 1
    ioc_env_stamp_write IOC_MASLAB_VENV "$want" || return 1
    # The two schema rows do their own installs into the second environment, so it has to
    # exist, and it starts empty: one whose stamp is not that of the first, or that a
    # rebuild of the first left behind, is made again from nothing.
    if [ "$rebuilt" -eq 1 ] || ! ioc_env_built IOC_MASLAB_WHEEL_VENV "$want"; then
        ioc_env_unstamp IOC_MASLAB_WHEEL_VENV
        uv venv --clear "$IOC_MASLAB_WHEEL_VENV" >&2 ||
            ioc_env_refuse "IOC_MASLAB_WHEEL_VENV" "uv venv --clear $IOC_MASLAB_WHEEL_VENV failed" || return 1
    fi
    ioc_env_check_frozen IOC_MASLAB_WHEEL_VENV || return 1
    ioc_env_stamp_write IOC_MASLAB_WHEEL_VENV "$want"
}

# ioc-concord, built once into a wheel under evidence/build-constraints.txt. The tc-run
# rows run it with uv run --with, which takes no build constraint, so they run this one
# build rather than building the source again per row; a wheel needs no build. The wheel
# is reused only when its stamp names this build constraint and this clone commit; the
# stamp goes before the build and is written after it, so a killed build is built again.
ioc_env_concord() {
    local dir="$IOC_CACHE/wheels" build="$IOC_EVID/build-constraints.txt" want
    export IOC_CONCORD_WHEEL="$dir/ioc_concord-0.1.0-py3-none-any.whl"
    [ -s "$build" ] ||
        ioc_env_refuse "IOC_CONCORD_WHEEL" "$build, the pinned build environment, is missing" || return 1
    want="$(ioc_env_stamp "$(git -C "$ACCORD_SRC" rev-parse HEAD)" "$build")"
    if [ ! -s "$IOC_CONCORD_WHEEL" ] || [ "$(cat "$IOC_CONCORD_WHEEL.built-from" 2>/dev/null)" != "$want" ]; then
        rm -f "$IOC_CONCORD_WHEEL" "$IOC_CONCORD_WHEEL.built-from"
        uv build --wheel --python 3.12 -b "$build" --out-dir "$dir" "$ACCORD_SRC/packages/ioc-concord" >&2 ||
            ioc_env_refuse "IOC_CONCORD_WHEEL" "uv build of $ACCORD_SRC/packages/ioc-concord failed" || return 1
        [ -s "$IOC_CONCORD_WHEEL" ] && printf '%s\n' "$want" >"$IOC_CONCORD_WHEEL.built-from"
    fi
    [ -s "$IOC_CONCORD_WHEEL" ] ||
        ioc_env_refuse "IOC_CONCORD_WHEEL" "the build wrote no $IOC_CONCORD_WHEEL, the version 0.1.0 the tc-run rows run"
}

# The shapes graph, assembled the way the scope clone's own verification.py assembles it:
# the five ontology files, the generated mas-shapes.ttl and the committed custom shapes.
# tests/scope.md and evidence/scope-pyshacl.txt record it. Each of the two files is written
# as <file>.part and renamed into place only once it is whole, so a generator or a cat
# that was killed part-way leaves no file the next run reuses.
ioc_env_scope_shapes() {
    export IOC_SCOPE_MAS_SHAPES="$IOC_CACHE/mas-shapes.ttl"
    export IOC_SCOPE_SHAPES="$IOC_CACHE/scope-shapes.ttl"
    local src="$IOC_CLONES/scope/ontology/src/scope_ontology"
    if [ ! -s "$IOC_SCOPE_MAS_SHAPES" ]; then
        (
            cd "$IOC_CLONES/scope/ontology" || exit 1
            "$IOC_PY" scripts/generate_shacl_shapes.py "$IOC_SCOPE_MAS_SHAPES.part" >&2
        ) && [ -s "$IOC_SCOPE_MAS_SHAPES.part" ] && mv "$IOC_SCOPE_MAS_SHAPES.part" "$IOC_SCOPE_MAS_SHAPES" ||
            {
                rm -f "$IOC_SCOPE_MAS_SHAPES.part"
                ioc_env_refuse "IOC_SCOPE_SHAPES" "generate_shacl_shapes.py of the scope clone failed"
                return 1
            }
    fi
    if [ ! -s "$IOC_SCOPE_SHAPES" ]; then
        cat "$src/mas-ontology.ttl" "$src/metrics-ontology.ttl" "$src/semantic-ontology.ttl" \
            "$src/analysis-ontology.ttl" "$src/insight-ontology.ttl" "$IOC_SCOPE_MAS_SHAPES" \
            "$src/mas-shapes-custom.ttl" >"$IOC_SCOPE_SHAPES.part" && mv "$IOC_SCOPE_SHAPES.part" "$IOC_SCOPE_SHAPES" ||
            {
                rm -f "$IOC_SCOPE_SHAPES.part"
                ioc_env_refuse "IOC_SCOPE_SHAPES" "the seven files of the shapes graph did not read"
                return 1
            }
    fi
    [ -s "$IOC_SCOPE_SHAPES" ] || ioc_env_refuse "IOC_SCOPE_SHAPES" "$IOC_SCOPE_SHAPES is empty"
}

# The corpus of tc-corpus-1: the review bodies of the 200 merged pull requests the
# committed number list pins, read with curl from GitHub's REST API, a hundred to a page,
# sending the header file IOC_GITHUB_AUTH_LEGION names, at least as many as the
# corpus_floor the evidence of that row names (ioc_env_row_fields). Each body is one line,
# its keys in the order gh printed them when the corpus was first fetched. A machine whose
# credential helper gives no token for cybermonklabs/legion reads with none, and GitHub's
# sixty requests an hour for one address are fewer than the corpus takes, so the read stops
# at GitHub's refusal, naming it. The fetch takes about two minutes and runs once; the file
# is the corpus the disagreement search and the recorded script read. It prints how many
# requests it made and whether it sent a token, never the token.
ioc_env_reviews() {
    export IOC_TC_WORK="$IOC_CACHE/tc-work"
    export IOC_TC_PY="$IOC_TC_WORK/venv/bin/python"
    export IOC_REVIEWS="$IOC_TC_WORK/reviews.jsonl"
    local want part n page length requests=0 token="with no token"
    want="$(ioc_env_row_field tc-corpus-1 corpus_floor)"
    [ -n "$want" ] ||
        ioc_env_refuse "IOC_REVIEWS" "claims.jsonl holds no row tc-corpus-1 whose evidence names a corpus_floor" || return 1
    mkdir -p "$IOC_TC_WORK"
    if [ ! -s "$IOC_REVIEWS" ] || [ "$(wc -l <"$IOC_REVIEWS")" -lt "$want" ]; then
        [ -n "${IOC_GITHUB_AUTH_LEGION:-}" ] && [ -r "$IOC_GITHUB_AUTH_LEGION" ] ||
            ioc_env_refuse "IOC_REVIEWS" "IOC_GITHUB_AUTH_LEGION names no readable header file, which check.sh --replay and env.sh run on its own each write" || return 1
        [ -s "$IOC_GITHUB_AUTH_LEGION" ] && token="sending the token git credential fill gave"
        part="$IOC_REVIEWS.part"
        : >"$part"
        for n in $(cat "$IOC_EVID/concord-prs.txt"); do
            page=1
            while :; do
                # One read that fails leaves the corpus short of that pull request's
                # reviews, and a search of a partial corpus is no search of the corpus: the
                # build stops and names the pull request and what GitHub answered.
                requests=$((requests + 1))
                if ! curl -sS --fail-with-body -H @"$IOC_GITHUB_AUTH_LEGION" \
                    "https://api.github.com/repos/cybermonklabs/legion/pulls/$n/reviews?per_page=100&page=$page" \
                    >"$part.page" 2>"$part.err" ||
                    ! length="$(jq -r 'if type == "array" then length else error("the answer is no list of reviews") end' "$part.page" 2>>"$part.err")" ||
                    ! jq -S -c --argjson n "$n" '.[] | {pr: $n, id: .id, state: .state, commit: .commit_id, author: .user.login, submitted: .submitted_at, body: .body}' \
                        "$part.page" >>"$part" 2>>"$part.err"; then
                    ioc_env_refuse "IOC_REVIEWS" "curl of page $page of the reviews of cybermonklabs/legion pull request $n, $token, failed: $(cat "$part.err" "$part.page" | head -c 240 | tr '\n' ' ')"
                    rm -f "$part" "$part.page" "$part.err"
                    return 1
                fi
                [ "$length" -lt 100 ] && break
                page=$((page + 1))
            done
        done
        rm -f "$part.page" "$part.err"
        if [ "$(wc -l <"$part")" -lt "$want" ]; then
            rm -f "$part"
            ioc_env_refuse "IOC_REVIEWS" "curl read fewer than $want review bodies of cybermonklabs/legion in $requests requests, $token"
            return 1
        fi
        mv "$part" "$IOC_REVIEWS"
        printf 'env.sh read the review corpus: %s review bodies of %s pull requests in %s requests to GitHub'"'"'s REST API, %s\n' \
            "$(wc -l <"$IOC_REVIEWS")" "$(wc -l <"$IOC_EVID/concord-prs.txt")" "$requests" "$token" >&2
    fi
    # The recorded script builds its own install into ./venv of this directory, so env.sh
    # makes it empty, and makes it again from nothing when its stamp is not that of this
    # lock, as it is after a run whose row installed into it.
    local stamp
    stamp="$(ioc_env_stamp - "$IOC_EVID/ioc-venv.lock")"
    if ! ioc_env_built IOC_TC_WORK "$stamp"; then
        ioc_env_unstamp IOC_TC_WORK
        uv venv --clear --python 3.12 "$IOC_TC_WORK/venv" >&2 ||
            ioc_env_refuse "IOC_TC_WORK" "uv venv --clear $IOC_TC_WORK/venv failed" || return 1
    fi
    ioc_env_check_frozen IOC_TC_WORK || return 1
    ioc_env_stamp_write IOC_TC_WORK "$stamp"
}

# Our code as it stands at the pinned commit, written once with git archive and kept only
# when the whole archive was written. A row that builds or tests our code runs here, so a
# forward merge of the base branch into the working tree moves none of its readings.
ioc_env_our_tree() {
    local commit dir
    commit="$(jq -r '.our_repository.commit' "$IOC_ENV_HERE/sources.json")"
    dir="$IOC_CACHE/our-$commit"
    export IOC_OUR_TREE="$dir"
    [ -f "$dir/.complete" ] && return 0
    rm -rf "$dir" "$dir.part" && mkdir -p "$dir.part" ||
        ioc_env_refuse "IOC_OUR_TREE" "mkdir $dir.part failed" || return 1
    if ! git -C "$IOC_ENV_ROOT" archive -o "$dir.part.tar" "$commit" go.work go.work.sum \
        packages/daemon packages/envoy || ! tar -x -f "$dir.part.tar" -C "$dir.part"; then
        rm -rf "$dir.part" "$dir.part.tar"
        ioc_env_refuse "IOC_OUR_TREE" "git archive of $commit did not write" || return 1
    fi
    rm -f "$dir.part.tar"
    touch "$dir.part/.complete" && mv "$dir.part" "$dir" ||
        ioc_env_refuse "IOC_OUR_TREE" "$dir.part did not move into place"
}

# The working file the committed records name by path rather than take as an argument:
# the go overlay, whose two sides are absolute paths of the machine it runs on, built from
# the committed copy for the pinned tree.
ioc_env_working_files() {
    export IOC_TM_OVERLAY="$IOC_CACHE/tm-overlay.json"
    jq --arg root "$IOC_OUR_TREE" --arg evid "$IOC_EVID" \
        '{Replace: (.Replace | with_entries(
            .key |= ($root + "/packages/" + (. | sub("^.*/packages/"; ""))) |
            .value |= ($evid + "/" + (. | split("/") | last))))}' \
        "$IOC_EVID/tm-overlay.json" >"$IOC_TM_OVERLAY" ||
        ioc_env_refuse "IOC_TM_OVERLAY" "evidence/tm-overlay.json did not read"
}

ioc_env_main() {
    IOC_ENV_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    IOC_ENV_ROOT="${IOC_ROOT:-$(cd "$IOC_ENV_HERE/../../.." && pwd)}"
    ioc_env_caches
    export IOC_CLONES="${IOC_CLONES:-${TMPDIR:-/tmp}/ioc-clones}"
    export IOC_EVID="$IOC_ENV_HERE/evidence"
    export ACCORD_SRC="$IOC_CLONES/cognition-protocols"

    ioc_env_need uv git jq curl python3 dispatch || return 1
    mkdir -p "$IOC_CACHE" || ioc_env_refuse "the cache" "mkdir $IOC_CACHE failed" || return 1
    [ -d "$IOC_EVID" ] || ioc_env_refuse "IOC_EVID" "$IOC_EVID is not a directory" || return 1
    # check.sh --replay writes the GitHub header files under its run's scratch before it
    # sources env.sh; run on its own, env.sh writes them under IOC_ENV_SCRATCH, its own.
    [ -z "${IOC_ENV_SCRATCH:-}" ] || ioc_env_github_auth || return 1

    local name
    for name in mas-lab scope cognition-protocols; do
        ioc_env_clone "$name" || return 1
    done
    ioc_env_clone_private agent-infra || return 1

    ioc_env_enforce || return 1
    ioc_env_records || return 1
    ioc_env_python || return 1
    ioc_env_model || return 1
    ioc_env_maslab || return 1
    ioc_env_concord || return 1
    ioc_env_scope_shapes || return 1
    ioc_env_reviews || return 1
    ioc_env_our_tree || return 1
    ioc_env_working_files || return 1
}

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
    # Run on its own, env.sh writes the GitHub header files (ioc_env_github_auth) under a
    # scratch of its own, made before the first of them and removed when env.sh exits,
    # however it exits, so no token outlives the run that read it.
    IOC_ENV_SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/ioc-env.XXXXXX")" ||
        { ioc_env_refuse "the GitHub header files" "no scratch directory could be made under ${TMPDIR:-/tmp}"; exit 1; }
    trap 'rm -rf "$IOC_ENV_SCRATCH"' EXIT
    ioc_env_main || exit 1
    for ioc_env_name in IOC_CACHE IOC_CLONES IOC_EVID IOC_DESIGN IOC_COMMENT IOC_VENV IOC_PY \
        IOC_MODEL_DIR IOC_MASLAB_VENV IOC_MASLAB_WHEEL_VENV IOC_MASLAB_PY IOC_MASLAB_CTL IOC_MASLAB_WHEEL_CTL IOC_TC_PY IOC_SCOPE_SHAPES IOC_SCOPE_MAS_SHAPES \
        IOC_TC_WORK IOC_CONCORD_WHEEL IOC_REVIEWS IOC_OUR_TREE IOC_TM_OVERLAY ACCORD_SRC \
        $(ioc_env_cache_table | cut -d'|' -f1); do
        printf '%s=%s\n' "$ioc_env_name" "${!ioc_env_name}"
    done
elif [ "${IOC_ENV_FUNCTIONS_ONLY:-0}" = 1 ]; then
    # check.sh reads env.sh this way, for the functions its env_functions names: the
    # functions, and nothing prepared.
    return 0
else
    ioc_env_main || return 1
fi
