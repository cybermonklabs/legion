package api

import (
	"fmt"
	"sync"

	"github.com/sjawhar/envoy/internal/cipher/natshold"
)

// recordFloors is a broker process's floor of each hold record: the highest gen of it the routes
// have written, deleted or served verified, and whether the routes deleted it. A database writer
// other than the operator cannot write a record that verifies, but can put back an older one the
// operator signed, which does, or delete one: the floor answers either as unknown (GET's 409
// below_floor) and refuses a write that would store a gen at or below it again (409
// gen_mismatch), so the operator's commands never act on a journal a command has moved past. A
// record the floor has seen is a tombstone once it is absent, since the absence is a database
// writer's, unless the routes deleted it. The DELETE route keeps the operator's signed delete (the
// cohort's, signed over the gen it deleted) in the delete's own statement, in
// nats_hold_record_deletes, and every replica reads it beside the records: on each, a record whose
// delete verifies at or above its floor is absent as it should be, and reads as unknown if it
// comes back. A delete that does not verify, or one signed over a gen below the floor, deletes
// nothing the floor has seen. Only a record whose signature verifies raises its floor, so a
// writer's unsigned record at a high gen moves nothing.
//
// The residuals, which the hold's design accepts:
//
//   - Each broker replica keeps its own floor, for as long as its process lives. A broker-database
//     writer can put back an older journal on a replica whose floor is below the put-back gen, at
//     any time, not only after a restart; production runs two replicas. A floor both replicas
//     share, kept outside the database (one Secret Manager secret the broker alone writes), is the
//     fix if that acceptance changes. A replica that starts again, or one that has not yet written
//     or served a record, has no floor for it at all. The auth service's floor of the hold row
//     (natscallout's holdView) is per replica too, but every connection decision reads the row, so
//     both replicas track its newest gen.
//   - After a legitimate restore of the broker's database to an earlier point, every broker
//     answers each record it has seen at a later gen 409 below_floor until it restarts.
//   - A kind with several keys put back as an older set that lacks a key is caught only by the
//     tombstone of the missing key; the keys put back at gens the floor has not passed read as
//     genuine.
//   - Cutover rows carry no gen of their own, so the routes keep no floor for them: an older signed
//     cutover row put back reads as genuine, on any replica.
//   - A token taken at CONNECT can read every hold route. The operator's platform token is the
//     NATS CONNECT password, and the routes take it as their bearer for its life, at most an hour:
//     every write needs the operator's signature as well, but no read does, so whoever holds the
//     token reads the hold row, the journal, the cohort and the cutover rows.
//
// Safe for concurrent use.
type recordFloors struct {
	mu   sync.Mutex
	gens map[recordFloorKey]recordFloor
}

// recordFloorKey names one hold record.
type recordFloorKey struct{ holdID, kind, key string }

// recordFloor is one record's floor: its gen, and whether the routes deleted the record at it.
type recordFloor struct {
	gen     uint64
	deleted bool
}

func newRecordFloors() *recordFloors { return &recordFloors{gens: map[recordFloorKey]recordFloor{}} }

// above answers the record's floor (0 for none), and whether gen, the gen a write would store, is
// above it.
func (f *recordFloors) above(k recordFloorKey, gen uint64) (uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gens[k].gen, gen > f.gens[k].gen
}

// raise moves the record's floor up to gen, a gen the routes stored, with the record present.
func (f *recordFloors) raise(k recordFloorKey, gen uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gens[k] = recordFloor{gen: max(f.gens[k].gen, gen)}
}

// deleted moves the record's floor up to gen, the gen the routes deleted it at, and marks it
// deleted, so its absence is no tombstone and its return is a put-back.
func (f *recordFloors) deleted(k recordFloorKey, gen uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gens[k] = recordFloor{gen: max(f.gens[k].gen, gen), deleted: true}
}

// gone reports whether the floor has seen the record k present and the routes did not delete it,
// so that its absence now is a database writer's, not a record that never was.
func (f *recordFloors) gone(k recordFloorKey) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	floor, seen := f.gens[k]
	return seen && !floor.deleted
}

// below reports whether gen, a gen a read found, is below the record's floor: an older signed
// version a database writer put back.
func (f *recordFloors) below(k recordFloorKey, gen uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return gen < f.gens[k].gen
}

// serve checks the records one read of holdID's kind answered against their floors, and answers
// why the read reads as unknown, or "" and raises the floor of every record verified accepts to
// its gen. A delete among deletions that deletedByRoutes verifies, at or above its record's floor,
// first marks that floor deleted at its gen, as the replica that served the delete marked its own.
// A read reads as unknown when a record is below its floor, when a record the routes deleted is
// back, and when a record the floor has seen, and the routes did not delete, is absent.
func (f *recordFloors) serve(holdID, kind string, records []natshold.Record, deletions []natshold.Deletion,
	verified func(natshold.Record) bool, deletedByRoutes func(natshold.Deletion) bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range deletions {
		k := recordFloorKey{holdID, kind, d.Key}
		if floor := f.gens[k]; d.Gen >= floor.gen && deletedByRoutes(d) {
			f.gens[k] = recordFloor{gen: d.Gen, deleted: true}
		}
	}
	present := map[string]bool{}
	for _, rec := range records {
		present[rec.Key] = true
		floor, seen := f.gens[recordFloorKey{holdID, kind, rec.Key}]
		switch {
		case seen && floor.deleted:
			return fmt.Sprintf("the %s record %s of hold %s, which the routes deleted at gen %d, is back at gen %d: a deleted record put back, which reads as unknown",
				kind, rec.Key, holdID, floor.gen, rec.Gen)
		case seen && rec.Gen < floor.gen:
			return fmt.Sprintf("the %s record %s of hold %s is at gen %d, below gen %d, which this broker has written or served: an older record put back, which reads as unknown",
				kind, rec.Key, holdID, rec.Gen, floor.gen)
		}
	}
	for k, floor := range f.gens {
		if k.holdID == holdID && k.kind == kind && !floor.deleted && !present[k.key] {
			return fmt.Sprintf("the %s record %s of hold %s, which this broker has written or served at gen %d, is absent: a record deleted outside the routes, which reads as unknown",
				kind, k.key, holdID, floor.gen)
		}
	}
	for _, rec := range records {
		if verified(rec) {
			f.gens[recordFloorKey{holdID, kind, rec.Key}] = recordFloor{gen: rec.Gen}
		}
	}
	return ""
}
