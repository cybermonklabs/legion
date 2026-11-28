package shimwire

import (
	"fmt"

	"github.com/sjawhar/envoy/steprecord"
)

// The frames of a launch's model totals. The daemon reads the totals that the shim counted since
// its harness started, and the shim answers. When the harness exits, the shim sends its last totals
// unasked. The Claude Code bridge names its launch's set model to the shim, which forwards no such
// frame.
const (
	TypeModelTotals       = "model_totals"        // daemon → shim
	TypeModelTotalsResult = "model_totals_result" // shim → daemon
	TypeModelTotalsFinal  = "model_totals_final"  // shim → daemon, unasked, when the harness exits
	TypeLegionSetModel    = "legion_set_model"    // claude-bridge → shim, never forwarded
)

// MaxTotalsModels is the most models one launch's totals name: four times the models one step
// record names. A launch that more models answered reports that its totals overflowed.
const MaxTotalsModels = 4 * steprecord.AnswersMax

// ModelCount is one model and the calls it answered. A model's name is the frame's provider and
// model as ModelName joins them.
type ModelCount struct {
	Model string `json:"model"`
	Calls uint64 `json:"calls"`
}

// ModelName is the name of the model a frame names: the provider, a slash and the model, or the
// model alone where the provider is empty. The shim counts each call under it, and the daemon
// names the model that answered a turn by it (superviseEvent, internal/daemon).
func ModelName(provider, model string) string {
	if provider == "" {
		return model
	}
	return provider + "/" + model
}

// Totals is what one launch's shim counted since its harness started.
type Totals struct {
	Read        uint64       `json:"read"`               // raised by 1 at each read and at the final frame
	SetModel    string       `json:"setModel,omitempty"` // "" not known
	Counts      []ModelCount `json:"counts"`             // sorted by Model, never null
	OutsideForm uint64       `json:"outsideForm"`        // calls whose model name is outside the form
	Overflowed  bool         `json:"overflowed"`         // more than MaxTotalsModels distinct models answered
}

// ModelTotals asks the shim for its launch's totals.
type ModelTotals struct {
	ID string `json:"id"`
}

// ModelTotalsResult answers ModelTotals with the same ID: the totals, or the reason the shim
// gives none.
type ModelTotalsResult struct {
	ID     string  `json:"id"`
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Totals *Totals `json:"totals,omitempty"`
}

// ModelTotalsFinal is the last totals of a launch, which the shim sends unasked once its harness
// has exited.
type ModelTotalsFinal struct {
	Totals Totals `json:"totals"`
}

// LegionSetModel names the model that a Claude Code launch's session started on, as the bridge
// reads it from the launch's first init line.
type LegionSetModel struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

func (f ModelTotals) MarshalJSON() ([]byte, error) {
	type plain ModelTotals
	return marshalFrame(TypeModelTotals, plain(f))
}

func (f ModelTotalsResult) MarshalJSON() ([]byte, error) {
	type plain ModelTotalsResult
	return marshalFrame(TypeModelTotalsResult, plain(f))
}

func (f ModelTotalsFinal) MarshalJSON() ([]byte, error) {
	type plain ModelTotalsFinal
	return marshalFrame(TypeModelTotalsFinal, plain(f))
}

func (f LegionSetModel) MarshalJSON() ([]byte, error) {
	type plain LegionSetModel
	return marshalFrame(TypeLegionSetModel, plain(f))
}

// Validate refuses an answer the daemon cannot use. The shim is its only sender. It refuses an
// answer with no id, an ok answer with no totals, a refusal with no reason, and totals no shim
// counts.
func (f ModelTotalsResult) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: model_totals_result carries no id", ErrMalformedFrame)
	case f.OK && f.Totals == nil:
		return fmt.Errorf("%w: model_totals_result is ok and carries no totals", ErrMalformedFrame)
	case !f.OK && f.Error == "":
		return fmt.Errorf("%w: model_totals_result refuses with no error", ErrMalformedFrame)
	case f.Totals != nil:
		return f.Totals.check(TypeModelTotalsResult)
	}
	return nil
}

// Validate refuses final totals no shim counts, by the rules of an answer's totals.
func (f ModelTotalsFinal) Validate() error {
	return f.Totals.check(TypeModelTotalsFinal)
}

// check refuses totals no shim counts: a read numbered 0, more than MaxTotalsModels models, a
// model counted with no call, or a model listed twice.
func (t Totals) check(frameType string) error {
	if t.Read == 0 {
		return fmt.Errorf("%w: %s totals are numbered read 0", ErrMalformedFrame, frameType)
	}
	if len(t.Counts) > MaxTotalsModels {
		return fmt.Errorf("%w: %s totals name %d models, limit %d", ErrMalformedFrame, frameType, len(t.Counts), MaxTotalsModels)
	}
	seen := make(map[string]struct{}, len(t.Counts))
	for _, count := range t.Counts {
		if count.Calls == 0 {
			return fmt.Errorf("%w: %s totals count %q with 0 calls", ErrMalformedFrame, frameType, count.Model)
		}
		if _, twice := seen[count.Model]; twice {
			return fmt.Errorf("%w: %s totals name %q twice", ErrMalformedFrame, frameType, count.Model)
		}
		seen[count.Model] = struct{}{}
	}
	return nil
}
