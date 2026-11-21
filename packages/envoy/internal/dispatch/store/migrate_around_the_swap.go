package store

import (
	"context"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// EmbeddingsPartitionedThrough is the last migration of the release that partitions embeddings by
// project: 20261121120653, which records each dashboard publish under its own commit for the
// swap's fleet read, after 20261120105330 builds the partitioned table and 20261120105818 mirrors
// every write of embeddings into it. A deployment that ran that release swaps the table in at its
// boot's fill (FillEmbeddingProjectKeys), whose fleet read needs every one of them, so every later
// release's migration runs on embeddings partitioned.
const EmbeddingsPartitionedThrough int64 = 20261121120653

// MigrateAroundTheSwap migrates the database in the order a deployment that ran this release
// before a later one does: every migration through EmbeddingsPartitionedThrough, the fill that
// swaps the partitioned embeddings in (as a boot's runs it, for an unstamped build), and then
// every later migration, on the partitioned table. A fresh database migrated by Migrate alone
// applies a later migration to the table the swap then drops, so a migration that works on that
// order alone - a CREATE INDEX CONCURRENTLY on embeddings, which Postgres refuses on a partitioned
// table (SQLSTATE 0A000), or an index the swap then drops with the old table - passes there and
// fails or is lost here. storetest's template is made this way, so CI runs every later migration
// on the shape deployments hold.
func (s *Store) MigrateAroundTheSwap(ctx context.Context) error {
	return s.migrateAroundTheSwap(ctx, migrationFiles)
}

// migrateAroundTheSwap is MigrateAroundTheSwap over the migrations fsys holds.
func (s *Store) migrateAroundTheSwap(ctx context.Context, fsys fs.FS) error {
	if err := s.migrate(ctx, versionsThrough{fsys: fsys, through: EmbeddingsPartitionedThrough}); err != nil {
		return err
	}
	if _, err := s.FillEmbeddingProjectKeys(ctx, ""); err != nil {
		return fmt.Errorf("partition embeddings as a boot's fill does: %w", err)
	}
	return s.migrate(ctx, fsys)
}

// versionsThrough is fsys with only the migrations of version at most through in its migrations
// directory: a file whose name starts with no version is kept, for pgmigrate.Load to refuse as it
// refuses it in the whole set.
type versionsThrough struct {
	fsys    fs.FS
	through int64
}

func (m versionsThrough) Open(name string) (fs.File, error) { return m.fsys.Open(name) }

func (m versionsThrough) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(m.fsys, name)
	if err != nil || name != "migrations" {
		return entries, err
	}
	var kept []fs.DirEntry
	for _, entry := range entries {
		prefix, _, _ := strings.Cut(entry.Name(), "_")
		if version, err := strconv.ParseInt(prefix, 10, 64); err != nil || version <= m.through {
			kept = append(kept, entry)
		}
	}
	return kept, nil
}
