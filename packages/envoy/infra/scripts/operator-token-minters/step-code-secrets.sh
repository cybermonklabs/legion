# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/step-code-secrets.sh: the step "code: Secrets".
#
# The render has the same document shapes as --rbac-render: a Kubernetes List, an array of objects,
# or a JSON stream of objects. This reader uses only a Secret's namespace, name, type and the
# service-account-name annotation. It never selects any other Secret field.
#
# Each render must hold a JSON document and declare a Secret: a file of no document (zero bytes,
# or blanks alone) is no render, and a Secret program's render that declares no Secret cannot be
# told from a render of nothing, so each is UNKNOWN. Its records keep an empty field empty (rec.jq),
# so a token Secret without its namespace, name or service-account-name annotation is UNKNOWN,
# never read as a Secret of another type.
#
# SECRET_RIGHTS: a get by name of each token Secret a render declares for a guarded pair, which the
# RBAC steps ask. SECRET_DECLARED: the namespace and name of every Secret a render declares, whatever
# its type, which the broker's pod fence holds each other pod's Secrets to (broker-fence.sh).

SECRET_RIGHTS='[]' SECRET_DECLARED='[]'
step="code: Secrets"
if [ "${#SECRET_RENDERS[@]}" -eq 0 ]; then
  unknown "$step" "no --secret-render: what the Secret programs declare is not read"
else
  mark
  n=0 declared=0
  for file in "${SECRET_RENDERS[@]}"; do
    n=$((n + 1))
    if ! jq -r -s "$JQ_REC"'
      def objects:
        if type == "array" then .[]
        elif type == "object" and .kind == "List" and (.items | type == "array") then .items[]
        elif type == "object" and (.kind | type == "string") then .
        else error("not a Kubernetes-object document")
        end;
      def text: if type == "string" then . else "" end;
      if length == 0 then error("it holds no JSON document") else . end
      | .[] | objects
      | select(.kind == "Secret")
      | (.metadata | if type == "object" then . else {} end) as $metadata
      | ($metadata.annotations | if type == "object" then . else {} end) as $annotations
      | [($metadata.namespace | text), ($metadata.name | text), (.type | text),
         ($annotations["kubernetes.io/service-account.name"] | text)]
      | rec
    ' "$file" >"$WORK/secret-render-$n.rec" 2>"$WORK/secret-render-$n.err"; then
      unknown "$step" "--secret-render entry $n is not a Kubernetes-object JSON render: $(first_line "$WORK/secret-render-$n.err")"
      continue
    fi
    if [ ! -s "$WORK/secret-render-$n.rec" ]; then
      unknown "$step" "--secret-render entry $n declares no Secret, so it cannot be told from a render of nothing"
      continue
    fi
    while IFS="$US" read -r namespace name type service_account; do
      declared=$((declared + 1))
      [ -z "$namespace" ] || [ -z "$name" ] ||
        SECRET_DECLARED="$(jq -c --arg ns "$namespace" --arg name "$name" '. + [{ns: $ns, name: $name}] | unique' <<<"$SECRET_DECLARED")"
      [ "$type" = kubernetes.io/service-account-token ] || continue
      [ -n "$namespace" ] && [ -n "$name" ] && [ -n "$service_account" ] || {
        unknown "$step" "--secret-render entry $n declares a service-account token Secret without its namespace, name or service-account-name annotation"
        continue
      }
      pair="$namespace/$service_account"
      case " ${GUARDED[*]} " in
        *" $pair "*)
          right="$(jq -n -c --arg ns "$namespace" --arg name "$name" '
            {verb: "get", group: "", resource: "secrets", sub: "", ns: $ns, name: $name,
             label: "get secrets \($name) in \($ns)"}
            | . + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}')"
          SECRET_RIGHTS="$(jq -c --argjson right "$right" 'if any(.[]; .key == $right.key) then . else . + [$right] end' <<<"$SECRET_RIGHTS")"
          ;;
      esac
    done <"$WORK/secret-render-$n.rec"
  done
  if unchanged; then
    pass "$step" "the Secret renders declare $declared Secret(s), $(jq length <<<"$SECRET_RIGHTS") of them guarded service-account token Secret(s), without reading a Secret value"
  fi
fi
