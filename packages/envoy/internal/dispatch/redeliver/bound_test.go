package redeliver_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
)

// The sweep writes GitHub's text into its lines and its errors: a delivery's GUID, its event and
// its action, the keys of its bucket, every one of which holds a GUID, and GitHub's own answers to
// its requests: an error's body, a next page link, a value of a 200 listing it cannot decode, and
// the text of a transport error, which quotes what GitHub's answer held. GitHub bounds none of
// them, so each is shown as githubapp.ShownBody shows GitHub's text: whole up to 512 runes, and
// past that its first 512 runes and `… (<n> bytes)`. Each place names the size of the text it cut:
//
//   - The sweep cuts each GUID, event, action and key alone, with its own size.
//   - An error of the bucket quotes a key whole. The sweep cuts that error whole, with the size of
//     the error's text, as githubapp.ShownErr cuts any other error that quotes GitHub's text.
//   - The githubapp client cuts GitHub's answers once, where each enters the error it answers the
//     sweep with. It cuts a refusal's body and a next page link alone, each with its own size
//     (requireCut).
//   - The client cuts a value of an answer it cannot decode alone too, with the value's size, and
//     the decode error still names the layout (requireCut, inTimeQuotes). The reason of a time
//     parse error quotes the text after a valid time, and the client cuts it alone too, with the
//     reason's size. The cut reason ends the error (requireBounded).
//   - A transport error of the client cuts its URL, with the URL's size, and its cause. A line the
//     cause quotes is cut with the cause's text, with the size of the cause's text
//     (requireBounded).
//
// The tests here give each GUID, event and action boundRunes runes, past that bound, and keep the
// GUID within nats' key alphabet so that it reaches every site. They give each of GitHub's answers
// far more than that. They hold every line and error that names one to the bound and never to the
// whole.

// boundRunes is how long each GUID, event and action of the tests is, in runes and in bytes alike.
const boundRunes = 600

// longText is boundRunes bytes of unit over and over, cut where the count runs out. unit is ASCII,
// so each byte is a rune.
func longText(unit string) string {
	return strings.Repeat(unit, boundRunes/len(unit)+1)[:boundRunes]
}

// gitHubTexts is one delivery's text as GitHub sends it, each of the three boundRunes long and the
// GUID within nats' key alphabet.
type gitHubTexts struct{ guid, event, action string }

func textsOf(name string) gitHubTexts {
	return gitHubTexts{
		guid:   longText("guid-" + name + "-"),
		event:  longText("event-" + name + "-"),
		action: longText("action-" + name + "-"),
	}
}

func (g gitHubTexts) whole() []string { return []string{g.guid, g.event, g.action} }

// inLine is each of the delivery's texts as line shows it, in the attributes every line about one
// delivery carries.
func (g gitHubTexts) inLine(line, what string) []sighting {
	return []sighting{
		{where: "the guid of " + what, shown: lineAttr(line, "guid"), text: g.guid, alone: true},
		{where: "the event of " + what, shown: lineAttr(line, "event"), text: g.event, alone: true},
		{where: "the action of " + what, shown: lineAttr(line, "action"), text: g.action, alone: true},
	}
}

// failText records a failed attempt GitHub made of the delivery texts names, which the receiver
// answered with code, ago before now.
func (h *harness) failText(texts gitHubTexts, code int, ago time.Duration) githubapptest.Delivery {
	return h.webhook.Record(githubapptest.Attempt{GUID: texts.guid, Event: texts.event, Payload: []byte(`{}`)}, texts.action, 7, code, h.clock.Now().Add(-ago))
}

// linesOf is every line of the sweep's log that carries msg.
func (h *harness) linesOf(msg string) []string {
	var lines []string
	for _, line := range strings.Split(h.logs.String(), "\n") {
		if strings.Contains(line, `msg="`+msg+`"`) {
			lines = append(lines, line)
		}
	}
	return lines
}

// oneLine is the one line of the sweep's log that carries msg, and fails the test unless there is
// exactly one.
func (h *harness) oneLine(msg string) string {
	h.t.Helper()
	lines := h.linesOf(msg)
	if len(lines) != 1 {
		h.t.Fatalf("the sweep logged %d lines %q, want one; the log is %d bytes", len(lines), msg, len(h.logs.String()))
	}
	return lines[0]
}

// lineAttr is the value of the attribute name in line, as slog's text handler wrote it unquoted,
// and empty when line carries none.
func lineAttr(line, name string) string {
	at := strings.Index(line, " "+name+"=")
	if at < 0 {
		return ""
	}
	rest := line[at+len(name)+2:]
	if !strings.HasPrefix(rest, `"`) {
		value, _, _ := strings.Cut(rest, " ")
		return value
	}
	quoted, err := strconv.QuotedPrefix(rest)
	if err != nil {
		return ""
	}
	value, err := strconv.Unquote(quoted)
	if err != nil {
		return ""
	}
	return value
}

// sighting is one place that shows a reader a text of GitHub's: where it is, what it shows, and
// the text. alone is a place that shows that text and nothing else, a line's attribute; any other
// is a text that holds it among words of its own, an error. quote is how that error writes the
// text where it does not write the text as it is, and nil where it does: a time.ParseError quotes
// its value (inTimeQuotes).
type sighting struct {
	where, shown, text string
	alone              bool
	quote              func(string) string
}

// inTimeQuotes is text as the text of a time.ParseError quotes a value: in double quotes, with
// each byte of a rune past ASCII written as \xNN, so the `…` of githubapp.ShownBody shows as
// \xe2\x80\xa6. Package time writes it: the text of a ParseError with a Message is
// `parsing time "<Value>"` and then the Message.
func inTimeQuotes(text string) string {
	quoted := (&time.ParseError{Value: text, Message: "."}).Error()
	return strings.TrimSuffix(strings.TrimPrefix(quoted, "parsing time "), ".")
}

// requireCut fails the test unless s shows its text as githubapp.ShownBody shows it, its first
// 512 runes and `… (<n> bytes)`, n the size of the text, and never the text whole. Where s.quote
// is set, s shows that cut as s.quote writes it.
func requireCut(t *testing.T, s sighting) {
	t.Helper()
	cut := githubapp.ShownBody(s.text)
	if cut == s.text {
		t.Fatalf("%s: the text is %d bytes, which githubapp.ShownBody shows whole; the test needs one it cuts", s.where, len(s.text))
	}
	ending := cut[strings.LastIndex(cut, "…"):]
	if s.quote != nil {
		cut = s.quote(cut)
	}
	switch {
	case s.shown == "":
		t.Errorf("%s is missing; want the %d-byte text as githubapp.ShownBody shows it, ending %q", s.where, len(s.text), ending)
	case strings.Contains(s.shown, s.text):
		t.Errorf("%s holds the %d-byte text whole; want it as githubapp.ShownBody shows it, %d bytes ending %q", s.where, len(s.text), len(cut), ending)
	case s.alone && s.shown != cut:
		t.Errorf("%s is %.60q… (%d bytes); want the text as githubapp.ShownBody shows it, %d bytes ending %q", s.where, s.shown, len(s.shown), len(cut), ending)
	case !s.alone && !strings.Contains(s.shown, cut):
		t.Errorf("%s (%d bytes) does not hold the text as githubapp.ShownBody shows it, %d bytes ending %q", s.where, len(s.shown), len(cut), ending)
	}
}

// shownMost is the longest text githubapp.ShownBody makes: 512 runes of at most utf8.UTFMax bytes
// each, and `… (<n> bytes)` for the largest n there is.
var shownMost = 512*utf8.UTFMax + len(fmt.Sprintf("… (%d bytes)", math.MaxInt))

// shownEnd is the end githubapp.ShownBody gives a text it cuts.
var shownEnd = regexp.MustCompile(`… \(\d+ bytes\)$`)

// requireBounded fails the test unless s shows its text in a cut that ends an error, of a text
// that holds GitHub's text among words of its own. Two texts of the githubapp client do so:
//
//   - The cause of a transport error, which quotes a line of the answer. The githubapp client
//     cuts the cause's whole text, as githubapp.ShownErr cuts any such error. The client keeps the
//     shape `<Op> "<URL>": <cause>` and shows the URL as githubapp.ShownBody shows it, so the cut
//     cause ends the transport error's text.
//   - The reason of a time parse error, its Message, which quotes the text after a valid time
//     (`: extra text: "<text>"`). The client cuts the reason as githubapp.ShownBody shows it, and
//     the reason ends the parse error's text (`parsing time "<Value>"` and then the Message).
//
// Such a sighting holds the start of the text, never the text whole, is no longer than
// githubapp.ShownBody makes any text (shownMost), and ends with ShownBody's `… (<n> bytes)`, n the
// size of the cause's text or of the reason. requireCut does not hold of it, since the cut is of
// a text that holds GitHub's among words of its own, and not of GitHub's text alone. The texts
// here are ASCII, so the words before the cut leave it far inside shownMost.
func requireBounded(t *testing.T, s sighting) {
	t.Helper()
	start := s.text[:min(len(s.text), 32)]
	switch {
	case s.shown == "":
		t.Errorf("%s is missing; want the %d-byte text's start %q, cut to a bound", s.where, len(s.text), start)
	case strings.Contains(s.shown, s.text):
		t.Errorf("%s (%d bytes) holds the %d-byte text whole; want it cut to a bound", s.where, len(s.shown), len(s.text))
	case len(s.shown) > shownMost:
		t.Errorf("%s is %d bytes; want at most %d, the longest text githubapp.ShownBody makes", s.where, len(s.shown), shownMost)
	case !shownEnd.MatchString(s.shown):
		t.Errorf("%s ends %q; want githubapp.ShownBody's `… (<n> bytes)`", s.where, s.shown[max(0, len(s.shown)-60):])
	case !strings.Contains(s.shown, start):
		t.Errorf("%s is %.200q…; want it to hold the start of the text, %q", s.where, s.shown, start)
	}
}

// requireNoneWhole fails the test if any line the sweep logged holds one of texts whole.
func (h *harness) requireNoneWhole(texts ...string) {
	h.t.Helper()
	logged := h.logs.String()
	for _, text := range texts {
		if n := strings.Count(logged, text); n > 0 {
			h.t.Errorf("the sweep's log holds GitHub's %d-byte text %.40q… whole %d times; want it only as githubapp.ShownBody shows it", len(text), text, n)
		}
	}
}

// sweeperOver is a sweeper whose bucket is h's reached through legacy, a handle a test wraps to
// inject a fault, and the bus.KeyValue it reaches it by.
func (h *harness) sweeperOver(legacy natsgo.KeyValue) (*redeliver.Sweeper, bus.KeyValue) {
	kv := bus.NewKeyValue(legacy, h.state.WatchHandle(), h.state.Streams())
	sweeper := h.sweeper()
	sweeper.Open = func(context.Context) (bus.KeyValue, error) { return kv, nil }
	return sweeper, kv
}

// refusingCall is the sweep's bucket refusing one call, on the keys under one prefix, with nats.go's
// refusal of a key outside its alphabet, which bus.KeyValue names with the key quoted whole
// (bus/kv.go's invalidKey): the class of error the bucket answers that holds a GUID. Every other
// call is the bucket's own.
type refusingCall struct {
	natsgo.KeyValue
	call, prefix string
}

func (r refusingCall) refuses(call, key string) bool {
	return call == r.call && strings.HasPrefix(key, r.prefix)
}

func (r refusingCall) Get(key string) (natsgo.KeyValueEntry, error) {
	if r.refuses("Get", key) {
		return nil, natsgo.ErrInvalidKey
	}
	return r.KeyValue.Get(key)
}

func (r refusingCall) Update(key string, value []byte, last uint64) (uint64, error) {
	if r.refuses("Update", key) {
		return 0, natsgo.ErrInvalidKey
	}
	return r.KeyValue.Update(key, value, last)
}

func (r refusingCall) Delete(key string, opts ...natsgo.DeleteOpt) error {
	if r.refuses("Delete", key) {
		return natsgo.ErrInvalidKey
	}
	return r.KeyValue.Delete(key, opts...)
}

// removeBucketOnRedelivery makes GitHub's answer to a redelivery request remove the sweep's bucket
// before it answers with status: the sweep has read the delivery's record and claimed it before the
// request, so the bucket refuses the write after it, the one that records GitHub's answer.
func (h *harness) removeBucketOnRedelivery(status int) {
	h.webhook.SetRedeliver(func(githubapptest.Attempt) int {
		if err := h.state.Streams().DeleteKeyValue(context.Background(), h.state.Bucket()); err != nil {
			h.t.Errorf("remove the sweep's bucket while GitHub redelivers: %v", err)
		}
		return status
	})
}

// Every line about one delivery carries its GUID, its event and its action, all three GitHub's, so
// the line a sweep writes when GitHub takes a redelivery request shows each to the bound.
func TestTheSweepsDecisionLinesShowGitHubsTextToABound(t *testing.T) {
	h := newHarness(t)
	texts := textsOf("redelivered")
	h.failText(texts, http.StatusServiceUnavailable, time.Minute)

	if got := outcome(h.sweep(redeliver.Options{}), texts.guid); got != redeliver.Redelivered {
		t.Fatalf("the sweep's outcome %q, want %q", got, redeliver.Redelivered)
	}
	for _, s := range texts.inLine(h.oneLine("webhook redelivered"), "the webhook redelivered line") {
		requireCut(t, s)
	}
	h.requireNoneWhole(texts.whole()...)
}

// A record write the bucket refuses after GitHub has answered the request is logged with the GUID
// it was for, and the claim that write was to end is logged with every text of the delivery. Here
// GitHub answers that it no longer has the delivery, which closes the record: a closed record has
// no index entry, so the record is the first thing the sweep writes after the request, and the
// bucket is gone by then. A write the bucket refused is no error of the sweep's: it leaves the
// sweep incomplete, and the delivery to the sweep after it (errNotWritten).
func TestARecordWriteFailureShowsTheGUIDToABound(t *testing.T) {
	h := newHarness(t)
	texts := textsOf("record")
	failed := h.failText(texts, http.StatusServiceUnavailable, time.Minute)
	h.removeBucketOnRedelivery(http.StatusServiceUnavailable)
	h.webhook.AnswerAfterRedelivering(failed.ID, http.StatusNotFound)

	report, err := h.sweeper().Sweep(context.Background(), redeliver.Options{})
	if err != nil {
		t.Fatalf("the sweep whose record write was refused answered %.200v; want no error and an incomplete sweep", err)
	}
	if got := outcome(report, texts.guid); got != redeliver.Expired {
		t.Fatalf("the sweep's outcome %q, want %q: the decision whose write after the request is the record's", got, redeliver.Expired)
	}
	if report.Complete {
		t.Fatalf("the sweep whose record write was refused reported itself complete")
	}
	line := h.oneLine("webhook redelivery record not written; the delivery is decided again by the next sweep")
	requireCut(t, sighting{where: "the guid of the record-not-written line", shown: lineAttr(line, "guid"), text: texts.guid, alone: true})
	notRecorded := h.oneLine("webhook redelivery outcome not recorded; the delivery is decided again once its claim expires")
	for _, s := range texts.inLine(notRecorded, "the outcome-not-recorded line") {
		requireCut(t, s)
	}
	h.requireNoneWhole(texts.whole()...)
}

// An index entry the bucket refuses after GitHub has answered the request is logged with the GUID
// it was for and the entry's key, which holds the GUID, and the claim that write was to end is
// logged with every text of the delivery. Here GitHub takes the request: the delivery's next
// request is due on a later minute than its claim's end, so its index entry moves, and the new
// entry is the first thing the sweep writes after the request. The clock is taken to half a
// minute past the minute so that the two minutes differ whatever second the test starts on.
func TestAnIndexEntryWriteFailureShowsTheGUIDToABound(t *testing.T) {
	h := newHarness(t)
	h.clock.advance(time.Duration((90-h.clock.Now().Second())%60) * time.Second)
	texts := textsOf("entry")
	h.failText(texts, http.StatusServiceUnavailable, time.Minute)
	h.removeBucketOnRedelivery(http.StatusOK)

	report, err := h.sweeper().Sweep(context.Background(), redeliver.Options{})
	if err != nil {
		t.Fatalf("the sweep whose index entry write was refused answered %.200v; want no error and an incomplete sweep", err)
	}
	if got := outcome(report, texts.guid); got != redeliver.Redelivered {
		t.Fatalf("the sweep's outcome %q, want %q: the decision whose write after the request is an index entry", got, redeliver.Redelivered)
	}
	if report.Complete {
		t.Fatalf("the sweep whose index entry write was refused reported itself complete")
	}
	line := h.oneLine("webhook redelivery index entry not written; the delivery is decided again by the next sweep")
	requireCut(t, sighting{where: "the guid of the entry-not-written line", shown: lineAttr(line, "guid"), text: texts.guid, alone: true})
	// The key is pending.<YYYYMMDD>.<HH>.<MM>.<GUID>: its time is read off the line, and the key
	// it starts is the GUID's.
	shownKey := lineAttr(line, "key")
	timed := len(redeliver.PendingKey(h.clock.Now(), ""))
	if !strings.HasPrefix(shownKey, redeliver.PendingPrefix) || len(shownKey) < timed {
		t.Fatalf("the key of the entry-not-written line is %.60q, want an index entry's key", shownKey)
	}
	requireCut(t, sighting{where: "the key of the entry-not-written line", shown: shownKey, text: shownKey[:timed] + texts.guid, alone: true})
	notRecorded := h.oneLine("webhook redelivery outcome not recorded; the delivery is decided again once its claim expires")
	for _, s := range texts.inLine(notRecorded, "the outcome-not-recorded line") {
		requireCut(t, s)
	}
	h.requireNoneWhole(texts.whole()...)
}

// A sweep given a window of its own reads every record, and refuses one stored under one GUID whose
// delivery names another. Both GUIDs are GitHub's, the key's and the delivery's, so the error shows
// each to the bound.
func TestARecordNamingAnotherDeliveryShowsBothGUIDsToABound(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now().UTC()
	stored, named := textsOf("stored"), textsOf("named")
	failed := h.failText(named, http.StatusServiceUnavailable, 2*time.Hour)
	record, err := json.Marshal(map[string]any{
		"attempts": 1, "refusals": 0, "last_attempt_at": now.Add(-2 * time.Hour),
		"next_attempt_at": now.Add(-time.Hour), "first_delivered_at": failed.DeliveredAt,
		"delivery": failed,
	})
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	if _, err := h.state.Create(t.Context(), "guid."+stored.guid, record); err != nil {
		t.Fatalf("store the record under the other GUID: %v", err)
	}

	_, err = h.sweeper().Sweep(context.Background(), redeliver.Options{Since: now.Add(-time.Hour)})
	if err == nil {
		t.Fatalf("the sweep read a record stored under one GUID whose delivery names another and answered no error")
	}
	for _, text := range []string{stored.guid, named.guid} {
		requireCut(t, sighting{where: "the error the sweep answered", shown: err.Error(), text: text})
	}
	h.requireNoneWhole(stored.guid, named.guid)
}

// Every other line and error of the sweep that names a delivery shows its GitHub text to the bound:
// the lines about one delivery, each with its GUID, event and action; the back-fill's lines and
// the errors they carry; the line of an index entry not deleted, with the entry's key; and the
// errors a record that does not decode or cannot be read answers, with the record's GUID. The
// errors the bucket answers quote the key they refused whole, and every key of a delivery holds
// its GUID, so a refusal of that class (refusingCall) is shown to the bound too.
func TestEveryOtherSweepLineNamingADeliveryShowsItToABound(t *testing.T) {
	notJSON := []byte("not a record")
	for _, tc := range []struct {
		name string
		// run makes the case on h for the delivery texts names, and answers every place it shows
		// one of GitHub's texts.
		run func(t *testing.T, h *harness, texts gitHubTexts) []sighting
	}{
		{"a refused request, then the delivery's recovery", func(t *testing.T, h *harness, texts gitHubTexts) []sighting {
			failed := h.failText(texts, http.StatusServiceUnavailable, time.Minute)
			h.webhook.Refuse(failed.ID, http.StatusUnprocessableEntity)
			if got := outcome(h.sweep(redeliver.Options{}), texts.guid); got != redeliver.RequestRefused {
				t.Fatalf("the first sweep's outcome %q, want %q", got, redeliver.RequestRefused)
			}
			h.webhook.Record(githubapptest.Attempt{GUID: texts.guid, Event: texts.event, Payload: []byte(`{}`)}, texts.action, 7, http.StatusOK, h.clock.Now())
			h.clock.advance(redeliver.Interval)
			if got := outcome(h.sweep(redeliver.Options{}), texts.guid); got != redeliver.Delivered {
				t.Fatalf("the second sweep's outcome %q, want %q", got, redeliver.Delivered)
			}
			return append(texts.inLine(h.oneLine("webhook redelivery request refused"), "the request-refused line"),
				texts.inLine(h.oneLine("webhook delivery recovered"), "the recovered line")...)
		}},
		{"an expiry", func(t *testing.T, h *harness, texts gitHubTexts) []sighting {
			failed := h.failText(texts, http.StatusServiceUnavailable, time.Minute)
			h.webhook.Refuse(failed.ID, http.StatusNotFound)
			if got := outcome(h.sweep(redeliver.Options{}), texts.guid); got != redeliver.Expired {
				t.Fatalf("the sweep's outcome %q, want %q", got, redeliver.Expired)
			}
			return texts.inLine(h.oneLine("webhook delivery expired undelivered"), "the expired line")
		}},
		{"a claim not undone, whose refusal quotes the record's key", func(t *testing.T, h *harness, texts gitHubTexts) []sighting {
			h.failText(texts, http.StatusServiceUnavailable, time.Minute)
			h.webhook.Limit(http.MethodPost, http.StatusTooManyRequests, http.Header{"Retry-After": {"60"}})
			// The claim creates the record; the write that puts it back after the rate limit is
			// an update, and the bucket refuses it.
			sweeper, kv := h.sweeperOver(refusingCall{KeyValue: h.state.Legacy(), call: "Update", prefix: "guid."})
			report, err := sweeper.Sweep(context.Background(), redeliver.Options{})
			if err != nil {
				t.Fatalf("the sweep at the rate limit: %.200v", err)
			}
			if got := outcome(report, texts.guid); got != redeliver.RateLimited {
				t.Fatalf("the sweep's outcome %q, want %q", got, redeliver.RateLimited)
			}
			_, refused := kv.Update(t.Context(), "guid."+texts.guid, []byte(`{}`), 1)
			if refused == nil || !strings.Contains(refused.Error(), texts.guid) {
				t.Fatalf("the bucket's refusal of the record's update is %.80v; want one that quotes the key", refused)
			}
			line := h.oneLine("webhook redelivery claim not undone after a rate limit; the delivery is decided again once its claim expires")
			return append(texts.inLine(line, "the claim-not-undone line"),
				sighting{where: "the error of the claim-not-undone line", shown: lineAttr(line, "error"), text: refused.Error(), alone: true})
		}},
		{"a live claim the back-fill could not index", func(t *testing.T, h *harness, texts gitHubTexts) []sighting {
			// The bucket as an older build leaves it: a cursor without this build's mark, so the
			// back-fill runs, and a record that build claimed with no index entry, whose failed
			// attempt is outside the window, so the back-fill is what reads it.
			now := h.clock.Now().UTC()
			if _, err := h.state.Put(t.Context(), "cursor", fmt.Appendf(nil, `{"at":%q}`, now.Format(time.RFC3339))); err != nil {
				t.Fatalf("write the cursor the older build left: %v", err)
			}
			failed := h.failText(texts, http.StatusServiceUnavailable, 2*time.Hour)
			claimed, err := json.Marshal(map[string]any{
				"attempts": 1, "refusals": 0, "last_attempt_at": now.Add(-2 * time.Hour),
				"next_attempt_at": now.Add(-time.Hour), "first_delivered_at": failed.DeliveredAt,
				"delivery": failed, "in_flight": true, "claimed_at": now,
			})
			if err != nil {
				t.Fatalf("encode the claimed record: %v", err)
			}
			if _, err := h.state.Create(t.Context(), "guid."+texts.guid, claimed); err != nil {
				t.Fatalf("write the claimed record: %v", err)
			}
			sweeper, _ := h.sweeperOver(refusingPending{KeyValue: h.state.Legacy(), refused: errors.New("nats: no responders available for request")})
			if _, err := sweeper.Sweep(context.Background(), redeliver.Options{}); err != nil {
				t.Fatalf("the sweep whose back-fill entry was refused: %.200v", err)
			}
			refused := h.oneLine("webhook redelivery index entry not written; the delivery is decided again by the next sweep")
			backfill := h.oneLine("webhook redelivery claim not back-filled into the index; the next sweep reads every record again")
			return []sighting{
				{where: "the guid of the entry-not-written line", shown: lineAttr(refused, "guid"), text: texts.guid, alone: true},
				{where: "the key of the entry-not-written line", shown: lineAttr(refused, "key"), text: redeliver.PendingKey(now.Add(claimDeadline), texts.guid), alone: true},
				{where: "the guid of the claim-not-back-filled line", shown: lineAttr(backfill, "guid"), text: texts.guid, alone: true},
				{where: "the error of the claim-not-back-filled line", shown: lineAttr(backfill, "error"), text: texts.guid},
			}
		}},
		{"a record the back-fill could not index", func(t *testing.T, h *harness, texts gitHubTexts) []sighting {
			now := h.clock.Now().UTC()
			if _, err := h.state.Put(t.Context(), "cursor", fmt.Appendf(nil, `{"at":%q}`, now.Format(time.RFC3339))); err != nil {
				t.Fatalf("write the cursor an older build left: %v", err)
			}
			failed := h.failText(texts, http.StatusServiceUnavailable, 2*time.Hour)
			if _, err := h.state.Create(t.Context(), "guid."+texts.guid, recordWithoutPending(t, now, failed)); err != nil {
				t.Fatalf("write the record an older build wrote: %v", err)
			}
			sweeper, _ := h.sweeperOver(refusingRecord{KeyValue: h.state.Legacy(), refused: errors.New("nats: no responders available for request")})
			if _, err := sweeper.Sweep(context.Background(), redeliver.Options{}); err != nil {
				t.Fatalf("the sweep whose back-fill record write was refused: %.200v", err)
			}
			backfill := h.oneLine("webhook redelivery record not back-filled into the index; the next sweep reads every record again")
			return []sighting{
				{where: "the guid of the record-not-back-filled line", shown: lineAttr(backfill, "guid"), text: texts.guid, alone: true},
				{where: "the error of the record-not-back-filled line", shown: lineAttr(backfill, "error"), text: texts.guid},
			}
		}},
		{"an index entry not deleted, whose refusal quotes its key", func(t *testing.T, h *harness, texts gitHubTexts) []sighting {
			// A stray entry naming a record that is gone, keyed before the margin the repair leaves,
			// inside the first sweep's window of three days.
			stray := redeliver.PendingKey(h.clock.Now().Add(-5*time.Minute), texts.guid)
			if _, err := h.state.Put(t.Context(), stray, nil); err != nil {
				t.Fatalf("write the stray index entry: %v", err)
			}
			sweeper, kv := h.sweeperOver(refusingCall{KeyValue: h.state.Legacy(), call: "Delete", prefix: redeliver.PendingPrefix})
			if _, err := sweeper.Sweep(context.Background(), redeliver.Options{}); err != nil {
				t.Fatalf("the sweep whose delete of a stray entry was refused: %.200v", err)
			}
			refused := kv.Delete(t.Context(), stray)
			if refused == nil || !strings.Contains(refused.Error(), texts.guid) {
				t.Fatalf("the bucket's refusal of the entry's delete is %.80v; want one that quotes the key", refused)
			}
			line := h.oneLine("webhook redelivery index entry not deleted")
			return []sighting{
				{where: "the key of the entry-not-deleted line", shown: lineAttr(line, "key"), text: stray, alone: true},
				{where: "the error of the entry-not-deleted line", shown: lineAttr(line, "error"), text: refused.Error(), alone: true},
			}
		}},
		{"a record that does not decode, read in a window of the sweep's own", func(t *testing.T, h *harness, texts gitHubTexts) []sighting {
			if _, err := h.state.Create(t.Context(), "guid."+texts.guid, notJSON); err != nil {
				t.Fatalf("write the record: %v", err)
			}
			_, err := h.sweeper().Sweep(context.Background(), redeliver.Options{Since: h.clock.Now().Add(-time.Hour)})
			if err == nil {
				t.Fatalf("the sweep read a record that does not decode and answered no error")
			}
			return []sighting{{where: "the error the sweep answered", shown: err.Error(), text: texts.guid}}
		}},
		{"a listed delivery whose record does not decode", func(t *testing.T, h *harness, texts gitHubTexts) []sighting {
			h.failText(texts, http.StatusServiceUnavailable, time.Minute)
			if _, err := h.state.Create(t.Context(), "guid."+texts.guid, notJSON); err != nil {
				t.Fatalf("write the record: %v", err)
			}
			_, err := h.sweeper().Sweep(context.Background(), redeliver.Options{})
			if err == nil {
				t.Fatalf("the sweep read a record that does not decode and answered no error")
			}
			return []sighting{{where: "the error the sweep answered", shown: err.Error(), text: texts.guid}}
		}},
		{"a listed delivery whose record read is refused, the refusal quoting its key", func(t *testing.T, h *harness, texts gitHubTexts) []sighting {
			h.failText(texts, http.StatusServiceUnavailable, time.Minute)
			sweeper, kv := h.sweeperOver(refusingCall{KeyValue: h.state.Legacy(), call: "Get", prefix: "guid."})
			_, err := sweeper.Sweep(context.Background(), redeliver.Options{})
			if err == nil {
				t.Fatalf("the sweep whose record read was refused answered no error")
			}
			// The error is cut for its reader and still carries the refusal's classes.
			if !errors.Is(err, bus.ErrInvalidKey) || !errors.Is(err, natsgo.ErrInvalidKey) {
				t.Fatalf("the error the sweep answered, %.200v, is not a bus.ErrInvalidKey and a nats.ErrInvalidKey", err)
			}
			_, refused := kv.Get(t.Context(), "guid."+texts.guid)
			if refused == nil || !strings.Contains(refused.Error(), texts.guid) {
				t.Fatalf("the bucket's refusal of the record's read is %.80v; want one that quotes the key", refused)
			}
			return []sighting{
				{where: "the error the sweep answered", shown: err.Error(), text: texts.guid},
				{where: "the bucket's refusal in the error the sweep answered", shown: err.Error(), text: refused.Error()},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			texts := textsOf("site")
			for _, s := range tc.run(t, h, texts) {
				requireCut(t, s)
			}
			h.requireNoneWhole(texts.whole()...)
		})
	}
}

// A GUID the bucket cannot key fails that delivery's every read or write, so a sweep leaves the
// delivery out with a line and goes on: it answers no error, it is complete, and it redelivers the
// ordinary delivery beside it. One GUID holds a space, which no key may hold, so its record cannot
// even be read; the other is within the alphabet and so long that its record's key can be read and
// its index entry's key, longer by the minute it is due at, cannot be written.
func TestASweepLeavesOutADeliveryWhoseGUIDTheBucketCannotKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		guid string
		// check fails the test unless the case's GUID stands where its name says.
		check func(t *testing.T, h *harness, guid string)
	}{
		{"a GUID holding a space", longText("guid with a space-"), func(t *testing.T, h *harness, guid string) {
			if err := h.state.RefusesRead("guid." + guid); !errors.Is(err, bus.ErrInvalidSubject) {
				t.Fatalf("a read of the record's key is refused with %.80v; the case needs a key no read takes", err)
			}
		}},
		// The bucket's keys go into subjects of at most 4032 bytes: a write's subject puts 90 bytes
		// around its key in this bucket, a read's 76, so a write takes keys of up to 3942 bytes and
		// a read keys of up to 3956. A GUID of 3930 bytes makes a record key of 3935 and an index
		// entry key of 3953.
		{"a GUID whose index entry is too long to write", strings.Repeat("g", 3930), func(t *testing.T, h *harness, guid string) {
			if err := h.state.RefusesRead("guid." + guid); err != nil {
				t.Fatalf("a read of the record's key is refused with %.80v; the case needs a key a read takes", err)
			}
			if _, err := h.state.Put(t.Context(), redeliver.PendingKey(h.clock.Now(), guid), nil); !errors.Is(err, bus.ErrTooLarge) {
				t.Fatalf("a write of the index entry's key answered %.80v; the case needs a key the bucket refuses as too long", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.check(t, h, tc.guid)
			ordinary := h.fail("guid-ordinary", http.StatusServiceUnavailable, 2*time.Minute)
			// Newer than the ordinary delivery, so the listing returns it first.
			h.webhook.Record(githubapptest.Attempt{GUID: tc.guid, Event: "pull_request_review", Payload: []byte(`{}`)}, "submitted", 7, http.StatusServiceUnavailable, h.clock.Now().Add(-time.Minute))

			report, err := h.sweeper().Sweep(context.Background(), redeliver.Options{})
			if err != nil {
				t.Fatalf("the sweep answered %.200v; want it to leave the delivery out and go on", err)
			}
			if !report.Complete {
				t.Fatalf("the sweep ended incomplete; want it complete, with the delivery it cannot key left out")
			}
			if got := outcome(report, "guid-ordinary"); got != redeliver.Redelivered {
				t.Fatalf("the ordinary delivery's outcome %q, want %q", got, redeliver.Redelivered)
			}
			if got, want := h.requests(), []int64{ordinary.ID}; !slices.Equal(got, want) {
				t.Fatalf("redelivery requests %v, want %v", got, want)
			}
			if got := outcome(report, tc.guid); got != "" {
				t.Fatalf("the delivery the bucket cannot key was decided %q; want it left out", got)
			}
			if got := h.cursor(); !got.Equal(h.clock.Now()) {
				t.Fatalf("the cursor is at %s, want the complete sweep's start %s", got, h.clock.Now())
			}
			line := h.oneLine("webhook redelivery left out a delivery whose GUID the bucket cannot key")
			if !strings.Contains(line, "level=WARN") {
				t.Fatalf("the left-out line is not at WARN: %.120s", line)
			}
			if refusal := lineAttr(line, "error"); refusal == "" {
				t.Fatalf("the left-out line names no refusal: %.120s", line)
			}
			requireCut(t, sighting{where: "the guid of the left-out line", shown: lineAttr(line, "guid"), text: tc.guid, alone: true})
			h.requireNoneWhole(tc.guid)
		})
	}
}

// largeRefusal is a body GitHub could answer a refused request with: JSON that names its message
// first, filled with refusalFiller to the 1 MiB a response may hold. A line that quotes the body
// escapes the quotes of its JSON and leaves the filler as it is, so a search of the log finds the
// body whole where it finds the filler whole.
var (
	refusalFiller = strings.Repeat("x", 1<<20-len(`{"message":"Server Error","filler":""}`))
	largeRefusal  = `{"message":"Server Error","filler":"` + refusalFiller + `"}`
)

// longNextLink is the path and query of a next page link of the listing far past the longest link
// the client follows (githubapp's nextLinkMaxBytes, 2048 bytes): its cursor is 64 KiB.
var longNextLink = "/app/hook/deliveries?per_page=100&cursor=" + strings.Repeat("v", 1<<16)

// inFrontOfGitHub points h's client at a server of the test's own in front of a webhook fake that
// h holds from then on, and answers the server's origin: front answers every request the client
// sends, itself or by handing it to the fake, so a test overrides only the answers it is about.
// The fake newHarness made is left idle, since a request handed on must carry an App JWT the fake
// verifies, and the key newHarness signs with is newHarness's own.
func (h *harness) inFrontOfGitHub(front func(w http.ResponseWriter, r *http.Request, fake http.Handler)) string {
	h.t.Helper()
	key, pemText := githubapptest.Key(h.t)
	h.webhook = githubapptest.NewWebhook(h.t, &key.PublicKey, clientID, nil)
	h.webhook.SetClock(h.clock.Now)
	origin, err := url.Parse(h.webhook.URL())
	if err != nil {
		h.t.Fatalf("parse the fake's origin: %v", err)
	}
	fake := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(origin) }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { front(w, r, fake) }))
	h.t.Cleanup(server.Close)
	h.client, err = githubapp.New(&auth.AppConfig{ClientID: clientID, PEM: pemText}, server.URL, githubapp.WithLimits(h.limits))
	if err != nil {
		h.t.Fatalf("App client: %v", err)
	}
	return server.URL
}

// isListing is whether r is a request for a page of the deliveries listing.
func isListing(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/app/hook/deliveries"
}

// refuseLargely answers w as GitHub refuses a request, with status and largeRefusal.
func refuseLargely(t *testing.T, w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, largeRefusal); err != nil {
		t.Errorf("write GitHub's refusal: %v", err)
	}
}

// undecodableDeliveredAt is a delivered_at of 100,000 bytes that is no time, which a listing
// GitHub answers 200 can carry: the client's decode of the page fails with a time.ParseError whose
// Value and ValueElem are each the whole delivered_at, and the client cuts each alone
// (githubapp.ShownErr).
var undecodableDeliveredAt = strings.Repeat("v", 100_000)

// listingOf is a page of the listing, with the fields GitHub writes (githubapptest's Delivery),
// whose one delivery has deliveredAt as its delivered_at.
func listingOf(deliveredAt string) string {
	return `[{"id":17,"guid":"guid-undecodable","delivered_at":"` + deliveredAt +
		`","redelivery":false,"duration":0.3,"status":"Invalid HTTP Response: 503","status_code":503,` +
		`"event":"pull_request_review","action":"submitted","installation_id":1,"repository_id":7}]`
}

// extraText is 100,000 bytes after a valid time in extraTextDeliveredAt, a delivered_at that a
// listing GitHub answers 200 can carry. time.Parse reads the time and fails on extraText: the
// client's decode of the page fails with a time.ParseError whose Value is the whole delivered_at
// and whose reason, its Message, quotes extraText (`: extra text: "<extraText>"`). The client cuts
// each alone (githubapp.ShownErr).
var (
	extraText            = strings.Repeat("x", 100_000)
	extraTextDeliveredAt = "2026-11-20T00:00:00Z" + extraText
)

// answerListing puts a server in front of the fake that answers each request for a page of the
// listing 200 with listing, and hands every other request to the fake.
func (h *harness) answerListing(t *testing.T, listing string) {
	t.Helper()
	h.inFrontOfGitHub(func(w http.ResponseWriter, r *http.Request, fake http.Handler) {
		if isListing(r) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, listing); err != nil {
				t.Errorf("write GitHub's listing: %v", err)
			}
			return
		}
		fake.ServeHTTP(w, r)
	})
}

// malformedHeaderLine is a line of 64 KiB with no colon in it, which no header block may hold:
// net/http's client fails a request whose answer holds it with a transport error that quotes the
// line whole (`malformed MIME header: missing colon: "<line>"`).
var malformedHeaderLine = strings.Repeat("h", 1<<16)

// answerMalformed answers w's request with a raw HTTP/1.1 answer whose header block holds
// malformedHeaderLine, written on the connection it takes from the server (http.Hijacker), since
// the server writes no such header itself.
func answerMalformed(t *testing.T, w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Errorf("the server's writer %T cannot hand over its connection", w)
		return
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		t.Errorf("take the connection from the server: %v", err)
		return
	}
	defer conn.Close()
	if _, err := buffered.WriteString("HTTP/1.1 200 OK\r\n" + malformedHeaderLine + "\r\n\r\n"); err != nil {
		t.Errorf("write the malformed answer: %v", err)
		return
	}
	if err := buffered.Flush(); err != nil {
		t.Errorf("write the malformed answer: %v", err)
	}
}

// failedListing sweeps once and then runs one sweep of the running loop, each of which fails at
// the listing (what names why), and answers the places that show text: the error the sweep
// answered, and the WARN line the running sweep logs its error with (sweepAndReport).
func (h *harness) failedListing(t *testing.T, what, text string) []sighting {
	t.Helper()
	_, err := h.sweeper().Sweep(context.Background(), redeliver.Options{})
	if err == nil {
		t.Fatalf("the sweep whose %s answered no error", what)
	}
	runningSweep(h.sweeper())
	line := h.oneLine("webhook redelivery sweep failed")
	return []sighting{
		{where: "the error the sweep answered", shown: err.Error(), text: text},
		{where: "the error of the sweep-failed line", shown: lineAttr(line, "error"), text: text},
	}
}

// refusedRedelivery puts a server in front of the fake that answers each redelivery request by
// answer, records a failed attempt of the delivery guid, and sweeps once, which must leave the
// delivery RequestRefused. It answers the place that shows text: the error of the line the sweep
// logs the refused request with.
func (h *harness) refusedRedelivery(t *testing.T, guid, text string, answer func(w http.ResponseWriter)) []sighting {
	t.Helper()
	h.inFrontOfGitHub(func(w http.ResponseWriter, r *http.Request, fake http.Handler) {
		if r.Method == http.MethodPost {
			answer(w)
			return
		}
		fake.ServeHTTP(w, r)
	})
	h.fail(guid, http.StatusServiceUnavailable, time.Minute)
	if got := outcome(h.sweep(redeliver.Options{}), guid); got != redeliver.RequestRefused {
		t.Fatalf("the sweep's outcome %q, want %q", got, redeliver.RequestRefused)
	}
	line := h.oneLine("webhook redelivery request refused")
	return []sighting{{where: "the error of the request-refused line", shown: lineAttr(line, "error"), text: text}}
}

// GitHub's answers to the sweep's own requests are GitHub's text too, and each reaches the sweep
// in a GitHub call's error: the body of an error status GitHub answers the deliveries listing or a
// redelivery request with, which may be as long as a response may be (1 MiB); the listing's next
// page link, which the githubapp client refuses past the longest it follows; a value of a listing
// GitHub answers 200 that the client cannot decode, which the decode's error quotes; and a header
// line of an answer net/http cannot read, which the transport error's cause quotes, whether the
// answer is to the listing or to a redelivery request. The client cuts each once, where it enters
// the error, as githubapp.ShownBody shows GitHub's text. A body, a link and a value it cannot
// decode are each cut alone, so each shows its own size (requireCut). The decode error is a
// time.ParseError that still names the layout, and it writes the cut value in the quotes of
// package time (inTimeQuotes). For a valid time with text after it, the decode error's reason
// quotes that text, and the client cuts the reason with the reason's size (requireBounded). A
// transport error cuts its URL, with the URL's size, and its cause. A line the cause quotes is cut
// with the cause's own text, which holds GitHub's among words of its own, so it shows the size of
// the cause's text (requireBounded). The sweep shows each such error as it is, so each is held to
// the bound and never whole: in the error the sweep answers and in the WARN line the running sweep
// logs it with for each failed listing, and in the request-refused line for each failed
// redelivery request (refusedRedelivery). Each case is a real sweep through the real client
// against the fake, with a server in front of the fake that gives the one answer the case is about
// (inFrontOfGitHub).
func TestTheSweepShowsGitHubsErrorBodyAndNextLinkToABound(t *testing.T) {
	for _, tc := range []struct {
		name string
		// require is the check of every place the case shows GitHub's text: requireCut where the
		// client cuts GitHub's text alone, and requireBounded where it cuts the text of the
		// transport error's cause that quotes it.
		require func(t *testing.T, s sighting)
		// run makes the case on h, and answers every place it shows GitHub's text and the text a
		// search of the log must not find whole.
		run func(t *testing.T, h *harness) (shown []sighting, whole string)
	}{
		{"a listing GitHub refuses with a body past the bound", requireCut, func(t *testing.T, h *harness) ([]sighting, string) {
			h.inFrontOfGitHub(func(w http.ResponseWriter, r *http.Request, fake http.Handler) {
				if isListing(r) {
					refuseLargely(t, w, http.StatusInternalServerError)
					return
				}
				fake.ServeHTTP(w, r)
			})
			return h.failedListing(t, "listing GitHub refused", largeRefusal), refusalFiller
		}},
		{"a next page link past the bound", requireCut, func(t *testing.T, h *harness) ([]sighting, string) {
			origin := h.inFrontOfGitHub(func(w http.ResponseWriter, r *http.Request, fake http.Handler) {
				if isListing(r) {
					w.Header().Set("Link", "<http://"+r.Host+longNextLink+`>; rel="next"`)
				}
				fake.ServeHTTP(w, r)
			})
			h.fail("guid-paged", http.StatusServiceUnavailable, time.Minute)
			link := origin + longNextLink
			return h.failedListing(t, "next page link is past the bound", link), link
		}},
		{"a redelivery request GitHub refuses with a body past the bound", requireCut, func(t *testing.T, h *harness) ([]sighting, string) {
			return h.refusedRedelivery(t, "guid-refused", largeRefusal, func(w http.ResponseWriter) {
				refuseLargely(t, w, http.StatusUnprocessableEntity)
			}), refusalFiller
		}},
		{"a listing GitHub answers 200 with a value the client cannot decode", requireCut, func(t *testing.T, h *harness) ([]sighting, string) {
			h.answerListing(t, listingOf(undecodableDeliveredAt))
			shown := h.failedListing(t, "listing holds a delivered_at the client cannot decode", undecodableDeliveredAt)
			for i := range shown {
				shown[i].quote = inTimeQuotes
			}
			return shown, undecodableDeliveredAt
		}},
		// The delivered_at reaches the error twice. The parse error's Value is the whole
		// delivered_at, which the client cuts alone, with the value's size, in time's quotes, as in
		// the row above: so the row's check is requireCut. The parse error's reason quotes
		// extraText among time's words (`: extra text: "<extraText>"`), and the client cuts the
		// reason with the reason's size, not with extraText's. requireCut does not hold of that cut.
		// requireBounded does, since the cut reason ends the error, so each place is held to it
		// too, with extraText as its text, and must still name the reason.
		{"a listing GitHub answers 200 with a valid time and extra text past the bound", requireCut, func(t *testing.T, h *harness) ([]sighting, string) {
			h.answerListing(t, listingOf(extraTextDeliveredAt))
			shown := h.failedListing(t, "listing holds a delivered_at with extra text after a valid time", extraTextDeliveredAt)
			for i := range shown {
				shown[i].quote = inTimeQuotes
				requireBounded(t, sighting{where: shown[i].where + ", in its reason", shown: shown[i].shown, text: extraText})
				if reason := `": extra text: "` + extraText[:256]; !strings.Contains(shown[i].shown, reason) {
					t.Errorf("%s is %.200q…; want it to name the reason and the start of the text after the time, %.40q…", shown[i].where, shown[i].shown, reason)
				}
			}
			return shown, extraText
		}},
		{"a listing whose answer fails at the transport on a header line past the bound", requireBounded, func(t *testing.T, h *harness) ([]sighting, string) {
			h.inFrontOfGitHub(func(w http.ResponseWriter, r *http.Request, fake http.Handler) {
				if isListing(r) {
					answerMalformed(t, w)
					return
				}
				fake.ServeHTTP(w, r)
			})
			return h.failedListing(t, "listing's answer holds a header line with no colon", malformedHeaderLine), malformedHeaderLine
		}},
		{"a redelivery request whose answer fails at the transport on a header line past the bound", requireBounded, func(t *testing.T, h *harness) ([]sighting, string) {
			return h.refusedRedelivery(t, "guid-malformed", malformedHeaderLine, func(w http.ResponseWriter) {
				answerMalformed(t, w)
			}), malformedHeaderLine
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			shown, whole := tc.run(t, h)
			for _, s := range shown {
				tc.require(t, s)
			}
			h.requireNoneWhole(whole)
		})
	}
}
