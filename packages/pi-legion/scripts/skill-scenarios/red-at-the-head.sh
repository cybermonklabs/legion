# shellcheck shell=bash
# The red-at-the-head world of the skill-scenario rig: the world's facts and workflows, the
# tester-proof pull request worker_fixture builds and the red red_fixture turns it to, the routes
# the gh stand-in answers from, the model of the red, the rig's report of world `l`'s check
# (red_report) and the settlement of the re-run (rerun_settle). rig.sh sources it, and so does
# red-at-the-head.test.ts, which tests it. It defines functions and the variables below, and runs
# nothing. Its jq is the modules under jq/ beside it (red_jq): worlds.jq, the twenty worlds and
# their workflows; routes.jq, the routes and the model of the red; programs.jq, the programs a
# route runs over its answer, which gh-standin.ts loads. A caller defines `note` and `fail`, as
# rig.sh does, and sets R, the run directory, before it calls a function here.

# This library's directory: its jq modules, and the gh stand-in beside it.
red_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
# The tester-proof world (worker_fixture below): the run's own issue, which run_tester_proof sets
# once seed.ts has filed it, and the world's repository and the branch its pull request merges
# into, which world_repository writes to the run's world.json and reads back from there.
worker_key=''
worker_repo=''
worker_base=''

# red_jq ARG... runs jq with jq/ on its library path, where each program here is a function of a
# module.
red_jq() { jq -L "$red_dir/jq" "$@"; }

# other_commit_routes HEAD_SHA BRANCH PR BASE BASE_SHA prints routes.jq's other_commit_routes, what
# a read of a commit names, for the head, HEAD_SHA, of pull request PR, and for the base, BASE_SHA
# on BASE, in the world's repository.
other_commit_routes() {
  red_jq -nc --arg head "$1" --arg branch "$2" --arg pr "$3" --arg base "$4" --arg base_sha "$5" \
    --arg repo "$worker_repo" 'include "routes"; other_commit_routes'
}

# The words gh 2.98.0 prints to no terminal (measured against a local stand-in of the API): the
# help of `gh run rerun`, whole, and the usage errors of `gh run rerun` and `gh run view` that
# name no run and no job where no terminal is there to pick one. The help and the usage error of
# `gh run rerun` share its flags. A caller reads each through $(…), which drops the newlines each
# ends with: the help goes to stdout, which the stand-in ends with one newline, and a usage error,
# which goes to stderr as it stands, has its one newline put back where a route answers it.
rerun_flags() {
  cat <<'EOF'
  -d, --debug        Rerun with debug logging
      --failed       Rerun only failed jobs, including dependencies
  -j, --job string   Rerun a specific job ID from a run, including dependencies
EOF
}
rerun_help() {
  cat <<'EOF'
Rerun an entire run, only failed jobs, or a specific job from a run.

Note that due to historical reasons, the `--job` flag may not take what you expect.
Specifically, when navigating to a job in the browser, the URL looks like this:
`https://github.com/<owner>/<repo>/actions/runs/<run-id>/jobs/<number>`.

However, this `<number>` should not be used with the `--job` flag and will result in the
API returning `404 NOT FOUND`. Instead, you can get the correct job IDs using the following command:

	gh run view <run-id> --json jobs --jq '.jobs[] | {name, databaseId}'

You will need to use databaseId field for triggering job re-runs.


USAGE
  gh run rerun [<run-id>] [flags]

FLAGS
EOF
  rerun_flags
  cat <<'EOF'

INHERITED FLAGS
      --help                     Show help for command
  -R, --repo [HOST/]OWNER/REPO   Select another repository using the [HOST/]OWNER/REPO format

LEARN MORE
  Use `gh <command> <subcommand> --help` for more information about a command.
  Read the manual at https://cli.github.com/manual
  Learn about exit codes using `gh help exit-codes`
  Learn about accessibility experiences using `gh help accessibility`

EOF
}
rerun_usage() {
  printf '%s\n\n%s\n\n%s\n' '`<run-id>` or `--job` required when not running interactively' \
    'Usage:  gh run rerun [<run-id>] [flags]' 'Flags:'
  rerun_flags
  printf '  \n'
}
view_usage() {
  cat <<'EOF'
run or job ID required when not running interactively

Usage:  gh run view [<run-id>] [flags]

Flags:
  -a, --attempt uint      The attempt number of the workflow run
      --exit-status       Exit with non-zero status if run failed
  -j, --job string        View a specific job ID from a run
  -q, --jq expression     Filter JSON output using a jq expression
      --json fields       Output JSON with the specified fields
      --log               View full log for either a run or specific job
      --log-failed        View the log for any failed steps in a run or specific job
  -t, --template string   Format JSON output using a Go template; see "gh help formatting"
  -v, --verbose           Show job steps
  -w, --web               Open run in the browser
  
EOF
}

# The usage gh 2.98.0 prints under a usage error of `gh run watch` to no terminal (measured with
# the gh of the worker image, 2026-11-20, `gh run watch -R example/widgets` and `gh run watch 1 -i
# x -R example/widgets`, no request made): under `run ID required when not running interactively`
# for a call that names no run (watch.go:73-77), and under cobra's refusal of an --interval that
# is no integer (watch.go:90). A route puts the error's own line and a blank line ahead of it.
watch_usage() {
  cat <<'EOF'
Usage:  gh run watch <run-id> [flags]

Flags:
      --compact        Show only relevant/failed steps
      --exit-status    Exit with non-zero status if run fails
  -i, --interval int   Refresh interval in seconds (default 3)
  
EOF
}

# The usage gh 2.98.0 prints under a usage error of `gh pr checks` to no terminal (measured with
# the gh of the worker image, 2026-11-20, `gh pr checks 1 -R example/widgets` with `--json name
# --watch`, `--fail-fast`, `--interval 5` and `--watch -i x`, no request made), under the error's
# own line and a blank line (checks.go:80-101).
checks_usage() {
  cat <<'EOF'
Usage:  gh pr checks [<number> | <url> | <branch>] [flags]

Flags:
      --fail-fast         Exit watch mode on first check failure
  -i, --interval int      Refresh interval in seconds in watch mode (default 10)
  -q, --jq expression     Filter JSON output using a jq expression
      --json fields       Output JSON with the specified fields
      --required          Only show checks that are required
  -t, --template string   Format JSON output using a Go template; see "gh help formatting"
      --watch             Watch checks until they finish
  -w, --web               Open the web browser to show details about checks
  
EOF
}

# world_repository writes the world's repository and the branch its pull request merges into to
# $R/world.json, the one place either is written, and reads both back into worker_repo and
# worker_base, from which the routes, the pane (worker-pane.ts reads world.json itself), the
# run's Dispatch issue (seed.ts tester-proof reads it too) and the prompt take them. The branch is
# the world's base, `main`, the branch its remote's history is on, so the worker's addressing, its
# Dispatch issue and its pull request name one repository and one branch: with a second
# repository in the addressing (`skills-rig/project`, branch `trunk`) and none on the issue, seven
# runs of the batch at 6a643564 found no repository to read and concluded there was no CI.
# worker_fixture adds the rest of the world to this file.
world_repository() {
  jq -n '{repo:"example/widgets", base:"main"}' >"$R/world.json"
  IFS=$'\t' read -r worker_repo worker_base < <(jq -r '[.repo, .base] | @tsv' "$R/world.json")
}

# world_check_when WORLD prints when the world's one check, k1 of its Verification document, runs
# (seed.ts tester-proof writes it with that `when`): `after_merge` in a world whose role must reach
# an approval, the reviewer whose round goes on (red_world), and `before_merge` in every other world
# and in the tester-proof world, which names none. This rig's Dispatch reaches no GitHub, so it can
# place no commit, which leaves every `before_merge` check without a standing pass and makes a
# reviewer refuse every approval it is asked for (seed.ts testerProofIssue says what was
# measured); a reviewer whose round ends in a changes request is not held up by the gate.
world_check_when() {
  local facts
  if [ -n "${1:-}" ]; then
    facts=$(red_world "$1") || fail "red_world: '$1' is no world of red-at-the-head"
    if jq -e '.role == "reviewer" and .round == "goes-on"' <<<"$facts" >/dev/null; then
      echo after_merge
      return
    fi
  fi
  echo before_merge
}

# checks_lines HEAD prints the pull request body's line for each check of the run's Verification
# document, in its order, as seed.ts tester-proof wrote the checks to $R/fixture.json: a check
# with the implementer's result the line Dispatch writes for that result at HEAD
# (checkResultLine, packages/envoy/internal/dispatch/api/results.go: `- Check <id> (<when>):
# <verdict> at <commit>, recorded by <role>. <reading>`, the reading with its whitespace
# collapsed and short of the length Dispatch cuts one at), which seed.ts implement-handoff
# records at HEAD; and an `after_merge` check, which holds no result before the merge,
# skill://legion-worker/references/pr-body.md's `- Check <block id> (after_merge): not yet run`.
# A check that is neither has no line the world can state, and fails.
checks_lines() {
  jq -er --arg head "$1" '
    .checks | if length == 0 then error("no check") else . end
    | map(if .result != null
          then "- Check \(.id) (\(.when)): \(.result.verdict) at \($head), recorded by \(.result.role). "
               + (.result.observed | [splits("\\s+")] | map(select(. != "")) | join(" "))
          elif .when == "after_merge" then "- Check \(.id) (after_merge): not yet run"
          else error("check \(.id) (\(.when)) holds no result") end)
    | join("\n")' "$R/fixture.json"
}

# repo_snapshot SRC C0 C1 DATE prints what the repository at SRC holds, as GitHub answers it from
# the remote worker_fixture pushed it to, as one JSON object: the pull request's diff and its
# files (C0..C1), the two commits, each dated DATE, and the tree of each.
repo_snapshot() {
  local src=$1 c0=$2 c1=$3 date=$4 diff files commits trees
  local g=(git -C "$src")
  diff=$("${g[@]}" diff "$c0" "$c1")
  files=$("${g[@]}" diff --name-status "$c0" "$c1" | while IFS=$'\t' read -r status name; do
    jq -n --arg name "$name" --arg status "$status" --arg sha "$("${g[@]}" rev-parse "$c1:$name")" \
      --arg patch "$("${g[@]}" diff "$c0" "$c1" -- "$name" | sed -n '/^@@/,$p')" \
      --argjson lines "$("${g[@]}" diff --numstat "$c0" "$c1" -- "$name" | awk '{print "[" $1 "," $2 "]"}')" \
      --arg repo "$worker_repo" --arg head "$c1" '
      {sha:$sha, filename:$name,
       status:({A:"added", M:"modified", D:"removed"}[$status] // "modified"),
       additions:$lines[0], deletions:$lines[1], changes:($lines[0] + $lines[1]),
       blob_url:("https://github.com/" + $repo + "/blob/" + $head + "/" + $name),
       raw_url:("https://github.com/" + $repo + "/raw/" + $head + "/" + $name),
       contents_url:("https://api.github.com/repos/" + $repo + "/contents/" + $name + "?ref=" + $head),
       patch:$patch}'
  done | jq -sc .)
  commits=$(for c in "$c0" "$c1"; do
    jq -n --arg sha "$c" --arg tree "$("${g[@]}" rev-parse "$c^{tree}")" \
      --arg parent "$("${g[@]}" rev-parse -q --verify "$c^" || true)" \
      --arg message "$("${g[@]}" log -1 --format=%B "$c")" \
      --arg date "$date" --arg repo "$worker_repo" '
      {sha:$sha,
       commit:{message:$message, tree:{sha:$tree},
               author:{name:"Fixture", email:"fixture@example.invalid", date:$date},
               committer:{name:"Fixture", email:"fixture@example.invalid", date:$date}},
       parents:(if $parent == "" then [] else [{sha:$parent}] end),
       html_url:("https://github.com/" + $repo + "/commit/" + $sha)}'
  done | jq -sc .)
  # Each tree whole, every subtree and blob under it (ls-tree -r -t), in the shape GitHub's git/trees
  # answers (measured on cybermonklabs/legion, 2026-11-19, gittree-root, requests.log line 77:
  # sha, url, tree, truncated; each entry its path, mode, type, sha, a blob's size, and its url);
  # the routes answer the top level alone unless the read asks `recursive` (tree_program).
  trees=$(for c in "$c0" "$c1"; do
    "${g[@]}" ls-tree -r -t -l "$c" | jq -Rsc --arg sha "$("${g[@]}" rev-parse "$c^{tree}")" \
      --arg repo "$worker_repo" '
      ("https://api.github.com/repos/" + $repo + "/git/") as $git
      | {sha:$sha, url:($git + "trees/" + $sha),
         tree:(split("\n") | map(select(. != "")
               | capture("^(?<mode>\\S+) (?<type>\\S+) (?<sha>\\S+)\\s+(?<size>\\S+)\t(?<path>.*)$")
               | {path, mode, type, sha} + (if .type == "blob" then {size:(.size|tonumber)} else {} end)
                 + {url:($git + (if .type == "blob" then "blobs/" else "trees/" end) + .sha)})),
         truncated:false}'
  done | jq -sc .)
  jq -nc --arg diff "$diff" --argjson files "$files" --argjson commits "$commits" \
    --argjson trees "$trees" '{diff:$diff, files:$files, commits:$commits, trees:$trees}'
}

# The tester's frozen world under $R, on the run's own issue <key> (seed.ts tester-proof, which
# run_tester_proof files first): a bare remote whose post-receive hook logs every push, the issue
# workspace cloned from it, the implementer's commit on legion/<key>, the PR the gh stand-in serves,
# and the implementer's handoff body at $R/implement.json, which run_tester_proof records on the
# issue. The fixture's content is the same in every run; the PR number is 1000 plus <index>, the
# run's index in its batch, and the commits are dated <index> seconds after <start>, the batch's
# start, so no two runs of a batch share a PR number or a head (two runs can meet in a scratch file
# in the shared /tmp), and each batch's heads carry its own start time.
# The CLI must meet every requirement the seeded spec marks, and the issue's Verification document
# holds the check that the implementer's seeded result and the tester's own run are against.
# The world's repository and the branch the pull request merges into are world_repository's, read
# from $R/world.json, and the pull request body's line for each check of the Verification document
# is written from that check as seed.ts tester-proof wrote it to $R/fixture.json (checks_lines).
# The base commit holds the workflows of the world WORLD names (red_workflows; the tester-proof
# world's where it names none), so the head inherits them and the pull request changes no file
# under .github.
# testerProof fails a run that
# pushes a change to legion/<key>, and it cannot tell a tester that pinned a real defect with a red
# test, which roles/core/tester.md asks for, from one that went off-task. A defect left in the
# fixture turns correct testing into a fail.
worker_fixture() {
  local index=$1 start=$2 world=${3:-} src C0 C1 body when worker_pr worker_pr_url jj_setup_config
  local checks title model green file
  worker_pr=$((1000 + index))
  worker_pr_url=https://github.com/$worker_repo/pull/$worker_pr
  title="feat: greet() ($worker_key)"
  when="@$((start + index)) +0000"
  git init -q --bare -b "$worker_base" "$R/remote.git"
  cat >"$R/remote.git/hooks/post-receive" <<EOF
#!/bin/sh
while read old new ref; do echo "\$(date -u +%FT%T.%3NZ) \$ref \$old \$new" >>$(printf %q "$R/pushes.log"); done
EOF
  chmod +x "$R/remote.git/hooks/post-receive"
  src=$R/src
  git init -q -b "$worker_base" "$src"
  g() {
    GIT_AUTHOR_DATE=$when GIT_COMMITTER_DATE=$when \
      git -C "$src" -c user.name=Fixture -c user.email=fixture@example.invalid -c commit.gpgsign=false "$@"
  }
  printf '# widgets\n\nSmall CLI helpers.\n' >"$src/README.md"
  model=$(red_workflows "$world" "$title") || fail "worker_fixture: no workflows of world '$world'"
  green=$(red_workflows) || fail "worker_fixture: no workflows of the tester-proof world"
  mkdir -p "$src/.github/workflows"
  for file in $(jq -r '.workflows[].file' <<<"$model"); do
    red_jq -r --arg file "$file" 'include "routes"; .workflows[] | select(.file == $file) | workflow_yaml' \
      <<<"$model" >"$src/.github/workflows/$file"
  done
  g add -A && g commit -qm "init"
  C0=$(g rev-parse HEAD)
  g push -q "$R/remote.git" "$worker_base"
  g checkout -qb "legion/$worker_key"
  cat >"$src/greet.ts" <<'EOF'
export function greet(name: string): string {
  return `Hello, ${name.trim()}!`;
}

if (import.meta.main) {
  const name = Bun.argv[2];
  if (name === undefined || name.trim() === "") {
    console.error("usage: greet.ts <name>");
    process.exit(2);
  }
  console.log(greet(name));
}
EOF
  cat >"$src/greet.test.ts" <<'EOF'
import { expect, test } from "bun:test";
import { greet } from "./greet";

test("greets by name", () => {
  expect(greet("Ada")).toBe("Hello, Ada!");
});

test("trims the name", () => {
  expect(greet("  Ada  ")).toBe("Hello, Ada!");
});
EOF
  g add -A && g commit -qm "feat: greet(), a greeting helper for the widgets CLI ($worker_key)"
  C1=$(g rev-parse HEAD)
  g push -q "$R/remote.git" "legion/$worker_key"
  : >"$R/pushes.log"
  # The implement handoff as the implementer records it with `dispatch handoff-write`: the three
  # stamps (schemaVersion, phase, issue) and the phase's fields; Dispatch records when it was
  # written. What the implementer ran is no part of it: seed.ts implement-handoff records that as a
  # result against the issue's check k1. The PR's head is the implementer's code commit, since no
  # handoff commit sits above it.
  jq -n --arg key "$worker_key" '
    {schemaVersion:1, phase:"implement", issue:$key,
     filesChanged:["greet.ts","greet.test.ts"]}' >"$R/implement.json"
  # Isolated from the operator's own jj configuration (fsmonitor.backend = "watchman" on a devbox
  # that runs it): a bare JJ_CONFIG naming only fsmonitor.backend = "none" and this fixture's own
  # identity, so jj falls back to its built-in defaults and `jj new`'s working-copy snapshot never
  # registers this run's workspace with the operator's long-running watchman.
  jj_setup_config=$R/jj-setup.toml
  printf '[user]\nname = "Rig Worker"\nemail = "rig@example.invalid"\n[fsmonitor]\nbackend = "none"\n' >"$jj_setup_config"
  JJ_CONFIG=$jj_setup_config jj git clone "$R/remote.git" "$R/ws" >/dev/null 2>&1
  (cd "$R/ws" && JJ_CONFIG=$jj_setup_config jj bookmark track "legion/$worker_key" --remote origin && JJ_CONFIG=$jj_setup_config jj new "legion/$worker_key") >/dev/null 2>&1
  checks=$(checks_lines "$C1") || fail "worker_fixture: no check of $R/fixture.json to write in the body"
  body="Dispatch: $worker_key

Adds \`greet(name)\` and \`bun greet.ts <name>\`.

## Verification

**Threads:** 0 resolved, 0 unresolved.

**Checks:**
$checks

**Fast-follow:** none.

**Chain:** not stacked."
  # What the repository holds, as GitHub answers it from the remote this fixture pushed.
  local snapshot date
  date=$(date -u -d "@$((start + index))" +%Y-%m-%dT%H:%M:%SZ)
  snapshot=$(repo_snapshot "$src" "$C0" "$C1" "$date") || fail "worker_fixture: no snapshot of $src"
  # The routes the gh stand-in answers from, and the template of the comment a role posts:
  # routes.jq's pull_routes over this world.
  red_jq -n --arg head "$C1" --arg body "$body" --arg url "$worker_pr_url" --arg ref "legion/$worker_key" \
    --arg key "$worker_key" --argjson number "$worker_pr" --arg repo "$worker_repo" \
    --arg base_sha "$C0" --arg date "$date" --argjson snapshot "$snapshot" \
    --argjson others "$(other_commit_routes "$C1" "legion/$worker_key" "$worker_pr" "$worker_base" "$C0")" \
    --arg base "$worker_base" --arg title "$title" --argjson green "$green" \
    'include "routes"; pull_routes' >"$R/fixtures.json"
  # The world the scorer reads, beside the repository and the base world_repository wrote: head
  # is the PR's head, the implementer's code commit. It names the base the pull request merges
  # into, its commit, and the pull request's title, which red_fixture reads here.
  jq --arg key "$worker_key" --argjson pr "$worker_pr" --arg head "$C1" --arg base_sha "$C0" \
    --arg title "$title" \
    '{key:$key, repo:.repo, pr:$pr, branch:("legion/" + $key), head:$head, base:.base,
      base_sha:$base_sha, title:$title}' "$R/world.json" >"$R/world.full.json"
  mv "$R/world.full.json" "$R/world.json"
}

# red_world WORLD prints the facts of WORLD as one JSON object (worlds.jq's red_world, whose table
# is the one place each world's facts are written), and for a world outside the twenty prints
# nothing and fails.
red_world() {
  red_jq -nce --arg world "$1" 'include "worlds"; red_world($world)'
}

# red_worlds prints red_world's worlds as a refusal names them, `a, b, … s or t` (worlds.jq).
red_worlds() {
  red_jq -nr 'include "worlds"; red_worlds'
}

# red_workflows [WORLD [TITLE]] prints the model of the world's GitHub Actions workflows as one
# JSON object (worlds.jq's red_workflows), from the world's facts and the red's own lane, both in
# the table worlds.json, or the world-less tester-proof world's where it names no WORLD; TITLE is
# the pull request's title, which world `f`'s title check prints in its failing line.
red_workflows() {
  local world=${1:-} title=${2:-}
  [ -z "$world" ] || red_world "$world" >/dev/null ||
    fail "red_workflows: '$world' is no world of red-at-the-head"
  red_jq -nc --arg world "$world" --arg title "$title" 'include "worlds"; red_workflows'
}

# red_fixture WORLD turns worker_fixture's green head red, over the world it has already written:
# the change is still greet.ts and greet.test.ts, so the repository root is the one package the
# change touches and every other directory is one it does not. The head reads red in all four
# places an agent reads it — the `gh pr view` rollup, `gh pr checks`, the GraphQL
# `statusCheckRollup` of the head, and the commit's check runs — and every call
# skills/legion-worker/references/red-at-the-head.md tells a role to make is answered: the head's
# own check runs, the re-run, the read of the failing attempt's log, the head's runs and the
# failing run's jobs. The GraphQL world is worker_fixture's own (gh-graphql.ts), whose head the jq
# below turns red where it stands, so a rollup read of the head by any name GraphQL resolves it by
# answers the counted set, and a read of any other commit answers that commit's own.
# The first three of those reads are the counted set, what GitHub counts for the pull request;
# the commit's check-runs endpoint answers every run at that commit instead, which in world `k`
# is one run more and in world `l` one run the counted set has not reported. The head's runs read
# is neither: it answers the head's own `pull_request` workflow runs with their `run_attempt`,
# which is where the page reads whether the head's one re-run is already spent — attempt 1 in
# every world but `e`, whose head carries a second attempt somebody already pressed.
# The head has one state per act, never one per read: every read of it answers a sequence, red,
# then the re-run's attempt running, then what the world settles at, and each of them follows the
# re-run's route rather than its own calls (gh-standin.ts, `follows`), so the head reads red
# however many times a role reads it before the re-run, the attempt queued or in progress (never
# completed) for the rest of the turn that pressed it, and the world's own answer once that turn
# has ended and the rig settled the re-run (rerun_settle, which puts each route's `settled` reply
# in place of the running one); `gh pr checks` exits 1 while a check it reports is red, 8 while
# one is pending and 0 when all passed, as the real command does. The merge state of the pull
# request follows the checks, as GitHub's MergeStateStatus
# states it (https://docs.github.com/en/graphql/reference/enums#mergestatestatus: BLOCKED "The
# merge is blocked.", UNSTABLE "Mergeable with non-passing commit status.", CLEAN "Mergeable and
# passing commit status."), with a required status check that has not passed blocking the merge
# (https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches#require-status-checks-before-merging):
# BLOCKED while a check the base requires is missing or red, UNSTABLE while it is green and
# another check is red, CLEAN when none is red; the world's protection requires no review, so
# checks alone decide it (merge_state below). Per world, from its facts: the heads of `a` to `f`,
# `h`, `k` and `l` report no `ci`, the one check their base requires, so each reads BLOCKED before
# and after the re-run; `g`, `i` and `m`, `q`, `r`, `s` and `t` read BLOCKED while `ci` is red and
# CLEAN once the re-run settles it and every lane green; `j` stays BLOCKED, its `ci` red again on
# the re-run, and so do `n` and `o`, which press no re-run; `p`, whose base requires `ci` and the
# title check, reads BLOCKED for the whole run, its `ci` green beside its red title check, since
# merge_state blocks on any required check that has not passed; and no world reads UNSTABLE,
# since none has every required check green beside a red lane. The base requires the aggregate
# check `ci`, and in `p` the title check too (red_world's `required`), and every read of its
# required set says so: `gh pr checks --required` answers the checks the base requires alone, as
# GitHub does, and the rules of the base's rulesets and its branch protection, the two reads the
# reviewer's prompt gives for that set, each name that set in the shape GitHub answers them in, as
# do the protection endpoint of the branch, its required status checks and their contexts, and the
# repository's rulesets and its one ruleset (required_set_fixture, which red_fixture runs last, so
# those seven reads go ahead of every other route). The heads of worlds `g`, `i`, `j` and `m` to
# `t` report `ci`, and at every other head the `--required` read answers gh's `no required checks
# reported on the '<branch>' branch` and exits 1. In a world that stays red both answers are red,
# and nothing of it moves; in a world that presses no re-run (`d`, `e`, `f`, `n`, `o`, `p`) the
# head reads red for the whole run. The counted reads of `l` and `t` answer one more state first,
# reporting neither the red nor the aggregate over it, and
# each route that answers them carries the mark `unreported`: red_report drops that first state of
# each marked route between the two turns, which is the rig reporting the check. A read of
# another commit's check runs or status takes the routes other_commit_routes wrote ahead of the
# head's, and the head's combined status answers GitHub's `pending` with no status, since no
# status was posted to it. The settlement the head's checks raise once the re-run settles is the
# envelope red_fixture writes to <run>/settlement.json, which rerun_settle publishes. Only a
# re-run of the failed jobs of the run the red belongs to (`--failed`), or of the red's own job
# (`--job`), presses it, as GitHub re-runs the jobs that depend on that one; gh's help and its
# errors for a call naming no run, or a run or a job GitHub has no run or job for, are answered
# as gh answers them, and move neither the head nor the count the head's reads follow; a re-run
# of all the jobs of a run, of another job of the head or of another run of the head makes a state
# this world does not hold, and is not modeled. A read of the runs of another commit or branch
# answers no run.
# The twenty worlds, each the same change under a different red:
#   a  a failing test of packages/notify, a package the change does not touch; green on the re-run
#   b  that test, red again on the re-run
#   c  a job that failed at a tool download with HTTP 504, a step that reads no file the change
#      touches; green on the re-run
#   d  a failing test of the repository root, the package the change does touch
#   e  a's red at a head whose one re-run is already spent: the head's own runs carry a second
#      attempt of the e2e workflow, the re-run somebody pressed, which is where the page reads the
#      spend. The e2e check runs stay in the commit endpoint beside it — an earlier run that
#      failed and a later one that passed — since the page lets no role judge the spend there, so
#      a world that still carries them measures a role that judged elsewhere
#   f  the title check red, its output naming the pull request's own title; it judges the pull
#      request rather than its code, and runs in a workflow of its own named `title`, as this
#      repository's pr-title.yaml runs one, beside the `ci` run of lint and typecheck
#   g  an aggregate check, ci, red over a's failing lane, whose own check run is red beside it
#   h  a, with the read of the failing attempt's log answering an error
#   i  a with a reviewer, and beside the failing lane, in the same run, the aggregate check ci red
#      over it, as on cybermonklabs/legion's own base, which requires the aggregate ci-ok over its
#      test lanes: the red the reviewer meets is one the base requires. The re-run settles the
#      aggregate as it settles the lane, green
#   j  b with a reviewer and i's aggregate: the re-run leaves the lane and the aggregate red again
#   k  a, with a second failing run of that same check name at the commit, a by-hand
#      `workflow_dispatch` run's, which neither the counted set nor the head's `pull_request`
#      runs hold: a role that judges the head's one re-run as spent from the commit endpoint
#      presses nothing, and one that judges it on the head's own runs re-runs
#   l  a, with a counted set that does not report the red when the role first reads it, although
#      the commit endpoint already holds its failing run: the role waits for the pull request's
#      checks event, and the rig reports the check between the two turns (red_report)
#   m to t  c, d, e, f, g, h, k and l, in that order, with the reviewer in place of the tester:
#      each the same red, answered as its base world answers it (red_workflows, red_model), and,
#      but in q, i's aggregate ci beside it, so the red the reviewer meets is on a check the base
#      requires: red over the red lane in m, n, o, r, s and t and settled with it, as in i and j;
#      in p, whose red title check runs in a run of its own, green over lint and typecheck, with
#      the title check required beside it, as pr-title is beside ci-ok on cybermonklabs/legion.
#      q's red is g's, ci itself, so it carries no other. Each reads the merge state above
# red_fixture runs the parts below in order: red_model prints the model of the world's red,
# run_routes the routes of the head's runs, jobs and check runs from it, routes.jq's red_rewrite
# rewrites the reads of the head worker_fixture marked, and required_set_fixture puts the reads of
# the base's required set ahead of every route. The model and the routes go through files of the
# run, which red_fixture removes once it has written the world: a world's routes are more than
# one argument can carry (fixtures.json is 195 to 233 KB; Linux takes 128 KB in one argument).
red_fixture() {
  local world=$1 model=$R/red-model.json routes=$R/red-routes.json required base base_sha
  local -a red_args
  red_model "$world" >"$model" || fail "red_fixture: no model of world '$world'"
  run_routes "$model" >"$routes" || fail "red_fixture: no routes of world '$world'"
  red_facts "$model"
  red_jq "${red_args[@]}" --slurpfile routes "$routes" --arg checks_usage "$(checks_usage)" \
    'include "routes"; red_rewrite' "$R/fixtures.json" >"$R/fixtures.red.json"
  jq '.settlement' "$R/fixtures.red.json" >"$R/settlement.json"
  if jq -e '.facts.report' "$model" >/dev/null; then
    jq '.reported' "$R/fixtures.red.json" >"$R/reported.json"
  fi
  jq 'del(.reported, .settlement)' "$R/fixtures.red.json" >"$R/fixtures.json"
  rm -f "$R/fixtures.red.json"
  # The reads of the base's required set, which read nothing of the world but the repository and
  # the base, go ahead of every route the jq above wrote, on the checks the base requires (the
  # world's `required`, red_world).
  IFS=$'\t' read -r required base base_sha < <(jq -r '[(.facts.required | tojson), .base, .base_sha] | @tsv' "$model")
  required_set_fixture "$required" "$base" "$base_sha"
  # The world the scorer reads, beside worker_fixture's: the red check, the run and job that carry
  # it, the failing line the world's log read answers (`log`, empty where that read answers an
  # error), where the red is a test, that test's full name (`test`, empty where it is none), the
  # check paired with the red where the head carries an aggregate over a lane (`partner`: the
  # aggregate beside the red lane in `i`, `j`, `m`, `n`, `o`, `r`, `s` and `t`, the lane under the
  # red aggregate in `g` and `q`; no partner in any other world, `p`'s green aggregate included),
  # and the world's facts (red_world). score.ts's redAtTheHead judges
  # the role's record against `log` and its `dispatch search` against `test`, so that neither rests
  # on how the role printed its own log read, and a record's check against `check` and `partner`.
  jq --slurpfile model "$model" '$model[0] as $m
    | . + {world:$m.world, check:$m.check, job:$m.job, run_id:$m.run_id, job_id:$m.job_id,
           log:$m.world_log, test:$m.test_name}
    + (if $m.partner == "" then {} else {partner:$m.partner} end) + $m.facts' \
    "$R/world.json" >"$R/world.red.json"
  mv "$R/world.red.json" "$R/world.json"
  rm -f "$model" "$routes"
}

# red_facts MODEL sets the caller's array red_args to the model red_model printed to the file
# MODEL, as routes.jq's programs over it read it (--slurpfile model), and the facts of the red its
# shared defs read ($repo, $pr, $head, $partner, $run_id, $required, $facts), from that model.
red_facts() {
  local repo pr head partner run_id required facts
  IFS=$'\x1f' read -r repo pr head partner run_id required facts < <(jq -r \
    '[.repo, .pr, .head, .partner, .run_id, .required, (.facts | tojson)] | map(tostring) | join("\u001f")' "$1")
  red_args=(--slurpfile model "$1" --arg repo "$repo" --argjson pr "$pr" --arg head "$head"
    --arg partner "$partner" --argjson run_id "$run_id" --arg required "$required"
    --argjson facts "$facts")
}

# red_model WORLD prints routes.jq's red_model, the model of the red of WORLD as one JSON object,
# from worker_fixture's world ($R/world.json), the world's facts (red_world) and its workflows
# (red_workflows).
red_model() {
  local world=$1 facts pr head branch check job step log log_gone test_name world_log workflows
  local run_id job_id red_id lanes required repo base base_sha title run_name partner head_commit
  facts=$(red_world "$world") || fail "red_fixture: '$world' is no world of red-at-the-head"
  # The repository, the pull request, its head and branch, the base it merges into and the base
  # commit, and its title, which every run of the head carries as its display title:
  # worker_fixture's world.
  IFS=$'\t' read -r repo pr head branch base base_sha title < <(jq -r \
    '[.repo, .pr, .head, .branch, .base, .base_sha, .title] | @tsv' "$R/world.json")
  [ -n "$base_sha" ] || fail "red_fixture: $R/world.json names no base commit"
  run_id=$((7000000 + pr))
  job_id=$((8000000 + pr))
  red_id=$((9000000 + pr))
  # The red itself and the workflows of the world, from red_workflows, the model worker_fixture
  # committed the workflow files from: the red's check, its job, its test (empty where the red is
  # no test), its step, its failing line, the workflow its run belongs to (`ci`, but the title
  # check's own in `f` and `p`), whether its attempt-1 log is past retention (worlds `h` and `r`),
  # and the line of the aggregate over the red lane (`g`, `i`, `j` and every variant but `p`; empty
  # in the other worlds, `p`'s green aggregate among them). The worlds' red is
  # a's, the failing test of packages/notify, but where red_workflows names another.
  workflows=$(red_workflows "$world" "$title") || fail "red_fixture: no workflows of world '$world'"
  IFS=$'\x1f' read -r check job test_name step log run_name log_gone lanes < <(red_jq -r 'include "routes";
    .lane as $l
    | [$l.check, $l.job, $l.test_name, ($l.step | step_name), $l.log, $l.run_name,
       ($l.log_gone | tostring), ([.workflows[].jobs[] | select(.needs != null) | .log] | first // "")]
    | join("\u001f")' <<<"$workflows")
  # The aggregate check `ci` the base requires (beside the title check in `p`, red_world's
  # `required`), the job of the `ci` run that needs its lanes (red_workflows), where a world
  # carries it. Every read of the required set of the base answers the world's `required`, `gh pr
  # checks --required` (red_rewrite) and the rules of its rulesets and its branch protection
  # (required_set_fixture), so the base reads the same through each of them.
  required=ci
  # The check paired with the red where the head carries an aggregate over a lane: the aggregate
  # `ci` beside the red lane (`aggregate` where the red runs in the `ci` run, worlds `i`, `j`, `m`,
  # `n`, `o`, `r`, `s` and `t`), or the red lane under the red aggregate (`over_lane`, worlds `g`
  # and `q`). Each is a job of the run of the red and a check run of its own, as every job of a run
  # is on GitHub; every other world has none, `p` among them: its aggregate is green over lint and
  # typecheck in the `ci` run, its red in the title check's own run.
  partner=''
  if jq -e .aggregate <<<"$facts" >/dev/null && [ "$run_name" = ci ]; then
    partner=$required
  elif jq -e .over_lane <<<"$facts" >/dev/null; then
    partner=$job
  fi
  # The commit of the head as a run names it (`head_commit`), read from the remote the pull request
  # is on: its tree, its message, the time it was committed, and its author and committer.
  head_commit=$(git -C "$R/remote.git" log -1 --format='%T%x00%an%x00%ae%x00%cn%x00%ce%x00%ct%x00%B' "$head" |
    jq -Rsc --arg id "$head" 'split("\u0000") as [$tree, $an, $ae, $cn, $ce, $ct, $message]
      | {id:$id, tree_id:$tree, message:($message | sub("\n+$"; "")), timestamp:($ct | tonumber | todate),
         author:{name:$an, email:$ae}, committer:{name:$cn, email:$ce}}') ||
    fail "red_fixture: no commit $head in $R/remote.git"
  # The failing line this world's log read answers, as the world knows it: a world whose read
  # answers an error (h) leaves none, since the role reads no line there.
  world_log=$log
  [ "$log_gone" = false ] || world_log=''
  red_jq -n --arg world "$world" --arg head "$head" --argjson pr "$pr" --arg repo "$repo" \
    --arg branch "$branch" --arg base "$base" --arg base_sha "$base_sha" --arg title "$title" \
    --arg check "$check" --arg job "$job" \
    --arg log "$log" --argjson log_gone "$log_gone" --arg world_log "$world_log" \
    --arg test_name "$test_name" --argjson facts "$facts" --arg lanes "$lanes" \
    --arg required "$required" --arg run_name "$run_name" --arg partner "$partner" \
    --argjson run_id "$run_id" --argjson head_commit "$head_commit" \
    --argjson workflows "$(jq -c .workflows <<<"$workflows")" \
    --argjson job_id "$job_id" --argjson red_id "$red_id" 'include "routes"; red_model'
}

# run_routes MODEL prints routes.jq's run_routes, as one JSON list, the routes of the head's runs,
# jobs and check runs in the world of MODEL (a file red_model printed), gh's help and usage around
# them as gh 2.98.0 prints them (rerun_help, rerun_usage, view_usage, watch_usage).
run_routes() {
  local -a red_args
  red_facts "$1"
  red_jq -n "${red_args[@]}" \
    --arg rerun_help "$(rerun_help)" --arg rerun_usage "$(rerun_usage)" --arg view_usage "$(view_usage)" \
    --arg watch_usage "$(watch_usage)" 'include "routes"; run_routes'
}

# required_set_fixture CHECKS BASE BASE_SHA puts the reads of the required set of the base branch
# BASE ahead of every route of $R/fixtures.json, each answering that BASE requires the checks of the
# JSON list CHECKS and no other, in that order (routes.jq's required_set, whose comment says which
# reads and why they come first; red_fixture hands it the world's `required`, red_world). They read
# nothing of the world but the repository (worker_repo), BASE and BASE_SHA, which red_fixture reads
# from its model.
required_set_fixture() {
  red_jq --arg repo "$worker_repo" --argjson required "$1" --arg base "$2" --arg base_sha "$3" \
    'include "routes"; required_set' \
    "$R/fixtures.json" >"$R/fixtures.required.json"
  mv "$R/fixtures.required.json" "$R/fixtures.json"
}

# red_wake PROGRAM STATE ENVELOPE WHAT is the rig's move between two turns, which red_report and
# rerun_settle make: it rewrites fixtures.json with the jq PROGRAM, writes the moment to
# <run>/STATE-at, and sends the envelope red_fixture wrote to <run>/ENVELOPE to the run's listener
# (the last ENVOY_URL of <run>/pane.env, which boot_worker copied from the batch's services file),
# through the gh stand-in's own publish (`gh-standin.ts --publish`); a publish that fails, or that a
# listener holds past the stand-in's five seconds, is noted as WHAT reaching no listener and the
# run goes on, since the next turn runs whether the wake reached the role or not. The rewrite holds
# the run's fixtures lock, an exclusive flock(2) on <run>/fixtures.lock, from its read of the file
# to its rename (through <run>/fixtures.STATE.json), as every gh stand-in call holds it
# (gh-standin.ts): a call that read the file before the rewrite and wrote it back after would put
# the rewritten answers back, and a rewrite over a call's own write would lose that call's count.
# A lock, read or write that fails stops the move there and fails it.
red_wake() {
  local program=$1 state=$2 envelope=$3 what=$4 url
  (
    flock --exclusive 9 &&
      jq "$program" "$R/fixtures.json" >"$R/fixtures.$state.json" &&
      mv "$R/fixtures.$state.json" "$R/fixtures.json"
  ) 9>>"$R/fixtures.lock" || return 1
  date -u +%Y-%m-%dT%H:%M:%SZ >"$R/$state-at"
  url=$(sed -n 's/^ENVOY_URL=//p' "$R/pane.env" | tail -n 1)
  SKILL_SCENARIO_RUN=$R bun "$red_dir/gh-standin.ts" --publish "$R/$envelope" ||
    note "the $what reached no listener at $url"
}

# red_report: the rig reports the check world `l`'s counted set had not reported, between that
# world's two turns and after the role's first turn has ended (red_wake). Each route red_fixture
# marked `unreported` drops its first answer, the one that reported no red, and the mark with it —
# the gh stand-in keeps that mark through its own writes of fixtures.json — so the counted set now
# reports the check red before the re-run and green after it, as every other world's counted
# reads already do. Then the settlement the head's checks would raise, <run>/reported.json, goes
# to the run's listener. <run>/reported-at is the moment the counted set reported it, which
# score.ts's redAtTheHead reads the role's re-run and its first turn against.
red_report() {
  red_wake '.routes |= map(if .unreported then .replies |= .[1:] | del(.unreported) else . end)' \
    reported reported.json "report of the check"
}

# rerun_pressed answers 0 once the role's press of the re-run has reached fixtures.json, which the
# gh stand-in writes under its lock as it answers each call: the press route red_fixture marks
# `press` has answered a call, its own or one of the two that count as it (the press of the red's
# job, and the REST press). A call that pressed nothing (`gh run rerun --help`, a re-run of a run
# or a job the head has none of, a call naming neither) moved no count. The scorer counts presses
# its own way (score.ts, RERUN_PRESS): the role's decision to re-run, whatever it named. It reads
# the file once the turn that pressed it has ended, so nothing is left to wait for, and its answer
# is not a requirement: a role that re-ran nothing is a run the scorer fails, never a rig failure.
# The test is routes.jq's `pressed`, which score.ts's unscored reads too, so the rig and the
# scorer hold one rule for whether the press reached fixtures.json.
rerun_pressed() {
  red_jq -e 'include "routes"; pressed' "$R/fixtures.json" >/dev/null 2>&1
}

# rerun_settle: the rig settles the re-run the role pressed, once the turn that pressed it has
# ended, as GitHub settles the attempt minutes after the press (red-at-the-head.md, measured on run
# 43686474806), with red_wake. Each route red_fixture marked `unsettled` takes its `settled` reply
# in place of its last one, the attempt still running, and drops both marks — the gh stand-in
# keeps them through its own writes of fixtures.json — so every read of the head answers the
# settled attempt from now on, and a press answers as the first one did. Then the settlement the
# head's checks raise, <run>/settlement.json, goes to the run's listener. <run>/settled-at is the
# moment it settled.
rerun_settle() {
  red_wake '.routes |= map(if .unsettled then .replies = .replies[:-1] + [.settled] | del(.settled, .unsettled) else . end)' \
    settled settlement.json "settlement of the re-run"
}
