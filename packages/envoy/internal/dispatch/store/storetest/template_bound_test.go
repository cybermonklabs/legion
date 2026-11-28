package storetest

import (
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// deadlineAt is t with its Deadline answering at, the deadline testdb.ConnectEnd reads its connect
// end from, for a row that hands a call a connect end of its own. Every other method is t's.
type deadlineAt struct {
	*testing.T
	at time.Time
}

func (d deadlineAt) Deadline() (time.Time, bool) { return d.at, true }

// TestATemplateMigratesWithTheBoundLeftWhenItIsMade: the connection string a template's migrate is
// given bounds its connects by the seconds left, when the template is made, until the connect end
// of the test that asks for it (testdb.ConnectEnd, the binary's deadline less testwait's margin),
// not by a bound fixed when the admin pool opened, which a template first asked for later would
// carry past that end (review 6049526743 on cybermonklabs/legion#5189). The row opens the admin
// pool under the binary's own connect end, then asks for a template of its own under a connect end
// it stages 2 s before that one (deadlineAt): the migrate's connect_timeout must be named and at
// most the seconds left until the row's end when the row staged it. A bound taken when the pool
// opened is the seconds left until the binary's end then, at least 2 s more however long the
// machine took between the two, and a string with no bound names none. No verdict reads how long
// anything took: the product reads the row's end on the same clock, later, so it reads at most
// what the row read.
func TestATemplateMigratesWithTheBoundLeftWhenItIsMade(t *testing.T) {
	ready(t)
	end, bounded := testdb.ConnectEnd(t)
	if !bounded {
		t.Skip("the row needs a -timeout: without one no connect has an end")
	}
	staged := time.Now()
	left := int64(end.Sub(staged)/time.Second) - 2
	if left < 1 {
		t.Skipf("the binary's connect end is %s away, too close for a connect end of the row's own 2 s before it", end.Sub(staged).Round(time.Millisecond))
	}
	asking := deadlineAt{T: t, at: staged.Add(time.Duration(left)*time.Second + testwait.Margin)}
	var given string
	// A key of this run's own, so a later run of the row in the binary (-count) makes a template too.
	OpenFrom(asking, randomName(t, "template_bound_test_"), func(databaseURL string) error {
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
		t.Fatalf("the migrate's connect_timeout is %d s; want at least 1 and at most the %d s left until the row's connect end when it asked for the template", bound, left)
	}
	t.Logf("the template's migrate was given connect_timeout=%d s with %d s left until the row's connect end", bound, left)
}
