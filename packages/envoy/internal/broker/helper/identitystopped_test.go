package helper

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
)

// TestAStoppedTradeLoopSaysNothingTradesAgain: a helper in identity mode whose trade the broker
// refuses in a way no retry cures (a machine no entry of the deployment lists) stops its trade
// loop, and from then on tells a session and the operator that, not that a trade is coming: the
// enrolment's error, the socket's NO_CREDENTIAL answer to a machine command, and login-status,
// whose answer carries identity_stopped. None names a login, which would not list the machine
// either. Its log names the broker's refusal.
func TestAStoppedTradeLoopSaysNothingTradesAgain(t *testing.T) {
	const refusal = "The subject token was refused: no entry lists the Google account box@fleet.iam.gserviceaccount.com, which the actor token names as the machine."
	f := newEnrolBroker(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": refusal})
	})
	mux.Handle("/", f.srv.Config.Handler)
	f.srv.Config.Handler = mux

	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("projected.service.account.token"), 0o600); err != nil {
		t.Fatal(err)
	}
	machine, err := brokerclient.New(brokerclient.Config{
		BrokerURL: f.srv.URL, Key: key, HTTP: f.srv.Client(), Subject: brokerclient.NewProjectedToken(tokenFile),
	})
	if err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	b := &Broker{
		URL: f.srv.URL, Identity: machine, HTTP: f.srv.Client(),
		OperatorFile: filepath.Join(t.TempDir(), "no-operator-here"),
		Log:          slog.New(slog.NewJSONHandler(&out, nil)),
	}
	t.Cleanup(b.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	line := waitForRecord(t, &out, "the helper stopped trading this machine's identity; it enrols no session until this is fixed")
	if said, _ := line["error"].(string); !strings.Contains(said, refusal) {
		t.Fatalf("the stopped loop's line carries %v; want the broker's refusal %q", line, refusal)
	}

	stopped := "no machine credential; " + brokerclient.Hint(brokerclient.FailureIdentityStopped)
	if _, _, err := b.EnrollSessionKey(ctx, "box-after-the-stop", sessionThumbprint(t), nil); !errors.Is(err, errNoCredential) || err.Error() != stopped {
		t.Fatalf("an enrolment after the loop stopped answered %v; want %q", err, stopped)
	}
	served := serveBroker(t, b, "", b.Log)
	if signed := served.call(t, Request{Op: "sign-launcher", Method: http.MethodGet, URL: f.srv.URL + "/v1/operator/machines"}); signed.Code != CodeNoCredential || signed.Error != stopped {
		t.Fatalf("sign-launcher after the loop stopped answered %+v; want NO_CREDENTIAL %q", signed, stopped)
	}
	status := served.call(t, Request{Op: "login-status"})
	if !status.MachineIdentity || !status.IdentityStopped || status.NoCredentialFailure() != brokerclient.FailureIdentityStopped {
		t.Fatalf("login-status after the loop stopped answered %+v; want machine_identity and identity_stopped set", status)
	}
	for _, said := range []string{stopped, out.String()} {
		if strings.Contains(said, "login") || strings.Contains(said, "is trading") {
			t.Fatalf("the helper whose loop stopped said %q: it names a login or a trade to wait for, and neither comes", said)
		}
	}
}
