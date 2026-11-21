# operator-token-minters/named-rights.jq: named_rights's rights (reviews.sh), read from a cluster's
# ClusterRoleBindings and RoleBindings. RBAC allows a request against a rule with resourceNames only
# when the request names one of them, so a review that names nothing is refused for every
# name-limited grant: one that may impersonate the group system:masters by name becomes
# cluster-admin while the unnamed review says no. These rights name each name, as rights_json
# writes rights: impersonate groups for system:masters and every Group subject the bindings name;
# impersonate users for every User subject; and escalate, bind, update and patch on each role bound
# to a reviewed principal. $reviewed holds the reviewed principals' labels (the first field of
# principals.rec); a Group subject is reviewed when it is one the API server gives a reviewed
# ServiceAccount or anyone (system:authenticated, system:serviceaccounts and
# system:serviceaccounts:<its namespace>, system:unauthenticated), or one reviewed alone.
def right($verb; $group; $resource; $ns; $name):
  {verb: $verb, group: $group, resource: $resource, sub: "", ns: $ns, name: $name,
   key: ([$verb, $group, $resource, "", $ns, $name] | join("|")),
   label: ("\($verb) \($resource)\(if $group != "" then "." + $group else "" end) \($name)"
           + (if $ns != "" then " in " + $ns else " cluster-wide" end))};
([$reviewed[] | select(startswith("system:serviceaccount:")) | split(":")[2]] | unique) as $sa_namespaces
| def reviewed($s; $bns):
    if $s.kind == "ServiceAccount" then any($reviewed[]; . == "system:serviceaccount:\($s.namespace // $bns):\($s.name)")
    elif $s.kind == "Group" then
      if $s.name == "system:authenticated" or $s.name == "system:serviceaccounts" or $s.name == "system:unauthenticated" then true
      elif ($s.name | startswith("system:serviceaccounts:")) then any($sa_namespaces[]; . == ($s.name | ltrimstr("system:serviceaccounts:")))
      else any($reviewed[]; . == "group:\($s.name)") end
    elif $s.kind == "User" then
      ($s.name | if startswith("system:serviceaccount:") then . elif test("\\.gserviceaccount\\.com$") then "serviceAccount:" + . else "user:" + . end) as $l
      | any($reviewed[]; . == $l)
    else false end;
  [.items[]?] as $bindings
  | ([$bindings[].subjects[]? | select(.kind == "Group") | .name] + ["system:masters"] | unique) as $groups
  | ([$bindings[].subjects[]? | select(.kind == "User") | .name] | unique) as $users
  | [ ($groups[] | right("impersonate"; ""; "groups"; ""; .)),
      ($users[] | right("impersonate"; ""; "users"; ""; .)),
      ($bindings[] | . as $b | (.metadata.namespace // "") as $bns
       | select(any(($b.subjects // [])[]; reviewed(.; $bns)))
       | (if $b.roleRef.kind == "ClusterRole" then ["clusterroles", ""] else ["roles", $bns] end) as [$res, $ns]
       | ("escalate", "bind", "update", "patch") as $verb
       | right($verb; "rbac.authorization.k8s.io"; $res; $ns; $b.roleRef.name)) ]
  | reduce .[] as $r ([]; if any(.[]; .key == $r.key) then . else . + [$r] end)
