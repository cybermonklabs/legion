# operator-token-minters/guard-rights.jq: GUARD_RIGHTS (rights.sh), read from the guarded pairs a line
# (namespace/name). Cluster-wide: create namespaces, and the escalation rights by which a holder
# grants itself any of these (impersonate users, groups and serviceaccounts; escalate and bind
# clusterroles; create and patch clusterrolebindings), and create and get nodes/proxy, whose kubelet
# API enters any pod on the node. In each guarded namespace: create serviceaccounts and create pods,
# and bind and escalate roles and create and patch rolebindings; each label there names the pairs
# the namespace holds.
  [split("\n")[] | select(length > 0) | split("/") | {ns: .[0], sa: .[1]}] as $pairs
  | [{verb: "create", group: "", resource: "namespaces", sub: "", ns: "", name: "", label: "create namespaces cluster-wide"}]
    + ([["impersonate", "", "users", ""], ["impersonate", "", "groups", ""], ["impersonate", "", "serviceaccounts", ""],
        ["escalate", "rbac.authorization.k8s.io", "clusterroles", ""], ["bind", "rbac.authorization.k8s.io", "clusterroles", ""],
        ["create", "rbac.authorization.k8s.io", "clusterrolebindings", ""], ["patch", "rbac.authorization.k8s.io", "clusterrolebindings", ""],
        ["create", "", "nodes", "proxy"], ["get", "", "nodes", "proxy"]]
       | map({verb: .[0], group: .[1], resource: .[2], sub: .[3], ns: "", name: "",
              label: "\(.[0]) \(.[2])\(if .[3] != "" then "/" + .[3] else "" end)\(if .[1] != "" then "." + .[1] else "" end) cluster-wide"}))
    + [([$pairs[].ns] | unique)[] as $n
       | ([$pairs[] | select(.ns == $n) | "\(.ns)/\(.sa)"] | join(", ")) as $guarding
       | {verb: "create", group: "", resource: "serviceaccounts", sub: "", ns: $n, name: "", label: "create serviceaccounts in \($n), guarding \($guarding)"},
         {verb: "create", group: "", resource: "pods", sub: "", ns: $n, name: "", label: "create pods in \($n), guarding \($guarding)"},
         (["bind", "roles"], ["escalate", "roles"], ["create", "rolebindings"], ["patch", "rolebindings"]
          | {verb: .[0], group: "rbac.authorization.k8s.io", resource: .[1], sub: "", ns: $n, name: "",
             label: "\(.[0]) \(.[1]).rbac.authorization.k8s.io in \($n), guarding \($guarding)"})]
  | map(. + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))})
