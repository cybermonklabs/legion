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

// Identity is the migration's own identity: its file's name and the sha256 of its text.
func (m Migration) Identity() Identity { return Identity{Name: m.Name, Checksum: m.Checksum} }

// RecordedIdentity is the identity a runner records of the migration, when it applies it and when
// it fills a row that records no name: the first identity the file declares it replaces, or its own
// where it declares none. Each edit of a shipped file keeps the replaces lines before it, in their
// order, and adds its own after them (CheckEdits holds a change to that), so the first line names
// the version's first file, and every release whose file of the version is in the chain accepts
// what any of them records: a database a later release created or filled still boots the release
// before it, as a rollback and the two-builds tests need.
func (m Migration) RecordedIdentity() Identity {
	if len(m.Replaces) > 0 {
		return m.Replaces[0]
	}
	return m.Identity()
}

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

// isReplacesComment says whether text, from a -- on, reads as a replaces line, well formed or not.
func isReplacesComment(text string) bool {
	return strings.HasPrefix(text, "--") && hasPrefixFold(strings.TrimLeft(text[2:], " \t\f\v"), replacesMarker)
}

// readReplaces reads the replaces lines of the forward migration name, of version version and
// text sql: each -- comment that declares the identity of an earlier file the migration replaces
// (Migration.Replaces). It reads the text as Postgres reads it (scanSQL). It refuses a line that
// is not of replacesForm, and one that names a file of another version: a runner judges the row
// of a version against the file of that version alone, so such a line could never match. It
// also refuses a line that reads as a replaces line but where Postgres reads no comment of its
// own: inside a string literal or a dollar-quoted body, or between the parts of a continued
// string ('a' <newline> -- … <newline> 'b'), whose comments the string's token holds. Read as
// declaring nothing, such a line would neither save the database its author meant it to nor be
// refused when it is malformed.
func readReplaces(name string, version int, sql string) ([]Identity, error) {
	if !containsFold(sql, replacesMarker) {
		return nil, nil
	}
	var replaces []Identity
	var problems []error
	replacesCommentStarts := map[int]bool{}
	_ = scanSQL(sql, func(token sqlToken) error {
		text := sql[token.start:token.end]
		if token.kind != sqlComment || !isReplacesComment(text) {
			return nil
		}
		replacesCommentStarts[token.start] = true
		// The line is counted only for a refusal, which is the only place it is printed.
		line := func() int { return 1 + strings.Count(sql[:token.start], "\n") }
		match := replacesForm.FindStringSubmatch(text)
		if match == nil || token.start > 0 && sql[token.start-1] != '\n' {
			problems = append(problems, fmt.Errorf(
				"migration %s line %d: %q is not a replaces line: write it alone on its line, as -- replaces: <version>_<name>.up.sql sha256:<64 lower-case hex digits>, naming the file and checksum the database records",
				name, line(), text))
			return nil
		}
		replaced, err := parseVersion(strings.TrimSuffix(match[1], ".up.sql"))
		if err != nil || replaced != version {
			problems = append(problems, fmt.Errorf(
				"migration %s line %d replaces %s, which is not a file of version %d: a runner judges what a database records under version %d against this file alone, so the line could never match; name the file the database records under that version",
				name, line(), match[1], version, version))
			return nil
		}
		replaces = append(replaces, Identity{Name: match[1], Checksum: match[2]})
		return nil
	})
	for start, line := 0, 1; start < len(sql); line++ {
		end := len(sql)
		if newline := strings.IndexByte(sql[start:], '\n'); newline >= 0 {
			end = start + newline
		}
		text := strings.TrimLeft(sql[start:end], " \t\f\v")
		if at := end - len(text); isReplacesComment(text) && !replacesCommentStarts[at] {
			problems = append(problems, fmt.Errorf(
				"migration %s line %d reads as a replaces line, but Postgres reads it as part of a string literal, a dollar-quoted body or a continued string, where it declares nothing: put it on a line of its own outside every literal",
				name, line))
		}
		start = end + 1
	}
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

// Recorded is one row of a runner's version table. Unnamed is true where the row's name is null:
// every row a runner wrote before the runners recorded identities, and one written by hand. A
// name that is not null, the empty string included, is an identity like any other.
type Recorded struct {
	Version  int
	Identity Identity
	Unnamed  bool
}

// recordedRow is the row of version whose name and checksum columns read name and checksum, nil
// for a null.
func recordedRow(version int, name, checksum *string) Recorded {
	row := Recorded{Version: version, Unnamed: name == nil}
	if name != nil {
		row.Identity.Name = *name
	}
	if checksum != nil {
		row.Identity.Checksum = *checksum
	}
	return row
}

// Fix is a write a runner makes to one row of its version table before it applies anything: the
// row of Version recorded no name, and the runner records To, the identity it records of the set's
// file of that version (Migration.RecordedIdentity).
type Fix struct {
	Version int
	To      Identity
}

// Reconcile judges each recorded row against the set's file of its version, and answers the writes
// the rows need, in the order of recorded, or, when it refuses any row, every refusal and no
// write. A row is:
//
//   - left as it is where no file of the set has its version, as the runners have always left
//     it: a database that a later release migrated and an earlier one runs over, as an image
//     rollback does, records versions the earlier set does not hold;
//   - backfilled where its name is null, with the identity a runner records of the file
//     (Migration.RecordedIdentity). Nothing says which file wrote it, so it is never refused;
//   - left as it is where it records the file's own identity;
//   - left as it is where it records, name and checksum both, an identity the file declares it
//     replaces (Migration.Replaces). The row keeps the identity it records, so the release that
//     recorded it still finds its own file's identity there and boots, as a rolling update and a
//     rollback need; each later edit of the file adds a replaces line, so a chain of them covers
//     every identity a database may hold;
//   - refused otherwise, by a *Refusal.
//
// The refusals come back joined, each a *Refusal, which IsRefusal finds.
func Reconcile(migrations []Migration, recorded []Recorded) ([]Fix, error) {
	fixes, _, refusals := judge(migrations, recorded)
	if len(refusals) > 0 {
		return nil, errors.Join(refusals...)
	}
	return fixes, nil
}

// judge is Reconcile's reading, which answers apart, for CheckVersions, the fixes, the rows a
// replaces line accepts and the refusals.
func judge(migrations []Migration, recorded []Recorded) (fixes []Fix, replaced []Recorded, refusals []error) {
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
		case row.Unnamed:
			fixes = append(fixes, Fix{Version: row.Version, To: migration.RecordedIdentity()})
		case row.Identity == own:
		case slices.Contains(migration.Replaces, row.Identity):
			replaced = append(replaced, row)
		default:
			refusals = append(refusals, &Refusal{Recorded: row, File: migration.Identity(), Replaces: migration.Replaces})
		}
	}
	return fixes, replaced, refusals
}

// Refusal is Reconcile's refusal of one recorded row: the row records an identity other than its
// file's own and other than any the file replaces. Its message names the version, both identities
// and the remedies: a database that ran another branch's or an earlier commit's migration is
// reset, and an intended edit declares the recorded identity in a replaces line, offered only where
// the recorded identity is one a replaces line can name (replacesForm), since Load refuses any
// other line.
type Refusal struct {
	Recorded Recorded
	File     Identity
	Replaces []Identity
}

func (r *Refusal) Error() string {
	declared := ""
	if len(r.Replaces) > 0 {
		names := make([]string, len(r.Replaces))
		for i, replaced := range r.Replaces {
			names[i] = replaced.String()
		}
		declared = ", whose replaces lines name only " + strings.Join(names, ", ")
	}
	reset := "if this database ran another branch's or an earlier commit's migration, reset it (drop it, create it empty and let the server migrate it)"
	line := "-- replaces: " + r.Recorded.Identity.String()
	if replacedVersion, err := parseVersion(strings.TrimSuffix(r.Recorded.Identity.Name, ".up.sql")); !replacesForm.MatchString(line) || err != nil || replacedVersion != r.Recorded.Version {
		return fmt.Sprintf("version %d is recorded as %q sha256 %q, and this build's file of version %d is %s%s: %s; no replaces line can name what this row records, so a reset is the one remedy",
			r.Recorded.Version, r.Recorded.Identity.Name, r.Recorded.Identity.Checksum, r.Recorded.Version, r.File, declared, reset)
	}
	return fmt.Sprintf("version %d is recorded as %s, and this build's file of version %d is %s%s: %s; if the edit is intended, add this line to %s: %s",
		r.Recorded.Version, r.Recorded.Identity, r.Recorded.Version, r.File, declared, reset, r.File.Name, line)
}

// IsRefusal says whether err holds a *Refusal: a recorded row the set refuses, rather than a
// failure to read or write the version table, such as a lock timeout, which needs no reset.
func IsRefusal(err error) bool {
	var refusal *Refusal
	return errors.As(err, &refusal)
}

// Labelled is err, from a runner's judgment of its version table table, as the runner reports it.
// A refusal (IsRefusal) reads "migrations refused, none applied", the label the runbooks answer
// with a reset or a replaces line. Any other failure, a lock timeout or a failed read or write of
// the table, is labelled as such, since dropping a database is no answer to it.
func Labelled(table string, err error) error {
	if IsRefusal(err) {
		return fmt.Errorf("migrations refused, none applied: %w", err)
	}
	return fmt.Errorf("read or record what %s records, none applied: %w", table, err)
}

// versionTable is what the reads and writes of a version table run on: a runner's transaction,
// or the connection of a non-transactional migration.
type versionTable interface {
	Querier
	execer
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
		var version int
		var name, checksum *string
		if err := row.Scan(&version, &name, &checksum); err != nil {
			return Recorded{}, err
		}
		return recordedRow(version, name, checksum), nil
	})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", table, err)
	}
	return recorded, nil
}

// ReconcileRecorded is a runner's judgment of its version table table before it applies anything,
// on q, which holds the runner's advisory lock: it reads every row (ReadRecorded), judges them
// against migrations (Reconcile) and, where it refuses none, writes each fix. It answers the rows
// the table recorded before the fixes, for a runner that applies its whole set in the same
// transaction, and the fixes it wrote, for LogFixes once that transaction commits. A refusal
// writes nothing.
func ReconcileRecorded(ctx context.Context, q versionTable, table string, migrations []Migration) ([]Recorded, []Fix, error) {
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
	return recorded, fixes, nil
}

// RecheckRecorded is a runner's judgment, on q, of its version table before it applies migration,
// for a runner that releases its advisory lock between its first judgment of the table
// (ReconcileRecorded) and each migration, as Dispatch's does: another process may have recorded
// any version in between, this one or another. Where the table records migration's version, that
// row is judged as Reconcile judges it, applied is true, and nothing else is read: the runner
// applies nothing. Where it does not, the runner is about to apply migration, so every row is
// judged again against migrations, the runner's whole set (ReconcileRecorded), and a refusal of
// any of them is the error: no migration is applied while the table records a version under an
// identity the set refuses. Either way a fix is written and answered, for LogFixes once q's
// transaction commits.
func RecheckRecorded(ctx context.Context, q versionTable, table string, migrations []Migration, migration Migration) (applied bool, fixes []Fix, err error) {
	var name, checksum *string
	err = q.QueryRow(ctx, "select name, checksum from "+pgx.Identifier{table}.Sanitize()+" where version = $1::bigint", migration.Version).Scan(&name, &checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		_, fixes, err = ReconcileRecorded(ctx, q, table, migrations)
		return false, fixes, err
	}
	if err != nil {
		return false, nil, fmt.Errorf("read what %s records of migration %s: %w", table, migration.Name, err)
	}
	fixes, err = Reconcile([]Migration{migration}, []Recorded{recordedRow(migration.Version, name, checksum)})
	if err != nil {
		return true, nil, err
	}
	return true, fixes, writeFixes(ctx, q, table, fixes)
}

// writeFixes records each fix's identity in its row of table.
func writeFixes(ctx context.Context, q versionTable, table string, fixes []Fix) error {
	if len(fixes) == 0 {
		return nil
	}
	update := "update " + pgx.Identifier{table}.Sanitize() + " set name = $2, checksum = $3 where version = $1::bigint"
	for _, fix := range fixes {
		if _, err := q.Exec(ctx, update, fix.Version, fix.To.Name, fix.To.Checksum); err != nil {
			return fmt.Errorf("record the identity of version %d in %s: %w", fix.Version, table, err)
		}
	}
	return nil
}

// Record inserts migration's version and the identity a runner records of it (RecordedIdentity)
// into table on q, once the runner has applied it. The version is cast because pgx encodes a
// parameter for the column it finds, and Dispatch's column can be an integer until the boot that
// widens it.
func Record(ctx context.Context, q versionTable, table string, migration Migration) error {
	recorded := migration.RecordedIdentity()
	if _, err := q.Exec(ctx, "insert into "+pgx.Identifier{table}.Sanitize()+" (version, name, checksum) values ($1::bigint, $2, $3)",
		migration.Version, recorded.Name, recorded.Checksum); err != nil {
		return fmt.Errorf("record migration %s: %w", migration.Name, err)
	}
	return nil
}

// LogFixes logs one line for each fix a runner wrote to table, once the transaction that wrote
// them has committed, naming the version and the identity it recorded.
func LogFixes(table string, fixes []Fix) {
	for _, fix := range fixes {
		slog.Info("pgmigrate: recorded the identity of a migration applied before the runners recorded identities",
			"table", table, "version", fix.Version, "name", fix.To.Name, "checksum", fix.To.Checksum)
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
	// Replaced are the rows that record an identity their file declares it replaces, which the
	// next boot accepts and leaves as they are.
	Replaced []Recorded
	// NoFile is each recorded version no file of the set has, which the runner leaves as it is.
	NoFile []int
	// Pending is each file of the set whose version the table does not record.
	Pending []string
	// Refusals are the rows the next boot refuses, each a *Refusal, and so the boot.
	Refusals []error
}

// OK says whether the table records at least one version, holds only versions the set has files
// for, and the runner's next boot refuses none of them. A table that records nothing, or does not
// exist, is not OK: the check compared nothing, and a clean report of nothing would read the same
// as a clean report of a migrated database.
func (c *VersionCheck) OK() bool {
	return c.Recorded > 0 && len(c.Refusals) == 0 && len(c.NoFile) == 0
}

// Write prints the check, one fact to a line, ending with its verdict.
func (c *VersionCheck) Write(w io.Writer) {
	if c.Recorded == 0 {
		fmt.Fprintf(w, "versions: NOTHING CHECKED: %s records no version or does not exist on this connection's search_path, so no row was compared with a file; the next boot would apply every migration\n", c.Table)
		fmt.Fprintln(w, "versions: NOT OK")
		return
	}
	fmt.Fprintf(w, "versions: %s records %d versions: %d record their file's own identity, %d record no name and are filled from this build's file at the next boot, %d record an identity their file replaces and are accepted as they are, %d have no file, %d refuse\n",
		c.Table, c.Recorded, c.Matching, len(c.Backfills), len(c.Replaced), len(c.NoFile), len(c.Refusals))
	for _, row := range c.Replaced {
		fmt.Fprintf(w, "versions: version %d records %s, which its file replaces\n", row.Version, row.Identity)
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
// that a runner from before identities migrated as well as one this runner did. A connection
// whose search_path names no schema that exists cannot be checked, since neither the table nor
// anything else resolves there, and the runner could create nothing on it: that is an error, not
// an empty check.
func CheckVersions(ctx context.Context, conn *pgx.Conn, migrations []Migration, table string) (*VersionCheck, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin a read-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var schema *string
	if err := tx.QueryRow(ctx, "select current_schema()").Scan(&schema); err != nil {
		return nil, fmt.Errorf("read the connection's schema: %w", err)
	}
	if schema == nil {
		return nil, fmt.Errorf("the connection's search_path names no schema that exists, so current_schema() is null: neither %s nor any table resolves, and the runner can create neither; name an existing schema in the search_path", table)
	}
	recorded, err := ReadRecorded(ctx, tx, table)
	if err != nil {
		return nil, err
	}
	check := &VersionCheck{Table: table, Recorded: len(recorded)}
	check.Backfills, check.Replaced, check.Refusals = judge(migrations, recorded)
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
