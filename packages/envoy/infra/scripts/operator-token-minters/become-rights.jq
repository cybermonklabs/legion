# operator-token-minters/become-rights.jq: follow_becomes's rights (reviews.sh), read from a
# ServiceAccount a line (namespace, name, the right of its first finding, tab-separated). In each
# such namespace, every namespaced right of RIGHTS_TABLE ($table, each {verb, group, resource, sub,
# named}: workload controllers, pods/exec and attach, ephemeral containers, pod patch, rolebinding
# create, bind and escalate, secrets), and create on each custom resource of $names (custom.jq);
# a right `named` there (a token, impersonate) is asked of each ServiceAccount by its name. As
# rights_json writes rights, each with `targets`, the ServiceAccounts it makes its holder.
include "custom";
[split("\n")[] | select(length > 0) | split("\t") | {ns: .[0], name: .[1], why: .[2]}] as $sas
| def become($list): $list | map("system:serviceaccount:\(.ns):\(.name), which can \(.why)") | join(", or ");
  def right($r; $ns; $name; $list):
    {verb: $r.verb, group: $r.group, resource: $r.resource, sub: $r.sub, ns: $ns, name: $name,
     key: ([$r.verb, $r.group, $r.resource, $r.sub, $ns, $name] | join("|")),
     label: ("\($r.verb) \($r.resource)\(if $r.sub != "" then "/" + $r.sub else "" end)\(if $r.group != "" then "." + $r.group else "" end)"
             + "\(if $name != "" then " " + $name else "" end) in \($ns), so it can become " + become($list)),
     targets: [$list[] | "\(.ns):\(.name)"]};
  ([$table[] | select(.named | not)] + [custom_resources($names)[] | {verb: "create", group, resource, sub: ""}]) as $wide
  | [(([$sas[].ns] | unique)[] as $n | [$sas[] | select(.ns == $n)] as $in | $wide[] | right(.; $n; ""; $in)),
     ($sas[] as $s | $table[] | select(.named) | right(.; $s.ns; $s.name; [$s]))]
