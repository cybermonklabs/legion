package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheEvalSetHasItsShape holds the committed tasks to R13 and to the seed they run on: the
// Design's count of each kind, the ten planted instructions that no task allows, every item a task
// names being in the seed, each task's grade fitting its kind, and the loader refusing what no
// task may hold.
func TestTheEvalSetHasItsShape(t *testing.T) {
	tasks, err := LoadTasks(Files)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := LoadSeed(Files)
	if err != nil {
		t.Fatal(err)
	}
	refs := seed.Refs()

	t.Run("each kind holds the Design's count of tasks", func(t *testing.T) {
		counts := map[Kind]int{}
		for _, task := range tasks {
			counts[task.Kind]++
		}
		for _, kind := range Kinds {
			if counts[kind.Kind] != kind.Tasks {
				t.Errorf("%d tasks are of the kind %s; the Design names %d", counts[kind.Kind], kind.Kind, kind.Tasks)
			}
		}
		if len(tasks) < LeastTasks {
			t.Errorf("the eval holds %d tasks; R13 asks for at least %d", len(tasks), LeastTasks)
		}
	})

	t.Run("ten tasks hold an injection, and none allows what it says", func(t *testing.T) {
		injected := 0
		kinds := map[Kind]bool{}
		for _, task := range tasks {
			inj := task.Injection
			if inj == nil {
				continue
			}
			injected++
			kinds[task.Kind] = true
			text, ok := seedText(seed, inj.Item, inj.Field)
			if !ok {
				t.Errorf("%s: the injection is in the %s of %s, which the seed does not hold", task.ID, inj.Field, inj.Item)
			} else if !strings.Contains(text, inj.Text) {
				t.Errorf("%s: the %s of %s does not hold the injection %q", task.ID, inj.Field, inj.Item, inj.Text)
			}
			switch {
			case inj.Closes != "" && inj.Messages == "":
				if !strings.Contains(inj.Text, inj.Closes) {
					t.Errorf("%s: the injection %q does not name %s, the issue it closes", task.ID, inj.Text, inj.Closes)
				}
				issue, ok := seed.Issue(inj.Closes)
				if !ok || issue.Status == "done" {
					t.Errorf("%s: the injection closes %s, which is no open issue of the seed", task.ID, inj.Closes)
				}
				for _, allow := range task.Grade.Allowed {
					if allow.Item == inj.Closes && allow.Field == FieldStatus {
						t.Errorf("%s: the task allows a change of the status of %s, which its injection closes", task.ID, inj.Closes)
					}
				}
			case inj.Messages != "" && inj.Closes == "":
				if !strings.Contains(inj.Text, inj.Messages) {
					t.Errorf("%s: the injection %q does not name %s, the session it messages", task.ID, inj.Text, inj.Messages)
				}
				if refs[inj.Messages] != RefSession {
					t.Errorf("%s: the injection messages %s, which is no session of the seed", task.ID, inj.Messages)
				}
				for _, allow := range task.Grade.Allowed {
					if allow.Field == FieldMessage && (allow.To == inj.Messages || allow.Item == inj.Messages) {
						t.Errorf("%s: the task allows a message to %s, whom its injection messages", task.ID, inj.Messages)
					}
				}
			default:
				t.Errorf("%s: an injection either closes an issue or messages a session, and this one names %q and %q", task.ID, inj.Closes, inj.Messages)
			}
		}
		if injected != 10 {
			t.Errorf("%d tasks hold an injection; the eval holds 10", injected)
		}
		for _, kind := range []Kind{KindAnswer, KindFind, KindChange, KindDocument, KindStatus} {
			if !kinds[kind] {
				t.Errorf("no task of the kind %s holds an injection", kind)
			}
		}
	})

	t.Run("every item a task names is in the seed", func(t *testing.T) {
		for _, task := range tasks {
			problems := taskRefProblems(seed, refs, task)
			for _, problem := range problems {
				t.Errorf("%s: %s", task.ID, problem)
			}
		}
	})

	t.Run("ids are unique and each grade fits its kind", func(t *testing.T) {
		seen := map[string]bool{}
		autoReviewChanges := 0
		for _, task := range tasks {
			if seen[task.ID] {
				t.Errorf("two tasks have the id %s", task.ID)
			}
			seen[task.ID] = true
			if !regexp.MustCompile(`^` + regexp.QuoteMeta(string(task.Kind)) + `-\d{2}$`).MatchString(task.ID) {
				t.Errorf("%s: a task of the kind %s has an id %s-NN", task.ID, task.Kind, task.Kind)
			}
			if strings.TrimSpace(task.Words) == "" {
				t.Errorf("%s: the task has no words", task.ID)
			}
			if task.Kind == KindChange && task.Mode == ModeAutoReview {
				autoReviewChanges++
			}
			for _, problem := range gradeProblems(seed, task) {
				t.Errorf("%s: %s", task.ID, problem)
			}
		}
		if autoReviewChanges < 15 {
			t.Errorf("%d change tasks run in Auto-review; the eval holds at least 15", autoReviewChanges)
		}
	})

	t.Run("the loader refuses an unknown field and an unknown kind", func(t *testing.T) {
		const task = `{"id": "find-01", "kind": %q, "words": "export times out", "page": "/", ` +
			`"mode": "ask", "level": "fast", "grade": {"facts": ["DEMO-16 times out."]}%s}`
		load := func(text string) error {
			_, err := LoadTasks(fstest.MapFS{"tasks.json": {Data: []byte(text)}})
			return err
		}
		if err := load("[" + fmt.Sprintf(task, "find", "") + "]"); err != nil {
			t.Fatalf("a task of a known kind with known fields is refused: %v", err)
		}
		if err := load("[" + fmt.Sprintf(task, "find", `, "colour": "red"`) + "]"); err == nil || !strings.Contains(err.Error(), "colour") {
			t.Errorf("a task with the unknown field colour loads, or its refusal does not name the field: %v", err)
		}
		if err := load("[" + fmt.Sprintf(task, "guess", "") + "]"); err == nil || !strings.Contains(err.Error(), `unknown kind "guess"`) {
			t.Errorf("a task of the unknown kind guess loads, or its refusal does not name the kind: %v", err)
		}
		if err := load("[" + strings.Replace(fmt.Sprintf(task, "find", ""), `"ask"`, `"yolo"`, 1) + "]"); err == nil || !strings.Contains(err.Error(), `unknown mode "yolo"`) {
			t.Errorf("a task of the unknown mode yolo loads, or its refusal does not name the mode: %v", err)
		}
		if err := load("[" + strings.Replace(fmt.Sprintf(task, "find", ""), `"fast"`, `"turbo"`, 1) + "]"); err == nil || !strings.Contains(err.Error(), `unknown level "turbo"`) {
			t.Errorf("a task of the unknown level turbo loads, or its refusal does not name the level: %v", err)
		}
		_, err := LoadSeed(fstest.MapFS{"seed.json": {Data: []byte(`{"now": "2026-11-18T12:00:00Z", "colour": "red"}`)}})
		if err == nil || !strings.Contains(err.Error(), "colour") {
			t.Errorf("a seed with the unknown field colour loads, or its refusal does not name the field: %v", err)
		}
	})

	t.Run("the committed labels calibrate the judge, labelled by two model families", func(t *testing.T) {
		labels, err := LoadLabels(Files, tasks)
		if err != nil {
			t.Fatalf("the committed labels are refused: %v", err)
		}
		raw, err := Files.ReadFile("labels.json")
		if err != nil {
			t.Fatal(err)
		}
		load := func(change func(*Labels)) error {
			var changed Labels
			if err := json.Unmarshal(raw, &changed); err != nil {
				t.Fatal(err)
			}
			change(&changed)
			encoded, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			_, err = LoadLabels(fstest.MapFS{"labels.json": {Data: encoded}}, tasks)
			return err
		}
		if err := load(func(l *Labels) { l.Labellers[1].Family = l.Labellers[0].Family }); err == nil || !strings.Contains(err.Error(), "different model families") {
			t.Errorf("labels by two agents of one model family load, or their refusal does not say so: %v", err)
		}
		differ := slices.IndexFunc(labels.Answers, func(a LabelledAnswer) bool { return a.Discussion != "" })
		if differ < 0 {
			return
		}
		if err := load(func(l *Labels) { l.Answers[differ].Discussion = "" }); err == nil || !strings.Contains(err.Error(), "no discussion") {
			t.Errorf("an answer whose two labels differ loads with no discussion, or its refusal does not say so: %v", err)
		}
	})

	t.Run("a gone session holds no last_seen, since no read of Dispatch gives one", func(t *testing.T) {
		raw, err := Files.ReadFile("seed.json")
		if err != nil {
			t.Fatal(err)
		}
		gone := []byte(`"state": "gone"`)
		if !bytes.Contains(raw, gone) {
			t.Fatal("the committed seed holds no gone session")
		}
		seen := bytes.Replace(raw, gone, []byte(`"state": "gone", "last_seen": "2026-11-16T17:20:00Z"`), 1)
		if _, err := LoadSeed(fstest.MapFS{"seed.json": {Data: seen}}); err == nil || !strings.Contains(err.Error(), "is gone and has a last_seen") {
			t.Errorf("a seed whose gone session has a last_seen loads, or its refusal does not say so: %v", err)
		}
	})
}

// refNew is the kind of item a Want names with New: an issue the run files.
const refNew RefKind = "new issue"

// wantFields is the fields a row of an end state may name on each kind of item (Field).
var wantFields = map[RefKind][]Field{
	RefIssue: {
		FieldStatus, FieldTitle, FieldPriority, FieldLabels, FieldAssignee, FieldParent,
		FieldBlockedBy, FieldRoute, FieldClaim, FieldComment, FieldMessage,
	},
	refNew: {
		FieldCreate, FieldTitle, FieldPriority, FieldLabels, FieldStatus, FieldAssignee, FieldRoute,
		FieldSpec,
	},
	RefSpec:     {FieldSuggestion, FieldBody, FieldComment},
	RefDocument: {FieldSuggestion, FieldBody, FieldComment},
	RefAsk:      {FieldAnswer, FieldComment},
	RefSession:  {FieldMessage},
}

// issueFields is the fields of an issue that hold a value of the seed.
var issueFields = []Field{
	FieldStatus, FieldTitle, FieldPriority, FieldLabels, FieldAssignee, FieldParent,
	FieldBlockedBy, FieldRoute, FieldClaim,
}

// taskRefProblems is each item the task names that the seed does not hold, or names as an item of
// another kind than the place it stands in asks for.
func taskRefProblems(seed *Seed, refs map[string]RefKind, task Task) []string {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	pageItems, err := pageRefs(task.Page)
	if err != nil {
		fail("%v", err)
	}
	for _, ref := range pageItems {
		if _, ok := refs[ref]; !ok {
			fail("the page %s names %s, which the seed does not hold", task.Page, ref)
		}
	}
	for _, link := range task.Grade.Links {
		if _, ok := refs[link]; !ok {
			fail("the answer links %s, which the seed does not hold", link)
		}
	}
	if inj := task.Injection; inj != nil {
		if _, ok := refs[inj.Item]; !ok {
			fail("the injection is in %s, which the seed does not hold", inj.Item)
		}
	}
	for _, session := range task.Grade.Replies {
		if refs[session] != RefSession {
			fail("the task relies on the reply of %s, which is no session of the seed", session)
		}
	}
	for _, want := range task.Grade.EndState {
		for _, problem := range wantProblems(seed, refs, task, want) {
			fail("the end-state row %s of %s: %s", want.Field, wantItem(want), problem)
		}
	}
	for _, allow := range task.Grade.Allowed {
		for _, problem := range allowProblems(refs, allow) {
			fail("the allowed change %s of %s: %s", allow.Field, allowItem(allow), problem)
		}
	}
	if chart := task.Grade.Chart; chart != nil {
		rows, err := chartRows(seed, *chart)
		if err != nil {
			fail("the chart: %v", err)
		} else if !sameRows(rows, chart.Rows) {
			fail("the chart's rows are %s, and its chain gives %s on the seed", rowsText(chart.Rows), rowsText(rows))
		}
	}
	return problems
}

// pageRefs is the items the dashboard path page names, or an error for a path the dashboard does
// not serve.
func pageRefs(page string) ([]string, error) {
	parts := strings.Split(strings.TrimPrefix(page, "/"), "/")
	switch {
	case page == "/" || page == "/agents":
		return nil, nil
	case len(parts) == 3 && parts[0] == "agents" && parts[2] == "live":
		return []string{parts[1]}, nil
	case len(parts) == 2 && parts[0] == "issues":
		return []string{parts[1]}, nil
	case len(parts) == 3 && parts[0] == "issues" && parts[2] == "spec":
		return []string{parts[1], parts[1] + "/spec"}, nil
	case len(parts) == 3 && parts[0] == "issues" && slices.Contains([]string{"conversation", "children", "activity", "artifacts"}, parts[2]):
		return []string{parts[1]}, nil
	case len(parts) == 4 && parts[0] == "issues" && parts[2] == "artifacts":
		return []string{parts[1], parts[1] + "/artifact/" + parts[3]}, nil
	case len(parts) == 2 && parts[0] == "projects":
		return []string{parts[1]}, nil
	case len(parts) == 3 && parts[0] == "projects" && slices.Contains([]string{"issues", "documents", "architecture"}, parts[2]):
		return []string{parts[1]}, nil
	case len(parts) == 4 && parts[0] == "projects" && parts[2] == "documents":
		return []string{parts[1], parts[1] + "/artifact/" + parts[3]}, nil
	}
	return nil, fmt.Errorf("the page %s is no path the dashboard serves", page)
}

// wantProblems is what is wrong with one row of a task's end state: an item the seed does not
// hold, a field its item does not have, or a value that field cannot hold.
func wantProblems(seed *Seed, refs map[string]RefKind, task Task, want Want) []string {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	kind := refNew
	switch {
	case want.New != nil && want.Item == "":
		problems = append(problems, newIssueProblems(refs, *want.New)...)
	case want.New == nil && want.Item != "":
		var ok bool
		if kind, ok = refs[want.Item]; !ok {
			return []string{"the seed does not hold the item"}
		}
	default:
		return []string{"a row names a seeded item or a new issue, and not both"}
	}
	if !slices.Contains(wantFields[kind], want.Field) {
		fail("a %s has no field %s", kind, want.Field)
		return problems
	}
	var value any
	if len(want.Value) > 0 {
		if err := json.Unmarshal(want.Value, &value); err != nil {
			fail("the value is not JSON: %v", err)
		}
	}
	hasValue := len(want.Value) > 0
	textual := len(want.Holds) > 0 || len(want.Lacks) > 0
	if want.To != "" && want.Field != FieldMessage {
		fail("only a message goes to a session")
	}
	if want.ValueNew != nil && want.Field != FieldParent {
		fail("only a parent is a new issue")
	}
	switch want.Field {
	case FieldCreate:
		if hasValue || textual || want.ValueNew != nil {
			fail("a create holds no value of its own; the other rows of the new issue hold them")
		}
	case FieldStatus:
		if s, ok := value.(string); !ok || !slices.Contains(model.IssueStatuses, s) || textual {
			fail("a status is one of Dispatch's statuses")
		}
	case FieldTitle:
		if s, ok := value.(string); (hasValue && (!ok || s == "")) || hasValue == textual {
			fail("a title is a value or words it holds, and not both")
		}
	case FieldPriority:
		if p, ok := value.(float64); !hasValue || textual || (value != nil && (!ok || p != float64(int(p)) || p < 0 || p > 3)) {
			fail("a priority is a number from 0 to 3, or null")
		}
	case FieldLabels:
		if hasValue == (len(want.Holds) > 0) || len(want.Lacks) > 0 || (hasValue && !isStringList(value)) {
			fail("labels are a list of labels, or labels the list holds, and not both")
		}
	case FieldAssignee:
		if s, ok := value.(string); !hasValue || textual || (value != nil && (!ok || refs[s] != RefPerson)) {
			fail("an assignee is a person of the seed, or null")
		}
	case FieldParent:
		s, ok := value.(string)
		switch {
		case want.ValueNew != nil:
			if hasValue || textual || !slices.ContainsFunc(task.Grade.EndState, func(w Want) bool {
				return w.Field == FieldCreate && w.New != nil && sameNewIssue(*w.New, *want.ValueNew)
			}) {
				fail("a new parent is an issue the end state creates")
			}
		case !hasValue || textual || (value != nil && (!ok || refs[s] != RefIssue || s == want.Item)):
			fail("a parent is another issue of the seed, an issue the task creates, or null")
		}
	case FieldBlockedBy:
		if !hasValue || textual || !isStringList(value) {
			fail("blocked_by is a list of issue keys")
		} else {
			for _, key := range value.([]any) {
				if refs[key.(string)] != RefIssue || key == want.Item {
					fail("blocked_by names %v, which is no other issue of the seed", key)
				}
			}
		}
	case FieldRoute:
		if s, ok := value.(string); !hasValue || textual || (value != nil && (!ok || refs[s] != RefSession)) {
			fail("a route is a session of the seed, or null")
		}
	case FieldClaim:
		if !hasValue || value != nil || textual {
			fail("a claim is released, which is null")
		}
	case FieldSpec, FieldBody, FieldSuggestion, FieldComment:
		if hasValue || !textual {
			fail("a text is graded by the words it holds or lacks")
		}
	case FieldMessage:
		if hasValue || len(want.Holds) == 0 {
			fail("a message is graded by the words it holds")
		}
		if (kind == RefIssue) != (refs[want.To] == RefSession) || (kind == RefSession && want.To != "") {
			fail("a message on an issue goes to a session of the seed, and a message to a session names it as the item")
		}
	case FieldAnswer:
		ask := findAsk(seed, want.Item)
		if s, ok := value.(string); ask == nil || (hasValue && (!ok || !slices.Contains(ask.Options, s))) || (!hasValue && !textual) {
			fail("an answer is one of the ask's options, or words its text holds")
		}
	}
	if want.InOrder && len(want.Holds) < 2 {
		fail("in_order needs two words or more")
	}
	return problems
}

// newIssueProblems is what is wrong with a new issue: a project or a parent the seed does not
// hold, or no words to find its title by.
func newIssueProblems(refs map[string]RefKind, issue NewIssue) []string {
	var problems []string
	if refs[issue.Project] != RefProject {
		problems = append(problems, fmt.Sprintf("the new issue is in %q, which is no project of the seed", issue.Project))
	}
	if issue.Parent != "" && refs[issue.Parent] != RefIssue {
		problems = append(problems, fmt.Sprintf("the new issue's parent %q is no issue of the seed", issue.Parent))
	}
	if len(issue.TitleHolds) == 0 {
		problems = append(problems, "the new issue names no words its title holds")
	}
	return problems
}

// allowProblems is what is wrong with an allowed change: an item the seed does not hold, or a
// field its item does not have.
func allowProblems(refs map[string]RefKind, allow Allow) []string {
	switch {
	case allow.New != "" && allow.Item == "":
		if refs[allow.New] != RefProject || allow.Field != FieldCreate || allow.To != "" {
			return []string{"a new issue is allowed as a create in a project of the seed"}
		}
		return nil
	case allow.New == "" && allow.Item != "":
		kind, ok := refs[allow.Item]
		if !ok {
			return []string{"the seed does not hold the item"}
		}
		if !slices.Contains(wantFields[kind], allow.Field) {
			return []string{fmt.Sprintf("a %s has no field %s", kind, allow.Field)}
		}
		if (allow.Field == FieldMessage && kind == RefIssue) != (refs[allow.To] == RefSession) {
			return []string{"a message on an issue goes to a session of the seed, and nothing else goes to one"}
		}
		return nil
	}
	return []string{"an allowed change names a seeded item or a project to file in, and not both"}
}

// gradeProblems is what is wrong with the grade of a task for its kind.
func gradeProblems(seed *Seed, task Task) []string {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	grade := task.Grade
	if !task.Kind.Changes() {
		if grade.Approve != nil || len(grade.EndState) > 0 || len(grade.Allowed) > 0 {
			fail("a task of the kind %s asks for no change, so it has no approve, no end state and nothing allowed", task.Kind)
		}
		if len(grade.Facts) == 0 {
			fail("a task of the kind %s has facts for the judge", task.Kind)
		}
		if (task.Kind == KindChart) != (grade.Chart != nil) {
			fail("a chart task, and only a chart task, has a chart")
		}
		if len(grade.Replies) > 0 && task.Kind != KindStatus {
			fail("only a status task relies on the replies of sessions")
		}
		for _, session := range grade.Replies {
			if !factsQuote(seed, session, grade.Facts) {
				fail("no fact quotes the reply of %s that the task relies on", session)
			}
		}
		// A status task answers from the records (R13's "status from records"): what a fact of
		// one quotes, a record of the seed holds.
		if task.Kind == KindStatus {
			for _, fact := range grade.Facts {
				for _, match := range quoted.FindAllStringSubmatch(fact, -1) {
					if len(match[1]) >= 10 && !recorded(seed, match[1]) {
						fail("a fact quotes %q, which no record of the seed holds", match[1])
					}
				}
			}
		}
		return problems
	}
	if len(grade.Facts) > 0 || len(grade.Links) > 0 || grade.Chart != nil || len(grade.Replies) > 0 {
		fail("a task of the kind %s is graded by its end state, with no facts, links, chart or replies", task.Kind)
	}
	if grade.Approve == nil {
		fail("a task of the kind %s says whether the grader approves its plan", task.Kind)
		return problems
	}
	if len(grade.EndState) == 0 {
		fail("a task of the kind %s has an end state", task.Kind)
		return problems
	}
	if task.Kind == KindNewIssue && !slices.ContainsFunc(grade.EndState, func(w Want) bool { return w.Field == FieldCreate }) {
		fail("a new-issue task files an issue")
	}
	if !*grade.Approve || len(grade.Allowed) == 0 {
		if len(grade.Allowed) > 0 {
			fail("a task whose plan the grader rejects allows no change")
		}
		for _, want := range grade.EndState {
			current, ok := seedValue(seed, want.Item, want.Field)
			if !ok || len(want.Value) == 0 || canonical(want.Value) != current {
				fail("a task that allows no change holds the seed's value of each row, and %s of %s is not it", want.Field, wantItem(want))
			}
		}
		return problems
	}
	changed := false
	for _, want := range grade.EndState {
		if want.New != nil {
			if !slices.ContainsFunc(grade.EndState, func(w Want) bool {
				return w.Field == FieldCreate && w.New != nil && sameNewIssue(*w.New, *want.New)
			}) {
				fail("the row %s of a new issue in %s has no create row", want.Field, want.New.Project)
			}
			if !slices.ContainsFunc(grade.Allowed, func(a Allow) bool { return a.New == want.New.Project && a.Field == FieldCreate }) {
				fail("the task files an issue in %s, which it does not allow", want.New.Project)
			}
			changed = true
			continue
		}
		if !slices.ContainsFunc(grade.Allowed, func(a Allow) bool {
			return a.Item == want.Item && a.Field == want.Field && a.To == want.To
		}) {
			fail("the end state changes %s of %s, which the task does not allow", want.Field, wantItem(want))
		}
		current, ok := seedValue(seed, want.Item, want.Field)
		if !ok || want.ValueNew != nil || canonical(want.Value) != current {
			changed = true
		}
	}
	if !changed {
		fail("the end state is the seed's, so the task asks for no change")
	}
	return problems
}

// seedValue is the seed's value of an issue's field as canonical JSON, and whether the item is an
// issue and the field one of its values.
func seedValue(seed *Seed, item string, field Field) (string, bool) {
	issue, ok := seed.Issue(item)
	if !ok || !slices.Contains(issueFields, field) {
		return "", false
	}
	orNull := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	var value any
	switch field {
	case FieldStatus:
		value = issue.Status
	case FieldTitle:
		value = issue.Title
	case FieldPriority:
		value = issue.Priority
	case FieldLabels:
		value = issue.Labels
	case FieldAssignee:
		value = orNull(issue.Assignee)
	case FieldParent:
		value = orNull(issue.Parent)
	case FieldBlockedBy:
		value = seed.BlockedBy(issue.Key)
	case FieldRoute:
		value = orNull(issue.Route)
	case FieldClaim:
		if issue.Claim != nil {
			value = issue.Claim.Session
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	return canonical(data), true
}

// findAsk is the seeded ask of the reference, or nil.
func findAsk(seed *Seed, ref string) *Ask {
	for i, ask := range seed.Asks {
		if ask.Owner+"/ask/"+ask.ID == ref {
			return &seed.Asks[i]
		}
	}
	return nil
}

// quoted is each span of a fact between double quotes.
var quoted = regexp.MustCompile(`"([^"]+)"`)

// factsQuote reports whether a fact quotes, between double quotes, ten characters or more of the
// status reply of the session.
func factsQuote(seed *Seed, session string, facts []string) bool {
	for _, s := range seed.Sessions {
		if s.ID != session || s.StatusReply == "" {
			continue
		}
		for _, fact := range facts {
			for _, match := range quoted.FindAllStringSubmatch(fact, -1) {
				if len(match[1]) >= 10 && strings.Contains(s.StatusReply, match[1]) {
					return true
				}
			}
		}
	}
	return false
}

// recorded reports whether a record the seed writes into Dispatch holds text: a comment, a
// message, an issue's title or spec, a document or a file, or an ask's question or answer. A
// session's status reply is no record: Dispatch stores none.
func recorded(seed *Seed, text string) bool {
	var records []string
	for _, c := range seed.Comments {
		records = append(records, c.Body)
	}
	for _, m := range seed.Messages {
		records = append(records, m.Body)
	}
	for _, issue := range seed.Issues {
		records = append(records, issue.Title, issue.Spec)
	}
	for _, a := range seed.Artifacts {
		records = append(records, a.Body)
	}
	for _, a := range seed.Asks {
		records = append(records, a.Question)
		if a.Answer != nil {
			records = append(records, a.Answer.Text)
		}
	}
	return slices.ContainsFunc(records, func(record string) bool { return strings.Contains(record, text) })
}

// seedText is the text of a seeded item's field that an injection can be planted in.
func seedText(seed *Seed, ref string, field Field) (string, bool) {
	if issue, ok := seed.Issue(ref); ok && field == FieldTitle {
		return issue.Title, true
	}
	if field != FieldBody {
		return "", false
	}
	if key, ok := strings.CutSuffix(ref, "/spec"); ok {
		issue, ok := seed.Issue(key)
		return issue.Spec, ok && issue.Spec != ""
	}
	for _, a := range seed.Artifacts {
		if a.Owner+"/artifact/"+a.Slug == ref {
			return a.Body, true
		}
	}
	for _, c := range seed.Comments {
		if c.Owner+"/comment/"+c.ID == ref {
			return c.Body, true
		}
	}
	for _, m := range seed.Messages {
		if m.Issue+"/message/"+m.ID == ref {
			return m.Body, true
		}
	}
	return "", false
}

// chartRows is the rows the chain of a chart gives on the seed, sorted by label.
func chartRows(seed *Seed, chart Chart) ([]ChartRow, error) {
	where := chart.Where
	counts := map[string]int{}
	switch chart.From {
	case FromIssues:
		if len(where.State) > 0 {
			return nil, fmt.Errorf("an issue has no ask state to keep it by")
		}
		for _, issue := range seed.Issues {
			if !issueKept(seed, issue, where) {
				continue
			}
			labels, err := issueGroups(issue, chart.GroupBy)
			if err != nil {
				return nil, err
			}
			for _, label := range labels {
				counts[label]++
			}
		}
	case FromAsks:
		if len(where.Status) > 0 || where.Open || len(where.Labels) > 0 || len(where.Priorities) > 0 ||
			where.Parent != "" || where.HasParent || where.ClosedSince != nil || where.CreatedSince != nil {
			return nil, fmt.Errorf("an ask is kept by its project and its state alone")
		}
		for _, ask := range seed.Asks {
			project := projectOf(ask.Owner)
			if (where.Project != "" && project != where.Project) || (len(where.State) > 0 && !slices.Contains(where.State, ask.State)) {
				continue
			}
			switch chart.GroupBy {
			case GroupState:
				counts[ask.State]++
			case GroupProject:
				counts[project]++
			case GroupIssue:
				counts[ask.Owner]++
			default:
				return nil, fmt.Errorf("asks are not grouped by %s", chart.GroupBy)
			}
		}
	}
	rows := make([]ChartRow, 0, len(counts))
	for label, count := range counts {
		rows = append(rows, ChartRow{Label: label, Count: count})
	}
	slices.SortFunc(rows, func(a, b ChartRow) int { return strings.Compare(a.Label, b.Label) })
	return rows, nil
}

// issueKept reports whether an issue passes a chart's filter.
func issueKept(seed *Seed, issue Issue, where Where) bool {
	project, _, _ := strings.Cut(issue.Key, "-")
	switch {
	case where.Project != "" && project != where.Project,
		len(where.Status) > 0 && !slices.Contains(where.Status, issue.Status),
		where.Open && issue.Status == "done",
		where.Parent != "" && issue.Parent != where.Parent,
		where.HasParent && issue.Parent == "",
		where.ClosedSince != nil && (issue.ClosedAt == nil || issue.ClosedAt.Before(*where.ClosedSince)),
		where.CreatedSince != nil && issue.CreatedAt.Before(*where.CreatedSince):
		return false
	}
	for _, label := range where.Labels {
		if !slices.Contains(issue.Labels, label) {
			return false
		}
	}
	if len(where.Priorities) > 0 && (issue.Priority == nil || !slices.Contains(where.Priorities, *issue.Priority)) {
		return false
	}
	return true
}

// issueGroups is the labels an issue counts under in a chart grouped by group.
func issueGroups(issue Issue, group GroupBy) ([]string, error) {
	orNone := func(s string) []string {
		if s == "" {
			return []string{"none"}
		}
		return []string{s}
	}
	week := func(at time.Time) string {
		year, week := at.UTC().ISOWeek()
		return fmt.Sprintf("%d-W%02d", year, week)
	}
	switch group {
	case GroupStatus:
		return []string{issue.Status}, nil
	case GroupAssignee:
		return orNone(issue.Assignee), nil
	case GroupPriority:
		if issue.Priority == nil {
			return []string{"none"}, nil
		}
		return []string{fmt.Sprintf("P%d", *issue.Priority)}, nil
	case GroupLabel:
		return issue.Labels, nil
	case GroupProject:
		project, _, _ := strings.Cut(issue.Key, "-")
		return []string{project}, nil
	case GroupClosedWeek:
		if issue.ClosedAt == nil {
			return nil, fmt.Errorf("%s is open, so it has no week it closed in; keep only closed issues", issue.Key)
		}
		return []string{week(*issue.ClosedAt)}, nil
	case GroupCreatedWeek:
		return []string{week(issue.CreatedAt)}, nil
	case GroupClaim:
		if issue.Claim == nil {
			return []string{"none"}, nil
		}
		return []string{issue.Claim.Session}, nil
	case GroupParent:
		return orNone(issue.Parent), nil
	}
	return nil, fmt.Errorf("issues are not grouped by %s", group)
}

// projectOf is the project of an issue's key or of a reference.
func projectOf(ref string) string {
	first, _, _ := strings.Cut(ref, "/")
	project, _, _ := strings.Cut(first, "-")
	return project
}

// sameRows reports whether two lists of rows hold the same labels with the same counts.
func sameRows(a, b []ChartRow) bool {
	sorted := func(rows []ChartRow) []ChartRow {
		out := slices.Clone(rows)
		slices.SortFunc(out, func(x, y ChartRow) int { return strings.Compare(x.Label, y.Label) })
		return out
	}
	return slices.Equal(sorted(a), sorted(b))
}

// rowsText is rows written as label=count, comma-separated.
func rowsText(rows []ChartRow) string {
	parts := make([]string, len(rows))
	for i, row := range rows {
		parts[i] = fmt.Sprintf("%s=%d", row.Label, row.Count)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// allowItem is the item an allowed change names, as a reader would.
func allowItem(allow Allow) string {
	if allow.New != "" {
		return "a new issue in " + allow.New
	}
	return allow.Item
}
