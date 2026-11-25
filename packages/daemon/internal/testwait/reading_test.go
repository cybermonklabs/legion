package testwait

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// freshness is a reading shaped as the delivery hub's freshness (deliveryFreshnessHeard in
// internal/dispatch/api): unexported pointer fields, one of them nil.
type freshness struct {
	lastEventAt, lastReconcileAt *time.Time
	lastError                    *string
	unfetchable                  int
}

// node is a reading that can hold itself.
type node struct {
	name string
	next *node
}

// A reading prints what its pointers hold, or <nil>, never an address, wherever the pointer sits:
// a field, a slice, a map, an interface, a pointer to a pointer. A value with a String or Error
// method prints by it though its field is unexported, and a reading that holds itself ends. Each
// row's control is %+v of the same reading, which prints an address, so a row that names no
// address proves describe, not the reading, left it out.
func TestAReadingPrintsWhatItsPointersHoldNotTheirAddresses(t *testing.T) {
	at := time.Date(2026, 11, 25, 10, 18, 6, 0, time.UTC)
	failed := "the listener's read failed"
	count := 3
	pointer := &count
	loop := &node{name: "a"}
	loop.next = &node{name: "b", next: loop}
	for _, row := range []struct {
		name    string
		reading any
		want    string
	}{
		{"unexported pointer fields", freshness{lastReconcileAt: &at, lastError: &failed, unfetchable: 2},
			"{lastEventAt:<nil> lastReconcileAt:2026-11-25 10:18:06 +0000 UTC lastError:the listener's read failed unfetchable:2}"},
		{"a pointer to the reading", &freshness{lastEventAt: &at},
			"&{lastEventAt:2026-11-25 10:18:06 +0000 UTC lastReconcileAt:<nil> lastError:<nil> unfetchable:0}"},
		{"a slice, a map and a pointer to a pointer", struct {
			counts []*int
			byName map[string]*int
			twice  **int
		}{[]*int{&count, nil}, map[string]*int{"b": nil, "a": &count}, &pointer},
			"{counts:[3 <nil>] byName:map[a:3 b:<nil>] twice:3}"},
		{"an error and an interface", struct {
			err  error
			held any
		}{errors.New("no reconcile yet"), &at},
			"{err:no reconcile yet held:2026-11-25 10:18:06 +0000 UTC}"},
		{"a reading that holds itself", loop, "&{name:a next:&{name:b next:<cycle>}}"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if control := fmt.Sprintf("%+v", row.reading); !strings.Contains(control, "0x") {
				t.Fatalf("the control %%+v printed %s, with no address: this row tests nothing", control)
			}
			if got := describe(row.reading); got != row.want {
				t.Errorf("describe printed\n%s\nwant\n%s", got, row.want)
			}
		})
	}
	if got := describe(nil); got != "<nil>" {
		t.Errorf("describe(nil) printed %s, want <nil>", got)
	}
	if got := describe("a later\nreading"); got != "a later\nreading" {
		t.Errorf("describe of a string printed %q, want the string", got)
	}
}
