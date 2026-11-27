package intake

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/oauth2call"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// dispatchDocumentPrefix begins every Dispatch event of a project document
// (packages/envoy/internal/dispatch/outbox/publisher.go, documentTopicPrefix and envelope):
// notifications.dispatch.document.<KEY>.<slug>.<type>. Neither the project key nor the slug
// holds a dot, so the type is everything after the slug, its own dots included.
const dispatchDocumentPrefix = "notifications.dispatch.document."

// mergeHoldsSubject is the subject of every event of project's merge holds document
// (record.MergeHoldsDocument), which the Dispatch consumer hears beside every issue event.
func mergeHoldsSubject(project string) string {
	return dispatchDocumentPrefix + project + "." + record.MergeHoldsDocument + ".>"
}

// mergeHoldsChangeTypes are the types of the Dispatch events that change the merge holds document
// itself: its creation and each new version (packages/envoy/internal/dispatch/api/artifacts.go).
// Each fact is a notice to every architect in accepting and a wake for every merger in merging, so
// no other artifact.* type is one. A rename changes the document's title alone, and the slug a
// merger reads it by and every line of it stay (artifact_rename.go), so it changes no hold; and
// Dispatch removes no project document (artifact_removal.go, ARTIFACT_PROJECT_DOCUMENT), so no
// removal of it arrives. A person's edit writes its artifact.version at once and
// artifact.human_edited for the same change after the quiet window (docs/edit_notices.go), so a
// fact of both would tell each role twice of one change; artifact.approved and
// artifact.changes_requested change no line of the document, so no hold either.
var mergeHoldsChangeTypes = []string{"artifact.created", "artifact.version"}

// decodeDispatchDocumentFact decodes one Dispatch event of a project document. An event of
// project's merge holds document that changes the document (mergeHoldsChangeTypes) is a
// MergeHoldsChanged under eventID. Any other document, any other project's document, and an event
// of another type on the document, such as a comment or a person's edit settling, is no fact. The
// consumer's filter delivers none of the first two (mergeHoldsSubject), and the decoder holds the
// rule by itself all the same, so another project's merge holds never move this project's roles.
// The payload is the Dispatch event itself, with its project, its document's id and its type; one
// that disagrees with its subject is refused. A payload that does not decode is refused in fixed
// text (oauth2call.UnmarshalData), and one that disagrees is refused by the member alone, never by
// its value, which holds whatever the publisher wrote there.
func decodeDispatchDocumentFact(subject, project, eventID, payload string) (Fact, error) {
	subjectProject, rest, _ := strings.Cut(strings.TrimPrefix(subject, dispatchDocumentPrefix), ".")
	slug, eventType, _ := strings.Cut(rest, ".")
	if subjectProject == "" || slug == "" || eventType == "" {
		return nil, fmt.Errorf("Dispatch document subject names no project, document or event type: %s", subject)
	}
	if subjectProject != project || slug != record.MergeHoldsDocument || !slices.Contains(mergeHoldsChangeTypes, eventType) {
		return nil, nil
	}
	var event struct {
		Project    string  `json:"project"`
		Type       string  `json:"type"`
		ArtifactID *string `json:"artifact_id"`
	}
	if err := oauth2call.UnmarshalData([]byte(payload), &event); err != nil {
		return nil, fmt.Errorf("decode Dispatch document event: %w", err)
	}
	switch {
	case event.Project != subjectProject:
		return nil, errors.New("Dispatch document event payload project disagrees with its subject")
	case event.Type != eventType:
		return nil, errors.New("Dispatch document event payload type disagrees with its subject")
	case event.ArtifactID == nil || *event.ArtifactID == "":
		return nil, errors.New("Dispatch document event payload names no artifact_id")
	}
	return MergeHoldsChanged{Project: project, Event: eventID}, nil
}
