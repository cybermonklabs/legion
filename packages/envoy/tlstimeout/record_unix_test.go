//go:build unix

package tlstimeout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// R11 of dispatch://INFRA-3562 bounds a record in time, and every role of an issue pod shares the
// stamp: $TMPDIR is on the issue's volume and every role is one user. A record whose stamp another
// holder has locked, another record in its claim at that moment or a process stopped or stuck with
// the lock, returns while the lock is still held, writes nothing and runs no curl, as a record
// inside the minute does: of records made at once only one goes on. The stamp is a day old, so
// only the lock stops the record, and once the holder lets go a record writes its line.
func TestARecordWhoseStampAnotherHolderLocksWaitsForNothingAndWritesNothing(t *testing.T) {
	t.Parallel()
	rig := newRecordRig(t, curlTimesOut)
	if err := os.Mkdir(rig.r.StampDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(rig.r.StampDir, rig.r.Client)
	if err := os.WriteFile(stamp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dayOld := rig.r.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(stamp, dayOld, dayOld); err != nil {
		t.Fatal(err)
	}
	// A lock of an open file description of the test's own, which a record's open of the stamp is
	// not, so the record meets it as it meets another process's.
	holder, err := os.OpenFile(stamp, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	host := testport.Refused(t)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		rig.r.Record(host)
	}()
	testwait.Receive(t, "the record to return while another holder locks its stamp", returned)
	if rig.stderr.Len() != 0 || curlRuns(t, rig.curl) != 0 {
		t.Errorf("the record wrote %q and ran curl %d times while another holder locked its stamp, want nothing", rig.stderr.String(), curlRuns(t, rig.curl))
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	rig.r.Record(host)
	if written := rig.stderr.String(); !strings.HasPrefix(written, "agent-secrets: tls handshake timeout recorded: host="+host+" ") || curlRuns(t, rig.curl) != 1 {
		t.Errorf("a record once the holder let go wrote %q and ran curl %d times, want its line and one", written, curlRuns(t, rig.curl))
	}
}
