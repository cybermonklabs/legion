# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-admission-1.sh: the rows of operator-token-minters.test.sh on
# admission power: the rights on the admission objects themselves, and the discovery that lists
# them. Sourced by the test after its world and helpers; its header says what the fakes model. The
# rows:
#
#   (bf) each admission right (create, update and patch of mutatingwebhookconfigurations,
#        validatingwebhookconfigurations, validatingadmissionpolicies and their bindings, and of
#        mutatingadmissionpolicies and their bindings where the discovery lists them) given on the
#        operator's cluster, and create of each on another cluster, to a ServiceAccount that holds
#        nothing else, is a finding naming it, and no finding of that ServiceAccount names another
#        planted account's right or a right of another verb or resource (two worlds, one
#        ServiceAccount a right);
#   (bg) a discovery of admissionregistration.k8s.io that lists no validatingwebhookconfigurations,
#        or cannot be read on the operator's cluster or another, reads UNKNOWN.

# --- (bf) The admission rights. A mutating webhook or policy can add a container to the operator's
# next pod, and a validating one can block or reshape what the operator applies. Create, update and
# patch of each resource of admissionregistration.k8s.io on the operator's cluster, each given to
# legion/right-<its place>, and create of each on another of the pool, each given to
# ci/right-<its place>, each a ServiceAccount that holds nothing else (plant_rights). A GKE
# cluster's discovery lists the four resources of admission_gke; the mutating admission policies,
# beta and off by default, are listed in a world of their own, since the check asks what the
# cluster's discovery lists. So the row is two worlds and two runs. ---
admission_gke=(mutatingwebhookconfigurations validatingwebhookconfigurations validatingadmissionpolicies validatingadmissionpolicybindings)
admission_mutating=(mutatingadmissionpolicies mutatingadmissionpolicybindings)
# admission_rights_row RESOURCE...: the admission rights on each RESOURCE, in one world whose two
# clusters' discovery lists admission_gke's resources and each RESOURCE.
admission_rights_row() {
  local resource verb cluster operator=() other=()
  base_world
  add_cluster other-cluster us-east1
  for resource in "$@"; do
    for verb in create update patch; do
      operator+=("$verb|admissionregistration.k8s.io|$resource|||=$verb $resource.admissionregistration.k8s.io cluster-wide")
    done
    other+=("create|admissionregistration.k8s.io|$resource|||=create $resource.admissionregistration.k8s.io cluster-wide")
  done
  # The discovery lists each name once, in its order, as an API server does.
  for cluster in operator-cluster other-cluster; do
    printf '%s.admissionregistration.k8s.io\n' "${admission_gke[@]}" "$@" | sort -u >"$world/clusters/$cluster/admission-resources"
  done
  plant_rights operator-cluster legion "" "${operator[@]}"
  plant_rights other-cluster ci " on cluster $project/us-east1/other-cluster" "${other[@]}"
  run
  want_planted "the admission rights on $*, each given to a ServiceAccount of its own" own
}
if row bf; then
  admission_rights_row "${admission_gke[@]}"
  admission_rights_row "${admission_mutating[@]}"
  # Four rights a resource: create, update and patch on the operator's cluster, create on another.
  passed "(bf) each of the $((4 * (${#admission_gke[@]} + ${#admission_mutating[@]}))) admission rights, given on the operator's cluster or another of the pool where its discovery lists the resource to a ServiceAccount that holds nothing else, is a finding naming it, and no finding of that ServiceAccount names another planted account's right or a right of another verb or resource"
fi

# --- (bg) The admission resources' discovery. Every API server serves the two webhook
# configurations, so a list without one is not whole, and a discovery that cannot be read leaves
# the admission policies unasked. ---
admission_names_row() {
  base_world
  case "$1" in
    missing)
      printf '%s.admissionregistration.k8s.io\n' mutatingwebhookconfigurations validatingadmissionpolicies validatingadmissionpolicybindings \
        >"$world/clusters/operator-cluster/admission-resources"
      run
      want_status 2 "a discovery without validatingwebhookconfigurations"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server's discovery of admissionregistration.k8s.io lists no validatingwebhookconfigurations.admissionregistration.k8s.io, which every API server serves, so its list is not whole" \
        "a discovery without validatingwebhookconfigurations"
      ;;
    refused)
      : >"$world/clusters/operator-cluster/refuse-admission-resources"
      run
      want_status 2 "a discovery of admissionregistration.k8s.io that cannot be read"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server's discovery of admissionregistration.k8s.io could not be read" \
        "a discovery of admissionregistration.k8s.io that cannot be read"
      ;;
    refused-other)
      add_cluster other-cluster us-east1
      : >"$world/clusters/other-cluster/refuse-admission-resources"
      run
      want_status 2 "another cluster's discovery of admissionregistration.k8s.io that cannot be read"
      want_text "UNKNOWN live: Workload Identity on $project/us-east1/other-cluster: the API server's discovery of admissionregistration.k8s.io could not be read" \
        "another cluster's discovery of admissionregistration.k8s.io that cannot be read"
      ;;
  esac
}
if row bg; then
  each admission_names_row missing refused refused-other
  passed "(bg) a discovery of admissionregistration.k8s.io that lists no validatingwebhookconfigurations, or cannot be read on the operator's or another cluster, reads UNKNOWN"
fi
