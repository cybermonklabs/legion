# shellcheck shell=bash
# shellcheck disable=SC2016,SC2034,SC2154 # the world, its names, its helpers and the output and status a run sets are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-run.sh: the rows of operator-token-minters.test.sh on the run as
# a whole: a clean world, the permission names, reads that saw nothing, each step's witness, a
# refused identity, the admin tag, the report and its fixture, and a copy missing a part. Sourced
# by the test after its world and helpers; its header says what the fakes model. The rows:
#
#   (a)  a world of people, system namespaces and the Workload Identity member alone exits 0, and a
#        project with the operator's cluster alone prints that it has no other;
#   (h)  a permission name missing from roles/container.admin's list fails the run, naming it;
#   (i)  an empty listing exits 2 with UNKNOWN: agent namespaces holding no ServiceAccount, a Policy
#        Analyzer answer listing no one, an analysis not fully explored;
#   (j)  each live step's witness: the operator account's analysis without its Workload Identity
#        member, and an RBAC read whose cluster-admin binding does not answer, each read UNKNOWN;
#   (k)  with no identity that may run the live steps, each prints UNKNOWN naming the permission it
#        needs, and the run exits 2;
#   (l)  the admin tag: ENVOY_OPERATOR shown without "admin": "true", no such secret, and the scratch
#        secret ENVOY_OPERATOR_CONTROL at human tier without --admin each exit 1 naming the field;
#        a session that cannot show it reads UNKNOWN;
#   (m)  --report writes run_at, findings, credited and residuals ([] each for a run that credits
#        nothing; row fa holds their entries), unknown and allowances ([] for a run given none), with
#        a stubbed UNKNOWN step listed under unknown;
#   (v)  the report's findings carry role, resource, permissions and granted_at beside principal
#        and right, on an IAM finding and on an RBAC finding;
#   (z)  the fixture testdata/operator-token-minters-report.json is the --report of fence_check (the
#        script whose contract holds the test's example workload policies) in the broker's pod
#        fence's world (fence_world) with an IAM finding, grade-ci's RBAC findings, the fence's
#        credits and residuals and an UNKNOWN, run_at fixed; the row fails when the committed file
#        differs (OPERATOR_MINTERS_WRITE_FIXTURE=1 rewrites it, and OPERATOR_MINTERS_FIXTURE names
#        another file to compare, as the row's control does);
#   (al) a run that stops mid-way leaves an UNKNOWN report, never the old one, and a usage error
#        leaves none;
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
#        a grep that stops early (-q or -m), which pipefail and SIGPIPE can read as no match.

# --- (a) People, the system namespaces and the Workload Identity member alone. ---
if row a; then
  base_world
  run
  want_status 0 "a clean world"
  [ "$(count_prefix 'FINDING ')" -eq 0 ] && [ "$(count_prefix 'UNKNOWN ')" -eq 0 ] || fail "a clean world printed a FINDING or an UNKNOWN"
  want_line "PASS live: Workload Identity: the project runs the operator's cluster operator-cluster alone" "a clean world"
  for step in "permission names" "code: RBAC" "code: IAM" "live: RBAC on operator-cluster" "live: GKE IAM on project $project" \
    "live: minters of $gsa" "live: minters of the broker account $broker" "live: hold writers: Cloud SQL users of example-instance" \
    "live: hold writers: Secret example-db/broker-db-login" "live: hold writers: the broker program's state in gs://example-state" \
    "live: hold writers: the signing key hold-signing-example" "admin tag of ENVOY_OPERATOR" \
    "live: who can register a cluster to, or write namespaces and RBAC on, the fleet of project $project" "live: who can set IAM on organizations/100"; do
    want_text "PASS $step: " "a clean world's $step"
  done
  want_line "PASS live: Workload Identity: the fleet of project $project: 1 membership(s) listed, every location reached: 1 the operator's cluster, 0 read above as clusters of the project, 0 read here, and the fleet holds no scope" \
    "a clean world's fleet"
  passed "(a) people, the system namespaces and the Workload Identity member alone exit 0, and a project with the operator's cluster alone says so"
fi

# --- (h) A permission name roles/container.admin's list does not hold. ---
if row h; then
  base_world
  edit "$world/roles.json" '.["roles/container.admin"] -= ["container.pods.exec"]'
  run
  want_status 2 "roles/container.admin without container.pods.exec"
  want_line "UNKNOWN permission names: container.pods.exec is not in roles/container.admin's includedPermissions, so no check that names it can find a holder" \
    "roles/container.admin without container.pods.exec"
  passed "(h) a permission name missing from roles/container.admin's list fails the run, naming it"
fi

# --- (i) Empty listings. ---
if row i; then
  base_world
  mapfile -t args < <(args_without --agent-namespace)
  run_only "${args[@]}" --agent-namespace 'nobody-*'
  want_status 2 "agent namespaces holding no ServiceAccount"
  want_text "UNKNOWN live: RBAC on operator-cluster: the cluster lists no ServiceAccount in the agent namespaces" "agent namespaces holding no ServiceAccount"
  passed "(i) agent namespaces holding no ServiceAccount exit 2 with UNKNOWN"

  base_world
  : >"$world/empty-analysis"
  run
  want_status 2 "a Policy Analyzer that lists no one"
  want_text "UNKNOWN live: GKE IAM on project $project: the analysis lists no holder" "a Policy Analyzer that lists no one"
  passed "(i) a Policy Analyzer answer that lists no one exits 2 with UNKNOWN"

  base_world
  : >"$world/not-explored"
  run
  want_status 2 "an analysis not fully explored"
  want_text "UNKNOWN live: GKE IAM on project $project: the analysis was not fully explored" "an analysis not fully explored"
  passed "(i) an analysis not fully explored exits 2 with UNKNOWN"
fi

# --- (j) Each live step's positive witness. ---
if row j; then
  base_world
  unbind "$gsa_res" roles/iam.workloadIdentityUser
  run
  want_status 2 "the operator's account without its Workload Identity member"
  want_text "UNKNOWN live: minters of $gsa: the analysis does not list $wi_member" "no Workload Identity member"
  passed "(j) the operator account's analysis without its Workload Identity member reads UNKNOWN"

  base_world
  : >"$world/clusters/operator-cluster/no-masters"
  run
  want_status 2 "an RBAC read whose cluster-admin binding does not answer"
  want_text "UNKNOWN live: RBAC on operator-cluster: the witness review, the cluster-admin binding's group system:masters, was not allowed" "no witness"
  passed "(j) an RBAC read whose cluster-admin binding does not answer reads UNKNOWN"
fi

# --- (k) No identity that may run the live steps. ---
if row k; then
  base_world
  : >"$world/clusters/operator-cluster/refuse-sar"
  : >"$world/refuse-analyze"
  run
  want_status 2 "no identity that may run the live steps"
  want_text "UNKNOWN live: RBAC on operator-cluster: the API server refused the review (it needs create on subjectaccessreviews, container.subjectAccessReviews.create on GKE)" \
    "a refused review"
  for step in "live: GKE IAM on project $project" "live: minters of $gsa" "live: hold writers: Cloud SQL users of example-instance"; do
    want_text "UNKNOWN $step: Policy Analyzer refused the analysis (it needs cloudasset.assets.analyzeIamPolicy on organizations/$org)" "a refused analysis"
  done
  [ "$(count_prefix 'FINDING ')" -eq 0 ] || fail "no identity that may run the live steps: a finding"
  passed "(k) with no identity that may run them, the live steps print UNKNOWN naming the permission, and the run exits 2"
fi

# --- (l) The admin tag. ---
if row l; then
  base_world
  edit "$world/secrets/ENVOY_OPERATOR.json" '.admin = ""'
  run
  want_status 1 "ENVOY_OPERATOR without the admin tag"
  want_line "FINDING secret:ENVOY_OPERATOR: admin is \"\", not \"true\": without the administrator tag a model judge may decide its release" "no admin tag"
  passed "(l) ENVOY_OPERATOR shown without \"admin\": \"true\" exits 1 naming admin"

  base_world
  edit "$world/secrets/ENVOY_OPERATOR.json" '.tier = "agent"'
  run
  want_status 1 "ENVOY_OPERATOR at agent tier"
  want_line "FINDING secret:ENVOY_OPERATOR: tier is \"agent\", not \"human\"" "agent tier"
  passed "(l) ENVOY_OPERATOR at agent tier exits 1 naming tier"

  base_world
  rm "$world/secrets/ENVOY_OPERATOR.json"
  run
  want_status 1 "no ENVOY_OPERATOR"
  want_text "FINDING secret:ENVOY_OPERATOR: no such secret" "no such secret"
  passed "(l) no secret ENVOY_OPERATOR exits 1"

  base_world
  rm "$world/secrets/ENVOY_OPERATOR.json"
  : >"$world/old-words"
  run
  want_status 1 "no ENVOY_OPERATOR, in the released agent-secrets' words"
  want_text "FINDING secret:ENVOY_OPERATOR: no such secret" "no such secret, in the released agent-secrets' words"
  passed "(l) no secret ENVOY_OPERATOR exits 1 in gRPC's words too, as legion-envoy-v30.43.0 prints them"

  base_world
  run --secret ENVOY_OPERATOR_CONTROL
  want_status 1 "the scratch secret at human tier without --admin"
  want_line "FINDING secret:ENVOY_OPERATOR_CONTROL: admin is \"\", not \"true\": without the administrator tag a model judge may decide its release" \
    "the scratch secret"
  passed "(l) the scratch secret ENVOY_OPERATOR_CONTROL at human tier without --admin exits 1 naming admin (the plan's control)"

  base_world
  : >"$world/refuse-secret-show"
  run
  want_status 2 "a session that cannot show the secret"
  want_text "UNKNOWN admin tag of ENVOY_OPERATOR: agent-secrets secret show ENVOY_OPERATOR --json was refused: agent-secrets secret show: Secret Manager GetSecret answered PermissionDenied" \
    "a refused show"
  passed "(l) a session that cannot show the secret's tags reads UNKNOWN naming the refusal"
fi

# --- (m) The report. ---
if row m; then
  base_world
  iam "$project_res" roles/container.admin "serviceAccount:$ci_gsa"
  : >"$world/refuse-secret-show"
  before="$(date -u +%s)"
  run --report "$temporary_dir/report.json"
  want_status 1 "a run with a finding and an unknown step"
  report="$temporary_dir/report.json"
  [ -s "$report" ] || fail "--report wrote nothing"
  run_at="$(jq -r '.run_at' "$report")"
  [[ "$run_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || fail "the report's run_at is '$run_at', not an RFC 3339 UTC time"
  at="$(date -u -d "$run_at" +%s)"
  [ "$at" -ge "$((before - 1))" ] && [ "$at" -le "$(date -u +%s)" ] || fail "the report's run_at $run_at is not the run's"
  jq -e --arg p "serviceAccount:$ci_gsa" 'any(.findings[]; .principal == $p and (.right | startswith("container.serviceAccounts.createToken")))' "$report" >/dev/null ||
    fail "the report's findings do not name serviceAccount:$ci_gsa: $(jq -c .findings "$report")"
  jq -e 'any(.unknown[]?; .step == "admin tag of ENVOY_OPERATOR" and (.reason | contains("PermissionDenied")))' "$report" >/dev/null ||
    fail "the report's unknown does not list the step 'admin tag of ENVOY_OPERATOR': $(jq -c '.unknown // "absent"' "$report")"
  jq -e 'keys == ["allowances", "credited", "findings", "residuals", "run_at", "unknown"] and .allowances == [] and .credited == []
    and .residuals == [] and all(.findings[]; keys == ["granted_at", "permissions", "principal", "resource", "right", "role"])
    and all(.unknown[]; keys == ["reason", "step"])' "$report" >/dev/null ||
    fail "the report holds other fields than run_at, findings (principal, right, role, resource, permissions, granted_at), an empty credited and residuals, unknown and an empty allowances: $(jq -c . "$report")"
  passed "(m) --report writes run_at, findings, credited, residuals, unknown and allowances, with the stubbed UNKNOWN step under unknown"
fi

# --- (v) The report's parts of each finding, for a reader matching principal and role. ---
if row v; then
  base_world
  iam "$project_res" roles/container.admin "serviceAccount:$ci_gsa"
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods exec envoy-operator ""
  run --report "$temporary_dir/report.json"
  want_status 1 "an IAM and an RBAC finding"
  report="$temporary_dir/report.json"
  # Every finding carries every field, each of its type.
  missing="$(jq -r '.findings[] as $f
    | ((["principal", "right", "role", "resource", "granted_at"][] as $k
        | select(($f[$k] | type) != "string") | "\($f.principal) has no \($k)"),
       (select(($f.permissions | type) != "array") | "\($f.principal) has no permissions list"))' "$report" | paste -sd ';' | sed 's/;/; /g')"
  [ -z "$missing" ] || fail "the report's findings lack fields: $missing"
  jq -e --arg p "serviceAccount:$ci_gsa" --arg res "$project_res" 'any(.findings[]; .principal == $p and .role == "roles/container.admin"
      and .resource == $res and .granted_at == $res and (.permissions | index("container.serviceAccounts.createToken") != null))' "$report" >/dev/null ||
    fail "the IAM finding's parts are not principal, roles/container.admin, $project_res and its permissions: $(jq -c --arg p "serviceAccount:$ci_gsa" '[.findings[] | select(.principal == $p)]' "$report")"
  jq -e 'any(.findings[]; .principal == "system:serviceaccount:legion:worker" and .role == "create pods/exec in envoy-operator"
      and .resource == "cluster:operator-cluster" and .permissions == ["create pods/exec in envoy-operator"]
      and (.granted_at | startswith("RBAC: allowed by")))' "$report" >/dev/null ||
    fail "the RBAC finding's parts are not its label, cluster:operator-cluster and the API server's reason: $(jq -c '[.findings[] | select(.principal == "system:serviceaccount:legion:worker")]' "$report")"
  passed "(v) the report's findings carry role, resource, permissions and granted_at, on an IAM finding and on an RBAC finding"
fi

# --- (z) The report fixture: a fresh --report, run_at fixed, must equal the committed file. ---
if row z; then
  fence_world
  iam "$project_res" roles/container.admin "serviceAccount:$ci_gsa"
  grade_member
  cluster_admin grade-cluster "$(user_subject "$grade_ci")"
  : >"$world/refuse-secret-show"
  fence_run --guarded "$route_pair" --report "$temporary_dir/fixture-run.json"
  want_status 1 "the fixture's world"
  # run_at is the one field a run's clock sets; the fixture fixes it at the epoch, so no reader
  # takes it for a time something happened.
  jq '.run_at = "1970-01-01T00:00:00Z"' "$temporary_dir/fixture-run.json" >"$temporary_dir/fixture-fresh.json"
  jq -e 'any(.findings[]; .role | startswith("roles/")) and any(.findings[]; (.principal | startswith("serviceAccount:grade-ci@")) and (.granted_at | startswith("RBAC: ")))
      and any(.credited[]; .policy == "broker-pod-connect") and any(.residuals[]; .tracked_by == "INFRA-4604")
      and (.unknown | length) > 0' "$temporary_dir/fixture-fresh.json" >/dev/null ||
    fail "the fixture's world gave no IAM finding, no RBAC finding of grade-ci, no credit, no residual or no UNKNOWN: $(jq -c . "$temporary_dir/fixture-fresh.json")"
  # An RBAC finding's permissions are its one label, whole, though a guarded label holds a comma.
  split="$(jq -r '.findings[] | select(.granted_at | startswith("RBAC: ")) | select(.permissions != [.role]) | "\(.role): \(.permissions)"' "$temporary_dir/fixture-fresh.json")"
  [ -z "$split" ] || fail "an RBAC finding's permissions are not its label, whole: $split"
  if [ "${OPERATOR_MINTERS_WRITE_FIXTURE:-}" = 1 ]; then
    mkdir -p "$(dirname "$fixture")"
    cp "$temporary_dir/fixture-fresh.json" "$fixture"
    echo "wrote $fixture"
  fi
  [ -f "$fixture" ] || fail "no fixture at $fixture: run with OPERATOR_MINTERS_WRITE_FIXTURE=1 to write it"
  cmp -s "$fixture" "$temporary_dir/fixture-fresh.json" ||
    fail "the fixture $fixture differs from a fresh run (OPERATOR_MINTERS_WRITE_FIXTURE=1 rewrites it): $(diff "$fixture" "$temporary_dir/fixture-fresh.json" | sed -n '1,20p' || true)"
  passed "(z) the committed report fixture equals a fresh --report of its world"
fi

# --- (al) A report is this run's or none. A copy whose webhook-backends.jq prints nothing stops the
# run mid-way (with_rights refuses the empty rights, as row (cx) shows); the old report is replaced
# by the run's own, which holds its UNKNOWN run. ---
if row al; then
  base_world
  report="$temporary_dir/old-report.json"
  echo '{"run_at": "2000-01-01T00:00:00Z", "findings": [], "unknown": []}' >"$report"
  script="$(script_copy al)"
  echo 'empty' >"$(dirname "$script")/operator-token-minters/webhook-backends.jq"
  run_script="$script" run --report "$report"
  want_status 2 "a run that stops mid-way"
  want_text "UNKNOWN run: the run stopped before its end" "a run that stops mid-way"
  [ -s "$report" ] || fail "a run that stops mid-way left no report"
  jq -e '.run_at != "2000-01-01T00:00:00Z" and any(.unknown[]; .step == "run")' "$report" >/dev/null ||
    fail "a run that stops mid-way left this report, not its own UNKNOWN one: $(jq -c . "$report")"
  passed "(al) a run that stops mid-way leaves its own UNKNOWN report, not the old one"

  echo '{"run_at": "2000-01-01T00:00:00Z", "findings": [], "unknown": []}' >"$report"
  run --report "$report" --no-such-flag
  want_status 2 "a usage error"
  [ ! -e "$report" ] || fail "a usage error left the old report: $(cat "$report")"
  passed "(al) a usage error leaves no report"
fi

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
