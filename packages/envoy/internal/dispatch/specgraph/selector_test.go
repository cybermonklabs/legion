package specgraph

import (
	"errors"
	"reflect"
	"testing"
)

// MatchTests picks the entries of exactly the selector's package and test: neither a test whose
// name only starts with the name nor the same name in another package.
func TestMatchTestsIsExact(t *testing.T) {
	t.Parallel()
	ran := []TestOutcome{
		{Package: "example.com/acme/p", Test: "TestA", Outcome: "pass"},
		{Package: "example.com/acme/p", Test: "TestAB", Outcome: "fail"},
		{Package: "example.com/acme/p/q", Test: "TestA", Outcome: "fail"},
		{Package: "example.com/acme/pq", Test: "TestA", Outcome: "fail"},
	}
	got, err := MatchTests(Test{Runner: "go", Selector: "example.com/acme/p TestA", Job: "go-checks"}, ran)
	if err != nil || !reflect.DeepEqual(got, ran[:1]) {
		t.Errorf("MatchTests = %+v, %v, want %+v", got, err, ran[:1])
	}
	if got, err := MatchTests(Test{Runner: "go", Selector: "example.com/acme/p TestC"}, ran); err != nil || len(got) != 0 {
		t.Errorf("MatchTests of a test that did not run = %+v, %v, want none", got, err)
	}
}

// A name that runs in both package b and package b_test matches twice (v-citations fixture 6).
func TestMatchTestsFindsTwo(t *testing.T) {
	t.Parallel()
	got, err := MatchTests(Test{Runner: "go", Selector: "example.com/acme/gotest/b TestB"}, readReport(t, "p-and-p-test.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"example.com/acme/gotest/b TestB pass", "example.com/acme/gotest/b TestB fail"}; !reflect.DeepEqual(outcomes(got), want) {
		t.Errorf("MatchTests = %q, want %q", outcomes(got), want)
	}
}

// Another runner, and a go selector that names no one top-level test, are refused with
// ErrSelector.
func TestMatchTestsRefusesOtherRunners(t *testing.T) {
	t.Parallel()
	ran := []TestOutcome{{Package: "example.com/acme/p", Test: "TestA", Outcome: "pass"}}
	for _, test := range []Test{
		{Runner: "bun", Selector: "example.com/acme/p TestA"},
		{Runner: "", Selector: "example.com/acme/p TestA"},
		{Runner: "go", Selector: "TestA"},
		{Runner: "go", Selector: "example.com/acme/p TestA/sub"},
	} {
		if got, err := MatchTests(test, ran); !errors.Is(err, ErrSelector) || got != nil {
			t.Errorf("MatchTests(%+v) = %+v, %v, want ErrSelector", test, got, err)
		}
	}
}

// A go selector is a Go import path and a top-level test name, separated by one space.
func TestParseSelector(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		selector, path, name string
		ok                   bool
	}{
		{"github.com/sjawhar/envoy/cmd/listener TestARestartLosesNoMessage", "github.com/sjawhar/envoy/cmd/listener", "TestARestartLosesNoMessage", true},
		{"example.com/p Test", "example.com/p", "Test", true},
		{"example.com/p Test_x", "example.com/p", "Test_x", true},
		{"example.com/p Test2", "example.com/p", "Test2", true},
		{"TestX", "", "", false},
		{"example.com/p", "", "", false},
		{"example.com/p  TestA", "", "", false},
		{"example.com/p TestA extra", "", "", false},
		{"example.com/p Testa", "", "", false},
		{"example.com/p TestA/sub", "", "", false},
		{"example.com/p BenchmarkA", "", "", false},
		{"example.com/../p TestA", "", "", false},
		{"/abs/p TestA", "", "", false},
		{"./p TestA", "", "", false},
		{"example.com/p/ TestA", "", "", false},
		{"example.com//p TestA", "", "", false},
		{" example.com/p TestA", "", "", false},
		{"", "", "", false},
	} {
		path, name, err := ParseSelector(row.selector)
		if ok := err == nil; ok != row.ok || path != row.path || name != row.name {
			t.Errorf("ParseSelector(%q) = %q, %q, %v; want %q, %q, ok %t", row.selector, path, name, err, row.path, row.name, row.ok)
		}
	}
}
