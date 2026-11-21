package volumestat

import (
	"runtime"
	"strings"
	"testing"
)

// Read is one statfs on Linux and a refusal naming the platform anywhere else, so this case reads
// whichever twin the binary was built with. On Linux it reads a real filesystem, the test's own
// temporary directory, and holds the reading to what statfs can answer: a volume of some size,
// with neither the used nor the available bytes more than it holds. repocache's
// TestTheCacheReadsItsVolume holds the figures themselves to what `stat -f` prints.
func TestReadAnswersOneReadingOfTheFilesystemItIsGiven(t *testing.T) {
	dir := t.TempDir()
	reading, err := Read(dir)
	if runtime.GOOS != "linux" {
		if err == nil {
			t.Fatalf("Read(%s) answered %+v on %s, want a refusal naming the platform", dir, reading, runtime.GOOS)
		}
		if !strings.Contains(err.Error(), runtime.GOOS) {
			t.Errorf("Read refused with %q, want the platform %q named in it", err, runtime.GOOS)
		}
		return
	}
	if err != nil {
		t.Fatalf("Read(%s): %v", dir, err)
	}
	if reading.SizeBytes == 0 {
		t.Fatalf("Read(%s) = %+v, want a filesystem of some size", dir, reading)
	}
	if reading.UsedBytes > reading.SizeBytes || reading.AvailableBytes > reading.SizeBytes {
		t.Errorf("Read(%s) = %+v, want used and available bytes each within the volume's size", dir, reading)
	}
}
