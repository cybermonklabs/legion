package graphstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The attributes of the catalogue's nodes, as the tuple that states each value spells it, and as
// the attribute nodes of migration 20261123174710 declare them: a lens_kind's words, the words a
// person reads for that kind of lens; and a constraint's standard and reference, which for a
// constraint of the catalogue are the kind's primary source and that source's reference. A
// constraint written under an issue holds a standard with its reference, or a measurement with
// its value and where it was taken (AttributeValue, the measured value itself, is an attribute of
// the constraint type and no relation's far end, which KindValue names).
const (
	AttributeWords       = "words"
	AttributeStandard    = "standard"
	AttributeReference   = "reference"
	AttributeMeasurement = "measurement"
	AttributeValue       = "value"
	AttributeTakenWhere  = "taken_where"
)

// LensKind is one kind of lens as the catalogue holds it (spec requirement r-lens-catalogue):
// a lens_kind node addressed `dispatch://lens_kind/<ID>`, carrying Words, and one constrained_by
// tuple from it to a constraint node holding Source, the kind's primary source, as its standard,
// and that source's Reference.
type LensKind struct {
	ID, Words, Source, Reference string
}

// lensKindWriter is who a lens kind added after the seed is made by: the command that adds one
// (`envoy-dispatch lens-kind-add`), which runs with the database credential alone and acts for no
// person. The seed's own nodes are made by the migration's writer, `lens-catalogue`.
var lensKindWriter = model.Actor{Kind: "system", ID: "lens-kind-add"}

// ErrLensKindHeld refuses a lens kind whose id the catalogue already holds, at any state: a kind
// is added once, and a second add of the same id would give it a second source. What a write is
// refused with is a LensKindHeldError, which wraps it and names the id.
var ErrLensKindHeld = errors.New("the catalogue holds that lens kind already")

// LensKindHeldError is ErrLensKindHeld for one id.
type LensKindHeldError struct{ ID string }

func (e *LensKindHeldError) Error() string {
	return fmt.Sprintf("the catalogue holds the lens kind %s already (dispatch://%s/%s), so nothing was added", e.ID, KindLensKind, e.ID)
}

func (e *LensKindHeldError) Unwrap() error { return ErrLensKindHeld }

// ErrLensKindMalformed refuses a lens kind that is not one the catalogue can hold: an id no
// `dispatch://lens_kind/<slug>` address reads back (text.ParseRef), or empty words, source or
// reference. The error that wraps it says which.
var ErrLensKindMalformed = errors.New("the lens kind is not one the catalogue can hold")

// AddLensKind adds one kind to the lens catalogue in the caller's transaction: the lens_kind node,
// its constraint node holding the source and its reference, and the one constrained_by tuple from
// the first to the second, each project-visible and live, in no project, made by lensKindWriter at
// at, through one Write at the sentinel, since no event records a kind. It answers the
// constraint's id, a fresh uuid.
//
// The seed of migrations 20261123174710 and 20261123174720 writes the same shape, and the shape is
// a contract with cybermonklabs/legion#4177's project clause (the catalogue list of its
// refs/catalogue.go): every catalogue constraint is the far end of exactly one constrained_by tuple
// from a lens_kind node, and the catalogue writes no other constraint node. So adding a kind is one
// node, its source and one tuple, and no code changes.
//
// An id the catalogue holds, at any state, is refused with a LensKindHeldError and nothing is
// written. The read of it takes a transaction lock on the id first, so two adds of one id at once
// run one after the other, and the second reads the first's node and is refused.
func AddLensKind(ctx context.Context, tx pgx.Tx, kind LensKind, at time.Time) (string, error) {
	if err := checkLensKind(kind); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('lens-kind:' || $1))`, kind.ID); err != nil {
		return "", fmt.Errorf("take the lock of lens kind %s: %w", kind.ID, err)
	}
	if _, held, err := ReadNode(ctx, tx, KindLensKind, kind.ID); err != nil {
		return "", fmt.Errorf("read whether the catalogue holds lens kind %s: %w", kind.ID, err)
	} else if held {
		return "", &LensKindHeldError{ID: kind.ID}
	}
	source := uuid.NewString()
	// Ascending (kind, id), the order a write takes its node rows in: constraint before lens_kind.
	constraint := Node{
		Kind: KindConstraint, ID: source, Supertag: KindConstraint,
		Visibility: VisibilityProject, State: StateLive,
		Attributes: map[string]any{AttributeStandard: kind.Source, AttributeReference: kind.Reference},
		MadeAt:     at, MadeBy: lensKindWriter, SourceEvent: NoEvent,
	}
	lensKind := Node{
		Kind: KindLensKind, ID: kind.ID, Supertag: KindLensKind,
		Visibility: VisibilityProject, State: StateLive,
		Attributes: map[string]any{AttributeWords: kind.Words},
		MadeAt:     at, MadeBy: lensKindWriter, SourceEvent: NoEvent,
	}
	tuples := append(AttributeTuples(constraint, AttributeReference, AttributeStandard),
		AttributeTuples(lensKind, AttributeWords)...)
	tuples = append(tuples, Tuple{
		FromKind: KindLensKind, FromID: kind.ID, Kind: TupleConstrainedBy, ToKind: KindConstraint, ToID: source,
		At: at, Who: lensKindWriter, SourceEvent: NoEvent, Asserted: true,
	})
	if err := Write(ctx, tx, []Node{constraint, lensKind}, tuples); err != nil {
		return "", err
	}
	return source, nil
}

// checkLensKind refuses a kind the catalogue cannot hold (ErrLensKindMalformed), naming each
// part that is wrong.
func checkLensKind(kind LensKind) error {
	var problems []string
	if ref, ok := text.ParseRef("dispatch://"+KindLensKind+"/"+kind.ID, ""); !ok || ref.Kind != KindLensKind || ref.ID != kind.ID {
		problems = append(problems, fmt.Sprintf("the id %q is no lens kind's address: it is a lowercase slug, "+
			"letters and digits in words joined by single hyphens, as dispatch://lens_kind/<id> spells it", kind.ID))
	}
	for _, part := range []struct{ named, value string }{
		{"the words are", kind.Words}, {"the source is", kind.Source}, {"the reference is", kind.Reference},
	} {
		if strings.TrimSpace(part.value) == "" {
			problems = append(problems, part.named+" empty")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrLensKindMalformed, strings.Join(problems, "; "))
	}
	return nil
}
