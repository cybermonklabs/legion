package testnats

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestAMonitoringReadPrintsNoCredentialItsURLCarries: a monitoring endpoint's base URL
// (LEGION_TEST_NATS_MONITOR_URL, or a test's own server's) with made-up user information and a
// made-up access token in its query. The read of the server's connections names the URL it asked
// as oauth2call.AskedURL names it, where it wrote the joined URL whole: an answer that is not 200
// (status), one cut short (read), one that is not JSON (decode); a read that got no answer is
// oauth2call.Unanswered's, where it handed on net/http's error, which names the URL with its user
// name and its query (no-answer). The refusal of one of the two settings without the other names
// each as shownSetting names it, where it quoted both as set: a URL (setting) and one url.Parse
// refuses (setting-unparsed). The URL as written holds the credentials (twin). The control: each
// error still names the route asked, or the setting. Captures testnats.monitor-url/<case>.
func TestAMonitoringReadPrintsNoCredentialItsURLCarries(t *testing.T) {
	const (
		user     = "not-a-secret-user"
		password = "not-a-secret-password"
		token    = "not-a-secret-token"
	)
	credentialed := func(base string) string {
		return "http://" + user + ":" + password + "@" + strings.TrimPrefix(base, "http://") + "/prefix?access_token=" + token
	}
	serve := func(t *testing.T, answer func(w http.ResponseWriter)) string {
		t.Helper()
		monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { answer(w) }))
		t.Cleanup(monitor.Close)
		return monitor.URL
	}
	t.Run("twin", func(t *testing.T) {
		raw := credentialed("http://127.0.0.1:1")
		for _, secret := range []string{user, password, token} {
			if !strings.Contains(raw, secret) {
				t.Fatalf("the monitoring URL %q does not hold %q; want every credential, so the rows can see one printed", raw, secret)
			}
		}
	})
	read := func(t *testing.T, answer func(w http.ResponseWriter)) (string, string) {
		t.Helper()
		base := serve(t, answer)
		_, err := connectionsAt(credentialed(base))
		if err == nil {
			t.Fatalf("connectionsAt took the stand-in's answer; want it refused")
		}
		return err.Error(), base + "/prefix/connz"
	}
	for _, c := range []struct {
		name, prefix string
		answer       func(w http.ResponseWriter)
	}{
		{"status", "GET %s answered 503 Service Unavailable", func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) }},
		{"read", "read %s: ", func(w http.ResponseWriter) {
			conn, buffered, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			fmt.Fprint(buffered, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{\"connections\":")
			_ = buffered.Flush()
		}},
		{"decode", "decode %s: ", func(w http.ResponseWriter) { fmt.Fprint(w, "not JSON") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			said, asked := read(t, c.answer)
			oauth2calltest.RequireNoPiece(t, "testnats.monitor-url/"+c.name, said, user, password, token)
			if want := fmt.Sprintf(c.prefix, asked); !strings.HasPrefix(said, want) {
				t.Errorf("connectionsAt = %q; want it to begin %q", said, want)
			}
		})
	}
	t.Run("no-answer", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		dead.Close()
		_, err := connectionsAt(credentialed(dead.URL))
		if err == nil {
			t.Fatal("connectionsAt of a monitoring endpoint that is gone succeeded")
		}
		oauth2calltest.RequireNoPiece(t, "testnats.monitor-url/no-answer", err.Error(), user, password, token)
		if want := strings.TrimPrefix(dead.URL, "http://") + "/prefix/connz"; !strings.Contains(err.Error(), want) {
			t.Errorf("connectionsAt = %q; want it to name %s, the route asked", err, want)
		}
	})
	for _, c := range []struct{ name, monitoring, want string }{
		{"setting", credentialed("http://127.0.0.1:1"), externalURLEnv + ` is "", ` + externalMonitorEnv + " is http://127.0.0.1:1/prefix)"},
		{"setting-unparsed", "http://" + user + ":" + password + "@127.0.0.1:1/%zz?access_token=" + token, externalURLEnv + ` is "", ` + externalMonitorEnv + " is (not printed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(externalURLEnv, "")
			t.Setenv(externalMonitorEnv, c.monitoring)
			_, _, err := external()
			if err == nil {
				t.Fatal("external took a monitoring URL with no client URL")
			}
			oauth2calltest.RequireNoPiece(t, "testnats.monitor-url/"+c.name, err.Error(), user, password, token)
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("external = %q; want it to name each setting, %q", err, c.want)
			}
		})
	}
}
