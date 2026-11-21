# operator-token-minters/custom-rights.jq: custom_rights's rights (reviews.sh), read from the guarded
# pairs a line (namespace/name), with $names the discovery's namespaced, creatable resources and
# $style `guard` for GUARD_RIGHTS's labels: create on each custom resource in each guarded namespace.
    [split("\n")[] | select(length > 0) | split("/") | {ns: .[0], sa: .[1]}] as $pairs
    | [$names | split("\n")[] | select(contains("."))
       | {resource: (split(".")[0]), group: (split(".")[1:] | join("."))}
       | select(.group | contains("."))
       | select(((.group | endswith(".k8s.io")) | not) or (.group | endswith(".x-k8s.io")))] as $custom
    | [([$pairs[].ns] | unique)[] as $n
       | ([$pairs[] | select(.ns == $n) | "\(.ns)/\(.sa)"] | join(", ")) as $guarding
       | $custom[]
       | {verb: "create", group, resource, sub: "", ns: $n, name: "",
          label: ("create \(.resource).\(.group) in \($n)" + (if $style == "guard" then ", guarding \($guarding)" else "" end))}
       | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}]
