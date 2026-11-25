package workflow

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// architectReaderFiles are the prompt files of each architect that a worker-silent, a
// worker-background-stalled or a volume-near-full notice reaches (Engine.notice routes each to the
// architect that owns the issue, a root's or a child's), as prompts.Compose loads them. They are
// read from disk, as texts_steps_test.go reads them.
var architectReaderFiles = map[string][]string{
	"root architect":  {"../prompts/roles/architect-root.md", "../prompts/go/architect-root.md", "../prompts/go/architect-common.md"},
	"child architect": {"../prompts/roles/architect.md", "../prompts/go/architect.md", "../prompts/go/architect-common.md"},
}

// architectReaderPrompt is every file of files, joined.
func architectReaderPrompt(t *testing.T, files []string) string {
	t.Helper()
	var all strings.Builder
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		all.Write(body)
		all.WriteString("\n")
	}
	return all.String()
}

// architectCommonOpening is the line of go/architect-common.md, which both architects load, that
// opens with opening.
func architectCommonOpening(t *testing.T, opening string) string {
	t.Helper()
	const path = "../prompts/go/architect-common.md"
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, opening) {
			return line
		}
	}
	t.Fatalf("%s has no line opening %q", path, opening)
	return ""
}

// silentReadingAt is the moment the notices below were read.
var silentReadingAt = time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)

// Main's worker-silent notice (workflow/silence.go:workerSilentRecord at d46ec4e0) named the
// setting whose bound the worker passed: "past the bound this deployment gives a phase worker
// (worker_silence_minutes)". Its worker-background-stalled notice
// (workflow/silence.go:workerBackgroundRecord) did the same: "which is past the bound this
// deployment sets for it (worker_background_hours)". The branch's records carry the bound's value
// alone (`bound`), the setting's name being the same in every notice of each kind. INFRA-3686's
// R2: "No field holds a sentence that is the same in every text of its kind" and "a step that a
// text gives today goes into the reader's prompt where the prompt does not give it". The change
// keeps worker-stalled's setting that way (TestTheWorkerStalledPromptsNameTheBackstopSetting); each
// architect that reads these two kinds must be told their settings the same way.
func TestTheSilenceNoticesReadersAreToldTheSettingOfEachBound(t *testing.T) {
	issue := record.Issue{Key: "LEGION-301", Phase: phase.Testing}
	silent := WorkerSilentNotice(issue, claim.RoleTester, silentReadingAt.Add(-90*time.Minute), 90*time.Minute, time.Hour, false, 2, 2)
	stalled := WorkerBackgroundNotice(issue, claim.RoleTester, []string{"async-job"}, silentReadingAt.Add(-7*time.Hour), 7*time.Hour, 6*time.Hour)
	if silent.Bound == "" || stalled.Bound == "" {
		t.Fatalf("the notices carry no bound: worker-silent %+v, worker-background-stalled %+v", silent, stalled)
	}
	for reader, files := range architectReaderFiles {
		prompt := architectReaderPrompt(t, files)
		for _, want := range []struct{ kind, setting, bound string }{
			{"worker-silent", "worker_silence_minutes", silent.Bound},
			{"worker-background-stalled", "worker_background_hours", stalled.Bound},
		} {
			if !strings.Contains(prompt, want.setting) {
				t.Errorf("the %s's prompt (%s) names no `%s`: a %s notice gives `bound: %s` alone, where main's notice said the bound was the one this deployment sets in %s",
					reader, strings.Join(files, ", "), want.setting, want.kind, want.bound, want.setting)
			}
		}
	}
}

// Main's worker-silent notice (workflow/silence.go:workerSilentDid at d46ec4e0) said which start
// the daemon wrote, by the entry the phases row names and the one the claim was last given: "the
// daemon wrote the start row that sends the work of phase entry %d its phase row names, which its
// claim, last given entry %d, never had", "the daemon wrote the start row that sends the phase's
// task again, on phase entry %d its phase row names and its claim was already given", or "the
// daemon wrote the start row that sends the phase's task again". The branch's record names that in
// `sent` (`owed-entry` or `phase-task`), `entry` and `given`. dispatch://INFRA-3163's R2 (approved
// through dispatch://INFRA-3118) has the notice name "what the daemon did: the phase task sent
// again, with the assignment it carries", and INFRA-3686's R2 has what is the same in every text of
// a kind said in the reader's prompt, as the change says worker-stalled's `end` and `stop` words
// there. So each architect's prompt must say what each `sent` word sends and which entry `given`
// is.
func TestTheWorkerSilentPromptSaysWhatEachStartTheDaemonWroteSends(t *testing.T) {
	issue := record.Issue{Key: "LEGION-302", Phase: phase.Testing}
	behind := WorkerSilentNotice(issue, claim.RoleTester, silentReadingAt.Add(-90*time.Minute), 90*time.Minute, time.Hour, true, 5, 3)
	level := WorkerSilentNotice(issue, claim.RoleTester, silentReadingAt.Add(-90*time.Minute), 90*time.Minute, time.Hour, false, 5, 5)
	if behind.Sent == level.Sent || behind.Given == 0 || behind.Entry == 0 {
		t.Fatalf("the two starts are not told apart: %+v and %+v", behind, level)
	}
	for reader, files := range architectReaderFiles {
		prompt := architectReaderPrompt(t, files)
		for _, word := range []struct{ field, means string }{
			{"`" + behind.Sent + "`", "the work of the entry its phase row names, which the claim, last given an earlier entry, never had"},
			{"`" + level.Sent + "`", "the phase's task again, on an entry the claim was already given"},
			{"`given`", "the entry the claim was last given"},
		} {
			if !strings.Contains(prompt, word.field) {
				t.Errorf("the %s's prompt (%s) never names %s, which a worker-silent notice gives (sent: %s, entry: %d, given: %d) where main said %s",
					reader, strings.Join(files, ", "), word.field, behind.Sent, behind.Entry, behind.Given, word.means)
			}
		}
	}
}

// Main's volume-near-full notice (workflow/engine.go:volumeNoticeReason at d46ec4e0) told a sweep
// as "the volume %s's roles share on pod %s was still above its low mark when the sweep ended",
// and a trimmer that switched itself off, a report no sweep makes, as "the cache trimmer on %s's
// pod %s has switched itself off and frees nothing more until the pod's roles container restarts
// (trimmer %q)", then ": it made no reading of the volume" or ": its last reading of the volume
// was %d bytes used, %d free": no word of a volume filling or above its low mark.
// dispatch://INFRA-2009's R4 (approved v36) gives the reason: the switched-off report carries "no
// size walk, because the volume may be healthy". The branch's step row takes the sweep's sentence
// to the architect's prompt (go/architect-common.md, the volume-near-full line), as INFRA-3686's R2
// has it, but that line says it of every notice of the kind. A passage that gives main's sentence
// keeps main's condition: each sentence of that line that tells the volume as filling or above its
// low mark must say how it stands for a notice with `switchedOff: true`.
func TestASwitchedOffTrimmersNoticeIsNotToldAsAFillingVolume(t *testing.T) {
	notice := VolumeNearFullNotice("LEGION-303", record.VolumeNearFull{PodUID: "pod-303", Trimmer: "trim-303", Episode: 1, RaisedBy: "first",
		StopReason: "the trimmer switched itself off after 3 panics in a row of readings",
		Faults:     []string{"panic: statfs", "panic: statfs", "panic: statfs"}, SwitchedOff: true})
	if notice.Volume == nil || !notice.Volume.SwitchedOff || notice.Volume.UsedBytes != 0 || notice.Volume.FreeBytes != 0 {
		t.Fatalf("the notice is not that of a switched-off trimmer that made no reading: %+v", notice.Volume)
	}
	payload, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	const mainSaid = `the cache trimmer on LEGION-303's pod pod-303 has switched itself off and frees nothing more until the pod's roles container restarts (trimmer "trim-303"): it made no reading of the volume`
	line := architectCommonOpening(t, "A `volume-near-full` notice")
	for _, sentence := range strings.Split(line, ". ") {
		if !strings.Contains(sentence, "is filling") && !strings.Contains(sentence, "above its low mark") {
			continue
		}
		if !strings.Contains(sentence, "switchedOff") {
			t.Errorf("go/architect-common.md says %q of every volume-near-full notice, and so of %s, which no sweep made and which says nothing of a low mark; main told it as %q",
				sentence+".", payload, mainSaid)
		}
	}
}

// Main's volume-near-full notice (workflow/engine.go:volumeNoticeReason at d46ec4e0) wrote each
// size's name with %q, "; %q holds %d bytes (%d older than an hour, %d older than a day)", because
// "a name two levels under /legion, a directory's or a file's, is one a role of that pod chose",
// so a name holding a line feed and one holding a backslash and an `n` read apart ("a\nb" and
// "a\\nb"). dispatch://INFRA-2009's R4 (approved v36) has the notice name "everything the report
// carries", and INFRA-3686's R8 keeps "a fact or a step that it has the text give" as a field. The
// architect's prompt reads a value back by "Each line break inside a string of `volume` ... is
// written as Go writes it in a string, such as `\n` for a line feed", and the architect's skill
// has the pod's roles asked "to delete by exact path what they made". Two names that differ must
// reach the architect as two values.
func TestTwoNamesTheRoleChoseReachTheArchitectAsTwoValues(t *testing.T) {
	fed, slashed := "scratch/tmp/run\nlog", `scratch/tmp/run\nlog`
	notice := VolumeNearFullNotice("LEGION-304", record.VolumeNearFull{PodUID: "pod-304", Trimmer: "trim-304", Episode: 1, RaisedBy: "first",
		UsedBytes: 46 << 30, FreeBytes: 2 << 30, StopReason: "nothing left to free",
		Sizes: []record.VolumeSize{{Path: fed, Bytes: 3 << 30}, {Path: slashed, Bytes: 1 << 30}}, SizesAt: silentReadingAt})
	payload, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	var read record.Notice
	if err := json.Unmarshal(payload, &read); err != nil {
		t.Fatal(err)
	}
	if read.Volume == nil || len(read.Volume.Sizes) != 2 {
		t.Fatalf("the notice carries not the two sizes: %s", payload)
	}
	if got, other := read.Volume.Sizes[0].Path, read.Volume.Sizes[1].Path; got == other {
		t.Errorf("the size of %q and the size of %q reach the architect under one name, %q, which the prompt reads as a line feed; main wrote them %s and %s: %s",
			fed, slashed, got, strconv.Quote(fed), strconv.Quote(slashed), payload)
	}
}

// Main's volume-near-full notice (workflow/engine.go:volumeNoticeReason at d46ec4e0) counted the
// faults the report left out as the older ones: "; faults the trimmer recovered from: %d", then
// each fault, then "; %d older faults are left out of this report". The launcher keeps the newest
// faults and drops the older (internal/launcher's carryHeld and takeBack: "the bound keeps the
// newest"). The branch's record carries `faults` and `faultsOmitted`, and the architect's prompt
// says only "A count whose name ends in `Omitted` is the number of entries that the launcher left
// out of the list beside it, so that list is not all that the trimmer measured". INFRA-3686's R2
// has a fact that is the same in every text of its kind said in the reader's prompt, so the
// prompt must say that the faults left out are older than those the notice gives.
func TestTheArchitectIsToldTheFaultsLeftOutAreTheOlderOnes(t *testing.T) {
	notice := VolumeNearFullNotice("LEGION-305", record.VolumeNearFull{PodUID: "pod-305", Trimmer: "trim-305", Episode: 1, RaisedBy: "first",
		StopReason: "the trimmer switched itself off after 3 panics in a row of sweeps",
		Faults:     []string{"panic: newest but two", "panic: newest but one", "panic: newest"}, FaultsOmitted: 2, SwitchedOff: true})
	if notice.Volume == nil || notice.Volume.FaultsOmitted != 2 || len(notice.Volume.Faults) != 3 {
		t.Fatalf("the notice does not carry three faults and two left out: %+v", notice.Volume)
	}
	line := architectCommonOpening(t, "A `volume-near-full` notice")
	for _, sentence := range strings.Split(line, ". ") {
		if strings.Contains(sentence, "faultsOmitted") || (strings.Contains(sentence, "fault") && strings.Contains(sentence, "older")) {
			return
		}
	}
	t.Errorf("go/architect-common.md's volume-near-full line never says which faults `faultsOmitted: %d` counts; main said \"; 2 older faults are left out of this report\" after the %d it gave",
		notice.Volume.FaultsOmitted, len(notice.Volume.Faults))
}
