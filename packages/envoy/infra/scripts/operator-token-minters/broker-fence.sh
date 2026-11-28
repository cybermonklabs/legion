# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/broker-fence.sh: the broker's pod fence, the one admission refusal this check
# credits. Sourced by operator-token-minters.sh, whose header says what each step checks.
#
# RBAC answers whether a principal may make a request; admission can still refuse it. The program
# that declares the cluster's RBAC and admission policies (the one --rbac-render renders) declares
# five ValidatingAdmissionPolicies for the broker's pods in dispatch: broker-pod-connect refuses
# exec, attach and port forward to a pod named envoy-broker-*, broker-pod-update refuses any change
# but metadata to a pod of the broker's and any change of its ephemeral containers,
# broker-pod-create admits a pod running as envoy-broker from the broker's ReplicaSets alone, named
# envoy-broker-*, and broker-workload-create and -update hold who may name the account in a
# workload; each bound Deny with failurePolicy Fail, in dispatch alone, with no exception for any
# operator. broker-fence.json names that fence (its namespace, account and pod prefix) and holds each
# of its five policies and five bindings as reviewed, in canonical form (fence_canon, broker-fence.jq):
# the spec itself, or, for broker-workload-create's and -update's policies, whose rules name a
# private account, the sha256 of the spec (fence_spec_digest). Each was read from the program's
# committed render and from the dispatch cluster on 2026-11-23, which agree. The contract also states
# the residual below.
#
# The fence is credited when its pair, dispatch/envoy-broker, is a guarded pair (--broker-workload
# or --guarded), and only when every part of what follows holds:
#   - the renders declare it as reviewed and the operator's cluster holds it as reviewed: each
#     policy and binding once, its spec, less what the API server fills in by default, the
#     contract's, or, for one the contract holds by its digest, in the fence's shape (bound Deny,
#     failurePolicy Fail, to dispatch alone) and hashing to the digest; so the two agree;
#   - the cluster's pods in dispatch keep the account's shape (fence_pods): at least one pod named
#     envoy-broker-* runs as envoy-broker (a list that saw nothing proves nothing), and no pod named
#     otherwise runs as envoy-broker or takes a Secret of the broker's: --db-secret, a token Secret of
#     the namespace, or any Secret a pod named envoy-broker-* that runs as envoy-broker takes (its
#     NATS seeds and hold keys among them). A pod made before the fence keeps any name;
#   - no webhook of the cluster's configurations calls a Service in dispatch, whatever its selectors
#     and rules (admission_power's WEBHOOK_SERVICES): whoever enters a pod there that backs it
#     changes what the webhook does to each object it admits, and the fence does not refuse that
#     entry. The webhooks whose objects this check judges (webhook-backends.jq) are fewer; the
#     fence's credit stands on none being backed in dispatch at all.
# A fence the renders or the cluster lack, or hold otherwise (a binding of Warn or Audit, a policy of
# failurePolicy Ignore, a selector that misses dispatch, an exception added), or beside a pod or a
# webhook backend as above, credits nothing: what it would refuse stays a finding and the step prints
# NO CREDIT saying why; a fence, a pod list or a webhook list that cannot be read, or that is not one
# list (is_list, reviews.sh), credits nothing, and its read is UNKNOWN.
#
# A credited fence turns into a credit only a grant whose use on the broker's pods it refuses
# (fence_refuses): entry into a pod of dispatch (create and get on pods/exec, attach and
# portforward), outright or limited by name, live or declared, and in who can become envoy-broker by
# it. Each such grant prints a CREDIT line and goes into --report's `credited`, the grant kept as
# evidence. Its use on dispatch's other pods the fence does not refuse: those pods hold none of the
# broker's Secrets and back no webhook, but they are Dispatch's own, whose database login can write
# the writer hold's tables. That residual is broker-fence.json's, tracked by INFRA-4604: each
# credited grant that reaches those pods (one without `targets`) also prints an ALLOWANCE line naming
# it (residual_allowance) and goes into --report's `residuals`, which holds the residual's ALLOWANCE
# lines alone (`allowances` holds the run's flags). Who can become such a grant's holder reaches the
# residual too: a ServiceAccount is followed (RESIDUAL_SAS, reviews.sh) and a Google service
# account's minters are read (step-live-iam.sh), to a fixed point, each chain to it an ALLOWANCE
# naming the chain and the residual; a chain that reaches an account with a finding, or that gives
# its holder more than the residual, is a finding. In the guarded rights a
# grant is credited only when the broker's pair is the one guarded pair in dispatch: entry into
# another pod there reaches the other pair. Everything else stays a finding: a change of a pod in
# dispatch (patch and update of pods: broker-pod-update admits a change of any pod's metadata, and a
# pod given the broker's label app=envoy-broker takes the broker's Services' traffic), an ephemeral
# container there (on a pod not the broker's it can take any Secret of the namespace), create pods
# there (so can a new pod), every workload write, the token, impersonation, Secrets, the bindings,
# nodes/proxy, every Google IAM step and the hold writers. So does whoever can write the fence: its
# policies' and bindings' create, update and patch are among the admission rights, and their delete
# and deletecollection are asked beside them once the fence is credited (FENCE_WRITE_RIGHTS).

FENCE_CONTRACT="$(jq -c . "$LIB/broker-fence.json")"
IFS=$'\t' read -r FENCE_NS FENCE_ACCOUNT FENCE_PREFIX < <(jq -r '[.namespace, .account, .prefix] | @tsv' <<<"$FENCE_CONTRACT")
FENCE_PAIR="$FENCE_NS/$FENCE_ACCOUNT"
FENCE_POLICIES="$(jq -r '[.objects[] | select(.kind == "ValidatingAdmissionPolicy") | .name] | join(", ")' <<<"$FENCE_CONTRACT")"
# FENCE_GUARDED: whether the fence's pair is guarded, without which it is never read. FENCE_DIRECT:
# whether it is the one guarded pair in its namespace (fence_refuses).
FENCE_GUARDED=false FENCE_DIRECT=true
for pair in "${GUARDED[@]}"; do
  if [ "$pair" = "$FENCE_PAIR" ]; then
    FENCE_GUARDED=true
  elif [ "${pair%%/*}" = "$FENCE_NS" ]; then
    FENCE_DIRECT=false
  fi
done
# FENCE_DECLARED: the renders' fence's digest when they declare it as reviewed, else empty, with
# FENCE_DECLARED_WHY saying why. FENCE_CREDITED: the operator's cluster's credited fence, the fence
# fence_json writes with what credit and residual_allowance report beside it (the declared and live
# digests and each policy's and binding's versions), else empty. The reviews take a fence by their
# argument, which decides what they credit, and only the operator's cluster's are given it
# (step-rbac.sh), so no other cluster's credits a grant; credit and residual_allowance then read
# the versions and the residual from FENCE_CREDITED, the one fence a run credits. FENCE_LIVE_READ:
# whether the live step read the cluster's fence at all.
FENCE_DECLARED="" FENCE_DECLARED_WHY="the renders were not read" FENCE_CREDITED="" FENCE_LIVE_READ=false
# CODE_PENDING: the code step's grants whose use on the broker's pods the declared fence refuses,
# held until the live step reads the cluster's fence (fence_settle); CODE_PASS: the code step's PASS
# sentence while it waits on them.
CODE_PENDING=() CODE_PASS=""
FENCE_WRITE_RIGHTS="$(jq -n -c '[("validatingadmissionpolicies", "validatingadmissionpolicybindings") as $res
  | ("delete", "deletecollection")
  | {verb: ., group: "admissionregistration.k8s.io", resource: $res, sub: "", ns: "", name: "", why: "which removes the broker'"'"'s pod fence",
     label: "\(.) \($res).admissionregistration.k8s.io cluster-wide, which removes the broker'"'"'s pod fence"}
  | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}]')"

# no_credit STEP WHY: the fence is not credited, so what it would refuse stays a finding; printed once.
no_credit() {
  local line="NO CREDIT $1: the broker's pod fence ($FENCE_POLICIES in $FENCE_NS) is not credited, so what it would refuse stays a finding: ${2//[$'\t\r\n']/ }"
  [ -z "${RECORDED[N$line]:-}" ] || return 0
  RECORDED[N$line]=1
  echo "$line"
}
# fence_json: the fence fence_fields reads ({namespace, account, direct}).
fence_json() { jq -c --argjson direct "$FENCE_DIRECT" '{namespace, account, direct: $direct}' <<<"$FENCE_CONTRACT"; }
# fence_spec_digest FILE KIND NAME: the sha256 of the canonical spec of fence_read's object KIND NAME
# in FILE, its keys sorted (jq -S), as broker-fence.json writes a digest.
fence_spec_digest() {
  echo "sha256:$(jq -S -c --arg kind "$2" --arg name "$3" '.objects[] | select(.kind == $kind and .name == $name) | .spec' "$1" |
    sha256sum | cut -d' ' -f1)"
}
# fence_verdict FILE WHERE: fence_read's answer in FILE judged; sets FENCE_DIGEST, the sha256 of the
# canonical policies and bindings (fence_digest, the version the report names), when they are the
# reviewed fence, each object the contract holds by a digest hashing to it, else FENCE_WHY, naming
# WHERE.
fence_verdict() {
  local kind name want digest why=""
  FENCE_DIGEST="" FENCE_WHY=""
  case "$(jq -r .state "$1")" in
    read)
      while IFS=$'\t' read -r kind name want; do
        digest="$(fence_spec_digest "$1" "$kind" "$name")"
        [ "$digest" = "$want" ] || why="${why:+$why; }the $kind $name's rules hash to $digest, not to the reviewed $want"
      done < <(jq -r '.objects[] | select(.digest != "") | [.kind, .name, .digest] | @tsv' "$1")
      if [ -n "$why" ]; then
        FENCE_WHY="in $2, $why"
      else
        FENCE_DIGEST="sha256:$(jq -c -L "$LIB" 'include "broker-fence"; fence_digest' "$1" | jq -S -c . | sha256sum | cut -d' ' -f1)"
      fi
      ;;
    absent) FENCE_WHY="$2 hold none of its policies and bindings" ;;
    *) FENCE_WHY="$(jq -r --arg where "$2" '"in \($where), " + ([.objects[] | select(.state != "read") | "the \(.kind) \(.name) \(.why)"] | join("; "))' "$1")" ;;
  esac
}
# fence_declared: the renders' fence (the code step reads them; a render that is not JSON is that
# step's UNKNOWN), into FENCE_DECLARED or FENCE_DECLARED_WHY.
fence_declared() {
  [ "$FENCE_GUARDED" = true ] || return 0
  if ! cat -- "${RBAC_RENDERS[@]}" | jq -c -n -L "$LIB" --argjson contract "$FENCE_CONTRACT" \
    'include "rec"; include "broker-fence"; [inputs | objs] | fence_read($contract)' >"$WORK/fence-declared.json" 2>"$WORK/fence-declared.err"; then
    FENCE_DECLARED_WHY="the renders could not be read for it: $(first_line "$WORK/fence-declared.err")"
    return 0
  fi
  fence_verdict "$WORK/fence-declared.json" "the renders"
  FENCE_DECLARED="$FENCE_DIGEST" FENCE_DECLARED_WHY="$FENCE_WHY"
}
# fence_declared_json: the fence code-rbac.jq reads in the code step: the declared one, or null.
fence_declared_json() { if [ -n "$FENCE_DECLARED" ]; then fence_json; else echo null; fi; }
# fence_live KUBECONFIG STEP: on the operator's cluster, after admission_rights read its discovery,
# admission_power its webhooks (WEBHOOK_SERVICES) and the token Secrets of the guarded namespaces
# were listed: sets FENCE_CREDITED, the fence fence_json writes with the declared and live digests,
# each policy's and binding's versions and the contract's residual, when the cluster's fence is the
# reviewed one the renders declare, its pods keep the account's shape and no webhook's Service is in
# its namespace.
fence_live() {
  local kube="$1" step="$2" resource missing="" held outside witness backends
  FENCE_CREDITED=""
  [ "$FENCE_GUARDED" = true ] || return 0
  FENCE_LIVE_READ=true
  if [ -z "$FENCE_DECLARED" ]; then
    no_credit "$step" "$FENCE_DECLARED_WHY"
    return 0
  fi
  for resource in validatingadmissionpolicies validatingadmissionpolicybindings; do
    grep -qxF -- "$resource.admissionregistration.k8s.io" "$WORK/admission-resources" 2>/dev/null || missing="${missing:+$missing and }$resource"
  done
  if [ -n "$missing" ] && [ -n "$ADMISSION_ERROR" ]; then
    unknown "$step" "the broker's pod fence ($FENCE_POLICIES) is unread, since the API server's discovery of admissionregistration.k8s.io is not whole, so nothing it refuses is credited"
    return 0
  elif [ -n "$missing" ]; then
    no_credit "$step" "the API server serves no $missing"
    return 0
  fi
  if ! kubectl --kubeconfig "$kube" get validatingadmissionpolicies.admissionregistration.k8s.io,validatingadmissionpolicybindings.admissionregistration.k8s.io \
    -o json >"$WORK/fence-objects.json" 2>"$WORK/fence-objects.err"; then
    unknown "$step" "the API server refused the list of admission policies and their bindings (it needs list on validatingadmissionpolicies and validatingadmissionpolicybindings), so the broker's pod fence ($FENCE_POLICIES) is unread and nothing it refuses is credited: $(first_line "$WORK/fence-objects.err")"
    return 0
  fi
  if ! is_list "$WORK/fence-objects.json" "$WORK/fence-live.err" ||
    ! jq -c -L "$LIB" --argjson contract "$FENCE_CONTRACT" 'include "rec"; include "broker-fence"; [objs] | fence_read($contract)' \
      "$WORK/fence-objects.json" >"$WORK/fence-live.json" 2>"$WORK/fence-live.err"; then
    unknown "$step" "the API server's list of admission policies is not one JSON list, so the broker's pod fence is unread and nothing it refuses is credited: $(first_line "$WORK/fence-live.err")"
    return 0
  fi
  fence_verdict "$WORK/fence-live.json" "the cluster"
  if [ -z "$FENCE_DIGEST" ]; then
    no_credit "$step" "$FENCE_WHY"
    return 0
  fi
  # Each of the renders' and the cluster's objects equals the contract's, so the two agree.
  if ! kubectl --kubeconfig "$kube" get pods -n "$FENCE_NS" -o json >"$WORK/fence-pods.json" 2>"$WORK/fence-pods.err"; then
    unknown "$step" "the API server refused the list of pods in $FENCE_NS (it needs list on pods there, container.pods.list on GKE), so whether a pod outside the broker's pod fence runs as $FENCE_ACCOUNT or takes its Secrets is unread and nothing the fence refuses is credited: $(first_line "$WORK/fence-pods.err")"
    return 0
  fi
  # The Secrets of the broker's beside those its witness pods take: --db-secret when it is in the
  # fence's namespace, and each token Secret listed there.
  held="$({ [ "${DB_SECRET%%/*}" != "$FENCE_NS" ] || printf '%s\n' "${DB_SECRET#*/}"
    awk -F'\t' -v ns="$FENCE_NS" '$1 == ns { print $2 }' "$WORK/token-secrets.tsv"; } | jq -R -s -c 'split("\n") | map(select(length > 0))')"
  # The pod record read as a part of the guard: an answer jq reads as nothing writes no record, and
  # an unguarded read of none would end the run, losing why.
  if ! is_list "$WORK/fence-pods.json" "$WORK/fence-pods.err" ||
    ! jq -r -L "$LIB" --arg prefix "$FENCE_PREFIX" --arg account "$FENCE_ACCOUNT" --argjson secrets "$held" \
      'include "broker-fence"; fence_pods($prefix; $account; $secrets)' "$WORK/fence-pods.json" >"$WORK/fence-pods.rec" 2>"$WORK/fence-pods.err" ||
    ! IFS="$US" read -r outside witness <"$WORK/fence-pods.rec"; then
    unknown "$step" "the API server's list of pods in $FENCE_NS is not one JSON list, so nothing the broker's pod fence refuses is credited: $(first_line "$WORK/fence-pods.err")"
    return 0
  fi
  if [ -n "$outside" ]; then
    no_credit "$step" "pods of $FENCE_NS run as $FENCE_ACCOUNT or take a Secret of the broker's under a name the fence's connect rule does not hold ($FENCE_PREFIX*): $outside"
    return 0
  fi
  if [ "$witness" -eq 0 ]; then
    no_credit "$step" "$FENCE_NS lists no pod named $FENCE_PREFIX* that runs as $FENCE_ACCOUNT, so its list of pods cannot be told from one that saw nothing"
    return 0
  fi
  if [ -z "$WEBHOOK_SERVICES" ]; then
    no_credit "$step" "the webhook configurations were not read (above), so whether a webhook's backend is in $FENCE_NS, where the fence does not refuse entry into it, is unknown"
    return 0
  fi
  backends="$(jq -r --arg ns "$FENCE_NS" '[.[] | select(.ns == $ns) | "\(.id) calls the Service \(.ns)/\(.name)"] | unique | join("; ")' <<<"$WEBHOOK_SERVICES")"
  if [ -n "$backends" ]; then
    no_credit "$step" "a webhook calls a Service in $FENCE_NS, whose pods the fence does not keep anyone from entering: $backends"
    return 0
  fi
  FENCE_CREDITED="$(fence_json | jq -c --arg declared "$FENCE_DECLARED" --arg live "$FENCE_DIGEST" --slurpfile read "$WORK/fence-live.json" \
    --argjson contract "$FENCE_CONTRACT" '
    . + {declared: $declared, live: $live, residual: $contract.residual,
         versions: ([$read[0].objects[] | {name, v: "\(.kind) \(.name) generation \(.generation) resourceVersion \(.resourceVersion)"}]
           | group_by(.name) | map({(.[0].name): (map(.v) | join("; "))}) | add)}')"
}
# credit PRINCIPAL RIGHT RESOURCE GRANTED_AT POLICY [RESIDUAL]: one grant whose use on the broker's
# pods the credited fence's POLICY refuses (bound by its binding of the same name), printed as
# PRINCIPAL and RIGHT and reported under `credited` with the grant (GRANTED_AT, the API server's or
# the render's reason) kept as evidence beside the policy and the declared and live versions of the
# fence, all read from FENCE_CREDITED; and, with RESIDUAL `residual`, its use on the namespace's
# other pods (residual_allowance). Neither is a finding, so neither sets the exit. Two credits of
# one principal and one sentence are one.
credit() {
  local principal="${1//[$'\t\r\n']/ }" right="${2//[$'\t\r\n']/ }" resource="${3//[$'\t\r\n']/ }" granted="${4//[$'\t\r\n']/ }" policy="$5"
  local key="C$principal"$'\t'"$right" declared live
  [ -z "${RECORDED["$key"]:-}" ] || return 0
  RECORDED["$key"]=1
  IFS=$'\t' read -r declared live < <(jq -r --arg p "$policy" '[.declared, "\(.live); \(.versions[$p])"] | @tsv' <<<"$FENCE_CREDITED")
  CREDITS+=("$principal"$'\t'"$right"$'\t'"$resource"$'\t'"$granted"$'\t'"$policy"$'\t'"$declared"$'\t'"$live")
  echo "CREDIT $principal: $right, its use on $FENCE_PREFIX* pods refused by the ValidatingAdmissionPolicy $policy (bound Deny, failurePolicy Fail), declared and live alike"
  [ "${6:-}" != residual ] || residual_allowance "$1" "$2" "$3" "$4"
}
# residual_allowance PRINCIPAL RIGHT RESOURCE GRANTED_AT: RIGHT, a grant or a chain to a grant's
# holder, reaches the credited fence's residual and no more: printed as an ALLOWANCE naming RIGHT
# and the residual's text, and reported under `residuals` with the grant (GRANTED_AT) and the issue
# that tracks the residual, both read from FENCE_CREDITED (broker-fence.json's residual, which
# fence_live copies there). No finding, so it never sets the exit. Two of one principal and one
# sentence are one.
residual_allowance() {
  local principal="${1//[$'\t\r\n']/ }" right="${2//[$'\t\r\n']/ }" resource="${3//[$'\t\r\n']/ }" granted="${4//[$'\t\r\n']/ }"
  local key="R$principal"$'\t'"$right" text tracked
  [ -z "${RECORDED["$key"]:-}" ] || return 0
  RECORDED["$key"]=1
  IFS=$'\t' read -r text tracked < <(jq -r '.residual | [(.text // "" | gsub("[\t\r\n]"; " ")), (.tracked_by // "")] | @tsv' <<<"$FENCE_CREDITED")
  RESIDUALS+=("$principal"$'\t'"$right"$'\t'"$resource"$'\t'"$granted"$'\t'"$text"$'\t'"$tracked")
  echo "ALLOWANCE $principal: $right, $text"
}
# fence_settle: the code step's held grants, once the live step has read the cluster's fence:
# credited when it is, else findings (declared_finding, step-code-rbac.sh); then the code step's
# PASS, when it waited on them alone.
fence_settle() {
  local entry principal detail id policy residual
  [ "${#CODE_PENDING[@]}" -gt 0 ] || return 0
  [ -n "$FENCE_CREDITED" ] || [ "$FENCE_LIVE_READ" = true ] ||
    no_credit "code: RBAC" "the operator's cluster's fence was not read (live: RBAC)"
  for entry in "${CODE_PENDING[@]}"; do
    IFS="$US" read -r principal detail id policy residual <<<"$entry"
    if [ -n "$FENCE_CREDITED" ]; then
      credit "$principal" "$detail, declared by $id" "cluster:$cluster_name" "$id" "$policy" "$residual"
    else
      declared_finding "$principal" "$detail" "$id"
    fi
  done
  [ -z "$CODE_PASS" ] || [ -z "$FENCE_CREDITED" ] ||
    pass "code: RBAC" "$CODE_PASS, beside ${#CODE_PENDING[@]} grants whose use on the broker's pods its pod fence refuses, declared and live alike"
  CODE_PENDING=() CODE_PASS=""
}
