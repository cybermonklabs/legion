package daemon

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/fleetalert"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// fleetAlertLines keeps the open lines of project's R6 writer in the store (fleetalert.Lines), so
// a daemon that boots again writes no line of a case the daemon before it opened and nothing has
// ended since (store's FleetAlertLines).
type fleetAlertLines struct {
	st      *store.Store
	project string
}

func (l fleetAlertLines) OpenLines(ctx context.Context) ([]fleetalert.Line, error) {
	held, err := l.st.FleetAlertLines(ctx, l.project)
	if err != nil {
		return nil, err
	}
	lines := make([]fleetalert.Line, 0, len(held))
	for _, line := range held {
		lines = append(lines, fleetalert.Line{Word: line.Case, Claim: line.Claim})
	}
	return lines, nil
}

func (l fleetAlertLines) PutLine(ctx context.Context, line fleetalert.Line) error {
	return l.st.PutFleetAlertLine(ctx, l.stored(line))
}

func (l fleetAlertLines) DeleteLine(ctx context.Context, line fleetalert.Line) error {
	return l.st.DeleteFleetAlertLine(ctx, l.stored(line))
}

// stored is line as the store keeps it, under the project.
func (l fleetAlertLines) stored(line fleetalert.Line) store.FleetAlertLine {
	return store.FleetAlertLine{Project: l.project, Case: line.Word, Claim: line.Claim}
}
