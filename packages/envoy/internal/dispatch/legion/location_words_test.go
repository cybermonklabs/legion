package legion

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects"
)

// TestAStateReadPrintsNoLocationItCannotParse: a daemon, or whatever answers at its URL, that
// redirects the state read to a Location net/url cannot parse, with redacttest's token in the
// Location's query. net/http reads the Location before any redirect policy is asked and fails the
// call with an error that quotes it as it came (twin). The read's failure, which legion_reads
// keeps and the board's header shows, names the route and the failure's class, none of the
// Location (unparsed). The control: the failure still wraps ErrUnavailable and names the route.
// Captures legion.state-location/unparsed.
func TestAStateReadPrintsNoLocationItCannotParse(t *testing.T) {
	location := "/moved%zz?access" + "_token=" + redacttest.Token
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(daemon.Close)
	t.Run("twin", func(t *testing.T) {
		response, err := redirects.Refused(&http.Client{}).Get(daemon.URL + StatePath)
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("net/http took a Location it cannot parse; the row needs the error that quotes it")
		}
		if !strings.Contains(err.Error(), redacttest.Token) {
			t.Fatalf("net/http's error is %q; want the Location quoted with the token, so the row can see it printed", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		_, _, err := New(daemon.URL).State(t.Context())
		if err == nil {
			t.Fatal("the state read took a redirect to a Location that does not parse")
		}
		redacttest.RequireNoPiece(t, "legion.state-location/unparsed", err.Error())
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "GET "+StatePath+": ") {
			t.Errorf("the read failed with %q; want ErrUnavailable naming GET %s", err, StatePath)
		}
	})
}
