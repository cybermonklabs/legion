# operator-token-minters/code-rbac.jq: the step "code: RBAC" (step-code-rbac.sh) reads the renders
# as the RBAC authorizer does. Each binding gives a `grant` record per right of $rights its role
# holds (the binding, the right's label, and the subject's kind, namespace and name), or a `role`
# record when the render does not hold its role (or aggregates it); renders with no binding give
# one `empty` record with the number of objects they hold. A rule limited by resourceNames on
# impersonate or escalate is a grant too, its label naming the names: a review that names nothing
# never matches it, yet impersonating the group system:masters by name is cluster-admin.
include "rec";
        def objs: if type == "array" then .[] | objs
                  elif type == "object" and (.kind // "") == "List" then (.items // [])[] | objs
                  elif type == "object" then . else empty end;
        def has_($list; $x): any(($list // [])[]; . == $x);
        def matches_but_name($r): (has_(.verbs; $r.verb) or has_(.verbs; "*"))
          and (has_(.apiGroups; $r.group) or has_(.apiGroups; "*"))
          and (has_(.resources; "*") or has_(.resources; $r.resource + (if $r.sub != "" then "/" + $r.sub else "" end))
               or ($r.sub != "" and has_(.resources; "*/" + $r.sub)));
        def matches($r): matches_but_name($r)
          and (((.resourceNames // []) | length) == 0 or ($r.name != "" and has_(.resourceNames; $r.name)));
        def named($r): $r.name == "" and ($r.verb == "impersonate" or $r.verb == "escalate")
          and ((.resourceNames // []) | length) > 0 and matches_but_name($r);
        def named_label($r; $names): "\($r.verb) \($r.resource)\(if $r.sub != "" then "/" + $r.sub else "" end)\(if $r.group != "" then "." + $r.group else "" end)"
          + " \($names | join(", "))" + (if $r.ns != "" then " in " + $r.ns else " cluster-wide" end);
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
            | ([$rules[] | select(named($r)) | .resourceNames[]] | unique) as $names
            | if any($rules[]; matches($r)) then (["grant", $id, $r.label] + $subject) | rec
              elif ($names | length) > 0 then (["grant", $id, named_label($r; $names)] + $subject) | rec
              else empty end
            end
          end
