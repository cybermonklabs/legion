package pgmigrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Identity is what a runner records, beside a version, of the file it applied under that
// version: the file's name as Load gives it, with no directory, and the sha256 of its whole text in
// lower-case hex. A version alone cannot tell the file a database ran from another file that took
// the same number on another branch, or from an edit of that file made after it ran; the version
// with its identity can.
type Identity struct {
	Name     string
	Checksum string
}

// String writes the identity as a replaces line names it: <name> sha256:<hex>.
func (i Identity) String() string { return i.Name + " sha256:" + i.Checksum }

// Identity is the identity a runner records of the migration.
func (m Migration) Identity() Identity { return Identity{Name: m.Name, Checksum: m.Checksum} }

// checksumOf is the sha256 of text in lower-case hex.
func checksumOf(text []byte) string {
	sum := sha256.Sum256(text)
	return hex.EncodeToString(sum[:])
}

// replacesMarker opens a replaces line. Load reads every -- comment whose text, after its two
// dashes and any space, begins with it in any case as a replaces line, so a misspelt one is
// refused and not read as prose.
const replacesMarker = "replaces:"

// replacesForm is the one form a replaces line takes: the comment alone on its line, from the
// line's first byte, naming a forward migration's file and its sha256 in lower-case hex.
var replacesForm = regexp.MustCompile(`^-- replaces: ([^\s/]+\.up\.sql) sha256:([0-9a-f]{64})$`)

// readReplaces reads the replaces lines of the forward migration name, of version version and
// text sql: each -- comment that declares the identity of an earlier file the migration replaces
// (Migration.Replaces). It reads the text as Postgres reads it (scanSQL), so a line of that form
// inside a string or a dollar-quoted body declares nothing. It refuses a line that is not of
// replacesForm, and one that names a file of another version: a runner judges the row of a
// version against the file of that version alone, so such a line could never match.
func readReplaces(name string, version int, sql string) ([]Identity, error) {
	if !containsFold(sql, replacesMarker) {
		return nil, nil
	}
	var replaces []Identity
	var problems []error
	_ = scanSQL(sql, func(token sqlToken) error {
		text := sql[token.start:token.end]
		if token.kind != sqlComment || !strings.HasPrefix(text, "--") ||
			!hasPrefixFold(strings.TrimLeft(text[2:], " \t\f\v"), replacesMarker) {
			return nil
		}
		line := 1 + strings.Count(sql[:token.start], "\n")
		match := replacesForm.FindStringSubmatch(text)
		if match == nil || token.start > 0 && sql[token.start-1] != '\n' {
			problems = append(problems, fmt.Errorf(
				"migration %s line %d: %q is not a replaces line: write it alone on its line, as -- replaces: <version>_<name>.up.sql sha256:<64 lower-case hex digits>, naming the file and checksum the database records",
				name, line, text))
			return nil
		}
		replaced, err := parseVersion(strings.TrimSuffix(match[1], ".up.sql"))
		if err != nil || replaced != version {
			problems = append(problems, fmt.Errorf(
				"migration %s line %d replaces %s, which is not a file of version %d: a runner judges what a database records under version %d against this file alone, so the line could never match; name the file the database records under that version",
				name, line, match[1], version, version))
			return nil
		}
		replaces = append(replaces, Identity{Name: match[1], Checksum: match[2]})
		return nil
	})
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return replaces, nil
}

// containsFold says whether s holds substr in any case, allocating nothing.
func containsFold(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if strings.EqualFold(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

// hasPrefixFold says whether s begins with prefix in any case.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// IdentityColumns are the columns of a version table that hold an applied migration's identity,
// as each runner creates its table with them: nullable text with no default, so a row an earlier
// runner wrote, and one an earlier release still writes while it rolls, records no name.
const IdentityColumns = "name text, checksum text"

// VersionTableColumns reads the data type of each column of table, in the schema the runner
// creates its tables in (current_schema()); none where the table is not there.
func VersionTableColumns(ctx context.Context, q Querier, table string) (map[string]string, error) {
	rows, err := q.Query(ctx, `select column_name::text, data_type::text from information_schema.columns
		where table_schema = current_schema() and table_name = $1`, table)
	if err != nil {
		return nil, fmt.Errorf("read the columns of %s: %w", table, err)
	}
	columns := map[string]string{}
	var name, dataType string
	if _, err := pgx.ForEachRow(rows, []any{&name, &dataType}, func() error {
		columns[name] = dataType
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the columns of %s: %w", table, err)
	}
	return columns, nil
}

// AddIdentityColumns is the alter table clauses that give a version table of columns the identity
// columns it lacks (IdentityColumns): none for a table a runner created with them. A runner reads
// the columns first and alters only when a clause is owed, because ALTER TABLE takes ACCESS
// EXCLUSIVE on the table even where IF NOT EXISTS would leave it as it is.
func AddIdentityColumns(columns map[string]string) []string {
	var clauses []string
	for _, column := range []string{"name", "checksum"} {
		if _, ok := columns[column]; !ok {
			clauses = append(clauses, "add column "+column+" text")
		}
	}
	return clauses
}

// Recorded is one row of a runner's version table. Identity is zero where the row records no
// name: every row a runner wrote before the runners recorded identities, and one written by hand.
type Recorded struct {
	Version  int
	Identity Identity
}

// Fix is a write a runner makes to one row of its version table before it applies anything. From
// is the identity the row records, zero for a row that records none, which the fix backfills; To
// is the identity of the set's file of the row's version.
type Fix struct {
	Version  int
	From, To Identity
}

// Backfill says whether the fix fills a row that recorded no name.
func (f Fix) Backfill() bool { return f.From.Name == "" }

// Reconcile judges each recorded row against the set's file of its version, and answers the writes
// the rows need, in the order of recorded, or, when it refuses any row, every refusal and no
// write. A row is:
//
//   - left as it is where no file of the set has its version, as the runners have always left
//     it: a database that a later release migrated and an earlier one runs over, as an image
//     rollback does, records versions the earlier set does not hold;
//   - backfilled with the file's identity where it records no name. Nothing says which file
//     wrote it, so it is never refused;
//   - left as it is where it records the file's own identity;
//   - moved to the file's own identity where it records, name and checksum both, an identity the
//     file declares it replaces (Migration.Replaces);
//   - refused otherwise, naming the version, both identities and the two remedies: a database
//     that ran another branch's or an earlier commit's migration is reset, and an intended edit
//     declares the recorded identity in a replaces line.
func Reconcile(migrations []Migration, recorded []Recorded) ([]Fix, error) {
	fixes, refusals := judge(migrations, recorded)
	if len(refusals) > 0 {
		return nil, errors.Join(refusals...)
	}
	return fixes, nil
}

// judge is Reconcile's reading, which answers the fixes and the refusals apart for CheckVersions.
func judge(migrations []Migration, recorded []Recorded) (fixes []Fix, refusals []error) {
	byVersion := make(map[int]*Migration, len(migrations))
	for i := range migrations {
		byVersion[migrations[i].Version] = &migrations[i]
	}
	for _, row := range recorded {
		migration, ok := byVersion[row.Version]
		if !ok {
			continue
		}
		own := migration.Identity()
		switch {
		case row.Identity.Name == "":
			fixes = append(fixes, Fix{Version: row.Version, To: own})
		case row.Identity == own:
		case slices.Contains(migration.Replaces, row.Identity):
			fixes = append(fixes, Fix{Version: row.Version, From: row.Identity, To: own})
		default:
			refusals = append(refusals, refusal(row, migration))
		}
	}
	return fixes, refusals
}

// refusal is Reconcile's refusal of row, which records an identity other than migration's own and
// other than any it replaces.
func refusal(row Recorded, migration *Migration) error {
	declared := ""
	if len(migration.Replaces) > 0 {
		names := make([]string, len(migration.Replaces))
		for i, replaced := range migration.Replaces {
			names[i] = replaced.String()
		}
		declared = ", whose replaces lines name only " + strings.Join(names, ", ")
	}
	return fmt.Errorf(
		"version %d is recorded as %s, and this build's file of version %d is %s%s: if this database ran another branch's or an earlier commit's migration, reset it (drop it, create it empty and let the server migrate it); if the edit is intended, add this line to %s: -- replaces: %s",
		row.Version, row.Identity, migration.Version, migration.Identity(), declared, migration.Name, row.Identity)
}

// versionTable is what the reads and writes of a version table run on: a runner's transaction,
// or the connection of a non-transactional migration.
type versionTable interface {
	Querier
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// ReadRecorded reads every row of the version table table on q, in version order, resolving the
// name through q's search_path as the runner's own statements do. A table without the name and
// checksum columns, as every runner before identities left it, reads as rows that record no name,
// and a table that does not exist reads as no row, so a read-only check can read any database;
// the runners add the columns before they read (each store's migrate).
func ReadRecorded(ctx context.Context, q Querier, table string) ([]Recorded, error) {
	var exists, hasName, hasChecksum bool
	if err := q.QueryRow(ctx, `select r is not null,
			exists(select 1 from pg_attribute where attrelid = r and attname = 'name' and attnum > 0 and not attisdropped),
			exists(select 1 from pg_attribute where attrelid = r and attname = 'checksum' and attnum > 0 and not attisdropped)
		from to_regclass($1::text) r`, table).Scan(&exists, &hasName, &hasChecksum); err != nil {
		return nil, fmt.Errorf("look for %s: %w", table, err)
	}
	if !exists {
		return nil, nil
	}
	name, checksum := "null::text", "null::text"
	if hasName {
		name = "name"
	}
	if hasChecksum {
		checksum = "checksum"
	}
	rows, err := q.Query(ctx, "select version, "+name+", "+checksum+" from "+pgx.Identifier{table}.Sanitize()+" order by version")
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", table, err)
	}
	recorded, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Recorded, error) {
		var r Recorded
		var name, checksum *string
		if err := row.Scan(&r.Version, &name, &checksum); err != nil {
			return r, err
		}
		if name != nil {
			r.Identity.Name = *name
		}
		if checksum != nil {
			r.Identity.Checksum = *checksum
		}
		return r, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", table, err)
	}
	return recorded, nil
}

// ReconcileRecorded is a runner's judgment of its version table table before it applies anything,
// on q, which holds the runner's advisory lock: it reads every row (ReadRecorded), judges them
// against migrations (Reconcile) and, where it refuses none, writes each fix. It answers the
// versions the table records, for a runner that applies its whole set in the same transaction,
// and the fixes it wrote, for LogFixes once that transaction commits. A refusal writes nothing.
func ReconcileRecorded(ctx context.Context, q versionTable, table string, migrations []Migration) (map[int]bool, []Fix, error) {
	recorded, err := ReadRecorded(ctx, q, table)
	if err != nil {
		return nil, nil, err
	}
	fixes, err := Reconcile(migrations, recorded)
	if err != nil {
		return nil, nil, err
	}
	if err := writeFixes(ctx, q, table, fixes); err != nil {
		return nil, nil, err
	}
	versions := make(map[int]bool, len(recorded))
	for _, row := range recorded {
		versions[row.Version] = true
	}
	return versions, fixes, nil
}

// RecheckRecorded is a runner's judgment, on q, of what its version table records of the one
// migration it is about to apply, for a runner that releases its advisory lock between its first
// judgment of the table (ReconcileRecorded) and each migration, as Dispatch's does: another process
// may have recorded the version in between. applied is false where the table records no row of
// the version. Where it does, the row is judged as Reconcile judges it: a refusal is the error,
// and a fix is written and answered, for LogFixes once q's transaction commits.
func RecheckRecorded(ctx context.Context, q versionTable, table string, migration Migration) (applied bool, fixes []Fix, err error) {
	var name, checksum *string
	err = q.QueryRow(ctx, "select name, checksum from "+pgx.Identifier{table}.Sanitize()+" where version = $1::bigint", migration.Version).Scan(&name, &checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("read what %s records of migration %s: %w", table, migration.Name, err)
	}
	row := Recorded{Version: migration.Version}
	if name != nil {
		row.Identity.Name = *name
	}
	if checksum != nil {
		row.Identity.Checksum = *checksum
	}
	fixes, err = Reconcile([]Migration{migration}, []Recorded{row})
	if err != nil {
		return true, nil, err
	}
	return true, fixes, writeFixes(ctx, q, table, fixes)
}

// writeFixes records each fix's identity in its row of table.
func writeFixes(ctx context.Context, q versionTable, table string, fixes []Fix) error {
	for _, fix := range fixes {
		if _, err := q.Exec(ctx, "update "+pgx.Identifier{table}.Sanitize()+" set name = $2, checksum = $3 where version = $1::bigint",
			fix.Version, fix.To.Name, fix.To.Checksum); err != nil {
			return fmt.Errorf("record the identity of version %d in %s: %w", fix.Version, table, err)
		}
	}
	return nil
}

// Record inserts migration's version and identity into table on q, once the runner has applied
// it. The version is cast because pgx encodes a parameter for the column it finds, and Dispatch's
// column can be an integer until the boot that widens it.
func Record(ctx context.Context, q versionTable, table string, migration Migration) error {
	if _, err := q.Exec(ctx, "insert into "+pgx.Identifier{table}.Sanitize()+" (version, name, checksum) values ($1::bigint, $2, $3)",
		migration.Version, migration.Name, migration.Checksum); err != nil {
		return fmt.Errorf("record migration %s: %w", migration.Name, err)
	}
	return nil
}

// LogFixes logs one line for each fix a runner wrote to table, once the transaction that wrote
// them has committed: a backfill names the version and the identity it recorded, and a move names
// the identity it replaced and the one it recorded.
func LogFixes(table string, fixes []Fix) {
	for _, fix := range fixes {
		if fix.Backfill() {
			slog.Info("pgmigrate: recorded the identity of a migration applied before the runners recorded identities",
				"table", table, "version", fix.Version, "name", fix.To.Name, "checksum", fix.To.Checksum)
			continue
		}
		slog.Info("pgmigrate: moved a migration's recorded identity to the file that replaces it",
			"table", table, "version", fix.Version, "from", fix.From.String(), "to", fix.To.String())
	}
}

// VersionCheck is what CheckVersions reads of a database's version table against a set, writing
// nothing: what the runner would do with each row at its next boot.
type VersionCheck struct {
	Table string
	// Recorded is the number of rows the table holds.
	Recorded int
	// Matching is the number of rows that record their file's own identity.
	Matching int
	// Backfills are the rows that record no name, which the next boot fills from the set's file.
	Backfills []Fix
	// Moves are the rows that record an identity a file replaces, which the next boot moves.
	Moves []Fix
	// NoFile is each recorded version no file of the set has, which the runner leaves as it is.
	NoFile []int
	// Pending is each file of the set whose version the table does not record.
	Pending []string
	// Refusals are the rows the next boot refuses (Reconcile), and so the boot.
	Refusals []error
}

// OK says whether the table holds only versions the set has files for and the runner's next boot
// refuses none of them.
func (c *VersionCheck) OK() bool { return len(c.Refusals) == 0 && len(c.NoFile) == 0 }

// Write prints the check, one fact to a line, ending with its verdict.
func (c *VersionCheck) Write(w io.Writer) {
	fmt.Fprintf(w, "versions: %s records %d versions: %d record their file's own identity, %d record no name and are filled from this build's file at the next boot, %d record an identity a file replaces and move to it, %d have no file, %d refuse\n",
		c.Table, c.Recorded, c.Matching, len(c.Backfills), len(c.Moves), len(c.NoFile), len(c.Refusals))
	for _, fix := range c.Moves {
		fmt.Fprintf(w, "versions: version %d moves from %s to %s\n", fix.Version, fix.From, fix.To)
	}
	for _, version := range c.NoFile {
		fmt.Fprintf(w, "versions: NO FILE version %d: this build has no migration of that version; the database ran one this build does not hold\n", version)
	}
	for _, refusal := range c.Refusals {
		fmt.Fprintf(w, "versions: REFUSED %v\n", refusal)
	}
	if len(c.Pending) > 0 {
		fmt.Fprintf(w, "versions: pending, applied at the next boot: %s\n", strings.Join(c.Pending, ", "))
	}
	if c.OK() {
		fmt.Fprintln(w, "versions: ok")
		return
	}
	fmt.Fprintln(w, "versions: NOT OK")
}

// CheckVersions reads the version table table on conn's database, in a read-only transaction of
// its own, and says what the runner would do with each row at its next boot against migrations
// (VersionCheck). It writes nothing, and needs no column the runners add, so it reads a database
// that a runner from before identities migrated as well as one this runner did.
func CheckVersions(ctx context.Context, conn *pgx.Conn, migrations []Migration, table string) (*VersionCheck, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin a read-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	recorded, err := ReadRecorded(ctx, tx, table)
	if err != nil {
		return nil, err
	}
	check := &VersionCheck{Table: table, Recorded: len(recorded)}
	fixes, refusals := judge(migrations, recorded)
	check.Refusals = refusals
	for _, fix := range fixes {
		if fix.Backfill() {
			check.Backfills = append(check.Backfills, fix)
		} else {
			check.Moves = append(check.Moves, fix)
		}
	}
	files := make(map[int]Identity, len(migrations))
	for _, migration := range migrations {
		files[migration.Version] = migration.Identity()
	}
	versions := make(map[int]bool, len(recorded))
	for _, row := range recorded {
		versions[row.Version] = true
		own, ok := files[row.Version]
		switch {
		case !ok:
			check.NoFile = append(check.NoFile, row.Version)
		case row.Identity == own:
			check.Matching++
		}
	}
	for _, migration := range migrations {
		if !versions[migration.Version] {
			check.Pending = append(check.Pending, migration.Name)
		}
	}
	return check, nil
}
