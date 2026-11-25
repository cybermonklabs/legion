#!/usr/bin/env bash
# Tests for `check-private-names.sh` and `check-pr-text.sh`: every spelling the guard refuses, the
# identifiers and the English word it must pass, what it skips, its two modes (the repository, and
# named paths such as the built site), file names, and a pull request's title, body, branch name,
# commits (message, author and committer), issue comments, reviews and review comments. The
# deployment's own term list, the script's fifth rule, has a harness of its own,
# check-private-names-terms.test.sh, and so does the issue-key rule,
# check-private-names-keys.test.sh; the fixtures, the name pieces and the stand-in GitHub API they
# read are check-private-names-test-lib.sh's.
#
# Every case names itself by an index or a neutral label, never the assembled form, since
# `check`'s PASS/FAIL line is this script's own stdout — the CI log; the last case runs the guard
# over that captured log.
#
# Run from anywhere: .github/scripts/check-private-names.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit
# No tracing: a traced run would print the placeholder terms the library sets.
set +x

# shellcheck source=.github/scripts/check-private-names-test-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-private-names-test-lib.sh"

# assert_each_refused <fixture-prefix> <label> <array-name> [line]: writes each line of the named
# array into its own fixture, on the given line (1 by default; 2 is preceded by a filler line), and
# checks the guard refuses it with that label there. Each case is named by its index, never the
# assembled form.
assert_each_refused() {
  local prefix=$1 label=$2 index
  local -n lines=$3
  local line=${4:-1}
  for index in "${!lines[@]}"; do
    root=$(fixture "$prefix-$index")
    if [ "$line" = 2 ]; then
      printf 'line one\n%s\n' "${lines[index]}" > "$root/docs/notes.md"
    else
      printf '%s\n' "${lines[index]}" > "$root/docs/notes.md"
    fi
    run_check "$root"
    check "$prefix-$index fails" "$(is "$status" 1)"
    check "$prefix-$index is named at its file and line" \
      "$(contains "$out" "docs/notes.md:$line: $label")"
  done
}

echo "case: a clean repository"
root=$(fixture clean)
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: every spelling of the deployment repository and its project key is refused"
mentions=(
  "the $repo repository"
  "see $repo's README"
  "${repo}#20006"
  "$repo_snake as a constant"
  "the $repo_joined checkout"
  "Agent""-C, capitalized"
  "issue $key-393"
  "dispatch://$key-1/ask/1"
  "project: \"$key\""
  "LEGION_PROJECT=$repo_joined"
  "ses_${repo_joined}_controller_title"
  "${key}_TOKEN"
  "the $repo_spaced repository"
  "the $repo_en_dash repository"
  "the $repo_nonbreaking_hyphen repository"
  "the $repo_percent_hyphen repository"
  "https://github.com/search?q=repo%3Aexample-org%2F$repo"
  "?q=the%20$repo_joined%20repo"
  "the agent${nbsp}c repository"
  "the agent""  c repository"
)
assert_each_refused repo "names the private deployment repository" mentions 2

echo "case: every spelling of the company is refused"
# shellcheck disable=SC2034 # read by name through assert_each_refused's nameref
mentions=(
  "$company_proper Labs"
  "$company-$labs-example/repo"
  "${company}_$labs"
  "https://dispatch.internal.$company$labs.com/"
  "$company.$labs"
  "${company_proper}${labs^}"
  "the $company_proper team"
  "($company_proper)"
  "$company$en_dash$labs"
  "$company%20$labs"
  "$company$nbsp$labs"
  "$company  $labs"
  "see the ${company^^} docs"
  "${company^^}_AWS_PROFILE=x"
  "${company_proper}_API_KEY=x"
  "MY_${company^^}_KEY=x"
)
assert_each_refused company "names the company" mentions

echo "case: a private internal host is refused, whatever its case"
# shellcheck disable=SC2034 # read by name through assert_each_refused's nameref
hosts=(
  "https://listener.$internal.$private_host_suffix"
  "https://listener.${internal^^}.$private_host_suffix"
  "https://listener.${internal^}.$private_host_suffix"
  "https://listener.$internal.${example_host_suffix}corp.net"
  "https://listener.$internal.$example_host_suffix.$private_host_suffix"
  "https://a.$internal.$example_host_suffix and https://b.${internal^^}.$private_host_suffix"
  "this.$internal.push(value)"
  "https://LISTENER.${internal^^}.${private_host_suffix^^}"
  "https://listener.$internal.${private_host_suffix^}"
  "listener.$internal.$private_host_suffix"
  "see _listener.$internal.${private_host_suffix}_ in italics"
  "NATS_URLS=a.$internal.$example_host_suffix,b.$internal.$private_host_suffix"
  "Listener.$internal.$private_host_suffix answers on 443."
  "LISTENER.$internal.$private_host_suffix"
  "fooBar.$internal.$private_host_suffix"
  "Listener.${internal^^}.$private_host_suffix"
  "Lean.$internal.otherName"
  "Lean.$internal.cmdlineSnapshotsX.get opts"
  "xLean.$internal.cmdlineSnapshots"
  "Lean.$internal.cmdlineSnapshots.get.listener"
)
assert_each_refused host "names a private internal host" hosts

echo "case: a reserved example host passes, the vendored NATS client's property in its own file, and" \
  "Lean and OCaml names"
root=$(fixture public-host)
{
  printf 'https://listener.%s.%s\n' "$internal" "$example_host_suffix"
  printf 'nats://nats.%s.%s.com:4222\n' "$internal" "$example_host_suffix"
  printf 'Point it at api.%s.%s.\n' "$internal" "$example_host_suffix"
  printf 'listener.%s.%s at the start of a line\n' "$internal" "$example_host_suffix"
  printf 'now https://ENVOY.%s.%s\n' "${internal^^}" "${example_host_suffix^^}"
  # A host that ends in `.internal` passes a run over the tree: only a pull request's own text
  # is read for that ending (the case of it below).
  printf 'Point it at wobble.%s:4000, https://wobble.%s/x or wobble.%s\n' "$internal" "$internal" "$internal"
} > "$root/docs/notes.md"
mkdir -p "$root/$(dirname "$vendored_nats")"
printf '        this.%s.push(sv);\n' "$internal" > "$root/$vendored_nats"
git -C "$root" add "$vendored_nats"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
# A Lean namespace, OCaml value paths, a Lean function's namespace and Lean's own option of the
# cmdline snapshots, each in its own file: none of them names a host.
namespaces=(
  "import Std.${internal^}.UV.System"
  "  if Environ.${internal^}.is_above_prop env q then"
  "  (Environ.${internal^}.View.view answer_env).Environ.${internal^}.View.env_modtypes"
  "      Classes.${internal^}.add_instance ~locality inst"
  "This module proves the one-step unfolding lemma \`repeatM.${internal^}.eq_of_monadTail\`,"
  "  rw [repeatM.${internal^}.eq_of_monadTail]; unfold repeatM.body; rw [bind_assoc]"
  "    rw [_root_.repeatM.${internal^}.eq_of_monadTail (f := f) a]"
  "  let opts := Lean.$internal.cmdlineSnapshots.setIfNotSet opts incrSaveFileName?.isNone"
)
for index in "${!namespaces[@]}"; do
  root=$(fixture "namespace-$index")
  printf '%s\n' "${namespaces[index]}" > "$root/docs/notes.md"
  run_check "$root"
  check "namespace-$index passes" "$(is "$status" 0)"
done

echo "case: a Tailscale MagicDNS host is refused, whatever its case"
# shellcheck disable=SC2034 # read by name through assert_each_refused's nameref
tailnet_hosts=(
  "https://nats.$tailnet_label.$tailnet_suffix:4222"
  "https://nats.${tailnet_label^^}.$tailnet_suffix:4222"
  "https://nats.$tailnet_label.${tailnet_suffix^^}"
  "https://a.$tailnet_example.$tailnet_suffix and https://b.$tailnet_label.$tailnet_suffix"
  "see _nats.$tailnet_label.${tailnet_suffix}_ in italics"
  "NATS_$tailnet_label.$tailnet_suffix"
)
assert_each_refused tailnet "names a Tailscale MagicDNS host" tailnet_hosts

echo "case: the reserved example tailnet passes, under any machine name, and a longer word is not" \
  "mistaken for one"
root=$(fixture public-tailnet)
{
  printf 'https://nats.%s.%s:4222\n' "$tailnet_example" "$tailnet_suffix"
  printf 'https://example-host-mx.%s.%s\n' "$tailnet_example" "$tailnet_suffix"
  printf 'nats://<tailnet>.%s:4222\n' "$tailnet_suffix"
  printf 'nats://<name>.<tailnet>.%s:4222\n' "$tailnet_suffix"
  printf 'host.%s.%swork, a longer word the boundary must not mistake for one\n' \
    "$tailnet_label" "$tailnet_suffix"
} > "$root/docs/notes.md"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: identifiers that merely start with those letters, and the English word, pass"
root=$(fixture lookalikes)
{
  printf 'agentCursor AgentConversationPage subagentContext agentCount agentCalls agentCard\n'
  printf 'leaveAgentComposer AGENT_COMPOSER_SELECTOR agent-composer agent-cursor agent-card agent-credentials\n'
  printf 'agent cards, an agent class, the agents C and D\n'
  printf 'The models may take a different %s in tool calling; %s, plotted. %sPlot.\n' \
    "$company" "$company" "$company_proper"
} > "$root/docs/notes.md"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: the log names the file and line, never the text"
root=$(fixture quiet)
printf 'the %s repository\n' "$repo" > "$root/docs/notes.md"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "annotates the file and line for GitHub" \
  "$(contains "$out" '^::error file=docs/notes.md,line=1::docs/notes.md:1:')"
check "does not repeat the name" "$(is "$(contains "$out" "$repo")" false)"
check "counts the findings" "$(contains "$out" '^check-private-names: 1 finding(s)')"

echo "case: a line naming both is reported once"
root=$(fixture both)
printf '%s, by %s Labs\n' "$repo" "$company_proper" > "$root/docs/notes.md"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "one finding counted" "$(contains "$out" '^check-private-names: 1 finding(s)')"

echo "case: a file git has not been told about still counts; an ignored one does not"
root=$(fixture untracked)
printf '%s\n' "$key-7" > "$root/docs/new.md"
run_check "$root"
check "an untracked file fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'docs/new.md:1:')"

root=$(fixture ignored)
printf 'dist/\n' > "$root/.gitignore"
git -C "$root" add .gitignore
mkdir -p "$root/dist"
printf '%s\n' "$key-7" > "$root/dist/page.html"
run_check "$root"
check "an ignored file passes the repository scan" "$(is "$status" 0)"

echo "case: a file git tracks under an ignored path still counts"
root=$(fixture tracked-ignored)
printf 'notes/\n' > "$root/.gitignore"
git -C "$root" add .gitignore
mkdir -p "$root/notes"
printf '%s\n' "$key-7" > "$root/notes/report.md"
git -C "$root" add -f notes/report.md
run_check "$root"
check "a tracked file under an ignored path fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'notes/report.md:1:')"

echo "case: a name in a file's path is refused, and the log does not print it"
root=$(fixture path-name)
mkdir -p "$root/docs/notes"
printf 'clean text\n' > "$root/docs/notes/$key-7-runbook.md"
git -C "$root" add docs/notes
run_check "$root"
check "a tracked path naming the project key fails" "$(is "$status" 1)"
check "names the directory it is in" "$(contains "$out" 'docs/notes/<name>')"
check "does not print the name" "$(is "$(contains "$out" "$key")" false)"

root=$(fixture path-name-dir)
mkdir -p "$root/docs/${company_proper}"
printf 'clean text\n' > "$root/docs/${company_proper}/notes.md"
git -C "$root" add docs
run_check "$root"
check "a directory naming the company fails" "$(is "$status" 1)"
check "does not print the directory's name" "$(is "$(contains "$out" "$company_proper")" false)"

echo "case: a non-ASCII file name reaches the rules unquoted"
root=$(fixture path-name-dash)
printf 'clean text\n' > "$root/docs/$repo_en_dash.md"
git -C "$root" add docs
run_check "$root"
check "a file named with a typographic dash between the halves fails" "$(is "$status" 1)"
check "masks it" "$(contains "$out" 'docs/<name>')"

root=$(fixture path-unicode)
printf '%s\n' "$key-7" > "$root/docs/café.md"
git -C "$root" add docs
run_check "$root"
check "a hit in a non-ASCII file fails" "$(is "$status" 1)"
check "names the file as it is, unquoted" "$(contains "$out" '^::error file=docs/café.md,line=1::')"

echo "case: lockfiles and binary files are skipped"
root=$(fixture skipped)
mkdir -p "$root/packages/x"
printf '"%s": "sha512-%s"\n' "$repo" "$key" > "$root/bun.lock"
printf 'example.com/%s v1 h1:x\n' "$repo" > "$root/packages/x/go.sum"
printf '%s\0binary\n' "$repo" > "$root/docs/image.bin"
git -C "$root" add bun.lock packages/x/go.sum docs/image.bin
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: named paths are scanned instead of the repository, ignored or not"
root=$(fixture paths)
printf 'dist/\n' > "$root/.gitignore"
mkdir -p "$root/dist/reference"
printf '<p>clean</p>\n' > "$root/dist/index.html"
run_check "$root" dist
check "a clean built tree passes" "$(is "$status" 0)"
check "says what it scanned" "$(contains "$out" '(1 files under dist)')"

printf '<pre>{"project": "%s"}</pre>\n' "$key" > "$root/dist/reference/tools.html"
printf 'WEBVTT\n\n00:00.000 --> 00:01.000\n%s Labs\n' "$company_proper" > "$root/dist/captions.vtt"
run_check "$root" dist
check "a built page and a caption naming them fail" "$(is "$status" 1)"
check "names the page" "$(contains "$out" 'dist/reference/tools.html:1: names the private deployment repository')"
check "names the caption" "$(contains "$out" 'dist/captions.vtt:4: names the company')"

printf '%s\n' "$key-7" > "$root/docs/notes.md"
rm "$root/dist/reference/tools.html" "$root/dist/captions.vtt"
run_check "$root" dist
check "a named path is all that is scanned" "$(is "$status" 0)"

echo "case: a single named file is reported under its own name"
root=$(fixture single-file)
printf 'line one\n%s\n' "$key-7" > "$root/body.md"
run_check "$root" body.md
check "fails" "$(is "$status" 1)"
check "names the file and line" "$(contains "$out" '^::error file=body.md,line=2::body.md:2: names the private deployment repository')"

echo "case: a named path that does not exist"
root=$(fixture missing)
run_check "$root" dist
check "exits 2" "$(is "$status" 2)"
check "names it" "$(contains "$out" 'dist does not exist')"

echo "case: a relative path from a subdirectory is read from that subdirectory"
root=$(fixture relative-subdir)
mkdir -p "$root/work"
printf 'line one\n%s\n' "$key-7" > "$root/work/body.md"
set +e
out=$(cd "$root/work" && ../.github/scripts/check-private-names.sh body.md 2>&1)
status=$?
set -e
check "fails" "$(is "$status" 1)"
check "names the file and line" "$(contains "$out" '^::error file=work/body.md,line=2::work/body.md:2: names the private deployment repository')"

echo "case: a missing relative path from a subdirectory is still refused"
root=$(fixture relative-missing)
mkdir -p "$root/work"
set +e
out=$(cd "$root/work" && ../.github/scripts/check-private-names.sh body.md 2>&1)
status=$?
set -e
check "exits 2" "$(is "$status" 2)"
check "names it" "$(contains "$out" 'work/body.md does not exist')"

echo "case: a relative path from a subdirectory of a checkout reached through a symlink"
root=$(fixture relative-symlinked)
mkdir -p "$root/work"
printf 'line one\n%s\n' "$key-7" > "$root/work/body.md"
ln -s "$root" "$work/relative-symlinked-link"
set +e
out=$(cd "$work/relative-symlinked-link/work" && ../.github/scripts/check-private-names.sh body.md 2>&1)
status=$?
set -e
check "fails" "$(is "$status" 1)"
check "names the file by its repository path" "$(contains "$out" '^::error file=work/body.md,line=2::work/body.md:2: names the private deployment repository')"

echo "case: a symlink before a '..' is followed as the kernel follows it"
root=$(fixture relative-symlink-dotdot)
mkdir -p "$root/work" "$root/elsewhere/deep"
ln -s ../elsewhere/deep "$root/work/link"
printf 'line one\n%s\n' "$key-7" > "$root/elsewhere/body.md"
printf 'Nothing private here.\n' > "$root/work/body.md"
run_check "$root" work/link/../body.md
check "from the root, fails" "$(is "$status" 1)"
check "from the root, names the file the link leads to" "$(contains "$out" '^::error file=work/link/\.\./body\.md,line=2::')"
set +e
out=$(cd "$root/work" && ../.github/scripts/check-private-names.sh link/../body.md 2>&1)
status=$?
set -e
check "from the subdirectory, fails" "$(is "$status" 1)"
check "from the subdirectory, names the file the link leads to" "$(contains "$out" '^::error file=work/link/\.\./body\.md,line=2::')"

echo "case: a relative name starting with '-' is read as a path"
root=$(fixture relative-dash)
mkdir -p "$root/work"
printf 'line one\n%s\n' "$key-7" > "$root/-dash.md"
printf 'line one\n%s\n' "$key-7" > "$root/work/-dash.md"
run_check "$root" -dash.md
check "from the root, fails" "$(is "$status" 1)"
check "from the root, names the file" "$(contains "$out" '^::error file=\./-dash\.md,line=2::')"
set +e
out=$(cd "$root/work" && ../.github/scripts/check-private-names.sh -dash.md 2>&1)
status=$?
set -e
check "from the subdirectory, fails" "$(is "$status" 1)"
check "from the subdirectory, names the file" "$(contains "$out" '^::error file=work/-dash\.md,line=2::')"

echo "case: an empty argument from a subdirectory is refused"
root=$(fixture relative-empty)
mkdir -p "$root/work"
set +e
out=$(cd "$root/work" && ../.github/scripts/check-private-names.sh '' 2>&1)
status=$?
set -e
check "exits 2" "$(is "$status" 2)"
check "names it" "$(contains "$out" '^check-private-names:  does not exist')"

echo "case: a relative path from a directory outside the repository is read from that directory"
root=$(fixture relative-outside)
outside="$work/relative-outside-caller"
mkdir -p "$outside"
printf 'line one\n%s\n' "$key-7" > "$outside/body.md"
set +e
out=$(cd "$outside" && "$root/.github/scripts/check-private-names.sh" body.md 2>&1)
status=$?
set -e
check "fails" "$(is "$status" 1)"
check "names the file by its absolute path" "$(contains "$out" '^::error file=/.*/relative-outside-caller/body\.md,line=2::')"

echo "case: an exported CDPATH does not move the repository root"
root=$(fixture relative-cdpath)
set +e
out=$(cd "$root" && CDPATH=. .github/scripts/check-private-names.sh docs/notes.md 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "scans the file" "$(contains "$out" '(1 files under docs/notes\.md)')"

echo "case: a relative path from a directory whose name ends in a newline"
root=$(fixture relative-newline)
newline_dir="$root/work"$'\n'
mkdir -p "$newline_dir"
printf 'line one\n%s\n' "$key-7" > "$newline_dir/body.md"
set +e
out=$(cd "$newline_dir" && ../.github/scripts/check-private-names.sh body.md 2>&1)
status=$?
set -e
check "fails" "$(is "$status" 1)"
check "names the file's line" "$(contains "$out" '/body\.md:2: names the private deployment repository')"

echo "case: a repository whose directory name ends in a newline"
root=$(fixture relative-newline-root$'\n'; printf x)
root=${root%$'\n'x}
run_check "$root" docs/notes.md
check "exits 0" "$(is "$status" 0)"
check "scans the file" "$(contains "$out" '(1 files under docs/notes\.md)')"

echo "case: a pull request's title, body, branch name and commits, which reach main and its release notes"
run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
check "a clean title, body and commits pass" "$(is "$status" 0)"
check "reads it all in one GraphQL call" "$(is "$(< "$work/calls")" "$(grep '^graphql PrText ' "$work/calls")")"
check "and that is the only call" "$(is "$(wc -l < "$work/calls")" 1)"

run_pr_text "fix(dispatch): keep the inbox order ($key-7)" "Refs LEGION-7" "fix: keep the order"
check "a title naming a deployment issue key fails" "$(is "$status" 1)"
check "names the title" "$(contains "$out" 'title:1: names the private deployment repository')"
check "does not print the key" "$(is "$(contains "$out" "$key")" false)"

run_pr_text "Keep the inbox order" "Refs LEGION-7" "fix: keep the order"
check "a title that is no conventional commit subject fails" "$(is "$status" 1)"
check "says the title must be one" \
  "$(contains "$out" '^::error::PR title must be a conventional commit subject (type(scope): summary)')"
check "does not print the title" "$(is "$(contains "$out" 'Keep the inbox order')" false)"

run_pr_text "feat!: keep the inbox order" "Refs LEGION-7" "fix: keep the order"
check "a breaking change with no scope passes" "$(is "$status" 0)"

echo "case: an edit after the event: the pull request's title and body as they are now are checked"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
jq --arg key "$key" '.title = "fix(dispatch): keep the inbox order (\($key)-7)"' "$work/pull" > "$work/pull.next"
mv "$work/pull.next" "$work/pull"
run_pr_text
check "a title edited to name a deployment issue key fails although the event's was clean" "$(is "$status" 1)"
check "names the title" "$(contains "$out" 'title:1: names the private deployment repository')"
write_pr_text "Keep the inbox order" "Refs LEGION-7" "fix: keep the order"
jq '.title = "fix(dispatch): keep the inbox order"' "$work/pull" > "$work/pull.next"
mv "$work/pull.next" "$work/pull"
run_pr_text
check "a title edited to a conventional one passes although the event's was not" "$(is "$status" 0)"

run_pr_text "fix(dispatch): keep the inbox order" "$(printf 'Line one.\nAs %s Labs found.\n' "$company_proper")" "fix: keep the order"
check "a body naming the company fails" "$(is "$status" 1)"
check "names the body's line" "$(contains "$out" 'body:2: names the company')"

run_pr_text "fix(dispatch): keep the inbox order" "" "fix: keep the order"
check "an empty body passes" "$(is "$status" 0)"

run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order" "fix/$key-7"
check "a branch naming a deployment issue key fails" "$(is "$status" 1)"
check "names the branch" "$(contains "$out" 'branch:1: names the private deployment repository')"
check "does not print the key" "$(is "$(contains "$out" "$key")" false)"

run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" \
  "$(printf 'fix: keep the order\n\nCo-authored-by: A <a@%s%s.example>\n' "$company" "$labs")"
check "a commit message naming the company fails" "$(is "$status" 1)"
check "names the commit messages' line" "$(contains "$out" 'commits:3: names the company')"

run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order" "fix/keep-the-order" \
  "a@$company$labs.example"
check "a commit's author email naming the company fails, with a clean message" "$(is "$status" 1)"
check "names the commits line" "$(contains "$out" 'commits:3: names the company')"
check "does not print the domain" "$(is "$(contains "$out" "$company$labs")" false)"

run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order" "fix/keep-the-order" \
  "dev@example.com" "a@$company$labs.example"
check "a commit's committer email naming the company fails, with a clean message and author" "$(is "$status" 1)"
check "names the commits line" "$(contains "$out" 'commits:5: names the company')"

echo "case: the commits of the head the run checks, when a later push lands while it runs"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
later_commit=$(pr_commit "fix: a later push ($key-7)")
jq -c --argjson later "$later_commit" '. + [$later]' "$work/pr-commits" > "$work/pr-commits.next"
mv "$work/pr-commits.next" "$work/pr-commits"
run_pr_text
check "a pull request whose head moved on mid-run passes" "$(is "$status" 0)"

# The same commit, this time on the head the run checks: the case above passes because the script
# read that head, not because the later push's name is one the guard would have let through.
jq -c --argjson later "$later_commit" '{total_commits: 2, commits: (.commits + [$later])}' \
  "$work/compare" > "$work/compare.next"
mv "$work/compare.next" "$work/compare"
run_pr_text
check "that same commit, on the run's own head, fails" "$(is "$status" 1)"
check "names its line" "$(contains "$out" 'commits:6: names the private deployment repository')"
check "does not print the key" "$(is "$(contains "$out" "$key")" false)"

echo "case: a commit list GitHub cut short, and one spread over pages"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
jq -c '.total_commits = 300' "$work/compare" > "$work/compare.next"
mv "$work/compare.next" "$work/compare"
run_pr_text
check "a list shorter than the route's own total fails loudly" "$(is "$status" 1)"
check "names both counts" "$(contains "$out" 'reports 300 commits .* but listed 1')"
check "says the compare route read it once GitHub's GraphQL comparison came back short" \
  "$(contains "$out" 'listed 1 of its 300 commits, so the compare route reads them instead')"

write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
second_commit=$(pr_commit "$(printf 'fix: keep the order\n\nCo-authored-by: A <a@%s%s.example>\n' "$company" "$labs")")
jq -c '.total_commits = 2' "$work/compare" > "$work/compare.next"
jq -nc --argjson second "$second_commit" '{total_commits: 2, commits: [$second]}' >> "$work/compare.next"
mv "$work/compare.next" "$work/compare"
run_pr_text
check "a commit on the comparison's second page naming the company fails" "$(is "$status" 1)"
check "names its line, counting the first page's commit" "$(contains "$out" 'commits:8: names the company')"
check "counts both pages' commits as the whole list" "$(is "$(contains "$out" 'but listed')" false)"
check "reads the comparison's second page by its cursor" \
  "$(contains "$(< "$work/calls")" '^graphql PrText .*"commitsAfter":"1"')"
check "and from GraphQL alone" "$(is "$(contains "$(< "$work/calls")" '^api ')" false)"

echo "case: a comparison GitHub's GraphQL API lists short of its total, as it does past 1000 commits"
# GitHub's GraphQL comparison stops at 1000 commits and says no more follow while its total counts
# them all (pull request 4259 of this repository on 2026-11-22: 1000 listed of its 1872). The
# compare route serves them all.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
jq -c '.total_commits = 2' "$work/compare" > "$work/compare.next"
jq -nc --argjson second "$second_commit" '{total_commits: 2, commits: [$second]}' >> "$work/compare.next"
mv "$work/compare.next" "$work/compare"
echo 1 > "$work/cut.commits"
run_pr_text
check "a commit past GraphQL's cut naming the company still fails" "$(is "$status" 1)"
check "names its line" "$(contains "$out" 'commits:8: names the company')"
check "says the compare route read them instead" \
  "$(contains "$out" '^check-pr-text: GitHub.s GraphQL comparison of main with event-head-sha listed 1 of its 2 commits, so the compare route reads them instead')"
check "reads the compare route from the event's base to its head" \
  "$(contains "$(< "$work/calls")" '^api repos/example/repo/compare/base-sha...event-head-sha?per_page=100 --paginate$')"
# GitHub answers a comparison it cannot make with an error, null at its path, on which gh fails: the
# run is refused by name, never passed on no commits.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
touch "$work/compare-not-found"
run_pr_text
check "a comparison GitHub cannot make is refused" "$(is "$status" 1)"
check "names the read and gh's message" \
  "$(contains "$out" "^::error::GitHub's GraphQL API did not answer the PrText read of #7 (gh: Could not resolve to a commit.)")"
# gh-read.sh reads the rate_limit route after a read that fails with no answer it can read, so the
# row looks for the compare route itself.
check "reads no compare route" "$(is "$(contains "$(< "$work/calls")" '^api repos/example/repo/compare/')" false)"
# A total above 1000 goes to the compare route after the first page: the pages GraphQL would serve
# after it cannot make the list whole.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
jq -c --argjson second "$second_commit" '[range(1000) as $i | .commits[0]] + [$second]
  | [range(0; length; 100) as $i | .[$i:$i + 100]][] | {total_commits: 1001, commits: .}' \
  "$work/compare" > "$work/compare.next"
mv "$work/compare.next" "$work/compare"
run_pr_text
check "a 1001st commit naming the company fails" "$(is "$status" 1)"
check "names its line" "$(contains "$out" 'commits:5003: names the company')"
check "reads one page of the comparison, then the compare route" \
  "$(is "$(grep -c '^graphql PrText ' "$work/calls") $(grep -c '^api repos/example/repo/compare/' "$work/calls")" '1 1')"
check "says why" "$(contains "$out" 'listed 100 of its 1001 commits, so the compare route reads them instead')"

echo "case: a compare answer the script cannot check against a total"
# The compare route is read once GitHub's GraphQL comparison comes back short, here a total of 1001
# with one commit listed; the route's own answer is `$work/compare-rest`.
short_comparison() {
  write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
  cp "$work/compare" "$work/compare-rest"
  jq -c '.total_commits = 1001' "$work/compare-rest" > "$work/compare"
}
short_comparison
jq -nc '{commits: []}' > "$work/compare-rest"
run_pr_text
check "an answer carrying no total_commits fails" "$(is "$status" 1)"
check "names the cause" "$(contains "$out" 'total_commits, so the commit list cannot be checked')"

short_comparison
: > "$work/compare-rest"
run_pr_text
check "an empty answer fails the same way" "$(is "$status" 1)"
check "names the cause" "$(contains "$out" 'total_commits, so the commit list cannot be checked')"

# A total of the wrong type, with fewer commits listed than it claims: `[`'s integer test would
# fail inside the guard's `if`, where set -e does not apply, and skip the guard.
for total in '"300"' 300.5; do
  short_comparison
  jq -c --argjson total "$total" '.total_commits = $total' "$work/compare-rest" > "$work/compare.next"
  mv "$work/compare.next" "$work/compare-rest"
  run_pr_text
  check "a total_commits of $total fails" "$(is "$status" 1)"
  check "names the cause" "$(contains "$out" 'total_commits, so the commit list cannot be checked')"
done

# The same answers from GitHub's GraphQL comparison are no page of its commits, refused by name.
for answer in '{"commits": []}' '{"total_commits": "300", "commits": []}' '{"total_commits": 300.5, "commits": []}'; do
  write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
  printf '%s\n' "$answer" > "$work/compare"
  run_pr_text
  check "a comparison of $answer fails" "$(is "$status" 1)"
  check "names the list" \
    "$(contains "$out" "^::error::GitHub's GraphQL API answered no page 1 of the commits from main to event-head-sha")"
  check "and reads no compare route" "$(is "$(contains "$(< "$work/calls")" '^api ')" false)"
done

long_body=$(printf '%.0s–' {1..50000})
run_pr_text "fix(dispatch): keep the inbox order" "$long_body" "fix: keep the order"
check "a body larger than one environment variable may hold is read" "$(is "$status" 0)"

echo "case: a pull request's issue comments, reviews and review comments, public on its page"
clean_pr
items 101 "Looks good." 102 "$(printf 'Two lines.\nThe second is clean too.')" > "$work/comments.1"
items 201 "Approved." 202 "" > "$work/reviews.1"
jq -c '.[1].body = null' "$work/reviews.1" > "$work/reviews.next" && mv "$work/reviews.next" "$work/reviews.1"
items 301 "A nit on this line." > "$work/review-comments.1"
run_pr_text
check "a clean set of comments, reviews (one with no body) and review comments passes" "$(is "$status" 0)"
check "reads them in one GraphQL call" "$(is "$(grep -c '^graphql PrText ' "$work/calls")" 1)"
check "asks for the issue comments" "$(contains "$(< "$work/calls")" '^graphql PrText .*"comments":true')"
check "asks for the reviews" "$(contains "$(< "$work/calls")" '^graphql PrText .*"reviews":true')"
check "asks for the review threads, each with its review comments" \
  "$(contains "$(< "$work/calls")" '^graphql PrText .*"threads":true')"
check "makes no REST call" "$(is "$(contains "$(< "$work/calls")" '^api ')" false)"

clean_pr
items 101 "Looks good." 102 "$(printf 'Line one.\nAs %s Labs found.\n' "$company_proper")" > "$work/comments.1"
run_pr_text
check "an issue comment naming the company fails" "$(is "$status" 1)"
check "names the comment by its id and line" "$(contains "$out" 'comment:102:2: names the company')"
check "names no other comment" "$(is "$(contains "$out" 'comment:101')" false)"
check "does not print the name" "$(is "$(contains "$out" "$company_proper")" false)"
check "says how to take a comment down" "$(contains "$out" 'delete each comment')"

clean_pr
items 201 "Approved." 202 "Blocking: see $key-7." > "$work/reviews.1"
run_pr_text
check "a review body naming a deployment issue key fails" "$(is "$status" 1)"
check "names the review by its id and line" "$(contains "$out" 'review:202:1: names the private deployment repository')"
check "does not call it a review comment" "$(is "$(contains "$out" 'review-comment:202')" false)"
check "does not print the key" "$(is "$(contains "$out" "$key")" false)"

clean_pr
items 301 "A nit." 302 "$(printf 'Nit.\n\nSee https://listener.%s.%s/x\n' "$internal" "$private_host_suffix")" > "$work/review-comments.1"
run_pr_text
check "a review comment naming a private host fails" "$(is "$status" 1)"
check "names the review comment by its id and line" "$(contains "$out" 'review-comment:302:3: names a private internal host')"
check "does not print the host" "$(is "$(contains "$out" "$private_host_suffix")" false)"
check "says how to take a review comment down" "$(contains "$out" 'delete each comment and review comment')"

echo "case: a pull request's own text refuses a host that ends in .internal, which the tree passes"
# Each form of the ending in the body, one a line: a port, a path, a blank, a quote, a bracket, a
# sentence's full stop, and the end of the line; and one in the title, a commit message, a comment,
# a review and a review comment.
bare="wobble.$internal"
bare_forms=(
  "Point it at $bare:4000 now."
  "See https://$bare/x for it."
  "Reach $bare over the VPN."
  "The host is \"$bare\" today."
  "(or $bare)"
  "Reach it at $bare. Then go."
  "It answers at $bare"
)
write_pr_text "fix(dispatch): reach $bare:4000" "$(printf '%s\n' "${bare_forms[@]}")" \
  "fix: keep the order at https://$bare/x"
items 801 "Seen at $bare:4000." > "$work/comments.1"
items 802 "Blocking: $bare/x answers." > "$work/reviews.1"
items 803 "Nit: ($bare)" > "$work/review-comments.1"
run_pr_text
check "fails" "$(is "$status" 1)"
for index in "${!bare_forms[@]}"; do
  check "body form $index is named at its line" "$(contains "$out" "body:$((index + 1)): names a private internal host")"
done
check "names the title" "$(contains "$out" 'title:1: names a private internal host')"
check "names the commit message" "$(contains "$out" 'commits:1: names a private internal host')"
check "names the comment" "$(contains "$out" 'comment:801:1: names a private internal host')"
check "names the review" "$(contains "$out" 'review:802:1: names a private internal host')"
check "names the review comment" "$(contains "$out" 'review-comment:803:1: names a private internal host')"
check "counts the twelve" "$(contains "$out" '^check-private-names: 12 finding(s)')"
# The two well-known names of that shape pass, and so does the reserved example host; a longer
# name holding one of the two, and a label glued to the ending, are refused.
write_pr_text "fix(dispatch): keep the inbox order" \
  "$(printf 'http://metadata.google.%s/computeMetadata/v1/\nhost.docker.%s:8080\nwobble.%s.%s:4000\nwobble.%sx is one word\n' \
    "$internal" "$internal" "$internal" "$example_host_suffix" "$internal")" "fix: keep the order"
run_pr_text
check "the two well-known names, the example host and a longer word pass" "$(is "$status" 0)"
write_pr_text "fix(dispatch): keep the inbox order" \
  "$(printf 'x-metadata.google.%s/\na.host.docker.%s:8080\n' "$internal" "$internal")" "fix: keep the order"
run_pr_text
check "a longer name that holds one of them fails" "$(is "$status" 1)"
check "names both lines" \
  "$(is "$(contains "$out" 'body:1: names a private')$(contains "$out" 'body:2: names a private')" truetrue)"
# A label starts with a letter or a digit, and a `_` ends a host only with no letter or digit after
# it: a shell default that names the metadata server, and an identifier that runs on past the
# ending, pass; a host behind a `_` or a `-`, or followed by one, is refused, and so is a namespace
# segment with no full stop after it, while one with a full stop passes.
dollar='$'
write_pr_text "fix(dispatch): keep the inbox order" \
  "$(printf '%s\n' "${dollar}{GCE_METADATA_HOST:-metadata.google.$internal}" "set nats.${internal}_url first" \
    "import Std.${internal^}.UV.System" "it ends with Std.${internal^}." "see -metadata.google.$internal:80")" \
  "fix: keep the order"
run_pr_text
check "a shell default and a lone hyphen before the metadata server, an identifier and two namespaces pass" \
  "$(is "$status" 0)"
bare_edges=(
  "see _${bare}_ there"
  "see -$bare:80 there"
  "vm-1.c.$bare--then"
  "see Std.${internal^} here"
)
write_pr_text "fix(dispatch): keep the inbox order" "$(printf '%s\n' "${bare_edges[@]}")" "fix: keep the order"
run_pr_text
check "the edge forms fail" "$(is "$status" 1)"
for index in "${!bare_edges[@]}"; do
  check "edge form $index is named at its line" "$(contains "$out" "body:$((index + 1)): names a private internal host")"
done
root=$(fixture bare-host-value)
PRIVATE_NAME_BARE_HOSTS=maybe run_check "$root"
check "a PRIVATE_NAME_BARE_HOSTS that is neither yes nor no exits 2" "$(is "$status" 2)"
check "and says so" "$(contains "$out" '^check-private-names: PRIVATE_NAME_BARE_HOSTS must be yes or no')"

clean_pr
printf 'A NUL here:\0 the %s repository\n' "$repo" > "$work/comment-body"
jq -nc --rawfile body "$work/comment-body" --argjson author "$app" '[{id: 103, body: $body} + $author]' \
  > "$work/comments.1"
run_pr_text
check "a comment holding a NUL byte is still read" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'comment:103:1: names the private deployment repository')"

echo "case: comment lists GitHub serves over several pages"
clean_pr
jq -nc --argjson author "$app" '[range(1; 101) | {id: (1000 + .), body: "fine"} + $author]' > "$work/comments.1"
items 1101 "fine" 1102 "$company_proper Labs" > "$work/comments.2"
run_pr_text
check "a comment on the second page naming the company fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'comment:1102:1: names the company')"
check "reads the second page by its cursor" \
  "$(contains "$(< "$work/calls")" '^graphql PrText .*"comments":true.*"commentsAfter":"1"')"
check "stops at the second page, which says none follow" "$(is "$(grep -c '^graphql PrText ' "$work/calls")" 2)"
check "asks the second round for the comments alone" \
  "$(contains "$(sed -n 2p "$work/calls")" '"commits":false,"history":false,"comments":true,"reviews":false,"threads":false')"

clean_pr
jq -nc --argjson author "$app" '[range(1; 101) | {id: (2000 + .), body: "fine"} + $author]' > "$work/reviews.1"
run_pr_text
check "a full first page of reviews with no second passes" "$(is "$status" 0)"
check "reads it in one call, GitHub saying no more follow" "$(is "$(grep -c '^graphql PrText ' "$work/calls")" 1)"
items 2101 "fine" 2102 "Blocking: see $key-7." > "$work/reviews.2"
run_pr_text
check "a review on the second page naming a deployment issue key fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'review:2102:1: names the private deployment repository')"

clean_pr
jq -nc --argjson author "$app" '[range(1; 101) | {id: (3000 + .), body: "fine"} + $author]' > "$work/review-comments.1"
items 3101 "fine" 3102 "$(printf 'Nit.\n\nthe %s repository\n' "$repo")" > "$work/review-comments.2"
run_pr_text
check "a review comment in a review thread on the second page of threads fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'review-comment:3102:3: names the private deployment repository')"
check "reads the second page of threads by its cursor" \
  "$(contains "$(< "$work/calls")" '^graphql PrText .*"threadsAfter":"1"')"

clean_pr
jq -nc --argjson author "$app" --arg body "$company_proper Labs" \
  '[[range(1; 102) | {id: (4000 + .), body: (if . == 101 then $body else "fine" end)} + $author]]' \
  > "$work/review-comments.1"
run_pr_text
check "the 101st review comment of one review thread naming the company fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'review-comment:4101:1: names the company')"
check "reads that thread's comments past its first 20 by the thread's id" \
  "$(contains "$(< "$work/calls")" '^graphql ThreadComments {"thread":"thread-1-0","after":"1"}$')"
check "and names none of the first 100" "$(is "$(contains "$out" 'review-comment:40[0-9][0-9]:')" false)"

# A thread's first page holds 20 comments, so one answer holds at most 2,000 bodies; the rest come
# 100 a page, here over two more reads. The stand-in pages a thread's first comments by the size
# the query asks for, so a query that asked for 100 would read this thread in one more read.
clean_pr
jq -nc --argjson author "$app" --arg body "$company_proper Labs" \
  '[[range(1; 126) | {id: (4000 + .), body: (if . == 21 or . == 125 then $body else "fine" end)} + $author]]' \
  > "$work/review-comments.1"
run_pr_text
check "the 21st and 125th review comments of one thread naming the company fail" \
  "$(is "$(contains "$out" 'review-comment:4021:1: names the company')$(contains "$out" 'review-comment:4125:1: names the company')" truetrue)"
check "reads that thread's two later pages by its id, the first page having held 20" \
  "$(is "$(grep -c '^graphql ThreadComments {"thread":"thread-1-0","after":"[12]"}$' "$work/calls")" 2)"

clean_pr
jq -nc --argjson author "$app" '[range(1; 101) | {id: (5000 + .), body: "fine"} + $author]' > "$work/comments.1"
items 5101 "$company_proper Labs" > "$work/comments.2"
jq -nc --argjson author "$app" '[range(1; 101) | {id: (6000 + .), body: "fine"} + $author]' > "$work/reviews.1"
items 6101 "Blocking: see $key-7." > "$work/reviews.2"
run_pr_text
check "two lists with a second page each fail on both pages' items" \
  "$(is "$(contains "$out" 'comment:5101:1: names the company')$(contains "$out" 'review:6101:1: names the private')" truetrue)"
check "read in one second round for both" "$(is "$(grep -c '^graphql PrText ' "$work/calls")" 2)"

echo "case: a list GitHub's GraphQL API answers short of its total is refused by name"
clean_pr
jq -nc --argjson author "$app" '[range(1; 101) | {id: (1000 + .), body: "fine"} + $author]' > "$work/comments.1"
items 1101 "$company_proper Labs" > "$work/comments.2"
echo 1 > "$work/cut.comments"
run_pr_text
check "issue comments that stop short of their total fail" "$(is "$status" 1)"
check "names the list and both counts" \
  "$(contains "$out" "^::error::GitHub's GraphQL API reports 101 issue comments of #7 but listed 100, so not every one can be checked")"
clean_pr
jq -nc --argjson author "$app" '[range(1; 101) | {id: (2000 + .), body: "fine"} + $author]' > "$work/reviews.1"
items 2101 "fine" > "$work/reviews.2"
echo 1 > "$work/cut.reviews"
run_pr_text
check "reviews that stop short fail" "$(is "$status" 1)"
check "names them" "$(contains "$out" "^::error::GitHub's GraphQL API reports 101 reviews of #7 but listed 100")"
clean_pr
jq -nc --argjson author "$app" '[range(1; 101) | {id: (3000 + .), body: "fine"} + $author]' > "$work/review-comments.1"
items 3101 "fine" > "$work/review-comments.2"
echo 1 > "$work/cut.threads"
run_pr_text
check "review threads that stop short fail" "$(is "$status" 1)"
check "names them" "$(contains "$out" "^::error::GitHub's GraphQL API reports 101 review threads of #7 but listed 100")"
clean_pr
jq -nc --argjson author "$app" '[[range(1; 102) | {id: (4000 + .), body: "fine"} + $author]]' > "$work/review-comments.1"
echo 1 > "$work/cut.thread"
run_pr_text
check "a review thread's comments that stop short fail" "$(is "$status" 1)"
check "names the thread and both counts" \
  "$(contains "$out" "^::error::GitHub's GraphQL API reports 101 comments in review thread 1 of #7 but listed 20,")"

echo "case: a read GitHub's GraphQL API does not answer is refused by name"
clean_pr
jq -nc --argjson author "$app" '[range(1; 101) | {id: (1000 + .), body: "fine"} + $author]' > "$work/comments.1"
items 1101 "$company_proper Labs" > "$work/comments.2"
echo 2 > "$work/graphql-fails"
run_pr_text
check "a second round that fails is refused" "$(is "$status" 1)"
check "names the read and gh's message" \
  "$(contains "$out" "^::error::GitHub's GraphQL API did not answer the PrText read of #7 (gh: Something went wrong while executing your query.), so its text cannot be checked")"
check "scans nothing" "$(is "$(contains "$out" 'comment:1101')" false)"
clean_pr
jq -nc --argjson author "$app" '[[range(1; 102) | {id: (4000 + .), body: "fine"} + $author]]' > "$work/review-comments.1"
echo 2 > "$work/graphql-fails"
run_pr_text
check "a review thread's later page that fails is refused" "$(is "$status" 1)"
check "names the read" "$(contains "$out" "^::error::GitHub's GraphQL API did not answer the ThreadComments read of #7")"
clean_pr
touch "$work/base-gone"
run_pr_text
check "a pull request whose base branch GitHub answers none for is refused" "$(is "$status" 1)"
check "names the commits it could not read" \
  "$(contains "$out" "^::error::GitHub's GraphQL API answered no page 1 of the commits from main to event-head-sha")"

echo "case: a spent GraphQL budget is waited out within the job's limit, and refused by name past it"
# GraphQL answers a spent budget with HTTP 200 and an error of type RATE_LIMITED; gh fails on it.
# gh-read.sh's gh_wait_reset, which every read goes through, reads the reset off GET /rate_limit,
# waits for it when it falls inside the job's limit, and calls once more. Here the budget is back
# 3 s after the refusal, inside a one-minute job.
clean_pr
echo "1 3" > "$work/graphql-rate-limited"
run_pr_text_in_job 1
check "a clean pull request whose first read meets a spent budget passes" "$(is "$status" 0)"
check "says it waits for the reset, within the job's limit" \
  "$(contains "$out" "^::warning::the API budget is spent until .*; waiting [0-9]* s for it, within this job's limit at ")"
check "reads the reset off the rate_limit route once" "$(is "$(grep -c '^api rate_limit ' "$work/calls")" 1)"
check "asks the same read again after the wait" \
  "$(is "$(grep -c '^graphql PrText {"full":true,' "$work/calls")" 2)"
check "after the reset" "$(is "$([ "$seconds" -ge 3 ] && echo true || echo false)" true)"

# GitHub's words for it are "API rate limit already exceeded"; the type alone is read as well.
clean_pr
echo "1 3 type" > "$work/graphql-rate-limited"
run_pr_text_in_job 1
check "a RATE_LIMITED answer whose words name no rate limit is waited out too: passes" "$(is "$status" 0)"
check "and says so" "$(contains "$out" '^::warning::the API budget is spent until ')"

# A later round's read: the first page is kept, and the round is asked again from its cursor.
clean_pr
jq -nc --argjson author "$app" '[range(1; 101) | {id: (1000 + .), body: "fine"} + $author]' > "$work/comments.1"
items 1101 "fine" 1102 "$company_proper Labs" > "$work/comments.2"
echo "2 3" > "$work/graphql-rate-limited"
run_pr_text_in_job 1
check "a second round that meets a spent budget is waited out, and its refused comment named" \
  "$(is "$status|$(contains "$out" 'comment:1102:1: names the company')" '1|true')"
check "asks that round again from its cursor" \
  "$(is "$(grep -c '^graphql PrText .*"commentsAfter":"1"' "$work/calls")" 2)"

# A reset an hour away does not fit a one-minute job: no wait, and a refusal that names both.
clean_pr
echo "1 3600" > "$work/graphql-rate-limited"
run_pr_text_in_job 1
check "a budget spent past the job's limit is refused" "$(is "$status" 1)"
check "with no wait" "$(is "$([ "$seconds" -lt 30 ] && echo true || echo false)" true)"
check "names the reset and the job's limit" \
  "$(contains "$out" "^::error::the API budget is spent until .*, past this job's limit at ")"
check "names the read it could not make, with gh's words" \
  "$(contains "$out" "^::error::GitHub's GraphQL API did not answer the PrText read of #7 (gh: API rate limit already exceeded for installation ID 1.)")"
check "asks that read once" "$(is "$(grep -c '^graphql PrText ' "$work/calls")" 1)"

echo "case: a comment list the script cannot read whole"
clean_pr
printf '{"message": "Not Found"}\n' > "$work/review-comments.1"
run_pr_text
check "an answer that is no page of review threads fails" "$(is "$status" 1)"
check "names the list" "$(contains "$out" "^::error::GitHub's GraphQL API answered no page 1 of the review threads of #7")"

clean_pr
items 101 "fine" > "$work/comments.1"
jq -c '.[0].id = "IC_kwDOexample"' "$work/comments.1" > "$work/comments.next" && mv "$work/comments.next" "$work/comments.1"
run_pr_text
check "an item whose id is GraphQL's node id, not a whole number, fails" "$(is "$status" 1)"
check "names the cause" "$(contains "$out" 'whose id is not a whole number')"

clean_pr
jq '{action: "created", comment: {id: 101, body: "fine"}, issue: {number: 7, title: "a plain issue"}}' \
  "$work/event.json" > "$work/event.next"
mv "$work/event.next" "$work/event.json"
run_pr_text
check "a payload with no pull_request is refused" "$(is "$status" 1)"
check "names the cause" "$(contains "$out" 'carries no pull request')"

echo "case: only the comments of an App installed here or of the repository's own people count"
# On a public repository anyone signed in can comment or review, and every run reports the required
# check, so a run that read a stranger's text would let them turn it red on anyone's head.
# three_items: a comment, a review and a review comment, each naming something private.
three_items() {
  items 401 "As $company_proper Labs found." > "$work/comments.1"
  items 402 "Blocking: see $key-7." > "$work/reviews.1"
  items 403 "$(printf 'Nit.\n\nthe %s repository\n' "$repo")" > "$work/review-comments.1"
}
clean_pr
three_items
by drive-by User NONE "$work/comments.1"
by drive-by User NONE "$work/reviews.1"
by drive-by User CONTRIBUTOR "$work/review-comments.1"
run_pr_text
check "a stranger's comment, review and review comment naming private things pass" "$(is "$status" 0)"
check "names the skipped comment and its author's association" \
  "$(contains "$out" '^skipped comment:401: its author (NONE) is not trusted$')"
check "names the skipped review" "$(contains "$out" '^skipped review:402: its author (NONE) is not trusted$')"
check "names the skipped review comment, whose author contributed before" \
  "$(contains "$out" '^skipped review-comment:403: its author (CONTRIBUTOR) is not trusted$')"
check "does not print the stranger's login" "$(is "$(contains "$out" 'drive-by')" false)"
check "does not print the company" "$(is "$(contains "$out" "$company_proper")" false)"
check "does not print the key" "$(is "$(contains "$out" "$key")" false)"
check "does not print the repository" "$(is "$(contains "$out" "$repo")" false)"

clean_pr
items 404 "fine" > "$work/comments.1"
jq -c '.[0].user = null | .[0].author_association = "NONE"' "$work/comments.1" > "$work/comments.next"
mv "$work/comments.next" "$work/comments.1"
run_pr_text
check "a comment GitHub names no author for is skipped" \
  "$(contains "$out" '^skipped comment:404: its author (NONE) is not trusted$')"

for author in "dev-app[bot] Bot CONTRIBUTOR" "new-app[bot] Bot NONE" "owner User OWNER" \
  "member User MEMBER" "collaborator User COLLABORATOR"; do
  read -r login type association <<< "$author"
  clean_pr
  three_items
  for page in comments.1 reviews.1 review-comments.1; do by "$login" "$type" "$association" "$work/$page"; done
  run_pr_text
  check "the same three from $login ($type, $association) fail" "$(is "$status" 1)"
  check "names the comment" "$(contains "$out" 'comment:401:1: names the company')"
  check "names the review" "$(contains "$out" 'review:402:1: names the private deployment repository')"
  check "names the review comment" "$(contains "$out" 'review-comment:403:3: names the private deployment repository')"
  check "skips none" "$(is "$(contains "$out" '^skipped ')" false)"
done

clean_pr
items 405 "As $company_proper Labs found." 406 "All clear now." > "$work/comments.1"
by drive-by User NONE "$work/comments.1" 1
run_pr_text
check "a stranger's clean comment after an App's red one still fails, so it clears nothing" "$(is "$status" 1)"
check "names the App's comment" "$(contains "$out" 'comment:405:1: names the company')"
check "skips the stranger's" "$(contains "$out" '^skipped comment:406: its author (NONE) is not trusted$')"

finish_self_log
summary "check-private-names.sh"
