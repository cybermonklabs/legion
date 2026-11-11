#!/usr/bin/env bash
# The port of this repository's four private-name rules, proved by comparison rather than by
# assertion: both engines exist at this head, so every case below runs through
# .github/scripts/check-private-names.sh — the frozen shell script, which this test never edits —
# and through packages/daemon/internal/leakguard's rule file as the `legion` binary runs it, and
# the two are held to one verdict and one class.
#
# The engine's two doors, both of them the guard's own (no gitleaks binary, no install action):
#
#   * `legion leak-check text`, which reads a text on standard input. It is the door a publisher
#     uses, and the whole of what it scans is that text: its records are "the title" (line one)
#     and "the body" (the rest), so a rule that reads a file's path cannot be reached through it.
#   * `legion leak-check pre-push`, which reads a repository's own commits. A file a commit adds
#     becomes a record whose label is that file's path (packages/daemon/internal/leakguard's
#     push.go, commitRecords), so this is the door the three path rules are reached through, and
#     the only one that can answer the frozen script's `search_paths` at all.
#
# The shared public leak check the guard resolves first is a stand-in that clears everything, so
# that the rule file alone decides every verdict here; with no check resolvable the command
# answers 2 (a setup failure) and nothing would be compared.
#
# The corpus is .github/scripts/check-private-names.test.sh's, case for case: its 16 rule cases
# are ported here with the mapping table below, and its 13 argument-handling cases are run too,
# each one recording what the engine does instead of the frozen script's own path handling, which
# the engine replaces rather than inherits. Its 10 pull-request-text cases belong to
# check-pr-text.sh and stay there. Beyond the corpus the five rules are driven over a file's
# content and over a file's path, the conditional allow-list is driven with each half holding
# alone, and the frozen script's own selection rules are driven one by one.
#
# A class the mapping table does not hold fails its case rather than passing it silently: a port
# that grew a sixth class, or renamed one, is a port nobody has compared.
#
# Every difference the comparison finds is either a FAIL or a NOTE: a NOTE is a difference this
# change makes on purpose, named and explained where it is found and listed again at the end, and
# anything else is a failure of the port.
#
# At the merge base this test fails and cannot be made to pass: the rule file it compares against
# is not in the tree there, so the `legion` binary does not build (leakguard embeds that file) and
# the build below is the first thing to fail.
#
# Run from anywhere: .github/scripts/check-private-names-port.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$script_dir/../.." && pwd)
frozen_script="$script_dir/check-private-names.sh"
rule_file="$repo_root/packages/daemon/internal/leakguard/private-names.toml"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# This suite's own stdout is a CI log, and the last case runs both engines over it, exactly as
# check-private-names.test.sh does with its own: a case that one day printed an assembled name
# would turn red here rather than in a job log. `exec 3>&1 4>&2` keeps the real terminal
# reachable, so the redirection can be undone before the file is read.
self_log="$work/suite-output.log"
exec 3>&1 4>&2
exec > >(tee "$self_log") 2>&1
tee_pid=$!

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

command -v git > /dev/null || { echo "  FAIL: git is required" >&2; exit 1; }
command -v go > /dev/null || { echo "  FAIL: go is required: this test builds the legion binary" >&2; exit 1; }

echo "building the engine: the legion binary of this head, once for the whole run"
engine="$work/legion"
(cd "$repo_root/packages/daemon" && go build -o "$engine" ./cmd/legion) || {
  echo "  FAIL: the legion binary did not build, so there is no port to compare with" >&2
  exit 1
}

# The stand-in shared check: it clears every write, so the rule file is the only thing that can
# refuse one here. PUBLIC_LEAK_CHECK is the first place the guard looks (leakguard's Resolve).
export PUBLIC_LEAK_CHECK="$work/public-leak-check.sh"
printf '#!/usr/bin/env bash\nexit 0\n' > "$PUBLIC_LEAK_CHECK"
chmod +x "$PUBLIC_LEAK_CHECK"
# A local path as the push's URL names no GitHub repository, so the guard reads the destination as
# public (its fail-closed answer) and scans, without a gh call.
remote_url="$work/remote.git"

# The names the rules hunt are assembled from the pieces below, so this file holds none of them
# whole and the guard scans it like any other file. The two that are this repository's own are
# split the way private-names.toml splits its own patterns; every host and tailnet below is
# invented, and the reserved names (the example host, the example tailnet) stand only in the
# cases that prove they pass.
repo="agent""-c"
repo_snake="agent""_c"
repo_joined="agent""c"
repo_spaced="agent"" c"
repo_en_dash="agent""–c"
repo_nonbreaking_hyphen="agent""‑c"
repo_percent_hyphen="agent""%2Dc"
nbsp=$'\xc2\xa0'
en_dash=$'\xe2\x80\x93'
key="AGENT""C"
company="traj""ectory"
company_proper="T""rajectory"
labs="labs"
internal="internal"
host_label="beacon"
host_suffix="quietforge"
example_host_suffix="example"
vendored_nats="packages/claude-envoy/dist/envoy-channel.js"
tailnet_suffix="ts.net"
tailnet_label="quiet-harbor"
tailnet_machine="mossy-lantern"
tailnet_example="example-tailnet"

# ---------------------------------------------------------------------------------------------
# The mapping table: each class the frozen script labels a finding with, and the rule of
# private-names.toml that carries it. The rule file was written with the script's labels as its
# descriptions, so the classes are equal word for word and the table's work is the other
# direction: a label it does not hold fails the case that produced it (`agree`), and a rule id it
# names that the rule file does not hold, or holds under another description, fails the run here.
#
#   frozen script's label                                               | where   | rule of the port
#   --------------------------------------------------------------------+---------+-----------------
#   names the private deployment repository or its Dispatch project key | content | repository_rule
#   names the company                                                   | content | company_rule, proper_noun_rule
#   names a private internal host                                       | content | private_host_rule
#   names a Tailscale MagicDNS host                                     | content | tailnet_host_rule
#   ... or its Dispatch project key in its path                         | path    | repository_rule_path
#   names the company in its path                                       | path    | company_rule_path, proper_noun_rule_path
rules_of() { # rules_of <label>: the rule ids of the port that carry that label, space separated
  case $1 in
    'names the private deployment repository or its Dispatch project key') echo "repository_rule" ;;
    'names the company') echo "company_rule proper_noun_rule" ;;
    'names a private internal host') echo "private_host_rule" ;;
    'names a Tailscale MagicDNS host') echo "tailnet_host_rule" ;;
    'names the private deployment repository or its Dispatch project key in its path') echo "repository_rule_path" ;;
    'names the company in its path') echo "company_rule_path proper_noun_rule_path" ;;
    *) return 1 ;;
  esac
}
mapped_labels=(
  'names the private deployment repository or its Dispatch project key'
  'names the company'
  'names a private internal host'
  'names a Tailscale MagicDNS host'
  'names the private deployment repository or its Dispatch project key in its path'
  'names the company in its path'
)

echo "case: every row of the mapping table is a rule of the rule file, under that description"
for label in "${mapped_labels[@]}"; do
  for id in $(rules_of "$label"); do
    description=$(sed -n "/^id = \"$id\"\$/,/^\$/p" "$rule_file" | sed -n 's/^description = "\(.*\)"$/\1/p')
    check "$id carries its row's class" "$(is "$description" "$label")"
  done
done

# ---------------------------------------------------------------------------------------------
# The two engines, as the drivers below reach them.

door_status=0
door_classes=""
door_out=""
frozen_status=0
frozen_classes=""
frozen_out=""

# text_door <text>: the engine's text door over that text.
text_door() {
  set +e
  door_out=$(printf '%s\n' "$1" | "$engine" leak-check text "the fixture" 2>&1)
  door_status=$?
  set -e
  door_classes=$(printf '%s\n' "$door_out" | sed -n -E 's/^  the (title|body): //p' | sort -u)
}

# push_door <root>: the engine's pre-push door over root's last commit, which is the commit the
# case made. The update names the parent as the commit the destination already holds, so the push
# the guard reads is that one commit and nothing before it.
push_door() {
  local root=$1 sha parent
  sha=$(git -C "$root" rev-parse HEAD)
  parent=$(git -C "$root" rev-parse HEAD~1)
  set +e
  door_out=$(cd "$root" && printf 'refs/heads/main %s refs/heads/main %s\n' "$sha" "$parent" |
    "$engine" leak-check pre-push origin "$remote_url" 2>&1)
  door_status=$?
  set -e
  door_classes=$(printf '%s\n' "$door_out" | sed -n -E 's/^  [^ :]+: //p' | sort -u)
}

# read_frozen <output> <status>: the frozen script's answer, as a verdict and a set of classes.
# Its two report shapes are a line hit (`::error file=…,line=N::path:N: class`) and a path hit
# (`::error::path: class in its path`).
read_frozen() {
  frozen_out=$1
  frozen_status=$2
  frozen_classes=$(printf '%s\n' "$frozen_out" |
    sed -n -E -e 's/^::error file=[^ ]+::[^ ]+ //p' -e 's/^::error::[^ ]+ //p' | sort -u)
}

# The corpus repository the content cases are scanned in: one tracked file, rewritten per case.
# The frozen script resolves its root from its own path, so the symlink puts that root here.
corpus="$work/corpus"
mkdir -p "$corpus/.github/scripts" "$corpus/docs"
ln -s "$frozen_script" "$corpus/.github/scripts/check-private-names.sh"
printf '# Notes\n\nNothing private here.\n' > "$corpus/docs/notes.md"
git -C "$corpus" init -q
git -C "$corpus" add docs/notes.md

# frozen_repo <text>: the frozen script's repository scan with that text as the one tracked file.
frozen_repo() {
  local out status=0
  printf '%s\n' "$1" > "$corpus/docs/notes.md"
  set +e
  out=$(cd "$corpus" && .github/scripts/check-private-names.sh 2>&1)
  status=$?
  set -e
  read_frozen "$out" "$status"
}

# ---------------------------------------------------------------------------------------------
# The comparison, and the two ways a difference is recorded.

divergences="$work/divergences"
: > "$divergences"
disagreements="$work/disagreements"
: > "$disagreements"

# note <case> <what the frozen script does> <what the engine does> <why>: a difference this change
# makes on purpose. It is printed where it is found and listed again at the end.
note() {
  printf '%s | frozen: %s | engine: %s | %s\n' "$1" "$2" "$3" "$4" >> "$divergences"
  echo "  NOTE: $1 — the frozen script $2, the engine $3; $4"
}

# verdicts <frozen status> <engine status>: whether the two refused or cleared together. The
# frozen script exits 1 for a finding; the engine's commands exit 3 for a refusal.
verdicts() {
  case "$1:$2" in
    0:0 | 1:3) echo true ;;
    *) echo false ;;
  esac
}

# agree <case>: the one assertion. The two must refuse or clear together, and every class the
# frozen script gave must be a class the engine gave, through the mapping table — a label the
# table does not hold fails the case rather than passing it.
agree() {
  local name=$1 label ok
  ok=$(verdicts "$frozen_status" "$door_status")
  if [ "$ok" = true ] && [ -n "$frozen_classes" ]; then
    while IFS= read -r label; do
      [ -n "$label" ] || continue
      if ! rules_of "$label" > /dev/null; then
        echo "  FAIL: $name: the frozen script's class is in no row of this test's mapping table"
        FAIL=$((FAIL + 1))
        printf '%s | unmapped class\n' "$name" >> "$disagreements"
        return 0
      fi
      grep -q -x -F -- "$label" <<< "$door_classes" || ok=false
    done <<< "$frozen_classes"
  fi
  check "$name: one verdict, one class" "$ok"
  if [ "$ok" != true ]; then
    printf '%s | frozen exit %s [%s] | engine exit %s [%s]\n' \
      "$name" "$frozen_status" "$(tr '\n' ';' <<< "$frozen_classes")" \
      "$door_status" "$(tr '\n' ';' <<< "$door_classes")" >> "$disagreements"
  fi
}

# compare <case> <text>: the same text through the frozen script's repository scan and through the
# engine's text door.
compare() {
  frozen_repo "$2"
  text_door "$2"
  agree "$1"
}

# compare_each <prefix> <array-name> [<line>]: one case per line of the named array, written on
# line 1 or, with a filler line in front, on line 2. Each case is named by its index, never by the
# text, since this suite's own output is a CI log.
compare_each() {
  local prefix=$1 index
  local -n lines=$2
  local line=${3:-1}
  for index in "${!lines[@]}"; do
    if [ "$line" = 2 ]; then
      compare "$prefix-$index" "$(printf 'line one\n%s' "${lines[index]}")"
    else
      compare "$prefix-$index" "${lines[index]}"
    fi
  done
}

# ---------------------------------------------------------------------------------------------
# The 16 rule cases of check-private-names.test.sh, in its own order.

echo "rule case 1: a clean repository"
compare clean "Nothing private here."

echo "rule case 2: every spelling of the deployment repository and its project key"
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
compare_each repo mentions 2

echo "rule case 3: every spelling of the company"
# shellcheck disable=SC2034 # read by name through compare_each's nameref
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
compare_each company mentions

echo "rule case 4: a private internal host, whatever its case"
# shellcheck disable=SC2034 # read by name through compare_each's nameref
hosts=(
  "https://$host_label.$internal.$host_suffix"
  "https://$host_label.${internal^^}.$host_suffix"
  "https://$host_label.${internal^}.$host_suffix"
  "https://$host_label.$internal.${example_host_suffix}corp.net"
  "https://$host_label.$internal.$example_host_suffix.$host_suffix"
  "https://a.$internal.$example_host_suffix and https://b.${internal^^}.$host_suffix"
  "this.$internal.push(value)"
)
compare_each host hosts

echo "rule case 5: a reserved example host passes"
compare example-host "$(printf 'https://%s.%s.%s\nnats://nats.%s.%s.com:4222\nPoint it at api.%s.%s.' \
  "$host_label" "$internal" "$example_host_suffix" \
  "$internal" "$example_host_suffix" "$internal" "$example_host_suffix")"
# The other half of that case — the vendored NATS client's own property, which the frozen script
# allows in the one bundle that vendors it — is the conditional allow-list, driven below where
# both of its halves can be driven.

echo "rule case 6: a Tailscale MagicDNS host, whatever its case"
# shellcheck disable=SC2034 # read by name through compare_each's nameref
tailnet_hosts=(
  "https://nats.$tailnet_label.$tailnet_suffix:4222"
  "https://nats.${tailnet_label^^}.$tailnet_suffix:4222"
  "https://nats.$tailnet_label.${tailnet_suffix^^}"
  "https://a.$tailnet_example.$tailnet_suffix and https://b.$tailnet_label.$tailnet_suffix"
  "see _nats.$tailnet_label.${tailnet_suffix}_ in italics"
  "NATS_$tailnet_label.$tailnet_suffix"
  "https://$tailnet_machine.$tailnet_label.$tailnet_suffix"
)
compare_each tailnet tailnet_hosts

echo "rule case 7: the reserved example tailnet passes, under any machine name"
compare example-tailnet "$(printf 'https://nats.%s.%s:4222\nhttps://%s.%s.%s\nnats://<tailnet>.%s:4222\nnats://<name>.<tailnet>.%s:4222\nhost.%s.%swork, a longer word the boundary must not mistake for one' \
  "$tailnet_example" "$tailnet_suffix" "$tailnet_machine" "$tailnet_example" "$tailnet_suffix" \
  "$tailnet_suffix" "$tailnet_suffix" "$tailnet_label" "$tailnet_suffix")"

echo "rule case 8: identifiers that merely start with those letters, and the English word, pass"
compare lookalikes "$(printf 'agentCursor AgentConversationPage subagentContext agentCount agentCalls agentCard\nleaveAgentComposer AGENT_COMPOSER_SELECTOR agent-composer agent-cursor agent-card agent-credentials\nagent cards, an agent class, the agents C and D\nThe models may take a different %s in tool calling; %s, plotted. %sPlot.' \
  "$company" "$company" "$company_proper")"

echo "rule case 9: the report names the record and the class, never the text"
compare quiet "the $repo repository"
check "the frozen script names the file and the line" \
  "$(contains "$frozen_out" '^::error file=docs/notes.md,line=1::docs/notes.md:1:')"
check "the frozen script does not repeat the name" "$(is "$(contains "$frozen_out" "$repo")" false)"
check "the engine names the record it refused" "$(contains "$door_out" '^  the title: names the private deployment repository')"
check "the engine does not repeat the name" "$(is "$(contains "$door_out" "$repo")" false)"
note "rule-9-record" "names the file and the line a hit is on" \
  "names the record of the write, which for a text is its title or its body" \
  "the engine scans a write's records, not a checkout's files, so a line number is not something it has"

echo "rule case 10: a line naming two classes"
compare both "$repo, by $company_proper Labs"
check "the frozen script counts one finding" "$(contains "$frozen_out" '^check-private-names: 1 finding(s)')"
check "the engine names both classes" "$(is "$(printf '%s\n' "$door_classes" | wc -l)" 2)"
note "rule-10-two-classes" "reports one class for a line (sort -u over the file and the line)" \
  "reports every class of a record" \
  "a verdict both agree on; the engine's report is the longer one, and no class is lost"

echo "rule case 11: a file git has not been told about, and an ignored one"
untracked=$(mktemp -d "$work/untracked.XXXX")
mkdir -p "$untracked/.github/scripts" "$untracked/docs"
ln -s "$frozen_script" "$untracked/.github/scripts/check-private-names.sh"
printf 'clean\n' > "$untracked/docs/notes.md"
git -C "$untracked" init -q
git -C "$untracked" add docs/notes.md
printf '%s\n' "$key-7" > "$untracked/docs/new.md"
set +e
out=$(cd "$untracked" && .github/scripts/check-private-names.sh 2>&1)
status=$?
set -e
read_frozen "$out" "$status"
check "the frozen script refuses a file git has not been told about" "$(is "$frozen_status" 1)"
note "rule-11-untracked" "scans a file git has not been told about" \
  "never sees it: a file no commit holds is in no push, and no publisher hands it to the text door" \
  "the guard checks what a write would publish, so an unadded file is caught at the push that adds it"
printf 'dist/\n' > "$untracked/.gitignore"
git -C "$untracked" add .gitignore
mkdir -p "$untracked/dist"
rm "$untracked/docs/new.md"
printf '%s\n' "$key-7" > "$untracked/dist/page.html"
set +e
out=$(cd "$untracked" && .github/scripts/check-private-names.sh 2>&1)
status=$?
set -e
read_frozen "$out" "$status"
check "the frozen script passes an ignored file" "$(is "$frozen_status" 0)"
check "the engine passes it too, having never been handed it" "$(is "$(verdicts "$frozen_status" 0)" true)"

# ---------------------------------------------------------------------------------------------
# The path cases, and everything else a commit is the only way to show. Each fixture's root
# commit holds a file of its own, because git's diff of a root commit against no parent lists no
# file at all and the guard's push records are that diff.

# push_fixture <name>: a repository with a root commit, and the frozen script symlinked in.
push_fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts"
  git -C "$root" init -q
  git -C "$root" config user.email author@example.com
  git -C "$root" config user.name "A Author"
  printf 'seed\n' > "$root/seed.txt"
  git -C "$root" add seed.txt
  git -C "$root" commit -q -m "chore: seed"
  ln -s "$frozen_script" "$root/.github/scripts/check-private-names.sh"
  echo "$root"
}

# frozen_tree <root>: the frozen script's repository scan of that fixture.
frozen_tree() {
  local out status=0
  set +e
  out=$(cd "$1" && .github/scripts/check-private-names.sh 2>&1)
  status=$?
  set -e
  read_frozen "$out" "$status"
}

# compare_file <case> <fixture> <path> <content>: one new file, tracked in the fixture the frozen
# script scans and added by the commit the engine's push door reads.
compare_file() {
  local name=$1 root=$2 path=$3 content=$4
  mkdir -p "$root/$(dirname -- "$path")"
  printf '%s\n' "$content" > "$root/$path"
  # -f, so the one case whose file sits under an ignored path tracks it as the frozen test does.
  git -C "$root" add -f -- "$path"
  git -C "$root" commit -q -m "chore: add a file"
  push_door "$root"
  frozen_tree "$root"
  agree "$name"
}

echo "rule case 12: a file git tracks under an ignored path"
root=$(push_fixture tracked-ignored)
printf 'notes/\n' > "$root/.gitignore"
git -C "$root" add .gitignore
git -C "$root" commit -q -m "chore: ignore notes"
compare_file tracked-ignored "$root" "notes/report.md" "$key-7"

echo "rule case 13: a name in a file's path"
root=$(push_fixture path-name)
compare_file path-name "$root" "docs/notes/$key-7-runbook.md" "clean text"
check "the frozen script masks the component" "$(contains "$frozen_out" 'docs/notes/<name>')"
check "the frozen script does not print it" "$(is "$(contains "$frozen_out" "$key")" false)"
check "the engine masks the component too" "$(contains "$door_out" 'docs/notes/<name>')"
check "the engine does not print it" "$(is "$(contains "$door_out" "$key")" false)"

root=$(push_fixture path-name-dir)
compare_file path-name-dir "$root" "docs/${company_proper}/notes.md" "clean text"
check "a directory component naming the company is masked" "$(is "$(contains "$door_out" "$company_proper")" false)"

echo "rule case 14: a non-ASCII file name reaches the rules unquoted"
root=$(push_fixture path-name-dash)
compare_file path-name-dash "$root" "docs/$repo_en_dash.md" "clean text"
check "the engine masks it" "$(contains "$door_out" 'docs/<name>')"
root=$(push_fixture path-unicode)
compare_file path-unicode "$root" "docs/café.md" "$key-7"
check "the engine names the file as it is, unquoted" "$(contains "$door_out" '^  docs/café\.md: ')"

echo "rule case 15: lockfiles and binary files"
root=$(push_fixture skipped-lockfile)
mkdir -p "$root/docs"
printf '"%s": "sha512-%s"\n' "$repo" "$key" > "$root/bun.lock"
git -C "$root" add -f -- bun.lock
git -C "$root" commit -q -m "chore: add a lockfile"
push_door "$root"
frozen_tree "$root"
check "the frozen script skips a lockfile" "$(is "$frozen_status" 0)"
check "the engine refuses the line a lockfile added" "$(is "$door_status" 3)"
note "lockfile" "skips bun.lock, go.sum and go.work.sum, whose integrity hashes are random text" \
  "has no lockfile exception: a line a commit adds to one is a record like any other" \
  "a tightening whose cost is a false refusal where a hash happens to spell one of these names; the push door reads only the lines a commit adds, so a lockfile nobody edits is never read"
root=$(push_fixture skipped-binary)
printf '%s\0binary\n' "$repo" > "$work/skipped-binary/docs-image.bin"
mkdir -p "$work/skipped-binary/docs"
mv "$work/skipped-binary/docs-image.bin" "$work/skipped-binary/docs/image.bin"
git -C "$root" add -A -- docs/image.bin
git -C "$root" commit -q -m "chore: add a binary"
push_door "$root"
frozen_tree "$root"
agree skipped-binary

# ---------------------------------------------------------------------------------------------
# The 13 argument-handling cases of check-private-names.test.sh. They are the frozen script's own
# way of being told what to scan — a path argument, resolved from the caller's directory — and the
# engine has no such input: a text comes on its standard input, a push comes from the repository
# the hook runs in, and neither is named by a path argument. Each case is run on both sides and
# the difference recorded.

# arg_fixture <name>: the frozen test's own fixture — a repository with one tracked file.
arg_fixture() {
  local root="$work/$1"
  mkdir -p "$root/.github/scripts" "$root/docs"
  ln -s "$frozen_script" "$root/.github/scripts/check-private-names.sh"
  printf '# Notes\n\nNothing private here.\n' > "$root/docs/notes.md"
  git -C "$root" init -q
  git -C "$root" add docs/notes.md
  echo "$root"
}

# frozen_in <dir> <arg...>: the frozen script run from dir, through the fixture's own symlink.
frozen_in() {
  local dir=$1 out status=0
  shift
  set +e
  out=$(cd "$dir" && ../.github/scripts/check-private-names.sh "$@" 2>&1)
  status=$?
  set -e
  read_frozen "$out" "$status"
}

# engine_in <dir> <text> <arg...>: the text door run from dir with that text on standard input.
engine_in() {
  local dir=$1 text=$2
  shift 2
  set +e
  door_out=$(cd "$dir" && printf '%s\n' "$text" | "$engine" leak-check text "$@" 2>&1)
  door_status=$?
  set -e
}

echo "argument case 16: named paths are scanned instead of the repository"
root=$(arg_fixture arg-paths)
printf 'dist/\n' > "$root/.gitignore"
mkdir -p "$root/dist"
printf '<pre>{"project": "%s"}</pre>\n' "$key" > "$root/dist/page.html"
set +e
out=$(cd "$root" && .github/scripts/check-private-names.sh dist 2>&1)
status=$?
set -e
read_frozen "$out" "$status"
check "the frozen script scans the named path, ignored or not" "$(is "$frozen_status" 1)"
engine_in "$root" '<pre>{"project": "'"$key"'"}</pre>' "the fixture"
check "the engine refuses that same text" "$(is "$door_status" 3)"
note "arg-16-named-paths" "takes a path argument and scans the tree under it" \
  "takes none: the write it checks is the text on its standard input, or the push the hook runs it for" \
  "the built site is scanned by the frozen script's own job, which this change does not move"

echo "argument case 17: a single named file is reported under its own name"
root=$(arg_fixture arg-single-file)
printf 'line one\n%s\n' "$key-7" > "$root/body.md"
set +e
out=$(cd "$root" && .github/scripts/check-private-names.sh body.md 2>&1)
status=$?
set -e
read_frozen "$out" "$status"
check "the frozen script names the file and the line" \
  "$(contains "$frozen_out" '^::error file=body.md,line=2::body.md:2: names the private deployment repository')"
engine_in "$root" "$(printf 'line one\n%s' "$key-7")" "the fixture"
check "the engine refuses the same text" "$(is "$door_status" 3)"
check "the engine names the record instead of the file" "$(contains "$door_out" '^  the body: names the private deployment repository')"

echo "argument case 18: a named path that does not exist"
root=$(arg_fixture arg-missing)
set +e
out=$(cd "$root" && .github/scripts/check-private-names.sh dist 2>&1)
status=$?
set -e
read_frozen "$out" "$status"
check "the frozen script exits 2" "$(is "$frozen_status" 2)"
check "and names it" "$(contains "$frozen_out" 'dist does not exist')"
set +e
printf 'clean\n' | "$engine" leak-check text > /dev/null 2>&1
no_argument=$?
printf 'clean\n' | "$engine" leak-check text one two > /dev/null 2>&1
two_arguments=$?
set -e
check "the engine answers a call it cannot read with 2 as well, given no argument" "$(is "$no_argument" 2)"
check "and given two" "$(is "$two_arguments" 2)"
note "arg-18-missing-path" "exits 2 for a path argument naming nothing" \
  "exits 2 for a call it cannot read, which is the same fail-closed status for the same kind of mistake" \
  "it has no path to miss: what it would scan is handed to it, not named"

echo "argument cases 19 to 28: a path argument resolved from the caller's directory"
# The frozen script reads a relative argument from the caller's directory, follows a symlink and a
# '..' as the kernel would, reads a name starting with '-' as a path, refuses an empty argument,
# takes an absolute path from outside the repository, is not moved by an exported CDPATH, and
# survives a directory name ending in a newline. The engine has none of that machinery: the same
# text answers the same way from every one of those directories, which is what the cases below
# show, one directory at a time.
odd_dirs=()
root=$(arg_fixture arg-relative)
mkdir -p "$root/work"
odd_dirs+=("$root/work")                                      # 19: a subdirectory
odd_dirs+=("$root")                                           # 20: the root a missing path was read from
ln -s "$root" "$work/arg-relative-link"
odd_dirs+=("$work/arg-relative-link/work")                    # 21: a checkout reached through a symlink
mkdir -p "$root/elsewhere/deep"
ln -s ../elsewhere/deep "$root/work/link"
odd_dirs+=("$root/work/link")                                 # 22: behind a symlink a '..' leads through
mkdir -p "$root/-dash-dir"
odd_dirs+=("$root/-dash-dir")                                 # 23: a directory whose name starts with '-'
outside="$work/arg-outside"
mkdir -p "$outside"
odd_dirs+=("$outside")                                        # 25: a directory outside the repository
newline_dir="$root/work"$'\n'
mkdir -p "$newline_dir"
odd_dirs+=("$newline_dir")                                    # 27: a directory whose name ends in a newline
odd_index=0
for dir in "${odd_dirs[@]}"; do
  odd_index=$((odd_index + 1))
  engine_in "$dir" "$(printf 'line one\n%s' "$key-7")" "the fixture"
  check "the engine's answer does not depend on the caller's directory ($odd_index)" "$(is "$door_status" 3)"
done
set +e
out=$(cd "$root/work" && ../.github/scripts/check-private-names.sh '' 2>&1)
status=$?
set -e
read_frozen "$out" "$status"
check "the frozen script refuses an empty path argument (24)" "$(is "$frozen_status" 2)"
engine_in "$root" "$(printf 'line one\n%s' "$key-7")" ''
check "the engine takes an empty argument as a nameless write and still refuses the text (24)" \
  "$(is "$door_status" 3)"
set +e
printf 'clean\n' | "$engine" leak-check text -dash.md > /dev/null 2>&1
dash_argument=$?
set -e
check "the engine reads an argument starting with '-' as a flag and exits 2 (23)" "$(is "$dash_argument" 2)"
set +e
out=$(cd "$root" && CDPATH=. .github/scripts/check-private-names.sh docs/notes.md 2>&1)
status=$?
set -e
read_frozen "$out" "$status"
check "an exported CDPATH does not move the frozen script's root (26)" "$(is "$frozen_status" 0)"
engine_in "$root" "clean text" "the fixture"
check "the engine resolves no root at all, so CDPATH reaches nothing of it (26)" "$(is "$door_status" 0)"
note "arg-19-to-28-paths" \
  "resolves a path argument from the caller's directory, through symlinks, '..', a leading '-', an empty argument, a directory outside the repository, an exported CDPATH and a directory name ending in a newline" \
  "answers the same for the same write from every one of those directories, and reads an argument starting with '-' as a flag" \
  "the write reaches the engine on its standard input or through the repository the hook runs in, so none of that resolution has an input to work on"

# ---------------------------------------------------------------------------------------------
# Beyond the corpus.

echo "beyond: each of the five rules over a file's content"
content_rules=(
  "repository_rule|names the private deployment repository or its Dispatch project key|the $repo repository"
  "company_rule|names the company|$company.$labs"
  "proper_noun_rule|names the company|the $company_proper team"
  "private_host_rule|names a private internal host|https://$host_label.$internal.$host_suffix"
  "tailnet_host_rule|names a Tailscale MagicDNS host|https://$tailnet_machine.$tailnet_label.$tailnet_suffix"
)
for row in "${content_rules[@]}"; do
  IFS='|' read -r id label text <<< "$row"
  compare "content-$id" "$text"
  check "content-$id: the engine gives that rule's class" "$(grep -q -x -F -- "$label" <<< "$door_classes" && echo true || echo false)"
done

echo "beyond: each of the five rules over a file's path"
path_rules=(
  "repository_rule_path|names the private deployment repository or its Dispatch project key in its path|docs/$key-7/notes.md"
  "company_rule_path|names the company in its path|docs/$company$labs/notes.md"
  "proper_noun_rule_path|names the company in its path|docs/$company_proper/report.md"
)
for row in "${path_rules[@]}"; do
  IFS='|' read -r id label path <<< "$row"
  root=$(push_fixture "path-rule-$id")
  compare_file "path-$id" "$root" "$path" "clean text"
  check "path-$id: the engine gives that rule's class" "$(grep -q -x -F -- "$label" <<< "$door_classes" && echo true || echo false)"
done
# The other two rules have no path form, in the script or in the rule file: the script's
# search_paths runs its first three rules only. A file whose name looks like a host or a tailnet
# host, with clean content, is where the two sides part.
root=$(push_fixture path-rule-host)
printf 'clean text\n' > "$root/$host_label.$internal.$host_suffix.md"
git -C "$root" add -A
git -C "$root" commit -q -m "chore: add a file"
push_door "$root"
frozen_tree "$root"
check "the frozen script passes a host-shaped file name with clean content" "$(is "$frozen_status" 0)"
check "the engine refuses it" "$(is "$door_status" 3)"
note "path-host-shaped-name" "reads a path with its first three rules only, so a host-shaped file name passes" \
  "reads an added path as a text as well as a path, so all five rules see it and this one is refused" \
  "a tightening, not a loss: the guard refuses more than the script did, and the class is the host rule's"
root=$(push_fixture path-rule-tailnet)
printf 'clean text\n' > "$root/$tailnet_label.$tailnet_suffix.md"
git -C "$root" add -A
git -C "$root" commit -q -m "chore: add a file"
push_door "$root"
frozen_tree "$root"
check "the frozen script passes a tailnet-shaped file name with clean content" "$(is "$frozen_status" 0)"
check "the engine refuses it" "$(is "$door_status" 3)"
note "path-tailnet-shaped-name" "reads a path with its first three rules only, so a tailnet-shaped file name passes" \
  "reads an added path as a text as well as a path, so this one is refused" \
  "the same tightening as the host-shaped name above"

echo "beyond: a directory entry holding no scanned file"
root=$(arg_fixture dir-entry)
mkdir -p "$root/dist/$key-7-runbook"
set +e
out=$(cd "$root" && .github/scripts/check-private-names.sh dist 2>&1)
status=$?
set -e
read_frozen "$out" "$status"
check "the frozen script refuses an empty directory whose name names the repository" "$(is "$frozen_status" 1)"
check "and reports it as a path" "$(contains "$frozen_out" 'in its path')"
note "dropped-empty-directory" "refuses a directory entry that holds no file, because find lists it" \
  "never sees one: git tracks files, so no commit adds a directory of its own and no record is made for it" \
  "a behaviour this change drops; a directory whose name names something private is caught at the first file committed under it"

echo "beyond: a zero-byte file's path"
root=$(push_fixture zero-byte)
: > "$root/docs-placeholder"
mkdir -p "$root/docs/$key-7"
: > "$root/docs/$key-7/.keep"
rm "$root/docs-placeholder"
git -C "$root" add -A
git -C "$root" commit -q -m "chore: add an empty file"
push_door "$root"
frozen_tree "$root"
agree zero-byte
check "the engine reads a zero-byte file's path, which an engine scanning a directory would skip" \
  "$(contains "$door_out" 'in its path')"

echo "beyond: the conditional allow-list, with each half holding alone"
root=$(push_fixture allowlist-both)
compare_file allowlist-both "$root" "$vendored_nats" "        this.$internal.push(sv);"
check "both halves holding, the engine allows it" "$(is "$door_status" 0)"
root=$(push_fixture allowlist-pattern-only)
compare_file allowlist-pattern-only "$root" "packages/claude-envoy/dist/other-channel.js" "        this.$internal.push(sv);"
check "the pattern alone, the engine refuses it" "$(is "$door_status" 3)"
root=$(push_fixture allowlist-path-only)
compare_file allowlist-path-only "$root" "$vendored_nats" "// see https://$host_label.$internal.$host_suffix"
check "the path alone, the engine refuses it" "$(is "$door_status" 3)"
text_door "        this.$internal.push(sv);"
check "through the text door the allowed property is refused, there being no path to allow it by" \
  "$(is "$door_status" 3)"
note "allowlist-through-the-text-door" \
  "allows the vendored NATS client's own property in the one bundle that vendors it" \
  "allows it at the push door, where the record carries that bundle's path, and refuses it at the text door, where a record has no path" \
  "the allow-list is a path and a pattern together, and a text has no path; a text that quoted that line would have to be reworded"

echo "beyond: the frozen script's selection rules, one by one"
root=$(push_fixture selection-untracked)
printf '%s\n' "$key-7" > "$root/docs-untracked.md"
mkdir -p "$root/docs"
mv "$root/docs-untracked.md" "$root/docs/new.md"
frozen_tree "$root"
check "the frozen script refuses an untracked file" "$(is "$frozen_status" 1)"
set +e
door_out=$(cd "$root" && printf 'refs/heads/main %s refs/heads/main %s\n' \
  "$(git -C "$root" rev-parse HEAD)" "$(git -C "$root" rev-parse HEAD)" |
  "$engine" leak-check pre-push origin "$remote_url" 2>&1)
door_status=$?
set -e
check "the engine's push door passes: the file is in no commit" "$(is "$door_status" 0)"
note "selection-untracked" "scans a file git has not been told about" \
  "sees it only once a commit adds it" \
  "the same file, caught at the push that would publish it rather than before"

root=$(push_fixture selection-ignored)
printf 'dist/\n' > "$root/.gitignore"
mkdir -p "$root/dist"
printf '%s\n' "$key-7" > "$root/dist/page.html"
git -C "$root" add .gitignore
git -C "$root" commit -q -m "chore: ignore dist"
push_door "$root"
frozen_tree "$root"
check "the frozen script passes an ignored file" "$(is "$frozen_status" 0)"
check "the engine passes it too: an ignored file is in no commit" "$(is "$door_status" 0)"

# A tracked file under an ignored path is rule case 12 above, and a binary file rule case 15.
# The second lockfile name the frozen script skips, read the same way the first was.
root=$(push_fixture selection-lockfile)
printf 'example.com/%s v1 h1:x\n' "$repo" > "$root/go.sum"
git -C "$root" add -f -- go.sum
git -C "$root" commit -q -m "chore: add a lockfile"
push_door "$root"
frozen_tree "$root"
check "the frozen script skips go.sum as well" "$(is "$frozen_status" 0)"
check "the engine refuses the line it added, as the lockfile divergence above says" "$(is "$door_status" 3)"

echo "rule case 16: the suite's own output never names a private thing"
exec 1>&3 2>&4
wait "$tee_pid" 2> /dev/null || true
frozen_self=0
"$frozen_script" "$self_log" > /dev/null 2>&1 || frozen_self=$?
engine_self=0
"$engine" leak-check text "this suite's own log" < "$self_log" > /dev/null 2>&1 || engine_self=$?
check "the frozen script clears this suite's own log" "$(is "$frozen_self" 0)"
check "the engine clears it too" "$(is "$engine_self" 0)"

echo
echo "Divergences recorded ($(wc -l < "$divergences")), each one named where it was found:"
sed 's/^/  - /' "$divergences"
if [ -s "$disagreements" ]; then
  echo "Unexplained disagreements:"
  sed 's/^/  - /' "$disagreements"
else
  echo "Unexplained disagreements: none"
fi

summary "the port of check-private-names.sh to private-names.toml"

# INFRA-1413 selection probe, never merged.
