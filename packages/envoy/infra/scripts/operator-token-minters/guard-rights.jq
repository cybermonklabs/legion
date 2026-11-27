# operator-token-minters/guard-rights.jq: GUARD_RIGHTS (rights.sh), read from the guarded pairs a line
# (namespace/name). Cluster-wide: create and patch namespaces (a patch that finds no object creates
# it, by server-side apply), the escalation rights by which a holder grants itself any of these
# (impersonate users, groups and serviceaccounts; escalate and bind clusterroles; create, patch and
# update clusterrolebindings; patch and update clusterroles, which add a rule to a role its holder
# already has), and create and get nodes/proxy, whose kubelet API enters any pod on
# the node. In each guarded namespace: create and patch serviceaccounts and pods, and bind and
# escalate roles, patch and update roles, and create, patch and update rolebindings; each label
# there names the pairs the namespace holds. A rule a role limits by name is read from the
# cluster's own roles (live_named), never asked by name for every principal.
  [split("\n")[] | select(length > 0) | split("/") | {ns: .[0], sa: .[1]}] as $pairs
  | [("create", "patch") | {verb: ., group: "", resource: "namespaces", sub: "", ns: "", name: "", label: "\(.) namespaces cluster-wide"}]
    + ([["impersonate", "", "users", ""], ["impersonate", "", "groups", ""], ["impersonate", "", "serviceaccounts", ""],
        ["escalate", "rbac.authorization.k8s.io", "clusterroles", ""], ["bind", "rbac.authorization.k8s.io", "clusterroles", ""],
        ["create", "rbac.authorization.k8s.io", "clusterrolebindings", ""], ["patch", "rbac.authorization.k8s.io", "clusterrolebindings", ""],
        ["update", "rbac.authorization.k8s.io", "clusterrolebindings", ""],
        ["patch", "rbac.authorization.k8s.io", "clusterroles", ""], ["update", "rbac.authorization.k8s.io", "clusterroles", ""],
        ["create", "", "nodes", "proxy"], ["get", "", "nodes", "proxy"]]
       | map({verb: .[0], group: .[1], resource: .[2], sub: .[3], ns: "", name: "",
              label: "\(.[0]) \(.[2])\(if .[3] != "" then "/" + .[3] else "" end)\(if .[1] != "" then "." + .[1] else "" end) cluster-wide"}))
    + [([$pairs[].ns] | unique)[] as $n
       | ([$pairs[] | select(.ns == $n) | "\(.ns)/\(.sa)"] | join(", ")) as $guarding
       | (("create", "patch") as $verb | ("serviceaccounts", "pods") as $res
          | {verb: $verb, group: "", resource: $res, sub: "", ns: $n, name: "", label: "\($verb) \($res) in \($n), guarding \($guarding)"}),
         (["bind", "roles"], ["escalate", "roles"], ["patch", "roles"], ["update", "roles"],
          ["create", "rolebindings"], ["patch", "rolebindings"], ["update", "rolebindings"]
          | {verb: .[0], group: "rbac.authorization.k8s.io", resource: .[1], sub: "", ns: $n, name: "",
             label: "\(.[0]) \(.[1]).rbac.authorization.k8s.io in \($n), guarding \($guarding)"})]
  | map(. + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))})
