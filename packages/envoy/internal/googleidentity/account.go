// packages/envoy/internal/googleidentity/account.go
//
// How a message names a Google service account. It lives here, beside the metadata reader, so
// every caller that prints one — the Google identity provider's own sources, and agent-secrets'
// GCP store — reads the same rule without naming a provider package.

package googleidentity

import "regexp"

// serviceAccountEmail and serviceAccountID are the ways Google names a service account, its
// email and its numeric unique id: the shapes of an account ServiceAccountShaped admits. An email
// is a name of lower-case letters, digits and dashes at a domain under gserviceaccount.com: a
// user-managed account's (<name>@<project>.iam.gserviceaccount.com), a Google service agent's
// (service-<number>@gcp-sa-<service>.iam.gserviceaccount.com), Compute Engine's default
// (<project number>-compute@developer.gserviceaccount.com), App Engine's default
// (<project id>@appspot.gserviceaccount.com) and Cloud Build's (<number>@cloudbuild...).
var (
	serviceAccountEmail = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}@[a-z][a-z0-9-]*(?:\.[a-z][a-z0-9-]*)*\.gserviceaccount\.com$`)
	serviceAccountID    = regexp.MustCompile(`^[0-9]{1,32}$`)
)

// ServiceAccountShaped reports whether account has the shape of a Google service account's email
// or unique id: lower-case letters, digits and dashes in Google's own forms, no control character
// and nothing a pasted credential's text could pass as. A message prints an account Google or a
// setting named only where it is so shaped, agent-secrets' using line and userinfo refusal among
// them.
func ServiceAccountShaped(account string) bool {
	return serviceAccountEmail.MatchString(account) || serviceAccountID.MatchString(account)
}

// ServiceAccountWords is account as a message names it: whole where it has a service account's
// shape (ServiceAccountShaped), else as the service account setting names, which is not printed.
// agent-secrets names a credentials file's account by it too.
func ServiceAccountWords(account, setting string) string {
	if ServiceAccountShaped(account) {
		return account
	}
	return "the service account " + setting + " names (not printed: it has neither the shape of a service account's email nor of its unique id)"
}
