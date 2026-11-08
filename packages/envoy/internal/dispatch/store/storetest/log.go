package storetest

import (
	"github.com/sjawhar/envoy/internal/logtest"
)

// LockedLog is a log sink that the goroutines of the code under test write while the test reads
// it, such as the handler a test installs with slog.SetDefault. It is logtest.Buffer, which
// carries Lines, under the name this package's twenty-five importers already use.
type LockedLog = logtest.Buffer
