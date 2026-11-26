package supervise

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Since #3711 a root architect's entry start can carry the decisions a person made while nobody
// in its tree could hear them and news of the approved design together. At its send, a decision
// whose issue has left the tree is dropped from the task (Deps.RootTask). The task's text and the
// news it carries are written by the one render (Deps.DesignNews, and Deps.RootTask's own
// narrowing through it), so a narrowed task carries exactly the news its text names: its prompt
// holds that news, its confirmation credits exactly what that prompt named, and a later send
// renders from the narrowed text, never naming the released decision again.

const (
	// heldDecisions is what the held task names of the tree's kept decisions: two, one of which
	// (LEGION-209) leaves the tree while the task waits.
	heldDecisions = `{"decisions":["LEGION-208","LEGION-209"]}`
	// keptDecisions is what the task names once LEGION-209 has left.
	keptDecisions = `{"decisions":["LEGION-208"]}`
	// wholeText is the start's own text, before any narrowing: both decisions.
	wholeText = "A person decided on LEGION-208 and on LEGION-209 while nobody could hear them. Read each."
	// narrowedText is the start's own text with LEGION-209 dropped.
	narrowedText = "A person decided on LEGION-208 while nobody could hear it. Read it."
)

// fakeNews is the shape the fake render reads: record.DeliveryNews's start text and items, opaque
// to supervise as the real one is.
type fakeNews struct {
	Task  string         `json:"task"`
	Items []fakeNewsItem `json:"items,omitempty"`
}

// fakeNewsItem is one item fakeNews tells.
type fakeNewsItem struct {
	Issue string `json:"issue"`
	Step  string `json:"step"`
	From  int    `json:"from"`
	To    int    `json:"to"`
}

// renderFakeNews is the fake one render: the start's own text, then one sentence for each item,
// so the text names exactly the items the news carries.
func renderFakeNews(t *testing.T, news []byte) string {
	t.Helper()
	var decoded fakeNews
	if err := json.Unmarshal(news, &decoded); err != nil {
		t.Fatalf("decode the news %s: %v", news, err)
	}
	text := decoded.Task
	for _, item := range decoded.Items {
		text += fmt.Sprintf("\n\nThe approved %s of %s changed: version %d, since version %d.", item.Step, item.Issue, item.To, item.From)
	}
	return text
}

// heldEntryNews is the news the entry start carries: version 5 of the root's spec since version 4,
// told to the architect, after the start's whole text.
func heldEntryNews(t *testing.T) []byte {
	t.Helper()
	news, err := json.Marshal(fakeNews{Task: wholeText, Items: []fakeNewsItem{{Issue: "LEGION-208", Step: "spec", From: 4, To: 5}}})
	if err != nil {
		t.Fatal(err)
	}
	return news
}

// narrowingRootTask is a Deps.RootTask that drops LEGION-209 from a task that names it, narrowing
// the start's own text and rendering the news after it through the fake render, as the daemon's
// rootTask does through its one render; a task that no longer names LEGION-209 is left as it is.
func narrowingRootTask(t *testing.T) func(context.Context, Claim, Delivery) (string, []byte, []byte, error) {
	return func(_ context.Context, _ Claim, d Delivery) (string, []byte, []byte, error) {
		if !strings.Contains(string(d.Kept), "LEGION-209") {
			return d.Task, d.Kept, d.News, nil
		}
		if len(d.News) == 0 {
			return narrowedText, []byte(keptDecisions), nil, nil
		}
		var decoded fakeNews
		if err := json.Unmarshal(d.News, &decoded); err != nil {
			return "", nil, nil, err
		}
		decoded.Task = narrowedText
		news, err := json.Marshal(decoded)
		if err != nil {
			return "", nil, nil, err
		}
		return renderFakeNews(t, news), []byte(keptDecisions), news, nil
	}
}

// wantNamesExactly fails the test unless the delivery carries news and message ends with the text
// that news renders, after only the sentences the machine puts in front of a task
// (Delivery.message, a provider error's note among them): every item the news carries is named by
// it, and nothing else is. Every caller here expects news, so a delivery that carries none fails:
// a narrowing that dropped the news would leave a text that names none, and pass otherwise.
func wantNamesExactly(t *testing.T, when, message string, news []byte) {
	t.Helper()
	if len(news) == 0 {
		t.Fatalf("%s: the delivery carries no news, want the news the held task carried (the prompt is %q)", when, message)
	}
	want := renderFakeNews(t, news)
	before, found := strings.CutSuffix(message, want)
	if !found || strings.Contains(before, "The approved spec") {
		t.Fatalf("%s: the prompt is %q, but the news the delivery carries, %s, renders %q", when, message, news, want)
	}
}

// A root architect's entry task held with kept decisions and news, one of whose decisions leaves
// the tree before its send, is sent narrowed with its news: the prompt names that news and not
// the released decision, the confirmation credits exactly the news the prompt named, and the send
// after a provider error takes the task back renders from the narrowed text again.
func TestANarrowedRootTaskIsSentWithTheNewsItCarriesAndCreditsOnlyThat(t *testing.T) {
	h := newBareHarness(t)
	h.token = rootToken
	h.deps.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return true, nil }
	h.deps.RootTask = narrowingRootTask(t)
	h.deps.DesignNews = func(_ context.Context, _ Claim, d Delivery) (string, []byte, error) {
		return renderFakeNews(t, d.News), d.News, nil
	}
	if err := h.store.PutClaim(h.ctx, rootClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(rootClaim())
	h.reach(StateReady)

	entry := startTask(rootToken, 30, 2, 1)
	entry.News = heldEntryNews(t)
	entry.Task, entry.Kept = renderFakeNews(t, entry.News), []byte(heldDecisions)
	h.must(entry)

	sent := h.wantPrompts(1)[0]
	if strings.Contains(sent.Message, "LEGION-209") {
		t.Fatalf("the prompt %q names LEGION-209, which has left the tree", sent.Message)
	}
	p := h.pending()
	wantNamesExactly(t, "at the narrowed task's send", sent.Message, p.News)
	if sent.DeliveryID != p.ID {
		t.Fatalf("the prompt went under %s while the delivery is %s", sent.DeliveryID, p.ID)
	}

	h.must(StreamTurnStart{Claim: rootToken})
	confirmed, ok := h.store.delivery(rootToken)
	if !ok || confirmed.ConfirmedAt.IsZero() {
		t.Fatalf("the delivery after its turn started = %+v (held %t), want it confirmed", confirmed, ok)
	}
	wantNamesExactly(t, "at the confirmation, the copy it credits", sent.Message, confirmed.Carried)

	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
	h.advance(30 * time.Second)
	prompts := h.wantPrompts(2)
	resent := prompts[1]
	if strings.Contains(resent.Message, "LEGION-209") {
		t.Fatalf("the prompt sent again after the provider error, %q, names LEGION-209 again", resent.Message)
	}
	wantNamesExactly(t, "at the send after the provider error", resent.Message, h.pending().News)
}
