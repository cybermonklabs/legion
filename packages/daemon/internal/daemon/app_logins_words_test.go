package daemon

import (
	"context"
	"log/slog"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// sameLogins answers one login for every App, as GitHub's App reads would if something in their
// place echoed one value for both.
type sameLogins string

func (l sameLogins) AppLogin(context.Context, appauth.AppRole) (string, error) { return string(l), nil }

// TestTheBootsAppLoginsRefusalNamesNoLoginOutsideItsShape: the boot's App logins read answers the
// same bot login for both Apps, an installation token something in GitHub's place echoed. The boot
// refuses two Apps under one login, and its refusal names each login only as a login GitHub
// allows, here its fixed phrase, and holds no run of the token. Captures
// daemon.answer-words/app-logins.
func TestTheBootsAppLoginsRefusalNamesNoLoginOutsideItsShape(t *testing.T) {
	login := githubtest.InstallationToken(t.Name()) + "-RDX-"
	_, err := appLoginsAtBoot(context.Background(), sameLogins(login), newStartWaits(productionAppCalls()), slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("the boot took two Apps under one bot login")
	}
	oauth2calltest.RequireNoPiece(t, "daemon.answer-words/app-logins", err.Error(), login)
	requireHolds(t, err.Error(), `bot login "`+notALogin+`" and the implement App "`+notALogin+`"`)
}
