package ompsessions

import (
	"context"
	"testing"
	"time"
)

// AUDIT: a row Oh My Pi itself wrote keeps its bytes in omp_session_files_parts and leaves
// content empty (Replace is the Go mirror of Oh My Pi's writeFull; the pinned binary writes the
// same layout). Titles reads the header out of substr(content, 1, 64k), so it reports no header.
func TestAuditTitlesReadsTheHeaderOfAnOhMyPiWrittenRow(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()
	if err := CreateTables(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := Replace(ctx, conn, "/s/omp.jsonl", Absent, []byte(describedSession), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "UPDATE omp_session_files SET title = 'Legion tester · INFRA-204', title_source = 'auto' WHERE path = '/s/omp.jsonl'"); err != nil {
		t.Fatal(err)
	}
	var contentLen, byteLen int64
	if err := conn.QueryRow(ctx, "SELECT length(content), byte_len FROM omp_session_files WHERE path = '/s/omp.jsonl'").Scan(&contentLen, &byteLen); err != nil {
		t.Fatal(err)
	}
	t.Logf("row: length(content)=%d byte_len=%d", contentLen, byteLen)
	titled, err := Titles(ctx, conn)
	if err != nil || len(titled) != 1 {
		t.Fatalf("Titles = %v, %v", titled, err)
	}
	got := titled[0]
	if got.ID != "0001" || got.Timestamp != "2026-10-26T00:00:00.000Z" || got.ParentSession != "/sessions/parent.jsonl" {
		t.Errorf("Titles of a row Oh My Pi wrote = %+v; want the header's id 0001, its timestamp and its parentSession", got)
	}
}
