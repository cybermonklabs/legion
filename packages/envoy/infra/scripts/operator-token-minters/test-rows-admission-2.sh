# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-admission-2.sh: the rows of operator-token-minters.test.sh on
# admission power outside the admission objects: a webhook's backend. Sourced by the test after its
# world and helpers; its header says what the fakes model. The rows:
#
#   (ce) a mutating webhook whose selector can match a guarded namespace, backed by the Service
#        hooks/hook: apps/deployer that may patch deployments in hooks, or create jobs (rv16),
#        cronjobs or replicationcontrollers, create pods/exec or pods/attach, or patch
#        pods/ephemeralcontainers there (N2), exits 1 naming the webhook, on the operator's cluster or
#        another; the same webhook whose namespaceSelector excludes the guarded namespaces by name
#        exits 0; a webhook that calls a URL, and a refused list of webhook configurations, read
#        UNKNOWN; apps/deployer that may update configmaps in hooks (rv17), the injector's template,
#        exits 1; and a backend Service of type ExternalName, or with no selector, reads UNKNOWN
#        (P2-4); a backend Service whose `.spec` jq cannot read ({"spec": {"selector": true}}) reads
#        UNKNOWN naming it; a webhook backed in dispatch whose selector names only legion, with a rule that can
#        match a cluster-scoped object (clusterrolebindings, `*`, or secrets in groups "" and
#        example.io where the cluster serves a cluster-scoped secrets.example.io), exits 1 naming the
#        webhook, and with rules on namespaced objects alone exits 0; a cluster-scoped discovery that
#        cannot be read whole reads UNKNOWN.

# --- (ce) A webhook's backend: whoever may write its Service, the workload behind it, its endpoints
# or its TLS Secret decides what the webhook does to the operator's next pod. A webhook whose rules
# can match a cluster-scoped object is judged whatever its namespaceSelector says, since the API
# server applies none to an object with no namespace: one backed in dispatch whose selector names
# only legion, with a rule on clusterrolebindings, on `*`, or on secrets in the groups "" and
# example.io where the cluster serves a cluster-scoped secrets.example.io, is counted; the same
# selector with a rule on pods, or on `*` of scope Namespaced, is not. Run by hand, the control for
# the cluster items is webhook-backends.jq's can_match as at bfb6ea5c85, which read only the
# selector: each exits 0; and for the crd-rule item, cluster_rule without its $cluster test, which
# counted secrets namespaced because the core group serves it so: it exits 0. ---
backend_row() {
  local service='{"service": {"namespace": "hooks", "name": "hook"}}' want
  base_world
  case "$1" in
    backend)
      hook_config "" "$service" >"$world/clusters/operator-cluster/webhooks.json"
      subject_grant operator-cluster "$(sa_subject apps deployer)" patch apps deployments "" hooks ""
      run
      want_status 1 "apps/deployer may patch the webhook backend's Deployment"
      want_line "FINDING system:serviceaccount:apps:deployer: patch deployments.apps in hooks, backing the webhook MutatingWebhookConfiguration hook-config/pods.example.com" \
        "apps/deployer may patch the webhook backend's Deployment"
      ;;
    other-cluster)
      add_cluster other-cluster us-east1
      add_sa other-cluster ci runner
      hook_config "" "$service" >"$world/clusters/other-cluster/webhooks.json"
      subject_grant other-cluster "$(sa_subject ci runner)" update "" services "" hooks ""
      run
      want_status 1 "ci/runner may update the webhook backend's Service on another cluster"
      want_line "FINDING system:serviceaccount:ci:runner: update services in hooks, backing the webhook MutatingWebhookConfiguration hook-config/pods.example.com on cluster $project/us-east1/other-cluster" \
        "ci/runner may update the webhook backend's Service on another cluster"
      ;;
    jobs | cronjobs | replicationcontrollers | exec | attach | ephemeralcontainers)
      # rv16 and N2: each pod-making or pod-entry right RIGHTS_TABLE asks a guarded namespace.
      local grant right verb group resource sub
      case "$1" in
        jobs) grant="create|batch|jobs|" right="create jobs.batch" ;;
        cronjobs) grant="create|batch|cronjobs|" right="create cronjobs.batch" ;;
        replicationcontrollers) grant="create||replicationcontrollers|" right="create replicationcontrollers" ;;
        exec) grant="create||pods|exec" right="create pods/exec" ;;
        attach) grant="create||pods|attach" right="create pods/attach" ;;
        ephemeralcontainers) grant="patch||pods|ephemeralcontainers" right="patch pods/ephemeralcontainers" ;;
      esac
      IFS='|' read -r verb group resource sub <<<"$grant"
      hook_config "" "$service" >"$world/clusters/operator-cluster/webhooks.json"
      subject_grant operator-cluster "$(sa_subject apps deployer)" "$verb" "$group" "$resource" "$sub" hooks ""
      run
      want_status 1 "apps/deployer may $right in the webhook backend's namespace"
      want_line "FINDING system:serviceaccount:apps:deployer: $right in hooks, backing the webhook MutatingWebhookConfiguration hook-config/pods.example.com" \
        "apps/deployer may $right in the webhook backend's namespace"
      ;;
    excluded)
      hook_config exclude "$service" >"$world/clusters/operator-cluster/webhooks.json"
      subject_grant operator-cluster "$(sa_subject apps deployer)" patch apps deployments "" hooks ""
      run
      want_status 0 "a webhook whose selector excludes the guarded namespaces"
      ;;
    cluster-rule | star-rule | crd-rule | namespaced-rule | star-namespaced)
      case "$1" in
        cluster-rule) rules='[{operations: ["CREATE", "UPDATE"], apiGroups: ["rbac.authorization.k8s.io"], apiVersions: ["v1"], resources: ["clusterrolebindings"]}]' ;;
        star-rule) rules='[{operations: ["*"], apiGroups: ["*"], apiVersions: ["*"], resources: ["*"]}]' ;;
        crd-rule)
          rules='[{operations: ["CREATE"], apiGroups: ["", "example.io"], apiVersions: ["*"], resources: ["secrets"]}]'
          echo secrets.example.io >"$world/clusters/operator-cluster/cluster-resources"
          ;;
        namespaced-rule) rules='[{operations: ["CREATE"], apiGroups: [""], apiVersions: ["v1"], resources: ["pods"]}]' ;;
        star-namespaced) rules='[{operations: ["*"], apiGroups: ["*"], apiVersions: ["*"], resources: ["*"], scope: "Namespaced"}]' ;;
      esac
      hook_config legion '{"service": {"namespace": "dispatch", "name": "hook"}}' "$(jq -n -c "$rules")" >"$world/clusters/operator-cluster/webhooks.json"
      subject_grant operator-cluster "$(sa_subject apps deployer)" patch apps deployments "" dispatch ""
      run
      case "$1" in
        cluster-rule | star-rule | crd-rule)
          want_status 1 "a webhook backed in dispatch whose selector names only legion, with a rule that can match a cluster-scoped object ($1)"
          want_line "FINDING system:serviceaccount:apps:deployer: patch deployments.apps in dispatch, backing the webhook MutatingWebhookConfiguration hook-config/pods.example.com" \
            "a webhook backed in dispatch whose selector names only legion, with a rule that can match a cluster-scoped object ($1)"
          ;;
        *)
          want_status 0 "a webhook backed in dispatch whose selector names only legion, with rules on namespaced objects alone ($1)"
          ;;
      esac
      ;;
    url)
      hook_config "" '{"url": "https://hooks.example.com/mutate"}' >"$world/clusters/operator-cluster/webhooks.json"
      run
      want_status 2 "a webhook that calls a URL"
      want_line "UNKNOWN live: RBAC on operator-cluster: the webhook MutatingWebhookConfiguration hook-config/pods.example.com can match a guarded namespace or a cluster-scoped object and calls https://hooks.example.com/mutate, outside the cluster, whose holders this check cannot read" \
        "a webhook that calls a URL"
      ;;
    configmaps)
      hook_config "" "$service" >"$world/clusters/operator-cluster/webhooks.json"
      subject_grant operator-cluster "$(sa_subject apps deployer)" update "" configmaps "" hooks ""
      run
      want_status 1 "apps/deployer may update configmaps in the webhook backend's namespace (rv17)"
      want_line "FINDING system:serviceaccount:apps:deployer: update configmaps in hooks, backing the webhook MutatingWebhookConfiguration hook-config/pods.example.com" \
        "apps/deployer may update configmaps in the webhook backend's namespace (rv17)"
      ;;
    external-name | no-selector)
      hook_config "" "$service" >"$world/clusters/operator-cluster/webhooks.json"
      if [ "$1" = external-name ]; then
        jq -n '{"hooks/hook": {spec: {type: "ExternalName", externalName: "hooks.example.com"}}}' >"$world/clusters/operator-cluster/services.json"
        want="is of type ExternalName (hooks.example.com), so admission calls leave the cluster"
      else
        jq -n '{"hooks/hook": {spec: {type: "ClusterIP"}}}' >"$world/clusters/operator-cluster/services.json"
        want="has no selector, so its endpoints can point anywhere, inside the cluster or out"
      fi
      run
      want_status 2 "a webhook backend Service that $want"
      want_line "UNKNOWN live: RBAC on operator-cluster: the webhook MutatingWebhookConfiguration hook-config/pods.example.com calls the Service hooks/hook, which $want, whose holders this check cannot read" \
        "a webhook backend Service that $want"
      ;;
    spec-unreadable)
      hook_config "" "$service" >"$world/clusters/operator-cluster/webhooks.json"
      # One JSON object, so is_object passes, whose .spec.selector jq cannot take the length of.
      jq -n '{"hooks/hook": {spec: {selector: true}}}' >"$world/clusters/operator-cluster/services.json"
      run
      want_rbac_unknown "the webhook MutatingWebhookConfiguration hook-config/pods.example.com calls the Service hooks/hook, whose spec could not be read (jq: error" \
        "a webhook backend Service whose spec jq cannot read"
      ;;
    refused)
      : >"$world/clusters/operator-cluster/refuse-webhooks"
      run
      want_status 2 "a refused list of webhook configurations"
      want_text "UNKNOWN live: RBAC on operator-cluster: the API server refused the list of webhook configurations" "a refused list of webhook configurations"
      ;;
    refused-cluster-discovery)
      : >"$world/clusters/operator-cluster/refuse-cluster-resources"
      run
      want_rbac_unknown "the API server's discovery of cluster-scoped resources could not be read whole, so a webhook rule on a resource it leaves out is judged as that resource's namespaced name alone: " \
        "a cluster-scoped discovery that cannot be read whole"
      ;;
  esac
}
if row ce; then
  each backend_row backend jobs cronjobs replicationcontrollers exec attach ephemeralcontainers configmaps other-cluster excluded url \
    external-name no-selector spec-unreadable refused cluster-rule star-rule crd-rule namespaced-rule star-namespaced refused-cluster-discovery
  passed "(ce) a write to a matching webhook's backend (a Deployment's patch, N2's jobs, cronjobs, replicationcontrollers, pods/exec, pods/attach and pods/ephemeralcontainers, and rv17's configmaps) exits 1 naming the webhook, on the operator's cluster or another, and so does one whose rule can match a cluster-scoped object whatever its selector; an excluding selector exits 0; a URL webhook, an ExternalName or selector-less Service, one whose spec jq cannot read, a refused list and a cluster-scoped discovery that cannot be read whole read UNKNOWN"
fi
