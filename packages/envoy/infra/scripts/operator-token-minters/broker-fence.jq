# operator-token-minters/broker-fence.jq: the broker's pod fence (broker-fence.sh says what it is and
# what a credit rests on). A program reads this module with `include "broker-fence";` and jq's -L at
# this directory.
#
# fence_read($contract) reads an array of objects (a render's, or a cluster's admission policies and
# their bindings) against broker-fence.json ($contract): each of its policies and bindings must be
# there once, and its spec, less what the API server fills in by default (fence_canon), must equal
# the contract's or, for one the contract holds by its digest alone, be in the fence's shape (each
# binding binding its own policy Deny to the contract's namespace alone, each policy failing
# closed) and hash to the digest, which broker-fence.sh checks. fence_pods reads the namespace's
# pods for the account's shape. fence_fields($fence; $r) gives a right's verdict: the policy of a
# credited fence that refuses its use on the broker's pods, and whether its use on the namespace's
# other pods carries the fence's residual.
include "rec";

# A field the API server fills in by default reads as one left out: null, [] and {} alike.
def fence_strip: with_entries(select(.value != null and .value != [] and .value != {}));
# fence_selects($ns): whether a label selector picks the namespace $ns by kubernetes.io/metadata.name
# alone, the label the API server sets to a namespace's name and no one can change. A requirement on
# any other label can stop matching when that label changes, so it never selects here.
def fence_selects($ns):
  ((.matchLabels // {}) | to_entries | all(.key == "kubernetes.io/metadata.name" and .value == $ns))
  and ((.matchExpressions // []) | all(.key == "kubernetes.io/metadata.name"
    and ((.operator == "In" and any(.values[]?; . == $ns)) or (.operator == "NotIn" and all(.values[]?; . != $ns))
         or .operator == "Exists")));
# A policy's matchConstraints or a binding's matchResources less its defaults: a rule's scope "*",
# matchPolicy Equivalent and empty selectors. A policy's own namespaceSelector that picks $ns is left
# out too: GKE writes into every admission policy a selector that skips kube-system and its
# gke-managed-* namespaces (read on the dispatch cluster on 2026-11-23), and the binding is what
# holds the fence to $ns.
def fence_match($ns; $policy):
  def rule: (if .scope == "*" then del(.scope) else . end) | fence_strip;
  (. // {})
  | .resourceRules = [(.resourceRules // [])[] | rule]
  | .excludeResourceRules = [(.excludeResourceRules // [])[] | rule]
  | (if .matchPolicy == "Equivalent" then del(.matchPolicy) else . end)
  | (if $policy and .namespaceSelector != null and (.namespaceSelector | fence_selects($ns)) then del(.namespaceSelector) else . end)
  | fence_strip;
# fence_canon($ns): an admission policy's or binding's spec less its defaults; a policy's
# failurePolicy, Fail by default, is written out.
def fence_canon($ns):
  (.spec // {}) as $spec
  | if .kind == "ValidatingAdmissionPolicy" then
      $spec | .failurePolicy = (.failurePolicy // "Fail") | .matchConstraints = (.matchConstraints | fence_match($ns; true))
      | .validations = [(.validations // [])[] | fence_strip] | fence_strip
    else $spec | .matchResources = (.matchResources | fence_match($ns; false)) | fence_strip end;
# fence_shape($kind; $ns; $name): what keeps a canonical policy or binding spec of kind $kind named
# $name from refusing what its rules say, to every request in $ns alone, or "": a policy must fail
# closed and be narrowed by nothing beside its rules (no selector, match condition or parameter); a
# binding must name its policy, deny, select $ns alone by its name, and take no parameter. It
# decides only an object broker-fence.json holds by its digest, whose spec this repository does not
# hold; for every other object it says why a spec that differs from the contract's does.
def fence_shape($kind; $ns; $name):
  if $kind == "ValidatingAdmissionPolicy" then
    ([((.matchConstraints // {}) | keys - ["resourceRules"])[], (keys - ["failurePolicy", "matchConstraints", "validations"])[]]) as $beside
    | if .failurePolicy != "Fail" then "has failurePolicy \(.failurePolicy | tojson), not \"Fail\""
      elif $beside != [] then "is narrowed or widened beside its rules (\($beside | join(", ")))"
      else "" end
  else
    (keys - ["matchResources", "policyName", "validationActions"]) as $beside
    | if .validationActions != ["Deny"] then "binds with validationActions \(.validationActions | tojson), not [\"Deny\"]"
      elif .policyName != $name then "binds the policy \(.policyName | tojson), not its own"
      elif .matchResources != {namespaceSelector: {matchExpressions: [{key: "kubernetes.io/metadata.name", operator: "In", values: [$ns]}]}} then
        "does not select \($ns) alone by its name (matchResources \(.matchResources | tojson))"
      elif $beside != [] then "takes \($beside | join(", "))"
      else "" end
  end;
# fence_read($contract): {state: read, absent or mismatch, objects: [{kind, name, state, why, spec,
# digest, generation, resourceVersion}]}, each object of the contract in its order; absent when none
# of them is there, read when each is there once with the contract's spec (fence_shape, or the keys
# that differ, saying why one is not) or, for one the contract holds by its digest (`digest`, which
# broker-fence.sh compares), with any spec in the fence's shape. Each spec the contract holds
# decides alone: the shape only explains a spec that differs from it.
def fence_read($contract):
  . as $objects
  | [$contract.objects[] as $want
     | [$objects[] | select(.kind == $want.kind and .metadata.name == $want.name)] as $found
     | {kind: $want.kind, name: $want.name}
       + if ($found | length) == 0 then {state: "absent", why: "is not there"}
         elif ($found | length) > 1 then {state: "mismatch", why: "is there \($found | length) times"}
         else ($found[0] | fence_canon($contract.namespace)) as $spec
           | ($spec | fence_shape($want.kind; $contract.namespace; $want.name)) as $shape
           | (if $want.spec == null then $shape
              elif $spec == $want.spec then ""
              elif $shape != "" then $shape
              else [$spec, $want.spec | keys[]] | unique | map(select(. as $k | $spec[$k] != $want.spec[$k]))
                | "differs from the reviewed fence in \(join(", "))" end) as $why
           | {state: (if $why == "" then "read" else "mismatch" end), why: $why, spec: $spec, digest: ($want.digest // ""),
              generation: ($found[0].metadata.generation // ""), resourceVersion: ($found[0].metadata.resourceVersion // "")}
         end]
  | {state: (if all(.[]; .state == "read") then "read" elif all(.[]; .state == "absent") then "absent" else "mismatch" end), objects: .};
# fence_digest: the canonical text broker-fence.sh hashes for the report's declared and live
# versions, from fence_read's answer: each object's kind, name and canonical spec, in the contract's
# order, its keys sorted (jq -S).
def fence_digest: [.objects[] | {kind, name, spec}];
# fence_secrets: the Secrets a pod takes, by a volume (a secret or a projected one) or by an env or
# envFrom of any of its containers, ephemeral ones included; an image pull Secret gives no container
# its data.
def fence_secrets:
  [(.spec.volumes // [])[] | (.secret.secretName // empty), ((.projected.sources // [])[] | .secret.name // empty)]
  + [((.spec.containers // []) + (.spec.initContainers // []) + (.spec.ephemeralContainers // []))[]
     | ((.env // [])[] | .valueFrom.secretKeyRef.name // empty), ((.envFrom // [])[] | .secretRef.name // empty)]
  | unique;
# fence_pods($prefix; $account; $secrets; $declared): a namespace's pod list read for the account's
# shape, as one record: the pods outside the fence's names ($prefix*) that run as $account, take a
# Secret of the broker's, or take a Secret $declared does not name, each written with what it runs
# as and takes; and how many witness pods there are, those named $prefix* that run as $account. The
# broker's Secrets are $secrets (--db-secret and the guarded pairs' token Secrets there) and every
# Secret a witness pod takes; $declared names each Secret of the namespace the Secret renders
# declare, and a Secret no render declares may hold the broker's token, which the check cannot read.
def fence_pods($prefix; $account; $secrets; $declared):
  [objs | {name: .metadata.name, account: (.spec.serviceAccountName // .spec.serviceAccount // "default"), secrets: fence_secrets}] as $pods
  | [$pods[] | select((.name | startswith($prefix)) and .account == $account)] as $witness
  | ($secrets + [$witness[].secrets[]] | unique) as $held
  | [$pods[] | select(.name | startswith($prefix) | not)
     | . + {takes: [.secrets[] | select(. as $s | any($held[]; . == $s))],
            undeclared: [.secrets[] | select(. as $s | any($held[]; . == $s) or any($declared[]; . == $s) | not)]}
     | select(.account == $account or (.takes | length) > 0 or (.undeclared | length) > 0)
     | "\(.name) (runs as \(.account)\(if (.takes | length) > 0 then ", takes \(.takes | join(", "))" else "" end)"
       + "\(if (.undeclared | length) > 0 then ", takes \(.undeclared | join(", ")), which no --secret-render declares" else "" end))"] as $outside
  | [($outside | join("; ")), ($witness | length)] | rec;
# fence_refuses($fence; $r): the policy that refuses $r's use on the broker's pods, $r a right as
# rights_json writes it, while $fence ({namespace, account, direct}) is credited; nothing for any
# other right. Only entry into a pod (exec, attach and port forward, by create or get, which the API
# server admits as CONNECT) in the fence's namespace, which broker-pod-connect refuses on every pod
# named $prefix* with no exception. A change of a pod (patch or update) is never credited:
# broker-pod-update admits a change of any pod's metadata, and a pod not the broker's that is given
# the broker's label app=envoy-broker takes the broker's Services' traffic; nor is an ephemeral
# container: on a pod not the broker's it can take any Secret of the namespace by an env or
# envFrom. A right with `targets` (a become right, become-rights.jq) is credited only when each
# target is the fence's account; one without, only when the fence's account is the one guarded pair
# in its namespace ($fence.direct).
def fence_refuses($fence; $r):
  if $fence == null or ($r.group // "") != "" or $r.resource != "pods" or ($r.ns // "") != $fence.namespace then empty
  elif (if $r.targets then ($r.targets | length) > 0 and all($r.targets[]; . == "\($fence.namespace):\($fence.account)")
        else $fence.direct end) | not then empty
  elif ($r.verb == "create" or $r.verb == "get") and ($r.sub == "exec" or $r.sub == "attach" or $r.sub == "portforward") then "broker-pod-connect"
  else empty end;
# fence_fields($fence; $r): a record's two last fields for the right $r: the policy fence_refuses
# names, or ""; and `residual` for a credited right without `targets`, whose use on the namespace's
# other pods the fence does not refuse (broker-fence.json's residual, which credit reads), or "". A
# become right's use there is the same grant's as the right without targets, which carries it.
def fence_fields($fence; $r):
  ([fence_refuses($fence; $r)] | first // "") as $policy
  | [$policy, (if $policy != "" and ($r.targets | not) then "residual" else "" end)];
