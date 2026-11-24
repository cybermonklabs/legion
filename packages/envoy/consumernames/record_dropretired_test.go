package consumernames

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

// Record.DropRetired is the one rule for dropping a retired name, Names.Cleanup's and a reader's
// that deletes retired names itself (the operator's cleanup): it drops the name from Retired and
// Abandoned together, so the record stays one the stores read; it keeps an abandoned candidate read
// absent while its client may still wait for its create, now before its until and the settle age
// after it (the clock skew allowed between the retirer, which wrote until, and the dropper); and it
// drops a name the caller deleted at once. Each answer is decoded as the stores decode it
// (DecodeKVValue). The control is the rule a reader would copy, dropping the name from Retired
// alone, whose record every read refuses as malformed. Before, the library held this rule only
// inside Names.Cleanup, so a reader could only copy it.
func TestDropRetiredDropsARetiredNameFromBothListsAsCleanupDoes(t *testing.T) {
	const key = "ses-1"
	now := testEpoch.Add(time.Hour)
	abandoned := Record{Key: key, Current: "session~C", Start: 3, Retired: []string{"session~A", "session~X"},
		Abandoned: []Abandoned{{Name: "session~X", Until: now.Add(time.Second)}}}
	decodes := func(t *testing.T, r Record) error {
		t.Helper()
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, err = DecodeKVValue(key, data)
		return err
	}
	for _, c := range []struct {
		name          string
		drop          string
		deleted       bool
		at            time.Time
		dropped       bool
		retired       []string
		abandonedLeft int
	}{
		{"an abandoned name read absent before its until is kept", "session~X", false, now, false, []string{"session~A", "session~X"}, 1},
		{"an abandoned name read absent at its until, within the settle age, is kept", "session~X", false, now.Add(time.Second), false, []string{"session~A", "session~X"}, 1},
		{"an abandoned name read absent at its until and the settle age is dropped from both lists", "session~X", false, now.Add(time.Second + defaultSettleAge), true, []string{"session~A"}, 0},
		{"an abandoned name the caller deleted is dropped at once", "session~X", true, now, true, []string{"session~A"}, 0},
		{"a retired name read absent is dropped", "session~A", false, now, true, []string{"session~X"}, 1},
		{"a name the record does not retire is not dropped", "session~Z", true, now, false, []string{"session~A", "session~X"}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, dropped := abandoned.DropRetired(c.drop, c.deleted, c.at)
			if dropped != c.dropped || !slices.Equal(got.Retired, c.retired) || len(got.Abandoned) != c.abandonedLeft {
				t.Fatalf("DropRetired(%s, deleted %t, %s) = %+v, %t; want retired %v, %d abandoned left, dropped %t",
					c.drop, c.deleted, c.at.Sub(now), got, dropped, c.retired, c.abandonedLeft, c.dropped)
			}
			if err := decodes(t, got); err != nil {
				t.Fatalf("the record DropRetired answered does not decode: %v", err)
			}
		})
	}
	if len(abandoned.Retired) != 2 || len(abandoned.Abandoned) != 1 {
		t.Fatalf("DropRetired changed the record it was called on: %+v", abandoned)
	}
	copied := abandoned.clone()
	copied.Retired = slices.DeleteFunc(copied.Retired, func(name string) bool { return name == "session~X" })
	if err := decodes(t, copied); !errors.Is(err, ErrMalformedRecord) {
		t.Fatalf("control: a record with X dropped from retired alone decodes as %v; want ErrMalformedRecord", err)
	}
}
