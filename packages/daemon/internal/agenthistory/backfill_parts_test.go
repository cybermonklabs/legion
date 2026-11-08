package agenthistory

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// AUDIT (A2). Oh My Pi under SQL storage keeps a session's bytes in omp_session_files_parts and
// leaves omp_session_files.content empty (proven against the pinned binary: the row's
// length(content) is 0 and two parts hold 4560 bytes). ompsessions.Titles reads the header out of
// substr(content, 1, 64k), so for every session Oh My Pi itself wrote the header is empty and the
// backfill skips the session as "it has no session header naming its id". The existing
// TestBackfillRecordsTitledOMPSessionsOfTheTableAndSkipsTheRest seeds its rows with
// ompsessions.Import, which puts the whole session in the content column, a layout only a
// file-to-table import produces, so it never sees this.
func TestAuditBackfillRecordsAnOMPSessionOhMyPiItselfWroteToTheTable(t *testing.T) {
	st, dsn := migratedStore(t)
	ctx := context.Background()
	conn, err := ompsessions.ConnectURL(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if err := ompsessions.CreateTables(ctx, conn); err != nil {
		t.Fatal(err)
	}
	path := "/home/legion/.omp/agent/sessions/--work--/ses-tester.jsonl"
	tester := token(t, "LEGION-7", claim.RoleTester)
	// Replace is the Go mirror of Oh My Pi's own writeFull: content '', the bytes in parts.
	content := ompSession("ses-tester", "", "", string(tester))
	if err := ompsessions.Replace(ctx, conn, path, ompsessions.Absent, []byte(content), time.Now()); err != nil {
		t.Fatal(err)
	}
	// Oh My Pi under SQL storage keeps the title in the row's column.
	if _, err := conn.Exec(ctx, "UPDATE omp_session_files SET title = $1, title_source = 'auto' WHERE path = $2",
		"Legion tester · LEGION-7", path); err != nil {
		t.Fatal(err)
	}

	log := &logs{}
	b := Backfill{Project: legionProject, Store: st, Now: func() time.Time { return backfillNow }, Log: log.logger()}
	if err := b.Table(ctx, conn); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.AgentConversation(ctx, "ses-tester")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("the tester's session Oh My Pi wrote to the table was not recorded; the backfill said: %v", log.lines())
	}
	if got.Token != tester || got.Location != path || !got.StartedAt.Equal(headerTime) {
		t.Errorf("recorded %+v, want the tester's conversation at %s started at %v", got, path, headerTime)
	}
}
