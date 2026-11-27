package appauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// Whether an App is installed on a repository is GitHub's answer to the App's own JWT on GET
// /repos/{owner}/{repo}/installation: 2xx is installed, 404 is not, and anything else is no
// answer, transient when GitHub's trouble: an outage (httpoutage.Is: a 5xx, a 408 or a 429)
// or a rate limit. It is read per repository, as an installation on an owner may cover only some
// of its repositories.
func TestInstalledOnReadsTheRepositorysInstallationForEachApp(t *testing.T) {
	key, privatePEM := testAppKey(t)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		status    int
		want      bool
		wantErr   string
		transient bool
	}{
		{name: "installed", status: http.StatusOK, want: true},
		{name: "not installed", status: http.StatusNotFound},
		{name: "GitHub's trouble", status: http.StatusBadGateway, wantErr: "(502)", transient: true},
		{name: "a request GitHub's front door timed out", status: http.StatusRequestTimeout, wantErr: "(408)", transient: true},
		{name: "a rate limit", status: http.StatusTooManyRequests, wantErr: "(429)", transient: true},
		{name: "a refusal", status: http.StatusForbidden, wantErr: "(403)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked = append(asked, r.Method+" "+r.URL.Path)
				verifyAppJWT(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &key.PublicKey, now, "67890")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"id": 41}`))
			}))
			defer server.Close()
			tokens := New(config.GitHubApps{
				Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
				Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
			}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})

			got, err := tokens.InstalledOn(context.Background(), Review, ghrepo.MustParse("acme/widgets"))
			if want := []string{"GET /repos/acme/widgets/installation"}; len(asked) != 1 || asked[0] != want[0] {
				t.Fatalf("GitHub was asked %v, want %v", asked, want)
			}
			if tc.wantErr == "" {
				if err != nil || got != tc.want {
					t.Fatalf("InstalledOn = %v, %v; want %v", got, err, tc.want)
				}
				return
			}
			var transient *TransientError
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "acme/widgets") || errors.As(err, &transient) != tc.transient {
				t.Fatalf("InstalledOn = %v, %v; want an error naming acme/widgets and %s, transient %v", got, err, tc.wantErr, tc.transient)
			}
		})
	}
}

// A read under a name's own name (InstalledUnder) follows no redirect, and InstalledOn follows it.
// GitHub answers the API path of a repository it moved with a 301 to where the repository is now,
// and the App JWT is kept on a redirect within one host, so the following read of the name the
// repository left answers the installation that covers it where it is: installed. The read under
// that name answers not installed, and asks GitHub nothing past the 301. A name GitHub answers for
// itself is read alike by both. Control: with InstalledUnder following the redirect (installedOn's
// client the Manager's own for both reads), it answers sjawhar/legion installed.
func TestInstalledUnderAnswersANameGitHubRedirectsNotInstalled(t *testing.T) {
	key, privatePEM := testAppKey(t)
	now := time.Date(2026, 11, 19, 12, 54, 55, 0, time.UTC)
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		verifyAppJWT(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &key.PublicKey, now, "12345")
		switch r.URL.Path {
		case "/repos/sjawhar/legion/installation":
			http.Redirect(w, r, "/repositories/1146927990/installation", http.StatusMovedPermanently)
		case "/repositories/1146927990/installation", "/repos/cybermonklabs/legion/installation":
			_, _ = w.Write([]byte(`{"id": 41}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	defer server.Close()
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})

	for _, tc := range []struct {
		name  string
		read  func(context.Context, AppRole, ghrepo.Repository) (bool, error)
		repo  string
		want  bool
		asked []string
	}{
		{name: "InstalledUnder of the name GitHub moved", read: tokens.InstalledUnder, repo: "sjawhar/legion", want: false,
			asked: []string{"/repos/sjawhar/legion/installation"}},
		{name: "InstalledOn of the name GitHub moved", read: tokens.InstalledOn, repo: "sjawhar/legion", want: true,
			asked: []string{"/repos/sjawhar/legion/installation", "/repositories/1146927990/installation"}},
		{name: "InstalledUnder of the name GitHub holds", read: tokens.InstalledUnder, repo: "cybermonklabs/legion", want: true,
			asked: []string{"/repos/cybermonklabs/legion/installation"}},
		{name: "InstalledOn of the name GitHub holds", read: tokens.InstalledOn, repo: "cybermonklabs/legion", want: true,
			asked: []string{"/repos/cybermonklabs/legion/installation"}},
		{name: "InstalledUnder of a name GitHub does not know", read: tokens.InstalledUnder, repo: "sjawhar/gone", want: false,
			asked: []string{"/repos/sjawhar/gone/installation"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked = nil
			got, err := tc.read(context.Background(), Implement, ghrepo.MustParse(tc.repo))
			if err != nil || got != tc.want {
				t.Fatalf("installed on %s = %v, %v; want %v", tc.repo, got, err, tc.want)
			}
			if !slices.Equal(asked, tc.asked) {
				t.Fatalf("GitHub was asked %v, want %v", asked, tc.asked)
			}
		})
	}
}

// An App's bot login is its slug's, read under the App's own JWT (GET /app): it needs no
// installation on any repository, so a daemon serving no repository yet still knows the review
// App's login the engine judges pushes and reviews by. GitHub's trouble, an outage (a 5xx, a 408,
// a 429), is transient, which the daemon's review App login waits out rather than refuses on.
func TestAppLoginReadsTheAppsSlugUnderItsOwnJWT(t *testing.T) {
	key, privatePEM := testAppKey(t)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/app" {
			t.Errorf("GitHub was asked %s %s, want GET /app alone", r.Method, r.URL.Path)
		}
		verifyAppJWT(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &key.PublicKey, now, "67890")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"slug":"legion-reviewer"}`))
	}))
	defer server.Close()
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: privatePEM},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})

	if login, err := tokens.AppLogin(context.Background(), Review); err != nil || login != "legion-reviewer[bot]" {
		t.Fatalf("AppLogin = %q, %v; want legion-reviewer[bot]", login, err)
	}
	for _, status = range []int{http.StatusBadGateway, http.StatusRequestTimeout, http.StatusTooManyRequests} {
		var transient *TransientError
		if _, err := tokens.AppLogin(context.Background(), Review); !errors.As(err, &transient) {
			t.Fatalf("AppLogin on GitHub's %d = %v, want a transient error", status, err)
		}
	}
}
