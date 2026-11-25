package docs

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Writes through a room and joined writes leave, once the queue has checked their document, the
// fold-point row and every row younger than the tail: the queue folds a document with at least
// compactMinRows rows older than the tail after its first, and leaves each younger row as written,
// byte for byte and under the transaction that wrote it. Each way of writing notes the document
// itself, so a document written only one way is compacted too.
func TestTheCompactionQueueFoldsTheOldRowsOfADocumentItsWritesNoted(t *testing.T) {
	t.Parallel()
	agent := model.Actor{Kind: "session", ID: "agent"}
	for _, writers := range []struct {
		name  string
		write func(t *testing.T, service *Service, artifactID string, index int)
	}{
		{"browser edits", func(t *testing.T, service *Service, artifactID string, index int) {
			editLiveTree(t, service, artifactID, appendBlocks(t, fmt.Sprintf("Paragraph %d.\n", index)))
		}},
		{"joined writes", func(t *testing.T, service *Service, artifactID string, index int) {
			if _, err := joinedReplaceText(service, artifactID, fmt.Sprintf("Replaced %d.\n", index), agent); err != nil {
				t.Fatalf("joined write %d: %v", index, err)
			}
		}},
		{"browser edits and joined writes", func(t *testing.T, service *Service, artifactID string, index int) {
			if index%2 == 0 {
				editLiveTree(t, service, artifactID, appendBlocks(t, fmt.Sprintf("Paragraph %d.\n", index)))
			} else if _, err := joinedReplaceText(service, artifactID, fmt.Sprintf("Replaced %d.\n", index), agent); err != nil {
				t.Fatalf("joined write %d: %v", index, err)
			}
		}},
	} {
		t.Run(writers.name, func(t *testing.T) {
			fake := clocktest.New(time.Now())
			service, artifactID := newClockedTestService(t, fake)
			steps := recordCompactionSteps(service)
			seedServiceText(t, service, artifactID, "First.\n")
			for index := range 20 {
				writers.write(t, service, artifactID, index)
			}
			waitForLandedAppends(t, service, artifactID)
			stepWhenDue(t, fake, service, artifactID)
			steps.await(t, artifactID, 0, "the queue's first check of the young rows", func(step compactionStepRecord) bool {
				return step.next.kind == compactionLater
			})
			// The queue steps only as the test moves the fake, so the rows the compaction folds are
			// read here before it runs; the writes below note the document as every write does.
			old := storedRows(t, service.store, artifactID)
			if len(old) < 1+compactMinRows {
				t.Fatalf("the writes stored %d rows, want at least %d", len(old), 1+compactMinRows)
			}
			ageUpdatesThrough(t, service.store, artifactID, everyVersion, 5*time.Minute)
			for index := 20; index < 22; index++ {
				writers.write(t, service, artifactID, index)
			}
			waitForLandedAppends(t, service, artifactID)
			before, cursor := storedRows(t, service.store, artifactID), settlementCursor(t, service.store, artifactID)
			reading := storedReading(t, NewPgVersioned(service.store), artifactID)
			foldPoint := old[len(old)-1].version

			from := steps.count()
			stepWhenDue(t, fake, service, artifactID)
			steps.await(t, artifactID, from, "the queue to compact the document", func(step compactionStepRecord) bool {
				return step.next.kind == compactionAgain
			})
			steps.await(t, artifactID, from, "the queue's check after the compaction", func(step compactionStepRecord) bool {
				return step.next.kind == compactionLater
			})
			requireCompactedFrom(t, service.store, artifactID, before, foldPoint, cursor)
			if got := storedReading(t, NewPgVersioned(service.store), artifactID); got != reading {
				t.Fatalf("the compacted document reads\n%s\nwant\n%s", got, reading)
			}
		})
	}
}

// A load notes the document it read, through a room's load or a cold read, and the queue compacts
// it exactly when it is due: at least compactMinRows rows older than the tail after its first row,
// or any row after its first older than compactMaxTailAge. A document that is not due keeps its
// rows as stored, and is checked again when it will be.
func TestTheCompactionQueueCompactsALoadedDocumentOnlyWhenItIsDue(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	database := storetest.Open(t)
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour, Clock: fake})
	testcleanup.Add(t, func() { _ = docstest.Shutdown(t, testwait.Context(t), service) })
	steps := recordCompactionSteps(service)
	loads := map[string]func(t *testing.T, artifactID string){
		"a room load": func(t *testing.T, artifactID string) { liveTree(t, service, artifactID) },
		"a cold read": func(t *testing.T, artifactID string) {
			if _, err := service.Text(context.Background(), artifactID); err != nil {
				t.Fatalf("read the document: %v", err)
			}
		},
	}
	for _, test := range []struct {
		name string
		rows int
		age  time.Duration
		// dueIn is how long until the document is due, zero for one due now.
		dueIn time.Duration
	}{
		{name: "nine rows five minutes old after the first", rows: 10, age: 5 * time.Minute, dueIn: 10 * time.Minute},
		{name: "fifteen rows five minutes old after the first", rows: 16, age: 5 * time.Minute, dueIn: 10 * time.Minute},
		{name: "sixteen rows five minutes old after the first", rows: 17, age: 5 * time.Minute},
		{name: "twenty rows a minute old after the first", rows: 21, age: time.Minute, dueIn: time.Minute},
		{name: "two rows twenty minutes old after the first", rows: 3, age: 20 * time.Minute},
	} {
		for loadName, load := range loads {
			t.Run(test.name+" noted by "+loadName, func(t *testing.T) {
				artifactID := storeAgedRows(t, database, test.rows, test.age)
				before := storedRows(t, database, artifactID)
				reading := storedReading(t, NewPgVersioned(database), artifactID)
				from := steps.count()
				load(t, artifactID)
				stepWhenDue(t, fake, service, artifactID)
				if test.dueIn == 0 {
					steps.await(t, artifactID, from, "the queue to compact the due document", func(step compactionStepRecord) bool {
						return step.next.kind == compactionAgain
					})
					steps.await(t, artifactID, from, "the queue to drop the compacted document", func(step compactionStepRecord) bool {
						return step.next.kind == compactionDone
					})
					after := storedRows(t, database, artifactID)
					if len(after) != 1 || after[0].version != before[len(before)-1].version {
						t.Fatalf("the compacted document holds rows %v, want the head %d alone", rowVersions(after), before[len(before)-1].version)
					}
					if got := storedReading(t, NewPgVersioned(database), artifactID); got != reading {
						t.Fatalf("the compacted document reads\n%s\nwant\n%s", got, reading)
					}
					return
				}
				step := steps.await(t, artifactID, from, "the queue's check of the document", func(step compactionStepRecord) bool {
					return step.next.kind == compactionLater
				})
				if wait := step.next.after; wait > test.dueIn || wait < test.dueIn-30*time.Second {
					t.Fatalf("the queue checks the document again in %s, want about %s, when it is due", wait, test.dueIn)
				}
				requireRowsAsStored(t, database, artifactID, before)
			})
		}
	}
}

// A document whose rows are all younger than the tail is checked again when they come of age: the
// queue schedules it for when its oldest row after the first passes compactMaxTailAge, and that
// check compacts it.
func TestTheCompactionQueueComesBackToADocumentWhenItsRowsComeOfAge(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	service, artifactID := newClockedTestService(t, fake)
	steps := recordCompactionSteps(service)
	seedServiceText(t, service, artifactID, "First.\n")
	for _, paragraph := range []string{"Second.\n", "Third.\n", "Fourth.\n"} {
		editLiveTree(t, service, artifactID, appendBlocks(t, paragraph))
	}
	waitForLandedAppends(t, service, artifactID)
	checked := stepWhenDue(t, fake, service, artifactID)
	step := steps.await(t, artifactID, 0, "the queue's check of the young rows", func(step compactionStepRecord) bool {
		return step.next.kind == compactionLater
	})
	if wait := step.next.after; wait > compactMaxTailAge || wait < compactMaxTailAge-time.Minute {
		t.Fatalf("the queue checks the young rows again in %s, want about %s", wait, compactMaxTailAge)
	}
	before := storedRows(t, service.store, artifactID)

	ageUpdatesThrough(t, service.store, artifactID, everyVersion, compactMaxTailAge+time.Minute)
	from := steps.count()
	if at := stepWhenDue(t, fake, service, artifactID); at.Sub(checked) != step.next.after {
		t.Fatalf("the queue checks the young rows again %s after their check, want %s, when that check said they are due", at.Sub(checked), step.next.after)
	}
	steps.await(t, artifactID, from, "the queue to compact the rows come of age", func(step compactionStepRecord) bool {
		return step.next.kind == compactionAgain
	})
	after := storedRows(t, service.store, artifactID)
	if len(after) != 1 || after[0].version != before[len(before)-1].version {
		t.Fatalf("the compacted document holds rows %v, want the head %d alone", rowVersions(after), before[len(before)-1].version)
	}
}

// A compaction another writer drops - a prune between its read and its write - is retried after the
// queue's backoff, and the retry folds the rows the prune left.
func TestTheCompactionQueueRetriesACompactionAPruneDropped(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	database := storetest.Open(t)
	persist := NewPgVersioned(database)
	service := New(Deps{Store: database, Persistence: persist, Events: events.NewBroker(), Settle: time.Hour, Clock: fake})
	testcleanup.Add(t, func() { _ = docstest.Shutdown(t, testwait.Context(t), service) })
	steps := recordCompactionSteps(service)
	artifactID := storeAgedRows(t, database, 6, 20*time.Minute)
	var pruned LoadedDocument
	duringCompactionFold(persist, artifactID, func() {
		if err := persist.PruneAfter(context.Background(), artifactID, 4, nil); err != nil {
			t.Errorf("prune to version 4: %v", err)
		}
		var err error
		if pruned, err = NewPgVersioned(database).LoadDocument(context.Background(), artifactID); err != nil {
			t.Errorf("load the pruned document: %v", err)
		}
	})

	if _, err := persist.Load(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document: %v", err)
	}
	dropped := stepWhenDue(t, fake, service, artifactID)
	steps.await(t, artifactID, 0, "the queue's compaction the prune drops", func(step compactionStepRecord) bool {
		return step.next.kind == compactionRetry
	})
	if retried := stepWhenDue(t, fake, service, artifactID); retried.Sub(dropped) != compactBackoff {
		t.Fatalf("the queue retried the dropped compaction %s after it, want its backoff, %s", retried.Sub(dropped), compactBackoff)
	}
	steps.await(t, artifactID, 0, "the queue to drop the compacted document", func(step compactionStepRecord) bool {
		return step.next.kind == compactionDone
	})
	got := steps.of(artifactID)
	want := []compactionStepRecord{
		{room: artifactID, next: compactionNext{kind: compactionRetry}},
		{room: artifactID, backoff: compactBackoff, next: compactionNext{kind: compactionAgain}},
		{room: artifactID, next: compactionNext{kind: compactionDone}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the queue's steps = %v, want the dropped compaction retried after %s and then compacted: %v", got, compactBackoff, want)
	}
	if after := storedRows(t, database, artifactID); len(after) != 1 || after[0].version != 4 {
		t.Fatalf("the document holds rows %v after the retry, want the pruned rows folded into version 4", rowVersions(after))
	}
	prunedReading := docMarkdown(pruned.Doc) + "\n" + rawReading(t, pruned.Doc)
	if reading := storedReading(t, NewPgVersioned(database), artifactID); reading != prunedReading {
		t.Fatalf("the document reads\n%s\nwant the pruned one\n%s", reading, prunedReading)
	}
}

// A document whose lock another holder has is retried, the backoff doubling to its cap, and
// compacts on the first retry after the lock is free.
func TestTheCompactionQueueRetriesABusyDocumentUntilItsLockIsFree(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	database := storetest.Open(t)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour, Clock: fake})
	testcleanup.Add(t, func() { _ = docstest.Shutdown(t, testwait.Context(t), service) })
	steps := recordCompactionSteps(service)
	artifactID := storeAgedRows(t, database, 4, 20*time.Minute)
	before := storedRows(t, database, artifactID)
	unlock := holdDocumentLock(t, database, artifactID)

	loadStored(t, service, artifactID)
	at := stepWhenDue(t, fake, service, artifactID)
	steps.await(t, artifactID, 0, "the busy document's first check", func(step compactionStepRecord) bool {
		return step.next.kind == compactionRetry
	})
	// Every retry finds the lock held too, and comes its backoff after the step before it: the
	// backoff doubles from compactBackoff to compactBackoffCap, and stays there.
	backoffs := []time.Duration{compactBackoff}
	for backoffs[len(backoffs)-1] < compactBackoffCap {
		backoffs = append(backoffs, min(2*backoffs[len(backoffs)-1], compactBackoffCap))
	}
	backoffs = append(backoffs, compactBackoffCap)
	for index, backoff := range backoffs {
		from := steps.count()
		retried := stepWhenDue(t, fake, service, artifactID)
		if gap := retried.Sub(at); gap != backoff {
			t.Fatalf("retry %d of the busy document came %s after the step before it, want %s", index+1, gap, backoff)
		}
		at = retried
		steps.await(t, artifactID, from, fmt.Sprintf("retry %d of the busy document", index+1), func(step compactionStepRecord) bool {
			return step.next.kind == compactionRetry
		})
	}
	requireRowsAsStored(t, database, artifactID, before)
	var got []time.Duration
	for _, step := range steps.of(artifactID) {
		got = append(got, step.backoff)
	}
	if want := append([]time.Duration{0}, backoffs...); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the busy document's steps ran after backoffs %v, want %v", got, want)
	}

	from := steps.count()
	unlock()
	stepWhenDue(t, fake, service, artifactID)
	steps.await(t, artifactID, from, "the retry after the lock is free to compact", func(step compactionStepRecord) bool {
		return step.backoff > 0 && step.next.kind == compactionAgain
	})
	if after := storedRows(t, database, artifactID); len(after) != 1 || after[0].version != before[len(before)-1].version {
		t.Fatalf("the document holds rows %v, want the head %d alone", rowVersions(after), before[len(before)-1].version)
	}
}

// A retry finds the document has no row older than the tail after its first, and the queue stops
// retrying it but does not drop it: the young rows stay as stored, and the queue checks the
// document again for when they come due, which compacts it.
func TestTheCompactionQueueComesBackToARetriedDocumentWhenItsYoungRowsComeDue(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	database := storetest.Open(t)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour, Clock: fake})
	testcleanup.Add(t, func() { _ = docstest.Shutdown(t, testwait.Context(t), service) })
	steps := recordCompactionSteps(service)
	artifactID := storeAgedRows(t, database, 4, 20*time.Minute)
	unlock := holdDocumentLock(t, database, artifactID)

	// The rows are made young only between two steps of the queue: a step checks the room
	// (tryCompaction) and then reads its fold point (readCompaction) in a snapshot of its own, so
	// rows made young between those two reads leave the read no fold point, and the step ends the
	// room as done and drops it, which no later note undoes. The worker is held in afterStep, after
	// the first retry and before the queue settles it, while the rows are changed: the retry is
	// recorded before the hold stops the worker, and no step settles or runs until release.
	release := holdAfterStep(t, service, artifactID, func(step compactionStepRecord) bool {
		return step.backoff > 0 && step.next.kind == compactionRetry
	})
	loadStored(t, service, artifactID)
	stepWhenDue(t, fake, service, artifactID)
	stepWhenDue(t, fake, service, artifactID)
	steps.await(t, artifactID, 0, "a retry of the busy document", func(step compactionStepRecord) bool {
		return step.backoff > 0 && step.next.kind == compactionRetry
	})
	ageUpdatesThrough(t, database, artifactID, everyVersion, 0)
	young := storedRows(t, database, artifactID)
	release()
	retried := stepWhenDue(t, fake, service, artifactID)
	step := steps.await(t, artifactID, 0, "the retry that finds no old row to check the document again later", func(step compactionStepRecord) bool {
		return step.backoff > 0 && step.next.kind == compactionLater
	})
	if wait := step.next.after; wait > compactMaxTailAge || wait < compactMaxTailAge-time.Minute {
		t.Fatalf("the queue checks the young rows again in %s, want about %s, when they are due", wait, compactMaxTailAge)
	}
	waitFor(t, "the queue to schedule the document's check", func() bool {
		return queueWaitsOn(service, artifactID)
	})
	requireRowsAsStored(t, database, artifactID, young)

	unlock()
	ageUpdatesThrough(t, database, artifactID, everyVersion, 20*time.Minute)
	from := steps.count()
	if at := stepWhenDue(t, fake, service, artifactID); at.Sub(retried) != step.next.after {
		t.Fatalf("the queue checks the young rows again %s after the retry, want %s, when that retry said they are due", at.Sub(retried), step.next.after)
	}
	steps.await(t, artifactID, from, "the check once the rows are due to compact them", func(step compactionStepRecord) bool {
		return step.backoff == 0 && step.next.kind == compactionAgain
	})
	if after := storedRows(t, database, artifactID); len(after) != 1 || after[0].version != young[len(young)-1].version {
		t.Fatalf("the document holds rows %v, want the head %d alone", rowVersions(after), young[len(young)-1].version)
	}
}

// A note that comes while its document's step runs is kept even when that step drops the document:
// an append that commits as the check after a compaction finds no row after the first has the
// queue check the document again, and once the appended row is due that check compacts it.
func TestTheCompactionQueueKeepsANoteThatComesWhileItsDocumentsStepRuns(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	database := storetest.Open(t)
	persist := NewPgVersioned(database)
	service := New(Deps{Store: database, Persistence: persist, Events: events.NewBroker(), Settle: time.Hour, Clock: fake})
	testcleanup.Add(t, func() { _ = docstest.Shutdown(t, testwait.Context(t), service) })
	steps := recordCompactionSteps(service)
	artifactID := createDocument(t, database, "renoted")
	writer := newProofHistory(t, 101)
	appendUpdates(t, NewPgVersioned(database), artifactID, writer.write("Row 0.\n"), writer.write("Row 1.\n"), writer.write("Row 2.\n"))
	ageUpdatesThrough(t, database, artifactID, everyVersion, 20*time.Minute)
	late := writer.write("Row 3.\n")
	// The append runs on the worker after the step that drops the document has decided, before the
	// queue settles that step: the room is still in flight when the append notes it.
	appended := make(chan error, 1)
	var once sync.Once
	record := service.compactions.afterStep
	service.compactions.afterStep = func(room string, backoff time.Duration, next compactionNext) {
		if room == artifactID && next.kind == compactionDone {
			once.Do(func() {
				_, err := persist.AppendUpdate(context.Background(), artifactID, late)
				appended <- err
			})
		}
		record(room, backoff, next)
	}

	loadStored(t, service, artifactID)
	compacted := stepWhenDue(t, fake, service, artifactID)
	steps.await(t, artifactID, 0, "the queue to compact the document", func(step compactionStepRecord) bool {
		return step.next.kind == compactionAgain
	})
	steps.await(t, artifactID, 0, "the check after the compaction to find no row after the first", func(step compactionStepRecord) bool {
		return step.next.kind == compactionDone
	})
	if err := <-appended; err != nil {
		t.Fatalf("append while the step was in flight: %v", err)
	}
	// The note in flight queues the document for the check a first note gives it.
	renoted := stepWhenDue(t, fake, service, artifactID)
	if renoted.Sub(compacted) != compactFirstCheck {
		t.Fatalf("the note in flight queued the document's check %s after the step it came in, want %s", renoted.Sub(compacted), compactFirstCheck)
	}
	step := steps.await(t, artifactID, 0, "the check the note in flight queued", func(step compactionStepRecord) bool {
		return step.next.kind == compactionLater
	})
	if wait := step.next.after; wait > compactMaxTailAge || wait < compactMaxTailAge-time.Minute {
		t.Fatalf("the queue checks the appended row again in %s, want about %s, when it is due", wait, compactMaxTailAge)
	}
	waitFor(t, "the queue to schedule the document's check", func() bool {
		return queueWaitsOn(service, artifactID)
	})
	before := storedRows(t, database, artifactID)
	if len(before) != 2 {
		t.Fatalf("the document holds rows %v, want the compacted row and the appended one", rowVersions(before))
	}

	ageUpdatesThrough(t, database, artifactID, everyVersion, 20*time.Minute)
	from := steps.count()
	stepWhenDue(t, fake, service, artifactID)
	steps.await(t, artifactID, from, "the check once the appended row is due to compact it", func(step compactionStepRecord) bool {
		return step.next.kind == compactionAgain
	})
	if after := storedRows(t, database, artifactID); len(after) != 1 || after[0].version != before[1].version {
		t.Fatalf("the document holds rows %v, want the appended head %d alone", rowVersions(after), before[1].version)
	}
}

// A burst of appends that starts after a quiet check brings the document's check forward: the
// document waits out its oldest row's compactMaxTailAge after a check that found it not due, and
// the burst's first append notes it, which brings its check to compactFirstCheck from then, so once
// compactMinRows of the burst's rows are older than the tail that check folds them, rather than
// leaving them all for the 15-minute check. The document then holds its fold point, the rows
// younger than the tail and no older row.
func TestABurstAfterAQuietCheckBringsTheDocumentsCheckForward(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	database := storetest.Open(t)
	persist := NewPgVersioned(database)
	service := New(Deps{Store: database, Persistence: persist, Events: events.NewBroker(), Settle: time.Hour, Clock: fake})
	testcleanup.Add(t, func() { _ = docstest.Shutdown(t, testwait.Context(t), service) })
	steps := recordCompactionSteps(service)
	artifactID := createDocument(t, database, "burst")
	writer := newProofHistory(t, 101)
	quiet := NewPgVersioned(database)
	for index := range 4 {
		appendUpdates(t, quiet, artifactID, writer.write(fmt.Sprintf("Quiet %d.\n", index)))
	}
	ageUpdatesThrough(t, database, artifactID, everyVersion, 5*time.Minute)

	loadStored(t, service, artifactID)
	checked := stepWhenDue(t, fake, service, artifactID)
	step := steps.await(t, artifactID, 0, "the quiet check", func(step compactionStepRecord) bool {
		return step.next.kind == compactionLater
	})
	if wait := step.next.after; wait < 9*time.Minute {
		t.Fatalf("the quiet check schedules the next in %s, want about 10 minutes, the oldest row's age rule", wait)
	}
	waitFor(t, "the queue to schedule the quiet document's check", func() bool {
		return queueWaitsOn(service, artifactID)
	})

	from := steps.count()
	for index := range 20 {
		appendUpdates(t, persist, artifactID, writer.write(fmt.Sprintf("Burst %d.\n", index)))
	}
	// The burst comes of age, as it does compactTail after it was written.
	ageUpdatesThrough(t, database, artifactID, everyVersion, 5*time.Minute)
	appendUpdates(t, persist, artifactID, writer.write("Young.\n"))
	if at := stepWhenDue(t, fake, service, artifactID); at.Sub(checked) != compactFirstCheck {
		t.Fatalf("after the burst the queue checks the document %s after the quiet check, want %s, the check its notes brought forward", at.Sub(checked), compactFirstCheck)
	}
	steps.await(t, artifactID, from, "the check the burst's notes brought forward to compact it", func(step compactionStepRecord) bool {
		return step.next.kind == compactionAgain
	})
	if after := storedRows(t, database, artifactID); len(after) != 2 {
		t.Fatalf("the document holds rows %v, want the fold point and the young row alone", rowVersions(after))
	}
}

// The queue's compaction never runs on a request: a joined write and a browser's edit of the
// document each return while the queue's compaction of it is held inside its fold, and the
// compaction then folds the rows it read beneath theirs.
func TestWritesReturnWhileTheCompactionQueueIsInsideTheirDocumentsFold(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	service, artifactID := newClockedTestService(t, fake)
	steps := recordCompactionSteps(service)
	persist := service.persistence.(*PgVersioned)
	held, release := holdCompactionFold(t, persist, artifactID)
	seedServiceText(t, service, artifactID, "First.\n")
	editLiveTree(t, service, artifactID, appendBlocks(t, "Second.\n"))
	waitForLandedAppends(t, service, artifactID)
	stepWhenDue(t, fake, service, artifactID)
	steps.await(t, artifactID, 0, "the queue's check of the young rows", func(step compactionStepRecord) bool {
		return step.next.kind == compactionLater
	})
	ageUpdatesThrough(t, service.store, artifactID, everyVersion, 20*time.Minute)
	rows := storedRows(t, service.store, artifactID)
	foldPoint := rows[len(rows)-1].version

	stepWhenDue(t, fake, service, artifactID)
	testwait.Receive(t, "the compaction queue's step to reach the held fold", held)
	wrote := make(chan error, 1)
	go func() {
		_, err := joinedReplaceText(service, artifactID, "Replaced.\n", model.Actor{Kind: "session", ID: "agent"})
		wrote <- err
	}()
	if err := testwait.Receive(t, "the joined write to return while the queue's compaction is inside its fold", wrote); err != nil {
		t.Fatalf("the joined write: %v", err)
	}
	editLiveTree(t, service, artifactID, appendBlocks(t, "After.\n"))
	waitForLandedAppends(t, service, artifactID)
	written, cursor := storedRows(t, service.store, artifactID), settlementCursor(t, service.store, artifactID)
	if len(written) < len(rows)+2 {
		t.Fatalf("the writes stored %d rows over the %d before them, want both of theirs", len(written), len(rows))
	}

	from := steps.count()
	release()
	steps.await(t, artifactID, from, "the held compaction to compact", func(step compactionStepRecord) bool {
		return step.next.kind == compactionAgain
	})
	requireCompactedFrom(t, service.store, artifactID, written, foldPoint, cursor)
	if got := docMarkdown(loadDocument(t, persist, artifactID).Doc); got != "Replaced.\n\nAfter.\n" {
		t.Fatalf("the compacted document reads %q, want the joined write's and the browser's", got)
	}
}

// Shutdown lets the queue's compaction in flight finish and commit before it returns, takes no note
// once it has begun, and returns only once the queue's worker has.
func TestShutdownFinishesTheCompactionQueuesCompactionAndStopsItsWorker(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	database := storetest.Open(t)
	persist := NewPgVersioned(database)
	service := New(Deps{Store: database, Persistence: persist, Events: events.NewBroker(), Settle: time.Hour, Clock: fake})
	testcleanup.Add(t, func() { _ = docstest.Shutdown(t, testwait.Context(t), service) })
	steps := recordCompactionSteps(service)
	compacting := storeAgedRows(t, database, 4, 20*time.Minute)
	later := storeAgedRows(t, database, 4, 20*time.Minute)
	untouched := storedRows(t, database, later)
	held, release := holdCompactionFold(t, persist, compacting)

	if _, err := persist.Load(context.Background(), compacting); err != nil {
		t.Fatalf("load the document: %v", err)
	}
	stepWhenDue(t, fake, service, compacting)
	testwait.Receive(t, "the compaction queue's step to reach the held fold", held)
	// Shutdown runs on a goroutine of its own and records whether the queue's compaction had been
	// let go of its fold before it returned: one that returned before then did not wait for it. It
	// runs on the shared wait's context: its drain budget is on fake, which nothing moves from here.
	var released atomic.Bool
	shutdownCtx := testwait.Context(t)
	shutdown := make(chan heldReturn, 1)
	go func() {
		err := service.Shutdown(shutdownCtx)
		shutdown <- heldReturn{err: err, afterRelease: released.Load()}
	}()
	waitFor(t, "Shutdown to stop the queue's intake", func() bool {
		service.compactions.mu.Lock()
		defer service.compactions.mu.Unlock()
		return service.compactions.shut
	})
	if _, err := persist.Load(context.Background(), later); err != nil {
		t.Fatalf("load the other document: %v", err)
	}
	if queueHolds(service, later) {
		t.Fatal("the queue took a note after Shutdown began")
	}
	select {
	case returned := <-shutdown:
		t.Fatalf("Shutdown returned (%v) while the queue's compaction was inside its fold", returned.err)
	default:
	}

	released.Store(true)
	release()
	returned := testwait.Receive(t, "Shutdown to return once the queue's compaction was released", shutdown)
	if returned.err != nil {
		t.Fatalf("shutdown: %v", returned.err)
	}
	if !returned.afterRelease {
		t.Fatal("Shutdown returned while the queue's compaction was inside its fold")
	}
	select {
	case <-service.compactions.done:
	default:
		t.Fatal("the queue's worker is still running after Shutdown returned")
	}
	if rows := storedUpdateCount(t, database, compacting); rows != 1 {
		t.Fatalf("the document held %d rows when Shutdown returned, want the compaction in flight committed", rows)
	}
	if got := steps.of(compacting); len(got) != 1 || got[0].next.kind != compactionAgain {
		t.Fatalf("the queue's steps of the compacted document = %v, want its one compaction and no check after Shutdown", got)
	}
	requireRowsAsStored(t, database, later, untouched)
}

// Quiesce waits for the queue's compaction in flight, takes no note while it runs and drops what the
// queue held, and the queue takes notes again once it returns.
func TestQuiesceWaitsForTheCompactionQueueAndReopensIt(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	database := storetest.Open(t)
	persist := NewPgVersioned(database)
	service := New(Deps{Store: database, Persistence: persist, Events: events.NewBroker(), Settle: time.Hour, Clock: fake})
	testcleanup.Add(t, func() { _ = docstest.Shutdown(t, testwait.Context(t), service) })
	steps := recordCompactionSteps(service)
	compacting := storeAgedRows(t, database, 4, 20*time.Minute)
	young := storeAgedRows(t, database, 4, 0)
	later := storeAgedRows(t, database, 4, 20*time.Minute)
	held, release := holdCompactionFold(t, persist, compacting)

	if _, err := persist.Load(context.Background(), young); err != nil {
		t.Fatalf("load the young document: %v", err)
	}
	stepWhenDue(t, fake, service, young)
	steps.await(t, young, 0, "the queue's check of the young document", func(step compactionStepRecord) bool {
		return step.next.kind == compactionLater
	})
	if _, err := persist.Load(context.Background(), compacting); err != nil {
		t.Fatalf("load the document: %v", err)
	}
	stepWhenDue(t, fake, service, compacting)
	testwait.Receive(t, "the compaction queue's step to reach the held fold", held)
	// Quiesce runs on a goroutine of its own and records whether the queue's compaction had been
	// let go of its fold before it returned: one that returned before then did not wait for it.
	var released atomic.Bool
	quiesced := make(chan heldReturn, 1)
	go func() {
		err := service.Quiesce(context.Background())
		quiesced <- heldReturn{err: err, afterRelease: released.Load()}
	}()
	waitFor(t, "Quiesce to pause the queue", func() bool {
		service.compactions.mu.Lock()
		defer service.compactions.mu.Unlock()
		return service.compactions.paused
	})
	if _, err := persist.Load(context.Background(), later); err != nil {
		t.Fatalf("load the other document: %v", err)
	}
	if queueHolds(service, later) {
		t.Fatal("the queue took a note while Quiesce ran")
	}
	select {
	case returned := <-quiesced:
		t.Fatalf("Quiesce returned (%v) while the queue's compaction was inside its fold", returned.err)
	default:
	}

	released.Store(true)
	release()
	returned := testwait.Receive(t, "Quiesce to return once the queue's compaction was released", quiesced)
	if returned.err != nil {
		t.Fatalf("quiesce: %v", returned.err)
	}
	if !returned.afterRelease {
		t.Fatal("Quiesce returned while the queue's compaction was inside its fold")
	}
	if rows := storedUpdateCount(t, database, compacting); rows != 1 {
		t.Fatalf("the document held %d rows when Quiesce returned, want the compaction in flight committed", rows)
	}
	service.compactions.mu.Lock()
	queued := len(service.compactions.rooms)
	service.compactions.mu.Unlock()
	if queued != 0 {
		t.Fatalf("the queue holds %d documents after Quiesce, want none", queued)
	}

	if _, err := persist.Load(context.Background(), later); err != nil {
		t.Fatalf("load the other document after Quiesce: %v", err)
	}
	stepWhenDue(t, fake, service, later)
	waitFor(t, "the queue to compact a document noted after Quiesce", func() bool {
		return storedUpdateCount(t, database, later) == 1
	})
}

// heldReturn is what a call the test runs on a goroutine of its own while it holds the queue's
// compaction inside its fold answers with: its error, and whether the test had let the compaction
// go before the call returned.
type heldReturn struct {
	err          error
	afterRelease bool
}

// The rooms Shutdown closes compact as they close, and leave every row younger than the tail as
// written, byte for byte and under the transaction that wrote it.
func TestShutdownsCompactionLeavesTheYoungRowsAsWritten(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: stagedAhead})
	testcleanup.Add(t, func() { _ = docstest.Shutdown(t, context.Background(), service) })
	seedServiceText(t, service, artifactID, "First.\n")
	editLiveTree(t, service, artifactID, appendBlocks(t, "Second.\n"))
	waitForLandedAppends(t, service, artifactID)
	ageUpdatesThrough(t, database, artifactID, everyVersion, 20*time.Minute)
	old := storedRows(t, database, artifactID)
	editLiveTree(t, service, artifactID, appendBlocks(t, "Third.\n"))
	editLiveTree(t, service, artifactID, appendBlocks(t, "Fourth.\n"))
	waitForLandedAppends(t, service, artifactID)
	young := storedRows(t, database, artifactID)[len(old):]
	if len(young) < 2 {
		t.Fatalf("the young edits stored %d rows, want two", len(young))
	}

	if err := docstest.Shutdown(t, context.Background(), service); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	after := storedRows(t, database, artifactID)
	if after[0].version != old[len(old)-1].version {
		t.Fatalf("the document holds rows %v after Shutdown, want the old rows folded into version %d", rowVersions(after), old[len(old)-1].version)
	}
	for _, row := range young {
		found := false
		for _, stands := range after {
			found = found || sameRow(stands, row)
		}
		if !found {
			t.Fatalf("young row %d was rewritten or deleted at Shutdown; rows now %v", row.version, rowVersions(after))
		}
	}
}

// compactionStepRecord is one step the compaction queue ran: its room, the backoff it retried
// under (zero for a check), and what it decided.
type compactionStepRecord struct {
	room    string
	backoff time.Duration
	next    compactionNext
}

// compactionSteps is every step a service's compaction queue has run, in order.
type compactionSteps struct {
	mu    sync.Mutex
	steps []compactionStepRecord
}

// recordCompactionSteps has the service's compaction queue record every step it runs, and returns
// the record. It runs before anything notes a room, so the worker reads no hook before it is set.
func recordCompactionSteps(service *Service) *compactionSteps {
	steps := &compactionSteps{}
	service.compactions.afterStep = func(room string, backoff time.Duration, next compactionNext) {
		steps.mu.Lock()
		defer steps.mu.Unlock()
		steps.steps = append(steps.steps, compactionStepRecord{room: room, backoff: backoff, next: next})
	}
	return steps
}

// newClockedTestService is newTestService on c, with settlement left to the test: the service's
// compaction queue steps only as the test moves c (stepWhenDue).
func newClockedTestService(t *testing.T, c *clocktest.Fake) (*Service, string) {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  headerIdentity(database),
		ServerURL: "https://dispatch.example",
		Settle:    time.Hour,
		Clock:     c,
	})
	testcleanup.Add(t, func() {
		if err := docstest.Shutdown(t, testwait.Context(t), service); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service, artifactID
}

// stepWhenDue moves fake to the time the service's compaction queue has scheduled room's next step
// for, once no step is in flight and the queue's worker waits on its timer for the earliest step,
// and returns that time: the step, and any step due before it, then runs as that time coming runs
// it. A move made before the worker armed its timer would leave the timer armed past the time. A
// try that finds no step to move to reads the room in flight, the rooms due and when, and the
// timers the fake holds armed.
func stepWhenDue(t *testing.T, fake *clocktest.Fake, service *Service, room string) time.Time {
	t.Helper()
	q := service.compactions
	var at time.Time
	testwait.Until(t, "the compaction queue to schedule a step of "+room+" and its worker to arm its timer for the earliest step", func() (string, bool) {
		q.mu.Lock()
		defer q.mu.Unlock()
		armed := fake.Armed()
		inFlight := "no room"
		if q.current != nil {
			inFlight = q.current.room
		}
		due := make([]string, 0, len(q.due))
		for _, entry := range q.due {
			due = append(due, entry.room+" at "+entry.at.Format(time.RFC3339Nano))
		}
		reading := fmt.Sprintf("%s in flight; due: %v; the fake at %s holds %v armed", inFlight, due, fake.Now().Format(time.RFC3339Nano), armed)
		if q.current != nil || len(q.due) == 0 {
			return reading, false
		}
		for _, entry := range q.due {
			if entry.room == room {
				if !slices.ContainsFunc(armed, q.due[0].at.Equal) {
					return reading, false
				}
				at = entry.at
				return reading, true
			}
		}
		return reading, false
	})
	fake.Set(at)
	return at
}

func (s *compactionSteps) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.steps)
}

// of is the steps the queue has run for room, in order.
func (s *compactionSteps) of(room string) []compactionStepRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var steps []compactionStepRecord
	for _, step := range s.steps {
		if step.room == room {
			steps = append(steps, step)
		}
	}
	return steps
}

// await waits for a step of room at or after index from that want accepts, and returns it. A wait
// that gives up names every step the queue ran for room as its last read, so the failure says what
// the queue decided instead of only that nothing it decided was wanted.
func (s *compactionSteps) await(t *testing.T, room string, from int, description string, want func(compactionStepRecord) bool) compactionStepRecord {
	t.Helper()
	var found compactionStepRecord
	testwait.Until(t, description+" (every step the queue ran for the room, last read)", func() ([]compactionStepRecord, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, step := range s.steps[min(from, len(s.steps)):] {
			if step.room == room && want(step) {
				found = step
				return nil, true
			}
		}
		var ran []compactionStepRecord
		for _, step := range s.steps {
			if step.room == room {
				ran = append(ran, step)
			}
		}
		return ran, false
	})
	return found
}

// queueHolds reports whether the service's compaction queue holds room.
func queueHolds(service *Service, room string) bool {
	service.compactions.mu.Lock()
	defer service.compactions.mu.Unlock()
	return service.compactions.rooms[room] != nil
}

// queueWaitsOn reports whether the service's compaction queue has room scheduled for a step, its
// step in flight settled.
func queueWaitsOn(service *Service, room string) bool {
	q := service.compactions
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, entry := range q.due {
		if entry.room == room {
			return true
		}
	}
	return false
}

// holdCompactionFold holds the first compaction of artifactID on versioned inside its fold: held is
// closed once it is there, and release lets it go on, which the test's cleanup does too.
func holdCompactionFold(t *testing.T, versioned *PgVersioned, artifactID string) (<-chan struct{}, func()) {
	t.Helper()
	held, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	testcleanup.Add(t, release)
	duringCompactionFold(versioned, artifactID, func() {
		close(held)
		<-released
	})
	return held, release
}

// holdAfterStep holds the service's compaction queue's worker after the first step of room that
// want accepts, from now on, until release, which the test's cleanup calls too. It wraps the
// recorder recordCompactionSteps set as the queue's afterStep, which the worker calls after each
// step and before it settles that step or takes the next (compactionQueue.run): the step it holds
// after is recorded first, so a test awaits that step, and no step of any room runs while it waits.
// Like recordCompactionSteps, it runs before anything notes a room, so the worker reads none of it
// before it is set.
func holdAfterStep(t *testing.T, service *Service, room string, want func(compactionStepRecord) bool) func() {
	t.Helper()
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	testcleanup.Add(t, release)
	q := service.compactions
	record := q.afterStep
	holding := false
	q.afterStep = func(stepRoom string, backoff time.Duration, next compactionNext) {
		record(stepRoom, backoff, next)
		if holding || stepRoom != room || !want(compactionStepRecord{room: stepRoom, backoff: backoff, next: next}) {
			return
		}
		holding = true
		<-released
	}
	return release
}

// storeAgedRows stores rows rows of a new document through a store no service notes, each created
// age before the database's clock, and returns the document.
func storeAgedRows(t *testing.T, database *store.Store, rows int, age time.Duration) string {
	t.Helper()
	artifactID := createDocument(t, database, "aged")
	versioned := NewPgVersioned(database)
	writer := newProofHistory(t, 101)
	for index := range rows {
		appendUpdates(t, versioned, artifactID, writer.write(fmt.Sprintf("Row %d.\n", index)))
	}
	ageUpdatesThrough(t, database, artifactID, everyVersion, age)
	return artifactID
}

// loadStored loads the document as a room's load does (PgVersioned.Load), without a room.
func loadStored(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	if _, err := service.persistence.Load(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document: %v", err)
	}
}
