# operator-token-minters/guard-rights.jq: GUARD_RIGHTS (rights.sh), read from the guarded pairs a line
# (namespace/name): create namespaces cluster-wide, and in each guarded namespace create
# serviceaccounts and create pods, each label naming the pairs the namespace holds.
  [split("\n")[] | select(length > 0) | split("/") | {ns: .[0], sa: .[1]}] as $pairs
  | [{verb: "create", group: "", resource: "namespaces", sub: "", ns: "", name: "", label: "create namespaces cluster-wide"}]
    + [([$pairs[].ns] | unique)[] as $n
       | ([$pairs[] | select(.ns == $n) | "\(.ns)/\(.sa)"] | join(", ")) as $guarding
       | {verb: "create", group: "", resource: "serviceaccounts", sub: "", ns: $n, name: "", label: "create serviceaccounts in \($n), guarding \($guarding)"},
         {verb: "create", group: "", resource: "pods", sub: "", ns: $n, name: "", label: "create pods in \($n), guarding \($guarding)"}]
  | map(. + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))})
