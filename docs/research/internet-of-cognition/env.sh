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
# It is idempotent. A second run reuses what the first built: it re-downloads nothing,
# re-installs nothing and re-reads Dispatch only when the copy is missing. Delete the
# cache directory to build it again from nothing.
#
# Everything it writes is under one directory, IOC_CACHE, which defaults to
# $TMPDIR/infra3134. It writes nothing anywhere else, and it writes nothing into this
# repository. It clones under a second directory, IOC_CLONES, which defaults to
# $TMPDIR/ioc-clones. No recorded command and no script it runs names either default root:
# each reads them through these variables, so a reader may move either one.
#
# What it exports, one line per input:
#
#   IOC_CACHE       the cache root, everything below is under it
#   IOC_CLONES      the clone root, the three clones at the commits sources.json pins
#   IOC_EVID        the absolute path of the committed evidence/ directory
#   IOC_DESIGN      the design under check, dispatch://INFRA-3134/artifact/ioc-design-2026-11-12-md
#   IOC_COMMENT     the reconciliation under check, dispatch://INFRA-3134/comment/326a044a-…
#   IOC_VENV        the virtual environment uv builds for the similarity and scope rows
#   IOC_PY          that environment's python: sentence-transformers, torch and rdflib
#   IOC_MODEL_DIR   the pinned all-MiniLM-L6-v2 snapshot, its model.safetensors checksummed
#   IOC_MASLAB_VENV       mas-lab runtime, ctl and library-standard installed from the clone
#   IOC_MASLAB_WHEEL_VENV the second, empty environment the two schema rows install into
#   IOC_SCOPE_SHAPES      the shapes graph the ts- rows validate against
#   IOC_SCOPE_MAS_SHAPES  where the shapes generator of the scope clone writes
#   IOC_TC_WORK     the working directory evidence/concord-build-run.sh runs in
#   IOC_REVIEWS     the review corpus of tc-corpus-1, fetched with gh. A review a reader
#                   may not see is not in it: a PENDING review is visible to its author
#                   alone, so the row this count serves is a lower bound.
#   IOC_TM_OVERLAY  the go overlay of tm-our-count, written for this workspace
#   ACCORD_SRC      the cognition-protocols clone, which similarity.py imports from
#   UV_CACHE_DIR PIP_CACHE_DIR HF_HOME XDG_CACHE_HOME   every cache, under IOC_CACHE
#
# It needs uv, git, jq, gh, python3 and dispatch, and it needs the three clones, which it
# clones at the commits sources.json pins when they are missing. It refuses, with a line
# naming what it could not prepare and a non-zero status, rather than leaving a variable
# pointing at something that is not there.

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
# git objects.
ioc_env_clone() {
    local name="$1" url commit dir
    url="$(jq -r --arg n "$name" '.repositories[] | select(.name==$n) | .url' "$IOC_ENV_HERE/sources.json")"
    commit="$(jq -r --arg n "$name" '.repositories[] | select(.name==$n) | .commit' "$IOC_ENV_HERE/sources.json")"
    [ -n "$url" ] && [ -n "$commit" ] || ioc_env_refuse "the clone $name" "sources.json pins no such repository" || return 1
    dir="$IOC_CLONES/$name"
    # The clone is the one the triage read: one commit deep and with no tags. The triage
    # recorded that shape as a fact of its own reading, so a clone of the whole history
    # would answer those rows differently, and the maturity of a repository is read from
    # its own REST answers, which are committed under published/github-api/.
    if [ ! -d "$dir/.git" ]; then
        mkdir -p "$dir"
        git -C "$dir" init --quiet >&2 ||
            ioc_env_refuse "the clone $name" "git init under $IOC_CLONES failed" || return 1
        git -C "$dir" remote add origin "$url" 2>/dev/null || true
    fi
    git -C "$dir" cat-file -e "$commit^{commit}" 2>/dev/null ||
        git -C "$dir" fetch --quiet --depth 1 --no-tags origin "$commit" >&2 ||
        ioc_env_refuse "the clone $name" "git fetch of $commit from $url failed" || return 1
    if [ "$(git -C "$dir" rev-parse HEAD 2>/dev/null)" != "$commit" ]; then
        git -C "$dir" checkout --quiet --detach "$commit" >&2 ||
            ioc_env_refuse "the clone $name" "it holds no commit $commit" || return 1
    fi
}

ioc_env_caches() {
    export UV_CACHE_DIR="$IOC_CACHE/cache/uv"
    export PIP_CACHE_DIR="$IOC_CACHE/cache/pip"
    export HF_HOME="$IOC_CACHE/cache/hf"
    export XDG_CACHE_HOME="$IOC_CACHE/cache/xdg"
    mkdir -p "$UV_CACHE_DIR" "$PIP_CACHE_DIR" "$HF_HOME" "$XDG_CACHE_HOME" ||
        ioc_env_refuse "the caches" "mkdir under $IOC_CACHE failed"
}

# ioc_env_is_refusal <file>; true when what a dispatch read printed is Dispatch's refusal
# rather than a record. A refusal is printed on stdout, so its text is read as well as
# the status: "not found", a missing block, a missing document, an unreachable server.
ioc_env_is_refusal() {
    head -1 "$1" | grep -qE '^(not found$|No block |document "[^"]*" not found|Dispatch at .* is unreachable|dispatch [a-z-]+ was not called)'
}

# ioc_env_read_record <ref> <file>; the one reader of a Dispatch record, which check.sh
# reads too. It writes through <file>.part and keeps the copy only when dispatch exited 0
# and what it printed is not a refusal; otherwise it removes the copy and refuses, naming
# the record, so no later run reads an error as the record.
ioc_env_read_record() {
    local ref="$1" file="$2" part="$2.part" rest status
    rest="${ref#dispatch://}"
    case "$ref" in
    *"/artifact/"*) dispatch doc-read --issue "${rest%%/*}" --artifact "${ref##*/artifact/}" >"$part" 2>/dev/null ;;
    *) dispatch read --ref "$ref" >"$part" 2>/dev/null ;;
    esac
    status=$?
    if [ "$status" -ne 0 ] || [ ! -s "$part" ] || ioc_env_is_refusal "$part"; then
        ioc_env_refuse "the record $ref" "dispatch exited $status and printed '$(head -c 160 "$part" 2>/dev/null | tr '\n' ' ')'; is DISPATCH_TOKEN_FILE readable and DISPATCH_URL reachable?"
        rm -f "$part"
        return 1
    fi
    mv "$part" "$file"
}

# The two records under check, read live from Dispatch and written into the cache, which
# is what the rows that count or quote a line of either one read. A copy an earlier run
# kept is read again when it is a refusal.
ioc_env_records() {
    local label file ref
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

# The interpreter the similarity table and the scope rows run on. similarity/table.md
# records how it was built; this builds the same environment, at the versions the recorded
# runs used, and adds rdflib, which the shapes generator and the turtle rows need.
IOC_ENV_PY_PINS="sentence-transformers==6.3.0 torch==2.15.0 rdflib==7.6.0"

ioc_env_python_pinned() {
    "$IOC_PY" -c '
import sys
from importlib.metadata import version
import sentence_transformers, torch, rdflib
for pin in sys.argv[1:]:
    name, want = pin.split("==")
    if version(name) != want:
        sys.exit(1)
' $IOC_ENV_PY_PINS >/dev/null 2>&1
}

ioc_env_python() {
    export IOC_VENV="$IOC_CACHE/venv"
    export IOC_PY="$IOC_VENV/bin/python"
    if ! ioc_env_python_pinned; then
        uv venv --allow-existing --python 3.12 "$IOC_VENV" >&2 ||
            ioc_env_refuse "IOC_PY" "uv venv --python 3.12 $IOC_VENV failed" || return 1
        # shellcheck disable=SC2086
        uv pip install --python "$IOC_PY" $IOC_ENV_PY_PINS >&2 ||
            ioc_env_refuse "IOC_PY" "uv pip install $IOC_ENV_PY_PINS failed" || return 1
    fi
    ioc_env_python_pinned ||
        ioc_env_refuse "IOC_PY" "$IOC_PY does not import $IOC_ENV_PY_PINS at those versions"
}

# The weights, pinned by revision and by the checksum the row s-model-checksum carries.
ioc_env_model() {
    local revision="1110a243fdf4706b3f48f1d95db1a4f5529b4d41" want got file
    export IOC_MODEL_DIR="$HF_HOME/hub/models--sentence-transformers--all-MiniLM-L6-v2/snapshots/$revision"
    file="$IOC_MODEL_DIR/model.safetensors"
    want="$(jq -r 'select(.id=="s-model-checksum") | .expect.contains' "$IOC_ENV_HERE/claims.jsonl" | head -1)"
    [ -n "$want" ] ||
        ioc_env_refuse "IOC_MODEL_DIR" "claims.jsonl carries no checksum in the row s-model-checksum" || return 1
    if [ ! -s "$file" ]; then
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
        ioc_env_refuse "IOC_MODEL_DIR" "$file has sha256 $got, and the row s-model-checksum pins $want"
}

# mas-lab, installed from the clone the way evidence/tm-load.sh installed it: the three
# workspace members, then ctl and the lab core component editable, because a built wheel
# of ctl carries no schemas and its validator imports mas.lab. Its third-party
# dependencies are held to the versions evidence/tm-constraints.txt records.
ioc_env_maslab() {
    export IOC_MASLAB_VENV="$IOC_CACHE/tm-venv"
    export IOC_MASLAB_WHEEL_VENV="$IOC_CACHE/tm-venv-wheel"
    local clone="$IOC_CLONES/mas-lab" constraints="$IOC_EVID/tm-constraints.txt"
    [ -s "$constraints" ] ||
        ioc_env_refuse "IOC_MASLAB_VENV" "$constraints, the pinned dependencies of mas-lab, is missing" || return 1
    if ! { [ -x "$IOC_MASLAB_VENV/bin/mas-ctl" ] && "$IOC_MASLAB_VENV/bin/python" -c 'import mas.lab' >/dev/null 2>&1; }; then
        uv venv --allow-existing "$IOC_MASLAB_VENV" >&2 ||
            ioc_env_refuse "IOC_MASLAB_VENV" "uv venv $IOC_MASLAB_VENV failed" || return 1
        (
            cd "$clone" || exit 1
            VIRTUAL_ENV="$IOC_MASLAB_VENV" uv pip install -c "$constraints" ./runtime ./ctl ./library-standard >&2 &&
                VIRTUAL_ENV="$IOC_MASLAB_VENV" uv pip install -c "$constraints" -e ./ctl -e ./lab/components/core >&2
        ) || ioc_env_refuse "IOC_MASLAB_VENV" "uv pip install from $clone failed" || return 1
    fi
    { [ -x "$IOC_MASLAB_VENV/bin/mas-ctl" ] && "$IOC_MASLAB_VENV/bin/python" -c 'import mas.lab' >/dev/null 2>&1; } ||
        ioc_env_refuse "IOC_MASLAB_VENV" "$IOC_MASLAB_VENV holds no mas-ctl that imports mas.lab" || return 1
    # The two schema rows do their own installs into this one; it has to exist first.
    if [ ! -x "$IOC_MASLAB_WHEEL_VENV/bin/python" ]; then
        uv venv --allow-existing "$IOC_MASLAB_WHEEL_VENV" >&2 ||
            ioc_env_refuse "IOC_MASLAB_WHEEL_VENV" "uv venv $IOC_MASLAB_WHEEL_VENV failed" || return 1
    fi
}

# The shapes graph, assembled the way the scope clone's own verification.py assembles it:
# the five ontology files, the generated mas-shapes.ttl and the committed custom shapes.
# tests/scope.md and evidence/scope-pyshacl.txt record it.
ioc_env_scope_shapes() {
    export IOC_SCOPE_MAS_SHAPES="$IOC_CACHE/mas-shapes.ttl"
    export IOC_SCOPE_SHAPES="$IOC_CACHE/scope-shapes.ttl"
    local src="$IOC_CLONES/scope/ontology/src/scope_ontology"
    if [ ! -s "$IOC_SCOPE_MAS_SHAPES" ]; then
        (
            cd "$IOC_CLONES/scope/ontology" || exit 1
            "$IOC_PY" scripts/generate_shacl_shapes.py "$IOC_SCOPE_MAS_SHAPES" >&2
        ) || ioc_env_refuse "IOC_SCOPE_SHAPES" "generate_shacl_shapes.py of the scope clone failed" || return 1
    fi
    if [ ! -s "$IOC_SCOPE_SHAPES" ]; then
        cat "$src/mas-ontology.ttl" "$src/metrics-ontology.ttl" "$src/semantic-ontology.ttl" \
            "$src/analysis-ontology.ttl" "$src/insight-ontology.ttl" "$IOC_SCOPE_MAS_SHAPES" \
            "$src/mas-shapes-custom.ttl" >"$IOC_SCOPE_SHAPES" ||
            {
                rm -f "$IOC_SCOPE_SHAPES"
                ioc_env_refuse "IOC_SCOPE_SHAPES" "the seven files of the shapes graph did not read"
                return 1
            }
    fi
    [ -s "$IOC_SCOPE_SHAPES" ] || ioc_env_refuse "IOC_SCOPE_SHAPES" "$IOC_SCOPE_SHAPES is empty"
}

# The corpus of tc-corpus-1: the review bodies of the 200 merged pull requests the
# committed number list pins, read with gh. The fetch takes about four minutes and runs
# once; the file is the corpus the disagreement search and the recorded script read.
ioc_env_reviews() {
    export IOC_TC_WORK="$IOC_CACHE/tc-work"
    export IOC_REVIEWS="$IOC_TC_WORK/reviews.jsonl"
    local want part n
    want="$(jq -r 'select(.id=="tc-corpus-1") | .expect.at_least' "$IOC_ENV_HERE/claims.jsonl" | head -1)"
    [ -n "$want" ] ||
        ioc_env_refuse "IOC_REVIEWS" "claims.jsonl carries no count in the row tc-corpus-1" || return 1
    mkdir -p "$IOC_TC_WORK"
    if [ ! -s "$IOC_REVIEWS" ] || [ "$(wc -l <"$IOC_REVIEWS")" -lt "$want" ]; then
        part="$IOC_REVIEWS.part"
        : >"$part"
        for n in $(cat "$IOC_EVID/concord-prs.txt"); do
            # One read that fails leaves the corpus short of that pull request's reviews,
            # and a search of a partial corpus is no search of the corpus: the build stops
            # and names the pull request.
            gh api --paginate "repos/cybermonklabs/legion/pulls/$n/reviews" \
                --jq ".[] | {pr: $n, id: .id, state: .state, commit: .commit_id, author: .user.login, submitted: .submitted_at, body: .body}" \
                2>"$part.err" >>"$part" || {
                ioc_env_refuse "IOC_REVIEWS" "gh api of the reviews of cybermonklabs/legion pull request $n failed: $(head -c 200 "$part.err" | tr '\n' ' ')"
                rm -f "$part" "$part.err"
                return 1
            }
        done
        rm -f "$part.err"
        if [ "$(wc -l <"$part")" -lt "$want" ]; then
            rm -f "$part"
            ioc_env_refuse "IOC_REVIEWS" "gh read fewer than $want review bodies of cybermonklabs/legion"
            return 1
        fi
        mv "$part" "$IOC_REVIEWS"
    fi
    # The recorded script builds its own install into ./venv of this directory.
    if [ ! -x "$IOC_TC_WORK/venv/bin/python" ]; then
        uv venv --python 3.12 "$IOC_TC_WORK/venv" >&2 ||
            ioc_env_refuse "IOC_TC_WORK" "uv venv $IOC_TC_WORK/venv failed" || return 1
    fi
}

# The working file the committed records name by path rather than take as an argument:
# the go overlay, whose two sides are absolute paths of the machine it runs on, built from
# the committed copy.
ioc_env_working_files() {
    export IOC_TM_OVERLAY="$IOC_CACHE/tm-overlay.json"
    jq --arg root "$IOC_ENV_ROOT" --arg evid "$IOC_EVID" \
        '{Replace: (.Replace | with_entries(
            .key |= ($root + "/packages/" + (. | sub("^.*/packages/"; ""))) |
            .value |= ($evid + "/" + (. | split("/") | last))))}' \
        "$IOC_EVID/tm-overlay.json" >"$IOC_TM_OVERLAY" ||
        ioc_env_refuse "IOC_TM_OVERLAY" "evidence/tm-overlay.json did not read"
}

ioc_env_main() {
    IOC_ENV_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    IOC_ENV_ROOT="${IOC_ROOT:-$(cd "$IOC_ENV_HERE/../../.." && pwd)}"
    export IOC_CACHE="${IOC_CACHE:-${TMPDIR:-/tmp}/infra3134}"
    export IOC_CLONES="${IOC_CLONES:-${TMPDIR:-/tmp}/ioc-clones}"
    export IOC_EVID="$IOC_ENV_HERE/evidence"
    export ACCORD_SRC="$IOC_CLONES/cognition-protocols"

    ioc_env_need uv git jq gh python3 dispatch || return 1
    mkdir -p "$IOC_CACHE" || ioc_env_refuse "the cache" "mkdir $IOC_CACHE failed" || return 1
    [ -d "$IOC_EVID" ] || ioc_env_refuse "IOC_EVID" "$IOC_EVID is not a directory" || return 1

    local name
    for name in mas-lab scope cognition-protocols; do
        ioc_env_clone "$name" || return 1
    done

    ioc_env_caches || return 1
    ioc_env_records || return 1
    ioc_env_python || return 1
    ioc_env_model || return 1
    ioc_env_maslab || return 1
    ioc_env_scope_shapes || return 1
    ioc_env_reviews || return 1
    ioc_env_working_files || return 1
}

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
    ioc_env_main || exit 1
    for ioc_env_name in IOC_CACHE IOC_CLONES IOC_EVID IOC_DESIGN IOC_COMMENT IOC_VENV IOC_PY \
        IOC_MODEL_DIR IOC_MASLAB_VENV IOC_MASLAB_WHEEL_VENV IOC_SCOPE_SHAPES IOC_SCOPE_MAS_SHAPES \
        IOC_TC_WORK IOC_REVIEWS IOC_TM_OVERLAY ACCORD_SRC UV_CACHE_DIR PIP_CACHE_DIR HF_HOME \
        XDG_CACHE_HOME; do
        printf '%s=%s\n' "$ioc_env_name" "${!ioc_env_name}"
    done
elif [ "${IOC_ENV_FUNCTIONS_ONLY:-0}" = 1 ]; then
    # check.sh reads the record reader this way: the functions, and nothing prepared.
    return 0
else
    ioc_env_main || return 1
fi
