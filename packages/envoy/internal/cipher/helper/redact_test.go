//go:build linux

package helper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/testport"
)

// TestABrokerCallErrorNamesNoCredentialTheURLCarries: a session's renew against a broker that
// never answers, or at a URL that does not parse, fails with an error the helper logs and
// records as the session's last error (which `agent-secrets register --wait` prints). For an
// AGENT_SECRETS_URL redact.URL will not print, it names the URL as redact.NotPrinted and the
// failure's class alone; for a clean one, it reads as net/http wrote it.
func TestABrokerCallErrorNamesNoCredentialTheURLCarries(t *testing.T) {
	t.Parallel()
	s, err := newSession(1, 1, "runtime", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	np := `"` + redact.NotPrinted + `": `
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	closing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	}))
	defer closing.Close()
	overHTTPS := "https://" + strings.TrimPrefix(plain.URL, "http://")
	// refused is a port this test holds that refuses each dial. The last four rows keep
	// 127.0.0.1:1: their address does not parse, or names no host or no http scheme, so they dial
	// nothing.
	refused := testport.Refused(t)
	for _, row := range []struct{ base, want string }{
		{"http://" + refused, `Post "http://` + refused + `/v1/enrollments//renew": dial tcp ` + refused + `: connect: connection refused`},
		{overHTTPS, `Post "` + overHTTPS + `/v1/enrollments//renew": http: server gave HTTP response to HTTPS client`},
		{closing.URL, `Post "` + closing.URL + `/v1/enrollments//renew": EOF`},
		{"http://" + "tokuser9f3:hunter2pw@" + refused, "Post " + np + "connection refused"},
		{"http://" + refused + "/?token=qtok77", "Post " + np + "connection refused"},
		{"http://" + "tokuser9f3:hunter2pw%zz@127.0.0.1:1", "parse " + np + "not a valid URL"},
		{"tokuser9f3:hunter2pw@127.0.0.1:1", "Post " + np + "the request failed"},
		{"http:/tokuser9f3:hunter2pw@127.0.0.1:1", "Post " + np},
		{"http://ada@corp.example:hunter2pw%40127.0.0.1:1", np},
	} {
		t.Run(row.base, func(t *testing.T) {
			b := &Broker{URL: cipheraddr.New(row.base), HTTP: &http.Client{Timeout: 5 * time.Second}}
			_, _, err := b.Renew(context.Background(), s)
			if err == nil {
				t.Fatal("renew against a broker that never answers succeeded")
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Fatalf("renew error %q; want it to name %s", err, row.want)
			}
			for _, secret := range []string{"tokuser9f3", "hunter2pw", "qtok77", "%zz", "ada@"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("renew error %q names %q", err, secret)
				}
			}
		})
	}
}
