package agent

// labels.go is the words the command bar shows for each step of a run: a tool's label
// (contracts.ToolLabel) filled from the call's arguments, in the present tense while the call runs
// and in the past tense once it ended, with the count of a list it answered, or one sentence that
// says why it could not; and the items the call named and answered, which the bar shows as chips.
// The words name no tool, hold no JSON, no status code and no result: the note a step saves for
// the model (toolRun) is another text, which never reaches the bar.

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The kinds of a step (contracts.ToolLabel.Kind), which the bar counts and folds steps by: a
// read, a search, a look for a tool, a chart drawn, a plan proposed, the review of a plan, and a
// write, which the bar draws no line of: a change an approved plan makes, which the bar shows only
// as its row's tick, and a question to the person, which is the run's answer.
const (
	kindRead   = "read"
	kindSearch = "search"
	kindLook   = "look"
	kindChart  = "chart"
	kindPlan   = "plan"
	kindReview = "review"
	kindWrite  = "write"
)

// labelKinds is every kind a label may be.
var labelKinds = []string{kindRead, kindSearch, kindLook, kindChart, kindPlan, kindReview, kindWrite}

// labelLimit is the most runes a filled label holds, and valueLimit the most one argument's value
// holds in it: each cut at a word, with "…".
const (
	labelLimit = 100
	valueLimit = 60
)

// The labels of the run's own tools that no spec of Dispatch states: find_tools; propose_plan,
// which names how many changes its plan holds; ask_person, whose kind draws no line, since the
// reply that calls it is the run's answer and sends no frame of the call (saveReply); the review
// of a plan; the test hooks; a tool no label names, which a model made up or call_tool names
// wrongly; and a tool of Dispatch the bar does not offer its model (going.UnofferedTools), which
// call_tool refuses by name: each of the last two a slip (slipWords), which the model mends
// itself. search and read are dispatch_search and dispatch_read, and their labels are those tools'
// own.
var (
	findToolsLabel     = contracts.ToolLabel{Kind: kindLook, Verb: "look", Present: "Looking for a way to {need}", Past: "Looked for a way to {need}"}
	proposePlanLabel   = contracts.ToolLabel{Kind: kindPlan, Verb: "propose", Present: "Proposing a plan of {changes}", Past: "Proposed a plan of {changes}"}
	askPersonLabel     = contracts.ToolLabel{Kind: kindWrite, Verb: "ask", Present: "Asking you a question", Past: "Asked you a question"}
	reviewPlanLabel    = contracts.ToolLabel{Kind: kindReview, Verb: "review", Present: "Reviewing the plan", Past: "Reviewed the plan"}
	hookLabel          = contracts.ToolLabel{Kind: kindRead, Verb: "run", Present: "Running a test hook", Past: "Ran a test hook"}
	unknownToolLabel   = contracts.ToolLabel{Kind: kindRead, Verb: "try", Present: "Trying a step Dispatch does not have", Past: "Tried a step Dispatch does not have"}
	unofferedToolLabel = contracts.ToolLabel{Kind: kindRead, Verb: "try", Present: "Trying a step the command bar does not offer", Past: "Tried a step the command bar does not offer"}
)

// goingReadLabels are the labels of the reads the server makes by going's names (going.ReadSpecs),
// which call_tool runs.
var goingReadLabels = map[string]contracts.ToolLabel{
	going.ReadRoster: {Kind: kindRead, Verb: "read", Present: "Reading the agent sessions", Past: "Read the agent sessions", Count: "session"},
	going.ReadProgress: {Kind: kindRead, Verb: "read",
		Present: "Reading the progress of {issue|{project|the page's issue}}", Past: "Read the progress of {issue|{project|the page's issue}}"},
	going.ReadArchitecture: {Kind: kindRead, Verb: "read",
		Present: "Reading the components of {project|the page's project}", Past: "Read the components of {project|the page's project}"},
	going.ReadProjectActivity: {Kind: kindRead, Verb: "read",
		Present: "Reading the activity of {project|the page's project}", Past: "Read the activity of {project|the page's project}"},
	going.ReadDelivery: {Kind: kindRead, Verb: "read", Present: "Reading the delivery {view}", Past: "Read the delivery {view}"},
}

// stepLabel is the label of a call of the run's tool named tool with input, and the arguments its
// forms are filled from: call_tool's are those of the tool it names.
func stepLabel(tool string, input json.RawMessage) (contracts.ToolLabel, map[string]any) {
	arguments := labelArguments(input)
	switch tool {
	case toolSearch:
		return namedLabel(dispatchSearch), arguments
	case toolRead:
		return namedLabel(dispatchRead), arguments
	case toolFindTools:
		return findToolsLabel, needInWords(arguments)
	case toolProposePlan:
		return proposePlanLabel, arguments
	case toolAskPerson:
		return askPersonLabel, arguments
	case toolCallTool:
		named, _ := arguments["tool"].(string)
		inner, _ := arguments["arguments"].(map[string]any)
		if inner == nil {
			inner = map[string]any{}
		}
		return namedLabel(named), inner
	case hookHang, hookPanic, hookCrash, hookSoftLimit:
		return hookLabel, arguments
	}
	return namedLabel(tool), arguments
}

// namedLabel is the label of the tool named name that call_tool runs or a plan's row changes: a
// tool of Dispatch, a read the server makes by going's name, a read outside Dispatch or
// draw_block. A tool of Dispatch the bar does not offer (going.UnofferedTools) is a step the
// command bar does not offer (unofferedToolLabel), and a name no tool has a step Dispatch does not
// have (unknownToolLabel).
func namedLabel(name string) contracts.ToolLabel {
	if _, unoffered := going.UnofferedTools[name]; unoffered {
		return unofferedToolLabel
	}
	if spec, known := dispatchTool(name); known {
		return spec.Label
	}
	if label, known := goingReadLabels[name]; known {
		return label
	}
	for _, tool := range slices.Concat(outsideReads, []contracts.DispatchTool{drawBlockTool}) {
		if tool.Name == name {
			return tool.Label
		}
	}
	return unknownToolLabel
}

// labelArguments is a call's arguments as its label reads them, none where they do not read as
// a JSON object.
func labelArguments(input json.RawMessage) map[string]any {
	arguments := map[string]any{}
	if err := json.Unmarshal(input, &arguments); err != nil || arguments == nil {
		return map[string]any{}
	}
	return arguments
}

// needInWords is find_tools' arguments as its label reads them: where a word of the need is the
// name of a tool find_tools can return - a model wrote "dispatch_issues" or "draw_block" as its
// need - the need is that tool's work in words (workWords), so the line names no tool (rule 1 of
// the bar's design): "Looked for a way to draw a table, a chart or one number". The arguments the
// call runs with are not these: the model's input is unchanged.
func needInWords(arguments map[string]any) map[string]any {
	need, _ := arguments["need"].(string)
	for _, word := range strings.FieldsFunc(need, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
		if tools := namedTools(strings.ToLower(word)); len(tools) > 0 {
			inWords := maps.Clone(arguments)
			inWords["need"] = workWords(tools[0].description)
			return inWords
		}
	}
	return arguments
}

// clauseEnds are what ends the first clause of a tool's description: its first sentence's end, a
// colon, a semicolon, a dash, or a bracket that opens an aside.
var clauseEnds = []string{". ", ": ", "; ", " - ", " — ", " ("}

// workWords is what a tool does, as a step line names it: the first clause of its description,
// with its first letter lower-cased: "list a project's issues for a roadmap or backlog pass",
// "draw a table, a chart or one number". A label cuts it on a word (labelValue).
func workWords(description string) string {
	end := len(description)
	for _, stop := range clauseEnds {
		if at := strings.Index(description, stop); at >= 0 && at < end {
			end = at
		}
	}
	clause := strings.TrimSuffix(strings.TrimSpace(description[:end]), ".")
	first, size := utf8.DecodeRuneInString(clause)
	if size == 0 {
		return ""
	}
	return string(unicode.ToLower(first)) + clause[size:]
}

// endedStep is what a tool frame says of a call that ended: its words, the count of the list it
// answered, the items it named and answered, and whether it was a read Dispatch answered 404,
// which found nothing and failed in nothing (noneFound).
type endedStep struct {
	label     string
	count     int
	items     []string
	noneFound bool
}

// noneFoundWords end the past form of a read Dispatch answered 404: "Read the plan handoff of
// DEMO-7 · none found".
const noneFoundWords = " · none found"

// ended is the end of a call of label with arguments that answered result, refused where the call
// failed: its past form filled, followed by the count of the list it answered where the label
// counts one ("· 3 hits"), of the total the answer states where that is more than the list holds
// ("· 50 of 73 sessions", listWords), or, for a call that failed, why in words (failedWords). A
// read Dispatch answered 404 - the item or record it reads does not exist - is no failure the
// person can act on: it reads as its past form with noneFoundWords, and is marked noneFound, so
// its frame carries no error. A 403, a 5xx and every other refusal stay failures.
func ended(label contracts.ToolLabel, arguments map[string]any, result string, refused bool) endedStep {
	answer, answered := parseResult(result)
	step := endedStep{items: argumentItems(arguments)}
	if refused && label.Kind == kindRead && answered && answer.Status == http.StatusNotFound {
		step.label = cutWords(fillLabel(label.Past, arguments), labelLimit-utf8.RuneCountInString(noneFoundWords)) + noneFoundWords
		step.noneFound = true
		return step
	}
	if refused {
		step.label = failedWords(label, arguments, result, answer, answered)
		return step
	}
	words := fillLabel(label.Past, arguments)
	if label.Count != "" && answered && !answer.Shortened && !answer.Cut {
		if list, total, listed := resultList(answer.Body, label.Count); listed {
			step.count = len(list)
			words += " · " + listWords(len(list), total, label.Count)
			step.items = appendItems(step.items, listKeys(list)...)
		}
	}
	step.label = cutWords(words, labelLimit)
	return step
}

// presentLabel is label's present form filled from arguments, cut to labelLimit.
func presentLabel(label contracts.ToolLabel, arguments map[string]any) string {
	return cutWords(fillLabel(label.Present, arguments), labelLimit)
}

// fillLabel is form, a label's present or past form, with each {name} the call's value of the
// argument name (labelValue), or, where the call gives it none, the words written after its "|",
// themselves filled the same way. A {name} with no such words and no value - a call that left out
// an argument its tool requires - reads as the argument's name in words.
func fillLabel(form string, arguments map[string]any) string {
	var out strings.Builder
	for len(form) > 0 {
		open := strings.IndexByte(form, '{')
		if open < 0 {
			out.WriteString(form)
			break
		}
		out.WriteString(form[:open])
		end := closingBrace(form, open)
		if end < 0 {
			out.WriteString(form[open:])
			break
		}
		name, absent, hasAbsent := strings.Cut(form[open+1:end], "|")
		switch value, given := labelValue(name, arguments); {
		case given:
			out.WriteString(value)
		case hasAbsent:
			out.WriteString(fillLabel(absent, arguments))
		default:
			out.WriteString("the " + strings.ReplaceAll(name, "_", " "))
		}
		form = form[end+1:]
	}
	return out.String()
}

// closingBrace is the index of the "}" that closes the "{" at open in form, past the one "{...}"
// its absence words may hold, or -1.
func closingBrace(form string, open int) int {
	depth := 0
	for at := open; at < len(form); at++ {
		switch form[at] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return at
			}
		}
	}
	return -1
}

// labelValue is the words a label shows for the call's argument name, and whether the call gives
// it any: ref names the item the call names (callItem); url is an address's host and path; the
// rest as valueWords writes them, text cut to valueLimit.
func labelValue(name string, arguments map[string]any) (string, bool) {
	switch name {
	case "ref":
		item := callItem(arguments)
		return item, item != ""
	case "url":
		raw, _ := arguments["url"].(string)
		address := urlWords(raw)
		return address, address != ""
	}
	value := valueWords(name, arguments[name])
	return cutWords(value, valueLimit), value != ""
}

// valueWords is the words of the value of the argument name, "" for none a label shows: text on
// one line, a number's digits, a list of texts or numbers its values, and a list of objects how
// many it holds of the thing its name counts ("3 changes").
func valueWords(name string, value any) string {
	switch value := value.(type) {
	case string:
		return strings.Join(strings.Fields(value), " ")
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case []any:
		words := make([]string, 0, len(value))
		for _, item := range value {
			switch item.(type) {
			case string, float64:
				if word := valueWords(name, item); word != "" {
					words = append(words, word)
				}
			default:
				return countWords(len(value), singular(strings.ReplaceAll(name, "_", " ")))
			}
		}
		return strings.Join(words, ", ")
	}
	return ""
}

// urlWords is an address as a label names it: its host and path, without its query or fragment.
func urlWords(raw string) string {
	raw = strings.TrimSpace(raw)
	address, err := url.Parse(raw)
	if err != nil || address.Host == "" {
		return strings.Join(strings.Fields(raw), " ")
	}
	return address.Host + strings.TrimSuffix(address.Path, "/")
}

// callItem is the item a call names, as {ref} shows it: its reference's (refWords), else that of
// its message, on its issue where it names one; its artifact, of its issue or its project; its
// issue, or its issue's events where it reads them; or the documents of its project. "" where it
// names none.
func callItem(arguments map[string]any) string {
	field := func(name string) string { return valueWords(name, arguments[name]) }
	if ref := field("ref"); ref != "" {
		return refWords(ref)
	}
	issue, project, artifact, message := field("issue"), field("project"), field("artifact"), field("message")
	switch {
	case message != "" && issue != "":
		return fmt.Sprintf("the message %s on %s", message, issue)
	case message != "":
		return "the message " + message
	case artifact != "" && issue != "":
		return fmt.Sprintf("the document %s of %s", artifact, issue)
	case artifact != "" && project != "":
		return fmt.Sprintf("the document %s of %s", artifact, project)
	case artifact != "":
		return "the document " + artifact
	case issue != "" && (arguments["events"] != nil || arguments["all_events"] == true):
		return "the events of " + issue
	case issue != "":
		return issue
	case project != "":
		return "the documents of " + project
	}
	return ""
}

// issueParts are the parts of an issue a reference names below its key that text.ParseRef does
// not read, as dispatch_read reads them: its comments, its log (the dashboard's activity) and its
// children, each in a label's words.
var issueParts = map[string]string{"comments": "the comments of ", "log": "the events of ", "activity": "the events of ", "children": "the children of "}

// documentVersion is the "@v<N>" that ends a reference to a document at version N.
var documentVersion = regexp.MustCompile(`/artifact/[^/#@]+@v([1-9][0-9]*)$`)

// refWords is the item a reference names, in a label's words: an issue by its key, an issue's
// comments, events or children, its spec, a document of an issue, a project or an agent session
// (at a version where the reference names one), a block of a document or of an item, an ask, a
// comment or a message, and a component; a dashboard URL is read on its own server. A reference
// that names none of these reads as written.
func refWords(value string) string {
	ref, part, parsed := parseRef(value)
	if !parsed {
		return value
	}
	if part != "" {
		return issueParts[part] + ref.IssueKey
	}
	switch ref.Kind {
	case "issue":
		return ref.IssueKey
	case "artifact":
		words := documentWords(ref)
		if version := documentVersion.FindStringSubmatch(value); version != nil {
			words = fmt.Sprintf("version %s of %s", version[1], words)
		}
		return words
	case "block":
		if ref.Item != "" {
			return "a block of " + itemWords(ref.Item, ref)
		}
		return "a block of " + documentWords(ref)
	case "ask", "comment", "message":
		return itemWords(ref.Kind, ref)
	case "component":
		return fmt.Sprintf("the component %s of %s", ref.ID, ref.Project)
	}
	return value
}

// parseRef reads value as a read's reference: a dispatch:// reference, or one to an issue's part
// below its key (issueParts), or a dashboard URL read against its own server.
func parseRef(value string) (text.Ref, string, bool) {
	if rest, scheme := strings.CutPrefix(value, "dispatch://"); scheme {
		if key, part, below := strings.Cut(rest, "/"); below && issueParts[part] != "" {
			ref, parsed := text.ParseRef("dispatch://"+key, "")
			return ref, part, parsed && ref.Kind == "issue"
		}
		ref, parsed := text.ParseRef(value, "")
		return ref, "", parsed
	}
	address, err := url.Parse(value)
	if err != nil || (address.Scheme != "http" && address.Scheme != "https") || address.Host == "" {
		return text.Ref{}, "", false
	}
	ref, parsed := text.ParseRef(value, address.Scheme+"://"+address.Host)
	return ref, "", parsed
}

// documentWords is the document ref names, or the document a block of one is on.
func documentWords(ref text.Ref) string {
	switch {
	case ref.IssueKey != "" && ref.ID == "spec":
		return "the spec of " + ref.IssueKey
	case ref.IssueKey != "":
		return fmt.Sprintf("the document %s of %s", ref.ID, ref.IssueKey)
	case ref.Session != "":
		return fmt.Sprintf("the document %s of the agent session %s", ref.ID, ref.Session)
	}
	return fmt.Sprintf("the document %s of %s", ref.ID, ref.Project)
}

// itemWords is the ask, comment or message (kind) ref names, on its issue or on the project
// document it is written on.
func itemWords(kind string, ref text.Ref) string {
	if ref.Document != "" {
		return fmt.Sprintf("the %s %s on the document %s of %s", kind, ref.ID, ref.Document, ref.Project)
	}
	return fmt.Sprintf("the %s %s on %s", kind, ref.ID, ref.IssueKey)
}

// issueKey is an issue's key as Dispatch writes one.
var issueKey = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*$`)

// argumentItems is the items a call's own arguments name, each once: the item its reference names
// (refItem), then its issue, then each other value, or item of a list, that is an issue's key, by
// its argument's name.
func argumentItems(arguments map[string]any) []string {
	items := []string{}
	first := map[string]int{"ref": 2, "issue": 1}
	names := slices.SortedFunc(maps.Keys(arguments), func(a, b string) int {
		if first[a] != first[b] {
			return cmp.Compare(first[b], first[a])
		}
		return strings.Compare(a, b)
	})
	for _, name := range names {
		values := []any{arguments[name]}
		if list, isList := arguments[name].([]any); isList {
			values = list
		}
		for _, value := range values {
			word, _ := value.(string)
			word = strings.TrimSpace(word)
			switch {
			case name == "ref":
				items = appendItems(items, refItem(word))
			case issueKey.MatchString(word):
				items = appendItems(items, word)
			}
		}
	}
	return items
}

// refItem is the item a reference names, as the bar's chips name items: an issue's key, also for
// a part of an issue, and the dispatch:// reference of any other item; "" for a reference that
// names none.
func refItem(value string) string {
	ref, part, parsed := parseRef(value)
	switch {
	case !parsed:
		return ""
	case part != "" || ref.Kind == "issue":
		return ref.IssueKey
	case ref.Kind == "artifact" && ref.IssueKey != "" && ref.ID == "spec":
		return "dispatch://" + ref.IssueKey + "/spec"
	case ref.Kind == "artifact" && ref.IssueKey != "":
		return "dispatch://" + ref.IssueKey + "/artifact/" + ref.ID
	case ref.Kind == "artifact" && ref.Session != "":
		return "dispatch://agent/" + ref.Session + "/artifact/" + ref.ID
	case ref.Kind == "artifact":
		return "dispatch://" + ref.Project + "/artifact/" + ref.ID
	case ref.Kind == "block":
		block, _ := text.FormatBlockRef(ref)
		return block
	case ref.Kind == "component":
		return "dispatch://" + ref.Project + "/component/" + ref.ID
	case ref.Document != "":
		return fmt.Sprintf("dispatch://%s/artifact/%s/%s/%s", ref.Project, ref.Document, ref.Kind, url.PathEscape(ref.ID))
	}
	return fmt.Sprintf("dispatch://%s/%s/%s", ref.IssueKey, ref.Kind, url.PathEscape(ref.ID))
}

// appendItems is items with each of more that is not empty and not among them already.
func appendItems(items []string, more ...string) []string {
	for _, item := range more {
		if item != "" && !slices.Contains(items, item) {
			items = append(items, item)
		}
	}
	return items
}

// resultList is the list a call's answer holds, the rows its label counts by noun (answerList),
// and how many items the answer states there are in all (answerTotal), the list's own length where
// it states none.
func resultList(body []byte, noun string) ([]any, int, bool) {
	var answer any
	if json.Unmarshal(body, &answer) != nil {
		return nil, 0, false
	}
	list, listed := answerList(answer, noun)
	total, stated := answerTotal(answer)
	if !stated {
		total = len(list)
	}
	return list, total, listed
}

// answerList is the list answer, a call's answer as its JSON decodes, holds, the rows its label
// counts by noun: the answer itself where it is a list, else the list under the noun's plural, or
// under results, rows or items, at its top or one object below it (a read's tables), else its one
// list.
func answerList(answer any, noun string) ([]any, bool) {
	if list, isList := answer.([]any); isList {
		return list, true
	}
	object, isObject := answer.(map[string]any)
	if !isObject {
		return nil, false
	}
	for _, name := range []string{plural(noun), "results", "rows", "items"} {
		if list, isList := object[name].([]any); isList {
			return list, true
		}
		for _, below := range object {
			if inner, isObject := below.(map[string]any); isObject {
				if list, isList := inner[name].([]any); isList {
					return list, true
				}
			}
		}
	}
	var only []any
	lists := 0
	for _, value := range object {
		if list, isList := value.([]any); isList {
			only, lists = list, lists+1
		}
	}
	return only, lists == 1
}

// listKeys is the issue keys of a list's rows, each once: a row's key, or the key of the issue it
// is or is on (a search hit's issue or owner).
func listKeys(list []any) []string {
	var keys []string
	for _, row := range list {
		object, isObject := row.(map[string]any)
		if !isObject {
			continue
		}
		for _, candidate := range []any{object["key"], nested(object, "issue", "key"), nested(object, "owner", "key")} {
			if key, isText := candidate.(string); isText && issueKey.MatchString(key) {
				keys = appendItems(keys, key)
				break
			}
		}
	}
	return keys
}

// nested is object's field inner of its field outer, nil where it holds none.
func nested(object map[string]any, outer, inner string) any {
	if field, isObject := object[outer].(map[string]any); isObject {
		return field[inner]
	}
	return nil
}

// plural is a count noun's plural: "hits", "passes", "priorities".
func plural(noun string) string {
	switch {
	case strings.HasSuffix(noun, "s"), strings.HasSuffix(noun, "x"), strings.HasSuffix(noun, "ch"), strings.HasSuffix(noun, "sh"):
		return noun + "es"
	case strings.HasSuffix(noun, "y") && !strings.ContainsAny(noun[max(len(noun)-2, 0):len(noun)-1], "aeiou"):
		return noun[:len(noun)-1] + "ies"
	}
	return noun + "s"
}

// singular is a plural noun's singular, plural's inverse: "changes", "passes", "priorities".
func singular(noun string) string {
	for _, ending := range []string{"sses", "ches", "shes", "xes"} {
		if strings.HasSuffix(noun, ending) {
			return strings.TrimSuffix(noun, "es")
		}
	}
	if stem, cut := strings.CutSuffix(noun, "ies"); cut {
		return stem + "y"
	}
	return strings.TrimSuffix(noun, "s")
}

// countWords is n of noun in words: "no hits", "1 hit", "3 hits".
func countWords(n int, noun string) string {
	switch n {
	case 0:
		return "no " + plural(noun)
	case 1:
		return "1 " + noun
	}
	return fmt.Sprintf("%d %s", n, plural(noun))
}

// listWords is a list of n of noun in words, out of total, the items its answer states there are
// in all: "50 of 73 sessions" where total is more than n, so a label never says more was read than
// was, and countWords of n otherwise.
func listWords(n, total int, noun string) string {
	if total > n {
		return fmt.Sprintf("%d of %s", n, countWords(total, noun))
	}
	return countWords(n, noun)
}

// cutWords is words at most limit runes long: cut at the last word that fits, with "…".
func cutWords(words string, limit int) string {
	runes := []rune(words)
	if len(runes) <= limit {
		return words
	}
	cut := string(runes[:limit-1])
	if space := strings.LastIndexByte(cut, ' '); space > 0 && runes[limit-1] != ' ' {
		cut = cut[:space]
	}
	return strings.TrimRight(cut, " ,;:·") + "…"
}

// failedWords is the one sentence a step that failed shows in place of its past form: that the
// call could not do what its present form says, in its base verb, and why (failedReason): "Could
// not read DEMO-12: you have no access".
func failedWords(label contracts.ToolLabel, arguments map[string]any, result string, answer SavedResult, answered bool) string {
	return cutWords(fmt.Sprintf("Could not %s: %s", didWords(label, arguments), failedReason(result, answer, answered)), labelLimit)
}

// didWords is what a call does, as its label's present form says it, in its base verb: "read
// DEMO-12".
func didWords(label contracts.ToolLabel, arguments map[string]any) string {
	_, object, _ := strings.Cut(fillLabel(label.Present, arguments), " ")
	return strings.TrimSpace(label.Verb + " " + object)
}

// slipWords is the words of a model's call that failed by the model's own slip, which it mends
// with its next call and its person cannot act on, and whether result says the call is one: a
// tool no findable tool is named (noSuchTool), or that the server has not (UNKNOWN_TOOL), "Tried a
// step Dispatch does not have", or "Tried a step the command bar does not offer" where label is
// that of a tool of Dispatch the bar does not offer (unofferedToolLabel); a change call_tool does
// not run, which only a plan makes, "Tried to update DEMO-7 outside a plan"; and arguments refused
// - by the tool's schema or translation (CALL_REFUSED), by the block's spec (BLOCK_REFUSED), by
// the route as a bad request (400), or by the agent's own tool, which takes other arguments -
// "Tried to read DEMO-12 the wrong way". A call refused for anything else - no access, not found,
// the run stopped, the server failing - is no slip: its failure sentence (failedWords) says why.
// Only the model's own calls are judged so: a refused row of a plan the person approved is the
// person's to know of.
func slipWords(label contracts.ToolLabel, arguments map[string]any, result string) (string, bool) {
	answer, answered := parseResult(result)
	switch {
	case answered:
		if answer.Status != http.StatusBadRequest {
			return "", false
		}
	case strings.HasPrefix(result, noSuchTool), refusedCode(result) == agentwire.CodeUnknownTool:
		if label == unofferedToolLabel {
			return unofferedToolLabel.Past, true
		}
		return unknownToolLabel.Past, true
	case strings.Contains(result, writeNotRunHere):
		return cutWords("Tried to "+didWords(label, arguments)+" outside a plan", labelLimit), true
	case strings.HasPrefix(result, callRefused):
		if code := refusedCode(result); code != agentwire.CodeCallRefused && code != agentwire.CodeBlockRefused {
			return "", false
		}
	case strings.HasPrefix(result, callCutOff), strings.HasPrefix(result, callNotMade), strings.HasPrefix(result, planUnread),
		strings.Contains(result, toolPanicked):
		return "", false
	}
	return cutWords("Tried to "+didWords(label, arguments)+" the wrong way", labelLimit), true
}

// refusedCode is the code of the server's refusal result tells the model of (serverCall: "the
// call was refused (CODE): ..."), "" where it tells none.
func refusedCode(result string) string {
	rest, refused := strings.CutPrefix(result, callRefused+" (")
	if !refused {
		return ""
	}
	code, _, _ := strings.Cut(rest, ")")
	return code
}

// failedReason is why a call failed, in words: the status Dispatch answered it with, or what
// stopped it before an answer.
func failedReason(result string, answer SavedResult, answered bool) string {
	if answered {
		switch status := answer.Status; {
		case status == http.StatusUnauthorized:
			return "your sign-in has ended"
		case status == http.StatusForbidden:
			return "you have no access"
		case status == http.StatusNotFound:
			return "it was not found"
		case status == http.StatusConflict:
			return "it changed since it was read"
		case status == http.StatusGone:
			return "it is gone"
		case status == http.StatusRequestEntityTooLarge:
			return "it is too large"
		case status == http.StatusTooManyRequests:
			return "Dispatch is busy"
		case status >= http.StatusInternalServerError:
			return "Dispatch failed to answer"
		}
		return "Dispatch refused the request"
	}
	switch {
	case strings.HasPrefix(result, callCutOff):
		return "the run stopped"
	case strings.Contains(result, writeNotRunHere):
		return "it changes something, so it waits for a plan you approve"
	case strings.HasPrefix(result, callRefused):
		return "Dispatch refused it"
	case strings.HasPrefix(result, callNotMade):
		return "the server could not make the call"
	case strings.Contains(result, toolPanicked):
		return "the tool failed"
	}
	return "the request was not valid"
}
