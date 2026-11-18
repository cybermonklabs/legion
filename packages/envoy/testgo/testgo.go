// Package testgo runs a go command a test starts, so that what go writes on its standard error
// reaches the test's output as go writes it. A go command whose output a test only buffers shows
// nothing until it returns, and a module download that stalls into the test's timeout never
// returns: the timeout's panic loses the buffer, and the log does not name the download.
//
// It is an ordinary package rather than a _test.go file so every test package can import it, and
// the helpers that build or list for a test (internal/cmdtest's runBuild, the daemon's testbuild
// and testtypes) too; Go cannot share test-only code across packages. It is public, as
// testnatsimages is, so that a module outside github.com/sjawhar/envoy (packages/daemon) runs its
// go commands through this one helper: Go's internal-package rule keeps that module from importing
// an internal package of this one.
package testgo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/sjawhar/envoy/internal/logtest"
)

// Output runs cmd, a go command a test starts, and returns its standard output. go's standard
// error also goes to this process's as go writes it. A cmd.Stderr already set gets the stream too;
// cmd.Stdout must be unset. When go fails, the error carries what go wrote on both streams, as
// each arrived.
func Output(cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("testgo.Output: cmd.Stdout is set, and Output returns standard output itself")
	}
	var stdout bytes.Buffer
	// Both of go's streams, which exec copies on two goroutines.
	output := &logtest.Buffer{}
	stderr := []io.Writer{os.Stderr, output}
	if cmd.Stderr != nil {
		stderr = append(stderr, cmd.Stderr)
	}
	cmd.Stdout, cmd.Stderr = io.MultiWriter(&stdout, output), io.MultiWriter(stderr...)
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%w\n%s", err, output.String())
	}
	return stdout.Bytes(), nil
}
