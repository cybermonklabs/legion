package asof_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
)

var updateAbsentFields = flag.Bool("update", false, "rewrite testdata/absent-fields.json from asof.AbsentFields")

// absentFieldsFixture is where TestTheAbsentFieldsFixtureIsAbsentFields commits the pointers of
// asof.AbsentFields, from this package's directory. The dispatch command's test of the words of
// its `Left out at` line (packages/envoy-client/src/__tests__/dispatch-as-of.test.ts) reads it, so
// it holds those words to every pointer the table names, as data and not as Go source.
const absentFieldsFixture = "testdata/absent-fields.json"

// TestTheAbsentFieldsFixtureIsAbsentFields holds testdata/absent-fields.json to asof.AbsentFields:
// an object that names, by route, each pointer the route's past-time answer can name in
// Dispatch-As-Of-Absent, sorted. The file is the bytes this test writes. A change to the table, or
// a hand edit of the file, is red here until the file is written again:
// `go -C packages/envoy test -count=1 -run '^TestTheAbsentFieldsFixtureIsAbsentFields$' ./internal/dispatch/asof -update`.
func TestTheAbsentFieldsFixtureIsAbsentFields(t *testing.T) {
	const rewrite = "go -C packages/envoy test -count=1 -run '^TestTheAbsentFieldsFixtureIsAbsentFields$' ./internal/dispatch/asof -update"
	pointers := make(map[string][]string, len(asof.AbsentFields))
	for route, fields := range asof.AbsentFields {
		named := make([]string, 0, len(fields))
		for _, field := range fields {
			named = append(named, field.Pointer)
		}
		slices.Sort(named)
		pointers[route] = named
	}
	encoded, err := json.MarshalIndent(pointers, "", "  ")
	if err != nil {
		t.Fatalf("encode the pointers of asof.AbsentFields: %v", err)
	}
	got := append(encoded, '\n')
	if *updateAbsentFields {
		if err := os.WriteFile(absentFieldsFixture, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", absentFieldsFixture)
		return
	}
	want, err := os.ReadFile(absentFieldsFixture)
	if err != nil {
		t.Fatalf("read %s (%s writes it): %v", absentFieldsFixture, rewrite, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is not the pointers of asof.AbsentFields; when the change is meant, rewrite it with %s, "+
			"and run the dispatch command's test that reads it (packages/envoy-client/src/__tests__/dispatch-as-of.test.ts). "+
			"The pointers are:\n%s", absentFieldsFixture, rewrite, got)
	}
}
