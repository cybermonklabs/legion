package testdb

import (
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// fatalRecorder is a testing.TB whose Fatalf records its text and ends the goroutine that called
// it, as testing's own does, so a test can read what a helper's fatal failure would print.
type fatalRecorder struct {
	testing.TB
	said string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.said = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// TestAnAdminURLThatDoesNotParsePrintsNoneOfIt: DatabaseOn's refusal of an admin URL url.Parse
// cannot read prints no run of the URL's user, password or token. url.Parse's own error quotes the
// URL whole, and the error it wraps is no safer: a URL whose user information names no host after
// it is read with the password as the port, and the wrapped error is `invalid port
// ":<password>" after host`. The refusal now prints the failure's class alone. The twin shows the
// wrapped error holds the password. Captures testdb.admin-url/no-host.
func TestAnAdminURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	noHost := "postgres://" + redacttest.User + ":" + redacttest.Password + "/dispatch?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		_, err := url.Parse(noHost)
		if err == nil {
			t.Fatalf("url.Parse took %q; the row needs a URL it refuses", noHost)
		}
		var unwrapped string
		if inner := (err.(*url.Error)).Err; inner != nil {
			unwrapped = inner.Error()
		}
		if !strings.Contains(unwrapped, redacttest.Password) {
			t.Errorf("the error url.Parse wraps is %q; want the password in it, so the row can see it printed", unwrapped)
		}
	})
	t.Run("no-host", func(t *testing.T) {
		recorder := &fatalRecorder{TB: t}
		done := make(chan struct{})
		go func() {
			defer close(done)
			DatabaseOn(recorder, noHost, "admin_url_words")
		}()
		<-done
		if recorder.said == "" {
			t.Fatal("DatabaseOn took an admin URL url.Parse refuses; want a fatal refusal")
		}
		redacttest.RequireNoPiece(t, "testdb.admin-url/no-host", recorder.said, redacttest.User, redacttest.Password, redacttest.Token)
		if want := "parse the URL of the test database"; !strings.Contains(recorder.said, want) {
			t.Errorf("the refusal is %q; want it to say %q", recorder.said, want)
		}
	})
}
