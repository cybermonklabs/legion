// packages/envoy/internal/broker/requests/narrowing_skip_reason_test.go
// The reason a skipped narrowing check logs is the one thing an operator reads to tell a broken
// App, a slow GitHub and an outage apart (docs/site/src/content/docs/broker/operate.md, the
// "narrowing check was skipped" row), and each of those leaves the request pending all the same,
// so the request's state cannot tell them apart: only the reason= of the line does.
package requests_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestASkippedNarrowingCheckNamesWhyItWasSkipped: a 401 to the App's own installations call is
// GitHub refusing the App, not an answer that decided nothing; a verdict mint the check's own
// bound cut while GitHub was still answering is the check's time running out, not GitHub's
// answer; and a 500 at once is GitHub's answer deciding nothing. Each row's request is pending,
// and the line's reason= is the one operate.md names for that cause.
func TestASkippedNarrowingCheckNamesWhyItWasSkipped(t *testing.T) {
	narrowing := &secrets.GitHubNarrowing{Owner: standInOwner, Repositories: []string{standInRepository}}
	for _, tc := range []struct {
		name    string
		trouble func(f *narrowingFixture)
		reason  string
	}{
		{
			name: "GitHub answers 401 to the App's installations call",
			trouble: func(f *narrowingFixture) {
				answersInstallationsWith(f.stand, http.StatusUnauthorized, `{"message":"A JSON web token could not be decoded"}`)
			},
			reason: `reason="GitHub refused the App rather than the narrowing"`,
		},
		{
			name: "the check's own bound cuts the verdict mint while GitHub is answering",
			trouble: func(f *narrowingFixture) {
				f.NarrowingCheckMint = 50 * time.Millisecond
				f.stand.answersTokensWith(func(w http.ResponseWriter, _ int) bool {
					time.Sleep(2 * time.Second)
					w.WriteHeader(http.StatusInternalServerError)
					return true
				})
			},
			reason: `reason="the check's time ran out, or its caller went away, before GitHub answered"`,
		},
		{
			name: "GitHub answers 500 at once",
			trouble: func(f *narrowingFixture) {
				f.stand.answersTokensWith(answerStatus(http.StatusInternalServerError, nil, `{"message":"Server Error"}`))
			},
			reason: `reason="GitHub's answer decided nothing about the narrowing"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := policytest.CaptureLog(t)
			f := newNarrowingFixture(t, map[string]int64{standInOwner: 7})
			tc.trouble(f)
			request, err := f.create(t, narrowing, "APP_KEY")
			if err != nil || request.State != "pending" {
				t.Fatalf("Create(APP_KEY) narrowed while %s = %+v, %v; want it pending", tc.name, request, err)
			}
			var skipped []string
			for line := range strings.Lines(logged.String()) {
				if strings.Contains(line, requests.NarrowingCheckSkippedMessage) {
					skipped = append(skipped, line)
				}
			}
			if len(skipped) != 1 || !strings.Contains(skipped[0], tc.reason) {
				t.Fatalf("the skipped-check lines are %q; want one line holding %s", skipped, tc.reason)
			}
		})
	}
}
