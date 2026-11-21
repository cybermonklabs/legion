# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/output.sh: what a run prints and reports. Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# --- What a run prints and reports. Each finding and each UNKNOWN is printed once, its fields with
# tabs and line breaks turned to spaces, the form every report field takes, so a refusal's
# multi-line text stays one line and one field. ---

FINDINGS=() UNKNOWNS=()
declare -A RECORDED=()
first_line() { sed -n '1{s/[[:cntrl:]]/ /g;p;}' "$1"; }
# last_line: a command's refusal where it prints lines before it, as agent-secrets prints the
# sign-in it uses before the failure.
last_line() { tail -n 1 "$1" | sed 's/[[:cntrl:]]/ /g'; }
# finding PRINCIPAL RIGHT ROLE RESOURCE PERMISSIONS GRANTED_AT: one finding, printed as PRINCIPAL and
# RIGHT, the sentence; the report carries the four parts beside it (the header says each), with
# PERMISSIONS separated by the unit separator ($US), never a comma: an RBAC label can hold one
# ("create pods in ns, guarding ns/a, ns/b"). Two findings of one principal and one sentence are one.
finding() {
  local principal="${1//[$'\t\r\n']/ }" right="${2//[$'\t\r\n']/ }" key
  local role="${3//[$'\t\r\n']/ }" resource="${4//[$'\t\r\n']/ }" permissions="${5//[$'\t\r\n']/ }" granted="${6//[$'\t\r\n']/ }"
  key="F$principal"$'\t'"$right"
  [ -z "${RECORDED[$key]:-}" ] || return 0
  RECORDED[$key]=1
  FINDINGS+=("$principal"$'\t'"$right"$'\t'"$role"$'\t'"$resource"$'\t'"$permissions"$'\t'"$granted")
  echo "FINDING $principal: $right"
}
unknown() {
  local step="${1//[$'\t\r\n']/ }" reason="${2//[$'\t\r\n']/ }" key
  key="U$step"$'\t'"$reason"
  [ -z "${RECORDED[$key]:-}" ] || return 0
  RECORDED[$key]=1
  UNKNOWNS+=("$step"$'\t'"$reason")
  echo "UNKNOWN $step: $reason"
}
pass() { echo "PASS $1: $2"; }
# write_report: --report, when it is named, from the findings and the UNKNOWNs so far.
write_report() {
  [ -n "$REPORT" ] || return 0
  printf '%s\n' ${FINDINGS[@]+"${FINDINGS[@]}"} >"$WORK/findings"
  printf '%s\n' ${UNKNOWNS[@]+"${UNKNOWNS[@]}"} >"$WORK/unknown"
  jq -n --arg run_at "$RUN_AT" --rawfile findings "$WORK/findings" --rawfile unknown "$WORK/unknown" '{
    run_at: $run_at,
    findings: [$findings | split("\n")[] | select(length > 0) | split("\t")
      | {principal: .[0], right: .[1], role: .[2], resource: .[3],
         permissions: ((.[4] // "") | split("\u001f") | map(select(length > 0))), granted_at: (.[5] // "")}],
    unknown: [$unknown | split("\n")[] | select(length > 0) | split("\t") | {step: .[0], reason: .[1]}]}' >"$WORK/report.json"
  cp "$WORK/report.json" "$REPORT"
}
# crash_report STATUS: on_exit's work for a run that stopped before its end.
crash_report() {
  unknown run "the run stopped before its end (exit $1), so every step after the last it printed is unread"
  write_report
  echo "operator-token-minters: stopped before its end, ${#UNKNOWNS[@]} unknown: exit 2"
}
# mark, then unchanged: whether a step recorded no finding and no UNKNOWN since its mark, so it
# prints PASS only then.
mark() { MARK_F="${#FINDINGS[@]}" MARK_U="${#UNKNOWNS[@]}"; }
unchanged() { [ "${#FINDINGS[@]}" -eq "$MARK_F" ] && [ "${#UNKNOWNS[@]}" -eq "$MARK_U" ]; }
have() { command -v "$1" >/dev/null; }
# The unit separator joins a record's fields; rec.jq says why. JQ_REC, prepended to a small inline
# jq program, defines rec there as `include "rec";` does in the .jq files.
US=$'\x1f'
JQ_REC="$(<"$LIB/rec.jq")"
