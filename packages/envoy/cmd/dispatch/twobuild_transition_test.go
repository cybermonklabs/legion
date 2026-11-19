//go:build twobuild

// The transition half of the two-build suite: what this tree's page admission does while a
// process of an earlier build is live beside this tree's, and what it does once that process has
// left the registry. One row runs the first-rollout build, a release that registers no schema
// numbers; the other runs the previous release, which a roll replaces
// (twobuild_harness_test.go holds the harness the builds start with, and
// twobuild_documents_test.go the page parameters and the socket a page opens).

package main

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/testwait"
)

// readOnlyEditSettle is how long an edit sent through a read-only socket is given to appear
// anywhere before the row reads the document again: past the document service's own settle delay
// (two seconds), so an edit the server took would be in the text by then. It is a span of the
// row's own (readOnlyEditWhy).
const readOnlyEditSettle = 4 * time.Second

// readOnlyEditWhy is why the transition row holds readOnlyEditSettle as a span: the server drops
// an edit a read-only socket sends with no answer the editor reads and no state the row can, both
// builds are child processes so synctest does not reach them, and a later edit through a writable
// path would change the very text the row reads unchanged.
const readOnlyEditWhy = "the server drops a read-only socket's edit with no answer the editor reads and no state the " +
	"row can read, both builds are child processes so synctest does not reach them, and a later edit through a " +
	"writable path would change the very text the row reads unchanged"

// admissionRecheck is how long a writable socket is held open while the previous build is live
// before the row asks whether it is still open: a live window, which is how often each process
// checks its writable sockets against the fleet again and closes each one the fleet no longer
// admits (docs.Service.RunSchemaAdmission), and admissionRecheckSlack on top. It is a span of the
// row's own (admissionRecheckWhy).
func admissionRecheck() time.Duration {
	return processes.LiveWindow + admissionRecheckSlack
}

// admissionRecheckSlack is what the span above allows a loaded runner past the live window for
// the pass that window brings, and the close it would send, to have run.
const admissionRecheckSlack = 5 * time.Second

// admissionRecheckWhy is why the transition row holds admissionRecheck as a span: each process
// checks its writable sockets against the fleet on a ticker of its own and records a pass only
// when it closes a socket, both builds are child processes so synctest does not reach them, and a
// later message on the socket is answered by the document's room, which says nothing of a pass
// still to come.
const admissionRecheckWhy = "each process checks its writable sockets against the fleet on a ticker of its own and " +
	"records a pass only when it closes a socket, both builds are child processes so synctest does not reach " +
	"them, and a later message on the socket is answered by the document's room, which says nothing of a pass " +
	"still to come"

// transitionSeed is the document the row seeds, one paragraph a replacement is made over
// (replaceText), and transitionRender what a build renders it as.
const (
	transitionSeed   = "before"
	transitionRender = transitionSeed + "\n"
)

// openPageEditor connects an editor to artifactID's room on server with page's parameters - the
// query a dashboard of that build opens its document socket with, schema_creatable included - and
// returns once the editor's copy renders want. It is dispatchProcess.openEditor
// (shutdown_test.go) for a page whose numbers are not the answering process's own.
func openPageEditor(t *testing.T, server *dispatchProcess, artifactID string, page pageParameters, want string) *docstest.Peer {
	t.Helper()
	address := "ws://127.0.0.1:" + strconv.Itoa(server.port) + "/ws/doc/" + artifactID + "?" + page.query.Encode()
	editor := docstest.Dial(t, address, http.Header{"X-Dispatch-User": {dispatchTestLogin}}, artifactID, crdt.New())
	if err := editor.WaitForText(t, markdown, want); err != nil {
		t.Fatalf("%v:\n%s", err, server.Output.String())
	}
	return editor
}

// waitForThePreviousBuildToLeaveTheFleet reads the registry until the current build's process is
// the only live row, and returns how long that took. It counts rows rather than reading their
// numbers, since a previous build that registers this build's own numbers leaves the fleet's
// numbers as they were.
func waitForThePreviousBuildToLeaveTheFleet(t *testing.T, builds *twoBuilds) time.Duration {
	t.Helper()
	started := time.Now()
	testwait.Until(t, "the "+builds.previousName()+"'s row to leave the registry, so the current build's process is the only live row", func() (int, bool) {
		fleet := builds.fleet(t)
		return fleet.live, fleet.live == 1
	})
	return time.Since(started)
}

// requireDocumentUnchanged fails unless both builds still render the document as want, and names
// what landed where they do not.
func requireDocumentUnchanged(t *testing.T, builds *twoBuilds, artifactID, want, when string) {
	t.Helper()
	for _, side := range []*dispatchProcess{builds.current(), builds.previous()} {
		if got := side.documentText(t, artifactID); got != want {
			t.Fatalf("%s: the %s renders the document as\n%q\nwant it untouched:\n%q", when, builds.buildName(side), got, want)
		}
	}
}

// requireEditLands waits until server renders the document as want; a failure names what it
// renders instead and when the edit was made.
func requireEditLands(t *testing.T, server *dispatchProcess, artifactID, want, when string) {
	t.Helper()
	testwait.Until(t, "the edit made through the writable socket "+when+" to land on the current build as "+strconv.Quote(want), func() (string, bool) {
		got := server.documentText(t, artifactID)
		return got, got == want
	})
}

// requirePageAcrossTheDeparture drives the transition a rolling release makes, with a process of
// each build started. While both are live, a document socket of this tree's own page on this
// tree's server is admitted as whileLive: an edit through a read-only socket lands nowhere, and an
// edit through a writable one lands, and that socket is still open after the fleet has been
// checked against it again. Once the previous build's process has stopped and its row has left
// the registry, the socket a reload opens - the same page, the same numbers - is admitted
// writable, and an edit through it lands on this build's server.
//
// The wait for the row to leave reads the registry until it has: a process stopped as a deploy
// stops it deletes its row at the end of its HTTP drain, so it leaves at once, and only a process
// killed without deregistering spends a live window (processes.LiveWindow) and a heartbeat.
func requirePageAcrossTheDeparture(t *testing.T, builds *twoBuilds, page pageParameters, whileLive string) {
	t.Helper()
	current := builds.current()
	project := uniqueProjectKey(t, "XBT")
	current.createProject(t, project, "Two builds, one leaving")
	_, artifactID := createDocument(t, current, project, "The fleet changes under an open page", transitionRender)

	socket := openDocumentSocket(t, current, artifactID, page)
	if socket.scope != whileLive {
		t.Fatalf("with the %s live, the current build admits a socket of its own page (%s) as %q, want %q",
			builds.previousName(), page, socket.scope, whileLive)
	}

	editor := openPageEditor(t, current, artifactID, page, transitionRender)
	text := transitionSeed
	if whileLive == readOnlyScope {
		const refused = "typed while the previous build was live"
		replaceText(t, editor, transitionSeed, refused)
		testwait.Span(t, readOnlyEditSettle, readOnlyEditWhy)
		requireDocumentUnchanged(t, builds, artifactID, transitionRender, "an edit sent through the read-only socket")
	} else {
		text = "typed while the previous build was live"
		replaceText(t, editor, transitionSeed, text)
		requireEditLands(t, current, artifactID, text+"\n", "while the previous build was live")
		// A window that wrongly closes shows as this socket's close: each process checks its
		// writable sockets against the fleet once a live window and closes each one it no longer
		// admits.
		testwait.Span(t, admissionRecheck(), admissionRecheckWhy)
	}
	if socket.closed() {
		t.Errorf("the current build closed the socket it admitted %s while the %s was live: %v", whileLive, builds.previousName(), socket.endErr)
	}
	editor.Close()

	stop(t, builds.previous())
	waited := waitForThePreviousBuildToLeaveTheFleet(t, builds)
	t.Logf("the %s left the fleet %s after its stop", builds.previousName(), waited.Round(time.Millisecond))

	reloaded := openDocumentSocket(t, current, artifactID, page)
	if reloaded.scope != writableScope {
		t.Fatalf("once the %s had left the fleet, the current build admits a socket of the same page (%s) as %q, want %q",
			builds.previousName(), page, reloaded.scope, writableScope)
	}

	writing := openPageEditor(t, current, artifactID, page, text+"\n")
	const landed = "typed once the previous build had gone"
	replaceText(t, writing, text, landed)
	requireEditLands(t, current, artifactID, landed+"\n", "once the previous build had gone")
	if reloaded.closed() {
		t.Errorf("the current build closed the socket it admitted writable: %v", reloaded.endErr)
	}
	if whileLive == writableScope && socket.closed() {
		t.Errorf("the current build closed the socket it admitted writable while the %s was live, once that build had left: %v",
			builds.previousName(), socket.endErr)
	}
}

// This tree's page admission is the whole fleet's, so a process of a build that registers no
// schema numbers holds this tree's own pages read-only while it lives: its row's zeros count as a
// process that reads version 0, and no page creates at or under that (docs.readSchemaFleet). That
// is the first rollout of the fleet's numbers, and this row drives it: with both builds live, a
// document socket of this tree's page on this tree's server is admitted read-only and an edit sent
// through it lands nowhere; once the earlier build's process has stopped and its row has left the
// registry, the socket a reload opens is admitted writable, and an edit through it lands
// (requirePageAcrossTheDeparture).
//
// Its earlier build is the first-rollout build (DISPATCH_FIRST_ROLLOUT_BUILD), a release before
// the first that registers its numbers, and not the previous release: from that first release on,
// the previous release registers numbers as well, and the read-only window it opens, if any, is
// the next row's to drive.
func TestThisBuildsPageTurnsWritableOnceThePreviousBuildsProcessLeavesTheFleet(t *testing.T) {
	builds := newFirstRolloutBuilds(t)
	builds.startPrevious(t, nil)
	builds.startCurrent(t, nil)
	page := builds.documentSides(t)[1].page

	if fleet := builds.fleet(t); fleet.minVersion != 0 {
		t.Fatalf("the fleet's lowest schema version is %d with the %s live; this row needs a build that registers no schema numbers, whose row reads 0, so %s must name a release before the first that registers them",
			fleet.minVersion, builds.previousName(), firstRolloutBuildVariable)
	}
	requirePageAcrossTheDeparture(t, builds, page, readOnlyScope)
}

// The previous release, the build a roll replaces, is admitted by the numbers it registers. When
// they are this tree's own - every roll that leaves the document schema manifest as it was - the
// live fleet reads as this tree alone, so this tree's page is writable from its first socket, an
// edit through it lands while both builds are live, and the socket stays open through a check of
// the fleet and through the previous build's departure: a roll that changes no schema opens no
// read-only window. A previous release with other numbers, one before a manifest bump or one
// before the first that registers its numbers, opens the window the admission rule states
// (admittedScope, twobuild_documents_test.go) while it lives, and the page is writable once it has
// left (requirePageAcrossTheDeparture). The row runs whichever the previous release is and skips
// neither case, since a skip would hide a window that wrongly opens or wrongly stays shut.
func TestThisBuildsPageIsAdmittedByThePreviousReleasesNumbersWhileItLives(t *testing.T) {
	builds := newTwoBuilds(t)
	builds.startPrevious(t, nil)
	builds.startCurrent(t, nil)
	this := builds.documentSides(t)[1]

	fleet := builds.fleet(t)
	ownNumbers := fleet.minVersion == this.manifest.Version && fleet.maxCreatable == this.page.creatable
	whileLive := admittedScope(this, this, fleet)
	if ownNumbers {
		whileLive = writableScope
	}
	t.Logf("with the %s live, the fleet reads version %d and creates up to %d, and this build reads %d and creates up to %d (the fleet reads as this build alone: %t), so its page is admitted %s",
		builds.previousName(), fleet.minVersion, fleet.maxCreatable, this.manifest.Version, this.page.creatable, ownNumbers, whileLive)
	requirePageAcrossTheDeparture(t, builds, this.page, whileLive)

	if alone := builds.fleet(t); alone.minVersion != this.manifest.Version || alone.maxCreatable != this.page.creatable {
		t.Errorf("with the current build alone, the fleet reads version %d and creates up to %d, but its manifest reads %d and creates up to %d: the row compared the fleet with numbers this build does not register",
			alone.minVersion, alone.maxCreatable, this.manifest.Version, this.page.creatable)
	}
}
