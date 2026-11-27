# operator-token-minters/review-request.jq: review's question (reviews.sh), one List of a
# SubjectAccessReview for each principal of this chunk of $principals ({principal, user, groups})
# and each right of $rights, as rights_json writes rights. review asks $chunks chunks at once, each
# by its own kubectl call: this writes chunk $chunk, the principals whose place in the array has
# that remainder, so the first chunk always holds the witness, the first principal of every review,
# and every principal is in exactly one chunk whatever $chunks is. $principals and $rights each come
# from a file (--slurpfile), and must hold one JSON array (one_array): a cluster's principals and
# rights are more than a command line's argument may hold.
include "rec";
one_array($principals; "the principals") as $principals | one_array($rights; "the rights") as $rights
| [$principals | to_entries[] | select((.key % $chunks) == $chunk) | .value] as $mine
| {apiVersion: "v1", kind: "List", items: [$mine[] as $p | $rights[] | {
      apiVersion: "authorization.k8s.io/v1", kind: "SubjectAccessReview",
      spec: ({resourceAttributes: ({verb, resource} + (if .group != "" then {group} else {} end)
                                   + (if .sub != "" then {subresource: .sub} else {} end)
                                   + (if .ns != "" then {namespace: .ns} else {} end)
                                   + (if .name != "" then {name} else {} end))}
             + (if $p.user != "" then {user: $p.user} else {} end)
             + (if ($p.groups | length) > 0 then {groups: $p.groups} else {} end))}]}
