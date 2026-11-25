package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The project census tells which document row a live node stands for by the row's own create
// event only where a removal can take the row away (graphcensus's removableDocument): only such
// a row gives up its address, so only its address is taken by a later row, and for every other
// document a live node is enough. The two sets must be one: a document the removal route takes
// that the census reads by live alone is one whose address a later row takes over unseen, and a
// document the census reads by its event that the removal never takes is one whose create records
// no artifact.created of its own (an issue's spec, a conversation's file), which the census would
// name gone forever.
//
// Every shape of document row the store holds - its owner (an issue, a project, a conversation),
// its kind, whether it is its issue's spec and the role it plays - is written in its own
// transaction, with a live node whose statement another row's create event made, and the census
// read in it: it must name the row gone exactly where the removal's own test of the row
// (artifactRowIsUntouched) lets a removal take it. A shape the store refuses is no shape a removal
// can meet, and is skipped.
//
// Its control is a removable kind planted in the removal and left out of the census: the removal's
// test made to take a file as well, which the census then reads by live alone.
func TestEveryDocumentARemovalCanTakeIsReadByItsCreateEvent(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "SHP", "The shapes of a document", "The spec.\n")
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	alice := model.Actor{Kind: "user", ID: "alice"}
	var held, removable int
	for index, shape := range documentShapes() {
		name := fmt.Sprintf("%s %s primary=%t role=%q", shape.owner, shape.kind, shape.primary, shape.role)
		err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			id, ref, err := insertDocumentShape(ctx, tx, index, shape)
			if err != nil {
				return errShapeRefused{err}
			}
			artifact, err := loadArtifactRowByID(ctx, tx, id)
			if err != nil {
				return err
			}
			// Another row's create event: it names an id no row holds.
			var other int64
			if err := tx.QueryRow(ctx, `
				insert into events (issue_key, seq, type, actor, payload, notify)
				values ($1, $2, 'artifact.created', '{"kind":"user","id":"alice"}',
				        jsonb_build_object('artifact', jsonb_build_object('id', gen_random_uuid()::text)), false)
				returning id
			`, issue.Key, 100000+index).Scan(&other); err != nil {
				return err
			}
			made := graphstore.PlanUncaused([]graphstore.Node{
				graphstore.ArtifactNode(ref, artifact.Project, deref(artifact.SessionID), alice, at)}, nil)
			if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
				graphstore.QueueNodes(b, made)
				return nil
			}); err != nil {
				return err
			}
			if err := graphstore.Caused(ctx, tx, made, other); err != nil {
				return err
			}
			incomplete, err := graphcensus.ProjectComplete(ctx, tx)
			if err != nil {
				return err
			}
			named := slices.Contains(incomplete, "artifact "+ref+": gone; run envoy-dispatch backfill-graph-store")
			takes := artifactRowIsUntouched(artifact) == nil
			held++
			if takes {
				removable++
			}
			if named != takes {
				t.Errorf("%s: the removal takes it %t, the census reads it by its create event %t; want the two the same", name, takes, named)
			}
			return errRollBack
		})
		var refused errShapeRefused
		switch {
		case err == nil:
			t.Fatalf("%s: the shape's transaction committed", name)
		case errors.As(err, &refused):
			continue
		case !errors.Is(err, errRollBack):
			t.Fatalf("%s: %v", name, err)
		}
	}
	if held == 0 || removable == 0 || removable == held {
		t.Fatalf("the store held %d shape(s), %d of them removable; want shapes of both sets, so this test proves nothing otherwise", held, removable)
	}
	t.Logf("%d shapes of document row, %d of them removable", held, removable)
}

// documentShape is one shape of document row: who owns it, its kind, whether it is its issue's
// spec and the role it plays.
type documentShape struct {
	owner, kind, role string
	primary           bool
}

// documentShapes is every shape the artifacts table's columns allow a row to take: each owner, each
// kind its check allows, spec or not, and each role its check allows.
func documentShapes() []documentShape {
	var shapes []documentShape
	for _, owner := range []string{"issue", "project", "conversation"} {
		for _, kind := range []string{"doc", "image", "file"} {
			for _, primary := range []bool{false, true} {
				for _, role := range []string{"", "verification", "plan", "decisions"} {
					shapes = append(shapes, documentShape{owner: owner, kind: kind, role: role, primary: primary})
				}
			}
		}
	}
	return shapes
}

// insertDocumentShape writes, in tx, a document row of shape, owned by an issue of its own, by
// project SHP or by a conversation of its own, and answers its id and ref_key.
func insertDocumentShape(ctx context.Context, tx pgx.Tx, index int, shape documentShape) (string, string, error) {
	var issue, project, session *string
	switch shape.owner {
	case "issue":
		key := fmt.Sprintf("SHP-%d", 1000+index)
		if _, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ($1, 'SHP', $2, 'A shape', '{"kind":"user","id":"alice"}', 'U')
		`, key, 1000+index); err != nil {
			return "", "", err
		}
		issue = &key
		shp := "SHP"
		project = &shp
	case "project":
		shp := "SHP"
		project = &shp
	default:
		id := fmt.Sprintf("ses_shape_%d", index)
		session = &id
	}
	var role *string
	if shape.role != "" {
		role = &shape.role
	}
	var id, ref string
	err := tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, session_id, slug, name, kind, is_primary, role, created_by)
		values ($1, $2, $3, $4, 'A shape', $5, $6, $7, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`, issue, project, session, fmt.Sprintf("shape-%d", index), shape.kind, shape.primary, role).Scan(&id, &ref)
	return id, ref, err
}

// errShapeRefused is a shape the store refuses to hold.
type errShapeRefused struct{ error }

// errRollBack rolls a shape's transaction back once it is read.
var errRollBack = errors.New("roll the shape back")
