# operator-token-minters/become-rights.jq: follow_becomes's rights (reviews.sh), read from a
# ServiceAccount a line (namespace, name, and the one of its rights found_sa keeps, whose label sorts
# first, tab-separated). In each such namespace, every namespaced right of RIGHTS_TABLE ($table, each
# {verb, group, resource, sub, named}: workload controllers, pods/exec and attach, ephemeral
# containers, pod patch, rolebinding create, bind and escalate, secrets), and create, update and
# patch on each custom resource of $names (custom.jq), as custom-rights.jq asks them in a guarded
# namespace; a right `named` there (a token, impersonate) is asked of each ServiceAccount by its
# name. As rights_json writes rights, each with `targets`, the ServiceAccounts it makes its holder,
# and `why`, the chain its label ends with, which code-rbac.jq adds to the label of a rule limiting
# it by name.
include "custom";
[split("\n")[] | select(length > 0) | split("\t") | {ns: .[0], name: .[1], why: .[2]}] as $sas
| def become($list): $list | map("system:serviceaccount:\(.ns):\(.name), which can \(.why)") | join(", or ");
  def right($r; $ns; $name; $list):
    ("so it can become " + become($list)) as $why
    | {verb: $r.verb, group: $r.group, resource: $r.resource, sub: $r.sub, ns: $ns, name: $name,
       key: ([$r.verb, $r.group, $r.resource, $r.sub, $ns, $name] | join("|")),
       label: ("\($r.verb) \($r.resource)\(if $r.sub != "" then "/" + $r.sub else "" end)\(if $r.group != "" then "." + $r.group else "" end)"
               + "\(if $name != "" then " " + $name else "" end) in \($ns), " + $why),
       why: $why, targets: [$list[] | "\(.ns):\(.name)"]};
  ([$table[] | select(.named | not)]
   + [custom_resources($names)[] | ("create", "update", "patch") as $verb | {verb: $verb, group, resource, sub: ""}]) as $wide
  | [(([$sas[].ns] | unique)[] as $n | [$sas[] | select(.ns == $n)] as $in | $wide[] | right(.; $n; ""; $in)),
     ($sas[] as $s | $table[] | select(.named) | right(.; $s.ns; $s.name; [$s]))]
