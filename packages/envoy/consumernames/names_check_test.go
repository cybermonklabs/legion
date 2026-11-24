package consumernames

import (
	"context"
	"strings"
	"testing"
	"time"
)

// check is the creator's own family fence: every record the creator's read refuses as naming a
// consumer outside its key's family, check refuses with the same class of error, naming the field
// that holds the name, and every record the creator serves, check passes. The rows are the ones
// TestARecordNamesOnlyItsOwnFamilysConsumers runs through the creator, and a retired name that is
// the key's fixed name. The control is a copy of check that skips the prefix test and asks only
// for a ~<random> name: it passes another family's names, so it disagrees with the creator.
func TestNamesCheckIsTheCreatorsFence(t *testing.T) {
	const key = "ses-1"
	rows := []struct {
		name, prefix, field string
		record              Record
	}{
		{"a current of the daemon's durable", "session", "current", Record{Current: "legion-go-p-dispatch~AAAAAAAAAAAAAAAAAAAAAA", Start: 3}},
		{"a current of the listener's durable", "session", "current", Record{Current: "listener-m~AAAAAAAAAAAAAAAAAAAAAA", Start: 3}},
		{"a current that is the fixed name", "session", "current", Record{Current: "s-ses-1", Start: 3}},
		{"a current of the prefix with no suffix", "session", "current", Record{Current: "session~", Start: 3}},
		{"a current of the prefix and more", "session", "current", Record{Current: "session-x~AAAA", Start: 3}},
		{"a retired name of the daemon's durable", "session", "retired name", Record{Current: "session~A", Start: 3, Retired: []string{"legion-go-p-dispatch"}}},
		{"a retired name that is the fixed name", "session", "retired name", Record{Current: "session~A", Start: 3, Retired: []string{"s-ses-1"}}},
		{"a candidate of another family", "session", "candidate", Record{Start: 3, Op: &Op{Kind: OpCreate, Candidate: "delivery-jobs~B", ReservedAt: testEpoch}}},
		{"an old name of another family", "session", "old name", Record{Current: "session~A", Start: 3, Op: &Op{Kind: OpDelete, Old: "delivery-events", ReservedAt: testEpoch}}},
		{"a session's current under a durable's prefix", "listener-m", "current", Record{Current: "session~A", Start: 3}},
		{"a current of the family", "session", "", Record{Current: "session~A", Start: 3}},
		{"an old name that is the key's fixed name", "session", "", Record{Current: "session~A", Start: 3, Op: &Op{Kind: OpDelete, Old: "s-ses-1", ReservedAt: testEpoch}}},
		{"a durable's own current", "listener-m", "", Record{Current: "listener-m~A", Start: 3}},
	}
	// skipPrefix is the control: check with no prefix test, asking each name only for '~' and a
	// fresh suffix's characters after it.
	skipPrefix := func(_ *Names, logical string, record Record) error {
		fresh := func(name string) bool {
			_, suffix, ok := strings.Cut(name, "~")
			return ok && suffix != "" && !strings.ContainsFunc(suffix, func(r rune) bool {
				return !('0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z')
			})
		}
		names := append([]string{record.Current}, record.Retired...)
		if record.Op != nil {
			names = append(names, record.Op.Candidate)
		}
		for _, name := range names {
			if name != "" && !fresh(name) {
				return ErrMalformedRecord
			}
		}
		return nil
	}
	check := func(n *Names, logical string, record Record) error { return n.check(logical, record) }
	disagreements := map[string][]string{}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := newHarness()
			record := row.record
			record.Key = key
			h.store.put(key, record)
			names := h.names("p1")
			names.cfg.Prefix = func(string) string { return row.prefix }
			names.cfg.Fixed = func(logical string) string { return "s-" + logical }
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, creatorErr := names.read(ctx, key)
			want := errorClass(creatorErr)
			if (row.field != "") != (want == "malformed record") {
				t.Fatalf("the creator's read of %s = %v; the row expects a refusal: %v", row.name, creatorErr, row.field != "")
			}
			for name, decide := range map[string]func(*Names, string, Record) error{"Check": check, "control": skipPrefix} {
				err := decide(names, key, record)
				var differs []string
				if got := errorClass(err); got != want {
					differs = append(differs, "class "+got)
				}
				if name == "Check" && err != nil && !strings.Contains(err.Error(), row.field) {
					differs = append(differs, "no field "+row.field+" in "+err.Error())
				}
				if len(differs) > 0 {
					disagreements[name] = append(disagreements[name], row.name+": "+strings.Join(differs, ", "))
				}
			}
		})
	}
	if got := disagreements["Check"]; len(got) > 0 {
		t.Errorf("check disagrees with the creator's read on %d rows: %v", len(got), got)
	}
	control := strings.Join(disagreements["control"], "; ")
	if !strings.Contains(control, "a current of the daemon's durable") || !strings.Contains(control, "a candidate of another family") {
		t.Errorf("control: the check that skips the prefix test disagrees with the creator on %q; want it to on another family's current and candidate", control)
	}
}
