package texts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// Every kind on the list names the record type that declares its fields, a struct of its own.
// A task's record opens with the kind and the issue, apart from the look task (its lead names the
// issue) and the controller's start message (its text is main's, word for word); a notice's opens
// with the kind, the issue and the resend count; a role message's with the kind and the issue,
// apart from the hook prompt, which is published as its text. A notice or role message holds no
// Words and declares the words key exactly where it has a words field; a task holds its words in
// Words fields and has no map.
func TestEveryEntryHasARecord(t *testing.T) {
	owner := map[reflect.Type]Kind{}
	wordsMap := reflect.TypeFor[map[string]WordsMeta]()
	for _, entry := range Entries() {
		if entry.Record == nil {
			t.Errorf("%s has no record", entry.Kind)
			continue
		}
		typ := reflect.TypeOf(entry.Record)
		if typ.Kind() != reflect.Struct {
			t.Errorf("%s's record is a %s, not a struct", entry.Kind, typ.Kind())
			continue
		}
		if other, taken := owner[typ]; taken {
			t.Errorf("%s and %s share the record %s", other, entry.Kind, typ)
		}
		owner[typ] = entry.Kind
		if !reflect.ValueOf(entry.Record).IsZero() {
			t.Errorf("%s's record is not the zero value of %s", entry.Kind, typ)
		}

		fields := jsonFields(typ)
		names := make([]string, len(fields))
		seen := map[string]bool{}
		for i, field := range fields {
			names[i] = field.name
			if seen[field.name] {
				t.Errorf("%s declares %s twice", typ, field.name)
			}
			seen[field.name] = true
		}
		var head []string
		switch {
		case entry.Kind == CheckLook:
			head = []string{"kind"}
		case entry.Kind == ControllerStart || entry.Kind == HookPrompt:
		case entry.Form == Task, entry.Form == RoleMessage:
			head = []string{"kind", "issue"}
		default:
			head = []string{"kind", "issue", "resends"}
		}
		if len(names) < len(head) || !slices.Equal(names[:len(head)], head) {
			t.Errorf("%s's record %s declares %v first, want %v", entry.Kind, typ, names[:min(len(names), len(head))], head)
		}

		hasWords, declaresWords := false, false
		for _, field := range fields {
			isWords := field.typ == wordsType || field.typ == wordsPtrType
			switch {
			case entry.Form == Task && field.words:
				t.Errorf("%s: the task record %s tags %s texts:\"words\": a task holds words in a Words field", entry.Kind, typ, field.name)
			case entry.Form != Task && isWords:
				t.Errorf("%s: %s holds %s as Words: a notice or a role message holds words in a string tagged texts:\"words\"", entry.Kind, typ, field.name)
			case field.words && field.typ.Kind() != reflect.String:
				t.Errorf("%s: %s tags %s texts:\"words\", which is no string", entry.Kind, typ, field.name)
			}
			if field.name == "words" {
				declaresWords = true
				if field.typ != wordsMap || !field.optional {
					t.Errorf("%s: %s declares words as %s, want map[string]WordsMeta with omitempty", entry.Kind, typ, field.typ)
				}
			}
		}
		walkFields(typ, func(field jsonField) {
			hasWords = hasWords || field.words
			if entry.Form == Task && field.typ.Kind() == reflect.Map {
				t.Errorf("%s: the task record %s holds the map %s, which Render refuses", entry.Kind, typ, field.name)
			}
		})
		if entry.Form != Task && hasWords != declaresWords {
			t.Errorf("%s: %s has a words field %t and declares the words key %t; it declares it exactly where it has one",
				entry.Kind, typ, hasWords, declaresWords)
		}
	}
}

// Each task record renders, with two samples that differ in every field, as lines that open with
// its kind and its issue (the look task with its lead, the controller's start message as main's
// text), and each of its fields is a line or a block of one of the two.
func TestEveryTaskRecordRendersItsKindAndIssueFirst(t *testing.T) {
	for _, entry := range Entries() {
		if entry.Form != Task {
			continue
		}
		var texts [2]string
		for variant := range texts {
			record := sample(t, entry, variant)
			text, err := Render(record)
			if err != nil {
				t.Errorf("%s, sample %d: %v", entry.Kind, variant, err)
				continue
			}
			texts[variant] = text
			t.Logf("%s, sample %d:\n%s", entry.Kind, variant, text)
			lines := strings.Split(text, "\n")
			issue := sampleIssue(variant)
			switch entry.Kind {
			case ControllerStart:
				if text != ControllerStartMessage {
					t.Errorf("the controller's start message renders %q, want main's %q", text, ControllerStartMessage)
				}
				continue
			case CheckLook:
				look := record.(*CheckLookRecord)
				want := []string{"Production check of " + issue + ": " + look.Title + ".", "kind: " + string(CheckLook)}
				if len(lines) < 2 || !slices.Equal(lines[:2], want) {
					t.Errorf("the look task opens %q, want %q", lines[:min(2, len(lines))], want)
				}
			default:
				want := []string{"kind: " + string(entry.Kind), "issue: " + issue}
				if len(lines) < 2 || !slices.Equal(lines[:2], want) {
					t.Errorf("%s opens %q, want %q", entry.Kind, lines[:min(2, len(lines))], want)
				}
			}
		}
		if entry.Kind == ControllerStart {
			continue
		}
		if texts[0] == texts[1] {
			t.Errorf("%s renders its two samples alike", entry.Kind)
		}
		for _, field := range jsonFields(reflect.TypeOf(entry.Record)) {
			if !renderShows(texts, field) {
				t.Errorf("%s: neither sample shows %s", entry.Kind, field.name)
			}
		}
	}
}

// renderShows says whether one of texts shows field: a block of its name for Words, and a line
// that opens with its key for any other field.
func renderShows(texts [2]string, field jsonField) bool {
	for _, text := range texts {
		for _, line := range strings.Split(text, "\n") {
			if field.typ == wordsType || field.typ == wordsPtrType {
				if strings.HasPrefix(line, "<"+field.name+">") || strings.HasPrefix(line, "<"+field.name+" ") {
					return true
				}
				continue
			}
			if strings.HasPrefix(line, field.name+":") || strings.HasPrefix(line, field.name+"[") {
				return true
			}
		}
	}
	return false
}

// Each notice and role-message record marshals, with two samples that differ in every field, as
// one JSON object that opens with its kind and its issue, holds each key the record declares in
// one sample at least, and gives each key a different value in the two. The hook prompt, published
// as its text, is held to that text's order instead.
func TestEveryNoticeAndRoleMessageRecordMarshalsAnObject(t *testing.T) {
	for _, entry := range Entries() {
		if entry.Form == Task {
			continue
		}
		if entry.Kind == HookPrompt {
			for variant := range 2 {
				hook := sample(t, entry, variant).(*HookPromptRecord)
				text, err := hook.Text()
				if err != nil {
					t.Fatalf("the hook prompt: %v", err)
				}
				facts, err := Render(hook.HookFacts)
				if err != nil {
					t.Fatal(err)
				}
				want := fmt.Sprintf("Legion hook %s on %s: work went back from %s to %s.\n\n%s\n\n%s", hook.Hook, hook.Issue, hook.From, hook.To, hook.Prompt, facts)
				if text != want || !strings.HasPrefix(facts, "count: ") {
					t.Errorf("the hook prompt is\n%s\nwant\n%s", text, want)
				}
			}
			continue
		}
		var objects [2]map[string]json.RawMessage
		for variant := range objects {
			record := sample(t, entry, variant)
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Errorf("%s, sample %d: %v", entry.Kind, variant, err)
				continue
			}
			t.Logf("%s, sample %d: %s", entry.Kind, variant, encoded)
			head := fmt.Sprintf(`{"kind":%q,"issue":%q`, entry.Kind, sampleIssue(variant))
			if !bytes.HasPrefix(encoded, []byte(head)) {
				t.Errorf("%s marshals as %s, want it to open with %s", entry.Kind, encoded, head)
			}
			if err := json.Unmarshal(encoded, &objects[variant]); err != nil {
				t.Errorf("%s does not marshal as an object: %v", entry.Kind, err)
			}
		}
		for _, field := range jsonFields(reflect.TypeOf(entry.Record)) {
			first, inFirst := objects[0][field.name]
			second, inSecond := objects[1][field.name]
			switch {
			case !inFirst && !inSecond:
				t.Errorf("%s: neither sample holds %s", entry.Kind, field.name)
			case inFirst && inSecond && field.name != "kind" && bytes.Equal(first, second):
				t.Errorf("%s: both samples hold %s as %s", entry.Kind, field.name, first)
			}
		}
	}
}

// A notice stays one record.Notice on the wire (the plan's decision 4), so a key has one Go type
// in every notice record that declares it, and a key of main's record.Notice keeps main's JSON
// shape: the same type of value, the same omitempty, and, for a nested object, each of main's
// members with its shape, beside new members that are all omitempty. The one change of omitempty
// allowed is a member of zeroMeansNone gaining it. The keys that main's notice lacks are logged,
// which is the list record.Notice gains.
func TestEachNoticeKeyHasOneShape(t *testing.T) {
	onMain := map[string]jsonField{}
	for _, field := range jsonFields(reflect.TypeFor[mainNotice]()) {
		onMain[field.name] = field
	}
	type declared struct {
		field jsonField
		kinds []Kind
	}
	keys := map[string]*declared{}
	var order []string
	for _, entry := range Entries() {
		if entry.Form != Notice && entry.Form != ControllerNotice {
			continue
		}
		for _, field := range jsonFields(reflect.TypeOf(entry.Record)) {
			known, ok := keys[field.name]
			if !ok {
				keys[field.name] = &declared{field: field, kinds: []Kind{entry.Kind}}
				order = append(order, field.name)
				continue
			}
			if field.typ != known.field.typ {
				t.Errorf("%s declares %s as %s, and %s declares it as %s", entry.Kind, field.name, field.typ, known.kinds[0], known.field.typ)
			}
			known.kinds = append(known.kinds, entry.Kind)
		}
	}
	for _, name := range order {
		key := keys[name]
		was, ok := onMain[name]
		if !ok {
			t.Logf("new key %s: %s (%s)", name, key.field.typ, kindList(key.kinds))
			continue
		}
		for _, kind := range key.kinds {
			entry, _ := Lookup(kind)
			for _, field := range jsonFields(reflect.TypeOf(entry.Record)) {
				if field.name == name && field.optional != was.optional && !mayLeaveOutItsZero(name, field.optional, shapeOf(was.typ)) {
					t.Errorf("%s declares %s with omitempty %t, and main's notice with %t", kind, name, field.optional, was.optional)
				}
			}
		}
		for _, problem := range keepsShape(shapeOf(key.field.typ), shapeOf(was.typ), name) {
			t.Error(problem)
		}
	}
}

// zeroMeansNone is each member of main's record.Notice, by its path, whose zero value says that
// there is nothing to state, and which a record now leaves out when it is zero (omitzero or
// omitempty) where main wrote it. Decision 4 keeps the one record.Notice shape because a notice
// comes back whole: the listener echoes it in a role-lane exception, and the daemon decodes that
// into record.Notice with unknown fields refused (internal/daemon/notice_exceptions.go,
// DisallowUnknownFields). A key left out decodes to the zero value, which is what main wrote for
// it, so every reader deployed, main's daemon among them, reads the same value either way. Only a
// time, a duration or a text may be listed (mayLeaveOutItsZero), and the list names each by its
// path, so a member added to it is an edit that a reviewer sees.
var zeroMeansNone = map[string]bool{
	// The last step of a worker whose claim's record holds no step of its agent, which only a
	// background stall, judged by its report, names (admit's silentWorkers), and the time quiet
	// since it.
	"silent[].lastStep": true,
	"silent[].silent":   true,
	// The drift of an update round as the text an older daemon's round records. A round this
	// daemon opens names its drift by its items (images, settings) and records no text, so an
	// update-stalled notice of it states no empty reasons (UpdateRound.Reasons).
	"update.reasons": true,
}

// mayLeaveOutItsZero says whether a member of main's notice at path, of main's shape was, may be
// optional (omitempty or omitzero) where main's was not: a member of zeroMeansNone that is a time,
// a duration or a text, gaining omitempty and never losing it.
func mayLeaveOutItsZero(path string, optional bool, was jsonShape) bool {
	return optional && zeroMeansNone[path] && (was.kind == "time" || was.kind == "string" || was.kind == reflect.Int64.String())
}

func kindList(kinds []Kind) string {
	names := make([]string, len(kinds))
	for i, kind := range kinds {
		names[i] = string(kind)
	}
	return strings.Join(names, ", ")
}

// A record with a words field set marshals a words object whose keys are WordsPaths: the path of
// each words field that holds words, a field inside a list or a nested object included, and no
// other. Each path names, in the marshaled payload, the words it is the path of.
func TestTheWordsObjectNamesEachWordsFieldByItsPath(t *testing.T) {
	at := time.Date(2026, 11, 14, 22, 44, 10, 0, time.UTC)
	cases := []struct {
		name   string
		record any
		want   []string
	}{
		{"a summary and a backward reason", PhaseFinishedRecord{Summary: "Done.", Reason: "Two lines\nof it.", Verdict: "pass"},
			[]string{"summary", "reason"}},
		{"no words", PhaseFinishedRecord{Verdict: "pass"}, nil},
		{"an escalation", &HeldRecord{Hold: HoldEscalation, Escalation: "It needs a person."}, []string{"escalation"}},
		{"a changes request", DesignChangesRequestedRecord{Reason: "Use a table."}, []string{"reason"}},
		{"a parked answer", ParkedDecisionRecord{Answer: "Yes, ship it.", Options: []string{"ship"}}, []string{"answer"}},
		{"the second of two kept decisions", CatchUpRecord{CatchUp: &CatchUpFacts{Kept: &KeptFacts{Decisions: []DecisionMark{
			{Ref: "dispatch://INFRA-1/ask/a", At: at},
			{Ref: "dispatch://INFRA-2/ask/b", At: at, Answer: "No."},
		}}}}, []string{"catch_up.kept.decisions[1].answer"}},
		{"a task's blocks", KeptDecisionsRecord{Kept: []KeptDecision{
			{Ref: "dispatch://INFRA-1/ask/a", Answer: &Words{}},
			{Ref: "dispatch://INFRA-2/ask/b", Answer: &Words{More: true}},
			{Ref: "dispatch://INFRA-3/ask/c", Answer: &Words{Text: "Yes."}},
		}}, []string{"kept[1].answer", "kept[2].answer"}},
		{"a resume's blocks", RootResumeRecord{Reply: &Words{Text: "Fine."}, Note: &Words{}}, []string{"reply"}},
	}
	for _, c := range cases {
		if got := WordsPaths(c.record); !slices.Equal(got, c.want) {
			t.Errorf("%s: WordsPaths gives %q, want %q", c.name, got, c.want)
		}
	}

	// One record's words object written by hand, as a builder writes it: its one key is the path
	// WordsPaths gives, and that path names the words in the marshaled payload.
	kept := CatchUpRecord{NoticeHead: NoticeHead{Kind: CatchUp, Issue: "INFRA-1"}, CatchUp: &CatchUpFacts{Kept: &KeptFacts{
		Decisions: []DecisionMark{{Ref: "dispatch://INFRA-1/ask/a", At: at}, {Ref: "dispatch://INFRA-2/ask/b", At: at, Answer: "No."}},
	}}, Words: map[string]WordsMeta{"catch_up.kept.decisions[1].answer": {By: "user:alice", At: at}}}
	if got := WordsPaths(kept); !slices.Equal(got, []string{"catch_up.kept.decisions[1].answer"}) {
		t.Errorf("WordsPaths of the hand-built catch-up gives %q, want the one key its words object names", got)
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if words, _ := payload["words"].(map[string]any); len(words) != 1 || words["catch_up.kept.decisions[1].answer"] == nil {
		t.Errorf("the hand-built catch-up's words object is %v, want its one key", payload["words"])
	}
	if value, _ := atPath(payload, "catch_up.kept.decisions[1].answer").(string); value != "No." {
		t.Errorf("the payload holds %q at catch_up.kept.decisions[1].answer, want the person's words: %s", value, encoded)
	}

	// Every notice and role message of the list: each path WordsPaths gives names words in the
	// marshaled payload.
	for _, entry := range Entries() {
		if entry.Form == Task || entry.Kind == HookPrompt {
			continue
		}
		record := sample(t, entry, 0)
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("%s: %v", entry.Kind, err)
		}
		var payload map[string]any
		if err := json.Unmarshal(encoded, &payload); err != nil {
			t.Fatal(err)
		}
		for _, path := range WordsPaths(record) {
			if value, ok := atPath(payload, path).(string); !ok || value == "" {
				t.Errorf("%s: the payload holds no words at %s: %s", entry.Kind, path, encoded)
			}
		}
	}
}

// The sentences that other issues' approved requirements fix by their words render word for word
// (dispatch://INFRA-3686/spec R8): the cut-turn note as dispatch://INFRA-3353's R2 keeps it, with a
// cause and with none; the controller's start message; the design-approved wake of a
// resume whose gate is open, and of no other; the look task's first line; and the review that
// ends a stuck round.
func TestTheSentencesR8KeepsRenderWordForWord(t *testing.T) {
	const cut = "Your previous turn on this task was interrupted. Your process ended because its pod was replaced. " +
		"Your tool calls in flight were never answered. The daemon has no report of the background work of that turn. " +
		"So the subagents and background jobs of that turn are gone. " +
		"Every other subagent and background job that you started in that process is gone too. " +
		"No result of that work will come to you. Your transcript does not say so. " +
		"Before you do any of that work again, check what it already did. Then continue the task."
	const uncaused = "Your previous turn on this task was interrupted. Your process ended, and the daemon recorded no cause for it. " +
		"Your tool calls in flight were never answered. The daemon has no report of the background work of that turn. " +
		"So the subagents and background jobs of that turn are gone. " +
		"Every other subagent and background job that you started in that process is gone too. " +
		"No result of that work will come to you. Your transcript does not say so. " +
		"Before you do any of that work again, check what it already did. Then continue the task."
	for cause, want := range map[string]string{"its pod was replaced": cut, "": uncaused} {
		text, err := Render(CutTurnNoteRecord{TaskHead: TaskHead{Kind: CutTurnNote, Issue: "INFRA-3686"}, Cause: cause})
		if err != nil {
			t.Fatal(err)
		}
		if wantText := "kind: cut-turn-note\nissue: INFRA-3686\n" + want; text != wantText {
			t.Errorf("the cut-turn note with cause %q is\n%s\nwant\n%s", cause, text, wantText)
		}
	}

	text, err := Render(ControllerStartRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if want := `Legion controller start: follow skill://legion-controller's start procedure now ("What happened before you started"), then end the turn.`; text != want {
		t.Errorf("the controller's start message is %q, want %q", text, want)
	}

	for _, open := range []bool{true, false} {
		resume := RootResumeRecord{TaskHead: TaskHead{Kind: RootResume, Issue: "INFRA-3348"}, Act: ActApproved, GateOpen: new(open), GateArmed: new(true)}
		text, err := Render(resume)
		if err != nil {
			t.Fatal(err)
		}
		kept := slices.Contains(strings.Split(text, "\n"), "this resume is your design-approved wake")
		if kept != open {
			t.Errorf("a resume whose gate is open %t carries the design-approved wake %t:\n%s", open, kept, text)
		}
	}

	look := CheckLookRecord{Issue: "INFRA-3686", Title: "Dispatch: reduce verbosity of agent notifications", Kind: CheckLook, Look: 1,
		WokenBy: WokenByMerge, PullRequest: PullRequestRef("cybermonklabs/legion", 4211)}
	text, err = Render(look)
	if err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(text, "\n"); first != "Production check of INFRA-3686: Dispatch: reduce verbosity of agent notifications." {
		t.Errorf("the look task opens %q", first)
	}

	if got := EndsRound("5c2b01d9e8f7"); got != "APPROVE of head 5c2b01d9e8f7" {
		t.Errorf("EndsRound gives %q", got)
	}
}

// The helpers write each reference in one form: a pull request as owner/name#n, the command that
// lists its checks, an image by the first 12 hexadecimal characters of its digest; and the two
// constructors set the facts that every text of their kind gives.
func TestTheHelpersWriteTheReferencesInOneForm(t *testing.T) {
	if got := PullRequestRef("cybermonklabs/legion", 4211); got != "cybermonklabs/legion#4211" {
		t.Errorf("PullRequestRef gives %q", got)
	}
	if got := ListPassed("cybermonklabs/legion", 4211); got != "gh pr checks 4211 --repo cybermonklabs/legion" {
		t.Errorf("ListPassed gives %q", got)
	}
	const hex = "5c2b01d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1"
	for image, want := range map[string]string{
		"ghcr.io/cybermonklabs/legion-worker@sha256:" + hex: "5c2b01d9e8f7",
		"sha256:" + hex:                    "5c2b01d9e8f7",
		"sha256:abc":                       "abc",
		"registry:5000/legion:v1":          "registry:5000/legion:v1",
		"ghcr.io/cybermonklabs/legion":     "ghcr.io/cybermonklabs/legion",
		"ghcr.io/x@sha256:NOT-HEX-HERE":    "ghcr.io/x@sha256:NOT-HEX-HERE",
		"ghcr.io/x/legion-worker:ff33c8c":  "ghcr.io/x/legion-worker:ff33c8c",
		"ghcr.io/x/legion-worker:20261118": "ghcr.io/x/legion-worker:20261118",
	} {
		if got := ShortDigest(image); got != want {
			t.Errorf("ShortDigest(%q) = %q, want %q", image, got, want)
		}
	}

	ends := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	ask := NewUpdateAsk("INFRA-3686", "round-7", "pod-a", []string{"5c2b01d9e8f7"}, []string{"runtime.kubernetes.image"}, ends)
	if ask.Kind != UpdateAsk || ask.Issue != "INFRA-3686" || ask.PushTo != claim.IssueBranch("INFRA-3686") || ask.Answer != "ready_for_update" ||
		ask.BackgroundWork != UpdateBackgroundStep || ask.Evidence != UpdateEvidenceStep || ask.Then != UpdateThen ||
		ask.LateBackgroundWork != UpdateLateBackgroundWork || ask.EveryAnswerBringsItForward != UpdateEarlyEnd ||
		!ask.NoAnswerDelaysIt || !ask.KeptByPushOnly || !ask.NextTaskHeld ||
		ask.Restart != UpdateRestartsIn || len(ask.GoesWithPod) != 7 || !ask.EndsAt.Equal(ends) {
		t.Errorf("NewUpdateAsk gives %+v", ask)
	}
	ask.GoesWithPod[0] = "changed"
	if again := NewUpdateAsk("INFRA-1", "r", "", nil, nil, ends); again.GoesWithPod[0] != "turn" {
		t.Error("a change to one ask's goes_with_pod reached the next ask")
	}
	if note := NewOutOfMemoryNote("INFRA-3686"); note.Kind != OutOfMemoryNote || note.Issue != "INFRA-3686" || note.Reason != "OOMKilled" {
		t.Errorf("NewOutOfMemoryNote gives %+v", note)
	}

	blocked, err := json.Marshal(BlockedIssue{Issue: "INFRA-1"})
	if err != nil || string(blocked) != `{"issue":"INFRA-1","blockedBy":[]}` {
		t.Errorf("a blocked issue with no blocker marshals as %s (%v), want blockedBy []", blocked, err)
	}
	round, err := json.Marshal(UpdateRound{Round: "r"})
	if err != nil || string(round) != `{"round":"r","waiting":[]}` {
		t.Errorf("an update round that waits on nobody and records no reasons marshals as %s (%v), want waiting [] and no reasons", round, err)
	}
}

// sampleAt is the time every sample's times count from.
var sampleAt = time.Date(2026, 11, 14, 21, 5, 1, 0, time.UTC)

// sampleIssue is the issue of sample variant.
func sampleIssue(variant int) string {
	return "INFRA-" + strconv.Itoa(3686+variant)
}

// sample is a pointer to a record of entry's kind with every field set, each to a value of its
// own that differs between variant 0 and variant 1: a string names its field and its place, a
// bool is true in variant 0 alone, a number counts, a time is a minute apart per field, a list
// holds one record in variant 0 and two in variant 1, and words are two lines. The kind and the
// issue are the entry's kind and sampleIssue. A record with a words object names each path of
// WordsPaths in it, with an author and a time.
func sample(t *testing.T, entry Entry, variant int) any {
	t.Helper()
	record := reflect.New(reflect.TypeOf(entry.Record))
	f := filler{variant: variant}
	f.fill(record.Elem(), "", false)
	if kind := record.Elem().FieldByName("Kind"); kind.IsValid() {
		kind.Set(reflect.ValueOf(entry.Kind))
	}
	if issue := record.Elem().FieldByName("Issue"); issue.IsValid() {
		issue.SetString(sampleIssue(variant))
	}
	if words := record.Elem().FieldByName("Words"); words.IsValid() && words.Kind() == reflect.Map {
		meta := map[string]WordsMeta{}
		for _, path := range WordsPaths(record.Interface()) {
			meta[path] = WordsMeta{By: "user:person-" + strconv.Itoa(variant), At: sampleAt.Add(time.Duration(variant) * time.Hour)}
		}
		if len(meta) > 0 {
			words.Set(reflect.ValueOf(meta))
		}
	}
	return record.Interface()
}

// filler sets every field of a sample (sample says how).
type filler struct {
	variant int
	count   int
}

func (f *filler) fill(v reflect.Value, name string, words bool) {
	f.count++
	n := f.variant*1000 + f.count
	switch v.Type() {
	case timeType:
		v.Set(reflect.ValueOf(sampleAt.Add(time.Duration(n) * time.Minute)))
		return
	case wordsType:
		v.Set(reflect.ValueOf(Words{Text: fmt.Sprintf("Words of %s, sample %d.\nA second line.", name, f.variant)}))
		return
	}
	switch v.Kind() {
	case reflect.String:
		if words {
			v.SetString(fmt.Sprintf("Words of %s, sample %d.\nA second line.", name, f.variant))
		} else {
			v.SetString(fmt.Sprintf("%s-%d", strings.ToLower(name), n))
		}
	case reflect.Bool:
		v.SetBool(f.variant == 0)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(n))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(n))
	case reflect.Pointer:
		target := reflect.New(v.Type().Elem())
		f.fill(target.Elem(), name, words)
		v.Set(target)
	case reflect.Slice:
		items := reflect.MakeSlice(v.Type(), f.variant+1, f.variant+1)
		for i := range items.Len() {
			f.fill(items.Index(i), fmt.Sprintf("%s%d", name, i), words)
		}
		v.Set(items)
	case reflect.Struct:
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() || field.Type.Kind() == reflect.Map {
				continue
			}
			f.fill(v.Field(i), field.Name, field.Tag.Get("texts") == wordsTag)
		}
	}
}

// jsonField is one key of a record as encoding/json reads it.
type jsonField struct {
	name     string
	typ      reflect.Type
	optional bool
	words    bool
}

// jsonFields is the keys of the struct type typ in declaration order, an embedded struct with no
// json tag giving its fields in its place, as encoding/json reads them.
func jsonFields(typ reflect.Type) []jsonField {
	var fields []jsonField
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag, tagged := field.Tag.Lookup("json")
		if tag == "-" {
			continue
		}
		if field.Anonymous && !tagged {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				fields = append(fields, jsonFields(embedded)...)
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		if name == "" {
			name = field.Name
		}
		options = "," + options + ","
		fields = append(fields, jsonField{
			name:     name,
			typ:      field.Type,
			optional: strings.Contains(options, ",omitempty,") || strings.Contains(options, ",omitzero,"),
			words:    field.Tag.Get("texts") == wordsTag,
		})
	}
	return fields
}

// walkFields calls visit for each key of typ and of every object nested in it.
func walkFields(typ reflect.Type, visit func(jsonField)) {
	for _, field := range jsonFields(typ) {
		visit(field)
		inner := field.typ
		for inner.Kind() == reflect.Pointer || inner.Kind() == reflect.Slice || inner.Kind() == reflect.Array {
			inner = inner.Elem()
		}
		if inner.Kind() == reflect.Struct && inner != timeType && inner != wordsType {
			walkFields(inner, visit)
		}
	}
}

// jsonShape is the shape a Go type takes in JSON: a primitive by its Go kind (every string type is
// string, a time is time), a list or a map of a shape, or an object of keys.
type jsonShape struct {
	kind    string
	elem    *jsonShape
	members map[string]jsonMember
}

// jsonMember is one key of an object's shape and whether encoding/json leaves it out when empty.
type jsonMember struct {
	shape    jsonShape
	optional bool
}

func shapeOf(typ reflect.Type) jsonShape {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == timeType {
		return jsonShape{kind: "time"}
	}
	switch typ.Kind() {
	case reflect.String:
		return jsonShape{kind: "string"}
	case reflect.Slice, reflect.Array:
		elem := shapeOf(typ.Elem())
		return jsonShape{kind: "list", elem: &elem}
	case reflect.Map:
		elem := shapeOf(typ.Elem())
		return jsonShape{kind: "map", elem: &elem}
	case reflect.Struct:
		members := map[string]jsonMember{}
		for _, field := range jsonFields(typ) {
			members[field.name] = jsonMember{shape: shapeOf(field.typ), optional: field.optional}
		}
		return jsonShape{kind: "object", members: members}
	}
	return jsonShape{kind: typ.Kind().String()}
}

// keepsShape lists how got fails to keep main's shape at path: another kind of value, a member of
// main's gone or with another omitempty (bar a member of zeroMeansNone gaining it), or a new
// member that is not omitempty.
func keepsShape(got, main jsonShape, path string) []string {
	if got.kind != main.kind {
		return []string{fmt.Sprintf("%s is a %s in the records, and a %s in main's notice", path, got.kind, main.kind)}
	}
	var problems []string
	switch got.kind {
	case "list", "map":
		problems = keepsShape(*got.elem, *main.elem, path+"[]")
	case "object":
		for _, name := range slices.Sorted(mapKeys(main.members)) {
			was := main.members[name]
			is, ok := got.members[name]
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("%s.%s, a key of main's notice, is gone", path, name))
				continue
			case is.optional != was.optional && !mayLeaveOutItsZero(path+"."+name, is.optional, was.shape):
				problems = append(problems, fmt.Sprintf("%s.%s has omitempty %t, and main's %t", path, name, is.optional, was.optional))
			}
			problems = append(problems, keepsShape(is.shape, was.shape, path+"."+name)...)
		}
		for _, name := range slices.Sorted(mapKeys(got.members)) {
			if _, ok := main.members[name]; !ok && !got.members[name].optional {
				problems = append(problems, fmt.Sprintf("%s.%s is new and not omitempty", path, name))
			}
		}
	}
	return problems
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

// atPath is the value at a words path (summary, catch_up.kept.decisions[1].answer) of a decoded
// payload, nil where the path names nothing.
func atPath(payload any, path string) any {
	value := payload
	for _, step := range strings.Split(path, ".") {
		name, indexes, _ := strings.Cut(step, "[")
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[name]
		for indexes != "" {
			index, rest, _ := strings.Cut(indexes, "]")
			indexes = strings.TrimPrefix(rest, "[")
			list, ok := value.([]any)
			i, err := strconv.Atoi(index)
			if !ok || err != nil || i >= len(list) {
				return nil
			}
			value = list[i]
		}
	}
	return value
}

// mainNotice is main's record.Notice at b4a566cb (packages/daemon/internal/record/outbox.go), with
// its nested types, field for field and tag for tag: the JSON shape that each of its keys keeps.
// Pause is the key main added at c7e1cbd5, the account of a project-paused or project-resumed
// notice, and Count and GiveUps the two main added at 40b01992, the count and the names of an
// intake-gave-up notice, each in main's shape.
type mainNotice struct {
	Kind             string             `json:"kind"`
	Role             claim.Role         `json:"role,omitempty"`
	Phase            phase.Phase        `json:"phase,omitempty"`
	Summary          string             `json:"summary,omitempty"`
	Verdict          string             `json:"verdict,omitempty"`
	Version          int                `json:"version,omitempty"`
	Reason           string             `json:"reason,omitempty"`
	Topic            string             `json:"topic,omitempty"`
	Failure          *mainNoticeFailure `json:"failure,omitempty"`
	Escalation       string             `json:"escalation,omitempty"`
	Resends          int                `json:"resends,omitempty"`
	PacedAt          time.Time          `json:"paced_at,omitzero"`
	PacedFrom        time.Time          `json:"paced_from,omitzero"`
	ResendOf         int64              `json:"resend_of,omitempty"`
	CatchUp          *mainCatchUp       `json:"catch_up,omitempty"`
	OpenCapabilities []string           `json:"openCapabilities,omitempty"`
	Blocked          []mainBlockedIssue `json:"blocked,omitempty"`
	Held             []mainHeldIssue    `json:"held,omitempty"`
	Silent           []mainSilentIssue  `json:"silent,omitempty"`
	Actor            string             `json:"actor,omitempty"`
	Ref              string             `json:"ref,omitempty"`
	Decision         string             `json:"decision,omitempty"`
	Root             string             `json:"root,omitempty"`
	Update           *mainUpdateStalled `json:"update,omitempty"`
	Pause            *mainProjectPause  `json:"pause,omitempty"`
	Count            int                `json:"count,omitempty"`
	GiveUps          []mainGiveUp       `json:"giveUps,omitempty"`
}

type mainGiveUp struct {
	ID       int64  `json:"id"`
	Consumer string `json:"consumer"`
	Subject  string `json:"subject"`
}

type mainProjectPause struct {
	ID     int64          `json:"id"`
	By     mainPauseActor `json:"by"`
	At     time.Time      `json:"at"`
	Reason string         `json:"reason,omitempty"`
}

type mainPauseActor struct {
	Kind    string `json:"kind"`
	Session string `json:"session,omitempty"`
}

type mainNoticeFailure struct {
	Cause          string            `json:"cause"`
	Detail         string            `json:"detail,omitempty"`
	LaunchFailures mainFailureBudget `json:"launchFailures"`
	Deaths         mainFailureBudget `json:"deaths"`
	PromptFailures mainFailureBudget `json:"promptFailures"`
	PromptRetires  mainFailureBudget `json:"promptRetires"`
	Death          string            `json:"death,omitempty"`
	StartFailure   string            `json:"startFailure,omitempty"`
}

type mainFailureBudget struct {
	Count int `json:"count"`
	Limit int `json:"limit"`
}

type mainUpdateStalled struct {
	Round   string              `json:"round"`
	Pod     string              `json:"pod,omitempty"`
	Reasons string              `json:"reasons"`
	Waiting []mainUpdateWaiting `json:"waiting"`
}

type mainUpdateWaiting struct {
	Claim    string     `json:"claim"`
	Role     claim.Role `json:"role,omitempty"`
	State    string     `json:"state"`
	Answered bool       `json:"answered,omitempty"`
}

type mainBlockedIssue struct {
	Issue     string   `json:"issue"`
	BlockedBy []string `json:"blockedBy"`
}

type mainHeldIssue struct {
	Issue  string   `json:"issue"`
	Reason string   `json:"reason"`
	HeldBy []string `json:"heldBy,omitempty"`
}

type mainSilentIssue struct {
	Issue           string     `json:"issue"`
	Role            claim.Role `json:"role"`
	LastStep        time.Time  `json:"lastStep"`
	Silent          string     `json:"silent"`
	BackgroundSince time.Time  `json:"backgroundSince,omitzero"`
}

type mainCatchUp struct {
	Generation uint64             `json:"generation"`
	Launch     uint64             `json:"launch"`
	HandedOver bool               `json:"handed_over,omitempty"`
	Carried    bool               `json:"carried,omitempty"`
	Gates      []mainCatchUpGate  `json:"gates"`
	Issues     []mainCatchUpIssue `json:"issues"`
	Decided    string             `json:"decided,omitempty"`
	Kept       *mainKeptDecisions `json:"kept,omitempty"`
}

type mainCatchUpGate struct {
	Step         string `json:"step"`
	Gated        bool   `json:"gated"`
	Unregistered bool   `json:"unregistered,omitempty"`
	Artifact     string `json:"artifact,omitempty"`
	Version      int    `json:"version,omitempty"`
	Approved     int    `json:"approved,omitempty"`
	Open         bool   `json:"open"`
}

type mainCatchUpIssue struct {
	Key    string      `json:"key"`
	Parent string      `json:"parent,omitempty"`
	Title  string      `json:"title"`
	Phase  phase.Phase `json:"phase"`
	Status string      `json:"status"`
}

type mainKeptDecisions struct {
	Decisions []mainDecisionMark `json:"decisions,omitempty"`
	mainKeptApproval
}

type mainKeptApproval struct {
	Step     string `json:"step,omitempty"`
	Approved int    `json:"approved,omitempty"`
	Latest   int    `json:"latest,omitempty"`
}

type mainDecisionMark struct {
	Ref      string    `json:"ref"`
	At       time.Time `json:"at"`
	Issue    string    `json:"issue,omitempty"`
	Decision string    `json:"decision,omitempty"`
	Actor    string    `json:"actor,omitempty"`
}
