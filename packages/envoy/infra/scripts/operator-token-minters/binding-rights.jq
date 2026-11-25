# operator-token-minters/binding-rights.jq: binding_rights's rights (reviews.sh), read from a
# cluster's ClusterRoleBindings and RoleBindings: for each namespace that holds a RoleBinding, the
# rights by which a principal writes a binding or a role there, naming no resource, as rights_json
# writes rights: bind and escalate on roles, bind on clusterroles, and patch and update on roles
# and on rolebindings. A RoleBinding is authorized in its namespace, a RoleBinding to a
# ClusterRole included, so one who may bind a role or a ClusterRole there, or write a binding
# there, grants a subject of its choosing what that role holds in that namespace, its Secrets
# among them. The guarded namespaces' and the cluster-wide rights of the kind are RIGHTS_TABLE's and
# GUARD_RIGHTS'; these carry the same rights to every other namespace that holds a binding. Each is
# reviewed for every principal, which finds a grant of any authorizer that names no resource, and
# live_named reads from the cluster's own roles each rule that limits it to a resource by name
# (code-rbac.jq, which reads a rule limited by name only for a right that names none): so the count
# grows with the namespaces, never with the names in them.
def right($verb; $resource; $ns):
  {verb: $verb, group: "rbac.authorization.k8s.io", resource: $resource, sub: "", ns: $ns, name: "",
   key: ([$verb, "rbac.authorization.k8s.io", $resource, "", $ns, ""] | join("|")),
   label: "\($verb) \($resource).rbac.authorization.k8s.io in \($ns)"};
[.items[]? | select(.kind == "RoleBinding") | .metadata.namespace // empty] | unique
| [.[] as $ns
   | (["bind", "roles"], ["escalate", "roles"], ["bind", "clusterroles"], ["patch", "roles"], ["update", "roles"],
      ["patch", "rolebindings"], ["update", "rolebindings"])
   | right(.[0]; .[1]; $ns)]
