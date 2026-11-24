package main

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testbinary"
)

// This test binary links no module of testcontainers-go. Many tests here start it again as
// `legion` (TestMain), and each start pays every package init linked into it; testcontainers-go's
// walks every process on the machine, ten seconds and a gigabyte of allocation on a box running
// 25,000 of them, so one `legion gh` start cost what that init cost, and a test of twenty-two such
// calls outlasted its deadline. So the nkey seeds these tests hand `legion` come from
// internal/testnkeys, not internal/testnats, and no test here starts a NATS server.
func TestThisTestBinaryLinksNoTestcontainers(t *testing.T) { testbinary.LinksNoTestcontainers(t) }
