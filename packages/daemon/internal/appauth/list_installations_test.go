package appauth

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/githubrate"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// ListInstallations reads every page GitHub lists an App's installations on, a hundred to a page,
// and keeps each installation GitHub lists, a suspended one marked so, with the permissions the
// list names it granted: here 101 installations come on two pages, the 58th suspended, each
// granted a permission that names it.
func TestListInstallationsReadsEveryPageAndKeepsASuspendedInstallation(t *testing.T) {
	_, key := testAppKey(t)
	const suspended = 57
	var mu sync.Mutex
	var pages []string
	server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" || r.URL.Query().Get("per_page") != "100" {
			githubtest.Unexpected(t, w, r)
			return
		}
		mu.Lock()
		pages = append(pages, r.URL.Query().Get("page"))
		mu.Unlock()
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		var entries []string
		for n := (page - 1) * 100; n < min(page*100, 101); n++ {
			since := "null"
			if n == suspended {
				since = `"` + suspendedSince + `"`
			}
			entries = append(entries, fmt.Sprintf(`{"id":%d,"app_slug":"example-app","account":{"login":"owner-%d"},"permissions":{"metadata":"read","contents":"%s"},"suspended_at":%s}`,
				1000+n, n, grantOf(n), since))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(entries, ","))
	}})
	tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", PrivateKey: key}}, Options{BaseURL: server.URL, HTTPClient: server.Client()})
	defer tokens.Close()

	listed, err := tokens.ListInstallations(context.Background(), Implement)
	if err != nil {
		t.Fatalf("ListInstallations: %v", err)
	}
	if len(listed.Installations) != 101 {
		t.Fatalf("ListInstallations listed %d installations, want the 101 of both pages", len(listed.Installations))
	}
	for n, installation := range listed.Installations {
		want := ListedInstallation{ID: strconv.Itoa(1000 + n), Owner: "owner-" + strconv.Itoa(n), Suspended: n == suspended,
			Permissions: map[string]string{"metadata": "read", "contents": grantOf(n)}}
		if installation.ID != want.ID || installation.Owner != want.Owner || installation.Suspended != want.Suspended || !maps.Equal(installation.Permissions, want.Permissions) {
			t.Errorf("installation %d = %+v, want %+v, in the order GitHub lists them", n, installation, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(pages, []string{"1", "2"}) {
		t.Errorf("ListInstallations read pages %v, want 1 and 2", pages)
	}
}

// listingManager is a key-mode Manager whose GitHub lists the App's installations as listing, one
// page of them, and answers nothing else.
func listingManager(t *testing.T, listing string) *Manager {
	t.Helper()
	_, key := testAppKey(t)
	server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" {
			githubtest.Unexpected(t, w, r)
			return
		}
		fmt.Fprint(w, listing)
	}})
	tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", PrivateKey: key}}, Options{BaseURL: server.URL, HTTPClient: server.Client()})
	t.Cleanup(tokens.Close)
	return tokens
}

// A suspended installation never fails the listing, whatever GitHub lists of it: one with no
// account is listed with no owner, for the rate reading to write its line, and one with no id,
// which no line could name, is left out alone. Discovery keeps every suspended installation out,
// as it did before, so the owners installed are acme's alone.
func TestASuspendedInstallationWithNoAccountOrNoIdFailsNoListing(t *testing.T) {
	tokens := listingManager(t, fmt.Sprintf(`[
		{"id":101,"app_slug":"example-app","account":{"login":"acme"},"permissions":{"contents":"read"},"suspended_at":null},
		{"id":102,"account":null,"permissions":{"metadata":"read"},"suspended_at":%q},
		{"account":{"login":"ghost"},"suspended_at":%q}
	]`, suspendedSince, suspendedSince))

	listed, err := tokens.ListInstallations(context.Background(), Implement)
	if err != nil {
		t.Fatalf("ListInstallations: %v", err)
	}
	want := []ListedInstallation{
		{ID: "101", Owner: "acme", Permissions: map[string]string{"contents": "read"}},
		{ID: "102", Suspended: true, Permissions: map[string]string{"metadata": "read"}},
	}
	if !slices.EqualFunc(listed.Installations, want, func(a, b ListedInstallation) bool {
		return a.ID == b.ID && a.Owner == b.Owner && a.Suspended == b.Suspended && maps.Equal(a.Permissions, b.Permissions)
	}) {
		t.Errorf("ListInstallations listed %+v, want %+v", listed.Installations, want)
	}
	owners, err := tokens.InstalledOwners(context.Background(), Implement)
	if err != nil || !slices.Equal(owners, []string{"acme"}) {
		t.Errorf("InstalledOwners = %v, %v, want acme alone", owners, err)
	}
}

// An installation listed with a permission whose value is not a string, one GitHub does not
// document today, keeps the listing and its other permissions: the value no access names is left
// out, rather than every installation of the App with it.
func TestAPermissionThatIsNoStringFailsNoListing(t *testing.T) {
	tokens := listingManager(t, `[{"id":101,"app_slug":"example-app","account":{"login":"acme"},"permissions":{"contents":"read","some_future_flag":true},"suspended_at":null}]`)

	listed, err := tokens.ListInstallations(context.Background(), Implement)
	if err != nil || len(listed.Installations) != 1 {
		t.Fatalf("ListInstallations = %+v, %v, want acme's installation", listed, err)
	}
	if got := listed.Installations[0].Permissions; !maps.Equal(got, map[string]string{"contents": "read"}) {
		t.Errorf("acme's permissions = %v, want contents: read alone", got)
	}
	if owners, err := tokens.InstalledOwners(context.Background(), Implement); err != nil || !slices.Equal(owners, []string{"acme"}) {
		t.Errorf("InstalledOwners = %v, %v, want acme", owners, err)
	}
}

// grantOf is the contents access the listing test's nth installation is granted, read or write by
// turns, so each installation's permissions are its own.
func grantOf(n int) string {
	if n%2 == 0 {
		return "read"
	}
	return "write"
}

// The slug ListInstallations names is the one GitHub lists each installation with (app_slug), so
// an App renamed while the daemon runs is named by its new slug at the next list, and GET /app is
// read only for a list that names none: one listing no installation among them.
func TestListInstallationsNamesTheSlugTheListCarriesAndReadsGETAppOnlyWhenItCarriesNone(t *testing.T) {
	_, key := testAppKey(t)
	for _, tc := range []struct {
		name, entries, slug string
		appReads            int
	}{
		{"a list naming the App's slug", `{"id":101,"app_slug":"example-renamed","account":{"login":"acme"},"suspended_at":null}`, "example-renamed", 0},
		{"a list naming no slug", `{"id":101,"account":{"login":"acme"},"suspended_at":null}`, "example-app", 1},
		{"an empty list", ``, "example-app", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			appReads := 0
			server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/app/installations":
					fmt.Fprintf(w, "[%s]", tc.entries)
				case "/app":
					mu.Lock()
					appReads++
					mu.Unlock()
					fmt.Fprint(w, `{"slug":"example-app"}`)
				default:
					githubtest.Unexpected(t, w, r)
				}
			}})
			tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", PrivateKey: key}}, Options{BaseURL: server.URL, HTTPClient: server.Client()})
			defer tokens.Close()
			listed, err := tokens.ListInstallations(context.Background(), Implement)
			if err != nil {
				t.Fatalf("ListInstallations: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if listed.Slug != tc.slug || appReads != tc.appReads {
				t.Errorf("ListInstallations named slug %q after %d reads of GET /app, want %q after %d", listed.Slug, appReads, tc.slug, tc.appReads)
			}
		})
	}
}

// A lease a token_command's release gives names the installation the release names it of, and
// the Manager tells its Counter of that token, so the calls made with it count on that
// installation: here the checks a release's token is held to (tokenIdentity), the App read and the
// bot user's on the core counter and the GraphQL viewer on the GraphQL one, each counted as
// token_identity.
func TestATokenModeLeaseNamesItsInstallationAndCountsItsIdentityReadsAsTokenIdentity(t *testing.T) {
	broker := newFakeBroker(t, []string{"acme"}, time.Now().Add(time.Hour))
	reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	spent := func(w http.ResponseWriter, counter string) {
		w.Header().Set("X-RateLimit-Resource", counter)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Used", "1")
		w.Header().Set("X-RateLimit-Reset", reset)
	}
	server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			spent(w, githubrate.GraphQL)
			fmt.Fprint(w, `{"data":{"viewer":{"login":"legion-implementer[bot]"}}}`)
		case r.URL.Path == "/apps/legion-implementer":
			spent(w, githubrate.Core)
			fmt.Fprint(w, `{"id":12345,"slug":"legion-implementer"}`)
		case r.URL.Path == "/users/legion-implementer[bot]":
			spent(w, githubrate.Core)
			fmt.Fprint(w, `{"id":100000001}`)
		default:
			githubtest.Unexpected(t, w, r)
		}
	}})
	counter := githubrate.NewCounter()
	tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", TokenCommand: broker.command}},
		Options{BaseURL: server.URL, HTTPClient: &http.Client{Transport: counter.Transport(nil)}, Counter: counter, Stderr: io.Discard})
	defer tokens.Close()

	lease, err := tokens.Token(context.Background(), Implement, "acme")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if lease.Installation != "100" {
		t.Errorf("the release's lease names installation %q, want the release's 100", lease.Installation)
	}
	core, graphql := counter.Own("100")
	if want := map[string]int{"token_identity": 2}; !maps.Equal(core, want) {
		t.Errorf("installation 100's own core calls = %v, want %v: the App read and the bot user's", core, want)
	}
	if want := map[string]int{"token_identity": 1}; !maps.Equal(graphql, want) {
		t.Errorf("installation 100's own GraphQL calls = %v, want %v: the viewer read", graphql, want)
	}
}
