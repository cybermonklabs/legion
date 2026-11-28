# shellcheck shell=bash
# shellcheck disable=SC2016,SC2034,SC2154 # the world, its names, its helpers and the output and status a run sets are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-run-2.sh: the rows of operator-token-minters.test.sh on the run
# as a whole: reads that saw nothing, a copy missing a part or with a part that fails, the
# allowances, text pasted into a flag, the token Secrets' lists, a cluster too large for one
# argument, a pick of the reviews that fails, and the test's own readings (its pipes, want_recorded
# and each's bound). Sourced by the test after its world and helpers; its header says what the fakes
# model. The rows:
#
#   (ba) a get-ancestors answer of [] reads UNKNOWN, and so does another cluster that lists no
#        ServiceAccount outside its system namespaces though a binding names a subject there;
#   (bc) a copy of the script missing output.sh, or missing analysis.jq, prints "UNKNOWN run" naming
#        it, writes that to --report, and exits 2 before any step runs;
#   (cj) --service-agent naming an agent's own Google service account is a usage error, and a run
#        given --service-agent, --rbac-only-member and --fleet none writes each into --report's
#        allowances;
#   (co) a text pasted alone, as the value of a flag that names a thing where it is not of that
#        flag's form, or as a file flag's file is printed in neither stdout, stderr nor --report: a
#        flag that names a thing refuses a value of another form, naming the flag and never its
#        text, and a file flag's file is named by the flag and its place;
#   (cp) the list of a guarded namespace's token Secrets is asked in the API server's Table form,
#        whose response carries no data field, so no Secret's data reaches the box, the run's work
#        directory or its output;
#   (cq) a credential of a name flag's form shaped as a credential its issuer marks (a ghp_ token,
#        a github_pat_ token, an AIza key and a Slack token as --signing-secret, an AWS access key
#        ID as --secret) is refused too, naming the flag and never the value, and is printed in
#        neither stdout, stderr nor --report;
#   (cr) a list of a guarded namespace's token Secrets that kubectl prints without the Table's
#        columns, as it does when a server or proxy that cannot make the Table answers with the
#        Secrets whole, reads UNKNOWN for that namespace, and the RBAC step does not pass;
#   (cs) a cluster whose principals and rights are each more than one argument of a command line
#        holds is reviewed whole: they reach jq through files and its stdin, never its arguments;
#   (cv) a part that builds the custom resources' rights or the binding namespaces' rights and
#        fails or prints nothing reads UNKNOWN naming those rights, and the RBAC step does not
#        pass;
#   (cx) with_rights handed an empty rights value refuses it and stops the run, UNKNOWN, exit 2;
#   (da) no line of the check, its parts or this test's own files reads a status from a pipe into
#        a grep that stops early (-q or -m), which pipefail and SIGPIPE can read as no match;
#   (db) a pick of the reviews to judge that stops part way (its awk under a 2 KiB file size limit)
#        reads UNKNOWN naming it, exit 2, and the RBAC step does not pass;
#   (df) want_recorded reads the check's last line and its FINDING and UNKNOWN lines as ten whole
#        outputs of real check runs hold them (testdata/operator-token-minters-runs/): the counts
#        stand on each output that reaches the exit block, a deleted or added line is named, and an
#        output that ends elsewhere is named by its last line; want_line_with and
#        want_own_principals each fail their control and pass the case beside it;
#   (dg) each runs at most min(8, OPERATOR_MINTERS_TEST_SLOTS) of a row's items at once, so one at
#        a time under one slot.

# --- (ba) Two reads that saw nothing. ---
saw_nothing_row() {
  base_world
  case "$1" in
    ancestors)
      mkdir -p "$world/ancestors"
      echo '[]' >"$world/ancestors/$project.json"
      run
      want_status 2 "a get-ancestors answer of []"
      want_text "UNKNOWN live: who can set IAM on the ancestors of project $project: gcloud projects get-ancestors $project answered no ancestor holding the project itself" \
        "a get-ancestors answer of []"
      ;;
    accounts)
      add_cluster other-cluster us-east1
      edit "$world/clusters/other-cluster/serviceaccounts.json" 'map(select(.namespace | test("^(kube-system|gke-.*|gmp-.*)$")))'
      subject_grant other-cluster "$(user_subject ops-bot@example-project.iam.gserviceaccount.com)" get "" configmaps "" default ""
      run
      want_status 2 "another cluster listing no ServiceAccount outside its system namespaces"
      want_text "UNKNOWN live: Workload Identity on $project/us-east1/other-cluster: the cluster lists no ServiceAccount outside its system namespaces, though every cluster has default/default" \
        "another cluster listing no ServiceAccount outside its system namespaces"
      ;;
  esac
}
if row ba; then
  each saw_nothing_row ancestors accounts
  passed "(ba) a get-ancestors answer of [], and a cluster listing no ServiceAccount outside its system namespaces, each read UNKNOWN"
fi

# --- (bc) A copy missing a part reads "UNKNOWN run" before any step runs. ---
missing_part_row() {
  local script report="$temporary_dir/bc-$1.json"
  base_world
  script="$(script_copy "bc-$1")"
  rm "$(dirname "$script")/operator-token-minters/$1"
  run_script="$script" run --report "$report"
  want_status 2 "a copy without $1"
  want_text "UNKNOWN run: the parts $1 of " "a copy without $1"
  jq -e --arg p "$1" '.findings == [] and any(.unknown[]; .step == "run" and (.reason | contains($p)))' "$report" >/dev/null 2>&1 ||
    fail "a copy without $1: the report does not hold the UNKNOWN run: $(cat "$report" 2>&1)"
}
if row bc; then
  each missing_part_row output.sh analysis.jq
  passed "(bc) a copy missing output.sh or analysis.jq prints UNKNOWN run naming it, reports it, and exits 2"
fi

# --- (cj) The allowances: --service-agent names a Google service agent alone, and every allowance a
# run is given (--service-agent, --rbac-only-member, --fleet none, --credentials-arg, which chooses
# how each cluster is reached) is written into --report, so a reader can hold each to what was
# accepted. ---
allowance_row() {
  local report="$temporary_dir/allowances.json"
  base_world
  case "$1" in
    not-an-agent)
      run --service-agent "$worker_gsa"
      want_status 2 "--service-agent naming an agent's own account"
      want_text "operator-token-minters: --service-agent takes a Google service agent, service-<number>@gcp-sa-<product>.iam.gserviceaccount.com; one of its values is not one" \
        "--service-agent naming an agent's own account"
      ! has_prefix "$worker_gsa" || fail "the usage error printed the value given: $worker_gsa"
      ;;
    written)
      edge_member
      edit "$world/fleet.json" '.memberships = [.memberships[] | select(.id == "edge")]'
      run --report "$report" --service-agent "service-$number@gcp-sa-cloudbuild.iam.gserviceaccount.com" --rbac-only-member edge --fleet none \
        --credentials-arg=--internal-ip
      jq -e --arg agent "service-$number@gcp-sa-cloudbuild.iam.gserviceaccount.com" '.allowances == [
          {flag: "--service-agent", value: $agent}, {flag: "--rbac-only-member", value: "edge"}, {flag: "--fleet", value: "none"},
          {flag: "--credentials-arg", value: "--internal-ip"}]' "$report" >/dev/null ||
        fail "the report does not hold the four allowances the run was given: $(jq -c '.allowances // "absent"' "$report")"
      ;;
  esac
}
if row cj; then
  each allowance_row not-an-agent written
  passed "(cj) --service-agent refuses an address that is no Google service agent, and every allowance a run is given, --credentials-arg included, is written into --report"
fi

# --- (co) No text given is printed unless it is of its flag's form: a token pasted alone, as the
# value of a flag that names a thing, of another form than the flag's, or as a file flag's file
# (--rbac-render, --iam-preview, --report, the last both where it can be written and where it
# cannot) is in neither stdout, stderr nor --report. A flag that names a thing refuses it, naming
# the flag and never the text; a file flag's file is named by the flag and its place. Each item
# runs in a directory of its own, with stdout and stderr apart, and writes --report there. A
# credential of a flag's form is (cq)'s, whose items are cases of pasted_row too, each marked by
# shaped_canary. shaped_canary stays 16 characters: AKIA${shaped_canary} is an AWS access key ID
# only at that length, and AIza${shaped_canary}0123456789abcdefghi holds exactly the 35 characters
# after AIza that credential_prefix needs. ---
pasted_canary="eyJhbGciOiJFZERTQSJ9.PASTED-CANARY-5c1e.sig"
shaped_canary=PASTEDCANARY5C1E
# shaped FLAG VALUE: a (cq) item of pasted_row, whose locals it sets: VALUE is of FLAG's form, so the
# form alone would admit it, and shaped as a credential its issuer marks, so the run refuses it.
shaped() {
  args=("$1" "$2") canary="$shaped_canary" what="the $item given as $1" want="operator-token-minters: $1 is "
  want_shape="; its value is shaped as a credential its issuer marks"
}
pasted_row() {
  local item="$1" own="$temporary_dir/co-$1" args=() full report want want_shape="" want_exit=2 where
  local canary=PASTED-CANARY-5c1e what="a pasted credential ($1)"
  base_world
  mkdir -p "$own"
  report="$own/report.json"
  case "$1" in
    alone) args=("$pasted_canary") want="operator-token-minters: argument " ;;
    unknown-flag) args=("--no-such-flag=$pasted_canary") want="operator-token-minters: unknown flag --no-such-flag" ;;
    rbac-render) args=("--$1" "$pasted_canary") want="UNKNOWN code: RBAC: --rbac-render entry 2 cannot be read" ;;
    iam-preview) args=("--$1" "$pasted_canary") want="UNKNOWN code: IAM: --iam-preview entry 2 cannot be read" ;;
    report) report="$own/$pasted_canary" want="operator-token-minters: no finding and nothing unknown: exit 0" want_exit=0 ;;
    report-unwritable) report="$own/$pasted_canary/report.json" want="operator-token-minters: --report cannot be written" ;;
    ghp-token) shaped --signing-secret "ghp_${shaped_canary}0123456789abcdefghij" ;;
    github-pat) shaped --signing-secret "github_pat_11${shaped_canary}abcdef_0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVW" ;;
    google-api-key) shaped --signing-secret "AIza${shaped_canary}0123456789abcdefghi" ;;
    slack-token) shaped --signing-secret "xoxb-${shaped_canary}-1234567890-abcdefghij" ;;
    aws-access-key-id) shaped --secret "AKIA${shaped_canary}" ;;
    *) args=("--$1" "$pasted_canary") want="operator-token-minters: --$1 " ;;
  esac
  mapfile -t full < <(full_args)
  status=0
  (cd "$own" && env PATH="$fake_bin:$PATH" OPERATOR_MINTERS_WORLD="$world" TMPDIR="$temporary_dir" bash "$check" "${full[@]}" \
    --report "$report" ${args[@]+"${args[@]}"} >"$own/stdout" 2>"$own/stderr") || status=$?
  output="$(cat "$own/stdout" "$own/stderr")"
  for where in stdout stderr; do
    ! grep -qF -- "$canary" "$own/$where" || fail "$what is in $where: $(grep -F -- "$canary" "$own/$where")"
  done
  [ ! -f "$report" ] || ! grep -qF -- "$canary" "$report" || fail "$what is in --report: $(cat "$report")"
  want_text "$want" "$what"
  [ -z "$want_shape" ] || want_text "$want_shape" "$what"
  want_status "$want_exit" "$what"
}
if row co; then
  each pasted_row alone unknown-flag project pool-project cluster location namespace service-account gsa agent-namespace \
    broker-account broker-workload sql-instance db-secret broker-state signing-secret secret rbac-only-member credentials-arg \
    analysis-scope fleet service-agent guarded rbac-render iam-preview report report-unwritable
  passed "(co) a text pasted alone, as a name flag's value of another form or as a file flag's file is printed in neither stdout, stderr nor --report"
fi

# printed_secret_data: whether the run printed a token Secret's data, as the world holds it
# (SECRET-DATA-CANARY-7f3a) or as the API server sends it, base64 (test-fakes.sh's secret_data_b64).
printed_secret_data() { has_prefix "SECRET-DATA-CANARY-7f3a" || has_prefix "$(printf '%s' SECRET-DATA-CANARY-7f3a | base64)"; }

# --- (cp) The token Secrets are listed in the Table form: the API server's response to each list
# carries no data field, and no Secret's data is written to the work directory or printed. The fake
# kubectl holds each Secret's data, as a real API server does, answers each list with what the API
# server sends kubectl for it (the Table for the default output; the Secrets whole, data included,
# for -o json, -o name and -o jsonpath), records each response, and on each later call looks for
# that data under the run's work directory; this row runs in a temporary directory of its own, so
# that look sees this run's work directory alone. ---
if row cp; then
  base_world
  # One in each guarded namespace: the shell empties a redirect's file before kubectl runs, so the
  # list's file in the work directory holds the last namespace's Secrets when later calls look.
  jq -n '[{namespace: "envoy-operator", name: "envoy-operator-token-legacy"}, {namespace: "secrets-broker", name: "secrets-broker-token-legacy"}]' \
    >"$world/clusters/operator-cluster/token-secrets.json"
  : >"$world/clusters/operator-cluster/token-secret-data"
  own_tmp="$temporary_dir/cp-tmp"
  mkdir -p "$own_tmp"
  mapfile -t cp_args < <(full_args)
  status=0
  output="$(env PATH="$fake_bin:$PATH" OPERATOR_MINTERS_WORLD="$world" TMPDIR="$own_tmp" bash "$check" "${cp_args[@]}" 2>&1)" || status=$?
  cp_failures=""
  responses="$world/clusters/operator-cluster/token-secret-responses"
  # One list for each guarded namespace, envoy-operator's and secrets-broker's: a row that read no
  # response would pass with nothing seen.
  listed="$(if [ -e "$responses" ]; then wc -l <"$responses"; else echo 0; fi)"
  [ "$listed" -eq 2 ] || cp_failures+="the API server answered $listed lists of token Secrets, not one for each of the 2 guarded namespaces "
  data_paths="$(if [ -e "$responses" ]; then jq -r 'paths | select(.[-1] == "data") | map(tostring) | join(".")' "$responses" | sort -u | paste -sd ' '; fi)"
  [ -z "$data_paths" ] || cp_failures+="the API server's response to the list of token Secrets carried a data field: $data_paths "
  [ ! -e "$world/clusters/operator-cluster/secret-data-asked" ] ||
    cp_failures+="the check asked the API server for token Secrets whole, data included: $(tr '\n' ';' <"$world/clusters/operator-cluster/secret-data-asked") "
  [ ! -e "$world/clusters/operator-cluster/secret-data-in-work" ] ||
    cp_failures+="a token Secret's data was in the run's work directory: $(sort -u "$world/clusters/operator-cluster/secret-data-in-work" | sed "s|^$own_tmp/||" | tr '\n' ' ') "
  ! printed_secret_data || cp_failures+="the check printed a token Secret's data "
  [ -z "$cp_failures" ] || fail "$cp_failures"
  passed "(cp) the token Secrets of a guarded namespace are listed in the Table form: no response carries a data field, and no Secret's data is kept in the work directory or printed"
fi

# --- (cq) A credential of a name flag's form is refused too when it is shaped as a credential its
# issuer marks (operator-token-minters.sh's credential_prefix). Each value is of its flag's form, so
# the form alone would admit it, and the run would print it: --signing-secret's in the signing key's
# step name (checks.sh), --secret's in the admin tag's. Each is refused, exit 2, naming the flag and
# never the value, which is in neither stdout, stderr nor --report. Each is a case of pasted_row,
# marked by shaped_canary. Run by hand, the control is the script with the credential_prefix line of
# refuse_form taken out: each item is printed, and the row fails naming where. ---
if row cq; then
  each pasted_row ghp-token github-pat google-api-key slack-token aws-access-key-id
  passed "(cq) a credential of a name flag's form, shaped as a credential its issuer marks, is refused, naming the flag, and printed in neither stdout, stderr nor --report"
fi

# --- (cr) The token Secrets' list answered without the Table: the world's API server answers as a
# server or proxy that cannot make the Table does, with the Secrets whole, data included, and the
# fake kubectl prints them under NAME and AGE alone, as kubectl 1.36.4 does. Each guarded
# namespace's list reads UNKNOWN, naming the namespace, the run exits 2, and the RBAC step does not
# pass. Run by hand, the control is step-rbac.sh's list as it was before this row, with
# --no-headers and no header check, keeping the first column whatever the columns are: the RBAC
# step passes, the run exits 0, and the row fails naming that exit. ---
if row cr; then
  base_world
  jq -n '[{namespace: "envoy-operator", name: "envoy-operator-token-legacy"}, {namespace: "secrets-broker", name: "secrets-broker-token-legacy"}]' \
    >"$world/clusters/operator-cluster/token-secrets.json"
  : >"$world/clusters/operator-cluster/token-secrets-no-table"
  # The Secrets hold their data, so the server's answer carries it, as the comment above says.
  : >"$world/clusters/operator-cluster/token-secret-data"
  run
  for ns in envoy-operator secrets-broker; do
    want_rbac_unknown "the API server's list of token Secrets in $ns came without the Table's columns NAME TYPE DATA AGE, so it was not the Table form: a server that cannot make the Table sends the Secrets whole, data included, which kubectl read though it printed none" \
      "the list of $ns's token Secrets answered without the Table"
  done
  responses="$world/clusters/operator-cluster/token-secret-responses"
  if [ ! -e "$responses" ] || ! jq -e -s 'any(.[].items[]?; has("data"))' "$responses" >/dev/null; then
    fail "the world's server sent no token Secret's data, so the row does not hold the case its comment names"
  fi
  ! printed_secret_data || fail "the check printed a token Secret's data"
  passed "(cr) a list of token Secrets that kubectl prints without the Table's columns reads UNKNOWN for its namespace, and the RBAC step does not pass"
fi

# --- (cs) A cluster whose principals and rights are each more than one argument of a command line
# holds. Linux refuses an exec whose one argument is longer than MAX_ARG_STRLEN, 128 KiB, which the
# operator's cluster passes (3,183 principals and 508 rights). Here jq refuses an argument longer
# than 16 KiB as the kernel would, with bash's words and exit 126, and the world holds 150 more
# ServiceAccounts, so its principals alone pass that, as its rights already do. The run reviews
# them all and exits 0, as row (a) does. Run by hand, the control is reviews.sh as it was before
# this row, which passed the principals and the rights to jq as arguments: the reviews are not
# built, the run reads UNKNOWN, and the row fails naming it. ---
if row cs; then
  base_world
  cap=16384 cap_bin="$temporary_dir/cs-bin" real_jq="$(command -v jq)"
  mkdir -p "$cap_bin"
  printf '#!/usr/bin/env bash\nfor arg in "$@"; do\n  [ "${#arg}" -le %s ] || { echo "bash: %s: Argument list too long" >&2; exit 126; }\ndone\nexec %q "$@"\n' \
    "$cap" "$real_jq" "$real_jq" >"$cap_bin/jq"
  chmod +x "$cap_bin/jq"
  edit "$world/clusters/operator-cluster/serviceaccounts.json" '. + [range(150) | {namespace: "bulk", name: "account-\(.)"}]'
  echo bulk >>"$world/clusters/operator-cluster/namespaces"
  # The principals as review sends them, so the row fails rather than passes on a world too small.
  principals="$(jq -c '[.[] | "system:serviceaccount:\(.namespace):\(.name)" as $u
    | {principal: $u, user: $u, groups: ["system:serviceaccounts", "system:serviceaccounts:\(.namespace)", "system:authenticated"]}]' \
    "$world/clusters/operator-cluster/serviceaccounts.json")"
  [ "${#principals}" -gt "$cap" ] || fail "the world's principals are ${#principals} characters, no more than the $cap a jq argument holds here, so the row tests nothing"
  PATH="$cap_bin:$PATH" run
  ! has_prefix "UNKNOWN" || fail "a cluster whose principals and rights pass a command line's argument read UNKNOWN"
  want_status 0 "a cluster whose principals and rights pass a command line's argument"
  accounts="$(sed -n 's/^PASS live: RBAC on operator-cluster: [0-9]* principals (\([0-9]*\) ServiceAccounts outside the system namespaces.*/\1/p' <<<"$output")"
  [ "${accounts:-0}" -gt 150 ] || fail "the RBAC step reviewed ${accounts:-no} ServiceAccounts outside the system namespaces, not the world's 150 more"
  passed "(cs) a cluster whose principals and rights each pass a command line's argument is reviewed whole, and exits 0"
fi

# --- (cv) A part that builds a step's rights and fails, or prints nothing. A copy of the script
# whose custom-rights.jq or binding-rights.jq is `empty` (it prints nothing and exits 0) or an
# error reads UNKNOWN naming the rights it could not build, exits 2, and the RBAC step does not
# pass: the review never goes on without those rights. Run by hand, the control is the script and
# its parts as at cae4905130, whose with_rights read an empty argument as no rights: each empty
# item and custom-error pass the RBAC step and exit 0. A broken binding-rights.jq also breaks the
# code step's build of the renders' binding rights, which reads UNKNOWN naming them and does not
# pass; control, step-code-rbac.sh without that UNKNOWN: both binding items fail. The items that
# broke a pipeline of the name-limited rights' build (the reviewed principals' list, the IAM
# holders' awk) went with that build: binding_rights reads the bindings alone, through one jq. ---
broken_rights_row() {
  local script lib want
  base_world
  script="$(script_copy "cv-$1")"
  lib="$(dirname "$script")/operator-token-minters"
  case "$1" in
    custom-*) want="the rights on the custom resources could not be built, so none is reviewed: " ;;
    binding-*) want="the rights of the namespaces that hold a RoleBinding could not be built, so none is reviewed or read from the cluster's roles: " ;;
  esac
  case "$1" in
    custom-empty) echo 'empty' >"$lib/custom-rights.jq" ;;
    custom-error) echo 'error("a part broken by the test")' >"$lib/custom-rights.jq" ;;
    binding-empty) echo 'empty' >"$lib/binding-rights.jq" ;;
    binding-error) echo 'error("a part broken by the test")' >"$lib/binding-rights.jq" ;;
  esac
  run_script="$script" run
  want_rbac_unknown "$want" "a broken part of the rights ($1)"
  # A broken binding-rights.jq breaks the code step's build of the renders' binding rights too,
  # which must read UNKNOWN and not pass.
  case "$1" in
    binding-*)
      want_text "UNKNOWN code: RBAC: the rights of the namespaces the renders' RoleBindings are in could not be built" \
        "a broken part of the rights ($1), in the code step"
      ! has_prefix "PASS code: RBAC: " || fail "a broken part of the rights ($1): the code step passed without the binding rights"
      ;;
  esac
}
if row cv; then
  each broken_rights_row custom-empty custom-error binding-empty binding-error
  passed "(cv) a part that builds the custom resources' rights or the binding namespaces' rights and fails, or prints nothing, reads UNKNOWN naming it, exits 2, and the RBAC step does not pass; a broken binding-rights.jq reads UNKNOWN in the code step too, which does not pass"
fi

# --- (cx) with_rights' own refusal. Every rights value a step joins goes through with_rights, which
# refuses anything but two JSON arrays and returns 1, so the step's assignment stops the run under
# errexit rather than let it go on without the rights the argument owed. A copy whose
# webhook-backends.jq prints nothing leaves POWER_RIGHTS empty, which the CRD branch hands
# with_rights: the run stops, printing with_rights' refusal and `UNKNOWN run`, exit 2, and the RBAC
# step does not pass. Run by hand, the control is reviews.sh's with_rights as at cae4905130, which
# read the empty argument as no rights: the RBAC step passes and the run exits 0. ---
if row cx; then
  base_world
  script="$(script_copy cx)"
  echo 'empty' >"$(dirname "$script")/operator-token-minters/webhook-backends.jq"
  run_script="$script" run
  want_status 2 "with_rights handed an empty webhook list"
  want_text "with_rights takes two JSON arrays, and was given: , [" "with_rights handed an empty webhook list"
  want_text "UNKNOWN run: the run stopped before its end" "with_rights handed an empty webhook list"
  ! has_prefix "PASS live: RBAC on operator-cluster: " || fail "with_rights handed an empty webhook list: the RBAC step passed"
  passed "(cx) with_rights handed an empty rights value (a webhook list built as nothing) refuses it and stops the run, UNKNOWN, exit 2"
fi

# --- (da) No status read from a pipe into a grep that stops early. Under pipefail, `cut FILE |
# grep -q TEXT` can fail although TEXT is there: grep stops at its first match, and the writer's
# next write then fails the pipeline with SIGPIPE, more often the earlier the match and the busier
# the box. Row cz's later-refused item failed so twice in the merge queue on 2026-11-26 (the fake
# kubectl's holding read a batch's first subject that way). quiet_pipes finds such a line in the
# check, its parts and this test's own files, the fakes among them: a `|` before a grep given -q,
# --quiet, --silent, -m or --max-count, on one line or after a line that ends in `|`. The control is
# its own other half: a file holding the two forms and the two forms it allows (a process
# substitution, and `||`), where it finds the two and no other. ---
# quiet_pipes FILE...: each such line, as FILE:LINE: TEXT; comment lines are not read.
quiet_pipes() {
  awk '
    function stops(text, words, n, i) {
      n = split(text, words, /[[:space:]]+/)
      for (i = 1; i <= n; i++) {
        if (words[i] == "--") return 0
        if (words[i] ~ /^--(quiet|silent|max-count)/) return 1
        if (words[i] ~ /^-[[:alnum:]]*[qm]/) return 1
      }
      return 0
    }
    FNR == 1 { follows = 0 }
    /^[[:space:]]*#/ { follows = 0; next }
    {
      text = $0
      found = follows && match(text, /^[[:space:]]*grep[[:space:]][^|;&)]*/) && stops(substr(text, RSTART, RLENGTH))
      rest = text
      while (!found && match(rest, /(^|[^|])\|[[:space:]]*grep[[:space:]][^|;&)]*/)) {
        found = stops(substr(rest, RSTART, RLENGTH))
        rest = substr(rest, RSTART + RLENGTH)
      }
      if (found) print FILENAME ":" FNR ": " text
      follows = text ~ /[^|]\|[[:space:]]*$/
    }' "$@"
}
if row da; then
  mapfile -t sources < <(printf '%s\n' "$check" "$(dirname "$check")"/operator-token-minters/*.sh \
    "$script_dir/operator-token-minters.test.sh" "$script_dir"/operator-token-minters/test-*.sh | sort -u)
  found="$(quiet_pipes "${sources[@]}")"
  [ -z "$found" ] || fail "a status is read from a pipe into a grep that stops early, which pipefail and SIGPIPE can turn into no match; read its input through a process substitution or a here-string: $found"
  # The control's pipes are written as $p, so that this row's own lines hold none of the forms.
  control=quiet-pipes-control.sh p='|'
  printf '%s\n' "cut -f7 \"\$file\" $p grep -qxF -- \"\$subject\" || echo none" \
    "awk '{ print }' \"\$file\" $p" "  grep -m 1 -F -- \"\$subject\"" \
    "grep -qxF -- \"\$subject\" < <(cut -f7 \"\$file\")" \
    "[ -n \"\$subject\" ] || grep -q x \"\$file\"" >"$temporary_dir/$control"
  found="$(cd "$temporary_dir" && quiet_pipes "$control" | cut -d: -f1,2 | tr '\n' ' ')"
  [ "$found" = "$control:1 $control:3 " ] ||
    fail "quiet_pipes found ${found:-nothing} in its control, where lines 1 and 3 alone pipe into a grep that stops early"
  passed "(da) no line of the check, its parts or this test pipes into a grep that stops early (-q or -m) for its status; the control finds both forms and neither allowed one"
fi

# --- (db) A pick of the reviews to judge that fails. judge_reviews picks a cluster's allowed and
# errored reviews out of the answer with one awk, and both its callers run it as a condition, where
# errexit is off. A copy of the script whose pick runs under `ulimit -f 2`, as on a disk that fills,
# has the awk killed by SIGXFSZ after 2 KiB, in a world where legion/worker is bound to cluster-admin
# so that its allowed reviews are far more than that. The RBAC step reads UNKNOWN naming the pick,
# the run exits 2, and the step does not pass. Run by hand, the control is judge_reviews as at
# ac0627c8, which read no status of the awk: it judged the reviews written before the kill alone,
# and the run exited 1 with legion/worker's findings cut short (0 had none been written). ---
if row db; then
  base_world
  cluster_admin operator-cluster "$(sa_subject legion worker)"
  script="$(script_copy db)"
  plant "$(dirname "$script")/operator-token-minters/reviews.sh" "  awk -F\"\$US\" '\$1 != \"witness\" && " \
    "  small_awk -F\"\$US\" '\$1 != \"witness\" && "
  printf '%s\n' 'small_awk() { (ulimit -f 2 && exec awk "$@"); }' >>"$(dirname "$script")/operator-token-minters/reviews.sh"
  run_script="$script" run
  want_rbac_unknown "the reviews to judge could not be picked out of the API server's answer (awk exited " \
    "a pick of the reviews to judge stopped by a file size limit"
  passed "(db) a pick of the reviews to judge that stops part way reads UNKNOWN naming it, exits 2, and the RBAC step does not pass"
fi

# --- (df) want_recorded's reading, held to ten whole outputs of real check runs captured at
# 98caff1b, in testdata/operator-token-minters-runs/ (its README names each run and the commands
# that captured it). Each is read into output as run_only holds it. recorded_differ prints nothing
# for each of the six that reach the exit block; the counts text for each of those with its first
# FINDING or UNKNOWN line deleted, and for the clean one with a FINDING line added; and, for each of
# the four that end elsewhere (the crash report, a copy missing a part, a --report that cannot be
# written, a usage error), the text naming its last line. The row holds want_line_with and
# want_own_principals to their controls too: each, in a subshell, fails a case it must refuse, and
# passes the case beside it. The row runs no check. ---
df_runs="$script_dir/testdata/operator-token-minters-runs"
# df_differ FIXTURE EDIT TEXT WANT: recorded_differ on TEXT, which is FIXTURE as EDIT leaves it, must
# print WANT; a wrong answer is added to df_wrong, one a line.
df_differ() {
  local output="$3" gave
  gave="$(recorded_differ)"
  [ "$gave" = "$4" ] || df_wrong+=$'\n'"  $1 $2 gave '$gave'; it should give '$4'"
}
# df_without TEXT PREFIX: TEXT less its first line that starts with PREFIX.
df_without() { awk -v prefix="$2" '!cut && index($0, prefix) == 1 { cut = 1; next } { print }' <<<"$1"; }
if row df; then
  df_wrong=""
  # Each output that reaches the exit block, with the FINDING and UNKNOWN lines it holds, as its
  # README's table counts them.
  for df_run in "exit0-row-a.txt 0 0" "exit1-one-finding-row-u.txt 1 0" "exit1-53-findings-row-b.txt 53 0" \
    "exit1-findings-and-unknowns-row-fb.txt 6 2" "exit2-one-unknown-row-h.txt 0 1" "exit2-ten-unknowns-row-ba.txt 0 10"; do
    read -r df_file df_findings df_unknowns <<<"$df_run"
    df_text="$(<"$df_runs/$df_file")"
    df_differ "$df_file" "as captured" "$df_text" ""
    [ "$df_findings" -eq 0 ] || df_differ "$df_file" "with its first FINDING line deleted" "$(df_without "$df_text" 'FINDING ')" \
      "the last line counts $df_findings finding(s) and $df_unknowns unknown; the run printed $((df_findings - 1)) FINDING and $df_unknowns UNKNOWN lines"
    [ "$df_unknowns" -eq 0 ] || df_differ "$df_file" "with its first UNKNOWN line deleted" "$(df_without "$df_text" 'UNKNOWN ')" \
      "the last line counts $df_findings finding(s) and $df_unknowns unknown; the run printed $df_findings FINDING and $((df_unknowns - 1)) UNKNOWN lines"
  done
  df_text="$(<"$df_runs/exit0-row-a.txt")"
  df_differ exit0-row-a.txt "with a line 'FINDING x: y' added before its last line" "${df_text%$'\n'*}"$'\n'"FINDING x: y"$'\n'"${df_text##*$'\n'}" \
    "the last line counts 0 finding(s) and 0 unknown; the run printed 1 FINDING and 0 UNKNOWN lines"
  # Each output that ends elsewhere, with its last line.
  for df_run in "stopped-before-end-row-al.txt=operator-token-minters: stopped before its end, 1 unknown: exit 2" \
    "no-step-ran-row-bc.txt=operator-token-minters: no step ran, 1 unknown: exit 2" \
    "report-unwritable-row-co.txt=operator-token-minters: --report cannot be written" \
    "usage-error-row-al.txt=  --report FILE              also write the result as JSON"; do
    df_text="$(<"$df_runs/${df_run%%=*}")"
    df_differ "${df_run%%=*}" "as captured" "$df_text" "the run did not reach the exit block; its last line is '${df_run#*=}'"
  done
  # want_line_with's control: one line starting with the prefix and another ending with the suffix
  # fail it; a line holding both passes.
  df_prefix="FINDING serviceAccount:perm-1@$project.iam.gserviceaccount.com: "
  df_suffix=" on $gsa_res through roles/iam.serviceAccountUser, granted at $gsa_res"
  df_apart="${df_prefix}iam.serviceAccountKeys.create on $gsa_res through roles/iam.serviceAccountKeyAdmin, granted at $gsa_res"
  df_apart+=$'\n'"FINDING serviceAccount:perm-2@$project.iam.gserviceaccount.com: iam.serviceAccounts.actAs$df_suffix"
  if df_said="$(output="$df_apart" && want_line_with "$df_prefix" "$df_suffix" "the control" 2>&1)"; then
    df_wrong+=$'\n'"  want_line_with passed an output with the prefix on one line and the suffix on another"
  elif [[ $df_said != *"FAIL: the control: no line starting '$df_prefix' and ending '$df_suffix'"* ]]; then
    df_wrong+=$'\n'"  want_line_with failed the prefix and the suffix on two lines, saying: $df_said"
  fi
  df_said="$(output="$df_apart"$'\n'"${df_prefix}iam.serviceAccounts.actAs$df_suffix" &&
    want_line_with "$df_prefix" "$df_suffix" "the control" 2>&1)" ||
    df_wrong+=$'\n'"  want_line_with failed a line holding both its prefix and its suffix, saying: $df_said"
  # want_own_principals' control: two entries on one principal fail it; two on two pass.
  df_member="serviceAccount:perm-1@$project.iam.gserviceaccount.com"
  df_signjwt="iam.serviceAccounts.signJwt on $gsa_res through projects/$project/roles/only.iam.serviceAccounts.signJwt, granted at $gsa_res"
  df_signblob="iam.serviceAccounts.signBlob on $gsa_res through projects/$project/roles/only.iam.serviceAccounts.signBlob, granted at $gsa_res"
  if df_said="$(OWN_FINDINGS=("$df_member"$'\t'"$df_signjwt" "$df_member"$'\t'"$df_signblob") && want_own_principals "the control" 2>&1)"; then
    df_wrong+=$'\n'"  want_own_principals passed two entries on $df_member"
  elif [[ $df_said != *"FAIL: the control: two entries of OWN_FINDINGS share the principal $df_member"$'\n'* ]]; then
    df_wrong+=$'\n'"  want_own_principals failed two entries on one principal, saying: $df_said"
  fi
  df_said="$(OWN_FINDINGS=("$df_member"$'\t'"$df_signjwt" "serviceAccount:perm-2@$project.iam.gserviceaccount.com"$'\t'"$df_signblob") &&
    want_own_principals "the control" 2>&1)" ||
    df_wrong+=$'\n'"  want_own_principals failed two entries on two principals, saying: $df_said"
  [ -z "$df_wrong" ] || fail "(df) want_recorded's reading of the ten outputs, or a helper's control, went wrong:$df_wrong"
  passed "(df) want_recorded reads the check's last line and its FINDING and UNKNOWN lines as ten real outputs hold them: the counts stand on each output that reaches the exit block, a deleted or added line is named, and an output that ends elsewhere is named by its last line"
fi

# --- (dg) each's bound. OPERATOR_MINTERS_TEST_SLOTS bounds a row's items as it bounds the row files:
# each runs at most min(8, slots) of them at once, so under one slot, as CI runs each part (a pool
# entry the pool counts as one slot), a part is one process at a time. Three probe items each mark
# themselves running, look five times, 0.1 s apart, for the other items' marks, write down the ones
# each look found, and take their own mark away; an item that found as many others at once as the
# bound, or more, ran beside more items than the bound lets run. The bound is read from the
# harness's own slots, its default where OPERATOR_MINTERS_TEST_SLOTS is unset. The row runs no
# check. Run by hand, the control is each with a fixed bound of 8, whatever the slots, as it was
# before the parts: under OPERATOR_MINTERS_TEST_SLOTS=1, p1 found p2 and p3. ---
# each_probe ITEM: marks ITEM running, writes the other items it finds running once a look, five
# looks 0.1 s apart, one line a look, and takes its mark away.
each_probe() {
  local dir="$temporary_dir/dg" look mark found
  : >"$dir/running-$1"
  for look in 1 2 3 4 5; do
    [ "$look" -eq 1 ] || sleep 0.1
    found=""
    for mark in "$dir"/running-*; do
      [ "$mark" = "$dir/running-$1" ] || [ ! -e "$mark" ] || found+=" ${mark##*/running-}"
    done
    printf '%s\n' "${found# }" >>"$dir/saw-$1"
  done
  rm "$dir/running-$1"
}
if row dg; then
  dg_bound=$((slots < 8 ? slots : 8))
  mkdir "$temporary_dir/dg"
  each each_probe p1 p2 p3
  dg_over=""
  for dg_item in p1 p2 p3; do
    dg_most=""
    while IFS= read -r dg_look; do
      read -ra dg_names <<<"$dg_look"
      read -ra dg_most_names <<<"$dg_most"
      [ "${#dg_names[@]}" -le "${#dg_most_names[@]}" ] || dg_most="$dg_look"
    done <"$temporary_dir/dg/saw-$dg_item"
    read -ra dg_most_names <<<"$dg_most"
    [ "${#dg_most_names[@]}" -lt "$dg_bound" ] || dg_over+="; $dg_item saw ${dg_most// / and } running at once"
  done
  [ -z "$dg_over" ] ||
    fail "each ran more of a row's items at once than min(8, OPERATOR_MINTERS_TEST_SLOTS), $dg_bound, lets run: ${dg_over#; }"
  passed "(dg) each runs at most min(8, OPERATOR_MINTERS_TEST_SLOTS) items at once, one at a time under one slot"
fi
