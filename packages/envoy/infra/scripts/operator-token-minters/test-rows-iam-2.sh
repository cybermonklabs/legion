# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-iam-2.sh: the rows of operator-token-minters.test.sh on Google
# IAM: IAM writers, a GKE fleet member's project and the folder above it, the admission, certificate
# and fleet permissions held through IAM, and a minter of a fleet member's RBAC holder. Sourced by
# the test after its world and helpers; its header says what the fakes model. The rows:
#
#   (ah) a holder of roles/resourcemanager.projectIamAdmin is a finding, and so is a holder of each
#        of iam.serviceAccounts.setIamPolicy, secretmanager.secrets.setIamPolicy,
#        resourcemanager.organizations.setIamPolicy and iam.roles.update over a checked resource
#        (those four in one world, one account a permission);
#   (at) grade-ci holding on a GKE fleet member's project each member permission (the container.*
#        escalation and admission permissions, nodes.proxy and the IAM writers), each through a role
#        granting it alone, is a finding naming it (one world, one role a permission);
#   (au) a holder of setIamPolicy on the folder above that member's project exits 1;
#   (av) grade-ci holding roles/resourcemanager.projectIamAdmin, or a role with
#        container.clusterRoleBindings.create and container.clusterRoles.bind, there exits 1;
#   (bh) container.mutatingWebhookConfigurations.create and update and
#        container.validatingWebhookConfigurations.create and update, each held on the project by an
#        agent's Google service account through a role granting it alone, are each a finding naming
#        it, in one world (their RBAC rights are row bf's);
#   (bk) container.certificateSigningRequests.approve held on the project by an agent's Google
#        service account exits 1 (its RBAC chain is row bj's);
#   (bl) each permission by which a holder makes a namespace or RBAC on the fleet's members
#        (gkehub.namespaces, rbacrolebindings and membershipbindings create and update, the scopes'
#        create, update and setIamPolicy, and the features' create and update), held on the pool
#        project by an agent's Google service account through a role granting it alone, is a
#        finding naming it, in one world;
#   (bn) grade-ci, bound to cluster-admin on a fleet member, is an RBAC finding, so an agent's
#        Google service account that may mint its token is a finding too, naming grade-ci's account.

# --- (ah) Who can write IAM policy over a checked resource holds every permission there. The four
# IAM-writing permissions share one world and one run, each held on its own resource through a role
# of its own that grants nothing else (plant_permissions, whose write refuses a role roles.json
# already holds) by a Google service account of its own, perm-<its place in the list>, which the
# check allows nowhere: the check prints each holder's finding once, naming the holder, so each
# permission is still judged by its own line, exact (want_planted lines, after want_own_principals
# holds the four to four members). want_recorded holds the run's last line to the FINDING and UNKNOWN
# lines it printed, so a line printed and not recorded fails the row. The projectIamAdmin holder
# keeps a run of its own. ---
iam_writer_shared() {
  local item i=0 what="the $# IAM-writing permissions, each held on its resource by perm-<its place>"
  base_world
  for item in "$@"; do
    i=$((i + 1))
    plant_permissions "${item#*=}" "serviceAccount:perm-$i@$project.iam.gserviceaccount.com" "${item%%=*}"
  done
  run
  want_own_principals "$what"
  want_planted "$what" lines
  want_recorded "$what"
}
if row ah; then
  base_world
  iam "$project_res" roles/resourcemanager.projectIamAdmin "serviceAccount:$ci_gsa"
  run
  want_status 1 "a CI account holding roles/resourcemanager.projectIamAdmin"
  want_line "FINDING serviceAccount:$ci_gsa: resourcemanager.projects.setIamPolicy on $project_res through roles/resourcemanager.projectIamAdmin, granted at $project_res" \
    "a CI account holding roles/resourcemanager.projectIamAdmin"
  passed "(ah) a holder of roles/resourcemanager.projectIamAdmin is a finding"

  iam_writer_shared "iam.serviceAccounts.setIamPolicy=$gsa_res" "secretmanager.secrets.setIamPolicy=$signing_res" \
    "resourcemanager.organizations.setIamPolicy=$org_res" "iam.roles.update=$project_res"
  passed "(ah) a holder of each IAM-writing permission over a checked resource is a finding"
fi

# --- (at) A fleet member's own project's IAM: each member permission held by grade-ci through a
# role of its own that grants nothing else (plant_permissions), all in one world and one run. ---
member_iam_permissions=(container.namespaces.update container.serviceAccounts.update container.pods.update
  container.clusters.impersonate container.clusterRoles.bind container.clusterRoles.escalate
  container.clusterRoleBindings.create container.clusterRoleBindings.update container.roles.bind container.roles.escalate
  container.roleBindings.create container.roleBindings.update container.nodes.proxy container.mutatingWebhookConfigurations.create
  container.mutatingWebhookConfigurations.update container.validatingWebhookConfigurations.create
  container.validatingWebhookConfigurations.update container.certificateSigningRequests.approve container.clusters.update
  resourcemanager.projects.setIamPolicy
  resourcemanager.folders.setIamPolicy resourcemanager.organizations.setIamPolicy iam.serviceAccounts.setIamPolicy
  secretmanager.secrets.setIamPolicy iam.roles.update)
if row at; then
  base_world
  grade_member
  plant_permissions "$grade_res" "serviceAccount:$grade_ci" "${member_iam_permissions[@]}"
  run
  want_planted "grade-ci holding each member permission through a role of its own on the fleet member's project" lines
  passed "(at) each of the ${#member_iam_permissions[@]} member permissions, held by grade-ci on a fleet member's project through a role granting it alone, is a finding naming it"
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

# --- (bh) The admission permissions, through IAM. GKE's IAM authorizer checks these for the webhook
# configurations' writes; roles/container.admin names none for the admission policies. ---
if row bh; then
  new_container_rows container.mutatingWebhookConfigurations.create container.mutatingWebhookConfigurations.update \
    container.validatingWebhookConfigurations.create container.validatingWebhookConfigurations.update
  passed "(bh) container.mutatingWebhookConfigurations.create and update and container.validatingWebhookConfigurations.create and update, each held by an agent's Google service account through a role granting it alone, are each a finding naming it"
fi

# --- (bk) The certificate approval permission, through IAM: GKE's IAM authorizer checks
# container.certificateSigningRequests.approve for an approval (row bj has its RBAC chain). ---
if row bk; then
  new_container_rows container.certificateSigningRequests.approve
  passed "(bk) container.certificateSigningRequests.approve held by an agent's Google service account exits 1"
fi

# --- (bl) The fleet's namespaces, RBAC and scopes, through IAM: a fleet scope's namespace and RBAC
# role binding reach every member cluster bound to it, the operator's namespace and Workload
# Identity member included. ---
if row bl; then
  new_container_rows gkehub.namespaces.create gkehub.namespaces.update gkehub.rbacrolebindings.create \
    gkehub.rbacrolebindings.update gkehub.membershipbindings.create gkehub.membershipbindings.update gkehub.scopes.create \
    gkehub.scopes.update gkehub.scopes.setIamPolicy gkehub.features.create gkehub.features.update
  passed "(bl) each fleet namespace, RBAC, scope and feature permission, held on the pool project by an agent's Google service account through a role granting it alone, is a finding naming it"
fi

# --- (bn) Who can mint the token of a Google service account an RBAC finding names holds what the
# finding gives it: once grade-ci's finding is accepted, an agent's account that may become grade-ci
# must still be reported. ---
if row bn; then
  base_world
  grade_member
  cluster_admin grade-cluster "$(user_subject "$grade_ci")"
  grade_ci_res="//iam.googleapis.com/projects/example-grade/serviceAccounts/$grade_ci"
  iam "$grade_ci_res" roles/iam.serviceAccountTokenCreator "serviceAccount:$worker_gsa"
  run
  want_status 1 "an agent's account that may mint grade-ci's token"
  want_text "FINDING serviceAccount:$worker_gsa: iam.serviceAccounts.getAccessToken," "an agent's account that may mint grade-ci's token"
  want_text " on $grade_ci_res through roles/iam.serviceAccountTokenCreator, granted at $grade_ci_res" "an agent's account that may mint grade-ci's token"
  ! has_prefix "PASS live: minters of $grade_ci, which an RBAC finding names: " || fail "the step that found worker's minting of grade-ci's token passed"
  passed "(bn) an agent's Google service account that may mint the token of grade-ci, an RBAC finding, exits 1 naming it"
fi
