package bus

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// The waits of a start that waits out a refusal of its credential (WithCredentialWait): a second
// after the first refusal, each later one twice the one before, up to ten seconds. A refusal that
// lasts is a grant or a server configuration still on its way, as the hub's servers reload a new
// nkey only once the kubelet has refreshed their mounted configuration, about a minute after it
// changed. The doubling keeps a start that waits minutes from trying a server every second, and
// the cap keeps its next try within ten seconds of the reload that admits it.
const (
	credentialRetryFirst = time.Second
	credentialRetryCap   = 10 * time.Second
)

// credentialRefusal reports whether err, the error of a try of a start (startClient, and the start
// check WithStartCheck sets), is a refusal of the client's credential: the server refusing the
// connect (authorizationRefusal), or a request of the stream's ensure, or of the start check, that
// the credential's grant does not cover (ErrPublishDenied, the server's permissions violation,
// recordPublishDenial).
func credentialRefusal(err error) bool {
	return authorizationRefusal(err) || errors.Is(err, ErrPublishDenied)
}

// endsStart is the error of a try of a start that no wait repairs, so the start ends at it at once:
// a start check's error that is no refusal of the credential (WithStartCheck), as a stream the
// server does not hold is to an edge listener, which may not create it. It reads as the error it
// carries.
type endsStart struct{ err error }

func (e endsStart) Error() string { return e.err.Error() }
func (e endsStart) Unwrap() error { return e.err }

// startClock is the clock a start that waits out refusals reads and waits on: wallStartClock, or a
// test's own, which only the start and its tries move.
type startClock struct {
	now   func() time.Time
	sleep func(time.Duration)
}

// wallStartClock is the process's clock.
var wallStartClock = startClock{now: time.Now, sleep: time.Sleep}

// startTryLateness is how late past its bound a start that waits out refusals of its credential
// may begin the try at the bound, the one the wait after a refusal, cut to end at the bound, leads
// to (waitOutCredentialRefusals). A sleep on the process's clock ends after its wait, never at it,
// by as long as the scheduler takes to run the start again, which on a loaded machine can be tens
// of milliseconds or more; without this, the try the bound schedules would never begin. Any other
// try after a refusal's wait, and the try at the bound past this, is not begun: the start ends
// instead. One second is the scheduling margin the listener's exit at its bound is held to beside
// a try's connectTimeout.
const startTryLateness = time.Second

// waitOutCredentialRefusals runs try, one try of a start, until it succeeds, and answers what it
// made. After a refusal of the credential (credentialRefusal) it waits credentialRetryFirst before
// the next try and doubles each wait up to credentialRetryCap, each wait cut to end at bound,
// counted from the first try; a try in progress at the bound may finish, its success counting, and
// no try begins after the bound on a refusal: it reads the clock as each such wait ends, before
// the try after it begins, and ends the start when the bound has passed. The one try a refusal's
// wait lets begin past the bound is the try at the bound, the one the wait cut to end at the bound
// leads to, which the clock may wake it for up to startTryLateness late.
//
// After any other error, a dial's that no wait for the credential repairs, it waits
// connectRetryWait, the wait after each failed dial of a start without the wait, the tenth too
// (connectTrying), and such errors count toward connectTries in a row whatever the bound, so no
// start ends on them sooner than one without the wait would: it ends where that start ends, a
// second after its tenth failed dial, and a connect begun with little of its bound left, as a later
// connect of the listener's start can be, still makes those ten tries. A refusal ends such a row.
// The start ends with the error of its last try at the first of the two limits, naming the last
// refusal (credentialWaitEnded, withLastRefusal), and at once on an error that no wait repairs
// (endsStart), with the error it carries, naming the last refusal too.
//
// refused, when not nil, is told of each refusal as it comes, with the wait before the next try is
// due, or zero when the start ends on it at once. A wait that ends past the bound ends the start
// after refused was told of its wait.
func waitOutCredentialRefusals(bound time.Duration, refused func(err error, next time.Duration), clock startClock, try func() (*Client, error)) (*Client, error) {
	end := clock.now().Add(bound)
	retry := credentialRetryFirst
	var lastRefusal error
	others := 0
	for {
		client, err := try()
		if err == nil {
			return client, nil
		}
		var final endsStart
		if errors.As(err, &final) {
			return nil, withLastRefusal(final.err, lastRefusal)
		}
		if !credentialRefusal(err) {
			others++
			clock.sleep(connectRetryWait)
			if others == connectTries {
				return nil, withLastRefusal(err, lastRefusal)
			}
			continue
		}
		lastRefusal, others = err, 0
		next := retry
		retry = min(retry*2, credentialRetryCap)
		left := end.Sub(clock.now())
		if left <= 0 {
			if refused != nil {
				refused(err, 0)
			}
			return nil, credentialWaitEnded(bound, err)
		}
		atBound := next >= left
		next = min(next, left)
		if refused != nil {
			refused(err, next)
		}
		clock.sleep(next)
		if late := clock.now().Sub(end); late > 0 && (!atBound || late > startTryLateness) {
			return nil, credentialWaitEnded(bound, err)
		}
	}
}

// credentialWaitEnded is the error a start that waited out refusals of its credential ends with at
// its bound: refusal, the refusal its last try met. Only a refusal ends a start at its bound; a
// try that failed otherwise counts toward the ten tries in a row of a start without the wait
// (waitOutCredentialRefusals), whose last error the start ends with as it is (withLastRefusal), so
// no words of the credential wait reach a start that ended for another reason.
func credentialWaitEnded(bound time.Duration, refusal error) error {
	return fmt.Errorf("bus: the NATS server still refused the credential when the start's %s wait ran out: %w", bound, refusal)
}

// withLastRefusal is err, the error of the last of connectTries tries in a row that failed other
// than by a refusal, naming refusal, the last refusal of the credential before them, when there
// was one.
func withLastRefusal(err, refusal error) error {
	if refusal == nil {
		return err
	}
	return fmt.Errorf("%w (last refusal of the credential: %w)", err, refusal)
}

// CredentialName names the NATS user the environment names its credential as (credential), for
// the line an operator reads to find the user a server refused: "nkey user" and the public key of
// a seed (NATS_NKEY_SEED_FILE or NATS_NKEY_SEED), the variable that names a service's token, "no
// credential" when none is set, and the reason a credential is unusable. It never carries a seed
// or a token: nats.Nkey keeps the public key in the options it sets, which is all this reads of
// the seed's option.
func CredentialName(environment func(string) (string, bool)) string {
	option, err := credential(environment)
	switch {
	case err != nil:
		return "an unusable credential (" + err.Error() + ")"
	case option == nil:
		return "no credential"
	}
	if file, set := environment(ServiceTokenFileEnvVar); set {
		return "the service token " + ServiceTokenFileEnvVar + " names (" + file + ")"
	}
	if audience, set := environment(ServiceGoogleAudienceEnvVar); set {
		return "the Google identity token for the audience " + strings.TrimSpace(audience)
	}
	var options nats.Options
	if err := option(&options); err != nil || options.Nkey == "" {
		return "an nkey user whose public key could not be read"
	}
	return "nkey user " + options.Nkey
}
