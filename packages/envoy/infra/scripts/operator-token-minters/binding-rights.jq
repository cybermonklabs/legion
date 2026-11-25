# operator-token-minters/binding-rights.jq: binding_rights's rights (reviews.sh) and the code step's
# (step-code-rbac.sh), read from its input's RoleBindings: a cluster's list of bindings, or the
# renders (each object, list or array of them, as code-rbac.jq reads a render). For each namespace
# that holds a RoleBinding, the rights by which a principal writes a binding or a role there,
# naming no resource, as rights_json writes rights: bind and escalate on roles, bind on
# clusterroles, and patch and update on roles and on rolebindings. A RoleBinding is authorized in
# its namespace, a RoleBinding to a ClusterRole included, so one who may bind a role or a
# ClusterRole there, or write a binding there, grants a subject of its choosing what that role
# holds in that namespace, its Secrets among them. The guarded namespaces' and the cluster-wide
# rights of the kind are RIGHTS_TABLE's and GUARD_RIGHTS'; these carry the same rights to every
# other namespace that holds a binding. Each is reviewed for every principal, which finds a grant
# of any authorizer that names no resource, and live_named reads from the cluster's own roles each
# rule that limits it to a resource by name (code-rbac.jq, which reads a rule limited by name only
# for a right that names none): so the count grows with the namespaces, never with the names.
#
# PATCH AND UPDATE OF ROLES AND ROLEBINDINGS ARE KEPT ON PURPOSE, though RBAC lets a writer of a
# role add only rules it already holds unless it may escalate, and checks a write of a RoleBinding
# the same way (an update that changes only its subjects still needs bind on its role, or every
# rule of that role: pkg/registry/rbac/rolebinding/policybased/storage.go), and they report every
# namespace admin (the built-in admin ClusterRole carries both). A namespace admin can rewrite the
# bindings of its namespace, so it can hand the rights it holds there, which may be the broker's,
# to a subject of its choosing; that is the reach this check exists to show, so it is a finding,
# not noise, and keeping these rights only adds findings. They stay out of TABLE_JSON (rights.sh),
# whose chains are ways to become a ServiceAccount, which a write of a role alone is not. Read from
# the deployment's four clusters' RBAC objects on 2026-11-25, a RoleBinding gives one of them to a
# subject that is not a person 37 times (subject and namespace), across 16 namespaces.
def objs: if type == "array" then .[] | objs
          elif type == "object" and (.kind // "") == "List" then (.items // [])[] | objs
          elif type == "object" then . else empty end;
def right($verb; $resource; $ns):
  {verb: $verb, group: "rbac.authorization.k8s.io", resource: $resource, sub: "", ns: $ns, name: "",
   key: ([$verb, "rbac.authorization.k8s.io", $resource, "", $ns, ""] | join("|")),
   label: "\($verb) \($resource).rbac.authorization.k8s.io in \($ns)"};
[inputs | objs | select(.kind == "RoleBinding") | .metadata.namespace // empty] | unique
| [.[] as $ns
   | (["bind", "roles"], ["escalate", "roles"], ["bind", "clusterroles"], ["patch", "roles"], ["update", "roles"],
      ["patch", "rolebindings"], ["update", "rolebindings"])
   | right(.[0]; .[1]; $ns)]
