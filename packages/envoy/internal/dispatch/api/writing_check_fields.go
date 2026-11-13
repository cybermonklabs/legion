package api

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/wire"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
)

// writing_check_fields.go is the writing check's field registry (INFRA-3231, R1): for every
// request type a route of routeShapes takes, every string member by its JSON name, marked prose
// or not prose with the reason it is not. A member is a leaf when it holds text itself - a
// string, a pointer to one, a list or a map of strings, a json.RawMessage or an untyped value -
// and every leaf needs a mark; a member that holds an object is walked into, and its own type
// carries the marks of its members. writingCheck reads a decoded request through this registry
// by reflection, so a route that calls it reads every prose member its request carries, nested
// ones included. TestEveryRequestStringFieldIsMarkedProseOrNot holds the registry to every
// request type, and TestEveryRouteThatTakesProseReachesTheWritingCheck holds every route whose
// request carries a prose member to the check.

// proseMark is what the registry says of one member.
type proseMark struct {
	// prose is a member whose text a person reads as sentences: the check reads it.
	prose bool
	// why is why a member is not prose; empty on a prose member.
	why string
	// raw is the Go type a json.RawMessage member holds when it holds an object, walked as a
	// member of the request is; nil for a raw member read as one leaf.
	raw reflect.Type
}

// The marks the registry gives, by what they say.
var (
	markProse      = proseMark{prose: true}
	markIdentifier = proseMark{why: "an identifier: a key, an id, a slug, a session, a reference or a block id"}
	markEnum       = proseMark{why: "one of a fixed set of values the server names"}
	markName       = proseMark{why: "a name or a label of a few words, not a sentence"}
	markPerson     = proseMark{why: "names a person or a session"}
	markTime       = proseMark{why: "a time, a duration or a precondition the server compares"}
	markQuote      = proseMark{why: "quotes text a document already holds, to find it; the write adds none of it"}
	markSecret     = proseMark{why: "a code or a digest the server compares, never shown as text"}
	markQuery      = proseMark{why: "a question or a draft the server reads to search and stores nothing of"}
	markRoundTrip  = proseMark{why: "a draft the server parses and renders back, storing nothing of it"}
	markConfig     = proseMark{why: "a repository, a branch, a path or a job name the server matches"}
	markHandoff    = proseMark{why: "a handoff body is one JSON object a daemon reads, never shown as Markdown"}
	markOverride   = proseMark{why: "the writing check's own override value: agent or generated"}
	markAttributes = proseMark{why: "a typed block's attributes: ids, states and values the block schema names"}
	markRawPayload = proseMark{why: "a value the server reads as a number, an id, a time or null"}
)

// rawAs is the mark of a json.RawMessage member that holds an object of type T, whose own members
// the registry marks.
func rawAs[T any]() proseMark {
	return proseMark{raw: reflect.TypeFor[T](), why: "an object the registry reads through its own members"}
}

// commentMention and commentSuggestion are commentInput's two unnamed member types, named here so
// the registry can mark their members.
var (
	commentMention    = reflect.TypeOf(commentInput{}.Mentions).Elem()
	commentSuggestion = reflect.TypeOf(commentInput{}.Suggestion).Elem()
)

// proseTypesNotRead are member types the check never reads, with why: a member of one of them is
// not walked.
var proseTypesNotRead = map[reflect.Type]string{
	reflect.TypeFor[model.Actor](): "the writer's identity, which the server resolves",
}

// proseRegistry is the registry: for each struct type a request reaches, each member's mark by
// its JSON name. A struct embedded with no JSON name of its own carries its members into the
// type that embeds it, as encoding/json reads them, and is marked as a type of its own.
var proseRegistry = map[reflect.Type]map[string]proseMark{
	reflect.TypeFor[writingCheckOverride](): {"override_writing_check": markOverride},

	// Issues.
	reflect.TypeFor[createIssueInput](): {
		"title": markProse, "spec": markProse, "components": rawAs[issueComponentsInput](),
		"project": markIdentifier, "parent": markIdentifier, "blocked_by": markIdentifier,
		"external": markIdentifier, "labels": markName, "priority": markRawPayload,
		"assignee": markPerson, "repo": markConfig,
	},
	reflect.TypeFor[updateIssueInput](): {
		"title": markProse, "components": rawAs[issueComponentsInput](),
		"status": markEnum, "labels": markName, "labels_add": markName, "priority": markRawPayload,
		"route": markPerson, "assignee": markPerson, "parent": markIdentifier,
		"blocked_by": markIdentifier, "repo": markConfig,
	},
	reflect.TypeFor[issueComponentsInput]():  {"reason": markProse, "mode": markEnum, "ids": markIdentifier},
	reflect.TypeFor[rankInput]():             {"before": markIdentifier, "after": markIdentifier},
	reflect.TypeFor[model.ExternalLink]():    {"url": markIdentifier, "kind": markEnum},
	reflect.TypeFor[claimIssueInput]():       {},
	reflect.TypeFor[actorInput]():            {},
	reflect.TypeFor[issueDesignGatesInput](): {},
	reflect.TypeFor[issueDesignGate]():       {"step": markEnum, "artifact_id": markIdentifier},
	reflect.TypeFor[createHandoffInput]():    {"body": markHandoff},
	reflect.TypeFor[checkResultInput](): {
		"run": markProse, "observed": markProse, "negative_control": markProse,
		"check": markIdentifier, "role": markEnum, "surface": markEnum, "commit": markIdentifier,
		"verdict": markEnum, "screenshots": markIdentifier,
	},
	reflect.TypeFor[checkResultRetractionInput](): {"reason": markProse},

	// Documents.
	reflect.TypeFor[jsonArtifactUpload](): {
		"content": markProse, "summary": markProse, "name": markName, "primary": markRawPayload,
	},
	reflect.TypeFor[createVersionInput]():     {"summary": markProse},
	reflect.TypeFor[rebuildArtifactInput]():   {"markdown": markProse},
	reflect.TypeFor[editArtifactInput]():      {"summary": markProse},
	reflect.TypeFor[renameArtifactInput]():    {"name": markName},
	reflect.TypeFor[markdownRoundTripInput](): {"markdown": markRoundTrip},
	reflect.TypeFor[model.EditOp](): {
		"with": markProse, "markdown": markProse,
		"op": markEnum, "find": markQuote, "after": markIdentifier, "before": markIdentifier,
		"block": markIdentifier, "index": markRawPayload, "type": markEnum, "attributes": markAttributes,
	},
	reflect.TypeFor[model.EditPrecondition]():      {"document": markSecret},
	reflect.TypeFor[model.EditBlockPrecondition](): {"id": markIdentifier, "token": markSecret},
	reflect.TypeFor[createArtifactReviewInput]():   {"reason": markProse, "state": markEnum},
	reflect.TypeFor[approvalRequestInput]():        {"summary": markProse},
	reflect.TypeFor[createPlanReviewInput]():       {},
	reflect.TypeFor[keepDecisionInput]():           {"text_sha256": markSecret},
	reflect.TypeFor[documentReadInput]():           {},

	// Asks.
	reflect.TypeFor[createAskInput](): {
		"question": markProse, "kind": markEnum, "urgency": markEnum,
	},
	reflect.TypeFor[editAskInput]():      {"question": markProse, "urgency": markEnum},
	reflect.TypeFor[model.AskOption]():   {"label": markProse, "description": markProse, "id": markIdentifier},
	reflect.TypeFor[model.AnchorInput](): {"artifact": markIdentifier, "quote": markQuote, "mark_id": markIdentifier},
	reflect.TypeFor[answerAskInput](): {
		"text": markProse, "selected": markIdentifier,
		"expected_edited_at": markTime, "expected_answer_at": markTime,
	},
	reflect.TypeFor[resolveAskInput](): {"reason": markProse, "kind": markEnum},
	reflect.TypeFor[declineAskInput](): {"note": markProse},
	reflect.TypeFor[askAskedOfInput](): {"asked_of": markPerson},
	reflect.TypeFor[askSnoozeInput]():  {"snoozed_until": markTime},

	// Comments.
	reflect.TypeFor[commentInput](): {
		"body": markProse, "reply_to": markIdentifier, "reply_to_block": markIdentifier,
		"ask_id": markIdentifier, "delivery": markEnum, "turn": markEnum,
	},
	commentMention:    {"target": markPerson},
	commentSuggestion: {"replace_with": markProse},
	reflect.TypeFor[commentReplyInput](): {
		"body": markProse, "error": markProse, "target": markPerson, "reply_to_block": markIdentifier,
	},
	reflect.TypeFor[editCommentInput]():           {"body": markProse},
	reflect.TypeFor[itemblocks.Carried]():         {"id": markIdentifier},
	reflect.TypeFor[createCommentDeliveryInput](): {"target": markPerson, "delivery": markEnum},

	// Messages.
	reflect.TypeFor[createMessageInput](): {
		"body": markProse, "in_reply_to": markIdentifier, "in_reply_to_block": markIdentifier,
		"target": markPerson, "delivery": markEnum, "urgency": markEnum,
	},
	reflect.TypeFor[createAgentMessageInput](): {
		"body": markProse, "delivery": markEnum, "in_reply_to": markIdentifier, "in_reply_to_block": markIdentifier,
	},
	reflect.TypeFor[messageReplyInput](): {
		"body": markProse, "error": markProse, "in_reply_to_block": markIdentifier,
	},
	reflect.TypeFor[createBroadcastInput](): {"body": markProse, "delivery": markEnum, "session_ids": markPerson},
	reflect.TypeFor[createDeliveryInput]():  {"delivery": markEnum},

	// Reads and the person's own state, which write no prose.
	reflect.TypeFor[relatedRequest](): {
		"title": markQuery, "text": markQuery, "exclude_issue": markIdentifier, "exclude_issues": markIdentifier,
	},
	reflect.TypeFor[createGoingRequestInput](): {"words": markQuery, "project": markIdentifier},
	reflect.TypeFor[threadReadInput]():         {"read_through": markTime},
	reflect.TypeFor[inboxThreadFilters](): {
		"kind": markEnum, "priority": markEnum, "urgency": markEnum, "project": markIdentifier,
		"issue": markIdentifier, "artifact": markIdentifier,
	},
	reflect.TypeFor[inboxReadRequest]():    {"read_through": markTime},
	reflect.TypeFor[inboxDismissRequest](): {"threads": markIdentifier, "seen_replies": markTime, "dismiss_through": markTime},
	reflect.TypeFor[userIssueStateInput](): {"dismissed": markIdentifier},
	reflect.TypeFor[userAgentStateInput](): {
		"cleared_before": markTime, "read_through": markTime, "read_replies": markIdentifier,
	},

	// Settings, projects and credentials.
	reflect.TypeFor[createProjectInput]():          {"key": markIdentifier, "name": markName, "default_assignee": markPerson},
	reflect.TypeFor[putRepoProjectInput]():         {"project": markIdentifier, "renamed_to": markConfig},
	reflect.TypeFor[putArchitectureSourceInput]():  {"branch": markConfig},
	reflect.TypeFor[projectDefaultAssigneeInput](): {"assignee": markPerson},
	reflect.TypeFor[projectGatedStepsInput]():      {"steps": markEnum},
	reflect.TypeFor[deliverySettingsInput](): {
		"deploy_repo": markConfig, "deploy_workflow_path": markConfig, "production_job_names": markConfig,
		"production_apply_step_name": markConfig, "pr_checks_workflow_path": markConfig,
		"population_authors": markPerson, "excluded_repos": markConfig, "staging_e2e_job_name": markConfig,
		"staging_apply_job_name": markConfig, "pr_checks_rollup_job_name": markConfig,
	},
	reflect.TypeFor[newIssueSettingsInput]():           {"label": markName, "session": markPerson},
	reflect.TypeFor[createDeliverySessionGroupInput](): {"name": markName, "sessions": markPerson},
	reflect.TypeFor[updateDeliverySessionGroupInput](): {"name": markName, "add": markPerson, "remove": markPerson},
	reflect.TypeFor[createAgentTokenInput]():           {"name": markName, "scopes": markEnum},
	reflect.TypeFor[updateAgentTokenScopesInput]():     {"scopes": markEnum},
	reflect.TypeFor[credentialDecisionInput]():         {"code": markSecret},
	reflect.TypeFor[wire.MachineLookupRequest]():       {"code": markSecret},

	// Test hooks.
	reflect.TypeFor[holdPersistenceInput](): {"duration": markTime},
	reflect.TypeFor[slowWriteInput]():       {"method": markEnum, "path": markConfig},
}

// artifactUploadFormValues marks the form values a multipart artifact upload reads
// (multipartArtifactUpload) as the registry marks a JSON upload's members: the summary is prose on
// a file upload too, and a Markdown file is read as an inline document's content is.
var artifactUploadFormValues = map[string]proseMark{
	"content":                markProse,
	"summary":                markProse,
	"actor":                  {why: "the writer's identity, which the server resolves"},
	"name":                   markName,
	"file":                   {why: "a file's bytes: a Markdown file is read as content, any other file is not read"},
	"primary":                markRawPayload,
	"override_writing_check": markOverride,
}

// writingCheckOverride is the member every request type carrying prose declares, embedded so one
// declaration serves them all: "" checks the write, "agent" sends it through as an agent's own
// judgement, and "generated" as text a plugin generated. Any other value is 400.
type writingCheckOverride struct {
	OverrideWritingCheck string `json:"override_writing_check,omitempty"`
}

// writingCheckOverrideValue answers the override a request carries; a request type that embeds
// writingCheckOverride carries this method too.
func (o writingCheckOverride) writingCheckOverrideValue() string { return o.OverrideWritingCheck }

// proseMember is one member of a request type as the registry walk meets it.
type proseMember struct {
	// path is the member's JSON path, "[]" standing for every element of a list and "{}" for
	// every value of a map.
	path string
	// owner is the struct type the member belongs to, and name its JSON name there.
	owner reflect.Type
	name  string
	// leaf is a member holding text itself, which needs a mark.
	leaf bool
	mark proseMark
	// marked is whether the registry marks the member.
	marked bool
}

// walkProseMembers calls visit for every member of request (a Go type), every nested one
// included, as encoding/json reads them: exported fields by JSON name, an embedded struct's
// members as members of the type that embeds it, a pointer read through.
func walkProseMembers(request reflect.Type, visit func(proseMember)) {
	walkProseType(request, "", map[reflect.Type]bool{}, visit)
}

func walkProseType(t reflect.Type, path string, seen map[reflect.Type]bool, visit func(proseMember)) {
	t = derefType(t)
	if t == nil || t.Kind() != reflect.Struct || seen[t] {
		return
	}
	if _, skipped := proseTypesNotRead[t]; skipped {
		return
	}
	seen[t] = true
	defer delete(seen, t)
	for index := range t.NumField() {
		field := t.Field(index)
		name, ok := jsonMemberName(field)
		if !ok {
			continue
		}
		if name == "" {
			walkProseType(field.Type, path, seen, visit)
			continue
		}
		memberPath := joinProsePath(path, name)
		mark, marked := proseRegistry[t][name]
		member := proseMember{path: memberPath, owner: t, name: name, mark: mark, marked: marked}
		inner, suffix := containerElem(field.Type)
		switch {
		case isTextLeaf(inner):
			member.leaf = true
			visit(member)
			if marked && mark.raw != nil {
				walkProseType(mark.raw, memberPath+suffix, seen, visit)
			}
		case derefType(inner).Kind() == reflect.Struct:
			if _, skipped := proseTypesNotRead[derefType(inner)]; skipped {
				continue
			}
			visit(member)
			walkProseType(inner, memberPath+suffix, seen, visit)
		}
	}
}

// jsonMemberName is field's JSON name; "" for an embedded struct that carries its members into
// the type that embeds it, and ok false for a field encoding/json neither reads nor writes.
func jsonMemberName(field reflect.StructField) (string, bool) {
	if !field.IsExported() && !field.Anonymous {
		return "", false
	}
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	if name == "-" {
		return "", false
	}
	if field.Anonymous && name == "" && derefType(field.Type).Kind() == reflect.Struct {
		return "", true
	}
	if !field.IsExported() {
		return "", false
	}
	if name == "" {
		name = field.Name
	}
	return name, true
}

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// isTextLeaf reports whether t, a member's type with its lists and maps taken off, holds text the
// check could read: a string, a json.RawMessage, or an untyped value.
func isTextLeaf(t reflect.Type) bool {
	if t == rawMessageType {
		return true
	}
	t = derefType(t)
	return t.Kind() == reflect.String || t.Kind() == reflect.Interface
}

// containerElem takes the pointers, lists and maps off t, answering what they hold and the path
// suffix they add ("[]" for a list, "{}" for a map). A json.RawMessage is a value, not a list.
func containerElem(t reflect.Type) (reflect.Type, string) {
	suffix := ""
	for {
		switch {
		case t == rawMessageType:
			return t, suffix
		case t.Kind() == reflect.Pointer:
			t = t.Elem()
		case t.Kind() == reflect.Slice || t.Kind() == reflect.Array:
			t, suffix = t.Elem(), suffix+"[]"
		case t.Kind() == reflect.Map:
			t, suffix = t.Elem(), suffix+"{}"
		default:
			return t, suffix
		}
	}
}

func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func joinProsePath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// requestCarriesProse reports whether a request of type request carries a member the registry
// marks prose.
func requestCarriesProse(request any) bool {
	if request == nil {
		return false
	}
	carries := false
	walkProseMembers(reflect.TypeOf(request), func(member proseMember) {
		if member.leaf && member.mark.prose {
			carries = true
		}
	})
	return carries
}

// proseFieldsOf reads every prose member input carries, in member order, each named by its JSON
// path with the index of each list element ("options[1].label"). An empty text is left out.
func proseFieldsOf(input any) []writingcheck.Field {
	var fields []writingcheck.Field
	if input != nil {
		readProseValue(reflect.ValueOf(input), "", &fields)
	}
	return fields
}

func readProseValue(value reflect.Value, path string, fields *[]writingcheck.Field) {
	value = derefValue(value)
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return
	}
	t := value.Type()
	if _, skipped := proseTypesNotRead[t]; skipped {
		return
	}
	for index := range t.NumField() {
		field := t.Field(index)
		name, ok := jsonMemberName(field)
		if !ok {
			continue
		}
		if name == "" {
			readProseValue(value.Field(index), path, fields)
			continue
		}
		mark := proseRegistry[t][name]
		readProseMember(value.Field(index), joinProsePath(path, name), mark, fields)
	}
}

// readProseMember reads one member's value: its text where the mark is prose, the object a raw
// member holds where the mark gives its type, and every element of a list or a map.
func readProseMember(value reflect.Value, path string, mark proseMark, fields *[]writingcheck.Field) {
	if value.Type() == rawMessageType {
		raw := value.Bytes()
		if mark.raw == nil || len(raw) == 0 {
			return
		}
		decoded := reflect.New(mark.raw)
		if err := json.Unmarshal(raw, decoded.Interface()); err != nil {
			// The handler refuses a raw member it cannot read; the check reads none of it.
			return
		}
		readProseValue(decoded, path, fields)
		return
	}
	value = derefValue(value)
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.String:
		if mark.prose && strings.TrimSpace(value.String()) != "" {
			*fields = append(*fields, writingcheck.Field{Name: path, Text: value.String()})
		}
	case reflect.Slice, reflect.Array:
		for index := range value.Len() {
			readProseMember(value.Index(index), path+"["+strconv.Itoa(index)+"]", mark, fields)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			readProseMember(iter.Value(), path+"."+iter.Key().String(), mark, fields)
		}
	case reflect.Struct:
		readProseValue(value, path, fields)
	}
}

func derefValue(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	return value
}
