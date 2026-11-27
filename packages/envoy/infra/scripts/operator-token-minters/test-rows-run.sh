# shellcheck shell=bash
# shellcheck disable=SC2016,SC2034,SC2154 # the world, its names, its helpers and the output and status a run sets are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-run.sh: the rows of operator-token-minters.test.sh on the run as
# a whole: a clean world, the permission names, reads that saw nothing, each step's witness, a
# refused identity, the admin tag, the report and its fixture, and a copy missing a part. Sourced
# by the test after its world and helpers; its header says what the fakes model. The rows:
#
#   (a)  a world where no person holds anything, the system namespaces, Google's service agents and
#        the Workload Identity member alone exits 0 with no FINDING and no UNKNOWN, and a project with
#        the operator's cluster alone prints that it has no other;
#   (sp) a standing person is a finding in IAM: a person holding a checked right on the operator's
#        account, on the organization, through an expanded group, on a hold writer's resource or in
#        a preview is a FINDING naming the person, the right and where it was granted, and the run
#        exits 1; the same world with those grants removed exits 0; control: a copy that passes
#        every `user:` member (checks.sh) prints no IAM finding for them and still finds the people
#        bound in the clusters;
#   (sb) a standing person bound in a cluster is a finding, on the operator's cluster, on another
#        cluster of the pool and through a right a role limits by name; the same world with those
#        grants removed exits 0, and a person bound to a right no review asks is counted among the
#        binding subjects, never as none; control: a copy that allows an email User subject
#        (principals.sh) prints no RBAC finding for them and still finds the people bound in IAM;
#   (h)  a permission name missing from roles/container.admin's list fails the run, naming it;
#   (i)  an empty listing exits 2 with UNKNOWN: agent namespaces holding no ServiceAccount, a Policy
#        Analyzer answer listing no one, an analysis not fully explored;
#   (j)  each live step's witness: the operator account's analysis without its Workload Identity
#        member, and an RBAC read whose cluster-admin binding does not answer, each read UNKNOWN;
#   (k)  with no identity that may run the live steps, each prints UNKNOWN naming the permission it
#        needs, and the run exits 2; a refused analysis names the three Cloud Asset permissions
#        Google requires on the scope and keeps Google's own line, and a copy dropping one fails;
#   (l)  the admin tag, read from the secret's Secret Manager annotations: ENVOY_OPERATOR at human
#        tier with --admin is a finding, each release waiting on one person's approval and no second
#        person's; without the admin annotation, at agent tier, and the scratch secret
#        ENVOY_OPERATOR_CONTROL at human tier without --admin each exit 1 naming the field; admin
#        held beside the annotations instead of in them is a finding too (the read's control); no
#        such secret passes, naming the secret and the secrets the prefix's project lists under it,
#        and reads UNKNOWN where that project lists none under the prefix (a mistyped prefix beside
#        the real secret, a project holding none, a prefix naming another project) or the list is
#        refused; controls: a copy without the list check passes the mistyped prefix, and a copy
#        listing --project instead passes another project's prefix; a refused describe, and a run
#        with no --secrets-prefix, read UNKNOWN;
#   (m)  --report writes run_at, findings, credited and residuals ([] each for a run that credits
#        nothing; row fa holds their entries), unknown, allowances ([] for a run given none) and
#        steps (the step census, row sc), with a stubbed UNKNOWN step listed under unknown;
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
#   (sc) no part can vanish from a run: a copy with any one part emptied (each step file, library
#        and jq file) prints "UNKNOWN run" naming it and exits 2 before any step runs; past that
#        guard (a copy without it), the step census names each step file emptied, "ran and reported
#        nothing", and one that never ran, and the run does not exit 0; a clean run's --report lists
#        each step file reported but live: audited clusters, quiet without its flag; control: a copy
#        without the guard and the census exits 0 with step-admin-tag.sh emptied;
#   (rl) a role file the permission check cannot read stops the run with an UNKNOWN naming the role;
#   (cj) --service-agent naming an agent's own Google service account is a usage error, and a run
#        given --service-agent, --rbac-only-member and --fleet none writes each into --report's
#        allowances;
#   (co) a text pasted alone, as the value of a flag that names a thing where it is not of that
#        flag's form, or as a file flag's file is printed in neither stdout, stderr nor --report: a
#        flag that names a thing refuses a value of another form, naming the flag and never its
#        text, and a file flag's file is named by the flag and its place;
#   (cp) the auditor never asks the API server for a Secret, even when a guarded namespace holds
#        token Secrets that carry data.
#   (rd) every fake kubectl call is in reads.json with its Kubernetes verb, resource or non-resource
#        URL and source step; removing get namespaces makes the workload-identity row fail; a row
#        that wants a finding's exit 1 fails on a read reads.json lacks, never passing on it; and
#        unused_reads names an entry no call uses (the inventory a run of every row holds
#        reads.json to);
#   (cq) a credential of a name flag's form shaped as a credential its issuer marks (a ghp_ token,
#        a github_pat_ token, an AIza key and a Slack token as --signing-secret, an AWS access key
#        ID as --secret) is refused too, naming the flag and never the value, and is printed in
#        neither stdout, stderr nor --report;
#   (cs) a cluster whose principals and rights are each more than one argument of a command line
#        holds is reviewed whole: they reach jq through files and its stdin, never its arguments;
#   (cv) a part that builds the custom resources' rights or the binding namespaces' rights and
#        fails or prints nothing reads UNKNOWN naming those rights, and the RBAC step does not
#        pass;
#   (cx) with_rights handed an empty rights value refuses it and stops the run, UNKNOWN, exit 2;
#   (da) no line of the check, its parts or this test's own files reads a status from a pipe into
#        a grep that stops early (-q or -m), which pipefail and SIGPIPE can read as no match;
#   (db) a pick of the reviews to judge that stops part way (its awk under a 2 KiB file size limit)
#        reads UNKNOWN naming it, exit 2, and the RBAC step does not pass.

# --- (a) No person holds anything; the system namespaces, Google's service agents and the Workload
# Identity member alone. ---
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
  passed "(a) a world where no person holds anything, the system namespaces, Google's service agents and the Workload Identity member alone exit 0, and a project with the operator's cluster alone says so"
fi

# --- (sp) and (sb) A standing person is a finding, in IAM and bound in a cluster. Each person
# below holds a right the reading asks about, with no second person approving its use, so each is a
# standing holder of the operator identity (INFRA-840's R16), and a FINDING names the person, the
# right and where the right was granted. The reading passed a person at two separate gates before
# R16, so each gate has a row and a control of its own: (sp) the IAM side, judged by iam_verdict
# (checks.sh), and (sb) the cluster side, where k8s_subject (principals.sh) marked an email User
# allowed and bound_subjects (reviews.sh) then skipped it. A copy of the check with one gate's old
# rule put back must miss that row's people and keep the other row's, which is why the controls
# read both halves. Both rows build the same world.
# In IAM: mallory@ mints the operator's account's token; eve@ owns the organization; admin@ holds
# roles/container.admin through the group cluster-admins; dba@ writes the broker database's users;
# reader@ reads the broker program's state; declared@ is given roles/container.admin by a preview.
# In the clusters, by each of k8s_subject's three paths: alice@ is bound to cluster-admin on the
# operator's cluster (bound_subjects, step-rbac.sh), carol@ to cluster-admin on another cluster of
# the pool (bound_subjects, step-workload-identity.sh), and bob@ may patch the Deployment
# envoy-operator by name (named_principal, live_named). The base world's RBAC lost no person to
# these rows, as its IAM did: the only User subjects it binds are the operator's own Google service
# account and comment-delivery's, both service accounts.
# The labels are INFRA-840's c-operator-minters-rows own: it greps for one line per gate. ---
readonly standing_people_iam=(mallory@example.com eve@example.com admin@example.com dba@example.com reader@example.com declared@example.com)
readonly standing_people_rbac=(alice@example.com carol@example.com bob@example.com)
readonly standing_people=("${standing_people_iam[@]}" "${standing_people_rbac[@]}")
# standing_people_world: the base world with each standing person planted.
standing_people_world() {
  base_world
  iam "$gsa_res" roles/iam.serviceAccountTokenCreator user:mallory@example.com
  iam "$org_res" roles/owner user:eve@example.com
  iam "$project_res" roles/container.admin group:cluster-admins@example.com
  iam "$project_res" roles/cloudsql.admin user:dba@example.com
  iam "$bucket_res" roles/storage.objectViewer user:reader@example.com
  edit "$world/preview.json" '.steps += [{op: "same", urn: "urn:pulumi:example::iam::gcp:projects/iAMMember:IAMMember::declared-container",
    provider: "example-provider", newState: {urn: "urn:pulumi:example::iam::gcp:projects/iAMMember:IAMMember::declared-container",
    custom: true, type: "gcp:projects/iAMMember:IAMMember", inputs: {project: $p, role: "roles/container.admin", member: "user:declared@example.com"}}}]' \
    --arg p "$project"
  cluster_admin operator-cluster "$(user_subject alice@example.com)"
  add_cluster other-cluster us-east1
  cluster_admin other-cluster "$(user_subject carol@example.com)"
  subject_grant operator-cluster "$(user_subject bob@example.com)" patch apps deployments "" envoy-operator envoy-operator
}
# standing_people_remove_grants: the same world with every planted person's grant taken away, in
# IAM, in the preview and on both clusters. A run of it must exit 0: the people are what the run
# reports, and nothing else of the world does.
standing_people_remove_grants() {
  edit "$world/iam.json" 'map(.members -= ($ARGS.positional | map("user:" + .)) | select(.members | length > 0)
    | select(.members != ["group:cluster-admins@example.com"]))' --args "${standing_people[@]}"
  edit "$world/preview.json" '.steps |= map(select(.urn | endswith("::declared-container") | not))'
  edit "$world/clusters/operator-cluster/grants.json" 'map(select(.subject.kind != "User" or ((.subject.name == "alice@example.com" or .subject.name == "bob@example.com") | not)))'
  edit "$world/clusters/other-cluster/grants.json" 'map(select(.subject != {kind: "User", name: "carol@example.com"}))'
}
# standing_people_report_problems REPORT PERSON...: each named person the --report at REPORT does
# not hold as a finding with where its right was granted, one a line.
standing_people_report_problems() {
  local report="$1" person
  shift
  for person in "$@"; do
    jq -e --arg p "user:$person" 'any(.findings[]; .principal == $p and .granted_at != "")' "$report" >/dev/null ||
      echo "no --report finding of user:$person with where it was granted"
  done
}
# standing_people_problems_iam REPORT: each line or report entry of the IAM side that the run of
# standing_people_world must hold and does not, one a line.
standing_people_problems_iam() {
  local want
  for want in \
    "FINDING user:mallory@example.com: iam.serviceAccounts.getAccessToken, iam.serviceAccounts.getOpenIdToken, iam.serviceAccounts.signJwt, iam.serviceAccounts.signBlob, iam.serviceAccounts.implicitDelegation on $gsa_res through roles/iam.serviceAccountTokenCreator, granted at $gsa_res" \
    "FINDING user:eve@example.com: resourcemanager.organizations.setIamPolicy, iam.roles.update on $org_res through roles/owner, granted at $org_res" \
    "FINDING user:dba@example.com: cloudsql.users.create, cloudsql.users.update on $sql_res through roles/cloudsql.admin, granted at $project_res" \
    "FINDING user:reader@example.com: storage.objects.get on $bucket_res through roles/storage.objectViewer, granted at $bucket_res"; do
    has_line "$want" || echo "no line '$want'"
  done
  has_prefix "FINDING user:admin@example.com: container.serviceAccounts.createToken" ||
    echo "no FINDING of admin@example.com's container.serviceAccounts.createToken through the group cluster-admins"
  has_prefix "FINDING user:declared@example.com: container.serviceAccounts.createToken" ||
    echo "no FINDING of declared@example.com's container.serviceAccounts.createToken declared by the preview"
  standing_people_report_problems "$1" "${standing_people_iam[@]}"
}
# standing_people_problems_rbac REPORT: the same for the cluster side.
standing_people_problems_rbac() {
  local want
  for want in \
    "FINDING user:alice@example.com: create serviceaccounts/token envoy-operator in envoy-operator" \
    "FINDING user:carol@example.com: create namespaces cluster-wide on cluster $project/us-east1/other-cluster"; do
    has_line "$want" || echo "no line '$want'"
  done
  has_prefix "FINDING user:bob@example.com: patch deployments.apps envoy-operator" ||
    echo "no FINDING of bob@example.com's patch of the Deployment envoy-operator by name"
  standing_people_report_problems "$1" "${standing_people_rbac[@]}"
}
# standing_people_problems REPORT: both halves, for the controls: a control must miss the half whose
# rule it puts back and keep the other half.
standing_people_problems() {
  standing_people_problems_iam "$1"
  standing_people_problems_rbac "$1"
}
if row sp; then
  report="$temporary_dir/sp-report.json"
  standing_people_world
  run --report "$report"
  problems="$(standing_people_problems_iam "$report")"
  [ -z "$problems" ] || fail "a standing person is a finding: ${problems//$'\n'/; }"
  want_status 1 "a standing person in IAM"
  ! has_prefix "PASS live: minters of $gsa: " || fail "a standing person: the minters of the operator's account passed"
  # The person inside the group is named by the group's expansion: a stub that drops the expanded
  # members (as row gp's control does) misses admin@ and nothing else.
  OPERATOR_MINTERS_GROUP_STUB=drop run --report "$report"
  problems="$(standing_people_problems "$report")"
  [[ "$problems" == *"through the group cluster-admins"* && "$problems" != *"mallory@"* && "$problems" != *"alice@"* ]] ||
    fail "the stub dropping expanded group members read as: ${problems:-nothing}"
  # The same world with each person's grant removed exits 0.
  standing_people_remove_grants
  run
  want_status 0 "the standing people's world with their grants removed"
  [ "$(count_prefix 'FINDING ')" -eq 0 ] || fail "the standing people's world with their grants removed printed a FINDING"
  # Control: a copy that passes every user: member, as the reading did before R16, prints no IAM
  # finding for them, and still finds alice@, carol@ and bob@ in the clusters.
  copy="$(script_copy person-ok-iam)"
  plant "$(dirname "$copy")/operator-token-minters/checks.sh" '  case "$member" in' $'  case "$member" in\n    user:*) VERDICT=ok; return ;;'
  standing_people_world
  run_script="$copy" run --report "$report"
  problems="$(standing_people_problems "$report")"
  [[ "$problems" == *"no line 'FINDING user:mallory@example.com: "* && "$problems" == *"no --report finding of user:eve@example.com "* &&
    "$problems" == *"through the group cluster-admins"* && "$problems" != *"alice@"* && "$problems" != *"carol@"* && "$problems" != *"bob@"* ]] ||
    fail "the control copy passing every user: member read as: ${problems:-nothing}"
  passed "(sp) a standing person is a finding, in IAM: on the operator's account, on the organization, through an expanded group, on a hold writer's resource and in a preview, naming the right and where it was granted; the same world without the grants exits 0; control: a copy passing every user: member misses them and still finds the people bound in the clusters"
fi

if row sb; then
  report="$temporary_dir/sb-report.json"
  standing_people_world
  run --report "$report"
  problems="$(standing_people_problems_rbac "$report")"
  [ -z "$problems" ] || fail "a standing person bound in a cluster is a finding: ${problems//$'\n'/; }"
  want_status 1 "a standing person bound in a cluster"
  ! has_prefix "PASS live: RBAC on operator-cluster: " || fail "a standing person bound in a cluster: the RBAC step passed"
  # The same world with each person's grant removed exits 0, and a person bound to a right no review
  # asks is reviewed and counted there, never read as no subject at all.
  standing_people_remove_grants
  run
  want_status 0 "the standing people's world with their grants removed"
  subject_grant operator-cluster "$(user_subject alice@example.com)" get "" configmaps "" legion ""
  run
  want_status 0 "a person bound to get configmaps alone"
  want_text ", 1 binding subjects, and anyone unauthenticated), " "a person bound to get configmaps alone"
  # Control: a copy that allows an email User subject, as the reading did before R16, prints no RBAC
  # finding for alice@, carol@ or bob@, and still finds the people bound in IAM.
  copy="$(script_copy person-ok-rbac)"
  plant "$(dirname "$copy")/operator-token-minters/principals.sh" '        *) SUBJECT="user:$name" ;;' \
    $'        *)\n          SUBJECT="user:$name"\n          if [[ "$name" =~ ^[^@/:[:space:]]+@[^@/:[:space:]]+\\.[^@/:[:space:]]+$ ]]; then SUBJECT_OK=true; fi\n          ;;'
  standing_people_world
  run_script="$copy" run --report "$report"
  problems="$(standing_people_problems "$report")"
  [[ "$problems" == *"no line 'FINDING user:alice@example.com: "* && "$problems" == *"no line 'FINDING user:carol@example.com: "* &&
    "$problems" == *"bob@example.com's patch"* && "$problems" != *"mallory@"* ]] ||
    fail "the control copy allowing an email User subject read as: ${problems:-nothing}"
  passed "(sb) a standing person bound in a cluster is a finding, on the operator's cluster, on another cluster of the pool and through a right a role limits by name, naming the right and where it was granted; the same world without the grants exits 0 and counts a bound person; control: a copy allowing an email User subject misses them and still finds the people bound in IAM"
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

# --- (k) No identity that may run the live steps. A refused analysis names the three permissions
# Google requires on the scope, cloudasset.assets.analyzeIamPolicy, searchAllResources and
# searchAllIamPolicies, and keeps Google's own line; control: a copy whose message drops
# searchAllResources fails the row's check. ---
analyze_names_all() {
  local step="$1" line needs name
  line="$(grep -F -- "UNKNOWN $step: Policy Analyzer refused the analysis (it needs " <<<"$output" || true)"
  [ -n "$line" ] || return 1
  # What the message says the analysis needs, apart from Google's own line after it.
  needs="${line#*"(it needs "}" needs="${needs%%"): "*}"
  for name in cloudasset.assets.analyzeIamPolicy cloudasset.assets.searchAllResources cloudasset.assets.searchAllIamPolicies; do
    [[ "$needs" == *"$name"* ]] || return 1
  done
  [[ "$needs" == *"on organizations/$org"* && "$line" == *"): ERROR: (gcloud.asset.analyze-iam-policy) PERMISSION_DENIED: $2" ]]
}
if row k; then
  base_world
  : >"$world/clusters/operator-cluster/refuse-sar"
  : >"$world/refuse-analyze"
  run
  want_status 2 "no identity that may run the live steps"
  want_text "UNKNOWN live: RBAC on operator-cluster: the API server refused the review (it needs create on subjectaccessreviews, container.subjectAccessReviews.create on GKE)" \
    "a refused review"
  for step in "live: GKE IAM on project $project" "live: minters of $gsa" "live: hold writers: Cloud SQL users of example-instance"; do
    want_text "UNKNOWN $step: Policy Analyzer refused the analysis (it needs cloudasset.assets.analyzeIamPolicy, cloudasset.assets.searchAllResources and cloudasset.assets.searchAllIamPolicies on organizations/$org, iam.roles.get for a custom role, and Google Workspace's groups.read to expand a group): ERROR: (gcloud.asset.analyze-iam-policy) PERMISSION_DENIED: Permission 'cloudasset.assets.analyzeIamPolicy' denied" \
      "a refused analysis"
  done
  [ "$(count_prefix 'FINDING ')" -eq 0 ] || fail "no identity that may run the live steps: a finding"
  passed "(k) with no identity that may run them, the live steps print UNKNOWN naming the permission, and the run exits 2"

  base_world
  : >"$world/refuse-analyze-search"
  run
  want_status 2 "an analysis refused for searchAllResources"
  analyze_names_all "live: minters of $gsa" "Caller doesn't have cloudasset.assets.searchAllResources permission on the requested scope" ||
    fail "the refused analysis does not name each of the three permissions and Google's own line: $(grep -F "UNKNOWN live: minters of $gsa" <<<"$output" || true)"
  copy="$(script_copy analyze-two-names)"
  plant "$(dirname "$copy")/operator-token-minters/iam-analysis.sh" 'cloudasset.assets.analyzeIamPolicy, cloudasset.assets.searchAllResources and cloudasset.assets.searchAllIamPolicies on $4' \
    'cloudasset.assets.analyzeIamPolicy and cloudasset.assets.searchAllIamPolicies on $4'
  run_script="$copy" run
  ! analyze_names_all "live: minters of $gsa" "Caller doesn't have cloudasset.assets.searchAllResources permission on the requested scope" ||
    fail "a copy whose message drops searchAllResources still passed the message check"
  passed "(k) a refused analysis names analyzeIamPolicy, searchAllResources and searchAllIamPolicies on the scope and keeps Google's line; a copy dropping searchAllResources fails the check"
fi

# --- (l) The admin tag, from the secret's annotations. A secret that releases the operator's
# credential is a standing route whatever its tags: with both, each release waits on one person's
# approval, the person an agent acts for included, and "An agent's request that is approved by the
# person the agent acts for has one person in it, not two, so it does not meet this rule either"
# (INFRA-840's c-operator-grant-door). The base world holds no ENVOY_OPERATOR, so each item that
# reads one stores it here. ---
readonly secret_id="projects/$number/secrets/agent-secrets-envoy-operator"
# What the step says it does not read, beside its PASS and its release finding.
readonly not_read="who can change the secret's annotations or create it (secretmanager.secrets.update on it, secretmanager.secrets.create on its project) is not read"
readonly no_route="PASS admin tag of ENVOY_OPERATOR: no such secret $secret_id, and project $number lists 1 secret(s) whose id starts agent-secrets-, so --secrets-prefix names where the broker keeps its secrets: nothing releases the operator's credential through the broker; $not_read"
admin_tag_row() {
  local args script="$check"
  base_world
  case "$1" in
    present | no-admin | admin-outside | agent-tier | mistyped | mistyped-control) broker_secret ENVOY_OPERATOR true ;;
  esac
  case "$1" in
    mistyped-control)
      # The step before the witness: NOT_FOUND read as no route, whatever the list holds.
      script="$(script_copy "no-witness-$1")"
      plant "$(dirname "$script")/operator-token-minters/step-admin-tag.sh" '  elif [ "$listed" -eq 0 ]; then' '  elif false; then'
      ;;
    other-project-control)
      script="$(script_copy "list-project-$1")"
      plant "$(dirname "$script")/operator-token-minters/step-admin-tag.sh" 'gcloud secrets list --project="$prefix_project"' 'gcloud secrets list --project="$PROJECT"'
      ;;
  esac
  case "$1" in
    present)
      run
      want_status 1 "ENVOY_OPERATOR at human tier with --admin"
      want_line "FINDING secret:ENVOY_OPERATOR: admin is \"true\" and tier is \"human\": each release waits on one person's approval and no second person's, so whoever may ask for it holds the operator identity standing (INFRA-840's R16), an open route until R16's grant door exists; $not_read" \
        "ENVOY_OPERATOR with both annotations"
      ! has_prefix "PASS admin tag of ENVOY_OPERATOR: " || fail "ENVOY_OPERATOR with both annotations: the step passed"
      ;;
    no-admin)
      edit "$world/secrets/ENVOY_OPERATOR.json" '.admin = ""'
      run
      want_status 1 "ENVOY_OPERATOR without the admin annotation"
      want_line "FINDING secret:ENVOY_OPERATOR: admin is \"\", not \"true\": without the administrator tag a model judge may decide its release" "no admin annotation"
      ;;
    admin-outside)
      : >"$world/admin-outside-annotations"
      run
      want_status 1 "admin held beside the annotations"
      want_line "FINDING secret:ENVOY_OPERATOR: admin is \"\", not \"true\": without the administrator tag a model judge may decide its release" \
        "admin held beside the annotations"
      ;;
    agent-tier)
      edit "$world/secrets/ENVOY_OPERATOR.json" '.tier = "agent"'
      run
      want_status 1 "ENVOY_OPERATOR at agent tier"
      want_line "FINDING secret:ENVOY_OPERATOR: tier is \"agent\", not \"human\"" "agent tier"
      ;;
    missing)
      run
      want_status 0 "no ENVOY_OPERATOR, the prefix holding the scratch secret"
      want_line "$no_route" "no ENVOY_OPERATOR"
      ;;
    mistyped | mistyped-control)
      # The real secret is there; the prefix misses it by one letter, so describe answers NOT_FOUND
      # for a secret that exists.
      run_script="$script" run --secrets-prefix "projects/$number/secrets/agent-secret-"
      if [ "$1" = mistyped ]; then
        want_status 2 "a mistyped --secrets-prefix beside the real ENVOY_OPERATOR"
        want_text "UNKNOWN admin tag of ENVOY_OPERATOR: no such secret projects/$number/secrets/agent-secret-envoy-operator, and project $number lists no secret whose id starts agent-secret-: either the broker keeps no secret there or --secrets-prefix names the wrong place" \
          "a mistyped --secrets-prefix"
        ! has_prefix "PASS admin tag of ENVOY_OPERATOR: " || fail "a mistyped --secrets-prefix: the step passed"
      else
        want_text "PASS admin tag of ENVOY_OPERATOR: no such secret projects/$number/secrets/agent-secret-envoy-operator" "the copy without the list check"
      fi
      ;;
    empty)
      rm "$world/secrets/ENVOY_OPERATOR_CONTROL.json"
      echo other-program-key >"$world/other-secrets"
      run
      want_status 2 "a prefix whose project lists no secret under it"
      want_text "UNKNOWN admin tag of ENVOY_OPERATOR: no such secret $secret_id, and project $number lists no secret whose id starts agent-secrets-" \
        "a prefix whose project lists no secret under it"
      ;;
    other-project | other-project-control)
      run_script="$script" run --secrets-prefix "projects/999999999999/secrets/agent-secrets-"
      if [ "$1" = other-project ]; then
        want_status 2 "a prefix naming another project"
        want_text "UNKNOWN admin tag of ENVOY_OPERATOR: no such secret projects/999999999999/secrets/agent-secrets-envoy-operator, and project 999999999999 lists no secret whose id starts agent-secrets-" \
          "a prefix naming another project"
      else
        want_text "PASS admin tag of ENVOY_OPERATOR: no such secret projects/999999999999/secrets/agent-secrets-envoy-operator" "the copy listing --project"
      fi
      ;;
    list-refused)
      : >"$world/refuse-secret-list"
      run
      want_status 2 "a refused list of the prefix's secrets"
      want_text "UNKNOWN admin tag of ENVOY_OPERATOR: no such secret $secret_id, and gcloud secrets list --project=$number was refused (it needs secretmanager.secrets.list on the project)" \
        "a refused list of the prefix's secrets"
      ;;
    control-secret)
      run --secret ENVOY_OPERATOR_CONTROL
      want_status 1 "the scratch secret at human tier without --admin"
      want_line "FINDING secret:ENVOY_OPERATOR_CONTROL: admin is \"\", not \"true\": without the administrator tag a model judge may decide its release" \
        "the scratch secret"
      ;;
    refused)
      : >"$world/refuse-secret-show"
      run
      want_status 2 "a refused describe"
      want_text "UNKNOWN admin tag of ENVOY_OPERATOR: gcloud secrets describe $secret_id was refused (it needs secretmanager.secrets.get on the secret): ERROR: (gcloud.secrets.describe) PERMISSION_DENIED" \
        "a refused describe"
      ;;
    no-prefix)
      mapfile -t args < <(args_without --secrets-prefix)
      run_only "${args[@]}"
      want_status 2 "no --secrets-prefix"
      want_text "UNKNOWN admin tag of ENVOY_OPERATOR: no --secrets-prefix" "no --secrets-prefix"
      ;;
  esac
}
if row l; then
  each admin_tag_row present no-admin admin-outside agent-tier missing mistyped mistyped-control empty other-project other-project-control \
    list-refused control-secret refused no-prefix
  passed "(l) the admin tag is read from the secret's annotations: ENVOY_OPERATOR at human tier with --admin, a missing admin annotation, admin outside them, agent tier and the scratch secret each exit 1; no secret passes only where the prefix's project lists the broker's secrets, and a mistyped prefix, an empty project, another project and a refused list read UNKNOWN, which copies without that witness miss; a refused describe and no --secrets-prefix read UNKNOWN"
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
  jq -e 'any(.unknown[]?; .step == "admin tag of ENVOY_OPERATOR" and (.reason | contains("PERMISSION_DENIED")))' "$report" >/dev/null ||
    fail "the report's unknown does not list the step 'admin tag of ENVOY_OPERATOR': $(jq -c '.unknown // "absent"' "$report")"
  jq -e --argjson ra "$(renders_allowances)" 'keys == ["allowances", "credited", "findings", "residuals", "run_at", "steps", "unknown"] and .allowances == $ra and .credited == []
    and .residuals == [] and all(.findings[]; keys == ["granted_at", "permissions", "principal", "resource", "right", "role"])
    and all(.unknown[]; keys == ["reason", "step"]) and (.steps | length) == 11 and all(.steps[]; keys == ["state", "step"])' "$report" >/dev/null ||
    fail "the report holds other fields than run_at, findings (principal, right, role, resource, permissions, granted_at), an empty credited and residuals, unknown, the renders' allowances and the 11 steps alone: $(jq -c . "$report")"
  passed "(m) --report writes run_at, findings, credited, residuals, unknown, allowances and steps, with the stubbed UNKNOWN step under unknown"
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
      and .resource == "cluster:example-project/us-central1/operator-cluster" and .permissions == ["create pods/exec in envoy-operator"]
      and (.granted_at | startswith("RBAC: allowed by")))' "$report" >/dev/null ||
    fail "the RBAC finding's parts are not its label, cluster:example-project/us-central1/operator-cluster and the API server's reason: $(jq -c '[.findings[] | select(.principal == "system:serviceaccount:legion:worker")]' "$report")"
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
  # Every cluster the report names, a declared finding's and a credit's among them, is named in full.
  [ -z "$(full_labels "$temporary_dir/fixture-fresh.json")" ] ||
    fail "the fixture's report names a cluster other than cluster:<project>/<location>/<name>: $(full_labels "$temporary_dir/fixture-fresh.json")"
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

# --- (sc) No part of the check can vanish from a run that exits 0. An empty file sources as a
# clean no-op under set -e, and an empty jq program is `.`: at 042cb479c40 an emptied
# step-admin-tag.sh, step-code-rbac.sh, step-gke-iam.sh, step-live-iam.sh, step-permission-names.sh,
# step-workload-identity.sh, step-audited-clusters.sh, node-escape.sh or become-rights.jq each left
# (a)'s world at exit 0, measured. The guard refuses an empty part before any step runs; the census
# names a step file that ran and reported nothing, or never ran. ---
readonly check_parts=(output.sh rights.sh principals.sh checks.sh reviews.sh admission-backends.sh broker-fence.sh iam-analysis.sh node-escape.sh
  step-permission-names.sh step-code-secrets.sh step-code-rbac.sh step-code-iam.sh step-clusters.sh step-gke-iam.sh step-rbac.sh
  step-workload-identity.sh step-audited-clusters.sh step-live-iam.sh step-admin-tag.sh analysis.jq become-rights.jq broker-fence.jq
  broker-fence.json code-iam.jq code-rbac.jq custom.jq custom-rights.jq fleet.jq guard-rights.jq binding-rights.jq rec.jq review-answer.jq
  review-request.jq webhook-backends.jq)
readonly step_files=(step-permission-names.sh step-code-secrets.sh step-code-rbac.sh step-code-iam.sh step-clusters.sh step-gke-iam.sh
  step-rbac.sh step-workload-identity.sh step-audited-clusters.sh step-live-iam.sh step-admin-tag.sh)
empty_part_row() {
  local script report="$temporary_dir/sc-$1.json"
  base_world
  script="$(script_copy "sc-$1")"
  : >"$(dirname "$script")/operator-token-minters/$1"
  run_script="$script" run --report "$report"
  want_status 2 "a copy whose $1 is empty"
  want_text "UNKNOWN run: the parts $1 of " "a copy whose $1 is empty"
  want_text "cannot be read or are empty, so no step runs" "a copy whose $1 is empty"
  [ "$(count_prefix 'PASS ')" -eq 0 ] || fail "a copy whose $1 is empty ran a step"
  jq -e --arg p "$1" '.findings == [] and any(.unknown[]; .step == "run" and (.reason | contains($p)))
    and (.steps | length) == 11 and all(.steps[]; .state == "not run")' "$report" >/dev/null 2>&1 ||
    fail "a copy whose $1 is empty: the report does not hold the UNKNOWN run and every step not run: $(cat "$report" 2>&1)"
}
# census_row ITEM: a copy without the empty-part guard, with the step file ITEM emptied, or (never-ran)
# with step-admin-tag.sh's line taken out, or (census-off) without the census too and
# step-admin-tag.sh emptied.
census_row() {
  local script lib target="$1"
  base_world
  script="$(script_copy "census-$1")"
  lib="$(dirname "$script")/operator-token-minters"
  plant "$script" '[ -r "$LIB/$part" ] && [ -s "$LIB/$part" ] ||' '[ -r "$LIB/$part" ] ||'
  case "$1" in
    never-ran)
      target="step-admin-tag.sh"
      plant "$script" '. "$LIB/step-admin-tag.sh"' ':'
      plant "$script" 'census_end step-admin-tag.sh' ':'
      ;;
    census-off)
      target="step-admin-tag.sh"
      : >"$lib/$target"
      plant "$lib/output.sh" 'census_end() {' 'census_end() { return 0;'
      plant "$lib/output.sh" 'census_close() {' 'census_close() { return 0;'
      ;;
    *) : >"$lib/$1" ;;
  esac
  run_script="$script" run
  case "$1" in
    census-off)
      want_status 0 "a copy without the guard and the census, step-admin-tag.sh emptied"
      [ "$(count_prefix 'UNKNOWN ')" -eq 0 ] || fail "a copy without the guard and the census read UNKNOWN"
      ;;
    never-ran)
      [ "$status" -ne 0 ] || fail "a copy that never runs step-admin-tag.sh exited 0"
      want_line "UNKNOWN run: the step file step-admin-tag.sh never ran, so what it checks is unread" "a copy that never runs step-admin-tag.sh"
      ;;
    *)
      [ "$status" -ne 0 ] || fail "a copy whose $target is empty exited 0"
      want_line "UNKNOWN run: the step file $target ran and reported nothing, no PASS, FINDING, UNKNOWN or credit, and gave no reason, so what it checks is unread" \
        "a copy whose $target is empty"
      ;;
  esac
}
if row sc; then
  each empty_part_row "${check_parts[@]}"
  passed "(sc) a copy with any one of its ${#check_parts[@]} parts empty prints UNKNOWN run naming it, runs no step, reports each step not run, and exits 2"
  each census_row "${step_files[@]}" never-ran census-off
  passed "(sc) past the guard, the census names each of the ${#step_files[@]} step files emptied, and one that never ran, and the run does not exit 0; a copy without the census exits 0"
  base_world
  run --report "$temporary_dir/sc-clean.json"
  want_status 0 "a clean run's step census"
  jq -e '[.steps[] | "\(.step)=\(.state)"] == ([$ARGS.positional[] | if . == "step-audited-clusters.sh" then "\(.)=quiet: no --audited-cluster: the step does not run" else "\(.)=reported" end])' \
    "$temporary_dir/sc-clean.json" --args "${step_files[@]}" >/dev/null ||
    fail "a clean run's --report does not list each step file reported: $(jq -c .steps "$temporary_dir/sc-clean.json")"
  passed "(sc) a clean run's --report lists each step file reported, and live: audited clusters quiet without its flag"
fi

# --- (rl) A role file the permission check cannot read stops the run with an UNKNOWN naming the
# role, never a quiet stop or a role read as listing nothing: a copy whose role_listed reads a file
# that is not there. ---
if row rl; then
  base_world
  copy="$(script_copy role-unread)"
  plant "$(dirname "$copy")/operator-token-minters/checks.sh" 'ROLE_LISTED["$ROLE_FILE $permission"]=1; done <"$ROLE_FILE"; }' \
    'ROLE_LISTED["$ROLE_FILE $permission"]=1; done <"$ROLE_FILE.gone"; }'
  run_script="$copy" run
  want_status 2 "a role file that cannot be read"
  want_text "UNKNOWN run: the permissions file of role roles_container.admin (role_perms) could not be read" "a role file that cannot be read"
  want_text "UNKNOWN run: the run stopped before its end (exit 2)" "a role file that cannot be read"
  passed "(rl) a role file that cannot be read stops the run with an UNKNOWN naming the role"
fi

# --- (cj) The allowances: --service-agent names a Google service agent alone, and every allowance a
# run is given (--service-agent, --rbac-only-member, --fleet none, --renders-admin and each
# --render-writer, --credentials-arg, which chooses how each cluster is reached) is written into
# --report, so a reader can hold each to what was accepted. ---
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
      jq -e --arg agent "service-$number@gcp-sa-cloudbuild.iam.gserviceaccount.com" --argjson ra "$(renders_allowances)" '.allowances == [
          {flag: "--service-agent", value: $agent}, {flag: "--rbac-only-member", value: "edge"}, {flag: "--fleet", value: "none"}]
          + $ra + [{flag: "--credentials-arg", value: "--internal-ip"}]' "$report" >/dev/null ||
        fail "the report does not hold the allowances the run was given: $(jq -c '.allowances // "absent"' "$report")"
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

printed_secret_data() { has_prefix "SECRET-DATA-CANARY-7f3a" || has_prefix "$(printf '%s' SECRET-DATA-CANARY-7f3a | base64)"; }

 # --- (cp) The auditor never asks the API server for a Secret. ---
 if row cp; then
   base_world
   jq -n '[{namespace: "envoy-operator", name: "envoy-operator-token-legacy"}, {namespace: "secrets-broker", name: "secrets-broker-token-legacy"}]' \
     >"$world/clusters/operator-cluster/token-secrets.json"
   : >"$world/clusters/operator-cluster/token-secret-data"
   run
   want_status 0 "an auditor that does not read Secrets"
   [ ! -e "$world/clusters/operator-cluster/token-secret-responses" ] ||
     fail "the auditor asked the API server for token Secrets"
   [ ! -e "$world/clusters/operator-cluster/secret-data-asked" ] ||
     fail "the auditor asked the API server for token Secrets whole"
   ! printed_secret_data || fail "the auditor printed a token Secret's data"
   passed "(cp) token Secrets present on the API server are never read"
 fi

# --- (rd) The recorded Kubernetes reads are the declared least privilege, and its inventory: a read
# reads.json lacks fails the row whatever the check's exit, a row that wants exit 1 included, and
# an entry no read uses is named by unused_reads, which the run of every row holds reads.json to. ---
if row rd; then
  base_world
  add_cluster other-cluster us-east1
  run
  want_status 0 "every recorded Kubernetes read is declared"
  [ "$reads_failed" = false ] || fail "every recorded Kubernetes read is declared: the reads failed"

  jq 'del(.[] | select(.step == "step-workload-identity.sh" and .verb == "get" and .apiGroup == "" and .resource == "namespaces"))' \
    "$reads" >"$temporary_dir/reads-without-namespaces.json"
  expect_reads_failure=true OPERATOR_MINTERS_READS="$temporary_dir/reads-without-namespaces.json" run
  [ "$reads_failed" = true ] || fail "a read removed from the manifest did not fail the run's reads"
  want_text "FAIL: recorded kubectl get namespaces is absent from reads.json for step-workload-identity.sh" \
    "a missing get namespaces declaration"
  passed "(rd) each fake kubectl call is declared, and dropping get namespaces fails the workload-identity row"

  # A row that wants a finding's exit 1, under a manifest without subjectaccessreviews: its run
  # fails the row, rather than its exit 1 reading as the finding's.
  base_world
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" envoy-operator ""
  jq 'del(.[] | select(.resource == "subjectaccessreviews"))' "$reads" >"$temporary_dir/reads-without-reviews.json"
  absorbed="$( (OPERATOR_MINTERS_READS="$temporary_dir/reads-without-reviews.json" run
    want_status 1 "a finding under a manifest without subjectaccessreviews"
    echo "ROW PASSED") 2>&1 || true)"
  [[ "$absorbed" != *"ROW PASSED"* ]] && [[ "$absorbed" == *"FAIL: a kubectl read this run made is not one reads.json declares"* ]] &&
    [[ "$absorbed" == *"FAIL: recorded kubectl create subjectaccessreviews is absent from reads.json for reviews.sh"* ]] ||
    fail "a row wanting exit 1 passed beside a read reads.json lacks: $absorbed"
  passed "(rd) a row that wants a finding's exit 1 fails on a read reads.json lacks, naming it"

  # The inventory's own control: an entry no recorded call uses is named, and only it.
  printf '%s\n' 'reviews.sh|create|authorization.k8s.io|subjectaccessreviews|' 'step-rbac.sh|list||serviceaccounts|' >"$temporary_dir/rd-calls.tsv"
  jq '[.[] | select(.resource == "subjectaccessreviews" or (.step == "step-rbac.sh" and .resource == "serviceaccounts"))]
      + [{step: "step-rbac.sh", verb: "list", apiGroup: "", resource: "secrets"}]' "$reads" >"$temporary_dir/reads-with-secrets.json"
  unused="$(unused_reads "$temporary_dir/reads-with-secrets.json" "$temporary_dir/rd-calls.tsv")"
  [ "$unused" = '{"step":"step-rbac.sh","verb":"list","apiGroup":"","resource":"secrets"}' ] ||
    fail "unused_reads named '$unused', not the one entry no call uses (list secrets)"
  passed "(rd) control: unused_reads names a reads.json entry no recorded call uses, list secrets, and no other"
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
