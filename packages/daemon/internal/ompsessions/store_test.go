package ompsessions

import (
	"net/url"
	"strings"
	"testing"
)

// The endpoint a pod's launcher serves: a socket in the generation's own private
// directory, and an IAM login, which holds an `@`.
var testEndpoint = Endpoint{
	SocketDir: "/var/run/legion/private/generation-1",
	Database:  "legion_sessions",
	User:      "legion-sessions@example-project.iam",
}

// Only the two database kinds put a session in a row; the volume store, which is the zero value's,
// leaves every session a file.
func TestInDatabaseIsTrueOfBothDatabaseKinds(t *testing.T) {
	for _, tc := range []struct {
		store Store
		want  bool
	}{
		{Store{}, false},
		{Store{Kind: StoreVolume}, false},
		{Store{Kind: StorePostgres, DSNSecret: "SESSION_DSN"}, true},
		{Store{Kind: StoreCloudSQLIAM, Database: Database{Instance: "example-project:us-central1:example-instance", Name: "legion_sessions", User: testEndpoint.User}}, true},
	} {
		t.Run(string(tc.store.Kind), func(t *testing.T) {
			if got := tc.store.InDatabase(); got != tc.want {
				t.Errorf("InDatabase() = %t, want %t", got, tc.want)
			}
		})
	}
}

// An instance is valid exactly when the Cloud SQL connector would dial it whole: its parser takes
// project:region:instance and the legacy domain-scoped domain:project:region:instance, never a
// name with a segment missing or empty, a longer string it would take only part of, or whitespace
// anywhere, which the Admin API would refuse at every generation start.
func TestValidInstanceIsTheConnectorsRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"example-project:us-central1:example-instance", true},
		{"example.com:example-project:us-central1:example-instance", true},
		{"example-instance", false},
		{"example-project:us-central1", false},
		{"example-project::example-instance", false},
		{":us-central1:example-instance", false},
		{"a:b:c:d:e", false},
		{" example-project:us-central1:example-instance", false},
		{"example-project:us-central1:example-instance ", false},
		{"example-project:us central1:example-instance", false},
		{"\texample-project:us-central1:example-instance", false},
		{"example-project:us-central1:example-instance\t", false},
		{"example-project:us\tcentral1:example-instance", false},
		{"example-project:us-central1:example-instance\n", false},
		{"example-project:us-central1\n:example-instance", false},
		{"", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidInstance(tc.name); got != tc.want {
				t.Errorf("ValidInstance(%q) = %t, want %t", tc.name, got, tc.want)
			}
		})
	}
}

// testWriterAudience is a workload identity provider's full resource name, with placeholder ids.
const testWriterAudience = "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/legion-session-writer/providers/example-cluster"

// A writer audience is valid exactly when it is a provider's full resource name whole: the IAM
// host after `//`, the project's number, the one location Google takes, and a pool id and a
// provider id of 4 to 32 characters of [a-z0-9-] that do not start with Google's reserved `gcp-`.
// Anything else would be an audience Google's security token service refuses at every
// generation's first dial, or a token minted for another audience than the provider's.
func TestValidWriterAudienceIsAProvidersFullResourceName(t *testing.T) {
	pool := func(id string) string {
		return "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/" + id + "/providers/example-cluster"
	}
	provider := func(id string) string {
		return "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/legion-session-writer/providers/" + id
	}
	for _, tc := range []struct {
		audience string
		want     bool
	}{
		{testWriterAudience, true},
		{pool("pool"), true},
		{pool(strings.Repeat("p", 32)), true},
		{provider("0-9a"), true},
		{provider(strings.Repeat("9", 32)), true},
		{"", false},
		{" " + testWriterAudience, false},
		{testWriterAudience + " ", false},
		{testWriterAudience + "\n", false},
		{"\t" + testWriterAudience, false},
		{strings.Replace(testWriterAudience, "/providers/", "/providers /", 1), false},
		{strings.Replace(testWriterAudience, "legion-session-writer", "Legion-Session-Writer", 1), false},
		{strings.Replace(testWriterAudience, "example-cluster", "Example-Cluster", 1), false},
		{strings.Replace(testWriterAudience, "//iam.googleapis.com/", "//IAM.googleapis.com/", 1), false},
		{strings.Replace(testWriterAudience, "workloadIdentityPools", "workloadidentitypools", 1), false},
		{strings.Replace(testWriterAudience, "//iam.googleapis.com/", "//sts.googleapis.com/", 1), false},
		{strings.Replace(testWriterAudience, "//iam.googleapis.com/", "//iam.example.com/", 1), false},
		{strings.Replace(testWriterAudience, "//iam.googleapis.com/", "https://iam.googleapis.com/", 1), false},
		{strings.TrimPrefix(testWriterAudience, "//"), false},
		{strings.TrimPrefix(testWriterAudience, "/"), false},
		{"principal:" + testWriterAudience, false},
		{testWriterAudience + "/", false},
		{testWriterAudience + "/extra", false},
		{strings.Replace(testWriterAudience, "/projects/", "/projects/123/projects/", 1), false},
		{strings.Replace(testWriterAudience, "123456789012", "example-project", 1), false},
		{strings.Replace(testWriterAudience, "123456789012", "12345678901a", 1), false},
		{strings.Replace(testWriterAudience, "123456789012", "", 1), false},
		{strings.Replace(testWriterAudience, "/locations/global/", "/locations/us-central1/", 1), false},
		{strings.TrimSuffix(testWriterAudience, "/providers/example-cluster"), false},
		{pool("abc"), false},
		{pool(strings.Repeat("p", 33)), false},
		{pool("legion_session_writer"), false},
		{pool("legion.session.writer"), false},
		{pool("gcp-legion"), false},
		{pool(""), false},
		{provider("abc"), false},
		{provider(strings.Repeat("p", 33)), false},
		{provider("example_cluster"), false},
		{provider("gcp-cluster"), false},
		{provider(""), false},
	} {
		t.Run(tc.audience, func(t *testing.T) {
			if got := ValidWriterAudience(tc.audience); got != tc.want {
				t.Errorf("ValidWriterAudience(%q) = %t, want %t", tc.audience, got, tc.want)
			}
		})
	}
}

// Oh My Pi's client takes the socket whole as the URL's `path` parameter and ignores the host; the
// login, which holds an `@`, is percent-encoded in the userinfo and survives net/url's own parser.
func TestAgentURLNamesTheSocketAndSurvivesARoundTrip(t *testing.T) {
	raw := testEndpoint.AgentURL()
	if strings.Count(raw, "@") != 1 {
		t.Errorf("AgentURL() = %q, want the login's @ percent-encoded, leaving only the userinfo separator", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	if got := u.User.Username(); got != testEndpoint.User {
		t.Errorf("user = %q, want %q", got, testEndpoint.User)
	}
	if u.Host != "localhost" || u.Path != "/"+testEndpoint.Database {
		t.Errorf("host and path = %q and %q, want localhost and /%s", u.Host, u.Path, testEndpoint.Database)
	}
	if got := u.Query().Get("path"); got != testEndpoint.Socket() {
		t.Errorf("path = %q, want the socket %q", got, testEndpoint.Socket())
	}
	if got := u.Query().Get("sslmode"); got != "disable" {
		t.Errorf("sslmode = %q, want disable", got)
	}
}

// The launcher's own pgx client takes libpq's `host` as the socket's directory and appends
// SocketName itself, and reads the login from the `user` parameter, where the `@` is again
// percent-encoded.
func TestClientURLNamesTheSocketsDirectoryAndSurvivesARoundTrip(t *testing.T) {
	raw := testEndpoint.ClientURL()
	if strings.Contains(raw, "@") {
		t.Errorf("ClientURL() = %q, want the login's @ percent-encoded", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	if u.Host != "" || u.Path != "/"+testEndpoint.Database {
		t.Errorf("host and path = %q and %q, want no host and /%s", u.Host, u.Path, testEndpoint.Database)
	}
	if got := u.Query().Get("host"); got != testEndpoint.SocketDir {
		t.Errorf("host parameter = %q, want the socket's directory %q", got, testEndpoint.SocketDir)
	}
	if got := u.Query().Get("user"); got != testEndpoint.User {
		t.Errorf("user parameter = %q, want %q", got, testEndpoint.User)
	}
	if got := u.Query().Get("sslmode"); got != "disable" {
		t.Errorf("sslmode = %q, want disable", got)
	}
}

// The socket is the directory's file under libpq's own name, which is what AgentURL names and the
// launcher binds.
func TestSocketIsLibpqsNameInTheDirectory(t *testing.T) {
	if got, want := testEndpoint.Socket(), testEndpoint.SocketDir+"/.s.PGSQL.5432"; got != want {
		t.Errorf("Socket() = %q, want %q", got, want)
	}
}
