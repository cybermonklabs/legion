package sessionproxy

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestAnImpersonationRefusalPrintsNoCredentialItEchoes: something in the IAM Credentials API's
// place refuses the impersonation with a page that echoes the bearer the pod's credentials signed
// the generateAccessToken call with. The library prints it as `impersonate: status code 403:
// <body>`, and the dial's error holds none of it: the impersonation's client records what each
// refused request carried (oauth2call.RefusalClient), and the boundary reads the library's error
// with it redacted (the red team's probe of #4368 at db7e4a0226). Capture
// sessionproxy.impersonation-echo.
func TestAnImpersonationRefusalPrintsNoCredentialItEchoes(t *testing.T) {
	usePodCredentials(t)
	admin := newAdminAPIFake(t)
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "proxy refused; the request carried Authorization: "+r.Header.Get("Authorization"))
	})
	err := dialThroughConnector(t, "legion-sessions@example-project.iam.gserviceaccount.com", echo, admin.url)
	if err == nil {
		t.Fatal("the dial succeeded")
	}
	const capture = "sessionproxy.impersonation-echo"
	t.Logf("REDACT-CAPTURE %s %s", capture, base64.StdEncoding.EncodeToString([]byte(err.Error())))
	if strings.Contains(err.Error(), podToken) {
		t.Errorf("REDACT-ASSERT %s: the output prints the secret %q: %q", capture, podToken, err.Error())
	}
}
