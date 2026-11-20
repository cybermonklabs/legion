package redeliver_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
)

// The sweep writes GitHub's text into its lines and its errors: a delivery's GUID, its event and
// its action, and the keys of its bucket, every one of which holds a GUID. GitHub bounds none of
// them, so each is shown as githubapp.ShownBody shows GitHub's text: whole up to 512 runes, and
// past that its first 512 runes and `… (<n> bytes)`. The tests here give each text boundRunes
// runes, past that bound, and keep the GUID within nats' key alphabet so that it reaches every
// site, and hold every line and error that names one to the cut text and never to the whole.

// boundRunes is how long each of the tests' texts is, in runes and in bytes alike.
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
// is a text that holds it among words of its own, an error.
type sighting struct {
	where, shown, text string
	alone              bool
}

// requireCut fails the test unless s shows its text as githubapp.ShownBody shows it, its first
// 512 runes and `… (<n> bytes)`, and never the text whole.
func requireCut(t *testing.T, s sighting) {
	t.Helper()
	cut := githubapp.ShownBody(s.text)
	if cut == s.text {
		t.Fatalf("%s: the text is %d bytes, which githubapp.ShownBody shows whole; the test needs one it cuts", s.where, len(s.text))
	}
	ending := cut[strings.LastIndex(cut, "…"):]
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
	if _, err := h.state.Create("guid."+stored.guid, record); err != nil {
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
			_, refused := kv.Update("guid."+texts.guid, []byte(`{}`), 1)
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
			if _, err := h.state.Put("cursor", fmt.Appendf(nil, `{"at":%q}`, now.Format(time.RFC3339))); err != nil {
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
			if _, err := h.state.Create("guid."+texts.guid, claimed); err != nil {
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
			if _, err := h.state.Put("cursor", fmt.Appendf(nil, `{"at":%q}`, now.Format(time.RFC3339))); err != nil {
				t.Fatalf("write the cursor an older build left: %v", err)
			}
			failed := h.failText(texts, http.StatusServiceUnavailable, 2*time.Hour)
			if _, err := h.state.Create("guid."+texts.guid, recordWithoutPending(t, now, failed)); err != nil {
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
			if _, err := h.state.Put(stray, nil); err != nil {
				t.Fatalf("write the stray index entry: %v", err)
			}
			sweeper, kv := h.sweeperOver(refusingCall{KeyValue: h.state.Legacy(), call: "Delete", prefix: redeliver.PendingPrefix})
			if _, err := sweeper.Sweep(context.Background(), redeliver.Options{}); err != nil {
				t.Fatalf("the sweep whose delete of a stray entry was refused: %.200v", err)
			}
			refused := kv.Delete(stray)
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
			if _, err := h.state.Create("guid."+texts.guid, notJSON); err != nil {
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
			if _, err := h.state.Create("guid."+texts.guid, notJSON); err != nil {
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
			_, refused := kv.Get("guid." + texts.guid)
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
			if _, err := h.state.Put(redeliver.PendingKey(h.clock.Now(), guid), nil); !errors.Is(err, bus.ErrTooLarge) {
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
