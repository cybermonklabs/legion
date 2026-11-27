package storetest

import (
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// TestATemplateMigratesWithTheBoundLeftWhenItIsMade: the connection string a template's migrate is
// given bounds its connects by the seconds left, when the template is made, until the binary's
// deadline less testwait's margin, not by a bound fixed when the admin pool opened, which a
// template first asked for later would carry past that end (review 6049526743 on
// cybermonklabs/legion#5189). The row opens the admin pool, lets two seconds pass, reads the
// seconds left, and asks for a template of its own: the migrate's connect_timeout must be named
// and at most those seconds. A bound taken when the pool opened is at least two seconds more; a
// string with no bound names none.
func TestATemplateMigratesWithTheBoundLeftWhenItIsMade(t *testing.T) {
	ready(t)
	end, bounded := testdb.ConnectEnd(t)
	if !bounded {
		t.Skip("the row needs a -timeout: without one no connect has an end")
	}
	time.Sleep(2 * time.Second)
	left := int64(time.Until(end) / time.Second)
	var given string
	OpenFrom(t, "template_bound_test", func(databaseURL string) error {
		given = databaseURL
		return nil
	})
	parsed, err := url.Parse(given)
	if err != nil {
		t.Fatal("parse the URL the migrate was given") // its error can quote the URL, which carries a credential
	}
	raw := parsed.Query().Get("connect_timeout")
	bound, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("the migrate's URL names connect_timeout=%q; want the whole seconds left until the connect end", raw)
	}
	if bound > left || bound < 1 {
		t.Fatalf("the migrate's connect_timeout is %d s; want at least 1 and at most the %d s left when the template was asked for", bound, left)
	}
	t.Logf("the template's migrate was given connect_timeout=%d s with %d s left before it", bound, left)
}
