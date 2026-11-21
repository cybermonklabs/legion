package asof_test

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// A cut is PostgreSQL's precision, a microsecond, and never later than the time the caller
// named: a cut rounded up would read an event written after that time. The Dispatch-As-Of
// header and the literal in the shadows both come from the parsed time, in UTC.
func TestParseCutRoundsDownToTheMicrosecond(t *testing.T) {
	for _, row := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "no fraction", raw: "2026-11-10T00:00:00Z", want: "2026-11-10T00:00:00.000000Z"},
		{name: "nine digits round down", raw: "2026-11-10T12:34:56.123456789Z", want: "2026-11-10T12:34:56.123456Z"},
		{name: "a fraction just short of the next second stays in its second", raw: "2026-11-10T23:59:59.9999999Z", want: "2026-11-10T23:59:59.999999Z"},
		{name: "an offset becomes UTC", raw: "2026-11-10T02:00:00.5+02:00", want: "2026-11-10T00:00:00.500000Z"},
	} {
		t.Run(row.name, func(t *testing.T) {
			cut, err := asof.ParseCut(row.raw)
			if err != nil {
				t.Fatalf("ParseCut(%q): %v", row.raw, err)
			}
			if got := cut.String(); got != row.want {
				t.Errorf("ParseCut(%q) = %s, want %s", row.raw, got, row.want)
			}
			if cut.Time().Location() != time.UTC {
				t.Errorf("ParseCut(%q) is in %s, want UTC", row.raw, cut.Time().Location())
			}
		})
	}

	for _, row := range []struct {
		name string
		raw  string
	}{
		{name: "a date alone", raw: "2026-11-10"},
		{name: "empty", raw: ""},
		{name: "garbage", raw: "yesterday"},
		{name: "a month that does not exist", raw: "2026-13-01T00:00:00Z"},
		{name: "no offset", raw: "2026-11-10T00:00:00"},
	} {
		t.Run(row.name, func(t *testing.T) {
			cut, err := asof.ParseCut(row.raw)
			var invalid *asof.InvalidError
			if !errors.As(err, &invalid) {
				t.Fatalf("ParseCut(%q) = %s, %v; want an *asof.InvalidError", row.raw, cut, err)
			}
		})
	}
}
