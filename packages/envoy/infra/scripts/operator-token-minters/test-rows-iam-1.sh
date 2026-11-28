# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-iam-1.sh: the rows of operator-token-minters.test.sh on Google
# IAM: GKE IAM, the minters of the operator's, the scratch and the broker's accounts, the hold
# writers, a long Policy Analyzer answer, a GKE fleet member's project, the new container.*
# permissions, who can register a cluster to the fleet, and IAM impersonation. Sourced by the test
# after its world and helpers; its header says what the fakes model. The rows:
#
#   (d)  a container.* holder that is a CI account's or an agent's Google service account exits 1
#        naming it (the GKE IAM step's control; the two in one world);
#   (e)  each of the seven Google service account permissions held on the operator's account by a
#        Google service account the check does not allow is a finding naming the permission (one
#        world, one account a permission);
#   (f)  roles/iam.serviceAccountTokenCreator, roles/iam.serviceAccountKeyAdmin and
#        roles/iam.serviceAccountUser on the scratch account (--namespace envoy-operator-control
#        --gsa envoy-operator-control@...) are each a finding (one world, one account a role);
#   (n)  one holder of each hold-writer right (Cloud SQL users, the database Secret, the broker
#        program's state, the signing key) is a finding in the report;
#   (o)  the broker's one binding on the operator's account is allowed only when --broker-account
#        names it, and a minter of the broker's own account is a finding;
#   (dd) the broker holding a second role on the operator's account (roles/iam.serviceAccountKeyAdmin
#        beside roles/iam.serviceAccountTokenCreator) is a finding naming that role;
#   (q)  a Policy Analyzer answer longer than a pipe holds (3000 people beside the Workload Identity
#        member) still finds its witness, and exits 0;
#   (t)  grade-ci holding roles/container.admin on a GKE fleet member's project (example-grade)
#        exits 1, since GKE authorizes IAM beside RBAC;
#   (ad) container.replicationControllers.create and update and container.nodes.proxy, each held on
#        the project by an agent's Google service account through a role granting it alone, are
#        each a finding naming it, in one world (their RBAC rights are row b's);
#   (ae) a CI account holding roles/gkehub.admin on the pool project exits 1;
#   (ag) an agent's Google service account holding roles/iam.serviceAccountTokenCreator on a holder
#        of roles/container.admin is a finding naming the chain, through one service account and
#        through two.

# --- (d) A project-level container.admin held by a CI account, and by an agent's Google service
# account (the GKE IAM step's control). The two share one world and one run, each member holding the
# role through a binding of its own on the project: the check prints each holder's finding on a line
# of its own, naming it, so each is still judged on its own line, one that both starts with its own
# member's text and ends with the project and the role (want_line_with), since both lines end with
# the same resource and role and two loose fragments of one member could be met by the other's line.
# want_recorded holds the run's last line to the FINDING and UNKNOWN lines it printed, so a line
# printed and not recorded fails the row. ---
container_admin_shared() {
  local member
  base_world
  for member in "$@"; do
    iam "$project_res" roles/container.admin "serviceAccount:$member"
  done
  run
  want_status 1 "roles/container.admin held by each of $*"
  want_recorded "roles/container.admin held by each of $*"
  for member in "$@"; do
    want_line_with "FINDING serviceAccount:$member: container.serviceAccounts.createToken," \
      " on $project_res through roles/container.admin, granted at $project_res" "roles/container.admin held by $member"
  done
}
if row d; then
  container_admin_shared "$ci_gsa" "$worker_gsa"
  passed "(d) roles/container.admin held by a CI account's or an agent's Google service account exits 1 naming it"
fi

# --- (e) Each of the seven Google service account permissions, held on the operator's account. The
# seven share one world and one run, each held through a role of its own that grants nothing else
# (plant_permissions, whose write refuses a role roles.json already holds) by a Google service
# account of its own, perm-<its place in the list>, which the check allows nowhere: the check prints
# each holder's finding once, naming the holder, so each permission is still judged by its own line,
# exact (want_planted lines, after want_own_principals holds the seven to seven members). Lines, not
# own: a holder of an impersonation permission also has a finding of its own beside it, the chain to
# what the operator's account may read. want_recorded holds the run's last line to the FINDING and
# UNKNOWN lines it printed, so a line printed and not recorded fails the row. ---
gsa_permissions=(iam.serviceAccounts.getAccessToken iam.serviceAccounts.getOpenIdToken iam.serviceAccounts.signJwt
  iam.serviceAccounts.signBlob iam.serviceAccounts.actAs iam.serviceAccounts.implicitDelegation iam.serviceAccountKeys.create)
gsa_permission_shared() {
  local permission i=0 what="the $# Google service account permissions, each held on the operator's account by perm-<its place>"
  base_world
  for permission in "$@"; do
    i=$((i + 1))
    plant_permissions "$gsa_res" "serviceAccount:perm-$i@$project.iam.gserviceaccount.com" "$permission"
  done
  run
  want_own_principals "$what"
  want_planted "$what" lines
  want_recorded "$what"
}
if row e; then
  gsa_permission_shared "${gsa_permissions[@]}"
  passed "(e) each of the seven Google service account permissions held on the operator's account exits 1 naming it"
fi

# --- (f) The plan's controls on the scratch account. The three roles share one world and one run,
# with the scratch account's flags, each held on the scratch account by a Google service account of
# its own, perm-<its place in the list>: the check prints each holder's finding once, naming the
# holder, so each role is still judged on its own line, one that both starts with its own member's
# text and ends with the scratch account and its own role (want_line_with). want_recorded holds the
# run's last line to the FINDING and UNKNOWN lines it printed, so a line printed and not recorded
# fails the row. ---
scratch_role_shared() {
  local role i=0
  base_world
  for role in "$@"; do
    i=$((i + 1))
    iam "$control_gsa_res" "$role" "serviceAccount:perm-$i@$project.iam.gserviceaccount.com"
  done
  run --namespace envoy-operator-control --gsa "$control_gsa"
  want_status 1 "each of $* on the scratch account"
  want_recorded "each of $* on the scratch account"
  i=0
  for role in "$@"; do
    i=$((i + 1))
    want_line_with "FINDING serviceAccount:perm-$i@$project.iam.gserviceaccount.com: " \
      " on $control_gsa_res through $role, granted at $control_gsa_res" "$role on the scratch account, held by perm-$i"
  done
}
if row f; then
  scratch_role_shared roles/iam.serviceAccountTokenCreator roles/iam.serviceAccountKeyAdmin roles/iam.serviceAccountUser
  passed "(f) roles/iam.serviceAccountTokenCreator, roles/iam.serviceAccountKeyAdmin and roles/iam.serviceAccountUser on the scratch account each exit 1"
fi

# --- (n) One holder of each hold-writer right. ---
if row n; then
  base_world
  iam "$project_res" roles/cloudsql.admin "serviceAccount:$worker_gsa"
  subject_grant operator-cluster "$(sa_subject legion worker)" get "" secrets "" example-db broker-db-login
  iam "$bucket_res" roles/storage.objectViewer "serviceAccount:$worker_gsa"
  iam "$signing_res" roles/secretmanager.secretAccessor "serviceAccount:$worker_gsa"
  run --report "$temporary_dir/report.json"
  want_status 1 "one holder of each hold-writer right"
  report="$temporary_dir/report.json"
  for right in "cloudsql.users.create, cloudsql.users.update on $sql_res" "storage.objects.get on $bucket_res" "secretmanager.versions.access on $signing_res"; do
    jq -e --arg p "serviceAccount:$worker_gsa" --arg r "$right" 'any(.findings[]; .principal == $p and (.right | startswith($r)))' "$report" >/dev/null ||
      fail "the report does not list serviceAccount:$worker_gsa with $right: $(jq -c .findings "$report")"
  done
  jq -e 'any(.findings[]; .principal == "system:serviceaccount:legion:worker" and .right == "get secrets broker-db-login in example-db")' "$report" >/dev/null ||
    fail "the report does not list legion/worker getting the database Secret: $(jq -c .findings "$report")"
  passed "(n) one holder of each hold-writer right is a finding in the report"
fi

# --- (o) The broker's one binding, and the broker account's own minters. ---
if row o; then
  base_world
  mapfile -t args < <(args_without --broker-account --broker-workload)
  run_only "${args[@]}"
  want_status 1 "the broker's binding without --broker-account"
  want_text "FINDING serviceAccount:$broker: iam.serviceAccounts.getAccessToken," "the broker's binding without --broker-account"
  passed "(o) the broker's roles/iam.serviceAccountTokenCreator on the operator's account is a finding unless --broker-account names it"

  base_world
  iam "$broker_res" roles/iam.serviceAccountTokenCreator "serviceAccount:$worker_gsa"
  run
  want_status 1 "a minter of the broker's own account"
  want_text "FINDING serviceAccount:$worker_gsa: iam.serviceAccounts.getAccessToken," "a minter of the broker's account"
  want_text " on $broker_res through roles/iam.serviceAccountTokenCreator" "a minter of the broker's account"
  passed "(o) a minter of the broker account's own tokens is a holder of the operator's power"

  base_world
  iam "$project_res" roles/iam.serviceAccountTokenCreator "serviceAccount:$broker"
  run
  want_status 1 "the broker minting through another binding"
  want_text "FINDING serviceAccount:$broker: iam.serviceAccounts.getAccessToken," "the broker minting through another binding"
  # On the operator's own account, so the finding is the operator check's: the broker account's
  # own minters check finds the same binding on the broker account, and a row reading that one
  # passed with the broker allowed in any role (the o-broker control, 2026-11-21).
  want_text " on $gsa_res through roles/iam.serviceAccountTokenCreator, granted at $project_res" "the broker minting through another binding"
  passed "(o) the broker's allowance is its one binding on the operator's account, by name: the same role on the project is a finding"
fi

# --- (dd) The broker's allowance is one role on the operator's account,
# roles/iam.serviceAccountTokenCreator. A second role of the broker there is a finding. With the
# allowance taken as any role of the broker, every other row passed (Tester5685, #5685). ---
if row dd; then
  base_world
  iam "$gsa_res" roles/iam.serviceAccountKeyAdmin "serviceAccount:$broker"
  run
  want_status 1 "the broker holding roles/iam.serviceAccountKeyAdmin on the operator's account too"
  want_line "FINDING serviceAccount:$broker: iam.serviceAccountKeys.create on $gsa_res through roles/iam.serviceAccountKeyAdmin, granted at $gsa_res" \
    "the broker holding a second role on the operator's account"
  passed "(dd) the broker holding a second role on the operator's account, beside its roles/iam.serviceAccountTokenCreator, is a finding naming that role"
fi

# --- (q) A long answer. Past the 64 KiB a pipe holds, `cut | grep -q` fails under pipefail with
# SIGPIPE once grep has matched, reading the witness as missing; the check must read the answer
# whole. ---
if row q; then
  base_world
  mapfile -t people < <(seq -f 'user:person-%04g@example.com' 1 3000)
  iam "$gsa_res" roles/iam.serviceAccountTokenCreator "${people[@]}"
  run
  want_status 0 "3000 people beside the Workload Identity member"
  want_text "PASS live: minters of $gsa: 3003 holders listed" "3000 people beside the Workload Identity member"
  passed "(q) a Policy Analyzer answer longer than a pipe holds still finds its witness"
fi

# --- (t) IAM on a fleet member's own project. ---
if row t; then
  base_world
  grade_member
  iam "$grade_res" roles/container.admin "serviceAccount:$grade_ci"
  run
  want_status 1 "grade-ci holding roles/container.admin on a fleet member's project"
  # roles/container.admin holds each member permission of the container.* kind; the line names them
  # in the check's order, from the six that make or patch a guarded namespace, ServiceAccount or pod.
  want_text "FINDING serviceAccount:$grade_ci: container.namespaces.create, container.namespaces.update, container.serviceAccounts.create, container.serviceAccounts.update, container.pods.create, container.pods.update," \
    "grade-ci holding roles/container.admin on a fleet member's project"
  want_text " on $grade_res through roles/container.admin, granted at $grade_res" "grade-ci holding roles/container.admin on a fleet member's project"
  passed "(t) a Google service account holding roles/container.admin on a fleet member's project exits 1 naming it"
fi

# --- (ad) The new container.* permissions, through IAM. ---
if row ad; then
  new_container_rows container.replicationControllers.create container.replicationControllers.update container.nodes.proxy
  passed "(ad) container.replicationControllers.create and update and container.nodes.proxy, each held by an agent's Google service account through a role granting it alone, are each a finding naming it"
fi

# --- (ae) Who can register a cluster to the pool's fleet. ---
if row ae; then
  base_world
  iam "$project_res" roles/gkehub.admin "serviceAccount:$ci_gsa"
  run
  want_status 1 "a CI account holding roles/gkehub.admin"
  want_line "FINDING serviceAccount:$ci_gsa: gkehub.memberships.create, gkehub.memberships.update, gkehub.namespaces.create, gkehub.namespaces.update, gkehub.rbacrolebindings.create, gkehub.rbacrolebindings.update, gkehub.membershipbindings.create, gkehub.membershipbindings.update, gkehub.scopes.create, gkehub.scopes.update, gkehub.scopes.setIamPolicy, gkehub.features.create, gkehub.features.update on $project_res through roles/gkehub.admin, granted at $project_res" \
    "a CI account holding roles/gkehub.admin"
  passed "(ae) a CI account holding roles/gkehub.admin on the pool project exits 1"
fi

# --- (ag) IAM impersonation: who can become a holder holds what it holds. ---
if row ag; then
  base_world
  iam "$project_res" roles/container.admin "serviceAccount:$applier"
  iam "$applier_res" roles/iam.serviceAccountTokenCreator "serviceAccount:$worker_gsa"
  run
  want_status 1 "an agent's Google service account minting the tokens of a container.admin holder"
  want_text "FINDING serviceAccount:$worker_gsa: container.serviceAccounts.createToken," "an agent's account minting a holder's tokens"
  want_text " on $project_res: serviceAccount:$worker_gsa -> impersonates serviceAccount:$applier (roles/iam.serviceAccountTokenCreator) -> roles/container.admin, granted at $project_res" \
    "an agent's account minting a holder's tokens"
  passed "(ag) an agent's Google service account holding roles/iam.serviceAccountTokenCreator on a holder is a finding naming the chain"

  base_world
  iam "$project_res" roles/container.admin "serviceAccount:$applier"
  iam "$applier_res" roles/iam.serviceAccountUser "serviceAccount:$relay"
  iam "$relay_res" roles/iam.serviceAccountTokenCreator "serviceAccount:$worker_gsa"
  run
  want_status 1 "an agent's Google service account two links from a container.admin holder"
  want_text " on $project_res: serviceAccount:$worker_gsa -> impersonates serviceAccount:$relay (roles/iam.serviceAccountTokenCreator) -> impersonates serviceAccount:$applier (roles/iam.serviceAccountUser) -> roles/container.admin, granted at $project_res" \
    "an agent's account two links from a holder"
  passed "(ag) a chain through two service accounts is followed to its outermost principal"
fi
