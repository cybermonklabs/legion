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
  "projects describe "*)
    # Another project the world holds a number for (numbers/<project>), or none: NOT_FOUND, as gcloud
    # answers a project the caller cannot see.
    other="${words[2]}"
    [ -e "$world/numbers/$other" ] ||
      { echo "ERROR: (gcloud.projects.describe) NOT_FOUND: Project $other not found or permission denied." >&2; exit 1; }
    jq -n --arg p "$other" --arg n "$(<"$world/numbers/$other")" \
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
    # for RBAC, and its `settings`, from fleet.json), as GKE describes it: authenticatorGroupsConfig
    # when it is set.
    cluster="${words[3]}"
    [ ! -e "$world/refuse-describe-$cluster" ] ||
      refuse container.clusters.describe "Required \"container.clusters.get\" permission(s) for \"projects/${flag[project]:-}/locations/${flag[location]:-}/clusters/$cluster\"."
    jq -e --arg c "$cluster" --arg l "${flag[location]:-}" --arg p "${flag[project]:-}" --arg project "$project" --slurpfile fleet "$world/fleet.json" '
      first((if $p == $project then .[] | select(.name == $c and .location == $l) else empty end),
            ($fleet[0].memberships[] | select(.gke == {project: $p, location: $l, cluster: $c})
             | {name: $c, location: $l} + (if .groups then {authenticatorGroupsConfig: .groups} else {} end) + (.settings // {})))
      // error("no such cluster")' "$world/clusters.json" ||
      { echo "ERROR: (gcloud.container.clusters.describe) NOT_FOUND: cluster $cluster in ${flag[location]:-} of project ${flag[project]:-} not found" >&2; exit 1; }
    ;;
  "container fleet scopes list")
    # As gcloud answers it: the permitted scopes at locations/global alone, so a scope elsewhere is
    # not listed. The check lists scopes through the API (fake curl); this case lets a run of an
    # older script, which asked gcloud, be compared on the same world.
    [ ! -e "$world/refuse-scopes" ] || refuse container.fleet.scopes.list "Permission 'gkehub.scopes.list' denied on 'projects/${flag[project]:-}/locations/global/scopes'"
    { if [ -e "$world/scopes.json" ]; then cat "$world/scopes.json"; else echo '[]'; fi; } | jq '[.[] | select(.name | contains("/locations/global/"))]'
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
# A world marked token-secret-data gives each token Secret data, as a real API server holds it;
# every call then looks under the run's work directories ($TMPDIR/operator-token-minters.*) for
# that data, and records where it finds it (row cp).
secret_data_b64="$(printf '%s' SECRET-DATA-CANARY-7f3a | base64)"
if [ -e "$dir/token-secret-data" ]; then
  for work in "${TMPDIR:-/tmp}"/operator-token-minters.*/; do
    [ -d "$work" ] || continue
    found="$(grep -rlF -e "$secret_data_b64" -e SECRET-DATA-CANARY-7f3a "$work" 2>/dev/null || true)"
    [ -z "$found" ] || printf '%s\n' "$found" >>"$dir/secret-data-in-work"
  done
fi
# shaped_answer NAME: when the world holds garbage-NAME, empty-NAME, object-NAME or emptylist-NAME,
# answers the call with exit 0 but not as kubectl writes the API server's answer (a List or an
# object, or for the reviews one document per review): an error page, an empty body, `{}`, or a
# List with no item, as a proxy between kubectl and the API server can (row cu). jq reads an empty
# body as no value and `{}` as an object whose .items[]? is nothing, so a program over either
# prints nothing or an empty list and exits 0; only a check of the answer's shape tells them from
# an empty list, and only a witness that every cluster holds tells an empty List from a whole one.
# Each branch it serves calls it first, so its command is matched once.
shaped_answer() {
  [ ! -e "$dir/garbage-$1" ] || { echo "<html>502 Bad Gateway</html>"; exit 0; }
  [ ! -e "$dir/empty-$1" ] || exit 0
  [ ! -e "$dir/object-$1" ] || { echo '{}'; exit 0; }
  [ ! -e "$dir/emptylist-$1" ] || { echo '{"apiVersion":"v1","kind":"List","items":[]}'; exit 0; }
}
case "${args[*]}" in
  "create -f - -o json")
    shaped_answer reviews
    [ ! -e "$dir/refuse-sar" ] || forbidden subjectaccessreviews.authorization.k8s.io 'create resource "subjectaccessreviews" in API group "authorization.k8s.io"' container.subjectAccessReviews.create
    # kubectl create, given a List, creates each object on its own and prints one JSON document per
    # object it made, never a List (kubectl 1.35.9's create.go), so this answers so. Each call is
    # counted in review-calls, under a lock, since a review may be asked by several calls at once
    # (REVIEW_WORKERS): two calls that took the same number would write one file over the other.
    # With one worker (the harness's default) the first call is the step's reviews and each later
    # one a follow_becomes round, which is what the markers below name; a row that sets
    # REVIEW_WORKERS itself uses no marker.
    # A world marked
    #   review-answers-reversed answers the reviews in the reverse of the order asked (row ct);
    #   review-follow-refused answers every review of a follow round refused, the witness's too, and
    #   review-follow-nostatus answers a follow round's reviews with no status (row cy);
    #   review-witness-copied answers the witness's reviews with copies of the first other review,
    #   and review-repeated answers the last review with a copy of the first, keeping the count (row cy).
    while ! mkdir "$dir/review-calls.lock" 2>/dev/null; do :; done
    calls=$(( $(cat "$dir/review-calls" 2>/dev/null || echo 0) + 1 ))
    echo "$calls" >"$dir/review-calls"
    rmdir "$dir/review-calls.lock"
    # Each review asked, one a line (verb, group, resource, subresource, namespace, name, and the
    # subject), in this call's own file, which a row reads as review-requests-*.tsv: calls that run
    # at once must not write one file between them. A row that holds the check to what it reviewed
    # and what it left to the roles' read reads them (row bu's handed-off item).
    tee "$dir/review-call-$calls.json" |
      jq -r '.items[] | .spec as $s | ($s.resourceAttributes // {})
        | [.verb, (.group // ""), .resource, (.subresource // ""), (.namespace // ""), (.name // ""),
           ($s.user // "group:" + ((($s.groups // []) | join(","))))] | @tsv' \
        >"$dir/review-requests-$calls.tsv" 2>/dev/null || true
    set -- "$dir/review-call-$calls.json"
    mark() { [ -e "$dir/$1" ] && echo on || echo off; }
    # A world marked review-refuse-holding or review-short-holding (a subject, as the record's
    # last field writes it) refuses, or answers one review short, the one call that asks about
    # that subject, whichever batch that is (row cz's later-batch items).
    holding() { [ -s "$dir/$1" ] && cut -f7 "$dir/review-requests-$calls.tsv" | grep -qxF -- "$(cat "$dir/$1")"; }
    ! holding review-refuse-holding ||
      forbidden subjectaccessreviews.authorization.k8s.io 'create resource "subjectaccessreviews" in API group "authorization.k8s.io"' container.subjectAccessReviews.create
    short=off
    ! holding review-short-holding || short=on
    jq --slurpfile grants "$dir/grants.json" --arg masters "$([ -e "$dir/no-masters" ] && echo off || echo on)" \
      --arg reversed "$(mark review-answers-reversed)" --arg copied "$(mark review-witness-copied)" \
      --arg repeated "$(mark review-repeated)" --argjson call "$calls" --arg short "$short" \
      --arg follow_refused "$(mark review-follow-refused)" --arg follow_nostatus "$(mark review-follow-nostatus)" '
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
      def witness: (.spec.user // "") == "" and (.spec.groups // []) == ["system:masters"];
      [.items[] | answer]
      | (if $reversed == "on" then reverse else . end)
      | (if $copied == "on" then (map(select(witness | not)) | first) as $other | map(if witness then $other else . end) else . end)
      | (if $repeated == "on" then .[-1] = .[0] else . end)
      | (if $call > 1 and $follow_refused == "on" then map(.status = {allowed: false}) else . end)
      | (if $call > 1 and $follow_nostatus == "on" then map(del(.status)) else . end)
      | (if $short == "on" then .[:-1] else . end)
      | .[]' "$1"
    ;;
  "get serviceaccounts --all-namespaces -o json")
    shaped_answer serviceaccounts
    [ ! -e "$dir/refuse-list" ] || forbidden serviceaccounts 'list resource "serviceaccounts" in API group ""' container.serviceAccounts.list
    jq '{apiVersion: "v1", kind: "List", items: [.[] | {apiVersion: "v1", kind: "ServiceAccount", metadata: ({namespace: .namespace, name: .name, uid: "uid-\(.namespace)-\(.name)"} + (if .gsa then {annotations: {"iam.gke.io/gcp-service-account": .gsa}} else {} end))} + (if .secrets then {secrets: .secrets} else {} end)]}' "$dir/serviceaccounts.json"
    ;;
  "get clusterrolebindings,rolebindings --all-namespaces -o json")
    # The built-in cluster-admin binding (unless the world drops it), the bootstrap RoleBinding
    # kube-system/system::extension-apiserver-authentication-reader, and one binding of each grant,
    # naming its subject: a RoleBinding when the grant names a namespace, to the grant's Role when
    # it has role_kind Role. A world marked no-rolebindings answers the ClusterRoleBindings alone,
    # as an answer whose RoleBinding half came back empty would (row cu).
    shaped_answer bindings
    [ ! -e "$dir/refuse-bindings" ] || forbidden clusterrolebindings.rbac.authorization.k8s.io 'list resource "clusterrolebindings" in API group "rbac.authorization.k8s.io"' container.clusterRoleBindings.list
    jq --arg boot "$([ -e "$dir/no-bootstrap-binding" ] && echo off || echo on)" --arg rolebindings "$([ -e "$dir/no-rolebindings" ] && echo off || echo on)" '{apiVersion: "v1", kind: "List", items: (
      [{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding",
        metadata: {name: "cluster-admin", labels: {"kubernetes.io/bootstrapping": "rbac-defaults"}},
        roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "cluster-admin"},
        subjects: [{apiGroup: "rbac.authorization.k8s.io", kind: "Group", name: "system:masters"}]} | select($boot == "on")]
      + [{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding",
          metadata: {namespace: "kube-system", name: "system::extension-apiserver-authentication-reader",
                     labels: {"kubernetes.io/bootstrapping": "rbac-defaults"}},
          roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "Role", name: "extension-apiserver-authentication-reader"},
          subjects: [{apiGroup: "rbac.authorization.k8s.io", kind: "User", name: "system:kube-scheduler"}]}]
      + [to_entries[] | .key as $i | .value as $g | {
          apiVersion: "rbac.authorization.k8s.io/v1",
          kind: (if $g.namespace == null then "ClusterRoleBinding" else "RoleBinding" end),
          metadata: ({name: "planted-\($i)"} + (if $g.namespace == null then {} else {namespace: $g.namespace} end)),
          roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: ($g.role_kind // "ClusterRole"), name: "planted-\($i)"},
          subjects: [$g.subject | if .kind == "ServiceAccount" then . else . + {apiGroup: "rbac.authorization.k8s.io"} end]}]
      | map(select($rolebindings == "on" or .kind != "RoleBinding")))}' \
      "$dir/grants.json"
    ;;
  "get secrets -n "*" --field-selector type=kubernetes.io/service-account-token "*)
    # A namespace's token Secrets: the world's token-secrets.json entries ({namespace, name}) there,
    # each with its data in a world marked token-secret-data, as a real API server holds them. The
    # fake answers as kubectl does from what the API server sends it (read with kubectl 1.36.4
    # against a server that records each request's Accept header): for the default output
    # (--server-print=true) kubectl asks for the server-side Table, whose rows carry each Secret's
    # name, type, number of data keys and age beside its metadata alone, and prints the header
    # NAME TYPE DATA AGE and a row a line. A world marked token-secrets-no-table answers as a
    # server or proxy that cannot make the Table does, with the Secrets whole, data included, which
    # kubectl prints under NAME and AGE alone (read with kubectl 1.36.4 against a server that
    # answers plain JSON whatever the Accept header asks; row cr). --no-headers prints the rows
    # alone; no row's run asks for it, but row cr's control does, step-rbac.sh's list as it was
    # before that row, so the branch is kept. For -o json, -o name and -o jsonpath kubectl asks for
    # plain JSON and gets the Secrets whole, data included. Each response the API server sends is
    # recorded, one a line, in token-secret-responses, and each list of the Secrets whole asked for
    # with -o in secret-data-asked (row cp).
    [ ! -e "$dir/refuse-token-secrets" ] || forbidden secrets 'list resource "secrets" in API group ""' container.secrets.list
    whole="$({ if [ -e "$dir/token-secrets.json" ]; then cat "$dir/token-secrets.json"; else echo '[]'; fi; } |
      jq -c --arg ns "${args[3]}" --arg data "$([ -e "$dir/token-secret-data" ] && echo "$secret_data_b64")" '{apiVersion: "v1", kind: "List", items: [.[] | select(.namespace == $ns)
        | {apiVersion: "v1", kind: "Secret", type: "kubernetes.io/service-account-token", metadata: {namespace: .namespace, name: .name}}
        + (if $data == "" then {} else {data: {token: $data}} end)]}')"
    case "${args[*]:6}" in
      "--server-print=true" | "--server-print=true --no-headers")
        if [ -e "$dir/token-secrets-no-table" ]; then
          printf '%s\n' "$whole" >>"$dir/token-secret-responses"
          header="NAME   AGE"
          rows="$(jq -r '.items[] | "\(.metadata.name)   1d"' <<<"$whole")"
        else
          table="$(jq -c '{apiVersion: "meta.k8s.io/v1", kind: "Table",
            columnDefinitions: [{name: "Name", type: "string"}, {name: "Type", type: "string"}, {name: "Data", type: "string"}, {name: "Age", type: "string"}],
            rows: [.items[] | {cells: [.metadata.name, .type, ((.data // {}) | length), "1d"],
              object: {apiVersion: "meta.k8s.io/v1", kind: "PartialObjectMetadata", metadata: .metadata}}]}' <<<"$whole")"
          printf '%s\n' "$table" >>"$dir/token-secret-responses"
          header="NAME   TYPE   DATA   AGE"
          rows="$(jq -r '.rows[].cells | map(tostring) | join("   ")' <<<"$table")"
        fi
        if [ -z "$rows" ]; then
          echo "No resources found in ${args[3]} namespace." >&2
        else
          [ "${args[7]:-}" = --no-headers ] || printf '%s\n' "$header"
          printf '%s\n' "$rows"
        fi
        ;;
      "-o json" | "-o name" | "-o jsonpath="*)
        printf '%s\n' "$whole" >>"$dir/token-secret-responses"
        printf '%s\n' "kubectl ${args[*]}" >>"$dir/secret-data-asked"
        case "${args[7]}" in
          json) jq . <<<"$whole" ;;
          name) jq -r '.items[] | "secret/\(.metadata.name)"' <<<"$whole" ;;
          'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}') jq -r '.items[].metadata.name' <<<"$whole" ;;
          *) echo "fake kubectl: unexpected output: ${args[7]}" >&2; exit 2 ;;
        esac
        ;;
      *) echo "fake kubectl: unexpected list of token Secrets: ${args[*]}" >&2; exit 2 ;;
    esac
    ;;
  "get service -n "*" -o json")
    # One Service: the world's services.json entry for <namespace>/<name>, or a ClusterIP Service
    # whose selector picks pods labelled app=<name>.
    shaped_answer service
    { if [ -e "$dir/services.json" ]; then cat "$dir/services.json"; else echo '{}'; fi; } |
      jq --arg key "${args[3]}/${args[4]}" --arg name "${args[4]}" \
        '.[$key] // {spec: {type: "ClusterIP", selector: {app: $name}}} | {apiVersion: "v1", kind: "Service"} + .'
    ;;
  "get mutatingwebhookconfigurations,validatingwebhookconfigurations -o json")
    # The cluster's webhook configurations: the world's webhooks.json (each a configuration object), or none.
    shaped_answer webhooks
    [ ! -e "$dir/refuse-webhooks" ] || forbidden mutatingwebhookconfigurations.admissionregistration.k8s.io 'list resource "mutatingwebhookconfigurations" in API group "admissionregistration.k8s.io"' container.mutatingWebhookConfigurations.list
    { if [ -e "$dir/webhooks.json" ]; then cat "$dir/webhooks.json"; else echo '[]'; fi; } | jq '{apiVersion: "v1", kind: "List", items: .}'
    ;;
  "get customresourcedefinitions -o json")
    # The cluster's CRDs: the world's crds.json (each a CustomResourceDefinition), or none.
    shaped_answer crds
    [ ! -e "$dir/refuse-crds" ] || forbidden customresourcedefinitions.apiextensions.k8s.io 'list resource "customresourcedefinitions" in API group "apiextensions.k8s.io"' container.customResourceDefinitions.list
    { if [ -e "$dir/crds.json" ]; then cat "$dir/crds.json"; else echo '[]'; fi; } | jq '{apiVersion: "v1", kind: "List", items: .}'
    ;;
  "get workloadallowlists.auto.gke.io -o json")
    # Autopilot's installed workload allowlists: the world's allowlists.json (names), or none.
    shaped_answer allowlists
    [ ! -e "$dir/refuse-allowlists" ] || forbidden workloadallowlists.auto.gke.io 'list resource "workloadallowlists" in API group "auto.gke.io"' container.thirdPartyObjects.list
    { if [ -e "$dir/allowlists.json" ]; then cat "$dir/allowlists.json"; else echo '[]'; fi; } |
      jq '{apiVersion: "v1", kind: "List", items: [.[] | {apiVersion: "auto.gke.io/v1", kind: "WorkloadAllowlist", metadata: {name: .}}]}'
    ;;
  "get "*admissionpolic*" -o json")
    # The cluster's admission policies and their bindings: the world's admission-policies.json, or none.
    shaped_answer policies
    [ ! -e "$dir/refuse-policies" ] || forbidden validatingadmissionpolicies.admissionregistration.k8s.io 'list resource "validatingadmissionpolicies" in API group "admissionregistration.k8s.io"' container.thirdPartyObjects.list
    { if [ -e "$dir/admission-policies.json" ]; then cat "$dir/admission-policies.json"; else echo '[]'; fi; } | jq '{apiVersion: "v1", kind: "List", items: .}'
    ;;
  "get pods -n "*" -o json")
    # A namespace's pods: the world's pods.json entries (each a Pod) there, or none.
    shaped_answer pods
    [ ! -e "$dir/refuse-pods" ] || forbidden pods 'list resource "pods" in API group ""' container.pods.list
    { if [ -e "$dir/pods.json" ]; then cat "$dir/pods.json"; else echo '[]'; fi; } |
      jq --arg ns "${args[3]}" '{apiVersion: "v1", kind: "List", items: [.[] | select(.metadata.namespace == $ns)]}'
    ;;
  "get --raw /metrics")
    # The API server's metrics, the seats of global-default among them: 40, as on the operator's
    # real cluster, or the world's global-default-seats. A world marked metrics-refused refuses the
    # read, and metrics-without-seats answers metrics that hold no global-default seats (row cz).
    if [ -e "$dir/metrics-refused" ]; then
      echo 'Error from server (Forbidden): forbidden: User "ci" cannot get path "/metrics"' >&2
      exit 1
    fi
    echo '# TYPE apiserver_flowcontrol_nominal_limit_seats gauge'
    echo 'apiserver_flowcontrol_nominal_limit_seats{priority_level="workload-low"} 46'
    [ -e "$dir/metrics-without-seats" ] ||
      echo "apiserver_flowcontrol_nominal_limit_seats{priority_level=\"global-default\"} $(cat "$dir/global-default-seats" 2>/dev/null || echo 40)"
    ;;
  "get --raw "*)
    # Discovery of one group version: the world's raw.json, keyed by path, or the API server's 404.
    jq -e --arg p "${args[2]}" '.[$p] // error("no such path")' "$dir/raw.json" 2>/dev/null ||
      { echo "Error from server (NotFound): the server could not find the requested resource" >&2; exit 1; }
    ;;
  "get roles,clusterroles --all-namespaces -o json")
    # The built-in cluster-admin role, the bootstrap Role kube-system/extension-apiserver-
    # authentication-reader, and one role of each grant (planted-<its index>, as the bindings' list
    # names it), holding the grant as a rule, limited by name when the grant names one: a Role in the
    # grant's namespace when it has role_kind Role, else a ClusterRole. A world marked no-roles
    # answers the ClusterRoles alone, as an answer whose Role half came back empty would (row cu).
    shaped_answer roles
    [ ! -e "$dir/refuse-roles" ] || forbidden roles.rbac.authorization.k8s.io 'list resource "roles" in API group "rbac.authorization.k8s.io"' container.roles.list
    jq --arg roles "$([ -e "$dir/no-roles" ] && echo off || echo on)" '{apiVersion: "v1", kind: "List", items: (
      [{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole",
        metadata: {name: "cluster-admin", labels: {"kubernetes.io/bootstrapping": "rbac-defaults"}},
        rules: [{apiGroups: ["*"], resources: ["*"], verbs: ["*"]}]},
       {apiVersion: "rbac.authorization.k8s.io/v1", kind: "Role",
        metadata: {namespace: "kube-system", name: "extension-apiserver-authentication-reader",
                   labels: {"kubernetes.io/bootstrapping": "rbac-defaults"}},
        rules: [{apiGroups: [""], resources: ["configmaps"], resourceNames: ["extension-apiserver-authentication"], verbs: ["get", "list", "watch"]}]}]
      + [to_entries[] | .key as $i | .value as $g | {
          apiVersion: "rbac.authorization.k8s.io/v1", kind: ($g.role_kind // "ClusterRole"),
          metadata: ({name: "planted-\($i)"} + (if ($g.role_kind // "") == "Role" then {namespace: $g.namespace} else {} end)),
          rules: [{apiGroups: [$g.group], verbs: [$g.verb],
                   resources: [if $g.resource == "*" or ($g.subresource // "") == "" then $g.resource else "\($g.resource)/\($g.subresource)" end]}
                  + (if $g.name then {resourceNames: [$g.name]} else {} end)]}]
      | map(select($roles == "on" or .kind != "Role")))}' "$dir/grants.json"
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
  "api-resources --namespaced=false -o name")
    # The cluster's cluster-scoped discovery: Kubernetes' own, and each of cluster-resources' lines,
    # a cluster-scoped custom resource the world plants (row ce).
    printf '%s\n' componentstatuses namespaces nodes persistentvolumes mutatingwebhookconfigurations.admissionregistration.k8s.io \
      validatingadmissionpolicies.admissionregistration.k8s.io validatingadmissionpolicybindings.admissionregistration.k8s.io \
      validatingwebhookconfigurations.admissionregistration.k8s.io customresourcedefinitions.apiextensions.k8s.io \
      apiservices.apiregistration.k8s.io tokenreviews.authentication.k8s.io subjectaccessreviews.authorization.k8s.io \
      certificatesigningrequests.certificates.k8s.io ingressclasses.networking.k8s.io runtimeclasses.node.k8s.io \
      clusterrolebindings.rbac.authorization.k8s.io clusterroles.rbac.authorization.k8s.io priorityclasses.scheduling.k8s.io \
      csidrivers.storage.k8s.io csinodes.storage.k8s.io storageclasses.storage.k8s.io volumeattachments.storage.k8s.io
    [ ! -e "$dir/cluster-resources" ] || cat "$dir/cluster-resources"
    if [ -e "$dir/refuse-cluster-resources" ]; then
      echo "error: unable to retrieve the complete list of server APIs: example.io/v1: the server is currently unable to handle the request" >&2
      exit 1
    fi
    ;;
  "api-resources --api-group=admissionregistration.k8s.io -o name")
    # The admission resources a GKE cluster of 1.30 or later serves, or the world's admission-resources.
    if [ -e "$dir/refuse-admission-resources" ]; then
      echo "error: unable to retrieve the complete list of server APIs: admissionregistration.k8s.io/v1: the server is currently unable to handle the request" >&2
      exit 1
    fi
    if [ -e "$dir/admission-resources" ]; then
      cat "$dir/admission-resources"
    else
      printf '%s.admissionregistration.k8s.io\n' mutatingwebhookconfigurations validatingadmissionpolicies \
        validatingadmissionpolicybindings validatingwebhookconfigurations
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
# unreachable locations on the first page alone; and GET projects/<p>/locations/-/scopes
# (ListScopesResponse: scopes, nextPageToken) from the world's scopes.json, one scope a page. The
# bearer token and the page token must come in through the configuration curl reads on stdin
# (-K -), never on the command line. Each page token holds a backslash and then a double quote,
# so a script that writes it into that configuration unescaped reads back another token, which
# this fake refuses. ---
fake curl <<'EOF'
config="$(cat)"
url="" token=""
for ((i = 1; i <= $#; i++)); do
  arg="${!i}"
  case "$arg" in
    --data-urlencode)
      i=$((i + 1))
      kv="${!i}"
      [ "${kv%%=*}" != pageToken ] || { echo "fake curl: the page token is on the command line" >&2; exit 2; }
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
# A page token, as curl reads a quoted configuration value: \" is a double quote and \\ a backslash.
page_line="$(grep '^data-urlencode = "pageToken=' <<<"$config" || true)"
if [ -n "$page_line" ]; then
  quoted="${page_line#data-urlencode = \"pageToken=}"
  [ "${quoted: -1}" = '"' ] || { echo "fake curl: the page token's configuration line does not end its quote: $page_line" >&2; exit 2; }
  read_token="$(printf '%s' "${quoted%\"}" | sed -e 's/\\\(["\\]\)/\1/g')"
  token="${read_token#p\\\"}"
  [[ "$read_token" == p\\\"* && "$token" =~ ^[0-9]+$ ]] || { echo "fake curl: the page token read back as $read_token, which this fake never sent" >&2; exit 2; }
fi
fleet_project="$(jq -r .project "$world/fleet.json")"
deny() {
  printf '{"error": {"code": 403, "message": "Permission %s denied on %s", "status": "PERMISSION_DENIED"}}\n' "'$1'" "'$2'"
  echo "curl: (22) The requested URL returned error: 403" >&2
  exit 22
}
project_of="${url#https://gkehub.googleapis.com/v1/projects/}"
if [ "$url" = "https://gkehub.googleapis.com/v1/projects/$fleet_project/locations/-/scopes" ]; then
  [ ! -e "$world/refuse-scopes" ] || deny gkehub.scopes.list "projects/$fleet_project/locations/-/scopes"
  { if [ -e "$world/scopes.json" ]; then cat "$world/scopes.json"; else echo '[]'; fi; } |
    jq --argjson at "${token:-0}" '. as $s | {scopes: [$s[$at] // empty]} + (if $at + 1 < ($s | length) then {nextPageToken: "p\\\"\($at + 1)"} else {} end)'
  exit 0
fi
if [ "$url" != "https://gkehub.googleapis.com/v1/projects/$fleet_project/locations/-/memberships" ] || [ -e "$world/refuse-fleet" ]; then
  deny gkehub.memberships.list "projects/${project_of%%/*}/locations/-/memberships"
fi
jq --argjson at "${token:-0}" '
  . as $w
  | ($w.memberships | length) as $n
  | {resources: [$w.memberships[$at] // empty | {
       name: "projects/\($w.project)/locations/\(.location)/memberships/\(.id)",
       uniqueId: "uid-\(.id)", state: {code: "READY"},
       endpoint: (if .gke then {gkeCluster: {resourceLink: "//container.googleapis.com/projects/\(.gke.project)/locations/\(.gke.location)/clusters/\(.gke.cluster)"}}
                  else {kubernetesMetadata: {kubernetesApiServerVersion: "v1.33.0"}} end)}]}
  + (if $at + 1 < $n then {nextPageToken: "p\\\"\($at + 1)"} else {} end)
  + (if $at == 0 and (($w.unreachable // []) | length) > 0 then {unreachable: $w.unreachable} else {} end)' "$world/fleet.json"
EOF
