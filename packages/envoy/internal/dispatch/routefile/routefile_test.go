package routefile

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The names of the rule's examples, and the refusals: each name is the one written, and none is a
// test file or a file built for some platforms alone. The control is a _linux.go name, which the
// same check must leave out.
func TestNameWritesOneNamePerRoute(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	builds := func(name string) bool {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package api\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, platform := range [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}} {
			context := build.Default
			context.GOOS, context.GOARCH = platform[0], platform[1]
			matched, err := context.MatchFile(dir, name)
			if err != nil {
				t.Fatal(err)
			}
			if !matched {
				return false
			}
		}
		return !strings.HasSuffix(name, "_test.go")
	}
	if builds("route_x_linux.go") || builds("route_x_test.go") {
		t.Fatal("the control failed: a _linux.go or _test.go name passes the check, so the check sees nothing")
	}
	for _, row := range []struct{ method, pattern, want string }{
		{"GET", "/api/v1", "route_api_v1+get.go"},
		{"GET", "/api/v1/issues/{key}", "route_api_v1_issues_{key}+get.go"},
		{"GET", "/api/v1/agents/{session_id}/answers", "route_api_v1_agents_{session_id}_answers+get.go"},
		{"POST", "/api/v1/live/_test/disconnect", "route_api_v1_live_~test_disconnect+post.go"},
		{"GET", "/api/v1/Upper", "route_api_v1_=55pper+get.go"},
		{"GET", "/api/v1/x_test", "route_api_v1_x~test+get.go"},
		{"GET", "/api/v1/linux", "route_api_v1_linux+get.go"},
	} {
		got, err := Name(row.method, row.pattern)
		if err != nil || got != row.want {
			t.Errorf("Name(%s, %s) = %q, %v, want %q", row.method, row.pattern, got, err, row.want)
		}
		if !Is(got) {
			t.Errorf("Is(%q) is false", got)
		}
		if !builds(got) {
			t.Errorf("%s is a name go build leaves out on some platform, or a test file", got)
		}
	}
	for _, row := range []struct{ method, pattern string }{
		{"get", "/api/v1"}, {"GET", "api/v1"}, {"GET", "/api/{a{b}}"}, {"GET", "/api/{a"}, {"GET", "/api/a}"}, {"GET", "/api/{a/b}"},
	} {
		if got, err := Name(row.method, row.pattern); err == nil {
			t.Errorf("Name(%s, %s) = %q, want a refusal", row.method, row.pattern, got)
		}
	}
	if Is("routes_table.go") || Is("route_owner_lock_test.go") {
		t.Error("Is takes a file of package api that is no route file")
	}
}
