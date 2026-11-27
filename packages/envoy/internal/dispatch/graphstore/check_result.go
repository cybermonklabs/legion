package graphstore

import (
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A result is one recorded run of a check (R12 of the issue-tree spec): a node of supertag result,
// addressed by the result's own uuid, which the check_results row of that id projects, beside the
// tuples out of it. It carries as values what a result holds and points at nothing it names by
// value: its verdict, where a person reaches the change (surface), the command that proved it
// (run), what that command printed (observed), the commit it ran at, the role that recorded it, its
// counts of failed, skipped and excluded cases, the negative control it states, and the check's
// words as they stood when it was recorded, the check block's changed_version and text_sha256
// (check_version, check_sha256). A retraction is one more value, retracted, appended with its
// reason by whoever retracted it, and the node itself never changes.
const (
	KindResult     = "result"
	SupertagResult = "result"
)

// The tuple kinds out of a result node beside its TuplePartOf and its TupleAuthoredBy: the check
// whose run it records (TupleResultOf), the session the run was recorded from (TupleRunSession),
// and each image it names as a screenshot (TupleScreenshot). A result points at the check and at
// no requirement, so a check serving several requirements holds one set of results. None of them
// is a kind graph_edges answers (refs.TupleKinds).
const (
	TupleResultOf   = "result_of"
	TupleRunSession = "run_session"
	TupleScreenshot = "screenshot"
)

// The values a result node carries, each named as the tuple that states it, beside
// AttributeVerdict, which a lens's verdict shares; AttributeRetracted is the one value a later
// write appends.
const (
	AttributeSurface         = "surface"
	AttributeRun             = "run"
	AttributeObserved        = "observed"
	AttributeCommit          = "commit"
	AttributeRole            = "role"
	AttributeFailed          = "failed"
	AttributeSkipped         = "skipped"
	AttributeExcluded        = "excluded"
	AttributeNegativeControl = "negative_control"
	AttributeCheckVersion    = "check_version"
	AttributeCheckSHA256     = "check_sha256"
	AttributeRetracted       = "retracted"
)

// ResultLinkKinds are the relations out of a result node, every one of which its check_results
// row states: its part_of and its result_of to its check, its authored_by, its run_session and
// each screenshot (graphcensus' check_result kind).
func ResultLinkKinds() []string {
	return []string{TuplePartOf, TupleResultOf, TupleAuthoredBy, TupleRunSession, TupleScreenshot}
}

// Result is one recorded result as the store holds it: ID, the result's uuid; Project, the project
// of the check's issue; Check, the address of the check block's node, `<ref_key>#<block id>` of
// the artifact its check_results row is keyed by; the values it carries; Session, the session the
// run was recorded from, empty for a person's; Screenshots, the ref_key of each image it names;
// Recorder, who recorded it, which its row's actor holds; At, its row's created_at; and Event, the
// event that recorded it, NoEvent for a row the backfill gives its node.
type Result struct {
	ID, Project, Check                                     string
	Verdict, Surface, Run, Observed, Commit, Role, Control string
	Failed, Skipped, Excluded, CheckVersion                int
	CheckSHA256                                            string
	Session                                                string
	Screenshots                                            []string
	Recorder                                               model.Actor
	At                                                     time.Time
	Event                                                  int64
}

// ResultFacts is what the store holds of one result: its node - project-visible, live, in its
// check's project, made by its recorder at its row's created_at and caused by its event, carrying
// its twelve values, each also a value tuple (AttributeFacts) - then the recorder's own nodes
// (AuthorFacts), and its tuples: its part_of and its result_of to its check, its authored_by, its
// run_session where the run had one, and one screenshot to each image it names, each once. Every
// relation carries the recorder, the time and the event of the node. ok is false where the
// result's id or its check's address is past the store's bound, of which nothing is stated; a
// session or an image past it is no tuple, as AuthorFacts states no author past it.
//
// The node is marked created (Created), so a record made for a run names it as changed by that
// run. It restates nothing: a result is written once, by the write that records it or, for a row
// an earlier release wrote, by the backfill, and no later write moves it.
func ResultFacts(result Result) (nodes []Node, tuples []Tuple, ok bool) {
	if !AddressFits(result.ID) || !AddressFits(result.Check) {
		return nil, nil, false
	}
	node := Created(Node{
		Kind: KindResult, ID: result.ID, Supertag: SupertagResult,
		Visibility: VisibilityProject, State: StateLive, Project: result.Project,
		Attributes: map[string]any{
			AttributeVerdict: result.Verdict, AttributeSurface: result.Surface, AttributeRun: result.Run,
			AttributeObserved: result.Observed, AttributeCommit: result.Commit, AttributeRole: result.Role,
			AttributeFailed: result.Failed, AttributeSkipped: result.Skipped, AttributeExcluded: result.Excluded,
			AttributeNegativeControl: result.Control, AttributeCheckVersion: result.CheckVersion,
			AttributeCheckSHA256: result.CheckSHA256,
		},
		MadeAt: result.At, MadeBy: result.Recorder, SourceEvent: result.Event,
	})
	relation := func(kind, toKind, toID string) Tuple {
		return Tuple{
			FromKind: KindResult, FromID: result.ID, Kind: kind, ToKind: toKind, ToID: toID,
			At: result.At, Who: result.Recorder, SourceEvent: result.Event, Asserted: true,
		}
	}
	tuples = AttributeFacts(nil, node)
	tuples = append(tuples, relation(TuplePartOf, blockKind, result.Check), relation(TupleResultOf, blockKind, result.Check))
	authors, authored, _ := AuthorFacts(KindResult, result.ID, result.Recorder, result.At, result.Event)
	tuples = append(tuples, authored...)
	if result.Session != "" && AddressFits(result.Session) {
		tuples = append(tuples, relation(TupleRunSession, KindSession, result.Session))
	}
	var named []string
	for _, screenshot := range result.Screenshots {
		if !AddressFits(screenshot) || slices.Contains(named, screenshot) {
			continue
		}
		named = append(named, screenshot)
		tuples = append(tuples, relation(TupleScreenshot, KindArtifact, screenshot))
	}
	return append([]Node{node}, authors...), tuples, true
}

// ResultRetracted is the retraction of result id: its retracted value, holding the reason, stated
// by who at at and caused by event, which the check_result_retractions row of the result
// projects.
func ResultRetracted(id, reason string, who model.Actor, at time.Time, event int64) Tuple {
	return Tuple{
		FromKind: KindResult, FromID: id, Kind: AttributeRetracted, ToKind: KindValue, ToID: reason,
		At: at, Who: who, SourceEvent: event, Asserted: true,
	}
}
