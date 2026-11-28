# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-rbac-5.sh: the rows of operator-token-minters.test.sh on
# Kubernetes RBAC by name: a token Secret by name and the bindings' witness, rights a live role
# limits by name, the list of roles they are read from, and a token Secret no ServiceAccount lists.
# Sourced by the test after its world and helpers; its header says what the fakes model. The rows:
#
#   (br) legion/worker that may get, by name, a token Secret the operator's ServiceAccount lists
#        exits 1 naming it; a list of bindings without the bootstrap cluster-admin binding, on the
#        operator's cluster or another, reads UNKNOWN;
#   (bu) a right a live role limits by name, and a bind of clusterroles in a namespace outside the
#        guarded ones, outright or by name (the reviewer's example-db world); the five probes: update of the mutating
#        webhook configuration existing-webhook on the operator's cluster, and patch of it on
#        another; create of certificate signing requests, update of the approval of my-csr and
#        approve of kubernetes.io/kube-apiserver-client; patch of the Deployment secrets-broker in
#        secrets-broker; update of the CronJob nightly in envoy-operator; and create of pods/exec of
#        secrets-broker-0: each exits 1 naming the name; create pods limited by name beside patch of
#        that name, and rv14's create and patch of the namespace envoy-operator by name on another
#        cluster, each exit 1 (server-side apply creates by patch); create limited by name alone exits 0;
#        those of the operator's cluster that exit 1 share one world, one ServiceAccount each;
#   (bv) a refused list of roles, on the operator's cluster or another, reads UNKNOWN;
#   (cc) legion/worker that may get, by name, a token Secret of envoy-operator that no ServiceAccount
#        lists exits 1 naming it, and a refused list of the namespace's token Secrets reads UNKNOWN.

# --- (br) A token Secret by name, and the bindings' witness. A review naming no Secret is refused for
# a get limited to a name, so each token Secret a guarded ServiceAccount lists is asked by name; and
# every cluster has the bootstrap cluster-admin binding, so a list without it is not whole. ---
secret_witness_row() {
  base_world
  case "$1" in
    named-secret)
      edit "$world/clusters/operator-cluster/serviceaccounts.json" \
        'map(if .namespace == "envoy-operator" and .name == "envoy-operator" then . + {secrets: [{name: "envoy-operator-token-x7k2p"}]} else . end)'
      subject_grant operator-cluster "$(sa_subject legion worker)" get "" secrets "" envoy-operator envoy-operator-token-x7k2p
      run
      want_status 1 "legion/worker may get the operator's token Secret by name"
      want_line "FINDING system:serviceaccount:legion:worker: get secrets envoy-operator-token-x7k2p in envoy-operator" \
        "legion/worker may get the operator's token Secret by name"
      ;;
    no-bootstrap)
      : >"$world/clusters/operator-cluster/no-bootstrap-binding"
      run
      want_status 2 "a bindings' list without the bootstrap binding"
      want_text "UNKNOWN live: RBAC on operator-cluster: the bindings' list holds no bootstrap cluster-admin ClusterRoleBinding, which every cluster has, so it is not the cluster's whole list" \
        "a bindings' list without the bootstrap binding"
      ;;
    no-bootstrap-other)
      add_cluster other-cluster us-east1
      : >"$world/clusters/other-cluster/no-bootstrap-binding"
      run
      want_status 2 "another cluster's bindings' list without the bootstrap binding"
      want_text "UNKNOWN live: Workload Identity on $project/us-east1/other-cluster: the bindings' list holds no bootstrap cluster-admin ClusterRoleBinding" \
        "another cluster's bindings' list without the bootstrap binding"
      ;;
  esac
}
if row br; then
  each secret_witness_row named-secret no-bootstrap no-bootstrap-other
  passed "(br) a get of the operator's token Secret by name exits 1, and a list of bindings without the bootstrap binding reads UNKNOWN"
fi

# --- (bu) Rights a live role limits by name. RBAC applies a rule with resourceNames only to a request
# that names one of them, so a review naming nothing is refused for each of these; the check reads
# the roles the bindings name. The operator's cluster's items of exit 1 but the hand-off share one
# world and one run (name_limited_shared), each on a ServiceAccount of its own, legion/<item>, beside
# the outside-namespace items' person, dba@example.com, planted once: live_named names the subject a
# named rule's binding gives it and the fake answers each review by its subject, so each item is
# still judged by its own line, exact, naming its own ServiceAccount; want_recorded holds the run's
# last line to the lines it printed. webhook-other and rv14 (each adds a cluster), the hand-off
# (which reads every review the run asked) and create-named (exit 0) keep a run of their own. ---
name_limited_row() {
  local who=system:serviceaccount:legion:worker cluster=operator-cluster subject suffix="" want
  base_world
  subject="$(sa_subject legion worker)"
  case "$1" in
    webhook-other)
      add_cluster other-cluster us-east1
      add_sa other-cluster ci runner
      cluster=other-cluster subject="$(sa_subject ci runner)" who=system:serviceaccount:ci:runner
      suffix=" on cluster $project/us-east1/other-cluster"
      subject_grant "$cluster" "$subject" patch admissionregistration.k8s.io mutatingwebhookconfigurations "" "" existing-webhook
      want="patch mutatingwebhookconfigurations.admissionregistration.k8s.io existing-webhook cluster-wide"
      ;;
    rv14)
      add_cluster other-cluster us-east1
      add_sa other-cluster ci runner
      cluster=other-cluster subject="$(sa_subject ci runner)" who=system:serviceaccount:ci:runner
      suffix=" on cluster $project/us-east1/other-cluster"
      subject_grant "$cluster" "$subject" create "" namespaces "" "" envoy-operator
      subject_grant "$cluster" "$subject" patch "" namespaces "" "" envoy-operator
      want="create namespaces envoy-operator cluster-wide, which its patch or update of that name makes"
      ;;
    create-named)
      subject_grant "$cluster" "$subject" create "" pods "" envoy-operator some-pod
      run
      want_status 0 "create pods limited by name alone, which no request is allowed"
      return
      ;;
    handed-off)
      # The hand-off: a name-limited grant is a finding, and no review names a resource for a
      # write of a role or a binding, or an impersonation of a user or a group: those are asked
      # with no name (RIGHTS_TABLE, and binding_rights for each namespace that holds a
      # RoleBinding), and live_named reads from the cluster's roles each rule that limits one of
      # them by name. The rights the check still reviews by name are its own fixed ones, not a
      # binding's: the signers of the certificate chain, a guarded pair's ServiceAccount, and each
      # token Secret it listed.
      subject_grant "$cluster" "$subject" bind rbac.authorization.k8s.io roles "" envoy-operator operator-role
      run
      want_status 1 "a bind limited to one role's name, read from the cluster's roles"
      want_line "FINDING $who: bind roles.rbac.authorization.k8s.io operator-role in envoy-operator" \
        "a bind limited to one role's name, read from the cluster's roles"
      asked="$temporary_dir/asked-$cluster.tsv"
      cat "$world/clusters/$cluster"/review-requests-*.tsv >"$asked" 2>/dev/null || true
      [ -s "$asked" ] || fail "the check reviewed nothing, so the item proves nothing"
      from_bindings='$6 != "" && ((($1 == "bind" || $1 == "escalate" || $1 == "patch" || $1 == "update") \
        && ($3 == "roles" || $3 == "clusterroles" || $3 == "rolebindings" || $3 == "clusterrolebindings")) \
        || ($1 == "impersonate" && ($3 == "users" || $3 == "groups")))'
      named="$(awk -F'\t' "$from_bindings" "$asked" | wc -l)"
      [ "$named" -eq 0 ] ||
        fail "the check reviewed $named rights a binding limits by name, which live_named reads from the roles: $(awk -F'\t' "$from_bindings" "$asked" | awk -F'\t' '{ print $1, $3, $6 }' | sort -u | head -n 3 | tr '\n' ';')"
      return
      ;;
  esac
  run
  want_status 1 "a right limited by name ($1)"
  want_line "FINDING $who: $want$suffix" "a right limited by name ($1)"
}
name_limited_shared() {
  local item subject want entry wants=()
  base_world
  # The outside-namespace items' person, who reads the broker's database login in example-db.
  subject_grant operator-cluster "$(user_subject dba@example.com)" get "" secrets "" example-db broker-db-login
  for item in "$@"; do
    add_sa operator-cluster legion "$item"
    subject="$(sa_subject legion "$item")"
    case "$item" in
      webhook-operator)
        subject_grant operator-cluster "$subject" update admissionregistration.k8s.io mutatingwebhookconfigurations "" "" existing-webhook
        want="update mutatingwebhookconfigurations.admissionregistration.k8s.io existing-webhook cluster-wide"
        ;;
      csr)
        subject_grant operator-cluster "$subject" create certificates.k8s.io certificatesigningrequests "" "" ""
        subject_grant operator-cluster "$subject" update certificates.k8s.io certificatesigningrequests approval "" my-csr
        subject_grant operator-cluster "$subject" approve certificates.k8s.io signers "" "" kubernetes.io/kube-apiserver-client
        want="create certificatesigningrequests, update certificatesigningrequests/approval my-csr and approve signers kubernetes.io/kube-apiserver-client cluster-wide, so it can have the cluster sign it a client certificate for a user, group or node it names"
        ;;
      deployment)
        subject_grant operator-cluster "$subject" patch apps deployments "" secrets-broker secrets-broker
        want="patch deployments.apps secrets-broker in secrets-broker"
        ;;
      cronjob)
        subject_grant operator-cluster "$subject" update batch cronjobs "" envoy-operator nightly
        want="update cronjobs.batch nightly in envoy-operator"
        ;;
      exec)
        subject_grant operator-cluster "$subject" create "" pods exec secrets-broker secrets-broker-0
        want="create pods/exec secrets-broker-0 in secrets-broker"
        ;;
      create-patch)
        subject_grant operator-cluster "$subject" create "" pods "" envoy-operator some-pod
        subject_grant operator-cluster "$subject" patch "" pods "" envoy-operator some-pod
        want="create pods some-pod in envoy-operator, which its patch or update of that name makes"
        ;;
      outside-namespace | outside-namespace-named)
        # A namespace outside the guarded ones, where a person reads the broker's database login:
        # this ServiceAccount may create rolebindings there and bind clusterroles there, so it can
        # bind itself a ClusterRole that reads that Secret, and the login's holders are hold
        # writers. Only binding_rights asks the bind in that namespace; without it no review allows
        # it. The named item limits the bind to one ClusterRole's name, which no review answers and
        # live_named reads from the cluster's roles for the unnamed right binding_rights asks.
        subject_grant operator-cluster "$subject" create rbac.authorization.k8s.io rolebindings "" example-db ""
        if [ "$item" = outside-namespace ]; then
          subject_grant operator-cluster "$subject" bind rbac.authorization.k8s.io clusterroles "" example-db ""
          want="bind clusterroles.rbac.authorization.k8s.io in example-db"
        else
          subject_grant operator-cluster "$subject" bind rbac.authorization.k8s.io clusterroles "" example-db secret-reader
          want="bind clusterroles.rbac.authorization.k8s.io secret-reader in example-db"
        fi
        ;;
    esac
    wants+=("$item"$'\t'"$want")
  done
  run
  want_status 1 "the rights limited by name ($*), each on a ServiceAccount of its own"
  want_recorded "the rights limited by name ($*), each on a ServiceAccount of its own"
  for entry in "${wants[@]}"; do
    want_line "FINDING system:serviceaccount:legion:${entry%%$'\t'*}: ${entry#*$'\t'}" \
      "a right limited by name, or a bind outside the guarded namespaces (${entry%%$'\t'*})"
  done
}
if row bu; then
  each name_limited_row webhook-other rv14 create-named handed-off
  name_limited_shared webhook-operator csr deployment cronjob exec create-patch outside-namespace outside-namespace-named
  passed "(bu) each right a live role limits by name (a webhook configuration on two clusters, a CSR approval, a Deployment, a CronJob, a pod's exec, a create with its patch) exits 1 naming the name, read from the cluster's roles and reviewed by no request that names a resource; create limited by name alone exits 0; and a bind of clusterroles in a namespace outside the guarded ones, outright or limited by name, exits 1 naming that namespace"
fi

# --- (bv) The roles' list, which the named rights are read from. ---
roles_refused_row() {
  base_world
  case "$1" in
    operator)
      : >"$world/clusters/operator-cluster/refuse-roles"
      run
      want_status 2 "a refused list of roles on the operator's cluster"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server refused the roles' list (it needs list on roles and clusterroles" \
        "a refused list of roles on the operator's cluster"
      ;;
    other)
      add_cluster other-cluster us-east1
      : >"$world/clusters/other-cluster/refuse-roles"
      run
      want_status 2 "a refused list of roles on another cluster"
      want_text "UNKNOWN live: Workload Identity on $project/us-east1/other-cluster: the API server refused the roles' list (it needs list on roles and clusterroles" \
        "a refused list of roles on another cluster"
      ;;
  esac
}
if row bv; then
  each roles_refused_row operator other
  passed "(bv) a refused list of roles, on the operator's cluster or another, reads UNKNOWN naming the permission"
fi

# --- (cc) A token Secret of a guarded namespace no ServiceAccount lists, from the namespace's list of
# that type, by name; a refused list reads UNKNOWN. ---
listed_secret_row() {
  base_world
  case "$1" in
    unlisted)
      jq -n '[{namespace: "envoy-operator", name: "envoy-operator-token-legacy"}]' >"$world/clusters/operator-cluster/token-secrets.json"
      subject_grant operator-cluster "$(sa_subject legion worker)" get "" secrets "" envoy-operator envoy-operator-token-legacy
      run
      want_status 1 "legion/worker may get a token Secret no ServiceAccount lists, by name"
      want_line "FINDING system:serviceaccount:legion:worker: get secrets envoy-operator-token-legacy in envoy-operator" \
        "legion/worker may get a token Secret no ServiceAccount lists, by name"
      ;;
    refused)
      : >"$world/clusters/operator-cluster/refuse-token-secrets"
      run
      want_status 2 "a refused list of token Secrets"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server refused the list of token Secrets in envoy-operator (it needs list on secrets there, container.secrets.list on GKE)" \
        "a refused list of token Secrets"
      ;;
  esac
}
if row cc; then
  each listed_secret_row unlisted refused
  passed "(cc) a guarded namespace's token Secret no ServiceAccount lists is asked by name, and a refused list of them reads UNKNOWN"
fi
