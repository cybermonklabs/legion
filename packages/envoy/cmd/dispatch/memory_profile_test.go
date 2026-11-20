//go:build memory

package main

// legion: tool - heap profiles of a cold one-word edit of the heaviest documents and its settlement, for a person who changes the document read or the settlement and must see where its memory goes; run with DISPATCH_EDIT_PROFILE_OUT=<dir> go test -tags memory -count=1 -run '^$' -bench '^BenchmarkProfileTheColdEditOfTheHeaviestDocuments$' -benchtime 1x -timeout 2h ./cmd/dispatch

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Profiling the cold edit (LEGION-504). Not a bound: a measurement the memory tests' servers take
// when DISPATCH_EDIT_PROFILE_OUT names a directory. No CI step passes -bench, so CI never runs it.

const (
	editProfileOutEnv     = "DISPATCH_EDIT_PROFILE_OUT"
	editProfileRepeatsEnv = "DISPATCH_EDIT_PROFILE_REPEATS"
	editProfileShapesEnv  = "DISPATCH_EDIT_PROFILE_SHAPES"
	// heapProfileDirEnv makes a memory test's server sample its live heap: each time the heap
	// grows a step past what the last collection left, it collects and, where the live heap is the
	// most its phase has seen, writes the heap profile as peak-<phase>.pb.gz. The phase is what
	// the test last wrote to the directory's phase file.
	heapProfileDirEnv = "DISPATCH_MEMORY_PROFILE_DIR"
)

func init() {
	dir := os.Getenv(heapProfileDirEnv)
	if os.Getenv(memoryTestServeEnv) != "1" || dir == "" {
		return
	}
	runtime.MemProfileRate = 16 << 10
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		panic(err)
	}
	go sampleLiveHeap(dir)
}

// sampleLiveHeap writes timeline.tsv, the live heap after a forced collection each time the heap
// grows a step past what the last one left, and peak-<phase>.pb.gz, the heap profile at the most
// live heap each phase reached.
func sampleLiveHeap(dir string) {
	samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/gc/heap/live:bytes"}}
	timeline, err := os.Create(filepath.Join(dir, "timeline.tsv"))
	if err != nil {
		panic(err)
	}
	started := time.Now()
	peaks := map[string]uint64{}
	phase := "start"
	var floor uint64
	for {
		time.Sleep(time.Millisecond)
		if current := readPhase(dir); current != phase {
			phase = current
			if phase == "final" {
				runtime.GC()
				writeProfile(filepath.Join(dir, "final.pb.gz"), "heap")
			}
		}
		metrics.Read(samples[:1])
		heap := samples[0].Value.Uint64()
		if heap < floor {
			floor = heap
		}
		if heap < floor+max(8<<20, floor/16) {
			continue
		}
		runtime.GC()
		metrics.Read(samples)
		live := samples[1].Value.Uint64()
		floor = live
		fmt.Fprintf(timeline, "%d\t%s\t%d\t%d\n", time.Since(started).Milliseconds(), phase, heap>>20, live>>20)
		if live > peaks[phase] {
			peaks[phase] = live
			writeProfile(filepath.Join(dir, "peak-"+phase+".pb.gz"), "heap")
			fmt.Fprintf(timeline, "#peak %s %d MiB\n", phase, live>>20)
		}
	}
}

func readPhase(dir string) string {
	phase, err := os.ReadFile(filepath.Join(dir, "phase"))
	if err != nil {
		return "start"
	}
	return strings.TrimSpace(string(phase))
}

func writeProfile(path, name string) {
	file, err := os.Create(path + ".tmp")
	if err != nil {
		panic(err)
	}
	if err := pprof.Lookup(name).WriteTo(file, 0); err != nil {
		panic(err)
	}
	if err := file.Close(); err != nil {
		panic(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		panic(err)
	}
}

// coldEditDocument is a document the profile edits: the heaviest the element limit admits of a
// shape, one of the repository's own, or the generated guide.
type coldEditDocument struct {
	name, markdown, artifactID string
}

// generatedGuideName names generatedGuide among the profile's documents, for
// DISPATCH_EDIT_PROFILE_SHAPES.
const generatedGuideName = "a generated guide"

// generatedGuideBytes is about the length of the longest guides of the repository.
const generatedGuideBytes = 640 << 10

// generatedGuide is a guide in the shape of the repository's long ones - sections of a heading,
// prose with inline code, links and emphasis, a list, a fenced block and now and then a table -
// to about generatedGuideBytes, generated so the document the profile edits is the same at every
// revision, where a guide of the repository changes as the repository does. It weighs about
// 53,000 elements, under pmdoc.MaxDocumentElements, so the server stores it.
func generatedGuide() string {
	var guide strings.Builder
	guide.WriteString("# A generated guide\n\n")
	for step := 1; guide.Len() < generatedGuideBytes; step++ {
		fmt.Fprintf(&guide, "## Step %d\n\n", step)
		fmt.Fprintf(&guide, "The step runs `scripts/step-%d.sh` against the box it is given, and it fails on the first line of its check that does not hold.\n", step)
		fmt.Fprintf(&guide, "It reads [the notes of step %d](#step-%d) before it starts, writes **one report** each run, and keeps every log it took under `$TMPDIR/step-%d`.\n", step, step, step)
		guide.WriteString("A run that stops early names the line it stopped on, and the next run starts again from that line rather than from the top.\n\n")
		guide.WriteString("Each run reads the box's own state first, and a box that is not in the state the step expects is refused before anything is written there.\n")
		guide.WriteString("The refusal names what it read and what it wanted, so the person who runs it can put the box right and run the step again with the same arguments.\n")
		guide.WriteString("Nothing the step writes outlives the run except the report and the logs, which the next run of the same step replaces.\n\n")
		guide.WriteString("- It needs `go`, `jq` and `curl` on `PATH`, and refuses to start without one.\n")
		guide.WriteString("- It reads no credential, and no variable but `STEP_ROUNDS` and `STEP_LOG`.\n\n")
		fmt.Fprintf(&guide, "```sh\nbash scripts/step-%d.sh --box <name>   # step %d: PASS, exit 0\n```\n\n", step, step)
		if step%4 == 0 {
			guide.WriteString("| Variable | Default | What it sets |\n| --- | --- | --- |\n")
			guide.WriteString("| `STEP_ROUNDS` | 3 | how many rounds the run makes |\n")
			guide.WriteString("| `STEP_LOG` | `$TMPDIR/step.log` | where the run's log goes |\n\n")
		}
	}
	return guide.String()
}

// BenchmarkProfileTheColdEditOfTheHeaviestDocuments stores the heaviest documents of every shape
// the element limit admits, some of the repository's own and the generated guide, then makes a
// same-length one-word edit of each on servers that have not loaded it: repeats of the edit and
// of the settlement after it, each peak logged above idle, then one run that samples the server's
// live heap. It is a tool, not a test: DISPATCH_EDIT_PROFILE_OUT, the directory the profiles go
// to, is its input, and it skips without it. DISPATCH_EDIT_PROFILE_REPEATS sets the repeats (3),
// and DISPATCH_EDIT_PROFILE_SHAPES, a comma-separated list of names, keeps only those documents.
// Run it once, with -benchtime 1x: each run of its body is the whole profile.
func BenchmarkProfileTheColdEditOfTheHeaviestDocuments(b *testing.B) {
	out := os.Getenv(editProfileOutEnv)
	if out == "" {
		b.Skip(editProfileOutEnv + " names where the profiles go")
	}
	repeats := 3
	if value := os.Getenv(editProfileRepeatsEnv); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			b.Fatal(err)
		}
		repeats = parsed
	}
	only := map[string]bool{}
	for name := range strings.SplitSeq(os.Getenv(editProfileShapesEnv), ",") {
		if name != "" {
			only[name] = true
		}
	}
	var documents []coldEditDocument
	for _, shape := range heaviestAdmittedShapes(b) {
		documents = append(documents, coldEditDocument{name: shape.name, markdown: withSentinelWord(b, shape.markdown)})
	}
	for _, path := range []string{"docs/superpowers/plans/2026-09-05-dispatch-conversations.md", "packages/envoy/README.md"} {
		text, err := os.ReadFile(filepath.Join("..", "..", "..", "..", path))
		if err != nil {
			b.Fatal(err)
		}
		documents = append(documents, coldEditDocument{name: path, markdown: string(text) + "\n\nThe " + sentinelWord + " ends it.\n"})
	}
	documents = append(documents, coldEditDocument{name: generatedGuideName, markdown: generatedGuide() + "\n\nThe " + sentinelWord + " ends it.\n"})
	memory := newMemoryHarness(b)
	writer := memory.start(b)
	var stored []coldEditDocument
	for _, document := range documents {
		if len(only) > 0 && !only[document.name] {
			continue
		}
		upload := writer.upload(b, "json", memory.issue, document.markdown)
		if upload.status != http.StatusCreated {
			b.Logf("%s: upload answered %d %.300s; not profiled", document.name, upload.status, upload.body)
			continue
		}
		memory.waitForSettlement(b, upload)
		document.artifactID = upload.artifactID
		stored = append(stored, document)
	}
	writer.stop(b)
	words := [2]string{sentinelWord, "wird"}
	edits := map[string]int{}
	edit := func(server *memoryServer, document coldEditDocument) time.Duration {
		find, with := words[edits[document.name]%2], words[(edits[document.name]+1)%2]
		edits[document.name]++
		started := time.Now()
		answer := server.edit(b, document.artifactID, map[string]any{"op": "replace", "find": find, "with": with})
		if answer.status != http.StatusOK {
			b.Fatalf("%s: the edit answered %d %.300s", document.name, answer.status, answer.body)
		}
		return time.Since(started).Round(time.Millisecond)
	}
	for _, document := range stored {
		for run := range repeats {
			server := memory.start(b)
			var elapsed time.Duration
			editPeak := server.peakAboveIdle(b, func() { elapsed = edit(server, document) })
			settlePeak := server.peakAboveIdle(b, func() { memory.waitForSettled(b, document.artifactID) })
			server.stop(b)
			b.Logf("RESULT\t%s\trun %d\tedit %d MiB in %s\tsettlement %d MiB", document.name, run, editPeak>>20, elapsed, settlePeak>>20)
		}
		dir := filepath.Join(out, strings.NewReplacer("/", "_", " ", "_").Replace(document.name))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		setPhase := func(phase string) {
			if err := os.WriteFile(filepath.Join(dir, "phase"), []byte(phase), 0o644); err != nil {
				b.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		setPhase("idle")
		memory.env = []string{heapProfileDirEnv + "=" + dir}
		server := memory.start(b)
		memory.env = nil
		time.Sleep(500 * time.Millisecond)
		setPhase("edit")
		elapsed := edit(server, document)
		setPhase("settle")
		memory.waitForSettled(b, document.artifactID)
		setPhase("done")
		time.Sleep(2 * time.Second)
		setPhase("final")
		time.Sleep(time.Second)
		server.stop(b)
		b.Logf("PROFILED\t%s\tedit in %s\t%s", document.name, elapsed, dir)
	}
}
