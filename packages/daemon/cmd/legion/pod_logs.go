package main

import (
	"io"
	"log"

	"github.com/sjawhar/legion/daemon/internal/redact"
)

// podLogs points Go's standard logger at stderr through the URL rule (redact.Writer) for the run
// of the worker's own processes, in a tmux pane or a pod (`legion worker-shim`, `legion
// claude-bridge`), and of a pod's own (`legion launcher`, `legion workspace-init`), and answers
// the func that puts back the writer the logger had, which the command defers: a test runs every
// command in one process. The standard logger is where Go's net/http writes the errors of a server
// that names no ErrorLog (the launcher's metadata endpoint), and where slog's default logger
// writes; such a line can quote a URL, and the process's stderr is its log.
// The logger's flags stay as they are.
func podLogs(stderr io.Writer) (restore func()) {
	previous := log.Writer()
	log.SetOutput(redact.Writer(stderr))
	return func() { log.SetOutput(previous) }
}
