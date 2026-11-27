package api

import (
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// peopleOf is the people store over database that the server under test reads.
func peopleOf(database *store.Store) *store.PgPeopleStore {
	return store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
}
