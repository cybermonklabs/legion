# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/output.sh: what a run prints and reports. Sourced by
# operator-token-minters.sh, whose header says what each step checks.
# --- What a run prints and reports. Each finding and each UNKNOWN is printed once, its fields with
# tabs and line breaks turned to spaces, the form every report field takes, so a refusal's
# multi-line text stays one line and one field. The broker's pod fence adds CREDITS and RESIDUALS
# (broker-fence.sh's credit), which the report lists too. ---

FINDINGS=() UNKNOWNS=() CREDITS=() RESIDUALS=()
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
PASSES=0
pass() { PASSES=$((PASSES + 1)); echo "PASS $1: $2"; }
# The step census. operator-token-minters.sh names each step file it sources (STEP_FILES) and
# brackets each with census_begin and census_end. A step reports what it read: a PASS, a FINDING, an
# UNKNOWN, a credit or a residual; one that prints none of them says why (census_quiet, as a step
# whose flag is not given does). A step file that ran and did neither is an UNKNOWN naming it: an
# empty or emptied file sources as a clean no-op under set -e, and would else read as a step that
# found nothing. A step file that never ran is one too (census_close). STEP_STATE keeps each file's
# state for --report's `steps`: reported, quiet: <why>, silent, or not run.
declare -A STEP_STATE=()
census_count() { echo "${#FINDINGS[@]} ${#UNKNOWNS[@]} $PASSES ${#CREDITS[@]} ${#RESIDUALS[@]}"; }
census_begin() { CENSUS_MARK="$(census_count)" CENSUS_QUIET=""; }
census_quiet() { CENSUS_QUIET="$1"; }
census_end() {
  if [ "$(census_count)" != "$CENSUS_MARK" ]; then
    STEP_STATE[$1]=reported
  elif [ -n "$CENSUS_QUIET" ]; then
    STEP_STATE[$1]="quiet: $CENSUS_QUIET"
  else
    STEP_STATE[$1]=silent
    unknown "run" "the step file $1 ran and reported nothing, no PASS, FINDING, UNKNOWN or credit, and gave no reason, so what it checks is unread"
  fi
}
census_close() {
  local file
  for file in "${STEP_FILES[@]}"; do
    [ -n "${STEP_STATE[$file]:-}" ] && continue
    STEP_STATE[$file]="not run"
    unknown "run" "the step file $file never ran, so what it checks is unread"
  done
}
# steps_json: --report's `steps`, each step file the run names in its order, with its census state.
steps_json() {
  local file
  for file in ${STEP_FILES[@]+"${STEP_FILES[@]}"}; do printf '%s\t%s\n' "$file" "${STEP_STATE[$file]:-not run}"; done |
    jq -R -s -c '[split("\n")[] | select(length > 0) | split("\t") | {step: .[0], state: .[1]}]'
}
# operator_res: the operator's cluster's label in a finding, a credit or a residual,
# cluster:<project>/<location>/<name>, as every cluster of the pool is labelled (README, "Labels"):
# the location is the one live: clusters holds it to (--location, or the project's list's when
# --location is not given), and `-`, Google's any-location, while that is unknown. The code steps
# run before live: clusters, so without --location they record `-`, and write_report gives that
# label the location live: clusters read, when it read one.
operator_res() { printf 'cluster:%s/%s/%s' "${PROJECT:--}" "${OPERATOR_LOCATION:-${LOCATION:--}}" "${CLUSTER:--}"; }
# write_report: --report, when it is named, from the findings, the credits and their residuals, the
# UNKNOWNs so far, the run's allowances (allowances_json, in operator-token-minters.sh) and the step
# census (steps_json). A report it cannot write is named by its flag, never its file. A label a code
# step recorded before live: clusters read the operator's cluster's location (operator_res, `-` for
# the location) is given the location live: clusters read, when it read one.
write_report() {
  [ -n "$REPORT" ] || return 0
  printf '%s\n' ${FINDINGS[@]+"${FINDINGS[@]}"} >"$WORK/findings"
  printf '%s\n' ${CREDITS[@]+"${CREDITS[@]}"} >"$WORK/credits"
  printf '%s\n' ${RESIDUALS[@]+"${RESIDUALS[@]}"} >"$WORK/residuals"
  printf '%s\n' ${UNKNOWNS[@]+"${UNKNOWNS[@]}"} >"$WORK/unknown"
  jq -n --arg run_at "$RUN_AT" --rawfile findings "$WORK/findings" --rawfile credits "$WORK/credits" \
    --rawfile residuals "$WORK/residuals" --rawfile unknown "$WORK/unknown" --argjson allowances "$(allowances_json)" \
    --argjson steps "$(steps_json)" --arg pending "cluster:${PROJECT:--}/-/${CLUSTER:--}" --arg operator "$(operator_res)" '
    def operator_label: if . == $pending then $operator else . end;
    {run_at: $run_at,
    findings: [$findings | split("\n")[] | select(length > 0) | split("\t")
      | {principal: .[0], right: .[1], role: .[2], resource: (.[3] | operator_label),
         permissions: ((.[4] // "") | split("\u001f") | map(select(length > 0))), granted_at: (.[5] // "")}],
    credited: [$credits | split("\n")[] | select(length > 0) | split("\t")
      | {principal: .[0], right: .[1], resource: (.[2] | operator_label), granted_at: .[3], policy: .[4], declared: .[5], live: .[6]}],
    residuals: [$residuals | split("\n")[] | select(length > 0) | split("\t")
      | {principal: .[0], right: .[1], resource: (.[2] | operator_label), granted_at: .[3], residual: .[4], tracked_by: .[5]}],
    unknown: [$unknown | split("\n")[] | select(length > 0) | split("\t") | {step: .[0], reason: .[1]}],
    allowances: $allowances, steps: $steps}' >"$WORK/report.json"
  cp "$WORK/report.json" "$REPORT" 2>/dev/null || { echo "operator-token-minters: --report cannot be written" >&2; return 1; }
}
# crash_report STATUS: on_exit's work for a run that stopped before its end.
crash_report() {
  unknown run "the run stopped before its end (exit $1), so every step after the last it printed is unread"
  write_report
  echo "operator-token-minters: stopped before its end, ${#UNKNOWNS[@]} unknown: exit 2"
}
# mark, then unchanged: whether a step recorded no finding and no UNKNOWN since its mark, so it
# prints PASS only then; credited and allowed, how many credits and residual ALLOWANCEs it recorded
# since, which its PASS names.
mark() { MARK_F="${#FINDINGS[@]}" MARK_U="${#UNKNOWNS[@]}" MARK_C="${#CREDITS[@]}" MARK_A="${#RESIDUALS[@]}"; }
unchanged() { [ "${#FINDINGS[@]}" -eq "$MARK_F" ] && [ "${#UNKNOWNS[@]}" -eq "$MARK_U" ]; }
credited() { echo "$((${#CREDITS[@]} - MARK_C))"; }
allowed() { echo "$((${#RESIDUALS[@]} - MARK_A))"; }
have() { command -v "$1" >/dev/null; }
# The unit separator joins a record's fields; rec.jq says why. JQ_REC, prepended to a small inline
# jq program, defines rec there as `include "rec";` does in the .jq files.
US=$'\x1f'
JQ_REC="$(<"$LIB/rec.jq")"
