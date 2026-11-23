# operator-token-minters/custom.jq: custom_resources($names), the custom resources among $names, the
# discovery's namespaced, creatable resources a line (`kubectl api-resources -o name`), as
# {resource, group}. A custom resource's controller may make a pod from it, as an agent sandbox
# does. Custom is any API group outside Kubernetes' own: not the core group, no dotless group (apps,
# batch), and none of the built-in *.k8s.io groups below; a CRD may use any other *.k8s.io group
# (snapshot.storage.k8s.io, gateway.networking.k8s.io, *.x-k8s.io), so those are kept. Read with
# `include "custom";` and -L.
def own_groups: ["admissionregistration.k8s.io", "apiextensions.k8s.io", "apiregistration.k8s.io",
  "authentication.k8s.io", "authorization.k8s.io", "certificates.k8s.io", "coordination.k8s.io",
  "discovery.k8s.io", "events.k8s.io", "flowcontrol.apiserver.k8s.io", "internal.apiserver.k8s.io",
  "metrics.k8s.io", "networking.k8s.io", "node.k8s.io", "rbac.authorization.k8s.io", "resource.k8s.io",
  "scheduling.k8s.io", "storage.k8s.io", "storagemigration.k8s.io"];
def custom_resources($names):
  [$names | split("\n")[] | select(contains("."))
   | {resource: (split(".")[0]), group: (split(".")[1:] | join("."))}
   | select(.group | contains("."))
   | select(.group as $g | own_groups | index($g) | not)];
