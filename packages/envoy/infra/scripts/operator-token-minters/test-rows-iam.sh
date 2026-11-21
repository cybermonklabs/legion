# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-iam.sh: the rows of operator-token-minters.test.sh on Google
# IAM: GKE IAM, the minters of the operator's and the broker's accounts, the hold writers, IAM
# impersonation and IAM writers, and a GKE fleet member's project and the folders above it.
# Sourced by the test after its world and helpers; its header says what the fakes model. The rows:
#
#   (d)  a container.* holder that is a CI account's or an agent's Google service account exits 1
#        naming it (the GKE IAM step's control);
#   (e)  each of the seven Google service account permissions held on the operator's account by an
#        agent's Google service account exits 1 naming the permission;
#   (f)  roles/iam.serviceAccountTokenCreator, roles/iam.serviceAccountKeyAdmin and
#        roles/iam.serviceAccountUser on the scratch account (--namespace envoy-operator-control
#        --gsa envoy-operator-control@...) each exit 1;
#   (n)  one holder of each hold-writer right (Cloud SQL users, the database Secret, the broker
#        program's state, the signing key) is a finding in the report;
#   (o)  the broker's one binding on the operator's account is allowed only when --broker-account
#        names it, and a minter of the broker's own account is a finding;
#   (q)  a Policy Analyzer answer longer than a pipe holds (3000 people beside the Workload Identity
#        member) still finds its witness, and exits 0;
#   (t)  grade-ci holding roles/container.admin on a GKE fleet member's project (example-grade)
#        exits 1, since GKE authorizes IAM beside RBAC;
#   (ad) container.replicationControllers.create and update and container.nodes.proxy, each held on
#        the project by an agent's Google service account, exit 1 (their RBAC rights are row b's);
#   (ae) a CI account holding roles/gkehub.admin on the pool project exits 1;
#   (ag) an agent's Google service account holding roles/iam.serviceAccountTokenCreator on a holder
#        of roles/container.admin is a finding naming the chain, through one service account and
#        through two;
#   (ah) a holder of roles/resourcemanager.projectIamAdmin is a finding, and so is a holder of each
#        of iam.serviceAccounts.setIamPolicy, secretmanager.secrets.setIamPolicy,
#        resourcemanager.organizations.setIamPolicy and iam.roles.update over a checked resource;
#   (at) grade-ci holding alone on a GKE fleet member's project each member permission (the
#        container.* escalation permissions, nodes.proxy and the IAM writers) exits 1;
#   (au) a holder of setIamPolicy on the folder above that member's project exits 1;
#   (av) grade-ci holding roles/resourcemanager.projectIamAdmin, or a role with
#        container.clusterRoleBindings.create and container.clusterRoles.bind, there exits 1.

# --- (d) A project-level container.admin held by a CI account, and by an agent's Google service
# account (the GKE IAM step's control). ---
container_admin_row() {
  base_world
  iam "$project_res" roles/container.admin "serviceAccount:$1"
  run
  want_status 1 "roles/container.admin held by $1"
  want_text "FINDING serviceAccount:$1: container.serviceAccounts.createToken," "roles/container.admin held by $1"
  want_text " on $project_res through roles/container.admin, granted at $project_res" "roles/container.admin held by $1"
}
if row d; then
  each container_admin_row "$ci_gsa" "$worker_gsa"
  passed "(d) roles/container.admin held by a CI account's or an agent's Google service account exits 1 naming it"
fi

# --- (e) Each of the seven Google service account permissions, held on the operator's account. ---
gsa_permissions=(iam.serviceAccounts.getAccessToken iam.serviceAccounts.getOpenIdToken iam.serviceAccounts.signJwt
  iam.serviceAccounts.signBlob iam.serviceAccounts.actAs iam.serviceAccounts.implicitDelegation iam.serviceAccountKeys.create)
gsa_permission_row() {
  base_world
  role "projects/$project/roles/only.$1" "$1"
  iam "$gsa_res" "projects/$project/roles/only.$1" "serviceAccount:$worker_gsa"
  run
  want_status 1 "$1 held by $worker_gsa"
  want_line "FINDING serviceAccount:$worker_gsa: $1 on $gsa_res through projects/$project/roles/only.$1, granted at $gsa_res" \
    "$1 held by $worker_gsa"
}
if row e; then
  each gsa_permission_row "${gsa_permissions[@]}"
  passed "(e) each of the seven Google service account permissions held on the operator's account exits 1 naming it"
fi

# --- (f) The plan's controls on the scratch account. ---
scratch_role_row() {
  base_world
  iam "$control_gsa_res" "$1" "serviceAccount:$worker_gsa"
  run --namespace envoy-operator-control --gsa "$control_gsa"
  want_status 1 "$1 on the scratch account"
  want_text "FINDING serviceAccount:$worker_gsa: " "$1 on the scratch account"
  want_text " on $control_gsa_res through $1, granted at $control_gsa_res" "$1 on the scratch account"
}
if row f; then
  each scratch_role_row roles/iam.serviceAccountTokenCreator roles/iam.serviceAccountKeyAdmin roles/iam.serviceAccountUser
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
  # in the check's order, from the three that make a guarded namespace, ServiceAccount or pod.
  want_text "FINDING serviceAccount:$grade_ci: container.namespaces.create, container.serviceAccounts.create, container.pods.create," \
    "grade-ci holding roles/container.admin on a fleet member's project"
  want_text " on $grade_res through roles/container.admin, granted at $grade_res" "grade-ci holding roles/container.admin on a fleet member's project"
  passed "(t) a Google service account holding roles/container.admin on a fleet member's project exits 1 naming it"
fi

# --- (ad) The new container.* permissions, through IAM. ---
new_container_row() {
  base_world
  role "projects/$project/roles/only.$1" "$1"
  iam "$project_res" "projects/$project/roles/only.$1" "serviceAccount:$worker_gsa"
  run
  want_status 1 "$1 held by $worker_gsa"
  want_line "FINDING serviceAccount:$worker_gsa: $1 on $project_res through projects/$project/roles/only.$1, granted at $project_res" "$1 held by $worker_gsa"
}
if row ad; then
  each new_container_row container.replicationControllers.create container.replicationControllers.update container.nodes.proxy
  passed "(ad) container.replicationControllers.create and update and container.nodes.proxy, each held by an agent's Google service account, exit 1"
fi

# --- (ae) Who can register a cluster to the pool's fleet. ---
if row ae; then
  base_world
  iam "$project_res" roles/gkehub.admin "serviceAccount:$ci_gsa"
  run
  want_status 1 "a CI account holding roles/gkehub.admin"
  want_line "FINDING serviceAccount:$ci_gsa: gkehub.memberships.create, gkehub.memberships.update on $project_res through roles/gkehub.admin, granted at $project_res" \
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

# --- (ah) Who can write IAM policy over a checked resource holds every permission there. ---
iam_writer_row() {
  local permission="${1%%=*}" resource="${1#*=}"
  base_world
  role "projects/$project/roles/only.$permission" "$permission"
  iam "$resource" "projects/$project/roles/only.$permission" "serviceAccount:$worker_gsa"
  run
  want_status 1 "$permission held by $worker_gsa on $resource"
  want_line "FINDING serviceAccount:$worker_gsa: $permission on $resource through projects/$project/roles/only.$permission, granted at $resource" \
    "$permission held by $worker_gsa on $resource"
}
if row ah; then
  base_world
  iam "$project_res" roles/resourcemanager.projectIamAdmin "serviceAccount:$ci_gsa"
  run
  want_status 1 "a CI account holding roles/resourcemanager.projectIamAdmin"
  want_line "FINDING serviceAccount:$ci_gsa: resourcemanager.projects.setIamPolicy on $project_res through roles/resourcemanager.projectIamAdmin, granted at $project_res" \
    "a CI account holding roles/resourcemanager.projectIamAdmin"
  passed "(ah) a holder of roles/resourcemanager.projectIamAdmin is a finding"

  each iam_writer_row "iam.serviceAccounts.setIamPolicy=$gsa_res" "secretmanager.secrets.setIamPolicy=$signing_res" \
    "resourcemanager.organizations.setIamPolicy=$org_res" "iam.roles.update=$project_res"
  passed "(ah) a holder of each IAM-writing permission over a checked resource is a finding"
fi

# --- (at) A fleet member's own project's IAM: each member permission held alone by grade-ci. ---
member_iam_permissions=(container.clusters.impersonate container.clusterRoles.bind container.clusterRoles.escalate
  container.clusterRoleBindings.create container.clusterRoleBindings.update container.roles.bind container.roles.escalate
  container.roleBindings.create container.roleBindings.update container.nodes.proxy resourcemanager.projects.setIamPolicy
  resourcemanager.folders.setIamPolicy resourcemanager.organizations.setIamPolicy iam.serviceAccounts.setIamPolicy
  secretmanager.secrets.setIamPolicy iam.roles.update)
member_iam_row() {
  base_world
  grade_member
  role "projects/$project/roles/only.$1" "$1"
  iam "$grade_res" "projects/$project/roles/only.$1" "serviceAccount:$grade_ci"
  run
  want_status 1 "$1 held by grade-ci on the fleet member's project"
  want_line "FINDING serviceAccount:$grade_ci: $1 on $grade_res through projects/$project/roles/only.$1, granted at $grade_res" \
    "$1 held by grade-ci on the fleet member's project"
}
if row at; then
  each member_iam_row "${member_iam_permissions[@]}"
  passed "(at) each of the ${#member_iam_permissions[@]} member permissions, held alone by grade-ci on a fleet member's project, exits 1 naming it"
fi

# --- (av) The reviewer's two roles on a fleet member's project: roles/resourcemanager.projectIamAdmin,
# and one holding both container.clusterRoleBindings.create and container.clusterRoles.bind. Each
# entry is the role and the permissions the line names. ---
member_role_row() {
  local role_name="${1%%=*}" held="${1#*=}"
  base_world
  grade_member
  [ "$role_name" != binder ] || {
    role_name="projects/$project/roles/binder"
    role "$role_name" container.clusterRoleBindings.create container.clusterRoles.bind
  }
  iam "$grade_res" "$role_name" "serviceAccount:$grade_ci"
  run
  want_status 1 "grade-ci holding $role_name on the fleet member's project"
  want_line "FINDING serviceAccount:$grade_ci: $held on $grade_res through $role_name, granted at $grade_res" \
    "grade-ci holding $role_name on the fleet member's project"
}
if row av; then
  each member_role_row "roles/resourcemanager.projectIamAdmin=resourcemanager.projects.setIamPolicy" \
    "binder=container.clusterRoles.bind, container.clusterRoleBindings.create"
  passed "(av) grade-ci holding roles/resourcemanager.projectIamAdmin, or a role with container.clusterRoleBindings.create and container.clusterRoles.bind, on a fleet member's project exits 1"
fi

# --- (au) The ancestors of a fleet member's project. ---
if row au; then
  base_world
  grade_member
  mkdir -p "$world/ancestors"
  jq -n '[{id: "example-grade", type: "project"}, {id: "200", type: "folder"}, {id: "100", type: "organization"}]' >"$world/ancestors/example-grade.json"
  iam "//cloudresourcemanager.googleapis.com/folders/200" roles/resourcemanager.folderIamAdmin "serviceAccount:$grade_ci"
  run
  want_status 1 "grade-ci holding roles/resourcemanager.folderIamAdmin on the folder above a fleet member's project"
  want_line "FINDING serviceAccount:$grade_ci: resourcemanager.folders.setIamPolicy on //cloudresourcemanager.googleapis.com/folders/200 through roles/resourcemanager.folderIamAdmin, granted at //cloudresourcemanager.googleapis.com/folders/200" \
    "grade-ci holding roles/resourcemanager.folderIamAdmin on the folder above a fleet member's project"
  passed "(au) a holder of setIamPolicy on the folder above a fleet member's project exits 1"
fi
