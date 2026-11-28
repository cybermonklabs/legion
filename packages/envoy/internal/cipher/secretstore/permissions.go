package secretstore

// PermissionsOf is every permission some call of calls needs, calls mapping each call a writer
// makes to the permissions it needs.
func PermissionsOf(calls map[string][]string) map[string]bool {
	all := map[string]bool{}
	for _, needs := range calls {
		for _, permission := range needs {
			all[permission] = true
		}
	}
	return all
}

// PermissionNotNamed is what a writer's refusal prints in place of a permission the service's
// message names that is none of the ones the writer's calls need.
const PermissionNotNamed = "a permission this client does not name (not printed)"
