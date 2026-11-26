# operator-token-minters/review-answer.jq: review's answer (reviews.sh). kubectl create prints one
# JSON document for each object it creates, never a List (kubectl 1.35.9's create.go flattens the
# List it is given and prints each object it made), so review reads that output with -s: an array of
# the documents, one SubjectAccessReview for each review asked. Each document must be a
# SubjectAccessReview whose status is an object with a boolean allowed, which a real API server
# always writes (so `{}` or `"allowed": "true"` is no answer), or the answer cannot be read, and the
# error names the first that is not. It gives a record per review of the principal, the right's
# label, allowed, refused or error, the evaluation error, the API server's reason, and, for an
# allowed review, the fence's verdict on the right (broker-fence.jq's fence_fields under $fence,
# the credited fence or null: the policy that refuses its use on the broker's pods, and `residual`
# when its use on the namespace's other pods carries the fence's residual; "" and "" for any other
# review), matched to its question by the subject and the attributes the answer echoes; or else one
# `short` record (the pairs answered, the reviews asked, the documents) unless every principal and
# right pair asked is answered, each once, in as many documents as reviews asked. $principals and
# $rights each come from a file (--slurpfile), and must hold one JSON array (one_array).
include "rec";
include "broker-fence";
one_array($principals; "the principals") as $principals | one_array($rights; "the rights") as $rights
| (($rights | map({(.key): .}) | add) // {}) as $by_key
| (($principals | map({("\(.user)|\(.groups | join(","))"): .principal}) | add) // {}) as $names
| (to_entries
   | map(select((.value | type) != "object" or .value.kind != "SubjectAccessReview" or (.value.status | type) != "object"
               or (.value.status.allowed | type) != "boolean"))
   | first) as $bad
| if $bad != null then error("document \($bad.key + 1) of the answer's \(length) is not a SubjectAccessReview with a status that says allowed true or false") else . end
| [.[]
   | (.spec.resourceAttributes // {}) as $a
   | ([$a.verb, $a.group, $a.resource, $a.subresource, $a.namespace, $a.name] | map(. // "") | join("|")) as $key
   | ("\(.spec.user // "")|\((.spec.groups // []) | join(","))") as $who
   | (if .status.allowed == true then "allowed" elif (.status.evaluationError // "") != "" then "error" else "refused" end) as $verdict
   | {pair: (if ($names | has($who)) and ($by_key | has($key)) then [$names[$who], $key] else null end),
      row: ([($names[$who] // ""), ($by_key[$key].label // ""), $verdict, (.status.evaluationError // ""), (.status.reason // "")]
            + (if $verdict == "allowed" and $by_key[$key] then fence_fields($fence; $by_key[$key]) else ["", ""] end))}] as $answers
| ([$answers[].pair | select(. != null)] | unique | length) as $answered
| (($principals | length) * ($rights | length)) as $asked
| if $answered != $asked or ($answers | length) != $asked then ["short", $answered, $asked, ($answers | length)] | rec
  else $answers[].row | rec end
