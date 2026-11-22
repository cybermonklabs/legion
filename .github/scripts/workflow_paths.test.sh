#!/usr/bin/env bash
# Tests for `workflow_paths.py`, the path model the three path checks import: a path glob as a
# regex (picomatch's `**`, `*`, `?` and `[...]`, with `{` and `}` as literal characters), a GitHub
# `paths` list where the last match wins and `!` negates, a dorny/paths-filter filter under its
# predicate quantifier, a workflow's events, the patterns the model refuses to read rather than
# guess, the loader that refuses a repeated mapping key as js-yaml does (UniqueKeyLoader), the
# reader that reads dorny/paths-filter's filters with the action's own js-yaml under Node
# (load_paths_filter), and the walk of the repository's files.
#
# Only what the model does. What each checker makes of it — its verdicts, its reading of a
# workflow — stays in that checker's own suite: check-ci-filters.test.sh,
# check-image-trigger-paths.test.sh and check-ci-images.test.sh, each of which symlinks this same
# module rather than reimplementing it.
#
# The module is symlinked into a temporary directory and imported the way the checks import it
# (PYTHONPATH, no bytecode), so the code under test is the file CI runs. It needs a python3 with
# PyYAML, which the events cases parse their workflow with, and node with the repository's
# node_modules installed (bun install --frozen-lockfile), which load_paths_filter reads with.
#
# Run from anywhere: .github/scripts/workflow_paths.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

ln -s "$script_dir/workflow_paths.py" "$work/workflow_paths.py"

# py <statements> [argument ...]: runs the statements with the model as `wp` and the arguments in
# sys.argv[1:], and echoes what they print.
py() {
  local statements=$1
  shift
  PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$work" python3 -c "import sys
import workflow_paths as wp
$statements" "$@"
}

# matches <pattern> <path>: true/false for check — whether glob_regex(<pattern>) matches the whole
# path, which is how Filter and PathsFilter ask it.
matches() {
  py 'print(str(wp.glob_regex(sys.argv[1]).fullmatch(sys.argv[2]) is not None).lower())' "$1" "$2"
}

# covers <path> <pattern>...: true/false — whether a GitHub `paths` list of those patterns covers
# the path. No pattern is the empty list, not the absent filter.
covers() {
  local path=$1
  shift
  py 'print(str(wp.Filter(sys.argv[2:]).covers(sys.argv[1])).lower())' "$path" "$@"
}

# ignore_covers <path> <pattern>...: the same for a `paths-ignore` list, where a path no pattern
# names is the covered one.
ignore_covers() {
  local path=$1
  shift
  py 'print(str(wp.Filter(sys.argv[2:], ignore=True).covers(sys.argv[1])).lower())' "$path" "$@"
}

# filter_covers <quantifier> <path> <pattern>...: true/false — whether a dorny/paths-filter filter
# of those patterns, under that predicate-quantifier, matches the path.
filter_covers() {
  local quantifier=$1 path=$2
  shift 2
  py 'print(str(wp.PathsFilter(sys.argv[3:], sys.argv[1]).covers(sys.argv[2])).lower())' \
    "$quantifier" "$path" "$@"
}

# refused_quantifier <name>: what PathsFilter raises for a quantifier it does not model.
refused_quantifier() {
  py 'try:
    wp.PathsFilter(["mod/**"], sys.argv[1])
except ValueError as refusal:
    print(refusal)
else:
    print("no refusal")' "$1"
}

# triggers <workflow>: the event names events() reads from that YAML document, space separated.
triggers() {
  py 'import yaml
print(" ".join(str(name) for name in wp.events(yaml.safe_load(sys.argv[1]))))' "$1"
}

# unread <pattern>...: the patterns unread_patterns refuses, space separated.
unread() { py 'print(" ".join(wp.unread_patterns(sys.argv[1:])))' "$@"; }

# unread_filter <pattern>...: the patterns unread_filter_patterns refuses, space separated.
unread_filter() { py 'print(" ".join(wp.unread_filter_patterns(sys.argv[1:])))' "$@"; }

# walked: every file walk() finds under the working directory, sorted, space separated.
walked() { py 'print(" ".join(sorted(wp.walk())))'; }

# unique_load <document>: the document as UniqueKeyLoader reads it, as sorted JSON, or `refused:`
# and the refusal's text on one line.
unique_load() {
  py 'import json, yaml
try:
    print(json.dumps(yaml.load(sys.argv[1], Loader=wp.UniqueKeyLoader), sort_keys=True))
except yaml.YAMLError as error:
    print("refused:", " ".join(str(error).split()))' "$1"
}

# filters_load <document>: the document as load_paths_filter reads it, as sorted JSON, or
# `refused:` and the refusal's text on one line.
filters_load() {
  py 'import json, yaml
try:
    print(json.dumps(wp.load_paths_filter(sys.argv[1]), sort_keys=True))
except yaml.YAMLError as error:
    print("refused:", " ".join(str(error).split()))' "$1"
}

# js_keys <document>: the keys of the document's top mapping as the action's js-yaml reads them
# (js_yaml_reading), space separated, or its refusal.
js_keys() {
  py 'answer = wp.js_yaml_reading(sys.argv[1])
print(answer["error"] if "error" in answer else " ".join(key for key, _ in answer["doc"][1]))' "$1"
}

echo "case: ** spans directories, * and ? stay inside one"
check "**/ matches no directory at all" "$(matches '**/Dockerfile' 'Dockerfile')"
check "**/ matches one" "$(matches '**/Dockerfile' 'docker/Dockerfile')"
check "**/ matches several" "$(matches '**/Dockerfile' 'packages/app/docker/Dockerfile')"
check "** at the end of a pattern spans directories" \
  "$(matches 'packages/app/**' 'packages/app/src/deep/nested.ts')"
check "a bare ** matches every path" "$(matches '**' 'packages/app/src/index.ts')"
check "* matches a whole path segment" "$(matches 'packages/*' 'packages/app')"
check "and part of one" "$(matches 'docker/*.yaml' 'docker/compose.yaml')"
check "but stays inside it" "$(is "$(matches 'packages/*' 'packages/app/src')" false)"
check "even with the rest of the pattern behind it" \
  "$(is "$(matches 'docker/*.yaml' 'docker/base/compose.yaml')" false)"
check "? is one character inside a segment" "$(matches 'package.jso?' 'package.json')"
check "not zero" "$(is "$(matches 'package.jso?' 'package.jso')" false)"
check "and not a /" "$(is "$(matches 'docker?Dockerfile' 'docker/Dockerfile')" false)"
check "a pattern matches a path whole, not its head" \
  "$(is "$(matches 'packages/app' 'packages/app/src/index.ts')" false)"

echo "case: a [...] class is the regex's, and an unclosed [ is a literal"
check "a class matches one character of itself" "$(matches 'v[0-9].yaml' 'v2.yaml')"
check "and only one" "$(is "$(matches 'v[0-9].yaml' 'v12.yaml')" false)"
check "an unclosed [ is the character itself" "$(matches 'docker/a[b' 'docker/a[b')"

echo "case: a brace is a literal character, not an expansion"
check "{a,b} matches the text it is" "$(matches '{docker/**,.dockerignore}' '{docker/**,.dockerignore}')"
check "and no branch of it" "$(is "$(matches '{docker,.dockerignore}' 'docker')" false)"
check "a closing brace alone is a literal too" "$(matches 'docker/}' 'docker/}')"

echo "case: a GitHub paths list, where the last match wins and ! negates"
check "a path a pattern matches is covered" "$(covers 'packages/app/src/index.ts' 'packages/app/**')"
check "a path no pattern matches is not" "$(is "$(covers 'docs/readme.md' 'packages/app/**')" false)"
check "a later ! pattern takes the match back" \
  "$(is "$(covers 'packages/app/src/index.ts' 'packages/app/**' '!packages/app/src/**')" false)"
check "an earlier one does not: the last match wins" \
  "$(covers 'packages/app/src/index.ts' '!packages/app/src/**' 'packages/app/**')"
check "a ! pattern the path does not match leaves it covered" \
  "$(covers 'packages/app/src/index.ts' 'packages/app/**' '!docs/**')"
check "an empty list covers nothing" "$(is "$(covers 'docs/readme.md')" false)"

echo "case: an event with no path filter covers everything"
check "no list at all is everything" "$(py 'print(str(wp.Filter(None).everything).lower())')"
check "and it covers a path no pattern names" \
  "$(py 'print(str(wp.Filter(None).covers(sys.argv[1])).lower())' 'docs/readme.md')"
check "an empty list is not everything" "$(is "$(py 'print(str(wp.Filter([]).everything).lower())')" false)"

echo "case: a paths-ignore list covers what it does not name"
check "a path a pattern names is left out" "$(is "$(ignore_covers 'docs/readme.md' 'docs/**')" false)"
check "every other path is covered" "$(ignore_covers 'packages/app/src/index.ts' 'docs/**')"
check "a ! pattern in it takes a path back in" \
  "$(ignore_covers 'docs/api/readme.md' 'docs/**' '!docs/api/**')"

echo "case: a dorny/paths-filter filter under the default quantifier, some"
check "a pattern that matches the path matches" "$(filter_covers some 'mod/lib/lib.go' 'mod/**')"
check "a path no pattern matches does not" "$(is "$(filter_covers some 'docs/readme.md' 'mod/**')" false)"
check "a ! pattern matches every file outside it" \
  "$(filter_covers some 'docs/readme.md' '!mod/other/**')"
check "and no file inside it" "$(is "$(filter_covers some 'mod/other/other.go' '!mod/other/**')" false)"
check "so beside an include it excludes nothing" \
  "$(filter_covers some 'mod/other/other.go' 'mod/**' '!mod/other/**')"

echo "case: the same filter under some-with-excludes, where a ! pattern excludes"
check "an included path matches" \
  "$(filter_covers some-with-excludes 'mod/lib/lib.go' 'mod/**' '!mod/other/**')"
check "an excluded one does not" \
  "$(is "$(filter_covers some-with-excludes 'mod/other/other.go' 'mod/**' '!mod/other/**')" false)"
check "the exclusion holds wherever it stands in the list" \
  "$(is "$(filter_covers some-with-excludes 'mod/other/other.go' '!mod/other/**' 'mod/**')" false)"
check "a path no include matches does not match" \
  "$(is "$(filter_covers some-with-excludes 'docs/readme.md' 'mod/**' '!mod/other/**')" false)"
check "nor does one only a ! pattern matches" \
  "$(is "$(filter_covers some-with-excludes 'mod/other/other.go' '!mod/other/**')" false)"

echo "case: a predicate-quantifier the model does not read is refused, not guessed"
check "every is refused" "$(is "$(refused_quantifier every)" 'predicate-quantifier every is not modelled')"
check "and so is a name that is no quantifier" \
  "$(is "$(refused_quantifier all)" 'predicate-quantifier all is not modelled')"

echo "case: events() reads the workflow's on:, which YAML 1.1 gives as the boolean true"
check "a bare on: key is that true" "$(is "$(triggers 'on:
  push:
    paths: [docker/**]
  workflow_dispatch:
')" 'push workflow_dispatch')"
check "a quoted \"on\" key is the string" "$(is "$(triggers '"on":
  push:
')" 'push')"
check "one event named as a string" "$(is "$(triggers 'on: push')" 'push')"
check "a list of events" "$(is "$(triggers 'on: [push, pull_request]')" 'push pull_request')"
check "a document with no on: at all" "$(is "$(triggers 'jobs: {}')" '')"
check "and each event keeps its own block" "$(is "$(py 'import yaml
print(" ".join(wp.events(yaml.safe_load(sys.argv[1]))["push"]["paths"]))' 'on:
  push:
    paths: [docker/**, .dockerignore]
')" 'docker/** .dockerignore')"

echo "case: a pattern GitHub reads another way is refused, not guessed"
check "? is refused" "$(is "$(unread 'package.json?')" 'package.json?')"
check "+ is refused" "$(is "$(unread 'docs/a+')" 'docs/a+')"
check "a backslash is refused" "$(is "$(unread 'docs/a\b')" 'docs/a\b')"
check "a brace is refused" "$(is "$(unread '{docker/**,.dockerignore}')" '{docker/**,.dockerignore}')"
check "a closing brace alone is refused" "$(is "$(unread 'docker/}')" 'docker/}')"
check "the patterns it does read are kept" \
  "$(is "$(unread 'packages/app/**' 'docker/*' 'v[0-9].yaml' '!docs/**')" '')"
check "and it names each refused pattern of a list" \
  "$(is "$(unread 'packages/app/**' 'package.json?' 'docker/}')" 'package.json? docker/}')"

echo "case: a paths-filter pattern keeps picomatch's ?, and only a brace is refused"
check "? is kept" "$(is "$(unread_filter 'package.jso?')" '')"
check "+ is kept" "$(is "$(unread_filter 'docs/a+')" '')"
check "a backslash is kept" "$(is "$(unread_filter 'docs/a\b')" '')"
check "a brace picomatch expands is refused" "$(is "$(unread_filter '{a,b}/**')" '{a,b}/**')"
check "a closing brace alone is refused" "$(is "$(unread_filter 'docker/}')" 'docker/}')"

echo "case: UniqueKeyLoader refuses a key written again as an alias of it, as js-yaml does"
# PyYAML composes an alias to the very node its anchor composed, so a key repeated through an alias
# of it is one node twice in its mapping, and that node carries its anchor's line alone. js-yaml
# 4.1.0, which dorny/paths-filter v4.0.3 reads its filters with, refuses it as a duplicated mapping
# key, in a block mapping and in a flow one, and so where the alias names a key anchored in another
# mapping or a value that repeats a plain key; each refusal names the line the repeat is written
# on. The controls are documents js-yaml 4.1.0 reads: an alias as a value, an anchored key aliased
# only as a value, and a merge key whose mapping a key written beside it overrides.
check "a key written twice is refused, naming the line of each copy" \
  "$(is "$(unique_load $'app: [a]\nsite: [b]\napp: [c]\n')" \
    "refused: the mapping key 'app' of line 3 is the key of line 1 again: js-yaml (dorny/paths-filter's reader) and GitHub's workflow parser refuse a repeated key, where yaml.safe_load keeps the last copy and reports nothing")"
check "a key written again as an alias of it is refused, naming the alias's line and the alias" \
  "$(is "$(unique_load $'site: [b]\n&name app: [a]\n*name : [c]\n')" \
    "refused: the mapping key 'app' of line 3, written as the alias *name, is the key of line 2 again: js-yaml (dorny/paths-filter's reader) and GitHub's workflow parser refuse a repeated key, where yaml.safe_load keeps the last copy and reports nothing")"
check "and so in a flow mapping" \
  "$(contains "$(unique_load '{ &name app: [a], *name : [c] }')" "^refused: the mapping key 'app' of line 1, written as the alias \*name, is the key of line 1 again: ")"
check "an alias of a key anchored in another mapping, beside the plain key, is refused on the alias's line" \
  "$(contains "$(unique_load $'a:\n  x: 1\n  &k app: [x]\nb:\n  app: [y]\n  *k : [z]\n')" "^refused: the mapping key 'app' of line 6, written as the alias \*k, is the key of line 5 again: ")"
check "an alias of a value that repeats a plain key is refused on the alias's line" \
  "$(contains "$(unique_load $'anchor: &k app\napp: [y]\n*k : [z]\n')" "^refused: the mapping key 'app' of line 3, written as the alias \*k, is the key of line 2 again: ")"
check "an alias of a key anchored in another mapping, alone in its own, is read" \
  "$(is "$(unique_load $'a:\n  x: 1\n  &k app: [x]\nb:\n  *k : [z]\n')" '{"a": {"app": ["x"], "x": 1}, "b": {"app": ["z"]}}')"
check "an alias as a value is read" \
  "$(is "$(unique_load $'shared: &shared [go.work]\napp:\n  - *shared\n  - mod/**\n')" \
    '{"app": [["go.work"], "mod/**"], "shared": ["go.work"]}')"
check "an anchored key aliased only as a value is read" \
  "$(is "$(unique_load $'site: [b]\n&name app: [a]\nother: [*name]\n')" \
    '{"app": ["a"], "other": ["app"], "site": ["b"]}')"
check "a merge key whose mapping a written key overrides is read" \
  "$(is "$(unique_load $'base: &base\n  app: a\n  site: b\nover:\n  <<: *base\n  app: c\n')" \
    '{"base": {"app": "a", "site": "b"}, "over": {"app": "c", "site": "b"}}')"
check "two merge keys written as such are read, as js-yaml reads them" \
  "$(is "$(unique_load $'site: [b]\n<<: {a: 1}\n<<: {b: 2}\n')" '{"a": 1, "b": 2, "site": ["b"]}')"
check "and so is an anchored merge key no alias names" \
  "$(is "$(unique_load $'site: [b]\n&merge <<: {}\n')" '{"site": ["b"]}')"

echo "case: UniqueKeyLoader refuses a merge key written as an alias"
# js-yaml 4.1.0 reads a `<<` as a merge key only where it is written as one: an alias of one is a
# key named `<<`, which it loads once and refuses twice as a duplicated mapping key. PyYAML merges
# the alias instead, so the two would read different mappings and the check would judge filters the
# action does not read; the loader refuses it at its first alias, naming the alias's own line.
check "an alias of a merge key is refused, naming its line, the alias and the merge key's line" \
  "$(is "$(unique_load $'site: [b]\n&merge <<: {}\n*merge : {}\n')" \
    "refused: the mapping key '<<' of line 3 is the merge key of line 2 written as an alias (*merge): js-yaml (dorny/paths-filter's reader) reads it as a key named '<<', where PyYAML merges it, so the two would read different mappings")"
check "and so where it is written twice" \
  "$(contains "$(unique_load $'site: [b]\n&merge <<: {}\n*merge : {}\n*merge : {}\n')" "^refused: the mapping key '<<' of line 3 is the merge key of line 2 written as an alias (\*merge): ")"

echo "case: load_paths_filter reads the filters with dorny/paths-filter's own js-yaml and refuses where it differs"
# dorny/paths-filter v4.0.3 reads its filters with js-yaml 4.1.0 under Node, and js-yaml keys a
# mapping by String() of each key's value with its default schema, where PyYAML resolves keys by
# YAML 1.1 and keys its mapping by the Python values. So two keys written differently can be one
# key to js-yaml, which refuses the file as a duplicated mapping key, while PyYAML keeps both or
# the last; and two keys can be one to PyYAML and two to js-yaml. Either way a check reading with
# PyYAML alone judges filters the action does not read. Every document here is read by that
# js-yaml itself (package.json's js-yaml-paths-filter alias, which bun.lock pins to the tarball
# the action's own lockfile names).
one_key() {
  check "$1" "$(contains "$(filters_load "$2")" "^refused: js-yaml 4.1.0, which dorny/paths-filter reads it with, refuses it: duplicated mapping key ($3)")"
}
one_key "01 and 1 are one key" $'01: [x]\n1: [y]\n' '2:1'
one_key "null and ~ are one key" $'null: [x]\n~: [y]\n' '2:1'
one_key "true and True are one key" $'true: [x]\nTrue: [y]\n' '2:1'
one_key "1 and 1.0 are one key" $'1: [x]\n1.0: [y]\n' '2:1'
one_key "0x1 and 1 are one key" $'0x1: [x]\n1: [y]\n' '2:1'
one_key "a key written as an alias of a value 1 is the key 01" $'anchor: &k 1\n01: [y]\n*k : [z]\n' '3:1'
one_key "a key written as an alias of a value ~ is the key null" $'anchor: &k ~\nnull: [y]\n*k : [z]\n' '3:1'
one_key "two timestamps a millisecond apart are one key: String() of a Date drops the milliseconds" \
  $'2001-01-01T00:00:00.001Z: [x]\n2001-01-01T00:00:00.002Z: [y]\n' '2:1'
one_key "0099-01-01 and 1999-01-01 are one key: Date.UTC reads a year under 100 as 1900 and over" \
  $'0099-01-01: [x]\n1999-01-01: [y]\n' '2:1'
one_key "a quoted key with a written !!int tag is that int" $'? !!int "01"\n: [x]\n? "1"\n: [y]\n' '3:2'
two_keys() {
  check "$1: js-yaml reads $3" "$(is "$(js_keys "$2")" "$3")"
  check "$1: and the loader refuses it, naming both readings" \
    "$(contains "$(filters_load "$2")" "^refused: js-yaml 4.1.0, which dorny/paths-filter reads it with, and PyYAML, which this check reads it with, read it differently: the document holds the keys $4 to js-yaml and $5 to PyYAML; ")"
}
two_keys "on and yes are two keys to js-yaml and one to PyYAML, True" $'on: [x]\nyes: [y]\n' "on yes" "'on', 'yes'" True
two_keys "010 and 8 are two keys to js-yaml and one to PyYAML, octal 8" $'010: [x]\n8: [y]\n' "8 10" "'8', '10'" 8
two_keys "1:20 and 80 are two keys to js-yaml and one to PyYAML, sexagesimal 80" $'1:20: [x]\n80: [y]\n' "80 1:20" "'80', '1:20'" 80
two_keys "null and a quoted ~ tagged ! are two keys to js-yaml, and one to PyYAML, None" \
  $'null: [x]\n? ! "~"\n: [y]\n' "null ~" "'null', '~'" None
check "a value js-yaml reads as text and PyYAML as a boolean is refused, naming both" \
  "$(contains "$(filters_load $'app:\n  - yes\n')" "^refused: .* read it differently: the document > 'app' > item 1 is the text 'yes' to js-yaml and True (bool) to PyYAML; ")"
check "an int past 2^53 js-yaml rounds to another double is refused, naming both" \
  "$(contains "$(filters_load $'app: [9007199254740993]\n')" "^refused: .* the document > 'app' > item 1 is the number 9007199254740992 to js-yaml and 9007199254740993 (int) to PyYAML; ")"
check "an int past the largest double is refused, not raised" \
  "$(contains "$(filters_load "app: [1$(printf '0%.0s' $(seq 400))]")" "^refused: .* the document > 'app' > item 1 is the number Infinity to js-yaml and ")"
check "a timestamp's microseconds js-yaml keeps only to the millisecond are refused" \
  "$(contains "$(filters_load $'app: [2001-01-01T00:00:00.123456Z]\n')" "^refused: .* the document > 'app' > item 1 is the instant 2001-01-01T00:00:00.123Z to js-yaml and ")"
check "an int js-yaml holds exactly, a float and a timestamp to the millisecond are read" \
  "$(is "$(py 'print(type(wp.load_paths_filter(sys.argv[1])).__name__)' $'app: [9007199254740992, 1.5, 2001-01-01T00:00:00.123Z]\n')" dict)"
check "the quoted on and yes are read as js-yaml reads them" \
  "$(is "$(filters_load $'"on": [x]\n"yes": [y]\n')" '{"on": ["x"], "yes": ["y"]}')"
check "an alias as a value is read" \
  "$(is "$(filters_load $'shared: &shared\n  - go.work\napp:\n  - *shared\n  - mod/**\n')" \
    '{"app": [["go.work"], "mod/**"], "shared": ["go.work"]}')"
check "a merge key whose mapping a written key overrides is read" \
  "$(is "$(filters_load $'base: &base\n  app: a\n  site: b\nover:\n  <<: *base\n  app: c\n')" \
    '{"base": {"app": "a", "site": "b"}, "over": {"app": "c", "site": "b"}}')"
check "the repository's own filters file is read" \
  "$(is "$(py 'import yaml
try:
    print(type(wp.load_paths_filter(open(sys.argv[1]).read())).__name__)
except yaml.YAMLError as error:
    print("refused:", " ".join(str(error).split()))' "$script_dir/../ci-filters.yaml")" dict)"

echo "case: load_paths_filter ends the check by name without node or with another js-yaml"
# What it reads with is no answer about the text when it is missing or is not the action's: each
# ends the check with an ::error line naming what is missing, never a verdict on the filters.
mkdir -p "$work/no-node" "$work/js-yaml-other" "$work/js-yaml-none"
ln -s "$(python3 -c 'import sys; print(sys.executable)')" "$work/no-node/python3"
echo '{"version": "4.3.2"}' > "$work/js-yaml-other/package.json"
check "a PATH without node ends it naming node" \
  "$(contains "$(PATH="$work/no-node" py 'wp.load_paths_filter("app: [x]\n")' 2>&1 || true)" '^::error::node is not on PATH: ')"
check "an installed alias of another js-yaml ends it naming both versions" \
  "$(contains "$(py 'from pathlib import Path
wp.JS_YAML_DIR = Path(sys.argv[1])
wp.load_paths_filter("app: [x]\n")' "$work/js-yaml-other" 2>&1 || true)" '^::error::the installed js-yaml-paths-filter is js-yaml 4.3.2, and the dorny/paths-filter pins of workflow_paths.PATHS_FILTER_JS_YAML read their filters with js-yaml 4.1.0: ')"
check "no installed alias ends it naming bun install" \
  "$(contains "$(py 'from pathlib import Path
wp.JS_YAML_DIR = Path(sys.argv[1])
wp.load_paths_filter("app: [x]\n")' "$work/js-yaml-none" 2>&1 || true)" 'is not an installed js-yaml .*: run bun install --frozen-lockfile')"
check "the installed alias is the version the recorded pin's js-yaml is" \
  "$(is "$(py 'import json; print(json.loads((wp.JS_YAML_DIR / "package.json").read_text())["version"], *wp.PATHS_FILTER_JS_YAML.values())')" "4.1.0 4.1.0")"

echo "case: walk() lists every file of the tree, version control and dependencies apart"
root="$work/tree"
mkdir -p "$root/packages/app/src" "$root/.git/objects" "$root/node_modules/dep" "$root/elsewhere"
echo '{}' > "$root/package.json"
echo 'export {};' > "$root/packages/app/src/index.ts"
echo 'object' > "$root/.git/objects/one"
echo 'module.exports = {};' > "$root/node_modules/dep/index.js"
echo 'linked' > "$root/elsewhere/file.txt"
ln -s ../../elsewhere "$root/packages/app/linked"
check "every file, by its path from the working directory, with no skipped directory and no symlinked one" \
  "$(is "$(cd "$root" && walked)" 'elsewhere/file.txt package.json packages/app/src/index.ts')"

summary "workflow_paths.py, the path model the CI path checks import"
