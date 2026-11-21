package docs

import (
	"io/fs"
	"math"
	"testing"
)

// The memory the process may use is the Go memory limit where GOMEMLIMIT sets one, whatever the
// container's cgroup says; else the cgroup's limit, from cgroup v2's memory.max and, on a box with
// no such file, cgroup v1's memory.limit_in_bytes; else none. v2's "max", v1's page-rounded
// math.MaxInt64 and an absent file each say there is no limit. The files are read through the
// function the test hands in, so each row is a box of its own.
func TestTheMemoryLimitIsGOMEMLIMITElseTheContainersCgroupElseNone(t *testing.T) {
	const twentyGiB = "21474836480\n"
	for _, test := range []struct {
		name    string
		goLimit int64
		files   map[string]string
		want    memoryLimit
	}{
		{"GOMEMLIMIT beside a cgroup v2 limit", 8 << 30, map[string]string{cgroupV2MemoryMax: twentyGiB}, memoryLimit{8 << 30, memoryLimitGo}},
		{"GOMEMLIMIT beside a cgroup v1 limit", 8 << 30, map[string]string{cgroupV1MemoryLimit: twentyGiB}, memoryLimit{8 << 30, memoryLimitGo}},
		{"GOMEMLIMIT with no cgroup", 8 << 30, nil, memoryLimit{8 << 30, memoryLimitGo}},
		{"a cgroup v2 limit", math.MaxInt64, map[string]string{cgroupV2MemoryMax: twentyGiB}, memoryLimit{20 << 30, memoryLimitCgroupV2}},
		{"cgroup v2's max", math.MaxInt64, map[string]string{cgroupV2MemoryMax: "max\n"}, memoryLimit{math.MaxInt64, memoryLimitNone}},
		{"cgroup v2's max beside a v1 file", math.MaxInt64, map[string]string{cgroupV2MemoryMax: "max\n", cgroupV1MemoryLimit: twentyGiB}, memoryLimit{math.MaxInt64, memoryLimitNone}},
		{"a cgroup v1 limit", math.MaxInt64, map[string]string{cgroupV1MemoryLimit: twentyGiB}, memoryLimit{20 << 30, memoryLimitCgroupV1}},
		{"cgroup v1's no limit", math.MaxInt64, map[string]string{cgroupV1MemoryLimit: "9223372036854771712\n"}, memoryLimit{math.MaxInt64, memoryLimitNone}},
		{"no cgroup file", math.MaxInt64, nil, memoryLimit{math.MaxInt64, memoryLimitNone}},
	} {
		readFile := func(name string) ([]byte, error) {
			text, ok := test.files[name]
			if !ok {
				return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
			}
			return []byte(text), nil
		}
		if got := processMemoryLimit(test.goLimit, readFile); got != test.want {
			t.Errorf("the memory limit with %s = %d bytes from %s, want %d bytes from %s", test.name, got.bytes, got.source, test.want.bytes, test.want.source)
		}
	}
}

// The peerless rooms' budget follows the memory the process may use: a quarter of it, at least
// 256 MiB and at most 8 GiB, and 256 MiB when no limit is known (math.MaxInt64). The rows between
// the floor and the cap are the fraction's own (roomBudgetDivisor), and change with it.
func TestTheRoomBudgetFollowsTheMemoryLimit(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit int64
		want  int64
	}{
		{"no limit", math.MaxInt64, 256 << 20},
		{"512 MiB, the floor", 512 << 20, 256 << 20},
		{"1 GiB, a quarter at the floor", 1 << 30, 256 << 20},
		{"2 GiB, a quarter", 2 << 30, 512 << 20},
		{"20 GiB, production's quarter", 20 << 30, 5 << 30},
		{"32 GiB, a quarter at the cap", 32 << 30, 8 << 30},
		{"64 GiB, the cap", 64 << 30, 8 << 30},
	} {
		if got := roomBudgetFor(test.limit); got != test.want {
			t.Errorf("the room budget under %s = %d MiB, want %d MiB", test.name, got>>20, test.want>>20)
		}
	}
}
