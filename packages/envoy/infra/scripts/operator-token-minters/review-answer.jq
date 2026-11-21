# operator-token-minters/review-answer.jq: review's answer (reviews.sh), a record per review of the
# principal, the right's label, allowed, refused or error, the evaluation error and the API server's
# reason, matched to its question by the subject and the attributes the answer echoes; one `short`
# record when the answer holds fewer reviews than were asked.
include "rec";
      ($rights | map({(.key): .label}) | add) as $labels
      | ($principals | map({("\(.user)|\(.groups | join(","))"): .principal}) | add) as $names
      | [(if .kind == "List" then .items[] else . end)
         | (.spec.resourceAttributes // {}) as $a
         | ([$a.verb, $a.group, $a.resource, $a.subresource, $a.namespace, $a.name] | map(. // "") | join("|")) as $key
         | ("\(.spec.user // "")|\((.spec.groups // []) | join(","))") as $who
         | [($names[$who] // ""), ($labels[$key] // ""),
            (if .status.allowed == true then "allowed" elif (.status.evaluationError // "") != "" then "error" else "refused" end),
            (.status.evaluationError // ""), (.status.reason // "")]] as $rows
      | ([$rows[] | select(.[0] != "" and .[1] != "")] | length) as $answered
      | (($principals | length) * ($rights | length)) as $asked
      | if $answered != $asked or ($rows | length) != $asked then ["short", $answered, $asked] | rec else $rows[] | rec end
