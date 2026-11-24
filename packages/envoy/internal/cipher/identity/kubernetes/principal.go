package kubernetes

// Principal is a Kubernetes service account's spelling of a principal: the account a pod's own
// projected token proves, led by its kind so it never spells another kind's principal. Every pod
// of one account is one holder of a session name. Postgres holds this spelling in
// oauth_session_bindings.principal and enrollments.carry_principal, and migration
// 0037_pod_carry_launcher writes it in SQL, so it never changes.
func Principal(account string) string { return "account:" + account }
