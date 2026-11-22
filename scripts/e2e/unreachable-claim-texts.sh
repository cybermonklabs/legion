#!/usr/bin/env bash
# Checks w7 and w11 of this change's verification: the texts that say the daemon relaunches a claim
# Envoy cannot reach (R5), read at one commit. w7 reads the architect's prompt, envoy_publish's tool
# description, the documentation site's Envoy page, Envoy's architecture page and the listener's
# architecture note; w11 reads the controller's and the Envoy skills. Each clause prints
# `PASS|FAIL <check> <clause>: <evidence>`, each check a count line, and the script exits 1 when a
# clause fails.
#
#   bash scripts/e2e/unreachable-claim-texts.sh [--rev <rev>] [--check w7|w11]
#
# It reads every file with `git show <rev>:<path>`, so it needs git and bash alone and starts
# nothing. The checks' negative control is the same run at the base (`--rev <base>`), where each
# old line is there and none of the new ones, so most clauses fail.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
rev=HEAD
only=''
while [ $# -gt 0 ]; do
  case "$1" in
    --rev)
      rev=${2:?--rev names no commit}
      shift 2
      ;;
    --check)
      only=${2:?--check names no check}
      shift 2
      ;;
    *)
      echo "usage: $0 [--rev <rev>] [--check w7|w11]" >&2
      exit 2
      ;;
  esac
done
sha=$(git -C "$root" rev-parse --verify "$rev^{commit}") || {
  echo "--rev '$rev' names no commit" >&2
  exit 2
}

passed=0
failed=0
check=''

# text is the file at path as the commit holds it, empty where it holds none.
text() { git -C "$root" show "$sha:$1" 2>/dev/null || true; }

clause() {
  local name=$1 ok=$2 evidence=$3
  if [ "$ok" = yes ]; then
    passed=$((passed + 1))
    echo "PASS $check $name: $evidence"
  else
    failed=$((failed + 1))
    echo "FAIL $check $name: $evidence"
  fi
}

# holds path phrase...: whether the file holds every phrase, in one line.
holds() {
  local path=$1 line phrase all
  shift
  while IFS= read -r line; do
    all=yes
    for phrase in "$@"; do
      case "$line" in *"$phrase"*) ;; *) all=no ;; esac
    done
    if [ "$all" = yes ]; then
      echo yes
      return
    fi
  done < <(text "$path")
  echo no
}

# lacks path phrase: whether no line of the file holds phrase.
lacks() {
  case "$(text "$1")" in *"$2"*) echo no ;; *) echo yes ;; esac
}

# qualified path old rule: whether every line holding old also holds rule.
qualified() {
  local path=$1 old=$2 rule=$3 line
  while IFS= read -r line; do
    case "$line" in
      *"$old"*) case "$line" in *"$rule"*) ;; *) echo no; return ;; esac ;;
    esac
  done < <(text "$path")
  echo yes
}

# controllerRelaunch paths...: the lines that have the controller relaunch a claim or name the
# legion tool's relaunch, as path:line.
controllerRelaunch() {
  local path line found=''
  for path in "$@"; do
    while IFS= read -r line; do
      case "$line" in
        *"controller relaunches"* | *"controller decides whether to relaunch"* | *"tool's relaunch"* | \
          *"tool's \`relaunch\`"* | *"case for \`relaunch\`"* | *"you relaunch that claim"*)
          found+="$path: ${line:0:120}; "
          ;;
      esac
    done < <(text "$path")
  done
  echo "$found"
}

architect=packages/daemon/internal/prompts/go/architect-common.md
contract=packages/envoy-client/src/tool-contract.ts
site=docs/site/src/content/docs/dispatch/envoy.md
architecture=packages/envoy/docs/architecture.md
listener=.dispatch/architecture/envoy-listener.md
controller=skills/legion-controller/SKILL.md
wakes=skills/legion-controller/references/wakes.md
session=skills/legion-controller/references/session.md
envoy=skills/envoy/SKILL.md
roles=skills/envoy/references/roles.md
legion=skills/envoy/references/legion.md
removed=skills/legion-controller/references/unreachable-claims.md

w7() {
  check=w7
  local found
  clause new-line-architect-prompt "$(holds "$architect" "nothing replaces it while its last registration is 20 minutes old or less")" "$architect"
  clause architect-prompt-names-no-unreachable-notice "$(lacks "$architect" "claim-out-of-reach")" "$architect"
  clause new-line-envoy-publish "$(holds "$contract" "skill://envoy/references/legion.md says what happens past that" "20 minutes old or less")" "$contract"
  clause new-line-site-envoy-page "$(holds "$site" "skills/envoy/references/legion.md" "20 minutes old or less")" "$site"
  clause new-line-envoy-architecture "$(holds "$architecture" "skills/envoy/references/legion.md" "20 minutes old or less")" "$architecture"
  clause new-line-listener-note "$(holds "$listener" "skills/envoy/references/legion.md" "20 minutes old or less")" "$listener"
  clause old-line-envoy-publish-qualified "$(qualified "$contract" "start no replacement for that session" "20 minutes old or less")" "'start no replacement for that session' only beside the 20-minute rule"
  clause old-line-site-not-dead-qualified "$(qualified "$site" "It is not dead." "20 minutes old or less")" "'It is not dead.' only beside the 20-minute rule"
  clause old-line-envoy-architecture-qualified "$(qualified "$architecture" "nothing starts a replacement for it" "20 minutes old or less")" "'nothing starts a replacement for it' only beside the 20-minute rule"
  clause old-line-listener-note-qualified "$(qualified "$listener" "nothing starts a replacement for it" "20 minutes old or less")" "'nothing starts a replacement for it' only beside the 20-minute rule"
  clause old-line-architect-prompt-qualified "$(qualified "$architect" "it keeps your role, nothing replaces it" "20 minutes old or less")" "'it keeps your role, nothing replaces it' only beside the 20-minute rule"
  found=$(controllerRelaunch "$architect" "$contract" "$site" "$architecture" "$listener")
  clause no-controller-relaunch "$([ -z "$found" ] && echo yes || echo no)" "${found:-no line has the controller relaunch a claim or names a relaunch of the legion tool}"
}

w11() {
  check=w11
  local found links
  clause r2-controller-line "$(holds "$controller" "A restarted session reads first whether its last turn was cut")" "$controller"
  clause controller-one-sentence "$(holds "$controller" "is the daemon's: it relaunches that claim on its same session and sends what a relaunch cannot fix to #fleet-alerts" "never \`fresh_session\` it")" "$controller"
  clause new-line-envoy-skill "$(holds "$envoy" "What becomes of a Legion claim it holds past that is in" "skill://envoy/references/legion.md")" "$envoy"
  clause new-line-roles "$(holds "$roles" "What becomes of a Legion claim past that is in" "skill://envoy/references/legion.md")" "$roles"
  clause new-line-legion "$(holds "$legion" "more than 20 minutes is unreachable" "the daemon relaunches it on that same session")" "$legion"
  clause controller-keeps-20-minutes "$(holds "$controller" "Start no replacement for an \`unresponsive\` session whose \`last_seen\` is 20 minutes old or less")" "$controller"
  clause envoy-skill-keeps-20-minutes "$(holds "$envoy" "Start no replacement for it" "20 minutes old or less")" "$envoy"
  clause roles-keeps-20-minutes "$(holds "$roles" "Start no replacement for that session" "20 minutes old or less")" "$roles"
  clause legion-relaunch-replaces-no-session "$(holds "$legion" "A relaunch replaces no session")" "$legion"
  clause old-line-keep-no-notes "$(lacks "$controller" "Keep no notes between turns, because a restarted session has only")" "$controller"
  clause old-line-envoy-skill-replacement "$(lacks "$envoy" "Start no replacement for it, take no role claim and no issue claim from it")" "$envoy"
  clause old-line-envoy-skill-taking "$(lacks "$envoy" "rather than starting a replacement for it or taking anything from it")" "$envoy"
  clause old-line-roles "$(lacks "$roles" "start no replacement for that session, and take nothing from it")" "$roles"
  clause old-line-legion "$(lacks "$legion" "it keeps the role, nothing replaces it")" "$legion"
  clause old-line-wakes-fresh-session "$(lacks "$wakes" "shows its session \`gone\`, \`fresh_session\` that architect")" "$wakes"
  clause unreachable-claims-md-gone "$(git -C "$root" cat-file -e "$sha:$removed" 2>/dev/null && echo no || echo yes)" "$removed"
  links=$(git -C "$root" grep -l "unreachable-claims" "$sha" -- skills/ docs/ 2>/dev/null | tr '\n' ' ' || true)
  clause nothing-links-it "$([ -z "$links" ] && echo yes || echo no)" "${links:-no file under skills/ or docs/ names unreachable-claims}"
  found=$(controllerRelaunch "$controller" "$wakes" "$session" "$envoy" "$roles" "$legion")
  clause no-controller-relaunch "$([ -z "$found" ] && echo yes || echo no)" "${found:-no line has the controller relaunch a claim or names a relaunch of the legion tool}"
}

echo "unreachable claim texts at $sha"
case "$only" in
  '') w7; w11 ;;
  w7) w7 ;;
  w11) w11 ;;
  *) echo "--check takes w7 or w11" >&2; exit 2 ;;
esac
echo "$passed of $((passed + failed)) clauses pass, $failed fail"
[ "$failed" -eq 0 ]
