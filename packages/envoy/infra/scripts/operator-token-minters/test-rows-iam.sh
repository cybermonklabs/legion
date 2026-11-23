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
#        container.* escalation and admission permissions, nodes.proxy and the IAM writers) exits 1;
#   (au) a holder of setIamPolicy on the folder above that member's project exits 1;
#   (av) grade-ci holding roles/resourcemanager.projectIamAdmin, or a role with
#        container.clusterRoleBindings.create and container.clusterRoles.bind, there exits 1;
#   (bh) container.mutatingWebhookConfigurations.create and update and
#        container.validatingWebhookConfigurations.create and update, each held on the project by an
#        agent's Google service account, exit 1 (their RBAC rights are row bf's);
#   (bk) container.certificateSigningRequests.approve held on the project by an agent's Google
#        service account exits 1 (its RBAC chain is row bj's);
#   (bl) each permission by which a holder makes a namespace or RBAC on the fleet's members
#        (gkehub.namespaces, rbacrolebindings and membershipbindings create and update, the scopes'
#        create, update and setIamPolicy, and the features' create and update), held alone on the
#        pool project by an agent's Google service account, exits 1;
#   (bn) grade-ci, bound to cluster-admin on a fleet member, is an RBAC finding, so an agent's
#        Google service account that may mint its token is a finding too, naming grade-ci's account;
#   (bq) container.clusters.update, by which a holder turns on legacy ABAC, held on the project by
#        an agent's Google service account exits 1;
#   (bs) container.secrets.create, by which a holder makes a token Secret for a guarded
#        ServiceAccount, held on the project by an agent's Google service account exits 1;
#   (cb) the Cloud Build service agent holding roles/container.admin exits 1 unless --service-agent
#        names it, and GKE's service agent holding it exits 0;
#   (cd) rv11: grade-ci, cluster-admin on a fleet member, has the Workload Identity member ci/runner,
#        and legion/worker may create pods in ci on the operator's cluster: exits 1 naming
#        legion/worker, who can become ci/runner and so act as grade-ci;
#   (ch) GKE's service agent of a project number the check does not read (999999999999) holding
#        roles/container.admin exits 1 naming the number; that of a fleet member's project
#        (example-grade, 222222222222) holding it there exits 0.

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
  # in the check's order, from the six that make or patch a guarded namespace, ServiceAccount or pod.
  want_text "FINDING serviceAccount:$grade_ci: container.namespaces.create, container.namespaces.update, container.serviceAccounts.create, container.serviceAccounts.update, container.pods.create, container.pods.update," \
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
  # An UNKNOWN beside the finding (a permission name the role does not hold) leaves exit 1 but means
  # the world is not the clean one plus this grant.
  [ "$(count_prefix 'UNKNOWN ')" -eq 0 ] || fail "$1 held by $worker_gsa: an UNKNOWN beside the finding"
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
member_iam_permissions=(container.namespaces.update container.serviceAccounts.update container.pods.update
  container.clusters.impersonate container.clusterRoles.bind container.clusterRoles.escalate
  container.clusterRoleBindings.create container.clusterRoleBindings.update container.roles.bind container.roles.escalate
  container.roleBindings.create container.roleBindings.update container.nodes.proxy container.mutatingWebhookConfigurations.create
  container.mutatingWebhookConfigurations.update container.validatingWebhookConfigurations.create
  container.validatingWebhookConfigurations.update container.certificateSigningRequests.approve container.clusters.update
  resourcemanager.projects.setIamPolicy
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

# --- (bh) The admission permissions, through IAM. GKE's IAM authorizer checks these for the webhook
# configurations' writes; roles/container.admin names none for the admission policies. ---
if row bh; then
  each new_container_row container.mutatingWebhookConfigurations.create container.mutatingWebhookConfigurations.update \
    container.validatingWebhookConfigurations.create container.validatingWebhookConfigurations.update
  passed "(bh) container.mutatingWebhookConfigurations.create and update and container.validatingWebhookConfigurations.create and update, each held by an agent's Google service account, exit 1"
fi

# --- (bk) The certificate approval permission, through IAM: GKE's IAM authorizer checks
# container.certificateSigningRequests.approve for an approval (row bj has its RBAC chain). ---
if row bk; then
  each new_container_row container.certificateSigningRequests.approve
  passed "(bk) container.certificateSigningRequests.approve held by an agent's Google service account exits 1"
fi

# --- (bl) The fleet's namespaces, RBAC and scopes, through IAM: a fleet scope's namespace and RBAC
# role binding reach every member cluster bound to it, the operator's namespace and Workload
# Identity member included. ---
if row bl; then
  each new_container_row gkehub.namespaces.create gkehub.namespaces.update gkehub.rbacrolebindings.create \
    gkehub.rbacrolebindings.update gkehub.membershipbindings.create gkehub.membershipbindings.update gkehub.scopes.create \
    gkehub.scopes.update gkehub.scopes.setIamPolicy gkehub.features.create gkehub.features.update
  passed "(bl) each fleet namespace, RBAC, scope and feature permission, held alone on the pool project by an agent's Google service account, exits 1"
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

# --- (bq) container.clusters.update, through IAM: a holder can turn on legacy ABAC, which authorizes
# requests beside RBAC. ---
if row bq; then
  each new_container_row container.clusters.update
  passed "(bq) container.clusters.update held by an agent's Google service account exits 1"
fi

# --- (bs) container.secrets.create, through IAM: a Secret of type kubernetes.io/service-account-token
# naming a ServiceAccount is filled with that ServiceAccount's token. ---
if row bs; then
  each new_container_row container.secrets.create
  passed "(bs) container.secrets.create held by an agent's Google service account exits 1"
fi

# --- (cb) Google's service agents: GKE's, GKE Hub's and Compute Engine's are allowed, and any other
# only by --service-agent, since a product's agent acts for whoever uses that product. ---
service_agent_row() {
  local agent
  base_world
  case "$1" in
    cloudbuild)
      agent="service-$number@gcp-sa-cloudbuild.iam.gserviceaccount.com"
      iam "$project_res" roles/container.admin "serviceAccount:$agent"
      run
      want_status 1 "the Cloud Build service agent holding roles/container.admin"
      want_text "FINDING serviceAccount:$agent: container.serviceAccounts.createToken," "the Cloud Build service agent holding roles/container.admin"
      ;;
    named)
      agent="service-$number@gcp-sa-cloudbuild.iam.gserviceaccount.com"
      iam "$project_res" roles/container.admin "serviceAccount:$agent"
      run --service-agent "$agent"
      want_status 0 "the Cloud Build service agent named by --service-agent"
      ;;
    gke)
      iam "$project_res" roles/container.admin "serviceAccount:service-$number@container-engine-robot.iam.gserviceaccount.com"
      run
      want_status 0 "GKE's service agent holding roles/container.admin"
      ;;
  esac
}
if row cb; then
  each service_agent_row cloudbuild named gke
  passed "(cb) another product's service agent holding roles/container.admin exits 1 unless --service-agent names it; GKE's exits 0"
fi

# --- (cd) rv11: who can become a Workload Identity member of an RBAC finding's Google account holds
# what the finding gives the account. ---
if row cd; then
  base_world
  grade_member
  cluster_admin grade-cluster "$(user_subject "$grade_ci")"
  grade_ci_res="//iam.googleapis.com/projects/example-grade/serviceAccounts/$grade_ci"
  add_sa operator-cluster ci runner
  iam "$grade_ci_res" roles/iam.workloadIdentityUser "serviceAccount:$project.svc.id.goog[ci/runner]"
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" ci ""
  run
  want_status 1 "legion/worker may create pods in ci, whose runner is grade-ci's Workload Identity member"
  want_text "FINDING system:serviceaccount:legion:worker: create pods in ci, so it can become system:serviceaccount:ci:runner" \
    "legion/worker may create pods in ci, whose runner is grade-ci's Workload Identity member"
  passed "(cd) rv11: who can become the Workload Identity member of an RBAC finding's Google account exits 1 naming it"
fi

# --- (ch) The service agents of the projects the check reads alone: another project's GKE robot acts
# for whoever owns that project. ---
foreign_agent_row() {
  local agent
  base_world
  case "$1" in
    unknown)
      agent="service-999999999999@container-engine-robot.iam.gserviceaccount.com"
      iam "$project_res" roles/container.admin "serviceAccount:$agent"
      run
      want_status 1 "the GKE service agent of an unknown project holding roles/container.admin"
      want_text "FINDING serviceAccount:$agent: container.serviceAccounts.createToken," "the GKE service agent of an unknown project"
      want_text "(the service agent of project number 999999999999, none of the projects this check reads)" "the GKE service agent of an unknown project"
      ;;
    member)
      grade_member
      iam "$grade_res" roles/container.admin "serviceAccount:service-222222222222@container-engine-robot.iam.gserviceaccount.com"
      run
      want_status 0 "the GKE service agent of a fleet member's project holding roles/container.admin there"
      ;;
  esac
}
if row ch; then
  each foreign_agent_row unknown member
  passed "(ch) GKE's service agent of an unknown project number holding roles/container.admin exits 1 naming the number; a fleet member project's exits 0"
fi
