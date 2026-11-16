package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

// This test binary links no module of testcontainers-go. Many tests here start it again as
// `legion` (TestMain), and each start pays every package init linked into it; testcontainers-go's
// walks every process on the machine, ten seconds and a gigabyte of allocation on a box running
// 25,000 of them, so one `legion gh` start cost what that init cost, and a test of twenty-two such
// calls outlasted its deadline. So the nkey seeds these tests hand `legion` come from
// internal/testnkeys, not internal/testnats, and no test here starts a NATS server. It reads the
// modules the binary was built from, as `go version -m` prints them.
func TestThisTestBinaryLinksNoTestcontainers(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("this test binary carries no build information to read its modules from")
	}
	if len(info.Deps) == 0 {
		t.Fatal("this test binary's build information lists no module, so it cannot say what is linked")
	}
	for _, dep := range info.Deps {
		if strings.HasPrefix(dep.Path, "github.com/testcontainers/") {
			t.Errorf("this test binary links %s %s, and every start of it as `legion` pays that module's package init; `go list -deps -test -f '{{.ImportPath}} {{.Imports}}' ./cmd/legion/` names the package that imports it", dep.Path, dep.Version)
		}
	}
}
