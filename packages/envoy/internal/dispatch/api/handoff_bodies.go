package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// handoffBodies is what GET /api/v1/issues/{key}/handoffs/bodies answers: the bodies of the
// handoffs it answered, and the ids it left for a later request.
type handoffBodies struct {
	// Bodies holds one entry for each id answered, in the order asked.
	Bodies []handoffBody `json:"bodies"`
	// Left is each id asked that Bodies holds no entry of, in the order asked: the id whose entry
	// would have taken the answer past contracts.HandoffBodiesMaxBytes, and every id after it.
	Left []string `json:"left"`
}

// handoffBody is one id the answer answered: the handoff's body, the exact text written, or null
// for an id that is no handoff of the issue the path names. For a caller that asked for the
// trees, FieldTrees is the tree of each text the body's card draws, as handoffRead carries them,
// and nil where they would take the answer past its limit (getHandoffBodies).
type handoffBody struct {
	ID         string                       `json:"id"`
	Body       *string                      `json:"body"`
	FieldTrees map[string]model.DisplayTree `json:"field_trees,omitempty"`
}

// getHandoffBodies is GET /api/v1/issues/{key}/handoffs/bodies?ids=<id>,<id>: the bodies of up to
// contracts.HandoffBodiesMaxIDs handoffs of one issue in one answer, so the issue page reads the
// bodies of the entries in view in one request rather than one GET /api/v1/handoffs/{id} each.
//
// The answer adds each entry's size as it is sent to a count: its body as a JSON string as
// WriteJSON's encoder writes it, HTML escaping included, and its trees where it carries them. It
// stops before the first entry that would take the count past contracts.HandoffBodiesMaxBytes,
// and names that id and every id after it in Left. The first id asked is always answered, so the
// page always moves on; a first entry whose body and trees together pass the limit is sent with
// its body and no trees, and the page reads that one text itself.
//
// The ids are held to their rules before anything is read (parseHandoffBodyIDs), and a key that
// names no issue is 404 NOT_FOUND, as GET /api/v1/issues/{key} answers it. The route writes
// nothing: the trees are read where they stand (shapeBuilt), since they are values inside a
// stored text, which no writer stores a tree of.
func (s *server) getHandoffBodies(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	ids, err := parseHandoffBodyIDs(r.URL.Query()["ids"])
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	stored, err := s.readHandoffBodies(r.Context(), r.PathValue("key"), ids)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer, err := answerHandoffBodies(ids, stored, wantsDisplayTrees(r))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// parseHandoffBodyIDs reads the route's one `ids`, each id the canonical spelling of a uuid. Its
// refusals come in this order, each 400: `ids` absent, empty or given more than once, an empty
// id, or an id asked twice, in any spelling of it (INVALID_QUERY); more than
// contracts.HandoffBodiesMaxIDs ids (INVALID_QUERY); an id that is not a uuid (HANDOFF_ID_INPUT).
func parseHandoffBodyIDs(values []string) ([]string, error) {
	switch {
	case len(values) > 1:
		return nil, errorf(http.StatusBadRequest, "INVALID_QUERY",
			"ids is given once, its handoff ids separated by commas")
	case len(values) == 0 || values[0] == "":
		return nil, errorf(http.StatusBadRequest, "INVALID_QUERY",
			"ids is required: the handoff ids to read, separated by commas, 1 to %d of them", contracts.HandoffBodiesMaxIDs)
	}
	parts := strings.Split(values[0], ",")
	ids := make([]string, len(parts))
	asked := make(map[string]bool, len(parts))
	notUUID := ""
	for index, part := range parts {
		id := strings.TrimSpace(part)
		if id == "" {
			return nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "ids holds an empty id")
		}
		if parsed, err := uuid.Parse(id); err == nil {
			id = parsed.String()
		} else if notUUID == "" {
			notUUID = id
		}
		if asked[id] {
			return nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "ids names %s twice", id)
		}
		asked[id] = true
		ids[index] = id
	}
	if len(ids) > contracts.HandoffBodiesMaxIDs {
		return nil, errorf(http.StatusBadRequest, "INVALID_QUERY",
			"ids names at most %d handoff ids (%d named)", contracts.HandoffBodiesMaxIDs, len(ids))
	}
	if notUUID != "" {
		return nil, errorf(http.StatusBadRequest, "HANDOFF_ID_INPUT", "ids holds %q: %s", notUUID, idPathProblem("handoff"))
	}
	return ids, nil
}

// readHandoffBodies is the body of each of ids that is a handoff of the issue key names, by id, in
// one read-only transaction that first reads the issue, so the two answer one moment. A key that
// names no issue is pgx.ErrNoRows, which writeHandlerError answers 404 NOT_FOUND in the words GET
// /api/v1/issues/{key} refuses it with.
func (s *server) readHandoffBodies(ctx context.Context, key string, ids []string) (map[string]string, error) {
	tx, err := s.deps.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var issue string
	if err := tx.QueryRow(ctx, `select key from issues where key = $1`, key).Scan(&issue); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `select id::text, body from handoffs where issue_key = $1 and id = any($2::uuid[])`, key, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bodies := make(map[string]string, len(ids))
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			return nil, err
		}
		bodies[id] = body
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return bodies, tx.Commit(ctx)
}

// answerHandoffBodies is the answer for ids, each answered from stored, the bodies of those that
// are handoffs of the issue. It counts the bodies first, so the trees are read only for the
// entries whose bodies alone fit; then, for a caller that asked for the trees, it counts each of
// those entries' trees beside its body and stops again where the two pass the limit.
func answerHandoffBodies(ids []string, stored map[string]string, trees bool) (handoffBodies, error) {
	entries := make([]handoffBody, 0, len(ids))
	sizes := make([]int, 0, len(ids))
	count := 0
	for _, id := range ids {
		entry := handoffBody{ID: id}
		if body, held := stored[id]; held {
			entry.Body = &body
		}
		size, err := encodedLength(entry.Body)
		if err != nil {
			return handoffBodies{}, err
		}
		if len(entries) > 0 && count+size > contracts.HandoffBodiesMaxBytes {
			break
		}
		count += size
		entries = append(entries, entry)
		sizes = append(sizes, size)
	}
	if trees {
		reads := make([]handoffRead, len(entries))
		shapeBuilt(true, func(reader *displayTreeReader) {
			for index, entry := range entries {
				if entry.Body != nil {
					reads[index].Body = *entry.Body
					reads[index].shape(reader)
				}
			}
		})
		count = 0
		for index := range entries {
			treesSize := 0
			if reads[index].FieldTrees != nil {
				var err error
				if treesSize, err = encodedLength(reads[index].FieldTrees); err != nil {
					return handoffBodies{}, err
				}
			}
			switch {
			case count+sizes[index]+treesSize <= contracts.HandoffBodiesMaxBytes:
				entries[index].FieldTrees = reads[index].FieldTrees
				count += treesSize
			case index > 0:
				return handoffBodies{Bodies: entries[:index], Left: ids[index:]}, nil
			}
			// Every entry kept counts its body. The first is kept whatever its size, without its
			// trees where they pass the limit.
			count += sizes[index]
		}
	}
	return handoffBodies{Bodies: entries, Left: ids[len(entries):]}, nil
}

// encodedLength is how many bytes WriteJSON's encoder writes for value, HTML escaping included and
// the newline it ends with left out, counted rather than kept.
func encodedLength(value any) (int, error) {
	var counted byteCount
	if err := json.NewEncoder(&counted).Encode(value); err != nil {
		return 0, fmt.Errorf("measure an answer's entry: %w", err)
	}
	return int(counted) - 1, nil
}

// byteCount is a writer that keeps nothing and counts the bytes it is given.
type byteCount int

func (c *byteCount) Write(written []byte) (int, error) {
	*c += byteCount(len(written))
	return len(written), nil
}
