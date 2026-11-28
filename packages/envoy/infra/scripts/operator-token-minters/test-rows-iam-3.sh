# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-iam-3.sh: the rows of operator-token-minters.test.sh on Google
# IAM: legacy ABAC and token Secrets made through IAM, Google's service agents, a fleet member's
# Workload Identity member, and Deployment Manager. Sourced by the test after its world and helpers;
# its header says what the fakes model. The rows:
#
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
