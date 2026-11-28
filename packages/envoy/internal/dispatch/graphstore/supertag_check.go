package graphstore

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// supertagKind is the node kind of a type, whose id is the type's name: the kind every node's
// supertag names a node of (migration 20261109051232). pmdoc/schemanodes.KindSupertag spells it
// for the packages that may import schemanodes, and this one may not.
const supertagKind = "supertag"

// ErrSupertagNotHeld refuses a write of a node whose supertag is no type the store holds: a type
// is a node of the store, a `supertag` row that is not gone, so a node of any other supertag is a
// node of a type nobody declared. It is a sentinel so a route can answer it as a refusal, 409
// SUPERTAG_NOT_HELD; what a write is refused with is a SupertagNotHeldError, which wraps it and
// names each type.
var ErrSupertagNotHeld = errors.New("the store holds no type of that name")

// SupertagNotHeldError is ErrSupertagNotHeld for the supertags of one write that the store holds
// no type of, sorted, each once.
type SupertagNotHeldError struct{ Supertags []string }

func (e *SupertagNotHeldError) Error() string {
	return fmt.Sprintf("the store holds no type %s, so it takes no node of it: a type is a supertag node of the store, "+
		"and a node of a type the store does not hold is refused", strings.Join(e.Supertags, ", "))
}

func (e *SupertagNotHeldError) Unwrap() error { return ErrSupertagNotHeld }

// queueSupertagCheck queues the read that refuses a write of any node whose supertag the store
// holds no type of (SupertagNotHeldError): one statement over the primary key of graph_nodes for
// the distinct supertags of the nodes given, queued before the node write, whose callback answers
// the refusal. Nothing holds the types in memory: a type a migration or an operator writes is taken
// by the next write of every process, and one deleted is refused by it, with no restart. The arms a
// caller splices into a statement of its own (WriteArms) run no such read: they write the person,
// session, role and page nodes alone, of supertags the store seeds, and the publisher's record
// that splices them into its one statement has no statement before it to refuse in.
func queueSupertagCheck(b *store.WriteBatch, nodes []Node) {
	// A write's nodes carry one or two supertags, a block index's every block `block`, so the
	// distinct ones are gathered by a scan of the few already held rather than a set.
	supertags := make([]string, 0, 2)
	for index := range nodes {
		if supertag := nodes[index].Supertag; !slices.Contains(supertags, supertag) {
			supertags = append(supertags, supertag)
		}
	}
	const what = "read the types of the nodes written"
	b.Queue(what, `
		select named.id from unnest($1::text[]) as named(id)
		where not exists (
			select 1 from graph_nodes held
			where held.kind = '`+supertagKind+`' and held.id = named.id and held.state <> '`+StateGone+`')
		order by named.id`, supertags).Query(func(rows pgx.Rows) error {
		missing, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if len(missing) > 0 {
			return &SupertagNotHeldError{Supertags: missing}
		}
		return nil
	})
}
