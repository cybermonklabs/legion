package redact_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// A refusal whose words run longer than the rule reads (redact.FieldLimit) prints them as
// redact.WordsNotReadable, and its status and its code still print; words of exactly FieldLimit
// bytes print as the rule reads them, the URL's user information removed. The URL's user and
// password share no run with the request's bearer, which Tokens would cut first and so shorten
// the words; the URL is written in parts so no line of this file reads as a credential to a
// scanner. Captures redact.field-limit/<case>.
func TestARefusalLongerThanTheFieldLimitPrintsItsStatusAndCodeAlone(t *testing.T) {
	const user, word = "ada-lovelace", "difference-engine"
	const written = "clone https://" + user + ":" + word + "@github.example/o/r failed "
	for _, row := range []struct {
		name     string
		length   int
		readable bool
	}{
		{"at-limit", redact.FieldLimit, true},
		{"past-limit", redact.FieldLimit + 1, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			words := written + strings.Repeat("x", row.length-len(written))
			body := `{"code":"INVALID_GRANT","error":"` + words + `"}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/token", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+redacttest.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			answer, err := redact.ReadAnswer(resp, int64(len(body)))
			if err != nil {
				t.Fatal(err)
			}
			var refusal struct {
				Code  redact.RemoteText `json:"code"`
				Error redact.RemoteText `json:"error"`
			}
			if err := answer.Decode(&refusal); err != nil {
				t.Fatal(err)
			}
			printed := fmt.Sprintf("%s: %s: %s", redact.StatusLine(answer.Status()), refusal.Code, refusal.Error)
			redacttest.RequireNone(t, "redact.field-limit/"+row.name, printed)
			for _, secret := range []string{user, word} {
				if strings.Contains(printed, secret) {
					t.Errorf("REDACT-ASSERT redact.field-limit/%s: the refusal printed %q, which holds %q", row.name, printed[:min(len(printed), 200)], secret)
				}
			}
			if !strings.HasPrefix(printed, "400 Bad Request: INVALID_GRANT: ") {
				t.Errorf("the refusal printed %.200q, want its status and its code first", printed)
			}
			if row.readable && !strings.Contains(printed, "clone https://github.example/o/r failed x") {
				t.Errorf("words of %d bytes printed %.200q, want them as the rule reads them", row.length, printed)
			}
			if !row.readable && !strings.HasSuffix(printed, ": "+redact.WordsNotReadable) {
				t.Errorf("words of %d bytes printed %.200q, want %q in their place", row.length, printed, redact.WordsNotReadable)
			}
		})
	}
}

// longFieldReads is how many times a row below reads its field, and longFieldBound how long the
// least of those reads may take: the least is the read's own cost, and what a busy box adds to one
// read is not. longFieldWait is how long one read may run before the row fails.
const (
	longFieldReads = 3
	longFieldBound = 20 * time.Millisecond
	longFieldWait  = 5 * time.Second
)

// A field another party writes can be as long as its answer, and githubrest reads answers of up to
// 64 MiB. Remote reads no field longer than redact.FieldLimit, so its read of a 64 MiB field of the
// two shapes that cost the rule most per byte stops at the limit: one long numeric reference
// (&amp;amp;amp;amp;amp;&#000...01;), which took 34 s at a load average of 110 to 130 on 288 CPUs
// before the limit, and one long name (&ampampamp...;), which took 0.84 s. Each read runs in a
// goroutine of its own, so a read past longFieldWait fails its row there.
func TestAFieldLongerThanTheLimitIsRefusedInBoundedTime(t *testing.T) {
	const size = 64 << 20
	for _, row := range []struct{ shape, field string }{
		{"long numeric reference", "&amp;amp;amp;amp;amp;&#" + strings.Repeat("0", size) + "1;"},
		{"long name", "&" + strings.Repeat("amp", size/3) + ";"},
	} {
		least := time.Duration(1<<63 - 1)
		for range longFieldReads {
			began := time.Now()
			done := make(chan bool, 1)
			go func() {
				_, readable := redact.Remote(row.field)
				done <- readable
			}()
			select {
			case readable := <-done:
				least = min(least, time.Since(began))
				if readable {
					t.Errorf("Remote read the %d-byte %s field as readable", len(row.field), row.shape)
				}
			case <-time.After(longFieldWait):
				t.Fatalf("Remote of the %d-byte %s field took more than %s", len(row.field), row.shape, longFieldWait)
			}
		}
		t.Logf("the least of %d reads of the %d-byte %s field took %s", longFieldReads, len(row.field), row.shape, least)
		if least > longFieldBound {
			t.Errorf("the least of %d reads of the %d-byte %s field took %s, more than %s", longFieldReads, len(row.field), row.shape, least, longFieldBound)
		}
	}
}
