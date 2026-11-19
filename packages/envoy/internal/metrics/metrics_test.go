package metrics

import (
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// scrape returns the registry's exposition text.
func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	r.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	return recorder.Body.String()
}

// An optional gauge whose function reports no value still names itself (HELP and TYPE), so the
// series is known to the scraper, but writes no sample line: the reader sees the value as missing,
// never as a number the function could not stand behind. A value it does report, zero included,
// is written as a sample.
func TestAnOptionalGaugeWritesNoSampleWhenItsFunctionReportsNone(t *testing.T) {
	var value atomic.Int64
	var known atomic.Bool
	r := New()
	r.NewOptionalGaugeFunc("envoy_test_listed", "Rows the test lists", func() (int64, bool) {
		return value.Load(), known.Load()
	})

	body := scrape(t, r)
	for _, line := range []string{"# HELP envoy_test_listed Rows the test lists\n", "# TYPE envoy_test_listed gauge\n"} {
		if !strings.Contains(body, line) {
			t.Fatalf("scrape with no value lacks %q:\n%s", line, body)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "envoy_test_listed") {
			t.Fatalf("scrape with no value has the sample %q:\n%s", line, body)
		}
	}

	known.Store(true)
	if body := scrape(t, r); !strings.Contains(body, "\nenvoy_test_listed 0\n") {
		t.Fatalf("scrape of a known zero lacks the sample 0:\n%s", body)
	}
	value.Store(3)
	if body := scrape(t, r); !strings.Contains(body, "\nenvoy_test_listed 3\n") {
		t.Fatalf("scrape of a known 3 lacks the sample 3:\n%s", body)
	}
}

// A gauge set writes, at each scrape, one sample for each series its function answers then, in
// the order of their label text whatever order the function answered them in, and nothing for a
// series it has stopped answering: a series leaves the scrape with no call to delete it. An empty
// set still names itself, so a scraper knows the metric and reads no series of it.
func TestAGaugeSetWritesTheSeriesItsFunctionAnswersAtEachScrape(t *testing.T) {
	var mu sync.Mutex
	var current []GaugeSample
	r := New()
	r.NewGaugeSetFunc("envoy_test_refused", "Secrets the test refuses", func() []GaugeSample {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(current)
	})
	const header = "# HELP envoy_test_refused Secrets the test refuses\n# TYPE envoy_test_refused gauge\n"
	a := GaugeSample{Labels: [][2]string{{"secret", "a"}, {"reason", "no-current-value"}}, Value: 1}
	b := GaugeSample{Labels: [][2]string{{"secret", "b"}, {"reason", "owner-tag-missing"}}, Value: 1}
	for _, step := range []struct {
		name    string
		samples []GaugeSample
		want    string
	}{
		{"an empty set", nil, header},
		{"two series, answered out of order", []GaugeSample{b, a}, header +
			`envoy_test_refused{secret="a",reason="no-current-value"} 1` + "\n" +
			`envoy_test_refused{secret="b",reason="owner-tag-missing"} 1` + "\n"},
		{"one of them gone", []GaugeSample{b}, header +
			`envoy_test_refused{secret="b",reason="owner-tag-missing"} 1` + "\n"},
		{"the set empty again", nil, header},
	} {
		mu.Lock()
		current = step.samples
		mu.Unlock()
		if body := scrape(t, r); body != step.want {
			t.Fatalf("%s: the scrape reads %q; want %q", step.name, body, step.want)
		}
	}
}

// A label value is written as the text format requires, whichever metric carries it: a
// backslash as \\, a double quote as \" and a line feed as \n. A value that is text the store
// holds - a secret's id, an owner tag - can hold any of them, and unescaped each would end the
// value or the sample's line early.
func TestALabelValueIsEscapedAsTheTextFormatRequires(t *testing.T) {
	for _, row := range []struct{ name, value, want string }{
		{"a backslash", `projects/1/secrets/a\b`, `projects/1/secrets/a\\b`},
		{"a double quote", `ada "the owner"`, `ada \"the owner\"`},
		{"a line feed", "first\nsecond", `first\nsecond`},
		{"all three together", "\\\"\n", `\\\"\n`},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := New()
			r.NewCounter("envoy_test_total", "A counter").Inc([2]string{"secret", row.value})
			r.NewGaugeSetFunc("envoy_test_refused", "A gauge set", func() []GaugeSample {
				return []GaugeSample{{Labels: [][2]string{{"secret", row.value}}, Value: 1}}
			})
			body := scrape(t, r)
			for _, want := range []string{
				`envoy_test_total{secret="` + row.want + `"} 1`,
				`envoy_test_refused{secret="` + row.want + `"} 1`,
			} {
				if !strings.Contains(body, "\n"+want+"\n") {
					t.Errorf("the scrape has no line %q:\n%s", want, body)
				}
			}
		})
	}
}

// A label value holding none of the three characters the text format escapes is written as it
// is, so the listener's and Dispatch's scrapes, whose label values are fixed words of their own,
// read byte for byte as the text pinned here. One registry with each kind of metric those two
// register, pinned whole.
func TestAPlainLabelValueIsWrittenAsItIs(t *testing.T) {
	r := New()
	received := r.NewCounter("envoy_messages_received_total", "Total messages received")
	received.Inc([2]string{"source", "agent"})
	received.Add(2, [2]string{"source", "github"}, [2]string{"topic_prefix", "notifications.github"})
	r.NewHistogram("envoy_delivery_duration_seconds", "Duration of delivery", []float64{0.5, 1}).
		Observe(0.25, [2]string{"delivery_status", "delivered"})
	r.NewGauge("envoy_active_sessions", "Number of active sessions").Set(4)
	r.NewGaugeFunc("envoy_consumer_pending", "Number of pending messages", func() int64 { return 5 })
	const want = "# HELP envoy_messages_received_total Total messages received\n" +
		"# TYPE envoy_messages_received_total counter\n" +
		`envoy_messages_received_total{source="agent"} 1` + "\n" +
		`envoy_messages_received_total{source="github",topic_prefix="notifications.github"} 2` + "\n" +
		"# HELP envoy_delivery_duration_seconds Duration of delivery\n" +
		"# TYPE envoy_delivery_duration_seconds histogram\n" +
		`envoy_delivery_duration_seconds_bucket{delivery_status="delivered",le="0.5"} 1` + "\n" +
		`envoy_delivery_duration_seconds_bucket{delivery_status="delivered",le="1"} 1` + "\n" +
		`envoy_delivery_duration_seconds_bucket{delivery_status="delivered",le="+Inf"} 1` + "\n" +
		`envoy_delivery_duration_seconds_sum{delivery_status="delivered"} 0.25` + "\n" +
		`envoy_delivery_duration_seconds_count{delivery_status="delivered"} 1` + "\n" +
		"# HELP envoy_active_sessions Number of active sessions\n" +
		"# TYPE envoy_active_sessions gauge\n" +
		"envoy_active_sessions 4\n" +
		"# HELP envoy_consumer_pending Number of pending messages\n" +
		"# TYPE envoy_consumer_pending gauge\n" +
		"envoy_consumer_pending 5\n"
	if body := scrape(t, r); body != want {
		t.Fatalf("the scrape reads\n%s\nwant\n%s", body, want)
	}
}
