package api

// going_tool_rules.go holds the cross-field rule each write tool's spec states beside its
// arguments (DispatchToolSpec.validation in packages/contracts/src/dispatch-tools.ts and
// dispatch-route-tools.ts), ported to Go. The bar holds a call to its tool's rule where it holds
// the call to the tool's schema (GoingCaller.prepare): when the agent proposes a plan, when the
// person approves its rows, and when a row runs. So a row its route would refuse 400 for how its
// arguments agree - a replace with no with, a dismissal of named rows with no seen_replies - is
// refused before anyone approves it, in the words the spec refuses it with, and nothing is sent.
//
// The generator marks each tool whose spec states a rule with the rule's whole words
// (contracts.DispatchTool.Validation), which a rule whose spec words no refusal of its own answers
// as it stands. TestEveryWriteToolsRuleIsPortedOnce holds goingToolRules to those marks: a port
// for each write tool marked, and none for any other tool. A read tool's rule is not ported: no
// read of the bar is held to its tool's schema, and a read its route refuses changes nothing.
//
// The bar departs from two rules. A close needs no reason, since the bar closes an issue with none
// and posts nothing (barTool, going/agent/tools.go, tells its model so). An upload naming no owner
// is the calling session's file of its own conversation (goingTranslateAgentUpload), which the
// spec's command never makes and whose route takes a file of any name, so the upload rule holds
// only an upload naming an issue, a project or a ref.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingToolRule is one write tool's rule: the refusal of a call that breaks it, "" for a call that
// keeps it. rule is the rule's whole words as the spec states them (GoingAction.rule), the refusal
// of a rule whose spec words none of its own.
type goingToolRule func(call goingRuleCall, rule string) string

// goingRuleCall is a call's arguments as a rule reads them, which are what JSON decodes to: a
// member the call does not name is absent, where the TypeScript reads it as undefined.
type goingRuleCall map[string]any

// has reports whether the call names argument, with any value, null included.
func (c goingRuleCall) has(argument string) bool {
	_, named := c[argument]
	return named
}

// text is argument as a text, and whether it is one.
func (c goingRuleCall) text(argument string) (string, bool) {
	text, isText := c[argument].(string)
	return text, isText
}

// isText reports whether argument is a text, an empty one included.
func (c goingRuleCall) isText(argument string) bool {
	_, isText := c.text(argument)
	return isText
}

// isList reports whether argument is a list.
func (c goingRuleCall) isList(argument string) bool {
	_, isList := c[argument].([]any)
	return isList
}

// names reports whether the call names argument with a value, as the route tools' rules read one
// (`names` in dispatch-route-tools.ts): absent, null, a text of spaces alone and an empty list name
// nothing.
func (c goingRuleCall) names(argument string) bool {
	switch value := c[argument].(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(value) != ""
	case []any:
		return len(value) > 0
	}
	return true
}

// goingBrokenParts joins the words of each part of a rule a call broke, in the rule's order, so a
// refusal never names a part the call kept (`brokenParts` in dispatch-tools.ts).
func goingBrokenParts(parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(part string) bool { return part == "" }), " ")
}

// goingDocumentOwnerKept is the rule of a document's owner (`documentOwnerValidation` in
// dispatch-tools.ts): exactly one of issue and project, or a ref alone; with project, the artifact
// that names the document, where artifact is required; and artifact or ref always, where it is
// required always.
func goingDocumentOwnerKept(call goingRuleCall, requireArtifact, alwaysRequireArtifact bool) bool {
	issue, project, artifact, ref := call.isText("issue"), call.isText("project"), call.isText("artifact"), call.isText("ref")
	return (issue != project || (!issue && !project && ref)) &&
		(!project || !requireArtifact || artifact) &&
		(!alwaysRequireArtifact || artifact || ref)
}

// The words of the rules ported here, each as the spec states it.
const (
	goingOwnerRule = "Exactly one of issue and project is required; with project, artifact names the document."

	goingIssueUpdateFieldsRule = "Issue update requires at least one field besides issue: status, title, labels, labels_add, priority, assignee, external_links, route, parent, blocked_by, advances, components, repo, before, or after."
	goingIssueUpdateDoneRule   = "status done requires reason, a non-empty note saying why the issue is closing, posted on the issue before it closes so the close carries its note"
	goingIssueUpdateReasonRule = "reason goes only with status done"
	goingIssueUpdateLabelsRule = "labels_add goes without labels: labels replaces every label the issue holds and labels_add adds to them, so name the labels to add in labels_add alone, or the whole set in labels alone."

	goingCommentTurnRule          = "turn requires reply_to_ask: turn says who holds an open ask's thread after the reply. Pass reply_to_ask with the open ask's id, or drop turn."
	goingCommentMentionMax        = 10
	goingCommentMentionLimitRule  = "mentions allow at most 10 targets"
	goingCommentMentionTargetRule = "mention target must be role:<name> or session:<id>"
	goingCommentMentionRepeatRule = "mentions must not repeat a target"

	goingPreconditionRule = "A precondition selects exactly one of document or blocks."

	goingResultRetractRule = "Pass retract with the id of a result recorded before, and reason with why that result is wrong, to retract it."

	goingArtifactSourceRule = "Give at most one of path and content. A call with neither makes an empty document under the name, which a person or an agent writes in afterwards with dispatch doc-edit."
	goingDocumentIsMarkdown = "a document holds Markdown, which rewrites other text (a JSONL file's lines come back joined into one paragraph)"
	goingPathKeepsBytes     = "path (--path <file>), which stores a file whose path does not end in .md or .markdown as its bytes, unchanged"

	goingHandoffPhaseRule = "phase is not a handoff phase: " + contracts.HandoffPhaseRule
	goingHandoffBodyRule  = `body is not one JSON object: a handoff body is a JSON object such as {"schemaVersion":1}`

	goingInboxOwnerRule = "Name issue or artifact, not both."
)

// goingSourceOwner and goingSourceName are the two parts of a GitHub repository written as
// owner/name, as dispatch_architecture_sync takes it (ARCHITECTURE_SOURCE_REPO_PATTERN), where a
// name is neither . nor ..: a path segment of either resolves away.
var (
	goingSourceOwner = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	goingSourceName  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// goingDeployRepo is a repository as dispatch_deploy takes one (DEPLOY_REPO), owner/repo; and
// goingDeployTime a time as it takes one (DEPLOY_TIME), RFC 3339 with seconds, any fraction of
// them and Z or an offset, its date's year, month and day captured.
var (
	goingDeployRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	goingDeployTime = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d+)?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$`)
)

// goingIfBroken is the refusal of a rule whose spec words none of its own: rule where the call
// broke it (kept false), "" where it kept it.
func goingIfBroken(kept bool, rule string) string {
	if kept {
		return ""
	}
	return rule
}

// goingToolRules is the Go port of each write tool's rule, by the tool's name.
var goingToolRules = map[string]goingToolRule{
	"dispatch_issue_update": goingIssueUpdateProblem,
	"dispatch_ask": func(call goingRuleCall, rule string) string {
		return goingIfBroken(goingDocumentOwnerKept(call, true, false), rule)
	},
	"dispatch_edit_ask": func(call goingRuleCall, rule string) string {
		_, multiple := call["multiple"].(bool)
		return goingIfBroken(call.isText("question") || call.isList("options") || multiple || call.isText("urgency"), rule)
	},
	"dispatch_comment": func(call goingRuleCall, _ string) string {
		owner := goingIfBroken(goingDocumentOwnerKept(call, true, false), goingOwnerRule)
		turn := goingIfBroken(!call.has("turn") || call.isText("reply_to_ask"), goingCommentTurnRule)
		return goingBrokenParts(owner, turn, goingMentionProblem(call["mentions"]))
	},
	"dispatch_suggest": func(call goingRuleCall, rule string) string {
		return goingIfBroken(goingDocumentOwnerKept(call, true, true), rule)
	},
	"dispatch_message": func(call goingRuleCall, rule string) string {
		return goingIfBroken(call.isText("issue") || call.isText("in_reply_to"), rule)
	},
	"dispatch_doc_edit": func(call goingRuleCall, _ string) string {
		owner := goingIfBroken(goingDocumentOwnerKept(call, true, false), goingOwnerRule)
		return goingBrokenParts(owner, goingIfBroken(goingPreconditionKept(call), goingPreconditionRule), goingReplaceOpsProblem(call))
	},
	"dispatch_request_approval": func(call goingRuleCall, rule string) string {
		return goingIfBroken(goingDocumentOwnerKept(call, true, false), rule)
	},
	"dispatch_review": func(call goingRuleCall, rule string) string {
		reason, _ := call.text("reason")
		return goingIfBroken(call["verdict"] != "changes_requested" || strings.TrimSpace(reason) != "", rule)
	},
	"dispatch_result":   goingResultProblem,
	"dispatch_deploy":   goingDeployProblem,
	"dispatch_artifact": goingArtifactProblem,
	"dispatch_architecture_sync": func(call goingRuleCall, rule string) string {
		repo, named := call.text("repo")
		owner, name, whole := strings.Cut(repo, "/")
		return goingIfBroken(!named || (whole && goingSourceOwner.MatchString(owner) && goingSourceName.MatchString(name) && name != "." && name != ".."), rule)
	},
	"dispatch_handoff_write": func(call goingRuleCall, _ string) string {
		if phase, named := call.text("phase"); named && !handoffPhasePattern.MatchString(phase) {
			return goingHandoffPhaseRule
		}
		body, named := call.text("body")
		if !named {
			return ""
		}
		var object any
		if json.Unmarshal([]byte(body), &object) != nil {
			return goingHandoffBodyRule
		}
		_, isObject := object.(map[string]any)
		return goingIfBroken(isObject, goingHandoffBodyRule)
	},
	"dispatch_create_priority": func(call goingRuleCall, rule string) string {
		title, named := call.text("title")
		return goingIfBroken(call.names("id") || !named || itemblocks.Units16(title) <= priorityIDMax, rule)
	},
	"dispatch_edit_priority": func(call goingRuleCall, _ string) string {
		if !call.has("title") && !call.has("description") && !call.has("owner") && !call.has("rank") {
			return "name at least one of title, description, owner and rank."
		}
		if rank, named := call["rank"].(map[string]any); named && !goingRuleCall(rank).names("before") && !goingRuleCall(rank).names("after") {
			return "rank names before, after, or both."
		}
		return ""
	},
	"dispatch_mark_inbox_read": func(call goingRuleCall, rule string) string {
		return goingIfBroken(!(call.names("issue") && call.names("artifact")), rule)
	},
	"dispatch_dismiss_inbox": goingDismissInboxProblem,
	"dispatch_answer_ask": func(call goingRuleCall, rule string) string {
		return goingIfBroken(call.names("selected") || call.names("text"), rule)
	},
	"dispatch_reply_comment_delivery": func(call goingRuleCall, _ string) string {
		switch {
		case call.has("body") == call.has("error"):
			return "name exactly one of body and error."
		case call.has("body") && !call.names("body"):
			return "body is blank."
		case call.has("reply_to_block") && !call.has("body"):
			return "reply_to_block names the block a body answers, and an error has none."
		}
		return ""
	},
	"dispatch_review_document": func(call goingRuleCall, rule string) string {
		return goingIfBroken(call["state"] != "changes_requested" || call.names("reason"), rule)
	},
	"dispatch_set_agent_state": func(call goingRuleCall, rule string) string {
		return goingIfBroken(call.has("pinned") || call.has("cleared_before") || call.has("read_through") || call.names("read_replies"), rule)
	},
}

// goingStatusOptions are the options of dispatch_issue_update that say what happens to the issues
// under the issue or above it when its status changes, in the order of ISSUE_STATUS_OPTIONS
// (dispatch-tools.ts). The bar never sends one: a plan call writes each out as one update row for
// each issue the option reaches (expandStatusOptions), so no stored row carries one.
var goingStatusOptions = []string{optionIncludeChildren, optionUnlinkChildren, optionIncludeParent}

// goingStatusOptionsPatchFields are the fields of dispatch_issue_update the status change route
// does not take, so no option goes with one of them (ISSUE_UPDATE_PATCH_FIELDS).
var goingStatusOptionsPatchFields = []string{"title", "labels", "labels_add", "priority", "assignee", "external_links", "route",
	"parent", "blocked_by", "advances", "advances_revision", "components", "repo", "before", "after"}

// The refusals of the options' parts that need no list of fields, as the spec words them.
const (
	goingOptionsExclusiveRule = "include_children does not go with unlink_children. The open children move with the issue or leave it, so name one of the two."
	goingOptionsDoneRule      = "include_children does not go with status done. Only a person who is signed in at a browser can close the open issues under an issue together with it, and this call presents a token. That person does it on the issue's page in Dispatch: Close issue, then Move them with it."
)

// goingStatusOptionsOf is the options of call that it sets to true, in the order of
// goingStatusOptions (`issueStatusOptionsOf`). An option set to false asks for nothing.
func goingStatusOptionsOf(call goingRuleCall) []string {
	var set []string
	for _, option := range goingStatusOptions {
		if call[option] == true {
			set = append(set, option)
		}
	}
	return set
}

// goingStatusOptionsProblem is the refusal of the parts of the options' rule call breaks, "" where
// it keeps it or sets no option to true (`issueStatusOptionsProblem`): include_children never goes
// with unlink_children, nor with status done, which only a person signed in at a browser may do;
// every option needs a status; and an option goes with status alone, and with reason when the
// status is done.
func goingStatusOptionsProblem(call goingRuleCall) string {
	options := goingStatusOptionsOf(call)
	if len(options) == 0 {
		return ""
	}
	named := strings.Join(options, ", ")
	if comma := strings.LastIndex(named, ", "); comma >= 0 {
		named = named[:comma] + " and " + named[comma+len(", "):]
	}
	var others []string
	for _, field := range goingStatusOptionsPatchFields {
		if call.has(field) {
			others = append(others, field)
		}
	}
	return goingBrokenParts(
		goingIfBroken(!(call[optionIncludeChildren] == true && call[optionUnlinkChildren] == true), goingOptionsExclusiveRule),
		goingIfBroken(!(call[optionIncludeChildren] == true && call["status"] == "done"), goingOptionsDoneRule),
		goingIfBroken(call.isText("status"), fmt.Sprintf("status is required with %s. Add status, or drop %s.", named, named)),
		goingIfBroken(len(others) == 0, fmt.Sprintf("With %s, give status alone, and reason when the status is done. This call also names %s. "+
			"Send those fields in a call of their own.", named, strings.Join(others, ", "))),
	)
}

// goingIssueReasonProblem is the reason's part of dispatch_issue_update's rule
// (`issueReasonProblem`): reason is the close's note, so status done takes it and every other
// call goes without it. The bar's close takes no reason (barTool), so a close without one keeps the
// rule here; one whose reason is no text, or blank, would post an empty note, and breaks it as
// the spec's does.
func goingIssueReasonProblem(call goingRuleCall) string {
	if call["status"] == "done" {
		if reason, isText := call.text("reason"); call.has("reason") && (!isText || strings.TrimSpace(reason) == "") {
			return goingIssueUpdateDoneRule + "."
		}
		return ""
	}
	if !call.has("reason") {
		return ""
	}
	status := "an update without status done"
	if named, isText := call.text("status"); isText {
		status = "status " + named
	}
	return goingIssueUpdateReasonRule + ": it is the note posted on the issue as it closes, and " + status +
		" closes nothing. Drop reason, or set status done to close the issue."
}

// goingIssueUpdateProblem is dispatch_issue_update's rule (`issueUpdateProblem`): a reason goes
// with status done and with nothing else, labels and labels_add never go together, as the route
// refuses both (400 LABELS_INPUT), an option of the status change goes with status alone
// (goingStatusOptionsProblem), and the update names at least one field to change. It names every
// part of the reason's, the labels' and the options' rules the call broke, joined by one space, so
// its caller fixes them all and calls once. The field part is read only when the call keeps those
// three: a call refused for its reason is never told it names no field.
func goingIssueUpdateProblem(call goingRuleCall, _ string) string {
	broken := goingBrokenParts(
		goingIssueReasonProblem(call),
		goingIfBroken(!(call.has("labels") && call.has("labels_add")), goingIssueUpdateLabelsRule),
		goingStatusOptionsProblem(call),
	)
	if broken != "" {
		return broken
	}
	_, numbered := goingNumber(call["priority"])
	components, _ := call["components"].(map[string]any)
	changes := call.isText("status") || call.isText("title") || call.isList("labels") || call.isList("labels_add") || numbered ||
		(call.has("priority") && call["priority"] == nil) || call.isText("assignee") ||
		(call.has("assignee") && call["assignee"] == nil) || call.isList("external_links") || call.isText("route") ||
		call.isText("parent") || call.isList("blocked_by") || call.isList("advances") || components != nil ||
		call.isText("repo") || call.isText("before") || call.isText("after")
	return goingIfBroken(changes, goingIssueUpdateFieldsRule)
}

// goingMentionProblem is the mention rule a comment's mentions break, in the route's words and its
// order (`mentionProblem`, validateCommentMentions): the count first, then each target in turn, its
// form before whether an earlier one named it. "" where they keep all three, or name none.
func goingMentionProblem(mentions any) string {
	targets, listed := mentions.([]any)
	if !listed {
		return ""
	}
	if len(targets) > goingCommentMentionMax {
		return goingCommentMentionLimitRule
	}
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		text, isText := target.(string)
		if !isText {
			return goingCommentMentionTargetRule
		}
		if _, err := model.ParseRoute(text); err != nil {
			return goingCommentMentionTargetRule
		}
		if seen[text] {
			return goingCommentMentionRepeatRule
		}
		seen[text] = true
	}
	return ""
}

// goingPreconditionKept reports whether an edit's precondition, where it names one, selects exactly
// one of a document token and a set of block tokens (`preconditionKept`).
func goingPreconditionKept(call goingRuleCall) bool {
	if !call.has("precondition") {
		return true
	}
	precondition, isObject := call["precondition"].(map[string]any)
	if !isObject {
		return false
	}
	return goingRuleCall(precondition).isText("document") != goingRuleCall(precondition).isList("blocks")
}

// goingReplaceTakes is the members a replace operation takes; every other member is another
// operation's, whose text a replace never writes.
var goingReplaceTakes = map[string]bool{"op": true, "find": true, "with": true, "occurrence": true}

// goingReplaceOpsProblem is the refusal of each replace operation of an edit that carries a member
// replace does not take or names no with, each by its index in ops, "" where every replace keeps
// the rule (`replaceOpsProblem`). The members a replace carries are named in the order of their
// names, which is the order the route names them in (replaceFieldsRefusal): a decoded call keeps
// no order of its members.
func goingReplaceOpsProblem(call goingRuleCall) string {
	ops, listed := call["ops"].([]any)
	if !listed {
		return ""
	}
	var problems []string
	for index, raw := range ops {
		op, isObject := raw.(map[string]any)
		if !isObject || op["op"] != "replace" {
			continue
		}
		var foreign []string
		for member := range op {
			if !goingReplaceTakes[member] {
				foreign = append(foreign, member)
			}
		}
		if len(foreign) > 0 {
			slices.Sort(foreign)
			problems = append(problems, fmt.Sprintf("ops[%d] is a replace carrying %s, which replace does not take: it takes find, with and occurrence, and its new text goes in with.",
				index, strings.Join(foreign, " and ")))
		}
		if _, with := op["with"]; !with {
			problems = append(problems, fmt.Sprintf(`ops[%d] is a replace with no with, which would delete the text find matches: write the new text in with, or with "" to delete it on purpose.`, index))
		}
	}
	return strings.Join(problems, " ")
}

// goingResultFields is every argument a record of a result carries, the five it must carry first
// (RESULT_RECORD_FIELDS, RESULT_REQUIRED_FIELDS).
var goingResultFields = []string{"issue", "check", "role", "verdict", "commit", "surface", "run", "observed",
	"negative_control", "failed", "skipped", "excluded", "screenshots"}

// goingResultRequired is how many of goingResultFields a record must carry.
const goingResultRequired = 5

// goingResultProblem is dispatch_result's rule (`resultProblem`): a call with retract retracts a
// result, with its reason and no other argument, and a call without it records one, with the five
// arguments a record must carry.
func goingResultProblem(call goingRuleCall, _ string) string {
	carried := slices.DeleteFunc(slices.Clone(goingResultFields), func(field string) bool { return !call.has(field) })
	if call.isText("retract") {
		if reason, _ := call.text("reason"); strings.TrimSpace(reason) == "" {
			return "retract needs reason, the words that say why the result is wrong."
		}
		if len(carried) > 0 {
			return fmt.Sprintf("retract takes reason alone, and this call also carries %s. Retract the result, then record a new one with a call of its own.",
				strings.Join(carried, ", "))
		}
		return ""
	}
	if call.has("reason") {
		return "reason goes with retract, and this call carries no retract. Name the result to retract, or drop reason."
	}
	missing := slices.DeleteFunc(slices.Clone(goingResultFields[:goingResultRequired]), call.has)
	if len(missing) > 0 {
		return fmt.Sprintf("recording a result needs %s. %s", strings.Join(missing, ", "), goingResultRetractRule)
	}
	return ""
}

// goingDeployProblem is dispatch_deploy's rule (`deployProblem`), the refusal of the first part a
// call breaks: repo is owner/repo, program 1 to 64 lowercase letters, digits or hyphens
// (deployProgram), commit 40 lowercase hexadecimal characters (resultCommitSHA), and deployed_at,
// where the call names it, an RFC 3339 time on a date the calendar holds, at most five minutes
// ahead of now, as the route refuses one (createDeploy).
func goingDeployProblem(call goingRuleCall, _ string) string {
	if repo, named := call.text("repo"); !named || !goingDeployRepo.MatchString(repo) {
		return "repo must be owner/repo"
	}
	if program, named := call.text("program"); !named || !deployProgram.MatchString(program) {
		return "program must be 1 to 64 lowercase letters, digits or hyphens"
	}
	if commit, named := call.text("commit"); !named || !resultCommitSHA.MatchString(commit) {
		return "commit must be 40 lowercase hexadecimal characters"
	}
	if !call.has("deployed_at") {
		return ""
	}
	at, named := call.text("deployed_at")
	parts := goingDeployTime.FindStringSubmatch(at)
	if !named || parts == nil {
		return "deployed_at must be RFC3339"
	}
	// A month or a day no month has is no time at all, as the spec's Date.parse reads it; a day
	// past its own month's end reads there as a day of the next month, which is no calendar date.
	month, _ := strconv.Atoi(parts[2])
	day, _ := strconv.Atoi(parts[3])
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return "deployed_at must be RFC3339"
	}
	deployed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return "deployed_at must be a valid calendar date"
	}
	if deployed.After(time.Now().Add(deployAhead)) {
		return deployAheadRefusal
	}
	return ""
}

// goingArtifactProblem is dispatch_artifact's rule (`artifactProblem`): exactly one owner, at most
// one of path and content, and, for a call without path, which writes a document, a name that
// says Markdown or says nothing, since what a document holds is Markdown. A call naming no owner
// keeps it: the bar sends it to the calling session's own conversation, which holds files.
func goingArtifactProblem(call goingRuleCall, _ string) string {
	if !call.isText("issue") && !call.isText("project") && !call.isText("ref") {
		return ""
	}
	owner := goingIfBroken(goingDocumentOwnerKept(call, false, false), goingOwnerRule)
	source := goingIfBroken(!call.isText("path") || !call.isText("content"), goingArtifactSourceRule)
	if broken := goingBrokenParts(owner, source); broken != "" {
		return broken
	}
	name, named := call.text("name")
	if call.isText("path") || !named {
		return ""
	}
	extension, extended := goingNameExtension(name)
	if !extended || strings.EqualFold(extension, "md") || strings.EqualFold(extension, "markdown") {
		return ""
	}
	return fmt.Sprintf("%s, and %s is not a Markdown name. Upload the file under the same name with %s; a document takes a name ending in .md or .markdown or with no extension.",
		goingDocumentIsMarkdown, goingJSONText(name), goingPathKeepsBytes)
}

// goingNameExtension is a name's extension, and whether it has one: what follows its last dot,
// where that holds no blank (NAME_EXTENSION), the name's own blanks around it trimmed.
func goingNameExtension(name string) (string, bool) {
	name = strings.TrimSpace(name)
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 {
		return "", false
	}
	extension := name[dot+1:]
	return extension, extension != "" && !strings.ContainsFunc(extension, unicode.IsSpace)
}

// goingJSONText is text as a JSON string, as JSON.stringify writes it: no character escaped for
// HTML.
func goingJSONText(text string) string {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(text)
	return strings.TrimSuffix(encoded.String(), "\n")
}

// goingDismissInboxNarrowing is the Inbox's narrowing a dismissal names in place of its rows.
var goingDismissInboxNarrowing = []string{"kind", "priority", "urgency", "project", "issue", "artifact"}

// goingDismissInboxProblem is dispatch_dismiss_inbox's rule (`dismissInboxProblem`, as
// dismissInbox refuses each part 400): the rows named by id, or the narrowing up to
// dismiss_through, never both; seen_replies beside rows named by id when they are taken out, and
// never on a restore.
func goingDismissInboxProblem(call goingRuleCall, _ string) string {
	restore := call["restore"] == true
	seen := call["seen_replies"] != nil
	if call.names("threads") {
		if call.names("dismiss_through") || slices.ContainsFunc(goingDismissInboxNarrowing, call.names) {
			return "threads names the rows itself, so it takes neither dismiss_through nor the narrowing."
		}
		switch {
		case restore && seen:
			return "a restore takes no seen_replies."
		case !restore && !seen:
			return "taking rows named by threads out carries seen_replies, what was seen of each."
		}
		return ""
	}
	switch {
	case seen:
		return "seen_replies goes with threads alone."
	case !call.names("dismiss_through"):
		return "dismiss_through is required when threads names no row."
	}
	return goingIfBroken(!(call.names("issue") && call.names("artifact")), goingInboxOwnerRule)
}
