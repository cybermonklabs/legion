package pgmigrate

import (
	"fmt"
	"io"
	"strings"
)

// EditCheck is CheckEdits' answer for one store's set: how many files the base holds were compared
// with the head's file of the same version, and how each compared.
type EditCheck struct {
	Store string
	// Compared is the number of the base's forward migration files.
	Compared int
	// Unchanged is the number whose head file has the base file's identity.
	Unchanged int
	// Declared is the number whose head file differs and accepts what a database the base accepts
	// records of it: the first identity, the base file's text, and each text the base's replaces
	// lines name, since the head's replaces lines begin with the base's, in their order.
	Declared int
	// Refusals name each base file whose head file a database the base accepts refuses, or that
	// does not keep one of the base's replaces lines at its place in the base's order, and each
	// the head no longer has a file of the version for.
	Refusals []error
}

// CheckEdits compares each forward migration of base, the set a base commit holds as Load gives
// it, with the head's file of the same version (head, as Load gives it). A database the base's
// runner accepts records, of each version, the identity a runner records of the base's file
// (Migration.RecordedIdentity) and, where a runner applied the file, the base file's text or one
// of the texts its replaces lines name: a release whose file was an earlier edit applied that one.
// The head's file must accept all of them, as Reconcile judges them, or the runner refuses that
// database at its next boot: its first replaces line must name the base's first identity, the
// base file itself where the base declares none; its replaces lines must begin with the base's,
// in the base's order, as every edit keeps the lines before it and adds its own after them; and
// its own text or a replaces line must name the base file's text. A pull request that edits an
// applied migration in place, a comment included, is caught here, before any database boots it.
// A base file whose version the head has no file for is refused as well: every database that ran
// it keeps a version no file of the head names.
func CheckEdits(store string, base, head []Migration) *EditCheck {
	byVersion := make(map[int]Migration, len(head))
	for _, migration := range head {
		byVersion[migration.Version] = migration
	}
	check := &EditCheck{Store: store, Compared: len(base)}
	for _, file := range base {
		first := file.RecordedIdentity()
		migration, ok := byVersion[file.Version]
		chain := brokenChain(file, migration)
		switch {
		case !ok:
			check.Refusals = append(check.Refusals, fmt.Errorf(
				"the base holds %s and this head has no migration of version %d: every database that ran it keeps a version no file of this head names; keep the file",
				file.Identity(), file.Version))
		case migration.Identity() == file.Identity():
			check.Unchanged++
		case migration.RecordedIdentity() != first:
			check.Refusals = append(check.Refusals, fmt.Errorf(
				"%s is %s, and a database the base migrated records version %d as %s, the first identity of the base's %s, which this head's file does not record: that database refuses this head at its next boot; leave the file as the base has it, or make this line its first replaces line, before any other: -- replaces: %s",
				migration.Name, migration.Identity(), file.Version, first, file.Name, first))
		case chain >= 0:
			// The first line is the first identity, which the case above holds. A later one names a
			// text a release before the base applied, which the base accepts. A head that still
			// accepts that text, by the line put elsewhere or by being that earlier file again,
			// refuses no database for it, but every edit keeps the lines before it in their order.
			line := file.Replaces[chain]
			if migration.acceptsText(line.Checksum) {
				check.Refusals = append(check.Refusals, fmt.Errorf(
					"%s is %s, and does not keep the base's replaces line -- replaces: %s at its place in the base's order: every edit keeps the replaces lines before it, in their order, and adds its own after them; leave the file as the base has it, or make its first replaces lines these, in this order: %s",
					migration.Name, migration.Identity(), line, keptLines(file)))
			} else {
				check.Refusals = append(check.Refusals, fmt.Errorf(
					"%s is %s, and drops the base's replaces line -- replaces: %s: a database that a release of that text created from empty applied it under version %d, the base accepts that database, and it refuses this head at its next boot; leave the file as the base has it, or make its first replaces lines these, in this order: %s",
					migration.Name, migration.Identity(), line, file.Version, keptLines(file)))
			}
		case !migration.acceptsText(file.Checksum):
			// The head's own text is the first identity when it declares nothing; any line changes
			// that text, so the first identity must then be declared first.
			add := "add this line after the base's replaces lines: -- replaces: " + file.Identity().String()
			if len(migration.Replaces) == 0 {
				add = "add these two lines, in this order: -- replaces: " + first.String() + " and -- replaces: " + file.Identity().String()
			}
			check.Refusals = append(check.Refusals, fmt.Errorf(
				"%s is %s, and a database the base migrated may have applied the base's text of version %d, %s, which this head's file does not accept: that database refuses this head at its next boot; leave the file as the base has it, or %s",
				migration.Name, migration.Identity(), file.Version, file.Identity(), add))
		default:
			check.Declared++
		}
	}
	return check
}

// brokenChain is the index of the first of base's replaces lines after its first that head's
// replaces lines do not hold at the same index, or -1 where head's lines begin with base's. The
// first line is the identity a runner records (Migration.RecordedIdentity), which CheckEdits
// compares on its own: a head that declares nothing and is the base's first file holds it too.
func brokenChain(base, head Migration) int {
	for i := 1; i < len(base.Replaces); i++ {
		if i >= len(head.Replaces) || head.Replaces[i] != base.Replaces[i] {
			return i
		}
	}
	return -1
}

// keptLines writes the replaces lines a head file that differs from base begins with when it
// accepts every identity and text the base accepts: base's own, in their order, then the one
// naming base's text.
func keptLines(base Migration) string {
	lines := make([]string, 0, len(base.Replaces))
	for _, replaced := range base.Replaces {
		lines = append(lines, "-- replaces: "+replaced.String())
	}
	return strings.Join(lines, ", ") + " and -- replaces: " + base.Identity().String()
}

// Write prints the check, one fact to a line. A check that compared no file says so: it saw
// nothing, which is no pass.
func (c *EditCheck) Write(w io.Writer) {
	if c.Compared == 0 {
		fmt.Fprintf(w, "edited-migrations: %s: NOTHING COMPARED: the base holds no forward migration of this store\n", c.Store)
		return
	}
	fmt.Fprintf(w, "edited-migrations: %s: compared the %d migrations the base holds: %d unchanged, %d edited accepting what the base records, %d refused\n",
		c.Store, c.Compared, c.Unchanged, c.Declared, len(c.Refusals))
	for _, refusal := range c.Refusals {
		fmt.Fprintf(w, "edited-migrations: %s: REFUSED %v\n", c.Store, refusal)
	}
}
