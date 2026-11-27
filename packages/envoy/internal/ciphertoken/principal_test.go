package ciphertoken_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// A principal names the kind of credential and its scope beside the person or service, so two
// credentials acting for one person on different grants are two principals: a person's own token,
// and a token an operator's entry gave the same person for a project, above all. Each row pins one
// form; a row fails where two kinds read as one principal, or a form changes under the listener's
// records and Dispatch's reply check, which compare these strings.
func TestAPrincipalNamesTheKindOfItsCredential(t *testing.T) {
	for _, row := range []struct {
		name   string
		caller ciphertoken.Caller
		want   string
	}{
		{"a token scoped to its person", ciphertoken.Caller{Session: "s", Person: "ada@example.com", ProjectScope: ciphertoken.ProjectScopePerson, Projects: []string{"INFRA", "PRIV"}}, "person:ada@example.com"},
		{"a project entry's token acting for that person", ciphertoken.Caller{Session: "s", Person: "ada@example.com", ProjectScope: ciphertoken.ProjectScopeNamed, Projects: []string{"INFRA"}}, "named:INFRA:ada@example.com"},
		{"the public-session entry's token, naming no project", ciphertoken.Caller{Session: "s", Person: "ada@example.com", ProjectScope: ciphertoken.ProjectScopeNamed}, "named::ada@example.com"},
		{"a machine's token naming a service and no person", ciphertoken.Caller{Session: "s", Service: "example-fleet", ProjectScope: ciphertoken.ProjectScopePerson}, "service:example-fleet"},
		{"an account entry's token naming no person", ciphertoken.Caller{Session: "s", Service: "system:serviceaccount:example-ns:example-job", ProjectScope: ciphertoken.ProjectScopeNamed}, "service:system:serviceaccount:example-ns:example-job"},
		{"a read entry's token", ciphertoken.Caller{Session: "s", Service: "example-reader", ProjectScope: ciphertoken.ProjectScopeNone, ReadOnly: true}, "read:service:example-reader"},
		{"a person's token narrowed to reading", ciphertoken.Caller{Session: "s", Person: "ada@example.com", ProjectScope: ciphertoken.ProjectScopePerson, ReadOnly: true}, "read:person:ada@example.com"},
	} {
		if got := row.caller.Principal(); got != row.want {
			t.Errorf("%s: Principal() = %q; want %q", row.name, got, row.want)
		}
	}
	for subject, want := range map[string]string{
		"system:serviceaccount:example-ns:example-worker":       "oidc:system:serviceaccount:example-ns:example-worker",
		"example-fleet@example-project.iam.gserviceaccount.com": "oidc:example-fleet@example-project.iam.gserviceaccount.com",
		"": "",
	} {
		if got := ciphertoken.IdentityPrincipal(subject); got != want {
			t.Errorf("IdentityPrincipal(%q) = %q; want %q", subject, got, want)
		}
	}
}
