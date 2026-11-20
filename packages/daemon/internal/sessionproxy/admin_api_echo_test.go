package sessionproxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
)

// adminBearer is the impersonated token this test's IAM Credentials stand-in mints, which signs
// every Admin API call; -RDX- is a marker the redaction check reads in captured output.
const adminBearer = "admin-bearer-RDX-7c1e"

// TestAnAdminAPIRefusalPrintsNoBearerItEchoes: something in the Cloud SQL Admin API's place
// refuses every call with googleapi's error shape, its message echoing the call's Authorization.
// The connector hands that refusal back as text alone (googleapi's), so a dial reads it under a
// scope of its own, whose record holds every request that dial sent: the Admin API client is the
// proxy's own RefusalClient, and the connector refreshes in the dial (lazy refresh), not in the
// background. The dial's error prints none of the bearer (the red team's Admin API probe of #4368
// at fd07192703). Capture sessionproxy.admin-api-echo.
func TestAnAdminAPIRefusalPrintsNoBearerItEchoes(t *testing.T) {
	usePodCredentials(t)
	const account = "legion-sessions@example-project.iam.gserviceaccount.com"
	iam := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"accessToken":%q,"expireTime":%q}`, adminBearer, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `{"error":{"code":403,"message":"the gateway saw %s","status":"PERMISSION_DENIED"}}`, r.Header.Get("Authorization"))
	}))
	defer admin.Close()
	opts, err := connectorOptions(context.Background(), account, handlerTransport{iam})
	if err != nil {
		t.Fatalf("connectorOptions: %v", err)
	}
	dialer, err := cloudsqlconn.NewDialer(context.Background(), append(opts, cloudsqlconn.WithAdminAPIEndpoint(admin.URL+"/"))...)
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	defer func() { _ = dialer.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := connectorDialer{dialer: dialer}.Dial(ctx, testInstance)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the dial answered no error")
	}
	const capture = "sessionproxy.admin-api-echo"
	t.Logf("REDACT-CAPTURE %s %s", capture, base64.StdEncoding.EncodeToString([]byte(err.Error())))
	if strings.Contains(err.Error(), adminBearer) {
		t.Errorf("REDACT-ASSERT %s: the output prints the secret %q: %q", capture, adminBearer, err.Error())
	}
}
