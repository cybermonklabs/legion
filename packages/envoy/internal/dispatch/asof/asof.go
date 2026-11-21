// Package asof reads Dispatch as it stood at a past time, the cut. A route that answers as_of
// runs its present statements under a context that carries the cut (WithCut). The store binds
// each statement to the cut (Bind): it puts a definition at the cut of each shadowed table that
// the statement names (a shadow, Shadows) in front of the statement as a WITH member of the same
// name, and PostgreSQL resolves the table's name to that member before the table. So a past-time
// read runs the present read's own filters, joins, order and paging. The package also holds the
// tables a past-time read reads with no shadow (MachineryTables, CutReadTables), the fields that
// a past-time answer can leave out (AbsentFields), and the read of the newest event id at the
// cut that bounds an event log's page (NewestEventIDStatement).
//
// The package imports no part of the store or of the API: the store imports it.
package asof

import (
	"context"
	"fmt"
	"time"
)

// cutLayout is RFC 3339 with exactly six fractional digits. A Cut in UTC formats its offset as
// Z.
const cutLayout = "2006-01-02T15:04:05.000000Z07:00"

// Cut is the time a past-time read answers as of: a UTC time at PostgreSQL's precision, the
// microsecond. Only ParseCut and CutAt make one, so every Cut is in UTC and holds no nanoseconds
// below its microsecond.
type Cut struct {
	t time.Time
}

// ParseCut reads raw as RFC 3339, with or without a fraction of a second, and gives the cut in
// UTC, rounded down to the microsecond. A cut rounded up would read events written after the
// time the caller named. A value that is not an RFC 3339 time with a date, a time and an offset
// (a date alone, an empty value, a month 13) is an *InvalidError. The gate refuses a cut later
// than the database's now() and an as_of given twice; ParseCut does not.
func ParseCut(raw string) (Cut, error) {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return Cut{}, &InvalidError{Value: raw, Err: err}
	}
	return CutAt(parsed), nil
}

// CutAt is the cut at instant: instant in UTC, rounded down to the microsecond, as ParseCut
// rounds a parsed as_of. The gate makes the cut of a read-only token's cut with it
// (model.Credential.Cut), which the broker's answer already gave as a time.
func CutAt(instant time.Time) Cut {
	return Cut{t: instant.UTC().Truncate(time.Microsecond)}
}

// Time is the cut as a time.Time in UTC.
func (c Cut) Time() time.Time { return c.t }

// String is the cut in RFC 3339 with six fractional digits in UTC, such as
// 2026-11-10T00:00:00.000000Z. It is the value of the Dispatch-As-Of header
// (contracts.DispatchAsOfHeader).
func (c Cut) String() string { return c.t.Format(cutLayout) }

// Literal is the cut as a PostgreSQL timestamptz literal, such as
// '2026-11-10T00:00:00.000000Z'::timestamptz. The shadows hold it. It comes from the parsed
// time, never from the text of a request.
func (c Cut) Literal() string { return "'" + c.String() + "'::timestamptz" }

// InvalidError is an as_of that ParseCut cannot read. The gate answers it with 400
// contracts.DispatchInvalidAsOf.
type InvalidError struct {
	// Value is the as_of as the request sent it.
	Value string
	// Err is why time.Parse refused Value.
	Err error
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("as_of %q is not an RFC 3339 time with a date, a time and an offset, such as 2026-11-10T00:00:00Z", e.Value)
}

func (e *InvalidError) Unwrap() error { return e.Err }

type cutKey struct{}

// WithCut returns ctx carrying cut. The store binds every statement it sends under ctx, or under
// a context derived from it, to cut (store.Pool).
func WithCut(ctx context.Context, cut Cut) context.Context {
	return context.WithValue(ctx, cutKey{}, cut)
}

// CutFrom is the cut ctx carries, and false when it carries none.
func CutFrom(ctx context.Context) (Cut, bool) {
	cut, ok := ctx.Value(cutKey{}).(Cut)
	return cut, ok
}

// WithoutCut returns ctx carrying no cut, for a read under a request at a cut that decides
// whether the cut can be answered, such as the reach of the keyword table's fill
// (store.TextVersionsAt): the store sends its statements unbound.
func WithoutCut(ctx context.Context) context.Context {
	return context.WithValue(ctx, cutKey{}, nil)
}
