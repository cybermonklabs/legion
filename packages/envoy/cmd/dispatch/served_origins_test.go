package main

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/config"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// servedOriginsReachTheirReaders hands DISPATCH_SERVED_ORIGINS, through the settings read alone, to
// each reader: boot, which reads each entry, trimmed, as a browser writes an origin and refuses an
// entry by its position; the document service and the API, which read a dashboard link at a served
// address as a reference; and rebuild-refs, which refuses a list the server would refuse.
func servedOriginsReachTheirReaders(t *testing.T) {
	t.Helper()
	boot := resolveWith(t, map[string]string{"DISPATCH_SERVED_ORIGINS": " https://b.example.test , http://c.example.test:8080/ "})
	if want := []string{"https://b.example.test", "http://c.example.test:8080"}; !slices.Equal(boot.ServedOrigins, want) {
		t.Fatalf("ServedOrigins = %q, want %q", boot.ServedOrigins, want)
	}
	linksAreReferences(t, boot, "https://b.example.test/issues/ABC-1", "http://c.example.test:8080/issues/ABC-1")
	if _, err := rebuildRefsOrigins(envGetter(map[string]string{"DISPATCH_SERVED_ORIGINS": "https://a.example.test:8443"}), "https://a.example.test"); err == nil || !strings.Contains(err.Error(), "DISPATCH_SERVED_ORIGINS") {
		t.Errorf("rebuild-refs with a served address on the canonical origin's host name: %v, want the server's refusal", err)
	}
	refusedWith(t, map[string]string{"DISPATCH_SERVED_ORIGINS": "https://b.example.test,"}, "DISPATCH_SERVED_ORIGINS entry 2 of 2 is empty")
	refusedWith(t, map[string]string{"DISPATCH_SERVED_ORIGINS": "https://b.example.test,https://c.example.test/dispatch"}, "DISPATCH_SERVED_ORIGINS entry 2 of 2: must be an absolute http or https URL")
}

// formerOriginsReachTheirReaders hands DISPATCH_FORMER_ORIGINS, through the settings read alone,
// to each reader: boot, which reads each entry as DISPATCH_SERVED_ORIGINS' and refuses an entry by
// its position; the document service and the API, which still read a dashboard link at a former
// address as a reference; and rebuild-refs, which refuses a list the server would refuse.
func formerOriginsReachTheirReaders(t *testing.T) {
	t.Helper()
	boot := resolveWith(t, map[string]string{"DISPATCH_FORMER_ORIGINS": " https://old.example.test/ "})
	if want := []string{"https://old.example.test"}; !slices.Equal(boot.FormerOrigins, want) {
		t.Fatalf("FormerOrigins = %q, want %q", boot.FormerOrigins, want)
	}
	linksAreReferences(t, boot, "https://old.example.test/issues/ABC-1")
	if _, err := rebuildRefsOrigins(envGetter(map[string]string{"DISPATCH_FORMER_ORIGINS": "http://a.example.test"}), "https://a.example.test"); err == nil || !strings.Contains(err.Error(), "DISPATCH_FORMER_ORIGINS") {
		t.Errorf("rebuild-refs with a former address on the canonical origin's host name: %v, want the server's refusal", err)
	}
	refusedWith(t, map[string]string{"DISPATCH_FORMER_ORIGINS": "https://old.example.test,,https://older.example.test"}, "DISPATCH_FORMER_ORIGINS entry 2 of 3 is empty")
}

// linksAreReferences fails the test unless the two readers of Dispatch links main builds from boot
// beside the canonical origin https://a.example.test read each of pages, a dashboard link, as a
// reference: the document service (documentDeps), which reads the issue the link names, and the
// API of the router (routerFor), whose GET /api/v1/reference-targets answers 200 for it. A link at
// an address that is none of the server's is the control: the document service reads it as a URL,
// and the API answers it 400 INVALID_REFERENCE.
func linksAreReferences(t *testing.T, boot bootConfig, pages ...string) {
	t.Helper()
	const unrelated = "https://other.example.test/issues/ABC-1"
	origins := documentDeps(boot, "https://a.example.test", docs.Deps{}).Origins
	handler := routerFor(t, boot, routes.AppContextOptions{Store: storetest.Open(t), ServerURL: "https://a.example.test"})
	targets := func(page string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, signedIn(t, httptest.NewRequest(http.MethodGet, "/api/v1/reference-targets?ref="+url.QueryEscape(page), nil), "alice"))
		return response
	}
	for _, page := range pages {
		if refs := text.Extract(page, origins); len(refs) != 1 || refs[0].Kind != "issue" {
			t.Errorf("the document service reads %s as %+v, want the issue it names", page, refs)
		}
		if response := targets(page); response.Code != http.StatusOK {
			t.Errorf("GET /api/v1/reference-targets?ref=%s: %d %s, want 200, a reference's", page, response.Code, response.Body.String())
		}
	}
	if refs := text.Extract(unrelated, origins); len(refs) != 1 || refs[0].Kind != "url" {
		t.Errorf("the document service reads %s as %+v, want the URL it is", unrelated, refs)
	}
	if response := targets(unrelated); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_REFERENCE"`) {
		t.Errorf("GET /api/v1/reference-targets?ref=%s: %d %s, want 400 INVALID_REFERENCE", unrelated, response.Code, response.Body.String())
	}
}

// bootRefusal is what a server started with env refuses its start with, in main's order: the
// settings read alone (resolveBootConfig), then, once the canonical origin resolves from them
// (loadEnvoyConfig, with no envoy.json of the machine's), the addresses beside it
// (checkServedOrigins). It is nil for a start the server takes.
func bootRefusal(t *testing.T, env settingValues) error {
	t.Helper()
	boot, err := resolveBootConfig(env)
	if err != nil {
		return err
	}
	envoyConfig, err := loadEnvoyConfig(env, config.LoadOptions{CWD: t.TempDir(), HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("load the envoy config: %v", err)
	}
	serverURL := ""
	if envoyConfig.Dispatch != nil {
		serverURL = envoyConfig.Dispatch.ServerURL
	}
	return checkServedOrigins(boot, serverURL)
}

// TestServedAndFormerAddressesAreRefusedAtBoot: the server refuses to start, naming the setting and
// the entry, on each list whose addresses share a host name, at any port and whatever the case of
// the name or a trailing dot: two served addresses, a served address and the canonical origin, two
// former addresses, a former address and a served address, a former address and the canonical
// origin, and a former address and a proof origin (DISPATCH_PROOF_ORIGINS). It refuses an entry
// that is no absolute http or https URL with no path, user, query or fragment, without echoing it;
// an entry spelt otherwise than a browser writes an origin; an empty entry; and either list under
// dev sign-in. rebuild-refs refuses each before it opens the database. The control: addresses on
// host names of their own, a served address also listed as a proof origin among them, start the
// server.
func TestServedAndFormerAddressesAreRefusedAtBoot(t *testing.T) {
	const canonical = "https://a.example.test"
	for _, row := range []struct {
		name   string
		values map[string]string
		// dev is a server under DISPATCH_DEV_SIGNIN=1, on a loopback canonical origin.
		dev bool
		// naming is what the refusal says.
		naming string
	}{
		{"two served addresses on one host name at two ports", map[string]string{"DISPATCH_SERVED_ORIGINS": "https://b.example.test,https://b.example.test:8443"}, false,
			"DISPATCH_SERVED_ORIGINS entry 2 of 2: its host name b.example.test is DISPATCH_SERVED_ORIGINS entry 1's"},
		{"a served address on the canonical origin's host name at another port", map[string]string{"DISPATCH_SERVED_ORIGINS": "https://a.example.test:8443"}, false,
			"DISPATCH_SERVED_ORIGINS entry 1 of 1: its host name a.example.test is DISPATCH_SERVER_URL's"},
		{"a served address on the host name of a canonical origin in upper case with a trailing dot", map[string]string{"DISPATCH_SERVER_URL": "https://A.Example.Test.", "DISPATCH_SERVED_ORIGINS": "http://a.example.test:8080"}, false,
			"DISPATCH_SERVED_ORIGINS entry 1 of 1: its host name a.example.test is DISPATCH_SERVER_URL's"},
		{"two former addresses on one host name at two ports", map[string]string{"DISPATCH_FORMER_ORIGINS": "http://old.example.test,https://old.example.test:8443"}, false,
			"DISPATCH_FORMER_ORIGINS entry 2 of 2: its host name old.example.test is DISPATCH_FORMER_ORIGINS entry 1's"},
		{"a former address on a served address's host name at another port", map[string]string{"DISPATCH_SERVED_ORIGINS": "https://c.example.test,https://b.example.test:8443", "DISPATCH_FORMER_ORIGINS": "https://b.example.test"}, false,
			"DISPATCH_FORMER_ORIGINS entry 1 of 1: its host name b.example.test is DISPATCH_SERVED_ORIGINS entry 2's"},
		{"a former address on the canonical origin's host name at another port", map[string]string{"DISPATCH_FORMER_ORIGINS": "http://a.example.test:8080"}, false,
			"DISPATCH_FORMER_ORIGINS entry 1 of 1: its host name a.example.test is DISPATCH_SERVER_URL's"},
		{"a former address on a proof origin's host name at another port", map[string]string{"DISPATCH_PROOF_ORIGINS": "https://old.example.test:9443", "DISPATCH_FORMER_ORIGINS": "https://old.example.test"}, false,
			"DISPATCH_FORMER_ORIGINS entry 1 of 1: its host name old.example.test is that of the proof origin https://old.example.test:9443; a former address passes no proof check"},
		{"a served address with a path", map[string]string{"DISPATCH_SERVED_ORIGINS": "https://b.example.test/dispatch"}, false,
			"DISPATCH_SERVED_ORIGINS entry 1 of 1: must be an absolute http or https URL with no path"},
		{"a former address with a path", map[string]string{"DISPATCH_FORMER_ORIGINS": "https://old.example.test/issues"}, false,
			"DISPATCH_FORMER_ORIGINS entry 1 of 1: must be an absolute http or https URL with no path"},
		{"a served address of another scheme", map[string]string{"DISPATCH_SERVED_ORIGINS": "ftp://b.example.test"}, false,
			"DISPATCH_SERVED_ORIGINS entry 1 of 1: must be an absolute http or https URL with no path"},
		{"a relative served address", map[string]string{"DISPATCH_SERVED_ORIGINS": "b.example.test"}, false,
			"DISPATCH_SERVED_ORIGINS entry 1 of 1: must be an absolute http or https URL with no path"},
		{"a served address with no scheme", map[string]string{"DISPATCH_SERVED_ORIGINS": "//b.example.test"}, false,
			"DISPATCH_SERVED_ORIGINS entry 1 of 1: must be an absolute http or https URL with no path"},
		{"a served address with a capital in its host", map[string]string{"DISPATCH_SERVED_ORIGINS": "https://B.example.test"}, false,
			`DISPATCH_SERVED_ORIGINS entry 1 of 1: host label "B" is not a lowercase DNS label`},
		{"a served address with its scheme's default port", map[string]string{"DISPATCH_SERVED_ORIGINS": "https://b.example.test:443"}, false,
			"DISPATCH_SERVED_ORIGINS entry 1 of 1: must be written as a browser writes this origin, https://b.example.test"},
		{"a former address with its scheme's default port", map[string]string{"DISPATCH_FORMER_ORIGINS": "http://old.example.test:80"}, false,
			"DISPATCH_FORMER_ORIGINS entry 1 of 1: must be written as a browser writes this origin, http://old.example.test"},
		{"an empty served entry", map[string]string{"DISPATCH_SERVED_ORIGINS": "https://b.example.test, ,https://c.example.test"}, false,
			"DISPATCH_SERVED_ORIGINS entry 2 of 3 is empty"},
		{"a former address carrying a user and a query", map[string]string{"DISPATCH_FORMER_ORIGINS": "https://reader-name@old.example.test/?topic=querytext"}, false,
			"DISPATCH_FORMER_ORIGINS entry 1 of 1: must be an absolute http or https URL with no path, user, query or fragment"},
		{"a served address under dev sign-in", map[string]string{"DISPATCH_SERVED_ORIGINS": "http://localhost:5174"}, true,
			"DISPATCH_SERVED_ORIGINS must be unset with DISPATCH_DEV_SIGNIN=1"},
		{"a former address under dev sign-in", map[string]string{"DISPATCH_FORMER_ORIGINS": "http://localhost:5175"}, true,
			"DISPATCH_FORMER_ORIGINS must be unset with DISPATCH_DEV_SIGNIN=1"},
	} {
		t.Run(row.name, func(t *testing.T) {
			values := maps.Clone(row.values)
			if values["DISPATCH_SERVER_URL"] == "" {
				values["DISPATCH_SERVER_URL"] = canonical
			}
			env := bootEnvironment(values)
			if row.dev {
				values["DISPATCH_SERVER_URL"] = "http://localhost:5173"
				env = devSignInEnvironment(values)
			}
			err := bootRefusal(t, env)
			if err == nil || !strings.Contains(err.Error(), row.naming) || strings.Contains(err.Error(), "reader-name") || strings.Contains(err.Error(), "querytext") {
				t.Errorf("the server's start: %v, want a refusal saying %q that echoes no user or query", err, row.naming)
			}

			// rebuild-refs, given a database it cannot reach, refuses the list before it dials it.
			values["XDG_CONFIG_HOME"] = t.TempDir()
			values["DATABASE_URL"] = "postgres://dispatch@127.0.0.1:1/never?connect_timeout=1"
			if row.dev {
				values["DISPATCH_DEV_SIGNIN"] = "1"
			}
			var stdout, stderr bytes.Buffer
			code := runSubcommand(context.Background(), []string{"rebuild-refs"}, envGetter(values), &stdout, &stderr)
			if output := stdout.String() + stderr.String(); code != 1 || !strings.Contains(output, row.naming) || strings.Contains(output, "reader-name") || strings.Contains(output, "querytext") {
				t.Errorf("rebuild-refs exited %d: %q, want 1 and the server's refusal %q", code, output, row.naming)
			}
		})
	}
	if err := bootRefusal(t, bootEnvironment(map[string]string{
		"DISPATCH_SERVER_URL":     canonical,
		"DISPATCH_PROOF_ORIGINS":  "https://b.example.test",
		"DISPATCH_SERVED_ORIGINS": "https://b.example.test,http://c.example.test:8080",
		"DISPATCH_FORMER_ORIGINS": "https://old.example.test,https://older.example.test",
	})); err != nil {
		t.Errorf("addresses on host names of their own, a served one also a proof origin: %v, want the server started", err)
	}
}
