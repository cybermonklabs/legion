# shellcheck shell=bash
# shellcheck disable=SC2016,SC2034,SC2154 # fake_bin is the test's; each single-quoted $name is jq's or a fake's own, not this shell's
# operator-token-minters/test-fakes.sh: the fake kubectl, gcloud, agent-secrets and curl that
# operator-token-minters.test.sh puts first on PATH, each answering from the world directory a run
# names. Sourced by the test, whose header says what each fake models.
# fake writes the fake command $1 from the body on stdin, after the header every fake shares: the
# world directory it reads, which each run names in OPERATOR_MINTERS_WORLD.
fake() {
  { printf '#!/usr/bin/env bash\nset -euo pipefail\nworld="${OPERATOR_MINTERS_WORLD:?the test names the world in OPERATOR_MINTERS_WORLD}"\n' && cat; } >"$fake_bin/$1"
  chmod +x "$fake_bin/$1"
}

# --- Fake gcloud. Every flag the check passes is --name=value or a bare --name, and the words
# before the first flag are the command. ---
fake gcloud <<'EOF'
read -r project <"$world/project"
read -r number <"$world/number"
words=()
declare -A flag=()
for arg in "$@"; do
  case "$arg" in
    --*=*) name="${arg%%=*}"; flag[${name#--}]="${arg#*=}" ;;
    --*) flag[${arg#--}]=true ;;
    *) words+=("$arg") ;;
  esac
done
refuse() { echo "ERROR: (gcloud.$1) PERMISSION_DENIED: $2" >&2; exit 1; }
case "${words[*]}" in
  "iam roles describe "*)
    role="${words[3]}"
    [ ! -e "$world/refuse-describe" ] || refuse iam.roles.describe "Permission 'iam.roles.get' denied on resource (or it may not exist)."
    if ! jq -e --arg r "$role" 'has($r)' "$world/roles.json" >/dev/null; then
      echo "ERROR: (gcloud.iam.roles.describe) NOT_FOUND: The role named $role was not found." >&2
      exit 1
    fi
    jq --arg r "$role" '{name: $r, title: $r, stage: "GA", etag: "AA==", includedPermissions: .[$r]}' "$world/roles.json"
    ;;
  "projects describe $project")
    jq -n --arg p "$project" --arg n "$number" \
      '{projectId: $p, projectNumber: $n, name: $p, lifecycleState: "ACTIVE", parent: {type: "organization", id: "100"}}'
    ;;
  "projects get-ancestors "*)
    # A project's own ancestors when the world holds them (ancestors/<project>.json), else the
    # project under organization 100.
    if [ -e "$world/ancestors/${words[2]}.json" ]; then
      cat "$world/ancestors/${words[2]}.json"
    else
      jq -n --arg p "${words[2]}" '[{id: $p, type: "project"}, {id: "100", type: "organization"}]'
    fi
    ;;
  "container clusters list")
    # The world's project's clusters, or another project's from pool-clusters/<project>.json; a
    # project the world holds no clusters for is refused, as GKE refuses one the caller cannot read.
    if [ "${flag[project]:-}" != "$project" ]; then
      [ -e "$world/pool-clusters/${flag[project]:-}.json" ] ||
        refuse container.clusters.list "Required \"container.clusters.list\" permission(s) for \"projects/${flag[project]:-}\"."
      cat "$world/pool-clusters/${flag[project]:-}.json"
      exit 0
    fi
    [ ! -e "$world/refuse-clusters" ] || refuse container.clusters.list "Required \"container.clusters.list\" permission(s) for \"projects/$project\"."
    [ ! -e "$world/missing-zones" ] || echo "WARNING: The following zones did not respond: us-west1-b. List results may be incomplete." >&2
    cat "$world/clusters.json"
    ;;
  "container clusters get-credentials "*)
    # A cluster of the project's own list, of another project's list, or a GKE member of the fleet,
    # in the project named.
    cluster="${words[3]}"
    others="$world/pool-clusters/${flag[project]:-}.json"
    [ -e "$others" ] || others=/dev/null
    jq -e -n --arg c "$cluster" --arg l "${flag[location]:-}" --arg p "${flag[project]:-}" --arg project "$project" \
      --slurpfile fleet "$world/fleet.json" --slurpfile clusters "$world/clusters.json" --slurpfile others "$others" \
      '($p == $project and any($clusters[0][]; .name == $c and .location == $l))
       or any(($others[0] // [])[]; .name == $c and .location == $l)
       or any($fleet[0].memberships[]; .gke == {project: $p, location: $l, cluster: $c})' >/dev/null ||
      { echo "ERROR: (gcloud.container.clusters.get-credentials) NOT_FOUND: cluster $cluster in ${flag[location]:-} of project ${flag[project]:-} not found" >&2; exit 1; }
    [ ! -e "$world/refuse-credentials-$cluster" ] ||
      refuse container.clusters.get-credentials "Required \"container.clusters.get\" permission(s) for \"projects/${flag[project]:-}/locations/${flag[location]:-}/clusters/$cluster\"."
    [ -n "${KUBECONFIG:-}" ] || { echo "fake gcloud: get-credentials with no KUBECONFIG would write the caller's own kubeconfig" >&2; exit 2; }
    # A second cluster of one name has its world at clusters/<name>@<location>.
    if [ -d "$world/clusters/$cluster@${flag[location]:-}" ]; then cluster="$cluster@${flag[location]:-}"; fi
    printf 'cluster: %s\n' "$cluster" >"$KUBECONFIG"
    echo "Fetching cluster endpoint and auth data." >&2
    ;;
  "container clusters describe "*)
    # A cluster of the project's own list, or a GKE member of the fleet (its `groups`, Google Groups
    # for RBAC, from fleet.json), as GKE describes it: authenticatorGroupsConfig when it is set.
    cluster="${words[3]}"
    [ ! -e "$world/refuse-describe-$cluster" ] ||
      refuse container.clusters.describe "Required \"container.clusters.get\" permission(s) for \"projects/${flag[project]:-}/locations/${flag[location]:-}/clusters/$cluster\"."
    jq -e --arg c "$cluster" --arg l "${flag[location]:-}" --arg p "${flag[project]:-}" --arg project "$project" --slurpfile fleet "$world/fleet.json" '
      first((if $p == $project then .[] | select(.name == $c and .location == $l) else empty end),
            ($fleet[0].memberships[] | select(.gke == {project: $p, location: $l, cluster: $c}) | {name: $c, location: $l} + (if .groups then {authenticatorGroupsConfig: .groups} else {} end)))
      // error("no such cluster")' "$world/clusters.json" ||
      { echo "ERROR: (gcloud.container.clusters.describe) NOT_FOUND: cluster $cluster in ${flag[location]:-} of project ${flag[project]:-} not found" >&2; exit 1; }
    ;;
  "container fleet memberships get-credentials "*)
    # A member of the fleet that is no GKE cluster, through the Connect gateway.
    id="${words[4]}"
    [ "${flag[project]:-}" = "$(jq -r .project "$world/fleet.json")" ] || { echo "fake gcloud: membership get-credentials in project ${flag[project]:-none}" >&2; exit 2; }
    member="$(jq -c --arg id "$id" --arg l "${flag[location]:-}" 'first(.memberships[] | select(.id == $id and .location == $l and (has("gke") | not))) // empty' "$world/fleet.json")"
    [ -n "$member" ] || { echo "ERROR: (gcloud.container.fleet.memberships.get-credentials) NOT_FOUND: membership $id in ${flag[location]:-} not found" >&2; exit 1; }
    [ "$(jq -r '.refuse // false' <<<"$member")" != true ] ||
      refuse container.fleet.memberships.get-credentials "Permission 'gkehub.gateway.generateCredentials' denied on 'projects/$number/locations/${flag[location]:-}/memberships/$id'"
    [ -n "${KUBECONFIG:-}" ] || { echo "fake gcloud: get-credentials with no KUBECONFIG would write the caller's own kubeconfig" >&2; exit 2; }
    printf 'cluster: %s\n' "$(jq -r .cluster <<<"$member")" >"$KUBECONFIG"
    echo "A new kubeconfig entry has been generated and set as the current context." >&2
    ;;
  "auth print-access-token")
    [ ! -e "$world/refuse-token" ] || { echo "ERROR: (gcloud.auth.print-access-token) You do not currently have an active account selected." >&2; exit 1; }
    echo example-access-token
    ;;
  "asset analyze-iam-policy")
    [ "${flag[show-response]:-}" = true ] || { echo "fake gcloud: without --show-response gcloud prints its own summary, which this fake does not render" >&2; exit 2; }
    scope=""
    for kind in organization folder project; do
      [ -z "${flag[$kind]:-}" ] || scope="$kind/${flag[$kind]}"
    done
    [ ! -e "$world/refuse-analyze" ] || refuse asset.analyze-iam-policy "Permission 'cloudasset.assets.analyzeIamPolicy' denied on resource '//cloudresourcemanager.googleapis.com/${scope/\//s/}' (or it may not exist)."
    # A binding on the resource, on the project that holds it (a bucket's `_` and the project
    # number are this world's project), or, from the organization's scope, on the organization.
    # With --analyze-service-account-impersonation, each service account among the holders gets an
    # analysis of who holds one of the six impersonation permissions on it, and so on through each
    # service account those list, as the API's serviceAccountImpersonationAnalysis does.
    jq -n --arg scope "$scope" --arg res "${flag[full-resource-name]:-}" --arg perms "${flag[permissions]:-}" \
      --arg project "$project" --arg number "$number" \
      --arg org_res "//cloudresourcemanager.googleapis.com/organizations/100" \
      --arg expand_groups "${flag[expand-groups]:-}${flag[output-group-edges]:-}" --arg expand_roles "${flag[expand-roles]:-}" \
      --arg impersonation "${flag[analyze-service-account-impersonation]:-}" \
      --arg explored "$([ -e "$world/not-explored" ] && echo false || echo true)" \
      --arg empty "$([ -e "$world/empty-analysis" ] && echo true || echo false)" \
      --slurpfile iam "$world/iam.json" --slurpfile roles "$world/roles.json" --slurpfile groups "$world/groups.json" '
      def results($res; $asked):
        ((([$res | capture("/projects/(?<p>[^/]+)") | .p] | first) // "") as $p
         | if $p == $number or $p == "_" then $project else $p end) as $owner
        | [ $iam[0][]
            | select(.attached == $res or .attached == "//cloudresourcemanager.googleapis.com/projects/\($owner)"
                     or (.attached == $org_res and $scope == "organization/100"))
            | . as $b
            | ([($roles[0][$b.role] // [])[] | select(. as $p | any($asked[]; . == $p))]) as $held
            | select(($held | length) > 0 and $empty != "true")
            | ([$b.members[] | select(startswith("group:")) | . as $g | ($groups[0][$g] // [])[] | {sourceNode: $g, targetNode: .}]) as $edges
            | {
                attachedResourceFullName: $b.attached,
                iamBinding: {role: $b.role, members: $b.members},
                accessControlLists: [{
                  resources: [{fullResourceName: $res}],
                  accesses: (if $expand_roles == "true" then [$held[] | {permission: .}] else [{role: $b.role}] end)
                }],
                identityList: (if $expand_groups == "truetrue"
                  then {identities: ([$b.members[], $edges[].targetNode] | unique | map({name: .})), groupEdges: $edges}
                  else {identities: ($b.members | map({name: .}))} end),
                fullyExplored: true
              } ];
      def sa_res: capture("^serviceAccount:(?<e>[^@]+@(?<p>[a-z0-9-]+)\\.iam\\.gserviceaccount\\.com)$")
        | "//iam.googleapis.com/projects/\(.p)/serviceAccounts/\(.e)";
      def accounts($rs): [$rs[] | .identityList.identities[].name | sa_res] | unique;
      ["iam.serviceAccounts.actAs", "iam.serviceAccounts.signBlob", "iam.serviceAccounts.signJwt", "iam.serviceAccounts.getAccessToken",
       "iam.serviceAccounts.getOpenIdToken", "iam.serviceAccounts.implicitDelegation"] as $impersonate
      | def derive($todo; $done):
          if ($todo | length) == 0 then []
          else $todo[0] as $r | results($r; $impersonate) as $rs
            | [{analysisQuery: {resourceSelector: {fullResourceName: $r}}, analysisResults: $rs, fullyExplored: true}]
              + derive((($todo[1:] + accounts($rs)) | unique) - $done - [$r]; $done + [$r])
          end;
      results($res; $perms | split(",")) as $main
      | {mainAnalysis: {analysisQuery: {scope: ($scope | sub("/"; "s/")), resourceSelector: {fullResourceName: $res}},
                        analysisResults: $main, fullyExplored: ($explored == "true")},
         fullyExplored: ($explored == "true")}
        + (if $impersonation == "true" then {serviceAccountImpersonationAnalysis: derive(accounts($main); [])} else {} end)'
    ;;
  *)
    echo "fake gcloud: unexpected call: gcloud $*" >&2
    exit 2
    ;;
esac
EOF

# --- Fake kubectl: the cluster is the one the kubeconfig get-credentials wrote names. ---
fake kubectl <<'EOF'
kubeconfig=""
args=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --kubeconfig) kubeconfig="$2"; shift 2 ;;
    *) args+=("$1"); shift ;;
  esac
done
[ -n "$kubeconfig" ] || { echo "fake kubectl: called without --kubeconfig, so it would reach the caller's own cluster" >&2; exit 2; }
read -r _ cluster <"$kubeconfig"
dir="$world/clusters/$cluster"
[ -d "$dir" ] || { echo "fake kubectl: no cluster $cluster" >&2; exit 2; }
forbidden() {
  echo "Error from server (Forbidden): $1 is forbidden: User \"reader@example-project.iam.gserviceaccount.com\" cannot $2 at the cluster scope: requires one of [\"$3\"] permission(s)." >&2
  exit 1
}
case "${args[*]}" in
  "create -f - -o json")
    [ ! -e "$dir/refuse-sar" ] || forbidden subjectaccessreviews.authorization.k8s.io 'create resource "subjectaccessreviews" in API group "authorization.k8s.io"' container.subjectAccessReviews.create
    jq --slurpfile grants "$dir/grants.json" --arg masters "$([ -e "$dir/no-masters" ] && echo off || echo on)" '
      def answer:
        .spec as $s | ($s.resourceAttributes // {}) as $a
        | ($s.user // "") as $user | ($s.groups // []) as $groups
        | [ $grants[0][]
            | select(
                (if .subject.kind == "ServiceAccount" then $user == "system:serviceaccount:\(.subject.namespace):\(.subject.name)"
                 elif .subject.kind == "Group" then (.subject.name as $name | any($groups[]; . == $name))
                 else $user == .subject.name end)
                and (.verb == "*" or .verb == ($a.verb // "")) and (.group == "*" or .group == ($a.group // ""))
                and (.resource == "*" or .resource == ($a.resource // "")) and (.subresource == "*" or .subresource == ($a.subresource // ""))
                and (.namespace == null or .namespace == ($a.namespace // ""))
                and (.name == null or .name == ($a.name // "")))
            | .binding ] as $by
        | (($masters == "on" and any($groups[]; . == "system:masters"))) as $master
        | . + {status: (if $master then {allowed: true, reason: "RBAC: allowed by ClusterRoleBinding \"cluster-admin\" of ClusterRole \"cluster-admin\" to Group \"system:masters\""}
                        elif ($by | length) > 0 then {allowed: true, reason: "RBAC: allowed by \($by[0])"}
                        else {allowed: false} end)};
      if .kind == "List" then .items |= map(answer) else answer end'
    ;;
  "get serviceaccounts --all-namespaces -o json")
    [ ! -e "$dir/refuse-list" ] || forbidden serviceaccounts 'list resource "serviceaccounts" in API group ""' container.serviceAccounts.list
    # An answer that is not the JSON it should be, which stops a run that reads it unguarded.
    if [ -e "$dir/garbage-serviceaccounts" ]; then
      echo "<html>502 Bad Gateway</html>"
      exit 0
    fi
    jq '{apiVersion: "v1", kind: "List", items: [.[] | {apiVersion: "v1", kind: "ServiceAccount", metadata: ({namespace: .namespace, name: .name, uid: "uid-\(.namespace)-\(.name)"} + (if .gsa then {annotations: {"iam.gke.io/gcp-service-account": .gsa}} else {} end))}]}' "$dir/serviceaccounts.json"
    ;;
  "get clusterrolebindings,rolebindings --all-namespaces -o json")
    # The built-in cluster-admin binding, and one binding of each grant, naming its subject.
    [ ! -e "$dir/refuse-bindings" ] || forbidden clusterrolebindings.rbac.authorization.k8s.io 'list resource "clusterrolebindings" in API group "rbac.authorization.k8s.io"' container.clusterRoleBindings.list
    jq '{apiVersion: "v1", kind: "List", items: (
      [{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding",
        metadata: {name: "cluster-admin", labels: {"kubernetes.io/bootstrapping": "rbac-defaults"}},
        roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "cluster-admin"},
        subjects: [{apiGroup: "rbac.authorization.k8s.io", kind: "Group", name: "system:masters"}]}]
      + [to_entries[] | .key as $i | .value as $g | {
          apiVersion: "rbac.authorization.k8s.io/v1",
          kind: (if $g.namespace == null then "ClusterRoleBinding" else "RoleBinding" end),
          metadata: ({name: "planted-\($i)"} + (if $g.namespace == null then {} else {namespace: $g.namespace} end)),
          roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "planted-\($i)"},
          subjects: [$g.subject | if .kind == "ServiceAccount" then . else . + {apiGroup: "rbac.authorization.k8s.io"} end]}])}' \
      "$dir/grants.json"
    ;;
  "get namespace "*" -o name")
    namespace="${args[2]}"
    if grep -qxF -- "$namespace" "$dir/namespaces"; then
      echo "namespace/$namespace"
    else
      echo "Error from server (NotFound): namespaces \"$namespace\" not found" >&2
      exit 1
    fi
    ;;
  "api-resources --namespaced=true --verbs=create -o name")
    # The cluster's discovery: Kubernetes' own namespaced, creatable resources, and each of
    # api-resources' lines, a custom resource the world plants.
    printf '%s\n' bindings configmaps endpoints events limitranges persistentvolumeclaims pods podtemplates \
      replicationcontrollers resourcequotas secrets serviceaccounts services controllerrevisions.apps daemonsets.apps \
      deployments.apps replicasets.apps statefulsets.apps localsubjectaccessreviews.authorization.k8s.io \
      horizontalpodautoscalers.autoscaling cronjobs.batch jobs.batch leases.coordination.k8s.io \
      endpointslices.discovery.k8s.io events.events.k8s.io ingresses.networking.k8s.io networkpolicies.networking.k8s.io \
      poddisruptionbudgets.policy rolebindings.rbac.authorization.k8s.io roles.rbac.authorization.k8s.io \
      csistoragecapacities.storage.k8s.io
    [ ! -e "$dir/api-resources" ] || cat "$dir/api-resources"
    if [ -e "$dir/refuse-api-resources" ]; then
      echo "error: unable to retrieve the complete list of server APIs: agents.x-k8s.io/v1alpha1: the server is currently unable to handle the request" >&2
      exit 1
    fi
    ;;
  *)
    echo "fake kubectl: unexpected call: kubectl ${args[*]}" >&2
    exit 2
    ;;
esac
EOF

# --- Fake agent-secrets: `secret show NAME --json`, as its failures print on stderr: a line naming
# the sign-in first, then the refusal (read from legion-envoy-v30.43.0 on 2026-11-21, which words a
# missing secret as gRPC's own `rpc error: code = NotFound` where the current source writes
# `GetSecret answered NotFound`; old-words picks the first wording). ---
fake agent-secrets <<'EOF'
[ "$#" -eq 4 ] && [ "$1 $2 $4" = "secret show --json" ] || { echo "fake agent-secrets: unexpected call: agent-secrets $*" >&2; exit 2; }
signed_in="agent-secrets secret show: using the active gcloud account (gcloud configuration example): reader@example-project.iam.gserviceaccount.com"
if [ -e "$world/refuse-secret-show" ]; then
  printf '%s\n' "$signed_in" "agent-secrets secret show: Secret Manager GetSecret answered PermissionDenied (missing permission: secretmanager.secrets.get; its message is not printed); looked in GCP Secret Manager project 123456789012" >&2
  exit 1
fi
if [ ! -e "$world/secrets/$3.json" ]; then
  if [ -e "$world/old-words" ]; then
    printf '%s\n' "$signed_in" "agent-secrets secret show: rpc error: code = NotFound desc = Secret [projects/123456789012/secrets/agent-secrets-$3] not found.; looked in GCP Secret Manager project 123456789012" >&2
  else
    printf '%s\n' "$signed_in" "agent-secrets secret show: Secret Manager GetSecret answered NotFound (its message is not printed); looked in GCP Secret Manager project 123456789012" >&2
  fi
  exit 1
fi
cat "$world/secrets/$3.json"
EOF

# --- Fake curl: the GKE Hub API's GET projects/<p>/locations/-/memberships, as v1 answers it
# (ListMembershipsResponse: resources, nextPageToken, unreachable), one membership a page, the
# unreachable locations on the first page alone. The bearer token must come in through the
# configuration curl reads on stdin (-K -), never on the command line. ---
fake curl <<'EOF'
config="$(cat)"
url="" token=""
for ((i = 1; i <= $#; i++)); do
  arg="${!i}"
  case "$arg" in
    --data-urlencode)
      i=$((i + 1))
      kv="${!i}"
      [ "${kv%%=*}" != pageToken ] || token="${kv#*=}"
      ;;
    --max-time | -K) i=$((i + 1)) ;;
    -*) ;;
    *) url="$arg" ;;
  esac
done
[[ "$*" != *example-access-token* ]] || { echo "fake curl: the access token is on the command line" >&2; exit 2; }
# The scheme is held apart from the fake token so no line of this file reads as a credential.
scheme=Bearer
[[ "$config" == *"header = \"Authorization: $scheme example-access-token\""* ]] || { echo "fake curl: no bearer token on stdin" >&2; exit 2; }
fleet_project="$(jq -r .project "$world/fleet.json")"
if [ "$url" != "https://gkehub.googleapis.com/v1/projects/$fleet_project/locations/-/memberships" ] || [ -e "$world/refuse-fleet" ]; then
  project_of="${url#https://gkehub.googleapis.com/v1/projects/}"
  printf '{"error": {"code": 403, "message": "Permission %s denied on %s", "status": "PERMISSION_DENIED"}}\n' \
    "'gkehub.memberships.list'" "'projects/${project_of%%/*}/locations/-/memberships'"
  echo "curl: (22) The requested URL returned error: 403" >&2
  exit 22
fi
jq --argjson at "${token:-0}" '
  . as $w
  | ($w.memberships | length) as $n
  | {resources: [$w.memberships[$at] // empty | {
       name: "projects/\($w.project)/locations/\(.location)/memberships/\(.id)",
       uniqueId: "uid-\(.id)", state: {code: "READY"},
       endpoint: (if .gke then {gkeCluster: {resourceLink: "//container.googleapis.com/projects/\(.gke.project)/locations/\(.gke.location)/clusters/\(.gke.cluster)"}}
                  else {kubernetesMetadata: {kubernetesApiServerVersion: "v1.33.0"}} end)}]}
  + (if $at + 1 < $n then {nextPageToken: "\($at + 1)"} else {} end)
  + (if $at == 0 and (($w.unreachable // []) | length) > 0 then {unreachable: $w.unreachable} else {} end)' "$world/fleet.json"
EOF
