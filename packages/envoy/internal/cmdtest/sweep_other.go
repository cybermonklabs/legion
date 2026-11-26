//go:build !unix

package cmdtest

import (
	"os"
	"strconv"
)

// makeProcessDir makes this process's directory of builds, named processDirPrefix, the pid and a
// dash. Here it holds no lock: on this platform no sweep reads one.
func makeProcessDir() (dir string, release func(), err error) {
	dir, err = os.MkdirTemp("", processDirPrefix+strconv.Itoa(os.Getpid())+"-")
	return dir, func() {}, err
}

// removeBuildsOfEndedProcesses removes nothing here: on this platform cmdtest cannot tell whose a
// directory is or whether its process runs, so a directory that go test's alarm left stays.
func removeBuildsOfEndedProcesses() {}
