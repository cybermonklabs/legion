package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// textsReports is where a launch of an arm's folder of texts reports: what it read, which the
// daemon's log records at INFO (read), and a launch that ran its fallback in place of the folder
// for a cause the daemon's own check found (specs.armTexts) or a runtime's (runtime.TextsFellBack),
// which the log names at WARN with the experiment and the arm, and the claim's issue is told of
// once for each claim and cause (fellBack).
//
// It is the seam between the launch's builders, the spec builder and the runtime, which boot
// makes before the workflow opens, and the workflow's fact path, which exists only once the
// workflow is open: prepare makes one, and the boot binds it to the workflow's pool and handlers
// the moment the workflow is open (plan.texts, daemon.run), before any claim's machine exists, so
// there is nothing to lock. One never bound, a daemon's with no workflow, logs alone.
type textsReports struct {
	log      *slog.Logger
	pool     *store.Pool
	handlers []intake.Handler
}

// bind gives the holder the daemon's guarded pool (store.Pool) and the handlers a fact applies
// through, which is what the workflow has and the launch's builders do not.
func (t *textsReports) bind(pool *store.Pool, handlers []intake.Handler) {
	t.pool, t.handlers = pool, handlers
}

// read logs, at INFO, what c's launch read of its arm's folder of texts (launched): the claim and
// its generation, the arm, the folder with its fingerprint, the daemon's copy, and each file the
// folder held with its digest, so the log records which text of each path the launch ran. A nil
// holder, a test's specs that made none, logs nothing.
func (t *textsReports) read(c supervise.Claim, launched *runtime.Texts, folder texts.Folder) {
	if t == nil {
		return
	}
	files := make(map[string]string, len(folder.Files))
	for path, body := range folder.Files {
		files[path] = texts.Digest(body)
	}
	t.log.Info("legion daemon launches with an arm's folder of texts", "claim", c.Token, "generation", c.Generation,
		"experiment", launched.Experiment, "arm", launched.Variant, "folder", launched.Folder, "fingerprint", launched.Fingerprint,
		"copy", launched.Copy, "files", files)
}

// fellBack logs, at WARN, one launch that ran its fallback in place of its arm's folder of texts
// (report): the claim and its generation, the experiment and the arm, the folder, the file (none
// where the fault is the folder's as a whole), the cause and the fingerprint the folder was read
// at. Then, where the holder is bound, it applies the fact that tells the claim's issue
// (intake.TextsFellBack) under the event id that names the claim and the cause
// (textsFallbackEventID), so the issue is told once for each claim and each cause, however many
// launches meet it and across restarts, since intake applies one event id once.
//
// Both the spec builder and the runtimes call it inside a launch, which the machine makes inside
// its own decision, so the fact is applied on a goroutine of its own, bounded by
// readyReleaseTimeout, as the session facts are (workflowRuntime.applySessionFact), and its
// outcome is logged: applied, applied already for this claim and cause, or failed. A launch that
// fell back runs whichever way the notice goes. A nil holder, a test's specs that made none, logs
// and tells nothing.
func (t *textsReports) fellBack(report runtime.TextsReport) {
	if t == nil {
		return
	}
	file := report.Fault.Path
	if file == "" {
		file = "none"
	}
	t.log.Warn("legion daemon launched Legion's own texts in place of an arm's folder of texts",
		"claim", report.Claim, "generation", report.Generation, "experiment", report.Experiment, "arm", report.Variant,
		"folder", report.Folder, "file", file, "cause", report.Fault.Cause, "fingerprint", report.Fingerprint, "detail", report.Fault.Detail)
	if t.pool == nil || len(t.handlers) == 0 || report.Issue == "" {
		return
	}
	id := textsFallbackEventID(report)
	fact := intake.TextsFellBack{Issue: report.Issue, Role: report.Role, Claim: report.Claim, Generation: report.Generation,
		Experiment: report.Experiment, Variant: report.Variant, Folder: report.Folder, Fingerprint: report.Fingerprint,
		File: report.Fault.Path, Cause: report.Fault.Cause, Detail: report.Fault.Detail}
	go func() {
		// This goroutine runs beside the launch that started it and every other, so it takes a
		// hold mark of its own (store.ForConcurrentWorker).
		ctx, cancel := context.WithTimeout(store.ForConcurrentWorker(context.Background()), readyReleaseTimeout)
		defer cancel()
		result, err := intake.ApplyFact(ctx, t.pool, "supervise", id, fact, t.handlers...)
		switch {
		case err != nil:
			t.log.Warn("apply the notice of a launch that ran Legion's own texts in place of an arm's folder of texts",
				"claim", report.Claim, "event", id, "error", err)
		case result.Duplicate:
			t.log.Info("legion daemon applied no second notice of a launch that ran Legion's own texts in place of an arm's folder of texts: its claim and cause were noticed already",
				"claim", report.Claim, "generation", report.Generation, "event", id)
		default:
			t.log.Info("legion daemon applied the notice of a launch that ran Legion's own texts in place of an arm's folder of texts",
				"claim", report.Claim, "generation", report.Generation, "event", id)
		}
	}()
}

// textsFallbackEventID is the event id the notice of report's fallback is applied under:
// `texts-fallback/<claim>/<the first 16 hex digits of the SHA-256 of the folder's fingerprint, a
// NUL, the file, a NUL and the cause>`. That is one id for each claim and each cause, a cause being
// the folder as it was read, the file and the reason: a later launch of the claim that meets the
// same cause applies nothing more, and a folder cut again is a new cause.
func textsFallbackEventID(report runtime.TextsReport) string {
	sum := sha256.Sum256([]byte(report.Fingerprint + "\x00" + report.Fault.Path + "\x00" + string(report.Fault.Cause)))
	return "texts-fallback/" + string(report.Claim) + "/" + hex.EncodeToString(sum[:8])
}
