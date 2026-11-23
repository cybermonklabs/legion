# operator-token-minters/review-request.jq: review's question (reviews.sh), one List of a
# SubjectAccessReview for each principal of $principals ({principal, user, groups}) and each right
# of $rights, as rights_json writes rights.
{apiVersion: "v1", kind: "List", items: [$principals[] as $p | $rights[] | {
      apiVersion: "authorization.k8s.io/v1", kind: "SubjectAccessReview",
      spec: ({resourceAttributes: ({verb, resource} + (if .group != "" then {group} else {} end)
                                   + (if .sub != "" then {subresource: .sub} else {} end)
                                   + (if .ns != "" then {namespace: .ns} else {} end)
                                   + (if .name != "" then {name} else {} end))}
             + (if $p.user != "" then {user: $p.user} else {} end)
             + (if ($p.groups | length) > 0 then {groups: $p.groups} else {} end))}]}
