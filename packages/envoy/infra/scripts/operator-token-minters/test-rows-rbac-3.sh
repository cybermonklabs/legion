# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-rbac-3.sh: the rows of operator-token-minters.test.sh on the
# Users and Groups a binding names, on any cluster of the pool: a User that is no email, Kubernetes'
# own groups, and any other Group. Sourced by the test after its world and helpers; its header says
# what the fakes model. The rows:
#
#   (aw) a User that is no email is reviewed: deployer-bot bound to cluster-admin on a non-GKE fleet
#        member, a principal:// User allowed to create pods in envoy-operator, and a render binding
#        deployer-bot to cluster-admin each exit 1 naming it;
#   (ax) Kubernetes' own groups are findings: system:authenticated or system:serviceaccounts:apps
#        allowed to create pods in envoy-operator on another cluster, system:serviceaccounts on the
#        operator's and a live binding of system:masters other than the bootstrap one (those two in
#        one world), and a render binding system:masters to cluster-admin each exit 1;
#   (ay) any other Group reads UNKNOWN where the cluster takes its groups from elsewhere: the Group
#        ci-deployers bound to cluster-admin on a non-GKE fleet member, a Group on another cluster
#        or a GKE fleet member with Google Groups for RBAC on, a GKE fleet member whose describe is
#        refused, and the operator's cluster with Google Groups for RBAC on and no Group bound.

# --- (aw) A User that is no email is reviewed, not taken for a person. ---
principal_user="principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/ci/subject/runner"
user_row() {
  local want
  base_world
  case "$1" in
    member)
      edge_member
      cluster_admin edge-cluster "$(user_subject deployer-bot)"
      want="FINDING user:deployer-bot: create namespaces cluster-wide on fleet member $project/global/edge"
      ;;
    principal)
      subject_grant operator-cluster "$(user_subject "$principal_user")" create "" pods "" envoy-operator ""
      want="FINDING user:$principal_user: create pods in envoy-operator"
      ;;
    code)
      refuse_live
      edit "$world/render.json" '.items += [{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding",
        metadata: {name: "bot", namespace: "envoy-operator"},
        roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "cluster-admin"},
        subjects: [{apiGroup: "rbac.authorization.k8s.io", kind: "User", name: "deployer-bot"}]}]'
      want="FINDING user:deployer-bot: create serviceaccounts/token envoy-operator in envoy-operator, declared by RoleBinding envoy-operator/bot"
      ;;
  esac
  run
  want_status 1 "a User that is no email ($1)"
  want_line "$want" "a User that is no email ($1)"
}
if row aw; then
  each user_row member principal code
  passed "(aw) a User that is no email (deployer-bot on a non-GKE fleet member, a principal:// name, a render's deployer-bot) is reviewed and exits 1 naming it"
fi

# --- (ax) Kubernetes' own groups, whatever Google Groups for RBAC says. On the operator's cluster,
# system:serviceaccounts and a live binding of system:masters share one world and one run
# (own_group_shared): each is a principal of its own, a Group reviewed with its own group alone, and
# the system:masters line is read from the bindings, not from a review, so each is still judged by
# its own line, exact; want_recorded holds the run's last line to the lines it printed. The other
# cluster's two groups (each adds that cluster) and the render's system:masters binding (refuse_live
# refuses the operator's reviews) keep a run of their own. ---
own_group_row() {
  local want
  base_world
  case "$1" in
    authenticated)
      add_cluster other-cluster us-east1
      subject_grant other-cluster "$(group_subject system:authenticated)" create "" pods "" envoy-operator ""
      want="FINDING group:system:authenticated: create pods in envoy-operator, guarding envoy-operator/envoy-operator on cluster $project/us-east1/other-cluster"
      ;;
    namespace)
      add_cluster other-cluster us-east1
      subject_grant other-cluster "$(group_subject system:serviceaccounts:apps)" create "" pods "" envoy-operator ""
      want="FINDING group:system:serviceaccounts:apps: create pods in envoy-operator, guarding envoy-operator/envoy-operator on cluster $project/us-east1/other-cluster"
      ;;
    masters-code)
      refuse_live
      edit "$world/render.json" '.items += [{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding",
        metadata: {name: "masters", namespace: "envoy-operator"},
        roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "cluster-admin"},
        subjects: [{apiGroup: "rbac.authorization.k8s.io", kind: "Group", name: "system:masters"}]}]'
      want="FINDING group:system:masters: create serviceaccounts/token envoy-operator in envoy-operator, declared by RoleBinding envoy-operator/masters"
      ;;
  esac
  run
  want_status 1 "Kubernetes' own group ($1)"
  want_line "$want" "Kubernetes' own group ($1)"
}
own_group_shared() {
  local item planted want entry wants=()
  base_world
  for item in "$@"; do
    case "$item" in
      serviceaccounts)
        subject_grant operator-cluster "$(group_subject system:serviceaccounts)" create "" pods "" envoy-operator ""
        want="FINDING group:system:serviceaccounts: create pods in envoy-operator"
        ;;
      masters)
        # The fake binds each grant by RoleBinding planted-<its index> to ClusterRole planted-<its index>.
        planted="planted-$(jq length "$world/clusters/operator-cluster/grants.json")"
        subject_grant operator-cluster "$(group_subject system:masters)" get "" configmaps "" legion ""
        want="FINDING group:system:masters: bound by RoleBinding legion/$planted to ClusterRole $planted, so every holder of a system:masters certificate takes it"
        ;;
    esac
    wants+=("$item"$'\t'"$want")
  done
  run
  want_status 1 "Kubernetes' own groups ($*) on the operator's cluster"
  want_recorded "Kubernetes' own groups ($*) on the operator's cluster"
  for entry in "${wants[@]}"; do
    want_line "${entry#*$'\t'}" "Kubernetes' own group (${entry%%$'\t'*})"
  done
}
if row ax; then
  each own_group_row authenticated namespace masters-code
  own_group_shared serviceaccounts masters
  passed "(ax) system:authenticated, system:serviceaccounts, system:serviceaccounts:apps, a non-bootstrap live binding of system:masters, and a render's system:masters binding each exit 1"
fi

# --- (ay) Any other Group, judged by where the cluster takes its groups from. ---
readonly security_group=gke-security-groups@example.com
other_group_row() {
  local want
  base_world
  case "$1" in
    member)
      edge_member
      cluster_admin edge-cluster "$(group_subject ci-deployers)"
      want="UNKNOWN live: Workload Identity on fleet member edge: it takes its users and groups from its own authenticator, not Google, so the Group subjects bound here (ci-deployers) name members this check cannot list"
      ;;
    google-on)
      add_cluster other-cluster us-east1
      edit "$world/clusters.json" 'map(if .name == "other-cluster" then . + {authenticatorGroupsConfig: {enabled: true, securityGroup: $g}} else . end)' \
        --arg g "$security_group"
      cluster_admin other-cluster "$(group_subject devs@example.com)"
      want="UNKNOWN live: Workload Identity on $project/us-east1/other-cluster: Google Groups for RBAC is on (security group $security_group), so a Group subject gives its Google group's members its rights, which this check neither lists nor judges (Group subjects bound now: devs@example.com)"
      ;;
    google-on-member)
      grade_member
      edit "$world/fleet.json" '.memberships |= map(if .id == "grade" then . + {groups: {enabled: true, securityGroup: $g}} else . end)' \
        --arg g "$security_group"
      cluster_admin grade-cluster "$(group_subject devs@example.com)"
      want="UNKNOWN $grade_step: Google Groups for RBAC is on (security group $security_group), so a Group subject gives its Google group's members its rights, which this check neither lists nor judges (Group subjects bound now: devs@example.com)"
      ;;
    describe-refused)
      grade_member
      : >"$world/refuse-describe-grade-cluster"
      cluster_admin grade-cluster "$(group_subject devs@example.com)"
      want="UNKNOWN $grade_step: whether Google Groups for RBAC is on is unknown (gcloud container clusters describe grade-cluster was refused (it needs container.clusters.get)"
      ;;
    google-on-operator)
      edit "$world/clusters.json" 'map(if .name == "operator-cluster" then . + {authenticatorGroupsConfig: {enabled: true, securityGroup: $g}} else . end)' \
        --arg g "$security_group"
      want="UNKNOWN live: RBAC on operator-cluster: Google Groups for RBAC is on (security group $security_group), so a Group subject gives its Google group's members its rights, which this check neither lists nor judges (Group subjects bound now: none)"
      ;;
  esac
  run
  want_status 2 "another Group ($1)"
  want_text "$want" "another Group ($1)"
}
if row ay; then
  each other_group_row member google-on google-on-member describe-refused google-on-operator
  passed "(ay) a Group other than Kubernetes' own reads UNKNOWN on a non-GKE member, where Google Groups for RBAC is on, where whether it is on is unknown, and on the operator's cluster with it on"
fi
