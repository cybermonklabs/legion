package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheFakeSecretsFilesFaultsAreNamedFromTheirType pins what each fault of
// BROKER_FAKE_SECRETS_FILE is refused as: a member the file does not name, by that member and
// where it starts; a value of another type than its member takes, by the member's path; text that
// is not JSON, by the byte at fault. None of them quotes the file
// (TestLocalFromFileNeverQuotesTheFile).
func TestTheFakeSecretsFilesFaultsAreNamedFromTheirType(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, content, want string }{
		{"an unknown field", `{"secrets": [{"name": "x", "secret_value": "v"}]}`, `unknown field "secret_value"`},
		{"an unknown field of the file", `{"secrets": [], "profiles": []}`, `unknown field "profiles"`},
		{"a value of the wrong type", `{"secrets": [{"name": "x", "value": 7}]}`, "secrets.0.value is not a string"},
		{"text that is not JSON", `{"secrets": [{"name": "x"},]}`, "not JSON"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "secrets.json")
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LocalFromFile(path)
			if err == nil {
				t.Fatalf("LocalFromFile took %s", c.name)
			}
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), path) {
				t.Errorf("LocalFromFile(%s) = %q; want it to name the path and %q", c.name, err, c.want)
			}
		})
	}
}
