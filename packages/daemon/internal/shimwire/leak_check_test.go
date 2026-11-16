package shimwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// theLeakCheckHello is the launcher_hello an older launcher writes, which names no leak check.
var theLeakCheckHello = LauncherHello{Token: "launcher-token", Sandbox: "legion-legion-legion-208", Role: "tester", PodUID: "pod-1", LauncherID: "launcher-1"}

// A launcher with no pin writes the hello it wrote before the leak check fields existed, to the
// byte, so a daemon reads it as before; one with a pin carries the tag, and once it has installed
// that tag says so, and both cross the wire whole.
func TestALauncherHelloRoundTripsWithAndWithoutTheLeakCheck(t *testing.T) {
	const older = `{"type":"launcher_hello","token":"launcher-token","sandbox":"legion-legion-legion-208","role":"tester","podUid":"pod-1","launcherId":"launcher-1"}`
	pinned := theLeakCheckHello
	pinned.LeakCheckTag = "leak-check-v15"
	installed := pinned
	installed.LeakCheckInstalled = true
	for _, tc := range []struct {
		name  string
		frame LauncherHello
		line  string
	}{
		{"no pin", theLeakCheckHello, older},
		{"a pin not installed", pinned, strings.TrimSuffix(older, "}") + `,"leakCheckTag":"leak-check-v15"}`},
		{"a pin installed", installed, strings.TrimSuffix(older, "}") + `,"leakCheckTag":"leak-check-v15","leakCheckInstalled":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, err := json.Marshal(tc.frame)
			if err != nil {
				t.Fatal(err)
			}
			if string(line) != tc.line {
				t.Fatalf("encoded as %s, want %s", line, tc.line)
			}
			decoded, err := Decode(line)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, tc.frame) {
				t.Fatalf("decoded %#v, want %#v", decoded, tc.frame)
			}
			if err := tc.frame.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The hello refuses a leak check said to be installed under no tag, and takes a tag of any other
// shape: that launcher still connects, and the daemon refuses the tag as the cause of the start
// it cannot serve the check for, which a refused hello would have left it no connection to name.
func TestALauncherHelloRefusesAnInstallWithNoTagAndTakesAnOffShapeTag(t *testing.T) {
	offShape := theLeakCheckHello
	offShape.LeakCheckTag = "leak-check-latest"
	if err := offShape.Validate(); err != nil {
		t.Fatalf("a hello naming the off-shape tag %q was refused: %v", offShape.LeakCheckTag, err)
	}
	noTag := theLeakCheckHello
	noTag.LeakCheckInstalled = true
	if err := noTag.Validate(); !errors.Is(err, ErrMalformedFrame) || !strings.Contains(err.Error(), "no leakCheckTag") {
		t.Fatalf("a hello installed under no tag validated as %v, want ErrMalformedFrame naming the tag", err)
	}
}

// A launcher's hello at the largest its fields reach — the daemon's 64-hex token, a 63-character
// sandbox name, the longest role, a pod UID, the launcher's 32-hex id — with a 64-character tag
// installed fits the listener's pre-newline bound.
func TestALauncherHelloWithALongLeakCheckTagFitsTheHelloBound(t *testing.T) {
	tag := "leak-check-v1" + strings.Repeat("9", 64-len("leak-check-v1"))
	if len(tag) != 64 || !ValidLeakCheckTag(tag) {
		t.Fatalf("the tag under test is %q, %d bytes, want a valid one of 64", tag, len(tag))
	}
	line, err := json.Marshal(LauncherHello{
		Token: strings.Repeat("f", 64), Sandbox: strings.Repeat("s", 63), Role: "implementer",
		PodUID: "0b9f7e51-0c6e-4d3e-9f11-3a4c8f5e2d10", LauncherID: strings.Repeat("a", 32),
		LeakCheckTag: tag, LeakCheckInstalled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(line)+1 > MaxHelloBytes {
		t.Fatalf("a launcher_hello with a 64-character tag is %d bytes; MaxHelloBytes is %d", len(line)+1, MaxHelloBytes)
	}
}

// The published tag shape: a version from 1 up, no leading zero, nothing around it.
func TestTheLeakCheckTagShape(t *testing.T) {
	for _, tag := range []string{"leak-check-v1", "leak-check-v15", "leak-check-v1000"} {
		if !ValidLeakCheckTag(tag) {
			t.Errorf("%q is refused", tag)
		}
	}
	for _, tag := range []string{"", "leak-check-v0", "leak-check-v015", "leak-check-v", "leak-check-latest", "v15", "leak-check-v15\n", " leak-check-v15", "leak-check-v15-rc1", "refs/tags/leak-check-v15"} {
		if ValidLeakCheckTag(tag) {
			t.Errorf("%q is taken", tag)
		}
	}
}

// Every form of a slice the launcher could not place, or would hold too much for, is refused
// before the launcher keeps anything of it.
func TestALauncherLeakCheckRefusesItsMalformedForms(t *testing.T) {
	good := LauncherLeakCheck{
		ID: "leak-check-leak-check-v15-2c26b46b68ff", Index: 1, Count: 2, Tag: "leak-check-v15",
		SHA256: "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae", Payload: []byte("foo"),
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("the good slice: %v", err)
	}
	largest := good
	largest.Count = MaxWorkspaceBundleBodyBytes / LauncherBundlePayloadBytes
	largest.Payload = make([]byte, LauncherBundlePayloadBytes)
	if err := largest.Validate(); err != nil {
		t.Fatalf("a full slice of a sequence at the bound: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*LauncherLeakCheck)
		want   string
	}{
		{"no id", func(f *LauncherLeakCheck) { f.ID = "" }, "no id"},
		{"a count of none", func(f *LauncherLeakCheck) { f.Count = 0 }, "count is 0"},
		{"a negative count", func(f *LauncherLeakCheck) { f.Count = -1 }, "count is -1"},
		{"a count whose slices pass the body bound", func(f *LauncherLeakCheck) {
			f.Count = MaxWorkspaceBundleBodyBytes/LauncherBundlePayloadBytes + 1
		}, "could pass"},
		{"a negative index", func(f *LauncherLeakCheck) { f.Index = -1 }, "index -1 of 2"},
		{"an index at the count", func(f *LauncherLeakCheck) { f.Index = 2 }, "index 2 of 2"},
		{"no tag", func(f *LauncherLeakCheck) { f.Tag = "" }, "tag"},
		{"a tag of another shape", func(f *LauncherLeakCheck) { f.Tag = "leak-check-latest" }, "leak-check-latest"},
		{"no payload", func(f *LauncherLeakCheck) { f.Payload = nil }, "no payload"},
		{"a payload over a slice", func(f *LauncherLeakCheck) { f.Payload = make([]byte, LauncherBundlePayloadBytes+1) }, "limit"},
		{"no digest", func(f *LauncherLeakCheck) { f.SHA256 = "" }, "sha256"},
		{"an upper-case digest", func(f *LauncherLeakCheck) { f.SHA256 = strings.ToUpper(good.SHA256) }, "sha256"},
		{"a short digest", func(f *LauncherLeakCheck) { f.SHA256 = good.SHA256[:63] }, "sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := good
			tc.change(&frame)
			err := frame.Validate()
			if !errors.Is(err, ErrMalformedFrame) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want ErrMalformedFrame naming %q", err, tc.want)
			}
		})
	}
}

// The daemon acts on an answer only when it names its slice and, for a refusal, the cause the
// start is refused with.
func TestALauncherLeakCheckResultRefusesWhatTheDaemonCouldNotActOn(t *testing.T) {
	const id = "leak-check-leak-check-v15-2c26b46b68ff"
	for _, good := range []LauncherLeakCheckResult{
		{ID: id, Index: 0, OK: true},
		{ID: id, Index: 1, OK: true, Installed: true},
		{ID: id, Index: 1, Error: "the shared public leak check could not be installed: the served check could not be run: exit status 2"},
	} {
		if err := good.Validate(); err != nil {
			t.Errorf("%+v: %v", good, err)
		}
	}
	for _, tc := range []struct {
		name  string
		frame LauncherLeakCheckResult
		want  string
	}{
		{"no id", LauncherLeakCheckResult{Index: 0, OK: true}, "no id"},
		{"a negative index", LauncherLeakCheckResult{ID: id, Index: -1, OK: true}, "index is -1"},
		{"a refusal with no cause", LauncherLeakCheckResult{ID: id, Index: 0}, "without a cause"},
		{"a refused install", LauncherLeakCheckResult{ID: id, Index: 0, Installed: true, Error: "x"}, "says it made"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.frame.Validate()
			if !errors.Is(err, ErrMalformedFrame) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want ErrMalformedFrame naming %q", err, tc.want)
			}
		})
	}
}

// The launcher reads one frame per line, so the packed check is sliced under the frame limit:
// 300 KiB, over one slice, crosses as two, each a valid frame of the one sequence named by the tag
// and the digest, which rebuild the body the digest is of.
func TestALeakCheckBodySlicesIntoFramesUnderTheLineLimit(t *testing.T) {
	body := bytes.Repeat([]byte("\x1f\x8b\x08\x00leak"), (300<<10)/8)
	if len(body) != 300<<10 {
		t.Fatalf("the body under test is %d bytes, want %d", len(body), 300<<10)
	}
	frames, err := SplitLeakCheck("leak-check-v15", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("a %d-byte body sliced into %d frames, want 2", len(body), len(frames))
	}
	digest := BundleDigest(body)
	var rebuilt []byte
	for index, frame := range frames {
		if err := frame.Validate(); err != nil {
			t.Fatalf("slice %d: %v", index, err)
		}
		if frame.Index != index || frame.Count != 2 || frame.Tag != "leak-check-v15" || frame.SHA256 != digest ||
			frame.ID != "leak-check-leak-check-v15-"+digest[:12] {
			t.Fatalf("slice %d is %d of %d of %s under %q declaring %s", index, frame.Index, frame.Count, frame.Tag, frame.ID, frame.SHA256)
		}
		line, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		if len(line) >= MaxFrameBytes {
			t.Fatalf("slice %d encodes to %d bytes, which the launcher's reader refuses", index, len(line))
		}
		decoded, err := Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded, frame) {
			t.Fatalf("slice %d decoded to another frame", index)
		}
		rebuilt = append(rebuilt, frame.Payload...)
	}
	if !bytes.Equal(rebuilt, body) {
		t.Fatalf("the slices rebuild %d bytes, not the %d-byte body", len(rebuilt), len(body))
	}
	if got := BundleDigest(rebuilt); got != digest {
		t.Fatalf("the slices rebuild to %s, not the %s they declared", got, digest)
	}
	// The same check pushed again is the same sequence; another body at that tag is another.
	again, err := SplitLeakCheck("leak-check-v15", body)
	if err != nil {
		t.Fatal(err)
	}
	other, err := SplitLeakCheck("leak-check-v15", append(bytes.Clone(body), 'x'))
	if err != nil {
		t.Fatal(err)
	}
	if again[0].ID != frames[0].ID || other[0].ID == frames[0].ID {
		t.Fatalf("the same check sliced to %q and another to %q, want only the first to repeat %q", again[0].ID, other[0].ID, frames[0].ID)
	}
}

// A sequence the launcher would refuse is not built: an off-shape tag, an empty body, a body over
// the sequence's bound.
func TestSplitLeakCheckRefusesWhatTheLauncherWouldRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		tag  string
		body []byte
	}{
		{"a tag of another shape", "leak-check-latest", []byte("foo")},
		{"no body", "leak-check-v15", nil},
		{"a body over the bound", "leak-check-v15", make([]byte, MaxWorkspaceBundleBodyBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if frames, err := SplitLeakCheck(tc.tag, tc.body); !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("SplitLeakCheck = %d frames, %v, want ErrMalformedFrame", len(frames), err)
			}
		})
	}
	frames, err := SplitLeakCheck("leak-check-v15", make([]byte, MaxWorkspaceBundleBodyBytes))
	if err != nil || len(frames) != MaxWorkspaceBundleBodyBytes/LauncherBundlePayloadBytes {
		t.Fatalf("a body at the bound sliced into %d frames, %v", len(frames), err)
	}
	if err := frames[len(frames)-1].Validate(); err != nil {
		t.Fatalf("the last slice of a body at the bound: %v", err)
	}
}
