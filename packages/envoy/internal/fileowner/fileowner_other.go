// packages/envoy/internal/fileowner/fileowner_other.go
//go:build !unix

package fileowner

import "os"

// Of answers no owner on a target that keeps no uid on a file, such as Windows.
func Of(os.FileInfo) (uid int, ok bool) { return 0, false }
