package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// mintingApps is a rateApps whose every read token is lease, or err where it is set; it lists
// nothing.
type mintingApps struct {
	lease appauth.Lease
	err   error
}

func (mintingApps) ListInstallations(context.Context, appauth.AppRole) (appauth.AppInstallations, error) {
	return appauth.AppInstallations{}, nil
}

func (a mintingApps) ReadToken(context.Context, appauth.AppRole, string) (appauth.Lease, error) {
	return a.lease, a.err
}

// The account GET /app/installations names an installation's owner by is GitHub's words, or those
// of whatever answers in GitHub's place, which can echo the App's JWT there. A reading whose mint
// fails (mint-refused), or whose lease names another installation (mint-other), names the owner
// only as a login GitHub allows (githubrest.ShownLogin), never as the list wrote it, and still
// names the App and the failure. Captures daemon.rate-reading-owner/<case>.
func TestARateReadingNamesTheOwnerOnlyAsALogin(t *testing.T) {
	installation := appauth.ListedInstallation{ID: "77", Owner: oauth2calltest.Words}
	for _, tc := range []struct {
		name string
		apps mintingApps
		says string
	}{
		{"mint-refused", mintingApps{err: errors.New("GitHub refused the mint")}, "mint the implement App's read token on " + githubrest.NotALogin + ": GitHub refused the mint"},
		{"mint-other", mintingApps{lease: appauth.Lease{Token: "ghs_other", Installation: "78"}},
			"the implement App's read token on " + githubrest.NotALogin + ` is of installation "78"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &rateReader{apps: tc.apps}
			_, _, err := reader.readCounters(context.Background(), rateApp{role: appauth.Implement, id: "1"}, installation, &rateKept{})
			if err == nil {
				t.Fatal("readCounters answered no failure")
			}
			oauth2calltest.RequireNone(t, "daemon.rate-reading-owner/"+tc.name, err.Error())
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("readCounters = %v, want %q in it", err, tc.says)
			}
		})
	}
}

// The full_name GitHub's repository listing names an installation's first repository by is
// GitHub's words, or those of whatever answers in its place, which can echo the installation's
// token there. One that is no owner/name GitHub allows is refused naming its place alone, where
// ghrepo.Parse's own refusal quotes it (twin). Captures daemon.rate-reading-repository/full-name.
func TestARateReadingNamesARefusedFirstRepositoryByItsPlace(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"total_count":1,"repositories":[{"full_name":%q}]}`, oauth2calltest.Words)
	}))
	t.Cleanup(github.Close)
	t.Run("twin", func(t *testing.T) {
		if _, err := ghrepo.Parse("the installation's first repository", oauth2calltest.Words); err == nil || !strings.Contains(err.Error(), oauth2calltest.Token) {
			t.Fatalf("ghrepo.Parse = %v, want its refusal quoting the probe, so the row can see a leak", err)
		}
	})
	t.Run("full-name", func(t *testing.T) {
		_, answered, err := (&rateReader{githubAPI: github.URL}).firstRepository(context.Background(), "ghs_installation")
		if err == nil {
			t.Fatal("firstRepository took a full_name that is no repository")
		}
		oauth2calltest.RequireNone(t, "daemon.rate-reading-repository/full-name", err.Error())
		if want := "GitHub's full_name of the installation's first repository is not an owner/name GitHub allows"; err.Error() != want {
			t.Errorf("firstRepository = %v, want %q", err, want)
		}
		if answered == nil {
			t.Error("firstRepository answered no headers, want the listing's, which the core counter is read from")
		}
	})
}
