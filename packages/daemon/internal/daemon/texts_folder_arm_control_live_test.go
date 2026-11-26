//go:build e2e && textscontrol

// The arm of texts_folder_live_test.go's experiment as the negative control of
// scripts/e2e/texts-folder.sh runs it, over a tree of main, which has no folder of texts: the
// folder's roles/reviewer.md, Legion's text with folderProofRemoved taken out and the marker after
// it, given as the arm's instructions file. The tag textscontrol keeps it out of this checkout's
// own build, where texts_folder_arm_live_test.go stands in its place; the script copies it into
// the tree of main with the driver and builds with `-tags 'e2e textscontrol'`. It uses nothing main
// lacks: it builds no folder, sets no plugin gate and calls nothing the folder of texts added, so
// the reviewer's request holds Legion's roles/reviewer.md with the sentence beside that file, and
// its agents, skills and dispatch-first are Legion's, which fails the proof at reviewer-request.
package daemon

import (
	"path/filepath"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// folderProofArm writes the folder's roles/reviewer.md as an instructions file and answers the arm
// that names it.
func folderProofArm(r *folderProofLive) config.Variant {
	t := r.t
	t.Helper()
	file := filepath.Join(t.TempDir(), "reviewer-instructions.md")
	writeProofFile(t, file, folderProofReviewer(t, folderProofPrompt(t, r.shared.root, "roles/reviewer.md"), r.markers.reviewerRole))
	r.note("the arm %s names the instructions file %s: Legion's roles/reviewer.md with the sentence taken out and the marker %s",
		folderProofArmName, file, r.markers.reviewerRole)
	return config.Variant{Name: folderProofArmName, Weight: 1, InstructionsPath: file}
}

// folderProofOverrides adds nothing: a tree of main runs no plugin gate in a live proof.
func folderProofOverrides(r *folderProofLive, _ *overrides) {
	r.note("no plugin gate: the control's tree reads no shipped names")
}

// folderProofBootRead reads nothing: a boot of main logs no shipped names.
func folderProofBootRead(r *folderProofLive) {
	r.note("the control's boot logs no shipped names, and this run reads none")
}

// folderProofBreak changes nothing: the control runs the accepted case alone.
func folderProofBreak(r *folderProofLive) {
	r.t.Fatalf("the negative control runs the accepted case alone, and %s changes a folder the control has none of", r.c.name)
}
