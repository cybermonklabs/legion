# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/admission-backends.sh: admission power outside the admission objects. A
# webhook configuration or an admission policy runs what something else holds: a webhook's backend
# Service and the workload behind it, a policy engine's cluster-scoped resources, an admission
# policy's parameter objects. Whoever may write those changes what the operator's next pod runs, or
# what the operator may apply. Sourced by operator-token-minters.sh, whose header says what each
# step checks.

# admission_power KUBECONFIG STEP [operator]: sets POWER_RIGHTS, the JSON array, as rights_json writes
# rights, of the writes that give admission power beside the admission objects' own on the cluster,
# and prints UNKNOWN on STEP for what it cannot read. Each list it reads (the webhook configurations,
# the CRDs, the workload allowlists, the admission policies and their bindings) and each webhook's
# Service is held to its shape (is_list, is_object) before any read of it, so an answer that is
# empty, `{}` or not JSON reads UNKNOWN naming it, never as a list of nothing:
#   - each webhook whose namespaceSelector can match a guarded namespace, or whose rules can match a
#     cluster-scoped object, which no namespaceSelector filters (webhook-backends.jq): in
#     the namespace of the Service it calls, every way to run or enter a pod there that a guarded
#     namespace is asked (RIGHTS_TABLE's namespaced rows and the custom resources), and the writes
#     to the Service, its Endpoints, EndpointSlices, Secrets (its TLS key) and ConfigMaps (an
#     injector's template); a webhook that calls a URL outside the cluster is UNKNOWN, as is a
#     refused list, and so is one whose Service is of type ExternalName or has no selector (its
#     endpoints can point anywhere), or cannot be read;
#   - each cluster-scoped custom resource of a policy engine (a CRD of kyverno.io, gatekeeper.sh,
#     kubewarden.io or a subgroup of one, or a group whose name holds "policy" or "policies"):
#     create, update and patch cluster-wide, as Kyverno's ClusterPolicy and MutatingPolicy or
#     Gatekeeper's Assign mutate pods; a refused list is UNKNOWN;
#   - each parameter object an admission policy binding names (paramRef), of the policy's paramKind:
#     create, and update and patch by its name (unnamed for a paramRef.selector), in the binding's
#     namespace, in each guarded namespace for a namespaced kind whose paramRef names none (the
#     admitted object's namespace is read then), or cluster-wide; a parameter kind the discovery
#     does not resolve, or a refused list, is UNKNOWN. Reads $WORK/admission-resources, which
#     admission_rights writes;
#   - on the operator's cluster (`operator`), GKE Autopilot's workload allowlists: each installed
#     WorkloadAllowlist admits the privileged workloads it names, which Autopilot otherwise
#     refuses, so it is UNKNOWN (a refused list too), and create, update and patch of
#     AllowlistSynchronizers, which install them, are asked cluster-wide;
#   - Backup for GKE (a CRD of gkebackup.gke.io): a restore, chosen in Google Cloud by a holder of
#     its restore rights, recreates a backup's workloads and RBAC in the cluster, so it is UNKNOWN.
admission_power() {
  local kube="$1" step="$2" guarded id url policies allowlist sns sname why engines
  POWER_RIGHTS='[]'
  guarded="$(printf '%s\n' "${GUARDED[@]%%/*}" | jq -R -s -c 'split("\n") | map(select(length > 0)) | unique')"
  # The cluster-scoped resources, so a webhook rule naming a resource that is namespaced in one group
  # it names and cluster-scoped in another (a CRD of the same plural) is judged. kubectl prints what
  # it read before an error, so a partial list is still used, and the error is UNKNOWN.
  kubectl --kubeconfig "$kube" api-resources --namespaced=false -o name >"$WORK/cluster-resources" 2>"$WORK/cluster-resources.err" ||
    unknown "$step" "the API server's discovery of cluster-scoped resources could not be read whole, so a webhook rule on a resource it leaves out is judged as that resource's namespaced name alone: $(first_line "$WORK/cluster-resources.err")"
  if ! kubectl --kubeconfig "$kube" get mutatingwebhookconfigurations,validatingwebhookconfigurations -o json >"$WORK/webhooks.json" 2>"$WORK/webhooks.err"; then
    unknown "$step" "the API server refused the list of webhook configurations (it needs list on mutatingwebhookconfigurations and validatingwebhookconfigurations, container.mutatingWebhookConfigurations.list and container.validatingWebhookConfigurations.list on GKE), so no webhook's backend is reviewed: $(first_line "$WORK/webhooks.err")"
  elif ! is_list "$WORK/webhooks.json" "$WORK/webhooks.err" ||
    ! jq -c -L "$LIB" --argjson guarded "$guarded" --argjson table "$TABLE_JSON" --rawfile names "$WORK/api-resources" \
      --rawfile cluster_names "$WORK/cluster-resources" -f "$LIB/webhook-backends.jq" "$WORK/webhooks.json" >"$WORK/webhooks.out" 2>"$WORK/webhooks.err"; then
    unknown "$step" "the API server's list of webhook configurations could not be read, so no webhook's backend is reviewed: $(first_line "$WORK/webhooks.err")"
  else
    POWER_RIGHTS="$(jq -c .rights "$WORK/webhooks.out")"
    while IFS=$'\t' read -r id url; do
      unknown "$step" "the webhook $id can match a guarded namespace or a cluster-scoped object and calls $url, outside the cluster, whose holders this check cannot read"
    done < <(jq -r '.urls[] | [.id, .url] | @tsv' "$WORK/webhooks.out")
    # The Service each such webhook calls: an ExternalName, or one with no selector, whose endpoints
    # are whatever someone wrote, can route admission calls outside what this check reads.
    while IFS=$'\t' read -r -u 7 id sns sname; do
      if ! kubectl --kubeconfig "$kube" get service -n "$sns" "$sname" -o json >"$WORK/service.json" 2>"$WORK/service.err"; then
        unknown "$step" "the webhook $id calls the Service $sns/$sname, which could not be read (it needs get on services there, container.services.get on GKE): $(first_line "$WORK/service.err")"
        continue
      fi
      if ! is_object "$WORK/service.json" "$WORK/service.err"; then
        unknown "$step" "the webhook $id calls the Service $sns/$sname, whose answer is not one JSON object ($(first_line "$WORK/service.err")), so where it sends admission calls is not read"
        continue
      fi
      # is_object passed, so jq fails here on a Service whose .spec, .spec.type or .spec.selector is
      # of a type it cannot read ({"spec": {"selector": true}}: "true has no length").
      if ! why="$(jq -r 'if .spec.type == "ExternalName" then "is of type ExternalName (\(.spec.externalName // "")), so admission calls leave the cluster"
        elif ((.spec.selector // {}) | length) == 0 then "has no selector, so its endpoints can point anywhere, inside the cluster or out"
        else "" end' "$WORK/service.json" 2>"$WORK/service.err")"; then
        unknown "$step" "the webhook $id calls the Service $sns/$sname, whose spec could not be read ($(first_line "$WORK/service.err")), so where it sends admission calls is not read"
        continue
      fi
      [ -z "$why" ] || unknown "$step" "the webhook $id calls the Service $sns/$sname, which $why, whose holders this check cannot read"
    done 7< <(jq -r '.services | unique_by([.ns, .name])[] | [.id, .ns, .name] | @tsv' "$WORK/webhooks.out")
  fi
  if ! kubectl --kubeconfig "$kube" get customresourcedefinitions -o json >"$WORK/crds.json" 2>"$WORK/crds.err"; then
    unknown "$step" "the API server refused the list of custom resource definitions (it needs list on customresourcedefinitions, container.customResourceDefinitions.list on GKE), so a policy engine's cluster-scoped resources are not reviewed: $(first_line "$WORK/crds.err")"
  elif ! is_list "$WORK/crds.json" "$WORK/crds.err" || ! engines="$(jq -c '
      def engine: . as $g | any("kyverno.io", "gatekeeper.sh", "kubewarden.io"; . as $e | $g == $e or ($g | endswith("." + $e)))
        or test("polic(y|ies)"; "i");
      [.items[] | select(.spec.scope == "Cluster") | .spec.group as $g | select($g | engine)
      | .spec.names.plural as $p | ("create", "update", "patch")
      | {verb: ., group: $g, resource: $p, sub: "", ns: "", name: "", label: "\(.) \($p).\($g) cluster-wide, a policy engine'"'"'s resource"}
      | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}]' "$WORK/crds.json" 2>"$WORK/crds.err")"; then
    unknown "$step" "the API server's list of custom resource definitions could not be read, so a policy engine's cluster-scoped resources are not reviewed: $(first_line "$WORK/crds.err")"
  else
    POWER_RIGHTS="$(with_rights "$POWER_RIGHTS" "$engines")"
    ! jq -e 'any(.items[]; .spec.group == "gkebackup.gke.io")' "$WORK/crds.json" >/dev/null ||
      unknown "$step" "Backup for GKE is installed (the gkebackup.gke.io API), so a restore can recreate a backup's workloads and RBAC here, chosen in Google Cloud by a holder of its restore rights, which this check does not read"
    if [ "${3:-}" = operator ] && jq -e 'any(.items[]; .spec.group == "auto.gke.io" and .spec.names.plural == "workloadallowlists")' "$WORK/crds.json" >/dev/null; then
      POWER_RIGHTS="$(with_rights "$POWER_RIGHTS" "$(jq -n -c '[("create", "update", "patch")
        | {verb: ., group: "auto.gke.io", resource: "allowlistsynchronizers", sub: "", ns: "", name: "",
           label: "\(.) allowlistsynchronizers.auto.gke.io cluster-wide, which install the workload allowlists Autopilot admits privileged pods by"}
        | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}]')")"
      if ! kubectl --kubeconfig "$kube" get workloadallowlists.auto.gke.io -o json >"$WORK/allowlists.json" 2>"$WORK/allowlists.err"; then
        unknown "$step" "the API server refused the list of workload allowlists (it needs list on workloadallowlists.auto.gke.io), so whether Autopilot admits a privileged pod here is unknown: $(first_line "$WORK/allowlists.err")"
      elif ! is_list "$WORK/allowlists.json" "$WORK/allowlists.err" ||
        ! jq -r '.items[].metadata.name' "$WORK/allowlists.json" >"$WORK/allowlists.txt" 2>"$WORK/allowlists.err"; then
        # Read before the loop: an answer that is not a list would loop over nothing and read as
        # none installed (is_list says why an empty one passes jq).
        unknown "$step" "the API server's list of workload allowlists could not be read, so whether Autopilot admits a privileged pod here is unknown: $(first_line "$WORK/allowlists.err")"
      else
        while IFS= read -r allowlist; do
          unknown "$step" "the workload allowlist $allowlist is installed, so Autopilot admits the privileged workloads it names, which this check does not read"
        done <"$WORK/allowlists.txt"
      fi
    fi
  fi
  policies="$(sed -n 's/^\([a-z]*admissionpolic\(y\|ies\)\(bindings\)\?\)\.admissionregistration\.k8s\.io$/\1/p' "$WORK/admission-resources" 2>/dev/null |
    sort -u | paste -sd, -)"
  [ -z "$policies" ] || param_rights "$kube" "$step" "$policies" "$guarded"
}

# param_rights KUBECONFIG STEP RESOURCES GUARDED: adds to POWER_RIGHTS the writes to each parameter
# object an admission policy binding of RESOURCES (the admission policies and their bindings the
# discovery lists, comma-separated) names, as admission_power says; GUARDED is the JSON array of the
# guarded namespaces.
param_rights() {
  local policy kind api resource namespaced pns pname path namespaces more
  if ! kubectl --kubeconfig "$1" get "$3" -o json >"$WORK/policies.json" 2>"$WORK/policies.err"; then
    unknown "$2" "the API server refused the list of admission policies and their bindings (it needs list on $3), so a policy's parameter objects are not reviewed: $(first_line "$WORK/policies.err")"
    return
  fi
  # The unit separator, not a tab: read merges a run of tabs, and the namespace or name may be empty.
  # The bindings are read before the loop: an answer that is not a list would loop over nothing and
  # read as no parameter to review (is_list says why an empty one passes jq).
  if ! is_list "$WORK/policies.json" "$WORK/policies.err" || ! jq -r '.items as $all
    | $all[] | select(.kind | endswith("PolicyBinding")) | . as $b
    | first($all[] | select((.kind + "Binding") == $b.kind and .metadata.name == $b.spec.policyName)) as $p
    | select($p.spec.paramKind != null and $b.spec.paramRef != null)
    | ["\($p.kind) \($p.metadata.name)", $p.spec.paramKind.apiVersion, $p.spec.paramKind.kind,
       ($b.spec.paramRef.namespace // ""), ($b.spec.paramRef.name // "")] | join("\u001f")' "$WORK/policies.json" \
    >"$WORK/policies.rec" 2>"$WORK/policies.err"; then
    unknown "$2" "the API server's list of admission policies and their bindings could not be read, so a policy's parameter objects are not reviewed: $(first_line "$WORK/policies.err")"
    return
  fi
  while IFS="$US" read -r policy api kind pns pname; do
    case "$api" in */*) path="/apis/$api" ;; *) path="/api/$api" ;; esac
    if ! kubectl --kubeconfig "$1" get --raw "$path" >"$WORK/raw.json" 2>"$WORK/raw.err" ||
      ! IFS=$'\t' read -r resource namespaced < <(jq -r --arg k "$kind" 'first(.resources[] | select(.kind == $k and (.name | contains("/") | not))) | [.name, .namespaced] | @tsv' "$WORK/raw.json" 2>/dev/null); then
      unknown "$2" "the parameter kind $kind ($api) of the admission policy $policy is not resolved by the API server's discovery, so who may write its parameters is not reviewed"
      continue
    fi
    case "$api" in */*) resource="$resource.${api%/*}" ;; esac
    # A namespaced kind whose paramRef names no namespace is read from the admitted object's own
    # namespace, so each guarded namespace's is asked; a cluster-scoped kind, cluster-wide.
    if [ "$namespaced" != true ]; then namespaces='[""]'; elif [ -n "$pns" ]; then namespaces="$(jq -n -c --arg n "$pns" '[$n]')"; else namespaces="$4"; fi
    # Each policy's writes are built on their own, so one that cannot be built reads UNKNOWN naming
    # the policy and leaves the others; with_rights joins them, as it joins every rights value.
    if ! more="$(jq -n -c --arg res "$resource" --argjson namespaces "$namespaces" --arg name "$pname" --arg policy "$policy" '
      ($res | split(".")) as $parts | ($parts[0]) as $r | ($parts[1:] | join(".")) as $g
      | [$namespaces[] as $ns
         # Update and patch by the name a paramRef gives, or unnamed for a paramRef.selector.
         | ({verb: "create", name: ""}, {verb: ("update", "patch"), name: $name})
         | {verb, group: $g, resource: $r, sub: "", ns: $ns, name,
            label: "\(.verb) \($res)\(if .name != "" then " " + .name else "" end)\(if $ns != "" then " in " + $ns else " cluster-wide" end), a parameter of the admission policy \($policy)"}
         | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}]' 2>"$WORK/param.err")"; then
      unknown "$2" "the writes to the parameters of the admission policy $policy could not be built, so who may write them is not reviewed: $(first_line "$WORK/param.err")"
      continue
    fi
    POWER_RIGHTS="$(with_rights "$POWER_RIGHTS" "$more")"
  done <"$WORK/policies.rec"
}
