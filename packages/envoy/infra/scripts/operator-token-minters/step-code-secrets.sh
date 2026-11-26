# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/step-code-secrets.sh: the step "code: Secrets".
#
# The render has the same document shapes as --rbac-render: a Kubernetes List, an array of objects,
# or a JSON stream of objects. This reader uses only a Secret's namespace, name, type and the
# service-account-name annotation. It never selects any other Secret field.

SECRET_RIGHTS='[]'
step="code: Secrets"
if [ "${#SECRET_RENDERS[@]}" -eq 0 ]; then
  unknown "$step" "no --secret-render: what the Secret programs declare is not read"
else
  mark
  n=0
  for file in "${SECRET_RENDERS[@]}"; do
    n=$((n + 1))
    if ! jq -r -s '
      def objects:
        if type == "array" then .[]
        elif type == "object" and .kind == "List" and (.items | type == "array") then .items[]
        elif type == "object" and (.kind | type == "string") then .
        else error("not a Kubernetes-object document")
        end;
      [ .[] | objects
        | select(.kind == "Secret")
        | (.metadata | if type == "object" then . else {} end) as $metadata
        | ($metadata.annotations | if type == "object" then . else {} end) as $annotations
        | {namespace: ($metadata.namespace // ""), name: ($metadata.name // ""), type: (.type // ""),
           service_account: ($annotations["kubernetes.io/service-account.name"] // "")}
      ]
      | .[] | [.namespace, .name, .type, .service_account] | @tsv
    ' "$file" >"$WORK/secret-render-$n.tsv" 2>"$WORK/secret-render-$n.err"; then
      unknown "$step" "--secret-render entry $n is not a Kubernetes-object JSON render: $(first_line "$WORK/secret-render-$n.err")"
      continue
    fi
    while IFS=$'\t' read -r namespace name type service_account; do
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
    done <"$WORK/secret-render-$n.tsv"
  done
  if unchanged; then
    pass "$step" "the Secret renders name $(jq length <<<"$SECRET_RIGHTS") guarded service-account token Secret(s), without reading a Secret value"
  fi
fi
