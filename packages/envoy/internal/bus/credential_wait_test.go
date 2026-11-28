package bus

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// fakeStartClock is a start's clock (startClock) that only the start and its tries move: a wait
// moves it by the wait, plus late[i] for the i'th wait when late names one, as a process the
// scheduler wakes late from a wait finds the clock, and a try by the time the try takes. The
// start's loop runs on the test's own goroutine, so each wait ends as it is asked for.
type fakeStartClock struct {
	at    time.Time
	late  []time.Duration
	slept []time.Duration
}

func (c *fakeStartClock) Now() time.Time { return c.at }

// After moves the clock past the wait, and answers a channel that already holds the moment it
// reached.
func (c *fakeStartClock) After(wait time.Duration) <-chan time.Time {
	if i := len(c.slept); i < len(c.late) {
		c.at = c.at.Add(c.late[i])
	}
	c.slept = append(c.slept, wait)
	c.at = c.at.Add(wait)
	reached := make(chan time.Time, 1)
	reached <- c.at
	return reached
}

// scriptedTry is one try of a start in a row of TestACredentialWaitWaitsOutRefusalsUpToItsBound:
// what it returns, and how long it takes.
type scriptedTry struct {
	err  error
	took time.Duration
}

// The schedule of a start that waits out refusals of its credential (waitOutCredentialRefusals), on
// a clock only the start moves: it waits 1 s after the first refusal and doubles each wait up to
// 10 s, cuts the last wait to end at its bound, counted from its first try, and lets a try in
// progress at the bound finish and succeed. It begins no try after the bound on a refusal: a wait
// the clock wakes it from past the bound ends the start, except that the try at the bound, the one
// the wait cut to the bound leads to, may begin up to startTryLateness late. A refusal of the
// stream's ensure counts as one of the connect. Any other error counts toward main's ten tries in
// a row, each followed by main's wait of a second, the tenth too, whatever the bound, so no start
// ends sooner than main's would; a refusal ends such a row.
func TestACredentialWaitWaitsOutRefusalsUpToItsBound(t *testing.T) {
	refusal := fmt.Errorf("dial: %w", nats.ErrAuthorization)
	denied := fmt.Errorf("ensure stream: %w: nats: Permissions Violation for Publish to \"$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS\"", ErrPublishDenied)
	other := errors.New("nats: no servers available for connection")
	repeat := func(n int, try scriptedTry) []scriptedTry { return slices.Repeat([]scriptedTry{try}, n) }
	second := time.Second
	for _, row := range []struct {
		name  string
		bound time.Duration
		// tries are the tries' outcomes in order; a start that tries past the last one gets the last
		// one again.
		tries []scriptedTry
		// late is how far past its end the clock wakes the start from each of its waits, in order.
		late      []time.Duration
		wantSlept []time.Duration
		// wantTries is how many tries the start makes, and wantErr what its error wraps (nil: none).
		wantTries int
		wantErr   error
		// wantRefused is how many refusals the start reports to its caller.
		wantRefused int
	}{
		{
			name:        "refused until the bound",
			bound:       30 * time.Second,
			tries:       []scriptedTry{{err: refusal}},
			wantSlept:   []time.Duration{second, 2 * second, 4 * second, 8 * second, 10 * second, 5 * second},
			wantTries:   7,
			wantErr:     nats.ErrAuthorization,
			wantRefused: 7,
		},
		{
			name:  "a try in progress at the bound succeeds",
			bound: 30 * time.Second,
			tries: append(repeat(6, scriptedTry{err: refusal}), scriptedTry{took: 3 * time.Second}),
			// The seventh try starts at the bound and takes three seconds past it.
			wantSlept:   []time.Duration{second, 2 * second, 4 * second, 8 * second, 10 * second, 5 * second},
			wantTries:   7,
			wantRefused: 6,
		},
		{
			name:        "a refusal of the stream's ensure is waited out",
			bound:       30 * time.Second,
			tries:       []scriptedTry{{err: denied}, {err: denied}, {}},
			wantSlept:   []time.Duration{second, 2 * second},
			wantTries:   3,
			wantRefused: 2,
		},
		{
			name:  "other errors end the start after ten in a row",
			bound: 180 * time.Second,
			tries: []scriptedTry{{err: other}},
			// Main's second after each of the ten, the tenth too.
			wantSlept: slices.Repeat([]time.Duration{second}, 10),
			wantTries: 10,
			wantErr:   other,
		},
		{
			name:  "a refusal ends a row of other errors",
			bound: 180 * time.Second,
			tries: slices.Concat(repeat(9, scriptedTry{err: other}), []scriptedTry{{err: refusal}}, repeat(9, scriptedTry{err: other}), []scriptedTry{{}}),
			wantSlept: slices.Concat(
				slices.Repeat([]time.Duration{second}, 9), []time.Duration{second},
				slices.Repeat([]time.Duration{second}, 9)),
			wantTries:   20,
			wantRefused: 1,
		},
		{
			name:  "other errors after a refusal still make main's ten tries past the bound",
			bound: 5 * time.Second,
			tries: []scriptedTry{{err: refusal}, {err: other}},
			// One wait of a second after the refusal, then main's second after each of the ten
			// other errors, the tenth too: tries at 0 to 10 s, the last five past the bound, and the
			// start ends at 11 s.
			wantSlept:   slices.Repeat([]time.Duration{second}, 11),
			wantTries:   11,
			wantErr:     other,
			wantRefused: 1,
		},
		{
			name:      "a start begun with half a second of its bound left still makes main's ten tries",
			bound:     500 * time.Millisecond,
			tries:     []scriptedTry{{err: other}},
			wantSlept: slices.Repeat([]time.Duration{second}, 10),
			wantTries: 10,
			wantErr:   other,
		},
		{
			name:  "a wait the clock wakes the start from past the bound begins no try",
			bound: 3 * time.Second,
			tries: []scriptedTry{{err: refusal}, {}},
			// The wait of a second after the first refusal ends at 11 s.
			late:        []time.Duration{10 * time.Second},
			wantSlept:   []time.Duration{second},
			wantTries:   1,
			wantErr:     nats.ErrAuthorization,
			wantRefused: 1,
		},
		{
			name:  "the try at the bound begins though its wait ends a little late",
			bound: 3 * time.Second,
			tries: []scriptedTry{{err: refusal}, {err: refusal}, {}},
			// The wait of 2 s after the second refusal is the one cut to end at the bound, and it
			// ends 200 ms past it.
			late:        []time.Duration{0, 200 * time.Millisecond},
			wantSlept:   []time.Duration{second, 2 * second},
			wantTries:   3,
			wantRefused: 2,
		},
		{
			name:        "the try at the bound begins no later than the start's lateness allows",
			bound:       3 * time.Second,
			tries:       []scriptedTry{{err: refusal}, {err: refusal}, {}},
			late:        []time.Duration{0, startTryLateness + time.Millisecond},
			wantSlept:   []time.Duration{second, 2 * second},
			wantTries:   2,
			wantErr:     nats.ErrAuthorization,
			wantRefused: 2,
		},
		{
			name:  "an error that ends the start ends it at once, as a failed start check's does",
			bound: 30 * time.Second,
			tries: []scriptedTry{{err: refusal}, {err: endsStart{other}}, {}},
			// After one wait, the second try fails in a way no wait repairs (a start check's error
			// that is no refusal, such as a stream the server does not hold): no third try.
			wantSlept:   []time.Duration{second},
			wantTries:   2,
			wantErr:     other,
			wantRefused: 1,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			clock := &fakeStartClock{at: time.Date(2026, 11, 22, 0, 3, 50, 0, time.UTC), late: row.late}
			made, tries, refused := &Client{}, 0, 0
			client, err := waitOutCredentialRefusals(row.bound, func(error, time.Duration) { refused++ }, clock, func() (*Client, error) {
				try := row.tries[min(tries, len(row.tries)-1)]
				tries++
				clock.at = clock.at.Add(try.took)
				if try.err != nil {
					return nil, try.err
				}
				return made, nil
			})
			if tries != row.wantTries {
				t.Errorf("the start made %d tries, want %d", tries, row.wantTries)
			}
			if !slices.Equal(clock.slept, row.wantSlept) {
				t.Errorf("the start waited %v, want %v", clock.slept, row.wantSlept)
			}
			if refused != row.wantRefused {
				t.Errorf("the start reported %d refusals, want %d", refused, row.wantRefused)
			}
			switch {
			case row.wantErr == nil && (err != nil || client != made):
				t.Fatalf("the start returned a client (%t) and %v; want the client its last try made", client != nil, err)
			case row.wantErr != nil && (err == nil || !errors.Is(err, row.wantErr)):
				t.Fatalf("the start returned a client (%t) and %v; want no client and an error wrapping %v", client != nil, err, row.wantErr)
			}
		})
	}
}

// A start that ends at its bound on a refusal names the bound and the refusal, and one that ends
// after main's ten other errors in a row, past its bound, names the last of them and the last
// refusal before them.
func TestACredentialWaitThatEndsNamesTheLastRefusal(t *testing.T) {
	refusal := fmt.Errorf("dial: %w", nats.ErrAuthorization)
	other := errors.New("nats: no servers available for connection")
	for _, row := range []struct {
		name string
		// try is the error of try n, from 1.
		try  func(n int) error
		want []string
		is   []error
	}{
		{
			name: "refused until the bound",
			try:  func(int) error { return refusal },
			want: []string{"5s", refusal.Error()},
			is:   []error{nats.ErrAuthorization},
		},
		{
			name: "refused once, then ten other errors",
			try: func(n int) error {
				if n == 1 {
					return refusal
				}
				return other
			},
			want: []string{refusal.Error(), other.Error()},
			is:   []error{nats.ErrAuthorization, other},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			clock := &fakeStartClock{at: time.Date(2026, 11, 22, 0, 3, 50, 0, time.UTC)}
			tries := 0
			_, err := waitOutCredentialRefusals(5*time.Second, nil, clock, func() (*Client, error) {
				tries++
				return nil, row.try(tries)
			})
			for _, want := range row.is {
				if !errors.Is(err, want) {
					t.Fatalf("the start ended with %v, want an error wrapping %v", err, want)
				}
			}
			for _, want := range row.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the start ended with %q, want it to name %q", err, want)
				}
			}
		})
	}
}
