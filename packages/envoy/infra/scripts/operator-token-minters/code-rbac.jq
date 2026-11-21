# operator-token-minters/code-rbac.jq: the step "code: RBAC" (step-code-rbac.sh) reads the renders
# as the RBAC authorizer does. Each binding gives a `grant` record per right of $rights its role
# holds (the binding, the right's label, and the subject's kind, namespace and name), or a `role`
# record when the render does not hold its role (or aggregates it); renders with no binding give
# one `empty` record with the number of objects they hold.
include "rec";
        def objs: if type == "array" then .[] | objs
                  elif type == "object" and (.kind // "") == "List" then (.items // [])[] | objs
                  elif type == "object" then . else empty end;
        def has_($list; $x): any(($list // [])[]; . == $x);
        def matches($r): (has_(.verbs; $r.verb) or has_(.verbs; "*"))
          and (has_(.apiGroups; $r.group) or has_(.apiGroups; "*"))
          and (has_(.resources; "*") or has_(.resources; $r.resource + (if $r.sub != "" then "/" + $r.sub else "" end))
               or ($r.sub != "" and has_(.resources; "*/" + $r.sub)))
          and (((.resourceNames // []) | length) == 0 or ($r.name != "" and has_(.resourceNames; $r.name)));
        [inputs | objs] as $objects
        | [$objects[] | select(.kind == "RoleBinding" or .kind == "ClusterRoleBinding")] as $bindings
        | if ($bindings | length) == 0 then ["empty", ($objects | length)] | rec
          else $bindings[] as $b
          | ($b.metadata.namespace // "") as $bns
          | (if $b.kind == "RoleBinding" then "RoleBinding \($bns)/\($b.metadata.name)" else "ClusterRoleBinding \($b.metadata.name)" end) as $id
          | ([$objects[] | select(.kind == ($b.roleRef.kind // "") and .metadata.name == $b.roleRef.name
                                   and (.kind == "ClusterRole" or (.metadata.namespace // "") == $bns))]) as $found
          | (if ($found | length) > 0 then (if $found[0].aggregationRule then null else ($found[0].rules // []) end)
             elif $b.roleRef.kind == "ClusterRole" and $b.roleRef.name == "cluster-admin" then [{verbs: ["*"], apiGroups: ["*"], resources: ["*"]}]
             elif $b.roleRef.kind == "ClusterRole" and $b.roleRef.name == "view" then []
             else null end) as $rules
          | ($b.subjects // [])[] as $s
          | [$s.kind, ($s.namespace // (if $s.kind == "ServiceAccount" then $bns else "" end)), $s.name] as $subject
          | if $rules == null then (["role", $id, "\($b.roleRef.kind) \($b.roleRef.name)"] + $subject) | rec
            else $rights[] as $r
            | select($b.kind == "ClusterRoleBinding" or ($r.ns != "" and $r.ns == $bns))
            | select(any($rules[]; matches($r)))
            | (["grant", $id, $r.label] + $subject) | rec
            end
          end
