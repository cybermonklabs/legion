package testbin

import (
	"os/exec"
	"testing"
)

// The stand-ins' shell encoder writes what FileURL does, byte for byte, for paths that hold each
// character a temporary directory's name may: a space, a '?', a quote, the rest of the URL path
// set, a control byte and a non-ASCII letter.
func TestFileURLShellEncodesAsFileURLDoes(t *testing.T) {
	for _, path := range []string{
		"/a/plain/omp.js",
		"/a/tmp with space/x",
		"/a/tmp-q?x/y",
		"/a/tmp-sq'x/z",
		"/a/100%/\"q\"#<>`{}\\b",
		"/a/caf\u00e9/t\tab",
	} {
		got, err := exec.Command("sh", "-c", FileURLShell+`url "$1"`, "sh", path).Output()
		if err != nil {
			t.Fatalf("url %q: %v", path, err)
		}
		if want := FileURL(path); string(got) != want {
			t.Errorf("url %q = %q, want FileURL's %q", path, got, want)
		}
	}
}

// FileURL percent-encodes what url.pathToFileURL does, measured on the pinned Oh My Pi: a directory
// named `tmp with space` comes back as `tmp%20with%20space`, and a '?' as %3F.
func TestFileURLEncodesTheURLPathSet(t *testing.T) {
	for path, want := range map[string]string{
		"/a/tmp with space/x": "file:///a/tmp%20with%20space/x",
		"/a/tmp-q?x/y":        "file:///a/tmp-q%3Fx/y",
		"/a/tmp-sq'x/z":       "file:///a/tmp-sq'x/z",
	} {
		if got := FileURL(path); got != want {
			t.Errorf("FileURL(%q) = %q, want %q", path, got, want)
		}
	}
}
