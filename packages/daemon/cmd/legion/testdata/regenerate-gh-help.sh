#!/usr/bin/env bash
# Saves the real gh's help of each command the `legion gh` tests read into gh-help/, one file per
# command path with its words joined by "_" (gh.txt for gh's own, `gh --help`), its reference of
# every command (`gh help reference`) as reference.txt, and the gh version into gh-help/VERSION.
# Run it with the gh the worker image pins on PATH, or LEGION_GH_PATH naming it, when that pin
# moves. gh runs with an empty configuration directory and no extensions, as in a fresh pane, so
# its own help lists gh's one default alias (co) and nothing of this machine's. Nothing gh says on
# its stderr is printed, since whatever answers gh's calls in GitHub's place holds gh's token and
# writes it: it goes into a file of its own, which a failure names and which stays when it holds
# anything. The version is printed only as a version line of gh's own shape.
set -euo pipefail
gh=${LEGION_GH_PATH:-$(command -v gh)}
out=$(cd "$(dirname "$0")" && pwd)/gh-help
clean=$(mktemp -d)
said=$(mktemp "${TMPDIR:-/tmp}/regenerate-gh-help.XXXXXX")
trap 'rm -rf "$clean"; [ -s "$said" ] || rm -f "$said"' EXIT
mkdir -p "$clean/config" "$clean/data"
# run ARGS: gh ARGS, its stderr added to $said; a failure stops the script, naming ARGS and $said.
run() {
  env GH_CONFIG_DIR="$clean/config" XDG_DATA_HOME="$clean/data" GH_NO_UPDATE_NOTIFIER=1 NO_COLOR=1 "$gh" "$@" 2>>"$said" || {
    local rc=$?
    echo "gh $* exited $rc; gh's own words are in $said" >&2
    exit "$rc"
  }
}
rm -rf "$out"
mkdir -p "$out"
run --version | head -1 >"$out/VERSION"
run --help >"$out/gh.txt"
run help reference >"$out/reference.txt"
for path in "pr" "pr view" "pr create" "pr list" "pr ls" "pr comment" "pr diff" "pr edit" "pr checks" "pr status" "pr merge" \
  "pr review" "api" "repo" "repo view" "repo clone" "repo create" "repo edit" "repo fork" "repo set-default" "repo sync" \
  "repo list" "repo deploy-key" "repo deploy-key add" "issue" "issue close" "issue comment" "issue create" "issue develop" \
  "issue edit" "issue transfer" "issue view" "issue list" "issue status" "label" "label clone" "label list" "label create" \
  "project" "project link" "project unlink" "run" "run list" "run view" "run watch" "run rerun" "release" "release create" \
  "release view" "release list" "release delete" "workflow" "workflow view" "workflow list" "workflow run" "cache" \
  "cache list" "cache delete" "search" "search code" "search commits" "search issues" "search prs" "search repos" "auth" \
  "auth setup-git" "auth status" "co" "secret" "secret set" "browse" "extension" "agent-task" "agent-task view"; do
  # shellcheck disable=SC2086
  run $path --help >"$out/${path// /_}.txt"
done
version=$(<"$out/VERSION")
[[ $version =~ ^gh\ version\ [0-9]+\.[0-9]+\.[0-9]+\ \([0-9]{4}-[0-9]{2}-[0-9]{2}\)$ ]] ||
  version="a version line not of gh's shape, in $out/VERSION"
[ ! -s "$said" ] || echo "gh wrote on its stderr; its words are in $said" >&2
echo "saved $(find "$out" -name '*.txt' ! -name reference.txt | wc -l) help files and gh's reference from $version"
