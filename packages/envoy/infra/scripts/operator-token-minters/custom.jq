# operator-token-minters/custom.jq: custom_resources($names), the custom resources among $names, the
# discovery's namespaced, creatable resources a line (`kubectl api-resources -o name`), as
# {resource, group}. A custom resource's controller may make a pod from it, as an agent sandbox
# does. Custom is any API group outside Kubernetes' own: not the core group, no dotless group (apps,
# batch), and no *.k8s.io group other than a *.x-k8s.io one. Read with `include "custom";` and -L.
def custom_resources($names):
  [$names | split("\n")[] | select(contains("."))
   | {resource: (split(".")[0]), group: (split(".")[1:] | join("."))}
   | select(.group | contains("."))
   | select(((.group | endswith(".k8s.io")) | not) or (.group | endswith(".x-k8s.io")))];
