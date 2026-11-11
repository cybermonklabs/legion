// write_advice_declined_asks_test.go proves INFRA-1282's R6: a session that opens a question on
// an issue holding a declined ask is told so by the write itself - which question a person
// declined, and what they wrote - so the same question is not put to them again. Only the two
// writes that open a question carry it, and it stands apart from the related-item check beside
// it: no crowding of that check's three places drops it, a check that was not made does not take
// it away, and its own read failing costs the rest of the advice nothing.
package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"
)

// declinedAsksOf is the declined_asks part of a write's advice and whether the write carried the
// part at all, which a decode into a struct could not tell from an empty list.
func declinedAsksOf(t *testing.T, what, responseBody string) ([]adviceDeclinedAsk, bool) {
	t.Helper()
	raw, carried := rawAdviceFields(t, what, responseBody)["declined_asks"]
	if !carried {
		return nil, false
	}
	var items []adviceDeclinedAsk
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("decode %s declined_asks: %v body=%s", what, err, responseBody)
	}
	return items, true
}

// requireNamesTheDeclinedAsk fails t unless the write's advice names that one declined ask, with
// the question that was asked and the person's note.
func requireNamesTheDeclinedAsk(t *testing.T, what, responseBody string, want adviceDeclinedAsk) {
	t.Helper()
	items, carried := declinedAsksOf(t, what, responseBody)
	if !carried {
		t.Fatalf("%s carried no declined_asks: %s", what, responseBody)
	}
	if len(items) != 1 || items[0] != want {
		t.Fatalf("%s declined_asks = %#v, want the one declined ask %#v", what, items, want)
	}
}

// declineTheAsk closes askID as a person who will not answer it, with their note.
func declineTheAsk(t *testing.T, handler http.Handler, askID, note string) {
	t.Helper()
	declined := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/decline", map[string]any{
		"note": note,
	}, "alice")
	if declined.Code != http.StatusOK {
		t.Fatalf("decline ask %s: status=%d body=%s", askID, declined.Code, declined.Body.String())
	}
}

// addAnAskBlock is the edit a session makes when it puts a question into a document: one more ask
// block at the end of it.
func addAnAskBlock(t *testing.T, handler http.Handler, artifactID string, actor map[string]any, blockID, question string) string {
	t.Helper()
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops": []map[string]string{{
			"op": "insert", "markdown": ":::ask{#" + blockID + "}\n" + question + "\n:::", "after": "end",
		}},
		"actor": actor,
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit that adds an ask block: status=%d body=%s", edited.Code, edited.Body.String())
	}
	return edited.Body.String()
}

func TestTheNextAskOnTheIssueNamesTheDeclinedAsk(t *testing.T) {
	const question = "Which transport should the billing exporter use?"
	const note = "I have no view on this one; pick what the team can run."
	// theDeclinedAsk is the part the write's answer carries, once the person has declined.
	theDeclinedAsk := func(id string) adviceDeclinedAsk {
		return adviceDeclinedAsk{ID: id, Question: question, Note: note}
	}

	// The two writes that open a question name it, and a comment on the same issue by the same
	// session does not: the reminder reaches the agent that is about to ask and nobody else.
	t.Run("the ask route and a document edit, but not a comment", func(t *testing.T) {
		handler := newTestHandler(t)
		createAdviceProject(t, handler, "NEXT")
		spec := "# Billing exporter\n\nPick the transport.\n"
		issue := createAdviceIssue(t, handler, "NEXT", "Billing exporter transport", &spec)
		actor := adviceSessionActor("session-asking-again")
		declinedID := openAskAs(t, handler, issue.Key, "session-asking-again", question)
		declineTheAsk(t, handler, declinedID, note)
		declined := theDeclinedAsk(declinedID)

		asked := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": "Which store should the exporter write to?", "actor": actor,
		})
		if asked.Code != http.StatusCreated {
			t.Fatalf("the next ask: status=%d body=%s", asked.Code, asked.Body.String())
		}
		requireNamesTheDeclinedAsk(t, "the next ask", asked.Body.String(), declined)

		edit := addAnAskBlock(t, handler, issue.PrimaryArtifactID, actor, "window", "Which deployment window?")
		if advice := adviceFromResponse(t, http.StatusOK, edit); advice.DecisionBlocksAdded == nil || *advice.DecisionBlocksAdded != 1 {
			t.Fatalf("the edit added %#v ask blocks, want one: %s", advice.DecisionBlocksAdded, edit)
		}
		requireNamesTheDeclinedAsk(t, "the edit that added an ask block", edit, declined)

		commented := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": "Carrying on without an answer.", "actor": actor,
		})
		if commented.Code != http.StatusCreated {
			t.Fatalf("the comment: status=%d body=%s", commented.Code, commented.Body.String())
		}
		if advice := adviceFromResponse(t, commented.Code, commented.Body.String()); advice.IssueStatus != "triage" {
			t.Fatalf("the comment's advice = %#v, want the issue's state reported as ever", advice)
		}
		if items, carried := declinedAsksOf(t, "the comment", commented.Body.String()); carried {
			t.Fatalf("a comment carried declined_asks %#v: %s", items, commented.Body.String())
		}
	})

	// Every declined ask of the issue, not a page of them: a cap would drop exactly the question
	// the session is about to put again. They come oldest first, the order the read gives.
	t.Run("every declined ask of the issue", func(t *testing.T) {
		handler := newTestHandler(t)
		createAdviceProject(t, handler, "EACH")
		spec := "# Three refusals\n"
		issue := createAdviceIssue(t, handler, "EACH", "Three declined asks", &spec)
		want := []adviceDeclinedAsk{
			{Question: "Which transport should the exporter use?", Note: "Whatever the team can run."},
			{Question: "Which store should it write to?", Note: "Not my call."},
			{Question: "Which region holds the replica?", Note: ""},
		}
		for index := range want {
			want[index].ID = openAskAs(t, handler, issue.Key, "session-each", want[index].Question)
			declineTheAsk(t, handler, want[index].ID, want[index].Note)
		}

		asked := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": "Which window do we deploy in?", "actor": adviceSessionActor("session-each"),
		})
		if asked.Code != http.StatusCreated {
			t.Fatalf("the next ask: status=%d body=%s", asked.Code, asked.Body.String())
		}
		items, carried := declinedAsksOf(t, "the ask after three declines", asked.Body.String())
		if !carried || !slices.Equal(items, want) {
			t.Fatalf("declined_asks = %#v, want all three declined asks oldest first %#v", items, want)
		}
	})

	// The part stands apart from the related-item check's own result: a check that filled all
	// three of its places with other items keeps it, and a check that was not made at all keeps
	// it too.
	t.Run("whatever the related-item check answered", func(t *testing.T) {
		handler, _, _, served := newRegisteredTestServer(t, testServerOptions{})
		createAdviceProject(t, handler, "CROWD")
		// One invented word per filler issue, each held by that issue alone, so the picker
		// chooses them all and every filler is a keyword match: four owners for three places.
		words := []string{"zymurvexa", "zymurvexb", "zymurvexc", "zymurvexd"}
		for _, word := range words {
			createSuggestionIssue(t, handler, map[string]any{
				"project": "CROWD", "title": "Filler " + word, "spec": "Filler issue number " + word, "force": true,
			})
		}
		crowding := "Which of " + words[0] + " " + words[1] + " " + words[2] + " " + words[3] + " comes first?"
		spec := "# Crowded\n\nThe check has more to offer than it has room for.\n"
		issue := createAdviceIssue(t, handler, "CROWD", "Crowded suggestions", &spec)
		declinedID := openAskAs(t, handler, issue.Key, "session-crowded", question)
		declineTheAsk(t, handler, declinedID, note)
		declined := theDeclinedAsk(declinedID)

		crowded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": crowding, "actor": adviceSessionActor("session-crowded"),
		})
		if crowded.Code != http.StatusCreated {
			t.Fatalf("the ask whose check is crowded: status=%d body=%s", crowded.Code, crowded.Body.String())
		}
		suggestions := decodeBody[testAskWithSuggestions](t, crowded).Advice.Suggestions
		if suggestions.Outcome != relatedFound || len(suggestions.Related) != suggestionRelatedCount {
			t.Fatalf("suggestions = %+v, want all %d places filled by other items", suggestions, suggestionRelatedCount)
		}
		requireNamesTheDeclinedAsk(t, "the ask whose check filled every place", crowded.Body.String(), declined)

		// A check that cannot run at all: the allowance ends before its first read.
		served.suggestionTimeout = time.Nanosecond
		unchecked := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": "Which of them comes last?", "actor": adviceSessionActor("session-crowded"),
		})
		if unchecked.Code != http.StatusCreated {
			t.Fatalf("the ask whose check was not made: status=%d body=%s", unchecked.Code, unchecked.Body.String())
		}
		if outcome := decodeBody[testAskWithSuggestions](t, unchecked).Advice.Suggestions.Outcome; outcome != relatedNotMade {
			t.Fatalf("the check's outcome = %q, want %q", outcome, relatedNotMade)
		}
		requireNamesTheDeclinedAsk(t, "the ask whose check was not made", unchecked.Body.String(), declined)
	})

	// The read fails on its own: the field is left unset and every other line of the advice is
	// answered as ever, because what the session asked about is not this reminder.
	t.Run("a failed read leaves the rest of the advice standing", func(t *testing.T) {
		handler, _ := newFailingAdviceHandler(t, "declined_asks", 0)
		createAdviceProject(t, handler, "STANDS")
		spec := "# Standing advice\n"
		issue := createAdviceIssue(t, handler, "STANDS", "A failed declined-asks read", &spec)
		declinedID := openAskAs(t, handler, issue.Key, "session-standing", question)
		declineTheAsk(t, handler, declinedID, note)
		// An open ask of the same session, so your_open_asks has something to lose. It is the
		// first write since the person's decline, which is the human event the count runs from.
		blocker := openAskAs(t, handler, issue.Key, "session-standing", "Which region holds the replica?")

		asked := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": "Which store should the exporter write to?", "actor": adviceSessionActor("session-standing"),
		})
		if asked.Code != http.StatusCreated {
			t.Fatalf("the ask whose declined-asks read failed: status=%d body=%s", asked.Code, asked.Body.String())
		}
		advice := adviceFromResponse(t, asked.Code, asked.Body.String())
		if advice.IssueStatus != "triage" || advice.SessionWritesSinceHuman != 2 {
			t.Fatalf("advice = %#v, want the issue in triage and two writes since the decline", advice)
		}
		if len(advice.YourOpenAsks) != 1 || advice.YourOpenAsks[0].ID != blocker {
			t.Fatalf("your_open_asks = %#v, want the open ask %q", advice.YourOpenAsks, blocker)
		}
		if advice.LegionManagedStatus == nil || *advice.LegionManagedStatus {
			t.Fatalf("legion_managed_status = %#v, want it answered false", advice.LegionManagedStatus)
		}
		if items, carried := declinedAsksOf(t, "the ask whose declined-asks read failed", asked.Body.String()); carried {
			t.Fatalf("a failed read still emitted declined_asks %#v: %s", items, asked.Body.String())
		}
	})
}
