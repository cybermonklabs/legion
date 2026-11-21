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
#   (m)  --report writes run_at, findings and unknown, with a stubbed UNKNOWN step listed under
#        unknown;
#   (v)  the report's findings carry role, resource, permissions and granted_at beside principal
#        and right, on an IAM finding and on an RBAC finding;
#   (z)  the fixture testdata/operator-token-minters-report.json is this script's --report from a
#        world with an IAM finding, grade-ci's RBAC findings and an UNKNOWN, run_at fixed; the row
#        fails when the committed file differs (OPERATOR_MINTERS_WRITE_FIXTURE=1 rewrites it, and
#        OPERATOR_MINTERS_FIXTURE names another file to compare, as the row's control does);
#   (al) a run that stops mid-way leaves an UNKNOWN report, never the old one, and a usage error
#        leaves none;
#   (ba) a get-ancestors answer of [] reads UNKNOWN, and so does another cluster that lists no
#        ServiceAccount outside its system namespaces though a binding names a subject there;
#   (bc) a copy of the script missing output.sh, or missing analysis.jq, prints "UNKNOWN run" naming
#        it, writes that to --report, and exits 2 before any step runs.

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
    "live: who can register a cluster to the fleet of project $project" "live: who can set IAM on organizations/100"; do
    want_text "PASS $step: " "a clean world's $step"
  done
  want_line "PASS live: Workload Identity: the fleet of project $project: 1 membership(s) listed, every location reached: 1 the operator's cluster, 0 read above as clusters of the project, 0 read here" \
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
  jq -e 'keys == ["findings", "run_at", "unknown"]
    and all(.findings[]; keys == ["granted_at", "permissions", "principal", "resource", "right", "role"])
    and all(.unknown[]; keys == ["reason", "step"])' "$report" >/dev/null ||
    fail "the report holds other fields than run_at, findings (principal, right, role, resource, permissions, granted_at) and unknown: $(jq -c . "$report")"
  passed "(m) --report writes run_at, findings and unknown, with the stubbed UNKNOWN step under unknown"
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
  base_world
  iam "$project_res" roles/container.admin "serviceAccount:$ci_gsa"
  grade_member
  cluster_admin grade-cluster "$(user_subject "$grade_ci")"
  : >"$world/refuse-secret-show"
  run --guarded "$route_pair" --report "$temporary_dir/fixture-run.json"
  want_status 1 "the fixture's world"
  # run_at is the one field a run's clock sets; the fixture fixes it at the epoch, so no reader
  # takes it for a time something happened.
  jq '.run_at = "1970-01-01T00:00:00Z"' "$temporary_dir/fixture-run.json" >"$temporary_dir/fixture-fresh.json"
  jq -e 'any(.findings[]; .role | startswith("roles/")) and any(.findings[]; (.principal | startswith("serviceAccount:grade-ci@")) and (.granted_at | startswith("RBAC: ")))
      and (.unknown | length) > 0' "$temporary_dir/fixture-fresh.json" >/dev/null ||
    fail "the fixture's world gave no IAM finding, no RBAC finding of grade-ci, or no UNKNOWN: $(jq -c . "$temporary_dir/fixture-fresh.json")"
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

# --- (al) A report is this run's or none. ---
if row al; then
  base_world
  report="$temporary_dir/old-report.json"
  echo '{"run_at": "2000-01-01T00:00:00Z", "findings": [], "unknown": []}' >"$report"
  : >"$world/clusters/operator-cluster/garbage-serviceaccounts"
  run --report "$report"
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
  local copy="$temporary_dir/copy-$1" args
  base_world
  rm -rf "$copy"
  mkdir -p "$copy"
  cp "$check" "$copy/operator-token-minters.sh"
  cp -r "$(dirname "$check")/operator-token-minters" "$copy/"
  rm "$copy/operator-token-minters/$1"
  mapfile -t args < <(full_args)
  status=0
  output="$(env PATH="$fake_bin:$PATH" OPERATOR_MINTERS_WORLD="$world" TMPDIR="$temporary_dir" bash "$copy/operator-token-minters.sh" \
    "${args[@]}" --report "$copy/report.json" 2>&1)" || status=$?
  want_status 2 "a copy without $1"
  want_text "UNKNOWN run: the parts $1 of " "a copy without $1"
  jq -e --arg p "$1" '.findings == [] and any(.unknown[]; .step == "run" and (.reason | contains($p)))' "$copy/report.json" >/dev/null 2>&1 ||
    fail "a copy without $1: the report does not hold the UNKNOWN run: $(cat "$copy/report.json" 2>&1)"
}
if row bc; then
  each missing_part_row output.sh analysis.jq
  passed "(bc) a copy missing output.sh or analysis.jq prints UNKNOWN run naming it, reports it, and exits 2"
fi
