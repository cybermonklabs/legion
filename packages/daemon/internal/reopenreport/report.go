// Package reopenreport is the read behind `legion reopened-report`: for each issue the controller
// reopened with a request for new work (D17), the interval from the accepted todo to the end of
// the change the request started, each version of the issue's Verification document that an
// actor other than the daemon wrote in it, whether that version changed a check block, the
// request's withdrawal, and what could not be read. It classifies a row only by what a program
// can tell; the controller's review judges whether the checks cover the omitted work.
package reopenreport

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// Reader is what the report reads of Dispatch, each read as dispatch.HTTPClient makes it.
type Reader interface {
	Artifacts(ctx context.Context, key string) ([]dispatch.Artifact, error)
	EventsAfter(ctx context.Context, key string, after int64) ([]json.RawMessage, error)
	DocumentVersion(ctx context.Context, artifactID string, version int) (string, error)
	ArtifactChanges(ctx context.Context, artifactID string, from, to int) (dispatch.ArtifactChanges, error)
	PlanDocument(ctx context.Context, artifactID string, version int) (dispatch.DocumentVersion, error)
}

// Category is what the program can tell of one row.
type Category string

// The categories of a row.
const (
	ChecksWritten    Category = "checks_written"
	RequestWithdrawn Category = "request_withdrawn"
	NotFollowed      Category = "instruction_not_followed"
	Pending          Category = "pending"
	NotAssessable    Category = "not_assessable"
)

// Categories is every category, in the order the report gives them.
var Categories = []Category{ChecksWritten, RequestWithdrawn, NotFollowed, Pending, NotAssessable}

// Assessed says whether a row of the category ended and could be read.
func (c Category) Assessed() bool { return false }

// Row is one issue's read.
type Row struct {
	Issue      string
	Category   Category
	Start      *Mark
	Change     uint64
	End        *End
	Versions   []Version
	Withdrawal *Withdrawal
	Unread     []string
}

// Mark is an event of the issue's Dispatch log: its sequence and its time.
type Mark struct {
	Seq int64
	At  time.Time
}

// End is the end of the request's change.
type End struct {
	PullRequest int
	At          time.Time
}

// Version is one version of the Verification document in the interval.
type Version struct {
	Number int
	Seq    int64
	Actor  dispatch.Actor
	At     time.Time
	Checks []dispatch.BlockChange
}

// Withdrawal is the request's withdrawal.
type Withdrawal struct {
	Request int64
	Reason  string
	At      time.Time
}

// Read reads the row of issue key.
func Read(ctx context.Context, reader Reader, key string) Row {
	return Row{Issue: key}
}

// Total is one category's rows.
type Total struct {
	Category Category
	Issues   []string
}

// Totals is each category's rows.
func Totals(rows []Row) []Total {
	return nil
}
