package access

import "testing"

// A credential covers every project that is not private, whether its list names that project
// or not, and the empty project, which stands for every project. It covers a private project
// only where its list names that project, which is the one exception to the rule that a
// credential naming one project writes every project's objects.
func TestCredentialCoversAPrivateProjectOnlyWhereItsListNamesIt(t *testing.T) {
	credential := Credential{Listed: []string{"BRK"}}
	for _, row := range []struct {
		name    string
		project string
		private bool
		covered bool
	}{
		{name: "a public project not in the list", project: "OTH", private: false, covered: true},
		{name: "a public project in the list", project: "BRK", private: false, covered: true},
		{name: "the empty project, every project", project: "", private: false, covered: true},
		{name: "a private project in the list", project: "BRK", private: true, covered: true},
		{name: "a private project not in the list", project: "OTH", private: true, covered: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			if covered := CredentialCovers(credential, row.project, row.private); covered != row.covered {
				t.Fatalf("CredentialCovers(%+v, %q, %t) = %t, want %t",
					credential, row.project, row.private, covered, row.covered)
			}
		})
	}
}
