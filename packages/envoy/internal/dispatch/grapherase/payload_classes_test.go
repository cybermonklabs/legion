package grapherase

import (
	"encoding"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// erasedPayload is what the payload of an event type the erase reads is built from: the Go value
// it is encoded from, the function that builds that value where it is a map (whose keys the walk
// reads off the value), and each top-level key a statement adds to the stored payload after the
// append, with the statement that adds it.
type erasedPayload struct {
	value   any
	builtBy string
	added   map[string]string
}

// reviewDigestKeys are the keys the review's statement adds to the payload of artifact.approved
// and artifact.changes_requested after the append, from the row the batch inserted (docs'
// approval review): the digest of the approved bytes and where it was read from.
var reviewDigestKeys = map[string]string{
	"content_sha256":        "docs' approval review statement",
	"content_sha256_source": "docs' approval review statement",
}

// erasedEventPayloads are the event types the erase reads (readEvents), each with what its
// payload is built from: a comment's own events and its delivery receipts, which the comment's
// predicate reads, and the document events that name their document as a top-level
// `artifact_id`, which the block's predicate reads.
var erasedEventPayloads = map[string]erasedPayload{
	"comment.anchor_refreshed":   {value: model.CommentEventPayload{}},
	"comment.answered":           {value: model.CommentEventPayload{}},
	"comment.created":            {value: model.CommentEventPayload{}},
	"comment.edited":             {value: model.CommentEventPayload{}},
	"comment.reopened":           {value: model.CommentEventPayload{}},
	"comment.resolved":           {value: model.CommentEventPayload{}},
	"suggestion.accepted":        {value: model.CommentEventPayload{}},
	"suggestion.rejected":        {value: model.CommentEventPayload{}},
	"comment.delivery":           {value: model.CommentDeliveryEventPayload{}},
	"artifact.version":           {value: fullArtifactVersionPayload(), builtBy: "docs.ArtifactVersionEventPayload"},
	"artifact.human_edited":      {value: model.ArtifactHumanEditedEventPayload{}},
	"artifact.approved":          {value: model.ArtifactReviewEventPayload{}, added: reviewDigestKeys},
	"artifact.changes_requested": {value: model.ArtifactReviewEventPayload{}, added: reviewDigestKeys},
}

// Why the erase never reads an event type: the field its payload lacks. The comment's predicate
// reads a payload naming the comment as `id` or `comment_id`; the block's reads an artifact.* or
// block.* event naming the block's document as a top-level `artifact_id`.
const (
	namesNoComment     = "it names no comment as `id` or `comment_id`, and is no artifact.* or block.* type"
	namesItsAsk        = "its `id` is its ask's, and it names no comment as `comment_id`"
	namesItsAskAsAskID = "it names its ask as `ask_id`, and no comment as `id` or `comment_id`"
	namesItsMessage    = "its `id` is its message's, and it names no comment as `comment_id`"
	namesItsMessageID  = "it names its message as `message_id`, and no comment as `id` or `comment_id`"
	namesItsBlock      = "it carries no top-level `artifact_id`: it names its block as `block_id` alone"
	nestsItsDocument   = "it carries no top-level `artifact_id`: its document stands inside `artifact`"
)

// unreadEventTypes are the event types the erase never reads, each with the field its payload
// lacks.
var unreadEventTypes = map[string]string{
	"architecture.sync_failed":               namesNoComment,
	"architecture.synced":                    namesNoComment,
	"artifact.created":                       nestsItsDocument,
	"artifact.removed":                       nestsItsDocument,
	"artifact.renamed":                       nestsItsDocument,
	"ask.anchor_refreshed":                   namesItsAsk,
	"ask.answered":                           namesItsAsk,
	"ask.asked_of":                           namesItsAsk,
	"ask.edited":                             namesItsAsk,
	"ask.follower_added":                     namesItsAskAsAskID,
	"ask.follower_removed":                   namesItsAskAsAskID,
	"ask.handed_back":                        namesItsAsk,
	"ask.opened":                             namesItsAsk,
	"ask.resolved":                           namesItsAsk,
	"block.invalid":                          namesItsBlock,
	"block.repaired":                         namesItsBlock,
	"child.added":                            namesNoComment,
	"child.removed":                          namesNoComment,
	"child.status":                           namesNoComment,
	"decision.kept":                          "its `artifact_id` stands under no artifact.* or block.* type, and it names no comment as `id` or `comment_id`",
	"handoff.recorded":                       namesNoComment,
	"issue.claimed":                          namesNoComment,
	"issue.closed":                           namesNoComment,
	"issue.created":                          namesNoComment,
	"issue.deployed":                         namesNoComment,
	"issue.pull_request_attribution_changed": namesNoComment,
	"issue.released":                         namesNoComment,
	"issue.updated":                          namesNoComment,
	"message.accepted":                       namesItsMessageID,
	"message.answered":                       namesItsMessage,
	"message.created":                        namesItsMessage,
	"message.delivery":                       namesItsMessageID,
	"priority.updated":                       "its `id` is its priority's, and it names no comment as `comment_id`",
	"project.created":                        namesNoComment,
	"project.updated":                        namesNoComment,
	"result.recorded":                        namesNoComment,
	"result.retracted":                       namesNoComment,
	"settings.architecture_source.updated":   namesNoComment,
	"settings.project_default_assignee.updated": namesNoComment,
	"settings.repo_project.updated":             namesNoComment,
	"spec.approved_written":                     namesNoComment,
	"spec.outcomes_recorded":                    namesNoComment,
	"spec.request_stale":                        namesNoComment,
	"subscription.remove_requested":             namesNoComment,
	"subscription.removed":                      namesNoComment,
	"user_agent_state.updated":                  namesNoComment,
	"user_state.updated":                        namesNoComment,
	"user_thread_state.updated":                 namesNoComment,
}

// fullArtifactVersionPayload is the payload docs.ArtifactVersionEventPayload builds holding every
// key it can write: a version with each of its optional fields and an author, a diff, and
// reference changes that name a target and a kind they were cut short of.
func fullArtifactVersionPayload() map[string]any {
	text, size := "text", 1
	return docs.ArtifactVersionEventPayload("artifact", "spec.md", model.Version{
		Number: 2, Named: true, Summary: &text, Authors: []model.Actor{{Kind: "user", ID: "alice"}},
		CreatedAt: time.Now(), Size: &size, MIME: &text, SHA256: &text,
	}, &text, model.ReferenceChanges{
		Targets:        []model.ChangedReference{{Kind: "issue", ID: "CORE-1"}},
		TruncatedKinds: []string{"ask"},
	})
}

// TestEveryEventTypeIsSortedForTheErase holds the erase's payload classification to every event
// type there is: each member of contracts.DispatchEventTypes, the DispatchEvent union of
// packages/contracts, is either a type the erase reads (erasedEventPayloads) or one it never
// reads (unreadEventTypes), and is in exactly one of the two; neither names a type the union
// lacks; and the types the erase reads are the types the classification holds
// (payloadClassification), so a type that starts to name a comment or a document's version the
// erase reads is classified before its payload is redacted by the fallback alone.
func TestEveryEventTypeIsSortedForTheErase(t *testing.T) {
	members := make(map[string]bool, len(contracts.DispatchEventTypes))
	var neither, both []string
	for _, typ := range contracts.DispatchEventTypes {
		members[typ] = true
		_, read := erasedEventPayloads[typ]
		_, unread := unreadEventTypes[typ]
		switch {
		case read && unread:
			both = append(both, typ)
		case !read && !unread:
			neither = append(neither, typ)
		}
	}
	var strays []string
	for _, typ := range slices.Sorted(maps.Keys(erasedEventPayloads)) {
		if !members[typ] {
			strays = append(strays, typ)
		}
	}
	for _, typ := range slices.Sorted(maps.Keys(unreadEventTypes)) {
		if !members[typ] {
			strays = append(strays, typ)
		}
	}
	if len(neither) > 0 {
		t.Errorf("%d of the %d event types are in neither erasedEventPayloads nor unreadEventTypes: %s",
			len(neither), len(contracts.DispatchEventTypes), strings.Join(neither, ", "))
	}
	if len(both) > 0 {
		t.Errorf("%d event types are in both erasedEventPayloads and unreadEventTypes: %s", len(both), strings.Join(both, ", "))
	}
	if len(strays) > 0 {
		t.Errorf("the erase's inventory names %d types that are no member of contracts.DispatchEventTypes: %s",
			len(strays), strings.Join(strays, ", "))
	}
	read, classified := slices.Sorted(maps.Keys(erasedEventPayloads)), slices.Sorted(maps.Keys(payloadClassification))
	if !slices.Equal(read, classified) {
		t.Errorf("the erase reads the event types %s, and its payload classification holds %s: they must be the same",
			strings.Join(read, ", "), strings.Join(classified, ", "))
	}
}

// TestEveryStringOfAnEventPayloadTheEraseReadsHasAClass walks the Go payload of each event type
// the erase reads (erasedEventPayloads) and holds the payload classification to it: each string
// path the walk reaches is in exactly one class of the type's classification
// (payloadClassification), each path the classification names is one the walk reaches, so none
// is stale, and each legacy path (legacyIdentityPaths) is reached by no walk, since a Go field
// that writes it again belongs in the classification. A value the walk cannot read - an
// interface, a map, a json.RawMessage or a type that encodes itself - fails the test, so no field
// passes unread.
func TestEveryStringOfAnEventPayloadTheEraseReadsHasAClass(t *testing.T) {
	for _, typ := range slices.Sorted(maps.Keys(erasedEventPayloads)) {
		built := erasedEventPayloads[typ]
		builtBy := built.builtBy
		if builtBy == "" {
			builtBy = reflect.TypeOf(built.value).String()
		}
		walk := payloadWalk{strings: map[string]string{}}
		walk.value(reflect.ValueOf(built.value), "", builtBy)
		for _, key := range slices.Sorted(maps.Keys(built.added)) {
			if _, reached := walk.strings[key]; !reached {
				walk.strings[key] = built.added[key]
			}
		}
		for _, refused := range walk.refused {
			t.Errorf("%s: %s", typ, refused)
		}
		shape, held := payloadClassification[typ]
		if !held {
			t.Errorf("%s: the erase reads it, and its payload classification holds no classes for it", typ)
			continue
		}
		if shape.content != ownWords && shape.content != quotedWords {
			t.Errorf("%s: its content class is %d, want the node's own words or quoted words", typ, shape.content)
		}
		classes := map[string][]payloadClass{}
		for class, paths := range map[payloadClass][]string{ownWords: shape.own, quotedWords: shape.quoted, identity: shape.identity} {
			for _, path := range paths {
				classes[path] = append(classes[path], class)
			}
		}
		for _, path := range slices.Sorted(maps.Keys(classes)) {
			if len(classes[path]) > 1 {
				t.Errorf("%s: %s is in %d classes of the erase's payload classification, want one", typ, path, len(classes[path]))
			}
			if _, reached := walk.strings[path]; !reached {
				t.Errorf("%s: %s is in the erase's payload classification, and no field of its payload writes it", typ, path)
			}
		}
		for _, path := range slices.Sorted(maps.Keys(walk.strings)) {
			if _, classified := classes[path]; !classified {
				t.Errorf("%s: %s (%s) is in no class of the erase's payload classification", typ, path, walk.strings[path])
			}
		}
		for _, legacy := range legacyIdentityPaths {
			if !slices.Contains(legacy.types, typ) {
				continue
			}
			if declaring, reached := walk.strings[legacy.path]; reached {
				t.Errorf("%s: the legacy path %s is written by a field of %s again: move it into the classification", typ, legacy.path, declaring)
			}
			if _, classified := classes[legacy.path]; classified {
				t.Errorf("%s: the legacy path %s is in the classification as well as in legacyIdentityPaths", typ, legacy.path)
			}
		}
	}
	for _, legacy := range legacyIdentityPaths {
		if legacy.seen == "" || legacy.why == "" {
			t.Errorf("the legacy path %s does not say where it was seen and why no field writes it", legacy.path)
		}
		for _, typ := range legacy.types {
			if _, read := erasedEventPayloads[typ]; !read {
				t.Errorf("the legacy path %s names %s, a type the erase does not read", legacy.path, typ)
			}
		}
	}
}

// payloadWalk is a walk of what a payload is encoded from, as encoding/json encodes it: strings
// holds each string path it reached (payloadPaths) with the Go type that declares it, and refused
// each value it could not read.
type payloadWalk struct {
	strings map[string]string
	refused []string
	// within are the struct types the walk is inside, so a type that holds itself is refused
	// rather than walked forever.
	within []reflect.Type
}

var (
	timeType       = reflect.TypeFor[time.Time]()
	rawMessageType = reflect.TypeFor[json.RawMessage]()
	jsonMarshaler  = reflect.TypeFor[json.Marshaler]()
	textMarshaler  = reflect.TypeFor[encoding.TextMarshaler]()
)

// value walks v, which stands at path and is declared by declaring: a map with string keys is
// walked key by key through the value each key holds, since its type names no key, and any other
// value through its type.
func (w *payloadWalk) value(v reflect.Value, path, declaring string) {
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			w.refuse(path, declaring, "a nil interface, whose type the walk cannot read")
			return
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.Map && v.Type().Key().Kind() == reflect.String {
		for _, key := range v.MapKeys() {
			w.value(v.MapIndex(key), keyPath(path, key.String()), declaring)
		}
		return
	}
	w.walkType(v.Type(), path, declaring)
}

// walkType walks the values a field of type typ, standing at path and declared by declaring, can
// encode to.
func (w *payloadWalk) walkType(typ reflect.Type, path, declaring string) {
	if typ.Kind() == reflect.Pointer {
		w.walkType(typ.Elem(), path, declaring)
		return
	}
	switch {
	case typ == timeType:
		w.strings[path] = declaring
		return
	case typ == rawMessageType:
		w.refuse(path, declaring, "a json.RawMessage, whose keys the walk cannot read")
		return
	case typ.Implements(jsonMarshaler) || reflect.PointerTo(typ).Implements(jsonMarshaler),
		typ.Implements(textMarshaler) || reflect.PointerTo(typ).Implements(textMarshaler):
		w.refuse(path, declaring, typ.String()+", which encodes itself")
		return
	}
	switch typ.Kind() {
	case reflect.String:
		w.strings[path] = declaring
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			w.refuse(path, declaring, "bytes, which encode as a string the walk cannot classify by its type")
			return
		}
		w.walkType(typ.Elem(), path+"[]", declaring)
	case reflect.Struct:
		if slices.Contains(w.within, typ) {
			w.refuse(path, declaring, typ.String()+", which holds itself")
			return
		}
		w.within = append(w.within, typ)
		w.fields(typ, path)
		w.within = w.within[:len(w.within)-1]
	default:
		w.refuse(path, declaring, "a "+typ.Kind().String()+" ("+typ.String()+"), whose keys or encoding the walk cannot read")
	}
}

// fields walks the fields of the struct typ, which stands at path, by the key each encodes under:
// its json tag's name, else its own; a field tagged `json:"-"` and an unexported one are not
// encoded, and an untagged embedded struct's fields stand at path as typ's own do.
func (w *payloadWalk) fields(typ reflect.Type, path string) {
	for index := range typ.NumField() {
		field := typ.Field(index)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		if field.Anonymous && name == "" {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				w.walkType(embedded, path, typ.String())
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		at := keyPath(path, name)
		if slices.Contains(strings.Split(options, ","), "string") {
			// The string option encodes a number or a bool as a JSON string.
			switch field.Type.Kind() {
			case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
				reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
				w.strings[at] = typ.String()
				continue
			}
		}
		w.walkType(field.Type, at, typ.String())
	}
}

// refuse records a value at path, declared by declaring, that the walk cannot read.
func (w *payloadWalk) refuse(path, declaring, what string) {
	w.refused = append(w.refused, fmt.Sprintf("%s (%s) is %s: the walk cannot classify it", path, declaring, what))
}
