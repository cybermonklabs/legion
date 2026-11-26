//go:build !unix

// packages/envoy/cmd/cipher/proxy_trust_read_other.go
package main

import "os"

// readTrustSource is what the file at path holds, for the bundle writer, read whole as
// os.ReadFile reads it. agent-secrets ships for macOS and Linux (proxy_trust_read_unix.go); this
// keeps the package building elsewhere.
func readTrustSource(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// openNoWait opens path for reading and answers whether what it opened is a regular file, and
// its size. Off unix no path names a named pipe an open could wait on.
func openNoWait(path string) (*os.File, bool, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, false, 0, err
	}
	return f, info.Mode().IsRegular(), info.Size(), nil
}

// openDirNoWait opens the directory dir for reading its entries.
func openDirNoWait(dir string) (*os.File, error) {
	return os.Open(dir)
}
