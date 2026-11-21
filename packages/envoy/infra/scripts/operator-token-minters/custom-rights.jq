# operator-token-minters/custom-rights.jq: custom_rights's rights (reviews.sh), read from the guarded
# pairs a line (namespace/name), with $names the discovery's namespaced, creatable resources and
# $style `guard` for GUARD_RIGHTS's labels: create on each custom resource in each guarded namespace.
include "custom";
    [split("\n")[] | select(length > 0) | split("/") | {ns: .[0], sa: .[1]}] as $pairs
    | custom_resources($names) as $custom
    | [([$pairs[].ns] | unique)[] as $n
       | ([$pairs[] | select(.ns == $n) | "\(.ns)/\(.sa)"] | join(", ")) as $guarding
       | $custom[]
       | {verb: "create", group, resource, sub: "", ns: $n, name: "",
          label: ("create \(.resource).\(.group) in \($n)" + (if $style == "guard" then ", guarding \($guarding)" else "" end))}
       | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}]
