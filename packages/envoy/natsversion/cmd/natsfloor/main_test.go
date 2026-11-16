package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/natsversion"
)

// natsfloor answers each version with natsversion.Refusal's verdict, one line each and in the
// order given, names the floor's releases on -releases, and refuses to run on nothing. Which
// versions the floor takes is natsversion's own test (TestRefusalNamesEveryServerThatCanSkipAFilter);
// this one holds the command to answering with that rule and in the shape its callers read.
func TestNatsfloorAnswersWithNatsversionsRule(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"2.15.1", "v2.10.22"}, &stdout, &stderr); code != 0 {
		t.Fatalf("natsfloor 2.15.1 v2.10.22 exited %d, want 0; stderr: %s", code, stderr.String())
	}
	refusal := natsversion.Refusal("v2.10.22")
	if refusal == nil {
		t.Fatal("natsversion.Refusal(v2.10.22) = nil: this test needs a version the floor refuses")
	}
	want := "taken\t2.15.1\nrefused\tv2.10.22\t" + refusal.Error() + "\n"
	if stdout.String() != want {
		t.Errorf("natsfloor 2.15.1 v2.10.22 printed %q, want %q", stdout.String(), want)
	}

	stdout.Reset()
	if code := run([]string{"-releases"}, &stdout, &stderr); code != 0 || stdout.String() != natsversion.FixedReleases+"\n" {
		t.Errorf("natsfloor -releases = %d, %q; want 0, %q", code, stdout.String(), natsversion.FixedReleases+"\n")
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(nil, &stdout, &stderr); code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "usage") {
		t.Errorf("natsfloor with no version = %d, stdout %q, stderr %q; want 2, nothing, a usage line", code, stdout.String(), stderr.String())
	}
}
