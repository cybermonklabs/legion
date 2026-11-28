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
#        through two;
#   (ah) a holder of roles/resourcemanager.projectIamAdmin is a finding, and so is a holder of each
#        of iam.serviceAccounts.setIamPolicy, secretmanager.secrets.setIamPolicy,
#        resourcemanager.organizations.setIamPolicy and iam.roles.update over a checked resource;
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
#        (example-grade, 222222222222) holding it there exits 0;
#   (dm) Deployment Manager acts as the project's Google APIs service agent, which holds roles/editor:
#        an agent's Google service account holding roles/deploymentmanager.editor on the project
#        exits 1 naming it and the route, and a copy of the check without the step exits 0 in that
#        world (the step's control); a role list without deploymentmanager.deployments.update reads
#        UNKNOWN in the permission names, and a refused analysis UNKNOWN in the step.
#   (ua) the broker's name member alone, the broker's UID read on the operator's cluster, exits 0,
#        and the broker step's PASS names the member by UID it allows and the cluster it read;
#   (ub) the broker's name member and its member by UID, both on the broker account's workload
#        binding, exit 0, the broker, gsa and signing steps each passing;
#   (uc) the member by UID alone there exits 0, it being the broker step's witness;
#   (ud) the gsa and signing steps count the chain through the broker's member by UID as designed,
#        and print it as a finding once the broker's UID is another, so the member names another;
#   (ue) a member by UID of another ServiceAccount (legion/worker) beside the name member is a
#        finding on the broker, gsa and signing steps, each in the report with its parts;
#   (uf) that member alone gives the same three findings, and the broker step's UNKNOWN names both
#        of the broker's members as the witness the analysis does not list;
#   (ug) the broker's subject-form name member, its name member and its member by UID, each holding
#        roles/iam.serviceAccountTokenCreator on the operator's account, are each the same finding;
#   (uh) the broker's UID unread (the read refused, the ServiceAccount missing, each shape of an
#        answer that is no list, a UID that is no UUID): the member by UID beside the name member
#        reads UNKNOWN on the broker, gsa and signing steps, each naming why, and exits 2;
#   (ui) the member by UID alone, its UID unread, reads those three and the broker step's UNKNOWN
#        naming the name member alone as its witness;
#   (uj) the pool project's number unread: the member by UID reads UNKNOWN naming that, beside the
#        signing key's own UNKNOWN, and a member by another UID stays a finding;
#   (uk) neither of the broker's members on the broker's account reads UNKNOWN naming both;
#   (ul) a member by the broker's UID in another pool, the UID unread, is a finding;
#   (uo) the broker's UID is read on --cluster's cluster alone: the broker's pair on another cluster
#        of the pool, under another UID, is that cluster's finding and changes no verdict of the
#        member by UID;
#   (ut) with --pool-project example-pool, the member by UID of the pool's project and number passes
#        and is named in the broker step's PASS, and one of --project's number or pool is a finding;
#        a member by UID of ci/runner on relay@'s account is followed by the pool's number, and one
#        by --project's number is not;
#   (um) on every IAM check the member by the broker's UID is judged as the name member: their
#        findings as holders on the organization are twins, their chains through the broker are
#        designed, the ALLOWANCE of a credited grant's account has a twin, code: IAM's findings are
#        twins, and the steps a clean run passes are the checks the row lists;
#   (un) the broker's member by UID on grade-ci's account, an RBAC finding's, is followed on the
#        operator's cluster as its name member is: legion/worker, which may create pods in
#        secrets-broker, can become the broker's ServiceAccount and so act as grade-ci;
#   (va) ci/runner's member by UID on grade-ci's account is followed as its name member is: the
#        finding that legion/worker can become ci/runner and so act as grade-ci, and the same other
#        lines, but those of the clusters that do not hold the UID;
#   (vc) ci/runner on two clusters under two UIDs: each member by UID is followed on the cluster
#        that holds its UID alone, and the name member on both;
#   (vd) a member by a UID no listed ServiceAccount holds reads UNKNOWN on grade-ci's minters step,
#        naming the member and the UID, and nothing is followed from it;
#   (ve) with the pool project's number unread, ci/runner's member by UID reads UNKNOWN naming
#        that, and nothing is followed from it;
#   (vf) a member by UID of another pool, or of another project number, is not followed, as another
#        pool's name member is not.

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
# new_container_rows PERMISSION...: each permission held by an agent's Google service account on the
# project through a role of its own that grants nothing else (plant_permissions), all in one world
# and one run; each is a finding naming its role.
new_container_rows() {
  base_world
  plant_permissions "$project_res" "serviceAccount:$worker_gsa" "$@"
  run
  want_planted "$* held by $worker_gsa, each through a role of its own" lines
  # An UNKNOWN beside the findings (a permission name the role does not hold) leaves exit 1 but
  # means the world is not the clean one plus these grants.
  [ "$(count_prefix 'UNKNOWN ')" -eq 0 ] || fail "$* held by $worker_gsa: an UNKNOWN beside the findings"
}
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
  new_container_rows container.clusters.update
  passed "(bq) container.clusters.update held by an agent's Google service account exits 1"
fi

# --- (bs) container.secrets.create, through IAM: a Secret of type kubernetes.io/service-account-token
# naming a ServiceAccount is filled with that ServiceAccount's token. ---
if row bs; then
  new_container_rows container.secrets.create
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

# --- (dm) Who can make or change a Deployment Manager deployment acts as roles/editor. ---
readonly dm_step="live: Deployment Manager on project $project"
dm_row() {
  local copy
  base_world
  case "$1" in
    holder)
      iam "$project_res" roles/deploymentmanager.editor "serviceAccount:$worker_gsa"
      run
      want_status 1 "an agent's account holding roles/deploymentmanager.editor"
      want_line "FINDING serviceAccount:$worker_gsa: deploymentmanager.deployments.create, deploymentmanager.deployments.update on $project_res through roles/deploymentmanager.editor, granted at $project_res, by which Deployment Manager creates a deployment's resources as the project's Google APIs service agent, which holds roles/editor there" \
        "an agent's account holding roles/deploymentmanager.editor"
      [ "$(count_prefix 'UNKNOWN ')" -eq 0 ] || fail "an agent's account holding roles/deploymentmanager.editor: an UNKNOWN beside the finding"
      ;;
    control)
      # The same world, run by a copy without the step: what found the holder is the step.
      iam "$project_res" roles/deploymentmanager.editor "serviceAccount:$worker_gsa"
      copy="$temporary_dir/copy-without-dm-$$"
      rm -rf "$copy"
      mkdir -p "$copy"
      cp "$check" "$copy/operator-token-minters.sh"
      cp -r "$(dirname "$check")/operator-token-minters" "$copy/"
      sed -i '/^live_iam dm$/d' "$copy/operator-token-minters/step-gke-iam.sh"
      ! grep -qx 'live_iam dm' "$copy/operator-token-minters/step-gke-iam.sh" || fail "the test could not take the step out of its copy"
      run_script="$copy/operator-token-minters.sh" run
      want_status 0 "the holder of roles/deploymentmanager.editor, read by a copy without the step"
      ;;
    misspelt)
      edit "$world/roles.json" '.["roles/deploymentmanager.editor"] -= ["deploymentmanager.deployments.update"]'
      run
      want_status 2 "a role list without deploymentmanager.deployments.update"
      want_line "UNKNOWN permission names: deploymentmanager.deployments.update is not in roles/deploymentmanager.editor's includedPermissions, so no check that names it can find a holder" \
        "a role list without deploymentmanager.deployments.update"
      ;;
    unread)
      : >"$world/refuse-analyze"
      run
      want_status 2 "a refused analysis"
      want_text "UNKNOWN $dm_step: Policy Analyzer refused the analysis" "a refused analysis"
      ;;
  esac
}
if row dm; then
  each dm_row holder control misspelt unread
  passed "(dm) an agent's Google service account that may make or change a Deployment Manager deployment exits 1 naming it and the route, which a copy without the step misses; a misspelt name and a refused analysis read UNKNOWN"
fi

# --- (ua) to (ul), (uo), (ut) and (um): the broker's ServiceAccount named by its UID. Each row runs
# in uid_world, where the run reads the broker's UID, unless it says otherwise, and holds its exit and
# its whole set of FINDING and UNKNOWN lines. uid_N is the broker's name member, uid_U its member by
# UID, uid_U2 the member by UID of legion/worker (other_uid, which a row that names it plants), and
# uid_S the broker's name member in the subject form. ---
uid_N="$broker_wi_member"
uid_U="$(uid_member "$broker_uid")"
uid_U2="$(uid_member "$other_uid")"
uid_S="principal://iam.googleapis.com/projects/$number/locations/global/workloadIdentityPools/$project.svc.id.goog/subject/ns/secrets-broker/sa/secrets-broker"
uid_broker_step="live: minters of the broker account $broker"
uid_gsa_step="live: minters of $gsa"
uid_signing_step="live: hold writers: the signing key hold-signing-example"
uid_pair_note="the broker's ServiceAccount secrets-broker/secrets-broker is allowed as"
# The rights the broker, gsa and signing steps name for a member X on the broker account's workload
# binding (uid_b, uid_g X, uid_k X), and the gsa and signing steps for a member X holding
# roles/iam.serviceAccountTokenCreator on the operator's account (uid_t, uid_t2 X), as the check
# prints each for any member it finds there: a line is `FINDING X: <right>`.
uid_b() { printf '%s\n' "iam.serviceAccounts.getAccessToken, iam.serviceAccounts.getOpenIdToken on $broker_res through roles/iam.workloadIdentityUser, granted at $broker_res"; }
uid_g() {
  printf '%s\n' "iam.serviceAccounts.getAccessToken, iam.serviceAccounts.getOpenIdToken, iam.serviceAccounts.signJwt, iam.serviceAccounts.signBlob, iam.serviceAccounts.implicitDelegation on $gsa_res: $1 -> impersonates serviceAccount:$broker (roles/iam.workloadIdentityUser) -> roles/iam.serviceAccountTokenCreator, granted at $gsa_res"
}
uid_k() {
  printf '%s\n' "secretmanager.versions.access on $signing_res: $1 -> impersonates serviceAccount:$broker (roles/iam.workloadIdentityUser) -> impersonates serviceAccount:$gsa (roles/iam.serviceAccountTokenCreator) -> roles/secretmanager.secretAccessor, granted at $signing_res"
}
uid_t() {
  printf '%s\n' "iam.serviceAccounts.getAccessToken, iam.serviceAccounts.getOpenIdToken, iam.serviceAccounts.signJwt, iam.serviceAccounts.signBlob, iam.serviceAccounts.implicitDelegation on $gsa_res through roles/iam.serviceAccountTokenCreator, granted at $gsa_res"
}
uid_t2() {
  printf '%s\n' "secretmanager.versions.access on $signing_res: $1 -> impersonates serviceAccount:$gsa (roles/iam.serviceAccountTokenCreator) -> roles/secretmanager.secretAccessor, granted at $signing_res"
}
# uid_bgk X: the broker, gsa and signing steps' FINDING lines of X on the broker account's workload
# binding; uid_bgk_unknown X REASON: their UNKNOWN lines of X there when the run cannot compare it.
uid_bgk() { printf '%s\n' "FINDING $1: $(uid_b)" "FINDING $1: $(uid_g "$1")" "FINDING $1: $(uid_k "$1")"; }
uid_bgk_unknown() {
  uid_unknown "$uid_broker_step" "$(uid_b)" "$1" "$2"
  uid_unknown "$uid_gsa_step" "$(uid_g "$1")" "$1" "$2"
  uid_unknown "$uid_signing_step" "$(uid_k "$1")" "$1" "$2"
}
# The broker step's witness UNKNOWNs: neither of the broker's members listed, and the name member
# not listed when the member by UID is not known.
uid_neither="UNKNOWN $uid_broker_step: the analysis lists neither $uid_N nor $uid_U, one of which holds a permission on it by design, so it cannot be read as complete"
uid_not_n="UNKNOWN $uid_broker_step: the analysis does not list $uid_N, which holds a permission on it by design, so it cannot be read as complete"
# The reasons the run cannot compare a member by UID when the broker's UID read fails one way
# (uid_unread_reason ITEM, for (uh)'s items) or the pool project's number is not read.
uid_unread_reason() {
  local why="the UID of secrets-broker/secrets-broker is not read:"
  case "$1" in
    refused) printf '%s\n' "$uid_refused" ;;
    missing | emptylist) printf '%s\n' "$why cluster operator-cluster holds no ServiceAccount secrets-broker/secrets-broker" ;;
    garbage) printf '%s\n' "$why the API server's answer to the list of ServiceAccounts named secrets-broker in secrets-broker could not be read: " ;;
    empty) printf '%s\n' "$why the API server's answer to the list of ServiceAccounts named secrets-broker in secrets-broker could not be read: the answer was empty" ;;
    object) printf '%s\n' "$why the API server's answer to the list of ServiceAccounts named secrets-broker in secrets-broker could not be read: the answer is JSON, but not one object with a list of items" ;;
    default) printf '%s\n' "$why its metadata.uid on cluster operator-cluster is no UUID, as every Kubernetes UID is" ;;
  esac
}
uid_number_reason="the number of the pool project $project is not read: gcloud projects describe $project gave no number (it needs resourcemanager.projects.get)"
uid_signing_unknown="UNKNOWN $uid_signing_step: the project number, which the key's resource name holds, is unknown: gcloud projects describe $project was refused (it needs resourcemanager.projects.get): ERROR: (gcloud.projects.describe) PERMISSION_DENIED: Permission 'resourcemanager.projects.get' denied on resource '//cloudresourcemanager.googleapis.com/projects/$project' (or it may not exist)."
# uid_workload MEMBER...: the broker account's roles/iam.workloadIdentityUser binding holds MEMBERs
# alone, or no binding of that role when none is given.
uid_workload() {
  unbind "$broker_res" roles/iam.workloadIdentityUser
  [ "$#" -eq 0 ] || iam "$broker_res" roles/iam.workloadIdentityUser "$@"
}
# uid_pass STEP TEXT WHAT: the step STEP passed, its PASS line holding TEXT.
uid_pass() {
  awk -v p="PASS $1: " -v t="$2" 'index($0, p) == 1 && index($0, t) > 0 { found = 1 } END { exit !found }' <<<"$output" ||
    fail "$3: no line 'PASS $1: …' holding '$2'"
}
# uid_chains STEP: the count of impersonation chains STEP's PASS line names, or nothing.
uid_chains() {
  awk -v p="PASS $1: " 'index($0, p) == 1 && match($0, /, and [0-9]+ impersonation chains/) {
    s = substr($0, RSTART + 6, RLENGTH - 6); sub(/ .*/, "", s); print s }' <<<"$output"
}
# uid_lines_with MEMBER [TEXT]: the run's FINDING, UNKNOWN and ALLOWANCE lines that name MEMBER (and
# hold TEXT).
uid_lines_with() {
  awk -v m="$1" -v t="${2:-}" '/^(FINDING|UNKNOWN|ALLOWANCE) / && index($0, m) > 0 && (t == "" || index($0, t) > 0)' <<<"$output"
}
# uid_naming WHAT MEMBER LINE...: the run's lines that name MEMBER, of any kind, are exactly LINEs.
uid_naming() {
  local what="$1" member="$2" got
  shift 2
  got="$(grep -F -- "$member" <<<"$output" || true)"
  [ "$(LC_ALL=C sort <<<"$got")" = "$(printf '%s\n' "$@" | LC_ALL=C sort)" ] ||
    fail "$what: the lines naming $member are not the ones wanted, but: ${got//$'\n'/ | }"
}
# uid_parity WHAT KIND A B [TEXT]: each KIND line of the principal A (holding TEXT) has a twin, the
# same line with B in place of A wherever A is in it (a chain names its outermost principal twice),
# and each of B's the other way; A has at least one.
uid_parity() {
  local what="$1" kind="$2" a="$3" b="$4" text="${5:-}" line n=0
  while IFS= read -r line; do
    n=$((n + 1))
    has_line "${line//"$a"/"$b"}" || fail "$what: '$line' has no twin naming $b"
  done < <(awk -v p="$kind $a: " -v t="$text" 'index($0, p) == 1 && (t == "" || index($0, t) > 0)' <<<"$output")
  [ "$n" -gt 0 ] || fail "$what: no $kind line of $a${text:+ holding '$text'}"
  while IFS= read -r line; do
    has_line "${line//"$b"/"$a"}" || fail "$what: '$line' has no twin naming $a"
  done < <(awk -v p="$kind $b: " -v t="$text" 'index($0, p) == 1 && (t == "" || index($0, t) > 0)' <<<"$output")
}

# --- (ua) The name member alone, the broker's UID read: the PASS names the member by UID too. ---
if row ua; then
  uid_world
  run
  want_status 0 "the broker's name member alone, its UID read"
  want_lines "the broker's name member alone, its UID read" "FINDING UNKNOWN"
  uid_pass "$uid_broker_step" "$uid_pair_note $uid_N and, by its UID on cluster operator-cluster, as $uid_U" \
    "the broker's name member alone, its UID read"
  passed "(ua) the broker's name member alone exits 0, and the broker step's PASS names the member by UID it allows and the cluster whose UID it read"
fi

# --- (ub) Both of the broker's members on its account. ---
if row ub; then
  uid_world
  uid_workload "$uid_N" "$uid_U"
  run
  want_status 0 "the broker's name member and its member by UID"
  want_lines "the broker's name member and its member by UID" "FINDING UNKNOWN"
  for step in "$uid_broker_step" "$uid_gsa_step" "$uid_signing_step"; do
    want_text "PASS $step: " "the broker's name member and its member by UID"
  done
  passed "(ub) the broker's name member and its member by UID on the broker's account exit 0, the broker, gsa and signing steps each passing"
fi

# --- (uc) The member by UID alone: it is the witness. ---
if row uc; then
  uid_world
  uid_workload "$uid_U"
  run
  want_status 0 "the broker's member by UID alone"
  want_lines "the broker's member by UID alone" "FINDING UNKNOWN"
  for step in "$uid_broker_step" "$uid_gsa_step" "$uid_signing_step"; do
    want_text "PASS $step: " "the broker's member by UID alone"
  done
  passed "(uc) the broker's member by UID alone on the broker's account exits 0, the broker step taking it as its witness"
fi

# --- (ud) The chain through the member by UID is designed, and a finding once the member names
# another UID (the broker's planted as other_uid). ---
uid_chain_row() {
  local chains step
  uid_world
  uid_workload "$uid_U"
  case "$1" in
    designed)
      run
      want_status 0 "the chain through the broker's member by UID"
      want_lines "the chain through the broker's member by UID" "FINDING UNKNOWN"
      for step in "$uid_gsa_step" "$uid_signing_step"; do
        chains="$(uid_chains "$step")"
        [[ $chains =~ ^[0-9]+$ ]] && [ "$chains" -ge 1 ] ||
          fail "the chain through the broker's member by UID: the step $step did not pass counting an impersonation chain"
      done
      ;;
    control)
      sa_uid operator-cluster secrets-broker secrets-broker "$other_uid"
      run
      want_status 1 "the chain through a member by UID that names another UID"
      mapfile -t want < <(uid_bgk "$uid_U")
      want_lines "the chain through a member by UID that names another UID" "FINDING UNKNOWN" "${want[@]}" \
        "UNKNOWN $uid_broker_step: the analysis lists neither $uid_N nor $uid_U2, one of which holds a permission on it by design, so it cannot be read as complete"
      ;;
  esac
}
if row ud; then
  each uid_chain_row designed control
  passed "(ud) the gsa and signing steps pass, counting the chain through the broker's member by UID as designed, and print it as a finding once the broker's UID is another"
fi

# --- (ue) A member by another UID beside the name member, and its report. ---
if row ue; then
  uid_world
  sa_uid operator-cluster legion worker "$other_uid"
  uid_workload "$uid_N" "$uid_U2"
  run --report "$temporary_dir/report.json"
  want_status 1 "legion/worker's member by UID beside the broker's name member"
  mapfile -t want < <(uid_bgk "$uid_U2")
  want_lines "legion/worker's member by UID beside the broker's name member" "FINDING UNKNOWN" "${want[@]}"
  jq -e --arg p "$uid_U2" --arg broker_res "$broker_res" --arg gsa_res "$gsa_res" --arg signing_res "$signing_res" \
    --arg b "$(uid_b)" --arg g "$(uid_g "$uid_U2")" --arg k "$(uid_k "$uid_U2")" '
    .unknown == [] and (.findings | length) == 3
    and any(.findings[]; . == {principal: $p, right: $b, role: "roles/iam.workloadIdentityUser", resource: $broker_res,
      permissions: ["iam.serviceAccounts.getAccessToken", "iam.serviceAccounts.getOpenIdToken"], granted_at: $broker_res})
    and any(.findings[]; . == {principal: $p, right: $g, role: "roles/iam.workloadIdentityUser", resource: $gsa_res,
      permissions: ["iam.serviceAccounts.getAccessToken", "iam.serviceAccounts.getOpenIdToken", "iam.serviceAccounts.signJwt",
        "iam.serviceAccounts.signBlob", "iam.serviceAccounts.implicitDelegation"], granted_at: $broker_res})
    and any(.findings[]; . == {principal: $p, right: $k, role: "roles/iam.workloadIdentityUser", resource: $signing_res,
      permissions: ["secretmanager.versions.access"], granted_at: $broker_res})' "$temporary_dir/report.json" >/dev/null ||
    fail "legion/worker's member by UID: the report does not hold its three findings with their parts and an empty unknown: $(jq -c '{findings, unknown}' "$temporary_dir/report.json")"
  passed "(ue) a member by UID of another ServiceAccount beside the broker's name member is a finding on the broker, gsa and signing steps, each in the report with its parts"
fi

# --- (uf) A member by another UID alone: the same findings, and neither witness listed. ---
if row uf; then
  uid_world
  sa_uid operator-cluster legion worker "$other_uid"
  uid_workload "$uid_U2"
  run
  want_status 1 "legion/worker's member by UID alone on the broker's account"
  mapfile -t want < <(uid_bgk "$uid_U2")
  want_lines "legion/worker's member by UID alone on the broker's account" "FINDING UNKNOWN" "${want[@]}" "$uid_neither"
  passed "(uf) a member by UID of another ServiceAccount alone is the same three findings, and the broker step's UNKNOWN names both of the broker's members"
fi

# --- (ug) Token Creator on the operator's account: the broker's members in each form, one verdict. ---
if row ug; then
  uid_world
  iam "$gsa_res" roles/iam.serviceAccountTokenCreator "$uid_S" "$uid_N" "$uid_U"
  run
  want_status 1 "the broker's three members holding roles/iam.serviceAccountTokenCreator on the operator's account"
  want=()
  for member in "$uid_S" "$uid_N" "$uid_U"; do
    want+=("FINDING $member: $(uid_t)" "FINDING $member: $(uid_t2 "$member")")
  done
  want_lines "the broker's three members holding roles/iam.serviceAccountTokenCreator on the operator's account" "FINDING UNKNOWN" "${want[@]}"
  passed "(ug) the broker's subject-form name member, its name member and its member by UID holding roles/iam.serviceAccountTokenCreator on the operator's account are each the same finding"
fi

# --- (uh) The broker's UID unread, each way: its member by UID cannot be compared. ---
uid_unread_row() {
  local dir reason line
  if [ "$1" = default ]; then base_world; else uid_world; fi
  dir="$world/clusters/operator-cluster"
  uid_workload "$uid_N" "$uid_U"
  case "$1" in
    refused) : >"$dir/refuse-serviceaccount-read" ;;
    missing) edit "$dir/serviceaccounts.json" 'map(select(.namespace != "secrets-broker" or .name != "secrets-broker"))' ;;
    garbage | empty | object | emptylist) : >"$dir/$1-serviceaccount" ;;
  esac
  reason="$(uid_unread_reason "$1")"
  run
  want_status 2 "the broker's UID unread ($1)"
  if [ "$1" = garbage ]; then
    # The reason ends with jq's own error, which each jq words its own way.
    [ "$(count_prefix 'FINDING ')" -eq 0 ] && [ "$(count_prefix 'UNKNOWN ')" -eq 3 ] ||
      fail "the broker's UID unread ($1): not three UNKNOWN lines and no FINDING"
    while IFS= read -r line; do
      awk -v p="$line" 'index($0, p) == 1 { found = 1 } END { exit !found }' <<<"$output" ||
        fail "the broker's UID unread ($1): no line starting '$line'"
    done < <(uid_bgk_unknown "$uid_U" "$reason")
  else
    mapfile -t want < <(uid_bgk_unknown "$uid_U" "$reason")
    want_lines "the broker's UID unread ($1)" "FINDING UNKNOWN" "${want[@]}"
  fi
  ! has_prefix "PASS $uid_broker_step: " || fail "the broker's UID unread ($1): the broker step passed"
}
if row uh; then
  each uid_unread_row refused missing garbage empty object emptylist default
  passed "(uh) with the broker's UID unread (refused, missing, an answer that is no list, a UID that is no UUID), its member by UID reads UNKNOWN on the broker, gsa and signing steps, naming why, and exits 2"
fi

# --- (ui) The member by UID alone, its UID unread: the name member is the witness. ---
if row ui; then
  uid_world
  uid_workload "$uid_U"
  : >"$world/clusters/operator-cluster/refuse-serviceaccount-read"
  run
  want_status 2 "the broker's member by UID alone, its UID unread"
  mapfile -t want < <(uid_bgk_unknown "$uid_U" "$uid_refused")
  want_lines "the broker's member by UID alone, its UID unread" "FINDING UNKNOWN" "${want[@]}" "$uid_not_n"
  passed "(ui) the broker's member by UID alone, its UID unread, reads UNKNOWN on the broker, gsa and signing steps and for the name member as the witness"
fi

# --- (uj) The pool project's number unread: the member by UID cannot be compared, and one of another
# UID still can. GKE's service agent is unbound, since its project's number is unread too. ---
uid_number_row() {
  uid_world
  : >"$world/refuse-project-describe"
  unbind "$project_res" roles/container.serviceAgent
  case "$1" in
    broker)
      uid_workload "$uid_N" "$uid_U"
      run
      want_status 2 "the pool project's number unread"
      want_lines "the pool project's number unread" "FINDING UNKNOWN" "$uid_signing_unknown" \
        "$(uid_unknown "$uid_broker_step" "$(uid_b)" "$uid_U" "$uid_number_reason")" \
        "$(uid_unknown "$uid_gsa_step" "$(uid_g "$uid_U")" "$uid_U" "$uid_number_reason")"
      ;;
    other)
      sa_uid operator-cluster legion worker "$other_uid"
      uid_workload "$uid_N" "$uid_U2"
      run
      want_status 1 "the pool project's number unread, a member by another UID"
      want_lines "the pool project's number unread, a member by another UID" "FINDING UNKNOWN" "$uid_signing_unknown" \
        "FINDING $uid_U2: $(uid_b)" "FINDING $uid_U2: $(uid_g "$uid_U2")"
      ;;
  esac
}
if row uj; then
  each uid_number_row broker other
  passed "(uj) with the pool project's number unread, the broker's member by UID reads UNKNOWN naming it, and a member by another UID stays a finding"
fi

# --- (uk) Neither of the broker's members on its account. ---
if row uk; then
  uid_world
  uid_workload
  run
  want_status 2 "neither of the broker's members on its account"
  want_lines "neither of the broker's members on its account" "FINDING UNKNOWN" "$uid_neither"
  passed "(uk) neither of the broker's members on the broker's account reads UNKNOWN naming both"
fi

# --- (ul) A member by the broker's UID in another pool, the UID unread: it differs in what was read. ---
if row ul; then
  uid_world
  : >"$world/clusters/operator-cluster/refuse-serviceaccount-read"
  member="$(uid_member "$broker_uid" other-project.svc.id.goog)"
  uid_workload "$uid_N" "$member"
  run
  want_status 1 "a member by the broker's UID in another pool"
  mapfile -t want < <(uid_bgk "$member")
  want_lines "a member by the broker's UID in another pool" "FINDING UNKNOWN" "${want[@]}"
  passed "(ul) a member by the broker's UID in another pool, the UID unread, is a finding on the broker, gsa and signing steps"
fi

# --- (uo) The UID is read on --cluster's cluster alone: another cluster of the pool holds the broker's
# pair under another UID. ---
if row uo; then
  uid_world
  uid_workload "$uid_N" "$uid_U"
  add_cluster other-cluster us-east1
  add_sa other-cluster secrets-broker secrets-broker
  sa_uid other-cluster secrets-broker secrets-broker "$other_uid"
  run
  want_status 1 "the broker's pair on another cluster under another UID"
  want_lines "the broker's pair on another cluster under another UID" "FINDING UNKNOWN" \
    "FINDING cluster:$project/us-east1/other-cluster: namespace secrets-broker exists there, so its ServiceAccount secrets-broker/secrets-broker acts as the broker account $broker through the workload pool $project.svc.id.goog"
  uid_pass "$uid_broker_step" "$uid_pair_note $uid_N and, by its UID on cluster operator-cluster, as $uid_U" \
    "the broker's pair on another cluster under another UID"
  passed "(uo) the broker's UID is read on --cluster's cluster alone: its pair on another cluster under another UID is that cluster's finding, and its member by UID is allowed"
fi

# --- (ut) The member by UID names the pool's project and number, not --project's: the run with
# --pool-project example-pool (number 333333333333, no cluster of its own), the operator's and the
# broker's accounts bound to the pool's name members, and relay@ (a User) able to create pods in
# envoy-operator, so its minters are read too. The pool's other steps read UNKNOWN (its fleet is not
# listed, as row r shows), so each item holds the lines that name its member, the broker step's and,
# for the follow-* items, the follow's. ---
uid_np="serviceAccount:example-pool.svc.id.goog[secrets-broker/secrets-broker]"
uid_pool_row() {
  local member become up
  up="$(uid_member "$broker_uid" example-pool.svc.id.goog 333333333333)"
  uid_world
  mkdir -p "$world/numbers" "$world/pool-clusters"
  echo 333333333333 >"$world/numbers/example-pool"
  echo '[]' >"$world/pool-clusters/example-pool.json"
  unbind "$gsa_res" roles/iam.workloadIdentityUser
  iam "$gsa_res" roles/iam.workloadIdentityUser "serviceAccount:example-pool.svc.id.goog[envoy-operator/envoy-operator]"
  subject_grant operator-cluster "$(user_subject "$relay")" create "" pods "" envoy-operator ""
  case "$1" in
    pool) member="$up" ;;
    project-number) member="$(uid_member "$broker_uid" example-pool.svc.id.goog "$number")" ;;
    project-pool) member="$uid_U" ;;
    # The follow-* items: ci/runner (its UID planted), which legion/worker may create pods
    # to become, named by UID on relay@'s account by the pool's number and by --project's.
    follow-pool) member="$(uid_member "$ci_uid" example-pool.svc.id.goog 333333333333)" ;;
    follow-project) member="$(uid_member "$ci_uid" example-pool.svc.id.goog "$number")" ;;
  esac
  case "$1" in
    follow-*)
      add_sa operator-cluster ci runner
      sa_uid operator-cluster ci runner "$ci_uid"
      subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" ci ""
      iam "$relay_res" roles/iam.workloadIdentityUser "$member"
      uid_workload "$uid_np"
      ;;
    *) uid_workload "$uid_np" "$member" ;;
  esac
  run --pool-project example-pool
  want_status 1 "the pool's member by UID ($1)"
  case "$1" in
    pool)
      uid_pass "$uid_broker_step" "$uid_pair_note $uid_np and, by its UID on cluster operator-cluster, as $member" "the pool's member by UID"
      [ -z "$(uid_lines_with "$member")" ] || fail "the pool's member by UID: a line names it: $(uid_lines_with "$member")"
      ;;
    project-*)
      mapfile -t want < <(uid_bgk "$member")
      uid_naming "a member by UID of --project's ${1#project-}" "$member" "${want[@]}"
      ! has_prefix "PASS $uid_broker_step: " || fail "a member by UID of --project's ${1#project-}: the broker step passed"
      ;;
    follow-*)
      uid_pass "$uid_broker_step" "$uid_pair_note $uid_np and, by its UID on cluster operator-cluster, as $up" "ci/runner's member by UID on relay@'s account ($1)"
      uid_naming "ci/runner's member by UID on relay@'s account ($1)" "$member" \
        "FINDING $member: iam.serviceAccounts.getAccessToken, iam.serviceAccounts.getOpenIdToken on $relay_res through roles/iam.workloadIdentityUser, granted at $relay_res"
      # The follow's lines, which become ci/runner and so act as relay@: legion/worker's by the
      # pool's number, none by --project's.
      become=""
      [ "$1" != follow-pool ] ||
        become="FINDING system:serviceaccount:legion:worker: create pods in ci, so it can become system:serviceaccount:ci:runner, which can act as $relay through Workload Identity"
      [ "$(uid_lines_with ", so it can become system:serviceaccount:ci:runner, which can act as $relay through Workload Identity")" = "$become" ] ||
        fail "ci/runner's member by UID on relay@'s account ($1): the lines that follow it are not '${become:-none}', but: $(uid_lines_with ", so it can become system:serviceaccount:ci:runner, which can act as $relay")"
      ;;
  esac
}
if row ut; then
  each uid_pool_row pool project-number project-pool follow-pool follow-project
  passed "(ut) with --pool-project example-pool, the broker's member by UID of the pool's project and number passes, named in the broker step's PASS, and one of --project's number or pool is a finding; a member by UID of ci/runner is followed by the pool's number and not by --project's"
fi

# --- (um) The inventory of every IAM check: the member by the broker's UID is judged as the name
# member on each. The class is gke, dm, gsa, broker, sql, dbsecret, state, signing, fleet, a fleet
# member's project (gke:<project>), an RBAC finding's account (rbac-account:<email>), a credited
# grant's account (residual-account:<email>), each ancestor (ancestor:<folder or organization>) and
# code: IAM. uid_checks lists each live one but residual-account as a holder of roles/owner on the
# organization is named on it, CHECK|STEP|the start of its right|the end of it, where STEP is the step
# of a check every run makes (empty for one that a world must make run) and a start that ends ", "
# leaves out the permissions after it. The items: direct (uid_N and uid_U hold roles/owner on the
# organization, which every check's analysis sees), chain (the broker account holds it, uid_U beside
# uid_N on its workload binding), residual (both hold Token Creator on comment-delivery's account in
# the broker's pod fence's world), completeness (the steps a clean run passes are the checks listed)
# and code (a preview gives both roles/owner on the project). ---
uid_minter_perms="iam.serviceAccounts.getAccessToken, iam.serviceAccounts.getOpenIdToken, iam.serviceAccounts.signJwt, iam.serviceAccounts.signBlob, iam.serviceAccounts.actAs, iam.serviceAccounts.implicitDelegation, iam.serviceAccountKeys.create, iam.serviceAccounts.setIamPolicy"
uid_owner_tail=" through roles/owner, granted at $org_res"
uid_checks=(
  "gke|live: GKE IAM on project $project|container.serviceAccounts.createToken, container.clusters.impersonate, | on $project_res$uid_owner_tail"
  "dm|live: Deployment Manager on project $project|deploymentmanager.deployments.create, deploymentmanager.deployments.update| on $project_res$uid_owner_tail, by which Deployment Manager creates a deployment's resources as the project's Google APIs service agent, which holds roles/editor there"
  "gsa|live: minters of $gsa|$uid_minter_perms| on $gsa_res$uid_owner_tail"
  "broker|$uid_broker_step|$uid_minter_perms| on $broker_res$uid_owner_tail"
  "sql|live: hold writers: Cloud SQL users of example-instance|cloudsql.users.create, cloudsql.users.update| on $sql_res$uid_owner_tail"
  "dbsecret|live: hold writers: Secret example-db/broker-db-login|container.secrets.get| on $project_res$uid_owner_tail"
  "state|live: hold writers: the broker program's state in gs://example-state|storage.objects.get| on $bucket_res$uid_owner_tail"
  "signing|$uid_signing_step|secretmanager.versions.access, secretmanager.secrets.setIamPolicy| on $signing_res$uid_owner_tail"
  "fleet|live: who can register a cluster to, or write namespaces and RBAC on, the fleet of project $project|gkehub.memberships.create, gkehub.memberships.update| on $project_res$uid_owner_tail"
  "ancestor:organizations/$org|live: who can set IAM on organizations/$org|resourcemanager.organizations.setIamPolicy, iam.roles.update| on $org_res$uid_owner_tail"
  "gke:example-grade||container.namespaces.create, container.namespaces.update, | on $grade_res$uid_owner_tail"
  "rbac-account:$grade_ci||$uid_minter_perms| on $grade_ci_res$uid_owner_tail"
)
# uid_owner_lines WHAT PRINCIPAL [SKIP]: PRINCIPAL has a FINDING on each check uid_checks lists but
# SKIP, found by the start and the end of its right.
uid_owner_lines() {
  local entry name start end
  for entry in "${uid_checks[@]}"; do
    IFS='|' read -r name _ start end <<<"$entry"
    [ "$name" != "${3:-}" ] || continue
    case "$start" in
      *", ") awk -v p="FINDING $2: $start" -v s="$end" 'index($0, p) == 1 && length($0) >= length(p) + length(s) &&
        substr($0, length($0) - length(s) + 1) == s { found = 1 } END { exit !found }' <<<"$output" ;;
      *) has_line "FINDING $2: $start$end" ;;
    esac || fail "$1: $2 has no FINDING on the check $name ('$start…$end')"
  done
}
uid_inventory_row() {
  local entry step line known listed=()
  case "$1" in
    direct)
      uid_world
      grade_member
      cluster_admin grade-cluster "$(user_subject "$grade_ci")"
      iam "$org_res" roles/owner "$uid_N" "$uid_U"
      run
      uid_parity "the broker's members holding roles/owner on the organization" FINDING "$uid_N" "$uid_U"
      [ -z "$(uid_lines_with "$uid_N" " on $broker_res")$(uid_lines_with "$uid_U" " on $broker_res")" ] ||
        fail "the broker's members holding roles/owner on the organization: a line on the broker's account names one: $(uid_lines_with "$uid_U" " on $broker_res")"
      uid_owner_lines "the broker's members holding roles/owner on the organization" "$uid_N" broker
      ;;
    chain)
      uid_world
      grade_member
      cluster_admin grade-cluster "$(user_subject "$grade_ci")"
      uid_workload "$uid_N" "$uid_U"
      iam "$org_res" roles/owner "serviceAccount:$broker"
      run
      [ -z "$(uid_lines_with "$uid_N")$(uid_lines_with "$uid_U")" ] ||
        fail "the broker account holding roles/owner on the organization: a line names one of its members: $(uid_lines_with "$uid_N")$(uid_lines_with "$uid_U")"
      uid_owner_lines "the broker account holding roles/owner on the organization" "serviceAccount:$broker"
      ;;
    residual)
      fence_world
      sa_uid operator-cluster dispatch envoy-broker "$broker_uid"
      iam "$comment_delivery_res" roles/iam.serviceAccountTokenCreator "$fence_wi_member" "$uid_U"
      fence_run
      ! has_prefix "UNKNOWN run: " || fail "the broker's members minting comment-delivery's token: the run stopped"
      uid_parity "the broker's members minting comment-delivery's token" ALLOWANCE "$fence_wi_member" "$uid_U" \
        " on $comment_delivery_res through roles/iam.serviceAccountTokenCreator, granted at $comment_delivery_res, so it can act as serviceAccount:$comment_delivery, "
      ;;
    completeness)
      uid_world
      run
      for entry in "${uid_checks[@]}"; do
        IFS='|' read -r _ step _ _ <<<"$entry"
        [ -z "$step" ] || listed+=("$step")
      done
      while IFS= read -r line; do
        known=false
        for step in "${listed[@]}"; do
          [[ "$line" != "PASS $step: "* ]] || known=true
        done
        [ "$known" = true ] || fail "a clean run's IAM checks: '$line' is the PASS of a check (um) does not list"
      done < <(grep -E '^PASS live: (GKE IAM|Deployment Manager|minters of|hold writers|who can register|who can set IAM on) ' <<<"$output" || true)
      for step in "${listed[@]}"; do
        want_text "PASS $step: " "a clean run's IAM checks"
      done
      ;;
    code)
      uid_world
      refuse_live
      broker_member_step broker-by-uid "$uid_U"
      preview_step owners gcp:projects/iAMBinding:IAMBinding \
        "$(jq -nc --arg p "$project" --arg n "$uid_N" --arg u "$uid_U" '{project: $p, role: "roles/owner", members: [$n, $u]}')"
      run
      uid_parity "the broker's members given roles/owner by the code" FINDING "$uid_N" "$uid_U"
      [ -z "$(uid_lines_with "$uid_N" " on $broker_res")$(uid_lines_with "$uid_U" " on $broker_res")" ] ||
        fail "the broker's members given roles/owner by the code: a line on the broker's account names one: $(uid_lines_with "$uid_U" " on $broker_res")"
      ;;
  esac
}
if row um; then
  each uid_inventory_row direct chain residual completeness code
  passed "(um) on every IAM check the broker's member by UID is judged as its name member: twin findings as holders, designed chains through the broker, a twin ALLOWANCE on a credited grant's account, twin code findings, and a clean run passes the checks listed"
fi

# --- (un), (va) and (vc) to (vf): the follow of a ServiceAccount of the pool named by its UID. Each
# item runs in uid_follow_world's world (row cd's world with ci/runner's UID, ci_uid, planted on the
# operator's cluster) or, for (un), in that world with the broker's ServiceAccount in ci/runner's
# place, with a member M holding roles/iam.workloadIdentityUser on grade-ci's account, an RBAC
# finding's. uid_follow_run keeps each item's run; the row then holds each item, and an item by UID
# against its control, the same world with a member by name as M (uid_twin). ---
uid_follow_world() {
  base_world
  grade_member
  cluster_admin grade-cluster "$(user_subject "$grade_ci")"
  add_sa operator-cluster ci runner
  sa_uid operator-cluster ci runner "$ci_uid"
  subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" ci ""
}
uid_ci_name="serviceAccount:$project.svc.id.goog[ci/runner]"
uid_grade_cluster="example-grade/us-east1/grade-cluster"
uid_other_cluster="$project/us-east1/other-cluster"
# uid_become NAMESPACE:NAME [CLUSTER]: how a line of the follow ends that becomes that ServiceAccount,
# and so acts as grade-ci, on CLUSTER (as a finding names a cluster; the operator's when empty, whose
# lines name none).
uid_become() { printf '%s\n' ", so it can become system:serviceaccount:$1, which can act as $grade_ci through Workload Identity${2:+ on cluster $2}"; }
# The name member's follow lines in uid_follow_world's world, as row cd prints them: legion/worker's
# on the operator's cluster (uid_wf) and, once other-cluster holds ci/runner, apps/deployer's there
# (uid_df).
uid_wf="FINDING system:serviceaccount:legion:worker: create pods in ci$(uid_become ci:runner)"
uid_df="FINDING system:serviceaccount:apps:deployer: create pods in ci$(uid_become ci:runner "$uid_other_cluster")"
# uid_follow_unknown MEMBER REASON: the UNKNOWN grade-ci's minters step prints for MEMBER, a member by
# UID whose ServiceAccount the run cannot tell, so who can become it is not followed.
uid_follow_unknown() {
  printf '%s\n' "UNKNOWN live: minters of $grade_ci, which an RBAC finding names: $1 can act as $grade_ci through Workload Identity, and who can become the ServiceAccount it names is not followed: $2"
}
# uid_follow_member ITEM: the member M of an item of uid_follow_run.
uid_follow_member() {
  case "$1" in
    va-name | vc-name | vd-name | ve-name) printf '%s\n' "$uid_ci_name" ;;
    va-uid | vc-uid | ve-uid) uid_member "$ci_uid" ;;
    vc-uid2) uid_member "$ci_uid2" ;;
    vd-uid) uid_member "$unlisted_uid" ;;
    vf-pool) uid_member "$ci_uid" other-project.svc.id.goog ;;
    vf-number) uid_member "$ci_uid" "" 999999999999 ;;
    vf-name) printf '%s\n' "serviceAccount:other-project.svc.id.goog[ci/runner]" ;;
    un-name) printf '%s\n' "$uid_N" ;;
    un-uid) printf '%s\n' "$uid_U" ;;
  esac
}
# uid_follow_run ITEM: the run of ITEM, <row>-<its member>, in its world, kept for uid_follow_load in
# $temporary_dir/uid-follow-ITEM.out and .status. (vc) adds other-cluster, whose ci/runner holds
# ci_uid2 and which apps/deployer may create pods in ci on; (ve) refuses the project's number, and
# unbinds GKE's service agent, whose project number is then unread too.
uid_follow_run() {
  case "$1" in
    un-*)
      uid_world
      grade_member
      cluster_admin grade-cluster "$(user_subject "$grade_ci")"
      subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" secrets-broker ""
      ;;
    *) uid_follow_world ;;
  esac
  case "$1" in
    vc-*)
      add_cluster other-cluster us-east1
      add_sa other-cluster ci runner
      sa_uid other-cluster ci runner "$ci_uid2"
      subject_grant other-cluster "$(sa_subject apps deployer)" create "" pods "" ci ""
      ;;
    ve-*)
      : >"$world/refuse-project-describe"
      unbind "$project_res" roles/container.serviceAgent
      ;;
  esac
  iam "$grade_ci_res" roles/iam.workloadIdentityUser "$(uid_follow_member "$1")"
  run
  printf '%s\n' "$status" >"$temporary_dir/uid-follow-$1.status"
  printf '%s\n' "$output" >"$temporary_dir/uid-follow-$1.out"
}
uid_follow_load() { status="$(<"$temporary_dir/uid-follow-$1.status")" output="$(<"$temporary_dir/uid-follow-$1.out")"; }
# uid_ending END: the run's FINDING, UNKNOWN and ALLOWANCE lines that end with END.
uid_ending() {
  awk -v e="$1" '/^(FINDING|UNKNOWN|ALLOWANCE) / && length($0) >= length(e) && substr($0, length($0) - length(e) + 1) == e' <<<"$output"
}
# uid_twin CONTROL A B [END...]: sets UID_TWIN to the FINDING, UNKNOWN and ALLOWANCE lines of the
# item CONTROL's run with A replaced by B wherever it is in them, less each line that ends with an END,
# which an item by UID wants beside what its row adds; fails when CONTROL printed no line ending with
# an END, so each END leaves out lines that are there.
uid_twin() {
  local control="$1" a="$2" b="$3" line end
  local -A ended=()
  shift 3
  UID_TWIN=()
  while IFS= read -r line; do
    for end in "$@"; do
      if [[ "$line" == *"$end" ]]; then
        ended[$end]=1
        continue 2
      fi
    done
    UID_TWIN+=("${line//"$a"/"$b"}")
  done < <(grep -E '^(FINDING|UNKNOWN|ALLOWANCE) ' "$temporary_dir/uid-follow-$control.out" || true)
  for end in "$@"; do
    [ -n "${ended[$end]:-}" ] || fail "the control $control printed no line ending '$end'"
  done
}

# --- (un) The broker's ServiceAccount named by its UID on grade-ci's account is followed on the
# operator's cluster, which holds its UID, as its name member is: legion/worker, which may create pods
# in secrets-broker, can become it. The name member's run holds more lines, since a name member is
# followed on every remembered cluster: those that become the broker's ServiceAccount on
# grade-cluster, where grade-ci is cluster-admin, which the member by UID's leaves out alone. ---
uid_un_wf="FINDING system:serviceaccount:legion:worker: create pods in secrets-broker$(uid_become secrets-broker:secrets-broker)"
if row un; then
  each uid_follow_run un-name un-uid
  for item in un-name un-uid; do
    uid_follow_load "$item"
    want_status 1 "the broker's ${item#un-} member on grade-ci's account"
    [ "$(uid_ending "$(uid_become secrets-broker:secrets-broker)")" = "$uid_un_wf" ] ||
      fail "the broker's ${item#un-} member on grade-ci's account: its follow lines on the operator's cluster are not '$uid_un_wf', but: $(uid_ending "$(uid_become secrets-broker:secrets-broker)")"
  done
  uid_follow_load un-name
  want_lines "the broker's name member on grade-ci's account" UNKNOWN
  uid_follow_load un-uid
  uid_twin un-name "$uid_N" "$uid_U" "$(uid_become secrets-broker:secrets-broker "$uid_grade_cluster")"
  want_lines "the broker's member by UID on grade-ci's account" "FINDING UNKNOWN ALLOWANCE" "${UID_TWIN[@]}"
  passed "(un) the broker's member by UID on grade-ci's account is followed as its name member is, on the operator's cluster, which holds its UID: legion/worker can become the broker's ServiceAccount and so act as grade-ci"
fi

# --- (va) ci/runner named by its UID on grade-ci's account is followed as its name member is, on the
# operator's cluster, which holds the UID. The name member's run holds more lines, since a name member
# is followed on every remembered cluster: those that become ci/runner on grade-cluster, where
# grade-ci is cluster-admin, which the member by UID's leaves out alone. ---
if row va; then
  each uid_follow_run va-name va-uid
  uid_follow_load va-name
  want_status 1 "ci/runner's name member on grade-ci's account"
  want_line "$uid_wf" "ci/runner's name member on grade-ci's account"
  want_lines "ci/runner's name member on grade-ci's account" UNKNOWN
  uid_follow_load va-uid
  want_status 1 "ci/runner's member by UID on grade-ci's account"
  want_line "$uid_wf" "ci/runner's member by UID on grade-ci's account"
  uid_twin va-name "$uid_ci_name" "$(uid_follow_member va-uid)" "$(uid_become ci:runner "$uid_grade_cluster")"
  want_lines "ci/runner's member by UID on grade-ci's account" "FINDING UNKNOWN ALLOWANCE" "${UID_TWIN[@]}"
  passed "(va) ci/runner's member by UID on grade-ci's account is followed as its name member is, on the cluster that holds its UID: legion/worker can become ci/runner and so act as grade-ci"
fi

# --- (vc) ci/runner on two clusters under two UIDs, ci_uid on the operator's and ci_uid2 on
# other-cluster: each member by UID is followed on the cluster that holds its UID alone. The name
# member's run holds more lines, since a name member is followed on every remembered cluster: those
# that become ci/runner on the clusters that do not hold the UID (grade-cluster's, and the other
# cluster's of the two, whose lines on the operator's cluster name no cluster), which each member by
# UID's leaves out alone. ---
if row vc; then
  each uid_follow_run vc-uid2 vc-uid vc-name
  uid_follow_load vc-name
  uid_what="ci/runner's name member, ci/runner on two clusters"
  want_status 1 "$uid_what"
  want_line "$uid_wf" "$uid_what"
  want_line "$uid_df" "$uid_what"
  want_lines "$uid_what" UNKNOWN
  uid_follow_load vc-uid2
  uid_what="ci/runner's member by other-cluster's UID"
  want_status 1 "$uid_what"
  want_line "$uid_df" "$uid_what"
  ! has_line "$uid_wf" || fail "$uid_what: it is followed on the operator's cluster"
  uid_twin vc-name "$uid_ci_name" "$(uid_follow_member vc-uid2)" "$(uid_become ci:runner)" "$(uid_become ci:runner "$uid_grade_cluster")"
  want_lines "$uid_what" "FINDING UNKNOWN ALLOWANCE" "${UID_TWIN[@]}"
  uid_follow_load vc-uid
  uid_what="ci/runner's member by the operator's cluster's UID"
  want_status 1 "$uid_what"
  want_line "$uid_wf" "$uid_what"
  ! has_line "$uid_df" || fail "$uid_what: it is followed on other-cluster"
  uid_twin vc-name "$uid_ci_name" "$(uid_follow_member vc-uid)" "$(uid_become ci:runner "$uid_other_cluster")" "$(uid_become ci:runner "$uid_grade_cluster")"
  want_lines "$uid_what" "FINDING UNKNOWN ALLOWANCE" "${UID_TWIN[@]}"
  passed "(vc) ci/runner on two clusters under two UIDs: each member by UID is followed on the cluster that holds its UID alone, the name member on both"
fi

# --- (vd) A member by a UID that no ServiceAccount the run listed holds: UNKNOWN on grade-ci's minters
# step naming the member and the UID, and nothing followed. Its control is ci/runner's name member:
# the item by UID holds the control's lines with M swapped, less every line that becomes ci/runner
# (on the operator's cluster and on grade-cluster), and the UNKNOWN. ---
if row vd; then
  each uid_follow_run vd-name vd-uid
  uid_follow_load vd-name
  want_status 1 "ci/runner's name member on grade-ci's account"
  want_line "$uid_wf" "ci/runner's name member on grade-ci's account"
  want_lines "ci/runner's name member on grade-ci's account" UNKNOWN
  uid_follow_load vd-uid
  want_status 1 "a member by a UID no listed ServiceAccount holds"
  ! has_line "$uid_wf" || fail "a member by a UID no listed ServiceAccount holds: ci/runner is followed"
  uid_twin vd-name "$uid_ci_name" "$(uid_follow_member vd-uid)" "$(uid_become ci:runner)" "$(uid_become ci:runner "$uid_grade_cluster")"
  want_lines "a member by a UID no listed ServiceAccount holds" "FINDING UNKNOWN ALLOWANCE" "${UID_TWIN[@]}" \
    "$(uid_follow_unknown "$(uid_follow_member vd-uid)" "no ServiceAccount that the run listed on the clusters of the pool holds UID $unlisted_uid")"
  passed "(vd) a member by a UID no listed ServiceAccount holds reads UNKNOWN on grade-ci's minters step, naming it and the UID, and nothing is followed from it"
fi

# --- (ve) The pool project's number unread: ci/runner's member by UID reads UNKNOWN naming that, and
# nothing is followed; beside it the signing key's own UNKNOWN. Its control is the name member in the
# same world, as in (vd). ---
if row ve; then
  each uid_follow_run ve-name ve-uid
  uid_follow_load ve-name
  want_status 1 "ci/runner's name member, the pool project's number unread"
  want_line "$uid_wf" "ci/runner's name member, the pool project's number unread"
  want_lines "ci/runner's name member, the pool project's number unread" UNKNOWN "$uid_signing_unknown"
  uid_follow_load ve-uid
  want_status 1 "ci/runner's member by UID, the pool project's number unread"
  ! has_line "$uid_wf" || fail "ci/runner's member by UID, the pool project's number unread: ci/runner is followed"
  uid_twin ve-name "$uid_ci_name" "$(uid_follow_member ve-uid)" "$(uid_become ci:runner)" "$(uid_become ci:runner "$uid_grade_cluster")"
  want_lines "ci/runner's member by UID, the pool project's number unread" "FINDING UNKNOWN ALLOWANCE" "${UID_TWIN[@]}" \
    "$(uid_follow_unknown "$(uid_follow_member ve-uid)" "$uid_number_reason")"
  passed "(ve) with the pool project's number unread, ci/runner's member by UID reads UNKNOWN naming that, and nothing is followed from it"
fi

# --- (vf) A member by UID of another pool, or of another project number, is not followed, as another
# pool's name member (the control) is not: the three runs' lines are twins. ---
if row vf; then
  each uid_follow_run vf-pool vf-number vf-name
  uid_follow_load vf-name
  want_status 1 "another pool's name member of ci/runner"
  ! has_prefix "$(uid_become ci:runner)" || fail "another pool's name member of ci/runner: ci/runner is followed"
  want_lines "another pool's name member of ci/runner" UNKNOWN
  for item in vf-pool vf-number; do
    uid_follow_load "$item"
    want_status 1 "a member by UID of another ${item#vf-}"
    ! has_line "$uid_wf" || fail "a member by UID of another ${item#vf-}: ci/runner is followed"
    uid_twin vf-name "$(uid_follow_member vf-name)" "$(uid_follow_member "$item")"
    want_lines "a member by UID of another ${item#vf-}" "FINDING UNKNOWN ALLOWANCE" "${UID_TWIN[@]}"
  done
  passed "(vf) a member by UID of another pool, or of another project number, is not followed, as another pool's name member is not"
fi
