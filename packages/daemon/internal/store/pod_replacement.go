package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// PodReplacement is the record of a replacement of an issue's pod that may take a disk (R7 of
// #5285, GPT-6.1 Sol's points 1 and 2 on #5285): the daemon writes it before the
// runtime acts, and deletes it once the act is made, with every other record of a target the act
// took, in the transaction that queues the issue's architect's notice of what the act took
// (FinishPodReplacement), or once the runtime shows the act was not made (ForgetPodReplacement).
// Operation is the act's own identity, which keys the row (pod_replacements): two acts on the same
// pod are two rows, and no write goes over a row that stands (GPT-6.1 Sol's point 3 on #5285 at
// fa9deb04). PodUID is the pod the act takes, "" where
// the issue's Sandbox ran no pod of its own. Act is what the act takes and the uid of its target
// (runtime.RecordedAct; GPT-6.1 Sol's point 4), which the daemon has the runtime read to say
// whether the act was made. WithPod is the act's runtime.TakenPod.WithPod: a launch's delete of the
// issue's Sandbox and its kept volume over PodUID, a live pod of that Sandbox, which takes that pod
// with the volume (Targets). Record is the daemon's own encoding of the replacement, which the
// store keeps whole.
type PodReplacement struct {
	Operation string
	Issue     string
	PodUID    string
	Act       runtime.RecordedAct
	WithPod   bool
	Record    []byte
}

// Targets is every target the act r records takes: its own (Act), and, for a delete of the issue's
// Sandbox and its kept volume over a live pod of that Sandbox (WithPod), that pod (runtime.PodAct).
// It is the one rule of what an act takes, which the finish of a record reads to say which records
// go with it (FinishPodReplacement), and the daemon's sweep reads to say which records it decided
// (internal/daemon's finishReplacements; the Legion reviewer's Important on #5285 at 69e00593).
func (r PodReplacement) Targets() []runtime.RecordedAct {
	if r.WithPod && r.PodUID != "" {
		return []runtime.RecordedAct{r.Act, runtime.PodAct(r.PodUID)}
	}
	return []runtime.RecordedAct{r.Act}
}

// RecordPodReplacement writes r, the record of a replacement of one of project's issue pods, with
// the moment of this write. A row of r's operation that stands already is kept as it is: a record is
// never written over.
func (s *Store) RecordPodReplacement(ctx context.Context, project string, r PodReplacement) error {
	if project == "" || r.Operation == "" || r.Issue == "" || r.Act.Takes == "" || r.Act.Target == "" || len(r.Record) == 0 {
		return errors.New("the record of a pod's replacement needs its project, its operation, its issue, what it takes, its target and the record")
	}
	if err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into pod_replacements (project, operation, issue, pod_uid, takes, target_uid, with_pod, record, written_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, now())
			on conflict (project, operation) do nothing`,
			project, r.Operation, r.Issue, r.PodUID, string(r.Act.Takes), r.Act.Target, r.WithPod, string(r.Record))
		return err
	}); err != nil {
		return fmt.Errorf("record the replacement %s of %s's pod %s: %w", r.Operation, r.Issue, r.PodUID, err)
	}
	return nil
}

// standingReplacement is a record that stands, with the moment its row was written.
type standingReplacement struct {
	PodReplacement
	written time.Time
}

// scanReplacements is every record rows hold, in their order: rows select operation, issue,
// pod_uid, takes, target_uid, with_pod, record and written_at.
func scanReplacements(rows pgx.Rows) ([]standingReplacement, error) {
	defer rows.Close()
	var standing []standingReplacement
	for rows.Next() {
		var (
			r             standingReplacement
			takes, record string
		)
		if err := rows.Scan(&r.Operation, &r.Issue, &r.PodUID, &takes, &r.Act.Target, &r.WithPod, &record, &r.written); err != nil {
			return nil, err
		}
		r.Act.Takes, r.Record = runtime.Takes(takes), []byte(record)
		standing = append(standing, r)
	}
	return standing, rows.Err()
}

// PodReplacements is every record of a replacement of project's issue pods that stands, oldest
// first.
func (s *Store) PodReplacements(ctx context.Context, project string) ([]PodReplacement, error) {
	var standing []PodReplacement
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select operation, issue, pod_uid, takes, target_uid, with_pod, record::text, written_at
			from pod_replacements where project = $1 order by written_at, operation`, project)
		if err != nil {
			return err
		}
		read, err := scanReplacements(rows)
		if err != nil {
			return err
		}
		standing = make([]PodReplacement, 0, len(read))
		for _, r := range read {
			standing = append(standing, r.PodReplacement)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the records of %s's pod replacements: %w", project, err)
	}
	return standing, nil
}

// PodReplacementFinish is what a finish of a record did (FinishPodReplacement).
type PodReplacementFinish struct {
	// Found is whether the record stood. A record that another finish took, or that a finish of
	// the same act took before, is not found, and nothing is queued for it.
	Found bool
	// Yielded is whether the finish yielded to the record of another act that took the target: it
	// deleted the record alone, as the record of an act not made, and queued nothing.
	Yielded bool
	// Took is every record the finish took, oldest first: the finished record and each record its
	// act took with it (takenWith). It is empty where the record was not found or the finish
	// yielded.
	Took []PodReplacement
	// Told is whether the notice was queued.
	Told bool
}

// FinishPodReplacement finishes the record of operation, a replacement of issue's pod whose act is
// made. In one transaction it deletes that record and every standing record of the issue that the
// act took with it (takenWith), and queues notice, where it is not nil, as the issue's outbox row
// (insertOutboxRow). A target is taken once, so every act recorded on a target the act took is
// over once the act is made, and the issue's architect is told of it once (the Legion reviewer's
// Important 1 on #5285 at 7da847c0): two acts on one target each stand recorded under an operation
// of their own (a replacement by issue and the launch that replaced the round's pod while it
// waited, a launch that failed and the act after it, a launch that stopped before its delete and
// the next launch), and the runtime reads each made from the target alone. The finish reads the
// finished record whole from its row, and the issue's other records with their rows locked, and
// decides in Go which of them go, by the targets each names (PodReplacement.Targets), so the rule
// of what an act takes is written once (the Legion reviewer's Important on #5285 at 69e00593). The
// notice is queued exactly when the records go, so a store that refuses either keeps them all as
// they were, and a later finish of any of those records finds nothing and queues nothing. A nil
// notice finishes the records with nothing queued: an act made over a read that found nothing,
// which is the last read of the target before it was taken. A notice of an issue of another
// project than project is queued by no daemon of this one, so the records go with no row queued,
// and Told says so.
//
// yield is a finish whose caller's own call took nothing of the record's target: the runtime found
// the target taken already, and answered without asking the act's last word. Where a standing
// record of another act names that target beside its own (Targets: a launch's delete of the
// issue's Sandbox and its kept volume over the pod, WithPod), that act took it, and the finish
// yields to it: it deletes the finished record alone, as the record of an act not made, and queues
// nothing, and the other record tells the architect once its own act is confirmed (the Legion
// reviewer's Important on #5285 at 69e00593, path a). Where no such record stands, the finish
// takes what the act took, as every other finish does.
func (s *Store) FinishPodReplacement(ctx context.Context, project, issue, operation string, yield bool, notice *IdleReleaseRow) (PodReplacementFinish, error) {
	var finish PodReplacementFinish
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		finish = PodReplacementFinish{}
		finished := standingReplacement{PodReplacement: PodReplacement{Operation: operation, Issue: issue}}
		var takes, record string
		err := tx.QueryRow(ctx, `delete from pod_replacements where project = $1 and issue = $2 and operation = $3
			returning pod_uid, takes, target_uid, with_pod, record::text, written_at`, project, issue, operation).Scan(
			&finished.PodUID, &takes, &finished.Act.Target, &finished.WithPod, &record, &finished.written)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		finished.Act.Takes, finished.Record = runtime.Takes(takes), []byte(record)
		finish.Found = true
		rows, err := tx.Query(ctx, `select operation, issue, pod_uid, takes, target_uid, with_pod, record::text, written_at
			from pod_replacements where project = $1 and issue = $2 order by written_at, operation for update`, project, issue)
		if err != nil {
			return err
		}
		standing, err := scanReplacements(rows)
		if err != nil {
			return err
		}
		if yield && yieldsTo(finished.PodReplacement, standing) {
			finish.Yielded = true
			return nil
		}
		took := takenWith(finished.PodReplacement, standing)
		if len(took) > 0 {
			operations := make([]string, 0, len(took))
			for _, r := range took {
				operations = append(operations, r.Operation)
			}
			if _, err := tx.Exec(ctx, `delete from pod_replacements where project = $1 and issue = $2 and operation = any($3)`,
				project, issue, operations); err != nil {
				return err
			}
		}
		took = append(took, finished)
		slices.SortStableFunc(took, func(a, b standingReplacement) int {
			return cmp.Or(a.written.Compare(b.written), cmp.Compare(a.Operation, b.Operation))
		})
		for _, r := range took {
			finish.Took = append(finish.Took, r.PodReplacement)
		}
		if notice == nil || !claim.KeyOfProject(project, issue) {
			return nil
		}
		if _, err := insertOutboxRow(ctx, tx, project, issue, *notice); err != nil {
			return err
		}
		finish.Told = true
		return nil
	})
	if err != nil {
		return PodReplacementFinish{}, fmt.Errorf("finish the replacement %s of %s's pod: %w", operation, issue, err)
	}
	return finish, nil
}

// takenWith is every record of standing that the act finished records takes with it, by the
// targets each names (PodReplacement.Targets). The act takes its own targets. A record whose own
// act is on a taken target is over, and so is each target it names beside its own: the delete of a
// Sandbox took the live pod of that Sandbox (WithPod), so a record of that pod's own act goes too,
// whichever record of the Sandbox's delete is the one finished. Every record that names a taken
// target goes. A record that names a taken target beside its own act's target goes, but the
// target of its own act is not taken by that: the suspension of a pod takes no volume, so the
// finish of a round's act on the pod takes a launch's record of the Sandbox's delete over that pod,
// and no other record of that Sandbox.
func takenWith(finished PodReplacement, standing []standingReplacement) []standingReplacement {
	taken := map[runtime.RecordedAct]bool{}
	for _, target := range finished.Targets() {
		taken[target] = true
	}
	for grew := true; grew; {
		grew = false
		for _, r := range standing {
			if !taken[r.Act] {
				continue
			}
			for _, target := range r.Targets() {
				if !taken[target] {
					taken[target], grew = true, true
				}
			}
		}
	}
	var took []standingReplacement
	for _, r := range standing {
		if slices.ContainsFunc(r.Targets(), func(target runtime.RecordedAct) bool { return taken[target] }) {
			took = append(took, r)
		}
	}
	return took
}

// yieldsTo is whether a record of standing names the act that finished records as a target it
// takes beside its own (PodReplacement.Targets): another act took that target with its own.
func yieldsTo(finished PodReplacement, standing []standingReplacement) bool {
	return slices.ContainsFunc(standing, func(r standingReplacement) bool {
		return r.Act != finished.Act && slices.Contains(r.Targets(), finished.Act)
	})
}

// ForgetPodReplacement deletes the record of operation, a replacement of issue's pod whose act was
// not made: the runtime shows the target still in use, or the act's last word refused it. It
// deletes that record alone, since an act not made says nothing of another act on the same
// target, which the runtime reads of its own.
func (s *Store) ForgetPodReplacement(ctx context.Context, project, issue, operation string) error {
	if err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `delete from pod_replacements where project = $1 and issue = $2 and operation = $3`, project, issue, operation)
		return err
	}); err != nil {
		return fmt.Errorf("forget the replacement %s of %s's pod: %w", operation, issue, err)
	}
	return nil
}
