package redact

import (
	"strings"
	"testing"
	"time"
)

// longNameReads is how many times the row below reads its field, and longNameBound how long the
// least of those reads may take: the least is the read's own cost, and what a busy box adds to one
// read is not. With the name capped at 32 letters (characterReference), the least of five reads
// took 2.2 ms at a load average of 130 to 150 on 288 CPUs (this box runs at 100 to 1200),
// GOMAXPROCS 8, and the bound is more than ten times that; with no cap, each pass matched the
// 256 KiB name whole twice and the least read took 117 ms, almost four times the bound.
const (
	longNameReads = 5
	longNameBound = 30 * time.Millisecond
)

// One character reference whose name is the field's whole length (&ampampamp...;, 256 KiB), which
// another party can write: decodeSeparators reads it in at most ReferencePasses+1 passes, each pass
// losing one amp, and answers it is not readable, within longNameBound.
func TestALongReferenceNameIsReadInBoundedTime(t *testing.T) {
	field := "&" + strings.Repeat("amp", (256<<10)/3) + ";"
	least := time.Duration(1<<63 - 1)
	for range longNameReads {
		began := time.Now()
		decoded, readable := decodeSeparators(field)
		least = min(least, time.Since(began))
		if readable || decoded != "" {
			t.Fatalf("decodeSeparators of the %d-byte &ampamp...; field = %.40q..., %t; want \"\", false", len(field), decoded, readable)
		}
	}
	t.Logf("the least of %d reads of the %d-byte &ampamp...; field took %s", longNameReads, len(field), least)
	if least > longNameBound {
		t.Errorf("the least of %d reads of the %d-byte &ampamp...; field took %s, more than %s", longNameReads, len(field), least, longNameBound)
	}
}
