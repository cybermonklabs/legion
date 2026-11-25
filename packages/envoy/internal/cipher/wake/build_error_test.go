package wake

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/configaddr"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestANoticeTheBrokerCannotBuildLogsNoCredentialTheEnvoyURLCarries: a BROKER_ENVOY_URL carrying
// the probe's user information, query value and fragment value, with an escape net/url cannot
// read, sends no notice: net/http's error quotes the URL it could not parse, credential and all.
// The warning Notify logs names the failure's class (the address's Cause) and none of the
// credentials. Captures wake.build.
func TestANoticeTheBrokerCannotBuildLogsNoCredentialTheEnvoyURLCarries(t *testing.T) {
	unbuildable := "http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/%zz?access" + "_token=" +
		redacttest.Token + "#access" + "_token=" + redacttest.Token
	var logged bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	Envoy{URL: configaddr.New("BROKER_ENVOY_URL", unbuildable), Account: staticAccount("account-token"), HTTP: &http.Client{}}.
		Notify(context.Background(), "sess-123", "req-456", "approved")
	if !strings.Contains(logged.String(), "wake envoy: build request") {
		t.Fatalf("Notify logged no failed build: %q", logged.String())
	}
	redacttest.RequireNone(t, "wake.build", logged.String())
	if !strings.Contains(logged.String(), "not a valid URL") {
		t.Errorf("the warning names no class of the failure: %q", logged.String())
	}
}
