package docs

import (
	"log"
	"testing"
)

// A test that captures the default logger must give the log package its output and flags back
// when it ends: slog.SetDefault points log's output at the capturing handler and clears its
// flags, and setting the previous slog default back undoes neither, so every log.Print after
// the test would write into its finished buffer instead of stderr (internal/logtest).
func TestCaptureWarningsGivesTheLogPackageItsOutputBack(t *testing.T) {
	// Not parallel: it reads the log package's output and flags (log.Writer, log.Flags) around a
	// subtest that swaps the default logger (captureWarnings).
	output, flags := log.Writer(), log.Flags()
	t.Run("capture warnings", func(t *testing.T) {
		captureWarnings(t)
	})
	if log.Writer() != output || log.Flags() != flags {
		t.Errorf("after captureWarnings ended, log writes to %T with flags %d; want %T with flags %d", log.Writer(), log.Flags(), output, flags)
	}
}
