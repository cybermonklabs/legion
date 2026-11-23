# operator-token-minters/webhook-backends.jq: admission_power's webhooks (admission-backends.sh), read
# from a cluster's list of mutating and validating webhook configurations, with $guarded the guarded
# namespaces, $table RIGHTS_TABLE's rows and $names the discovery's namespaced, creatable resources
# (custom.jq). A webhook whose namespaceSelector can match a guarded namespace is judged: one that
# calls a Service gives, as rights (as rights_json writes them), every way to run or enter a pod in
# the Service's namespace that a guarded namespace is asked (each namespaced row of RIGHTS_TABLE:
# pods and their exec, attach and ephemeral containers, every pod controller, jobs and cronjobs,
# rolebindings, secrets; and create, update and patch of each custom resource), since a pod there
# that carries the Service's labels joins it and can mount its TLS Secret; the writes to the Service,
# its Endpoints and EndpointSlices and its Secrets; and the writes to its ConfigMaps, from which an
# injector reads the template it adds to a pod. Each such webhook's Service is listed in `services`,
# for admission_power to read; one that calls a URL is listed in `urls`. A
# selector is read against kubernetes.io/metadata.name alone, which the API server sets to the
# namespace's name: any other requirement can match, since a namespace's other labels can change,
# and an objectSelector can match a pod made to fit it.
include "custom";
def can_match($ns):
  (.namespaceSelector // {}) as $s
  | (($s.matchLabels // {}) | to_entries | all(.key != "kubernetes.io/metadata.name" or .value == $ns))
    and (($s.matchExpressions // []) | all(
      if .key != "kubernetes.io/metadata.name" then true
      elif .operator == "In" then any(.values[]; . == $ns)
      elif .operator == "NotIn" then all(.values[]; . != $ns)
      elif .operator == "Exists" then true
      else false end));
([$table[] | select(.named | not) | {verb, group, resource, sub}]
 + [custom_resources($names)[] | ("create", "update", "patch") as $verb | {verb: $verb, group, resource, sub: ""}]
 + [(["", "services"], ["", "endpoints"], ["discovery.k8s.io", "endpointslices"], ["", "secrets"], ["", "configmaps"]) as [$g, $res]
    | ("create", "update", "patch") | {verb: ., group: $g, resource: $res, sub: ""}]) as $ways
| [.items[]? | (.kind // "") as $kind | .metadata.name as $config | .webhooks[]? | . as $w
  | select(any($guarded[]; . as $ns | $w | can_match($ns)))
  | {id: "\($kind) \($config)/\(.name)", service: .clientConfig.service, url: .clientConfig.url}]
| {urls: [.[] | select(.service == null) | {id, url: (.url // "no URL and no Service")}],
   services: [.[] | select(.service != null) | {id, ns: .service.namespace, name: .service.name}],
   rights: ([.[] | select(.service != null) | . as $h | $h.service.namespace as $ns | $ways[]
     | {verb, group, resource, sub, ns: $ns, name: "",
        label: "\(.verb) \(.resource)\(if .sub != "" then "/" + .sub else "" end)\(if .group != "" then "." + .group else "" end) in \($ns), backing the webhook \($h.id)"}]
     | unique_by([.verb, .group, .resource, .sub, .ns])
     | map(. + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}))}
