# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-rbac-4.sh: the rows of operator-token-minters.test.sh on
# Kubernetes RBAC: a ServiceAccount a binding names that does not exist yet, writes to a binding, an
# email-shaped User on a fleet member that is no GKE cluster, a client certificate the cluster
# signs, and impersonating a user whose power is IAM's. Sourced by the test after its world and
# helpers; its header says what the fakes model. The rows:
#
#   (bb) a ServiceAccount a binding names that does not exist (ci/ghost, allowed to create pods in
#        envoy-operator) is reviewed and exits 1;
#   (bd) writes to a binding: a ServiceAccount with update rolebindings in envoy-operator, or with
#        patch of the operator's RoleBinding by name, beside bind of its ClusterRole by name; update
#        of that RoleBinding by name; escalate of that ClusterRole by name (those four in one world,
#        one ServiceAccount each); and on another cluster ci/runner with patch of the
#        ClusterRoleBinding cluster-admin and bind of the ClusterRole cluster-admin, each by name:
#        each exits 1 naming the rights;
#   (bi) on a fleet member that is no GKE cluster, a User that looks like an email
#        (ci-bot@corp.example) bound to cluster-admin reads UNKNOWN: that cluster's own
#        authenticator names its users, so the name proves no person;
#   (bj) a client certificate signed by the cluster: a ServiceAccount that may create certificate
#        signing requests, update their approval and approve the signer
#        kubernetes.io/kube-apiserver-client (rv9, beside a person who may mint the operator's
#        token), one that may approve for kubernetes.io/* without create, the approval's patch with
#        the client signer (rv13), its update with the kubelet signer (rv12), the signer
#        kubernetes.io/legacy-unknown and a signer of any name (those six in one world, one
#        ServiceAccount each), ci/runner with kubernetes.io/* on another cluster, and on a fleet
#        member that is no GKE cluster create of certificatesigningrequests/nodeclient or
#        selfnodeclient alone, each exit 1 naming the chain or the right; create alone, and create
#        and patch of the request my-csr by name with no signer (pn6), exit 0;
#   (bo) a ServiceAccount that may impersonate, by name, a user whose power is IAM's
#        (owner@example.com, who holds roles/owner on the organization, or admin@example.com, a
#        member of a group holding roles/container.admin) is a finding naming that user (one world,
#        one ServiceAccount a user).

# --- (bb) A ServiceAccount a binding names, which does not exist yet. ---
if row bb; then
  base_world
  subject_grant operator-cluster "$(sa_subject ci ghost)" create "" pods "" envoy-operator ""
  run
  want_status 1 "a bound ServiceAccount that does not exist"
  want_line "FINDING system:serviceaccount:ci:ghost: create pods in envoy-operator" "a bound ServiceAccount that does not exist"
  passed "(bb) a ServiceAccount a binding names, which does not exist yet, is reviewed and exits 1"
fi

# --- (bd) Writes to a binding. RBAC lets one who may bind a binding's role, by name, update the
# binding, and a name-limited write is refused to a review that names nothing. Each item reaches the
# operator's token through its own RoleBinding envoy-operator/planted-0 and ClusterRole planted-0
# (the fake names each grant's binding and role planted-<its index>, and the operator's token grant
# is the world's first), or cluster-admin through the bootstrap binding on another cluster. The four
# items on the operator's cluster share one world and one run (binding_write_shared), each on a
# ServiceAccount of its own, legion/<item>: grants are only appended, so planted-0 stays the
# operator's token grant, and the fake answers each review by its subject, so the bind line update
# and patch-named both want is each one's own. Each item is still judged by each of its own lines,
# exact; want_recorded holds the run's last line to the lines it printed. cluster-admin, which adds
# a cluster, keeps a run of its own. ---
binding_write_row() {
  local who suffix line want=()
  base_world
  case "$1" in
    cluster-admin)
      add_cluster other-cluster us-east1
      add_sa other-cluster ci runner
      subject_grant other-cluster "$(sa_subject ci runner)" patch rbac.authorization.k8s.io clusterrolebindings "" "" cluster-admin
      subject_grant other-cluster "$(sa_subject ci runner)" bind rbac.authorization.k8s.io clusterroles "" "" cluster-admin
      who=system:serviceaccount:ci:runner suffix=" on cluster $project/us-east1/other-cluster"
      want=("patch clusterrolebindings.rbac.authorization.k8s.io cluster-admin cluster-wide" "bind clusterroles.rbac.authorization.k8s.io cluster-admin cluster-wide")
      ;;
  esac
  run
  want_status 1 "a write to a binding ($1)"
  for line in "${want[@]}"; do
    want_line "FINDING $who: $line$suffix" "a write to a binding ($1)"
  done
}
binding_write_shared() {
  local item subject want line entry lines=()
  base_world
  for item in "$@"; do
    add_sa operator-cluster legion "$item"
    subject="$(sa_subject legion "$item")"
    case "$item" in
      update)
        subject_grant operator-cluster "$subject" update rbac.authorization.k8s.io rolebindings "" envoy-operator ""
        subject_grant operator-cluster "$subject" bind rbac.authorization.k8s.io clusterroles "" envoy-operator planted-0
        want=("update rolebindings.rbac.authorization.k8s.io in envoy-operator" "bind clusterroles.rbac.authorization.k8s.io planted-0 in envoy-operator")
        ;;
      patch-named)
        subject_grant operator-cluster "$subject" patch rbac.authorization.k8s.io rolebindings "" envoy-operator planted-0
        subject_grant operator-cluster "$subject" bind rbac.authorization.k8s.io clusterroles "" envoy-operator planted-0
        want=("patch rolebindings.rbac.authorization.k8s.io planted-0 in envoy-operator" "bind clusterroles.rbac.authorization.k8s.io planted-0 in envoy-operator")
        ;;
      update-named)
        subject_grant operator-cluster "$subject" update rbac.authorization.k8s.io rolebindings "" envoy-operator planted-0
        want=("update rolebindings.rbac.authorization.k8s.io planted-0 in envoy-operator")
        ;;
      escalate-named)
        subject_grant operator-cluster "$subject" escalate rbac.authorization.k8s.io clusterroles "" "" planted-0
        want=("escalate clusterroles.rbac.authorization.k8s.io planted-0 cluster-wide")
        ;;
    esac
    for line in "${want[@]}"; do lines+=("$item"$'\t'"$line"); done
  done
  run
  want_status 1 "the writes to a binding ($*), each on a ServiceAccount of its own"
  want_recorded "the writes to a binding ($*), each on a ServiceAccount of its own"
  for entry in "${lines[@]}"; do
    want_line "FINDING system:serviceaccount:legion:${entry%%$'\t'*}: ${entry#*$'\t'}" "a write to a binding (${entry%%$'\t'*})"
  done
}
if row bd; then
  each binding_write_row cluster-admin
  binding_write_shared update patch-named update-named escalate-named
  passed "(bd) update of rolebindings, patch or update of the operator's RoleBinding by name, bind or escalate of its ClusterRole by name, and patch of cluster-admin's binding with bind of its role by name on another cluster each exit 1 naming the rights"
fi

# --- (bi) An email-shaped User on a fleet member that is no GKE cluster. Its own authenticator names
# its users, as it names its groups, so ci-bot@corp.example there is no proof of a person. ---
if row bi; then
  base_world
  edge_member
  cluster_admin edge-cluster "$(user_subject ci-bot@corp.example)"
  run
  want_status 2 "ci-bot@corp.example bound to cluster-admin on a non-GKE member"
  want_text "UNKNOWN live: Workload Identity on fleet member edge: it takes its users from its own authenticator, not Google, so the User subjects bound here that look like emails (ci-bot@corp.example) are not known to be people" \
    "ci-bot@corp.example bound to cluster-admin on a non-GKE member"
  passed "(bi) an email-shaped User bound on a fleet member that is no GKE cluster reads UNKNOWN"
fi

# --- (bj) A client certificate the cluster signs. A certificate signed for
# kubernetes.io/kube-apiserver-client authenticates as any user and groups its request names but
# system:masters, and one for the kubelet signer as a node, so whoever may write a request's
# approval (update or patch) and approve such a signer becomes anyone, a person who may mint the
# operator's token included. The six items of exit 1 on the operator's cluster share one world and
# one run (csr_shared), each on a ServiceAccount of its own, legion/<item>: csr_chains joins one
# principal's parts into one line, so on one ServiceAccount their parts would merge, and it reads no
# other principal's, so rv9's person, who may mint the operator's token, changes no other item's
# line. Each item is still judged by its own line, exact; want_recorded holds the run's last line to
# the lines it printed. ci/runner on another cluster and the node client requests (each adds a
# cluster or a fleet member) keep a run of their own, and so do create-only and create-patch-named,
# which exit 0. ---
readonly csr_clause="cluster-wide, so it can have the cluster sign it a client certificate for a user, group or node it names"
csr_row() {
  local who=system:serviceaccount:legion:worker cluster=operator-cluster subject suffix="" want
  base_world
  subject="$(sa_subject legion worker)"
  case "$1" in
    other-cluster)
      add_cluster other-cluster us-east1
      add_sa other-cluster ci runner
      cluster=other-cluster subject="$(sa_subject ci runner)" who=system:serviceaccount:ci:runner
      suffix=" on cluster $project/us-east1/other-cluster"
      ;;
    nodeclient | selfnodeclient)
      edge_member
      cluster=edge-cluster subject="$(sa_subject apps deployer)" who=system:serviceaccount:apps:deployer
      suffix=" on fleet member $project/global/edge"
      ;;
  esac
  case "$1" in
    other-cluster)
      subject_grant "$cluster" "$subject" update certificates.k8s.io certificatesigningrequests approval "" ""
      subject_grant "$cluster" "$subject" approve certificates.k8s.io signers "" "" "kubernetes.io/*"
      want="update certificatesigningrequests/approval and approve signers kubernetes.io/* $csr_clause"
      ;;
    nodeclient | selfnodeclient)
      subject_grant "$cluster" "$subject" create certificates.k8s.io certificatesigningrequests "$1" "" ""
      want="create certificatesigningrequests/$1.certificates.k8s.io cluster-wide, which kube-controller-manager approves by itself"
      ;;
    create-patch-named)
      # pn6: a create limited by name, beside a patch of that name, is no chain without a signer.
      subject_grant "$cluster" "$subject" create certificates.k8s.io certificatesigningrequests "" "" my-csr
      subject_grant "$cluster" "$subject" patch certificates.k8s.io certificatesigningrequests "" "" my-csr
      run
      want_status 0 "create and patch of the certificate signing request my-csr by name, alone"
      return
      ;;
    create-only)
      subject_grant "$cluster" "$subject" create certificates.k8s.io certificatesigningrequests "" "" ""
      run
      want_status 0 "create certificatesigningrequests alone"
      return
      ;;
  esac
  run
  want_status 1 "a certificate signing chain ($1)"
  want_line "FINDING $who: $want$suffix" "a certificate signing chain ($1)"
}
csr_shared() {
  local item subject want entry wants=()
  base_world
  for item in "$@"; do
    add_sa operator-cluster legion "$item"
    subject="$(sa_subject legion "$item")"
    case "$item" in
      rv9)
        subject_grant operator-cluster "$(user_subject ops@example.com)" create "" serviceaccounts token envoy-operator envoy-operator
        subject_grant operator-cluster "$subject" create certificates.k8s.io certificatesigningrequests "" "" ""
        subject_grant operator-cluster "$subject" update certificates.k8s.io certificatesigningrequests approval "" ""
        subject_grant operator-cluster "$subject" approve certificates.k8s.io signers "" "" kubernetes.io/kube-apiserver-client
        want="create certificatesigningrequests, update certificatesigningrequests/approval and approve signers kubernetes.io/kube-apiserver-client $csr_clause"
        ;;
      wildcard)
        subject_grant operator-cluster "$subject" update certificates.k8s.io certificatesigningrequests approval "" ""
        subject_grant operator-cluster "$subject" approve certificates.k8s.io signers "" "" "kubernetes.io/*"
        want="update certificatesigningrequests/approval and approve signers kubernetes.io/* $csr_clause"
        ;;
      rv13)
        subject_grant operator-cluster "$subject" create certificates.k8s.io certificatesigningrequests "" "" ""
        subject_grant operator-cluster "$subject" patch certificates.k8s.io certificatesigningrequests approval "" ""
        subject_grant operator-cluster "$subject" approve certificates.k8s.io signers "" "" kubernetes.io/kube-apiserver-client
        want="create certificatesigningrequests, patch certificatesigningrequests/approval and approve signers kubernetes.io/kube-apiserver-client $csr_clause"
        ;;
      rv12)
        subject_grant operator-cluster "$subject" update certificates.k8s.io certificatesigningrequests approval "" ""
        subject_grant operator-cluster "$subject" approve certificates.k8s.io signers "" "" kubernetes.io/kube-apiserver-client-kubelet
        want="update certificatesigningrequests/approval and approve signers kubernetes.io/kube-apiserver-client-kubelet $csr_clause"
        ;;
      legacy)
        subject_grant operator-cluster "$subject" update certificates.k8s.io certificatesigningrequests approval "" ""
        subject_grant operator-cluster "$subject" approve certificates.k8s.io signers "" "" kubernetes.io/legacy-unknown
        want="update certificatesigningrequests/approval and approve signers kubernetes.io/legacy-unknown $csr_clause"
        ;;
      any-signer)
        subject_grant operator-cluster "$subject" update certificates.k8s.io certificatesigningrequests approval "" ""
        subject_grant operator-cluster "$subject" approve certificates.k8s.io signers "" "" ""
        want="update certificatesigningrequests/approval and approve signers of any name $csr_clause"
        ;;
    esac
    wants+=("$item"$'\t'"$want")
  done
  run
  want_status 1 "the certificate signing chains ($*), each on a ServiceAccount of its own"
  want_recorded "the certificate signing chains ($*), each on a ServiceAccount of its own"
  for entry in "${wants[@]}"; do
    want_line "FINDING system:serviceaccount:legion:${entry%%$'\t'*}: ${entry#*$'\t'}" "a certificate signing chain (${entry%%$'\t'*})"
  done
}
if row bj; then
  each csr_row other-cluster nodeclient selfnodeclient create-only create-patch-named
  csr_shared rv9 wildcard rv13 rv12 legacy any-signer
  passed "(bj) who may write a certificate signing request's approval (update or patch) and approve a client, kubelet, legacy or any signer, on the operator's cluster or another, and who may create a node client request on a non-GKE member, exits 1 naming the chain; create alone, or create and patch of one request by name (pn6), exits 0"
fi

# --- (bo) Impersonating, by name, a user whose power comes from IAM: GKE's IAM authorizer judges an
# impersonated user by its own IAM, so whoever may impersonate an IAM holder takes what it holds. The
# two share one world and one run, each on a ServiceAccount of its own, legion/<the user's name
# before its @>: live_named names the subject each named rule's binding gives it, so each is still
# judged by its own line, exact; want_recorded holds the run's last line to the lines it printed. ---
iam_user_shared() {
  local user
  base_world
  for user in "$@"; do
    add_sa operator-cluster legion "${user%%@*}"
    subject_grant operator-cluster "$(sa_subject legion "${user%%@*}")" impersonate "" users "" "" "$user"
  done
  run
  want_status 1 "a ServiceAccount of legion for each of $* that may impersonate it by name"
  want_recorded "a ServiceAccount of legion for each of $* that may impersonate it by name"
  for user in "$@"; do
    want_line "FINDING system:serviceaccount:legion:${user%%@*}: impersonate users $user cluster-wide" \
      "legion/${user%%@*} may impersonate $user by name"
  done
}
if row bo; then
  iam_user_shared owner@example.com admin@example.com
  passed "(bo) a ServiceAccount that may impersonate, by name, a user holding the GKE IAM permissions exits 1 naming the user"
fi
