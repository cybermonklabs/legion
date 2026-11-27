package supervise

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// resumeText is the resume task the fake Deps.RootTask writes from the answered wait at each send,
// as the daemon's rootTask writes it from the record (resumeTaskNow).
const resumeText = "A person answered the wait on your spec: version 5 is approved. Carry on from it."

// A resume task of a root a person answered (Delivery.Resume) is queued with no text of its own,
// and an entry start of it can carry news of the approved design (record.DeliveryNews), whose start
// text is that empty text. Its text is written from the record at every send (Deps.RootTask), and
// the news it carries follows that text, rendered in the same answer. So the send asks no render of
// the news alone (Deps.DesignNews): over the empty start text it would send the news without the
// resume, and where the news no longer stands it would read the task as one with nothing to say
// and retire the resume unsent.
func TestAResumeTaskIsSentWithItsTextAndTheNewsItCarriesAfterIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// stands is whether the news still stands at the send.
		stands bool
	}{
		{name: "its news still stands", stands: true},
		{name: "its news no longer stands"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBareHarness(t)
			h.token = rootToken
			h.deps.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return true, nil }
			var renders int
			h.deps.DesignNews = func(_ context.Context, _ Claim, d Delivery) (string, []byte, error) {
				renders++
				// The one render over the start text the delivery carries: the news alone, or
				// nothing at all where it no longer stands, as the daemon's designNews answers a
				// task whose start text is empty.
				if !tc.stands {
					return "", nil, nil
				}
				return renderFakeNews(t, d.News), d.News, nil
			}
			h.deps.RootTask = func(_ context.Context, _ Claim, d Delivery) (string, []byte, []byte, error) {
				if !tc.stands {
					return resumeText, d.Kept, nil, nil
				}
				var carried fakeNews
				if err := json.Unmarshal(d.News, &carried); err != nil {
					return "", nil, nil, err
				}
				carried.Task = resumeText
				news, err := json.Marshal(carried)
				if err != nil {
					return "", nil, nil, err
				}
				return renderFakeNews(t, news), d.Kept, news, nil
			}
			if err := h.store.PutClaim(h.ctx, rootClaim()); err != nil {
				t.Fatal(err)
			}
			h.start(rootClaim())
			h.reach(StateReady)

			news, err := json.Marshal(fakeNews{Items: []fakeNewsItem{{Issue: "LEGION-208", Step: "spec", From: 4, To: 5}}})
			if err != nil {
				t.Fatal(err)
			}
			resume := startTask(rootToken, 30, 2, 1)
			resume.Task, resume.News, resume.Resume = renderFakeNews(t, news), news, []byte(`{"gateVersion":5}`)
			h.must(resume)

			sent := h.wantPrompts(1)[0]
			if renders != 0 {
				t.Fatalf("the news alone was rendered %d times at the resume's send, want none: the resume's own answer renders it", renders)
			}
			// A resume retired at its send leaves the claim holding nothing, which pending fails on.
			p := h.pending()
			if !strings.Contains(sent.Message, resumeText) {
				t.Fatalf("the prompt %q does not carry the resume written at the send, %q", sent.Message, resumeText)
			}
			if !tc.stands {
				if len(p.News) != 0 || strings.Contains(sent.Message, "The approved spec") {
					t.Fatalf("the resume whose news no longer stands was sent as %q carrying %s, want the resume alone", sent.Message, p.News)
				}
				return
			}
			wantNamesExactly(t, "at the resume's send", sent.Message, p.News)
			var carried fakeNews
			if err := json.Unmarshal(p.News, &carried); err != nil || carried.Task != resumeText {
				t.Fatalf("the news the resume carries after its send = %s (%v), want it rendered after the resume's text", p.News, err)
			}
		})
	}
}
