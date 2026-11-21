//go:build memory

package main

import (
	"cmp"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Check c28 of INFRA-1588 compares production Dispatch's heap at the two ends of a window of at
// least 12 hours on one pod, in sets read from the pod's profiler: a heap profile after a
// collection (/debug/pprof/heap?gc=1) and the document rooms' figures (/debug/vars,
// dispatch_rooms). It compared the two kept profiles with go tool pprof -sample_index=inuse_space;
// compareC28Sets is the same method in Go, through the library go tool pprof is built on:
//
//   - the heap outside the rooms and the read cache is the inuse_space of the samples no frame of
//     which matches reearth/ygo or coldRead (pprof's -ignore), and it may grow by at most 2 MB an
//     hour;
//   - the rooms' part is the inuse_space of the samples a frame of which matches reearth/ygo
//     (pprof's -focus), and divided by dispatch_rooms' resident_bytes it stays under twice its
//     value at the first end;
//   - a growth past the bound is traced from the five functions whose flat heap outside the rooms
//     and the read cache grew most (pprof's -top -diff_base).

const (
	// c28Window is the window over which c28 compared production's heap, and c28Attachment the
	// comparison it attached to the issue.
	c28Window     = "2026-11-19T18:02:06Z to 2026-11-20T15:11:12Z"
	c28Attachment = "dispatch://INFRA-1588/artifact/c28-comparison-20261120-txt"
	// c28GrowthPerHour is the most the heap outside the rooms and the read cache may grow in an
	// hour, in bytes: 2 MB.
	c28GrowthPerHour = 2_000_000
	// c28RatioRise is how many times its value at the first end the rooms' part divided by
	// resident_bytes stays under.
	c28RatioRise = 2
	// c28Traced is how many of the functions whose flat heap grew most a growth is traced from.
	c28Traced = 5
)

var (
	// c28LeftOut matches a frame of the rooms (ygo's) or of the read cache (coldRead): the heap
	// c28 bounds leaves out every sample with one.
	c28LeftOut = regexp.MustCompile(`reearth/ygo|coldRead`)
	// c28Rooms matches a frame of the rooms: the samples with one are the rooms' part.
	c28Rooms = regexp.MustCompile(`reearth/ygo`)
)

// c28Leaks holds a test for each leak c28 named in production, under the leak's name (check c31).
// c28 compared production's heap over 2026-11-19T18:02:06Z to 2026-11-20T15:11:12Z and named none:
// the heap outside the rooms and the read cache shrank by 11.47 MB, the rooms' part divided by
// resident_bytes stayed under twice its first value, and each cache with a bound stayed within it.
//
// A leak c28 names is one row here. Its test starts a server with the memory harness
// (newMemoryHarness, memoryHarness.start, memoryServer.profiler), drives the leak's code path on it
// many times, until every cache that path fills is full, takes a set (takeC28Set), drives the path
// as many times again, takes a second set, and fails unless the flat heap of the function c28
// named (c28Set.flatOutside, or c28Set.inuse with c28Rooms for a leak of the rooms) grew by at
// most 1 MB between the two. At the commit before the leak's fix the test fails: that function's
// heap grows with each pass.
var c28Leaks []struct {
	name string
	test func(t *testing.T)
}

// Each leak c28 named in production is fixed, and a test that drives its path holds the heap of the
// function c28 named flat (c28Leaks). c28 named no leak, so the test holds no subtest, and says so.
func TestTheLeaksOfC28AreFixed(t *testing.T) {
	if len(c28Leaks) == 0 {
		t.Logf("c28 named no leak in production's heap over %s (%s), so this test holds no subtest", c28Window, c28Attachment)
		return
	}
	for _, leak := range c28Leaks {
		t.Run(leak.name, leak.test)
	}
}

const (
	// plantedRooms is how many rooms the first subtest below opens and closes between its two sets,
	// each of a document of plantedRoomParagraphs paragraphs.
	plantedRooms          = 12
	plantedRoomParagraphs = 4_000
	// plantedRequests is how many requests the second subtest below sends between its two sets.
	plantedRequests = 32
)

// c28's method finds a leak where a server holds one (c28's negative control). Each subtest runs a
// real server with the test hooks and a hook that plants a leak, takes a set from its profiler as
// production's profiler serves one, drives the leak's path, takes a second set, and compares the
// two as c28 does (compareC28Sets):
//
//   - with a hook that keeps every closed room's document (DISPATCH_TEST_KEEP_CLOSED_ROOMS), the
//     rooms' part divided by resident_bytes rises past twice its first value;
//   - with a hook that stores 1 MB for each request in a package-level map that nothing prunes
//     (DISPATCH_TEST_KEEP_REQUEST_MEGABYTES), the heap outside the rooms and the read cache grows by
//     more than 2 MB an hour over the time between the sets, and the hook's function is among the
//     five whose flat heap grew most.
func TestTheC28MethodFindsAPlantedLeak(t *testing.T) {
	t.Run("a hook that keeps every closed room's document", func(t *testing.T) {
		memory := newMemoryHarness(t)
		// The documents are written by a server of their own, stopped before the measured one
		// starts, so the measured server holds no room the test did not open.
		writer := memory.start(t)
		artifactIDs := make([]string, plantedRooms+1)
		for number := range artifactIDs {
			upload := writer.upload(t, "json", memory.issue, plantedRoomMarkdown(number))
			if upload.status != http.StatusCreated {
				t.Fatalf("upload room %d's document: status %d body %.300s, want 201", number, upload.status, upload.body)
			}
			memory.waitForSettlement(t, upload)
			artifactIDs[number] = upload.artifactID
		}
		writer.stop(t)

		server := startPlanted(t, memory, "DISPATCH_TEST_KEEP_CLOSED_ROOMS")
		profiler := server.profiler(t)
		// One room stays open from before the first set until after the second, as production
		// always holds rooms, so resident_bytes counts a room at each end.
		server.holdRoom(t, artifactIDs[0])
		waitForResidentRooms(t, profiler, 1)
		start := takeC28Set(t, profiler)
		for _, artifactID := range artifactIDs[1:] {
			if err := server.openRoom(t, artifactID).Close(); err != nil {
				t.Fatalf("close the socket of the room of %s: %v", artifactID, err)
			}
		}
		// ygo retires a room a minute after its last browser left it.
		waitForResidentRooms(t, profiler, 1)
		end := takeC28Set(t, profiler)

		comparison := compareC28Sets(t, start, end)
		kept := strings.Count(server.output.String(), `msg="dispatch: keeping a closed room's document for a test"`)
		t.Logf("the rooms' part: %d B over resident_bytes %d B at the start, %d B over %d B at the end, %v later; the hook kept %d documents",
			comparison.roomsPart[0], comparison.residentBytes[0], comparison.roomsPart[1], comparison.residentBytes[1], comparison.elapsed.Round(time.Second), kept)
		if comparison.residentBytes[0] <= 0 || comparison.residentBytes[1] <= 0 {
			t.Fatalf("resident_bytes is %d B at the start and %d B at the end with a room held open, want that room's bytes at each", comparison.residentBytes[0], comparison.residentBytes[1])
		}
		if before, after := comparison.roomsRatio(0), comparison.roomsRatio(1); after <= c28RatioRise*before {
			t.Errorf("with every closed room's document kept, the rooms' part divided by resident_bytes went from %.1f to %.1f, want past %d times its first value: c28's method misses a leak of the rooms", before, after, c28RatioRise)
		}
	})

	t.Run("a hook that stores 1 MB for each request in a package-level map that nothing prunes", func(t *testing.T) {
		memory := newMemoryHarness(t)
		server := startPlanted(t, memory, "DISPATCH_TEST_KEEP_REQUEST_MEGABYTES")
		profiler := server.profiler(t)
		start := takeC28Set(t, profiler)
		for range plantedRequests {
			server.get(t, "/api/v1/issues/"+memory.issue)
		}
		end := takeC28Set(t, profiler)

		comparison := compareC28Sets(t, start, end)
		hook := runtime.FuncForPC(reflect.ValueOf(storeRequestMegabyte).Pointer()).Name()
		t.Logf("the heap outside the rooms and the read cache grew %d B in %v, against a bound of %d B; the %d functions whose flat heap grew most: %v",
			comparison.outsideGrowth, comparison.elapsed.Round(time.Millisecond), comparison.growthBound, c28Traced, comparison.grewMost)
		if comparison.outsideGrowth <= comparison.growthBound {
			t.Errorf("with 1 MB stored for each of %d requests, the heap outside the rooms and the read cache grew %d B in %v, want past the bound of 2 MB an hour, %d B: c28's method misses a leak outside the rooms",
				plantedRequests, comparison.outsideGrowth, comparison.elapsed, comparison.growthBound)
		}
		if !slices.ContainsFunc(comparison.grewMost, func(growth functionGrowth) bool { return growth.function == hook }) {
			t.Errorf("the %d functions whose flat heap grew most are %v, want the hook's %s among them", c28Traced, comparison.grewMost, hook)
		}
	})
}

// startPlanted starts a server of memory's with the test hooks, the hook the variable hook names
// set to 1, and its profiler on a port the kernel chooses (memoryServer.profiler).
func startPlanted(t *testing.T, memory *memoryHarness, hook string) *memoryServer {
	t.Helper()
	memory.env = []string{"DISPATCH_TEST_HOOKS=1", hook + "=1", "DISPATCH_PPROF_PORT=0"}
	defer func() { memory.env = nil }()
	return memory.start(t)
}

// plantedRoomMarkdown is the document of room number of the first subtest above:
// plantedRoomParagraphs paragraphs of prose, about 250 KB.
func plantedRoomMarkdown(number int) string {
	var markdown strings.Builder
	for paragraph := range plantedRoomParagraphs {
		fmt.Fprintf(&markdown, "Room %d, paragraph %d, holds the words a person wrote in it.\n\n", number, paragraph)
	}
	return markdown.String()
}

// holdRoom opens the document's room (openRoom) and holds it open until the test ends, reading and
// dropping whatever the room sends, as a browser's tab left open on the document does.
func (p *memoryServer) holdRoom(t *testing.T, artifactID string) {
	t.Helper()
	connection := p.openRoom(t, artifactID)
	read := make(chan struct{})
	go func() {
		defer close(read)
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = connection.Close()
		<-read
	})
}

// waitForResidentRooms waits until dispatch_rooms, read from the profiler at profiler, counts want
// resident rooms. Three minutes is ygo's idle minute, after which it retires a room no browser is
// in, and its sweeps after it, with time to spare.
func waitForResidentRooms(t *testing.T, profiler string, want int64) {
	t.Helper()
	testwait.WithinPolling(t, 3*time.Minute, time.Second, fmt.Sprintf("dispatch_rooms to count %d resident rooms", want), func() bool {
		return roomFigures(t, profiler).ResidentRooms == want
	})
}

// c28Set is one set of c28's, read from a server's profiler as production's profiler serves it:
// the heap profile after a collection, the rooms' figures, and when it was taken.
type c28Set struct {
	at    time.Time
	heap  *profile.Profile
	rooms docs.RoomFigures
}

// takeC28Set takes a set from the profiler at profiler (memoryServer.profiler).
func takeC28Set(t *testing.T, profiler string) c28Set {
	t.Helper()
	at := time.Now()
	response, err := http.Get("http://" + profiler + "/debug/pprof/heap?gc=1")
	if err != nil {
		t.Fatalf("GET /debug/pprof/heap?gc=1 on the server's profiler: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /debug/pprof/heap?gc=1 on the server's profiler answered %d, want a heap profile", response.StatusCode)
	}
	heap, err := profile.Parse(response.Body)
	if err != nil {
		t.Fatalf("parse the server's heap profile: %v", err)
	}
	return c28Set{at: at, heap: heap, rooms: roomFigures(t, profiler)}
}

// kept is the set's heap samples a frame matching focus holds (every sample, for a nil focus) and
// no frame matching ignore holds (none left out, for a nil ignore), as go tool pprof's -focus and
// -ignore keep them, and the index of inuse_space among each sample's values.
func (s c28Set) kept(t *testing.T, focus, ignore *regexp.Regexp) ([]*profile.Sample, int) {
	t.Helper()
	heap := s.heap.Copy()
	heap.FilterSamplesByName(focus, ignore, nil, nil)
	index, err := heap.SampleIndexByName("inuse_space")
	if err != nil {
		t.Fatalf("the server's heap profile: %v", err)
	}
	return heap.Sample, index
}

// inuse is the inuse_space, in bytes, of the samples kept keeps: the bytes go tool pprof
// -sample_index=inuse_space -top -focus -ignore accounts for.
func (s c28Set) inuse(t *testing.T, focus, ignore *regexp.Regexp) int64 {
	t.Helper()
	samples, index := s.kept(t, focus, ignore)
	var bytes int64
	for _, sample := range samples {
		bytes += sample.Value[index]
	}
	return bytes
}

// flatOutside is the inuse_space outside the rooms and the read cache by the function that
// allocated it, the innermost frame of each sample's stack: pprof's flat, by function.
func (s c28Set) flatOutside(t *testing.T) map[string]int64 {
	t.Helper()
	samples, index := s.kept(t, nil, c28LeftOut)
	flat := make(map[string]int64)
	for _, sample := range samples {
		flat[allocatingFunction(sample)] += sample.Value[index]
	}
	return flat
}

// allocatingFunction names the innermost frame of sample's stack as pprof names it: its function,
// an inlined one included, or its address where the profile names no function.
func allocatingFunction(sample *profile.Sample) string {
	if len(sample.Location) == 0 {
		return ""
	}
	location := sample.Location[0]
	if len(location.Line) == 0 || location.Line[0].Function == nil {
		return fmt.Sprintf("%#x", location.Address)
	}
	return location.Line[0].Function.Name
}

// c28Comparison is what compareC28Sets finds between a set at each end.
type c28Comparison struct {
	// elapsed is the time from the first set to the second, and growthBound what the heap outside
	// the rooms and the read cache may grow in it (c28GrowthPerHour).
	elapsed     time.Duration
	growthBound int64
	// outsideGrowth is how much the heap outside the rooms and the read cache grew.
	outsideGrowth int64
	// roomsPart and residentBytes are the rooms' part and dispatch_rooms' resident_bytes at each
	// end, the first set's first.
	roomsPart     [2]int64
	residentBytes [2]int64
	// grewMost is the c28Traced functions whose flat heap outside the rooms and the read cache grew
	// most, most first.
	grewMost []functionGrowth
}

// roomsRatio is the rooms' part divided by resident_bytes at end, 0 for the first set and 1 for
// the second.
func (c c28Comparison) roomsRatio(end int) float64 {
	return float64(c.roomsPart[end]) / float64(c.residentBytes[end])
}

// functionGrowth is how much one function's flat heap grew, in bytes.
type functionGrowth struct {
	function string
	bytes    int64
}

func (g functionGrowth) String() string {
	return fmt.Sprintf("%s %+d B", g.function, g.bytes)
}

// compareC28Sets compares start and end as c28 compares production's two kept sets.
func compareC28Sets(t *testing.T, start, end c28Set) c28Comparison {
	t.Helper()
	elapsed := end.at.Sub(start.at)
	comparison := c28Comparison{
		elapsed:       elapsed,
		growthBound:   int64(c28GrowthPerHour * elapsed.Hours()),
		outsideGrowth: end.inuse(t, nil, c28LeftOut) - start.inuse(t, nil, c28LeftOut),
		roomsPart:     [2]int64{start.inuse(t, c28Rooms, nil), end.inuse(t, c28Rooms, nil)},
		residentBytes: [2]int64{start.rooms.ResidentBytes, end.rooms.ResidentBytes},
	}
	growth := end.flatOutside(t)
	for function, bytes := range start.flatOutside(t) {
		growth[function] -= bytes
	}
	for function, bytes := range growth {
		comparison.grewMost = append(comparison.grewMost, functionGrowth{function: function, bytes: bytes})
	}
	slices.SortFunc(comparison.grewMost, func(a, b functionGrowth) int {
		return cmp.Or(cmp.Compare(b.bytes, a.bytes), strings.Compare(a.function, b.function))
	})
	comparison.grewMost = comparison.grewMost[:min(c28Traced, len(comparison.grewMost))]
	return comparison
}
