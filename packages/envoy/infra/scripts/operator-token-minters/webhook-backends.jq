# operator-token-minters/webhook-backends.jq: admission_power's webhooks (admission-backends.sh), read
# from a cluster's list of mutating and validating webhook configurations (one List object, which
# admission_power holds to is_list before this runs), with $guarded the guarded namespaces, $table
# RIGHTS_TABLE's rows, $names the discovery's namespaced resources that take create (custom_rights
# reads them with --verbs=create; custom.jq) and $cluster_names its cluster-scoped ones. A webhook is
# judged when its namespaceSelector can match a guarded namespace, or when a rule of it can match a
# cluster-scoped object, whatever its namespaceSelector says: the API server applies no
# namespaceSelector to an object with no namespace, so such a webhook admits the operator's
# cluster-scoped writes (a ClusterRoleBinding, the next apply) wherever it is backed. A judged
# webhook that calls a Service gives, as rights (as rights_json writes them), every way to run or
# enter a pod in the Service's namespace that a guarded namespace is asked (each namespaced row of
# RIGHTS_TABLE:
# pods and their exec, attach and ephemeral containers, every pod controller, jobs and cronjobs,
# rolebindings, secrets; and create, update and patch of each custom resource), since a pod there
# that carries the Service's labels joins it and can mount its TLS Secret; the writes to the Service,
# its Endpoints and EndpointSlices and its Secrets; and the writes to its ConfigMaps, from which an
# injector reads the template it adds to a pod. Each such webhook's Service is listed in `services`,
# for admission_power to read; one that calls a URL is listed in `urls`. Every webhook's Service,
# judged or not, is listed in `backends`, for broker-fence.sh: entry into a backend's pod changes
# what its webhook does to every object it admits. Each list writes a webhook's id and Service from
# the one walk below. A selector is read against kubernetes.io/metadata.name alone, which the API
# server sets to the namespace's name: any other requirement can match, since a namespace's other
# labels can change, and an objectSelector can match a pod made to fit it. A rule can match a
# cluster-scoped object unless its scope is Namespaced: with scope Cluster it can, and with scope *
# (the default) it can when a resource it names is `*`, is cluster-scoped in a group it names
# ($cluster_names), or is namespaced in none of them ($names). So a namespaced resource with no
# create verb, which $names leaves out, counts as cluster-scoped: that judges more webhooks, never
# fewer.
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
# in_groups($groups): whether a discovered {resource, group} is in a group a rule names.
def in_groups($groups): .group as $g | any($groups[]; . == "*" or . == $g);
# cluster_rule($namespaced; $cluster): whether a webhook rule can match a cluster-scoped object, given
# the discovery's namespaced and cluster-scoped resources as {resource, group}.
def cluster_rule($namespaced; $cluster):
  (.scope // "*") as $scope
  | (.apiGroups // ["*"]) as $groups
  | if $scope == "Namespaced" then false
    elif $scope == "Cluster" then true
    else any((.resources // [])[] | split("/")[0];
      . as $r | $r == "*"
        or any($cluster[]; .resource == $r and in_groups($groups))
        or (any($namespaced[]; .resource == $r and in_groups($groups)) | not))
    end;
discovered($names) as $namespaced | discovered($cluster_names) as $cluster
| ([$table[] | select(.named | not) | {verb, group, resource, sub}]
 + [custom_resources($names)[] | ("create", "update", "patch") as $verb | {verb: $verb, group, resource, sub: ""}]
 + [(["", "services"], ["", "endpoints"], ["discovery.k8s.io", "endpointslices"], ["", "secrets"], ["", "configmaps"]) as [$g, $res]
    | ("create", "update", "patch") | {verb: ., group: $g, resource: $res, sub: ""}]) as $ways
| [.items[] | (.kind // "") as $kind | .metadata.name as $config | .webhooks[]? | . as $w
  | "\($kind) \($config)/\(.name)" as $id
  | {id: $id, url: .clientConfig.url,
     backend: (.clientConfig.service | if . == null then null else {id: $id, ns: .namespace, name: .name} end),
     judged: (any(.rules[]?; cluster_rule($namespaced; $cluster)) or any($guarded[]; . as $ns | $w | can_match($ns)))}] as $hooks
| [$hooks[] | select(.judged)] as $judged
| {backends: [$hooks[] | .backend // empty],
   urls: [$judged[] | select(.backend == null) | {id, url: (.url // "no URL and no Service")}],
   services: [$judged[] | .backend // empty],
   rights: ([$judged[] | .backend // empty | . as $b | $ways[]
     | {verb, group, resource, sub, ns: $b.ns, name: "",
        label: "\(.verb) \(.resource)\(if .sub != "" then "/" + .sub else "" end)\(if .group != "" then "." + .group else "" end) in \($b.ns), backing the webhook \($b.id)"}]
     | unique_by([.verb, .group, .resource, .sub, .ns])
     | map(. + {key: ([.verb, .group, .resource, .sub, .ns, .name] | join("|"))}))}
