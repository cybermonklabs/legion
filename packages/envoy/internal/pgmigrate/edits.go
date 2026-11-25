package pgmigrate

import (
	"fmt"
	"io"
	"slices"
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
	// records of it: the head's replaces lines begin with the base's chain (chainLine), every
	// identity and text the base's file is or names.
	Declared int
	// Refusals name each base file whose head file a database the base accepts refuses, or that
	// does not keep a line of the base's chain at its place in it, and each the head no longer has
	// a file of the version for.
	Refusals []error
}

// CheckEdits compares each forward migration of base, the set a base commit holds as Load gives
// it, with the head's file of the same version (head, as Load gives it). A database the base's
// runner accepts records, of each version, an identity the base's file is or names in a replaces
// line and, where a runner applied the file, the base file's text or one its replaces lines name:
// a release whose file was an earlier edit applied that one. The head's file must accept all of
// them, as Reconcile judges them, or the runner refuses that database at its next boot. So a head
// file that differs from the base's begins its replaces lines with the base's chain, in its order,
// each line naming the same file and text: the base's replaces lines, then the base file itself,
// as every edit keeps the lines before it, adds the one naming the file it edits, then its own.
// The first line is the identity a runner records (Migration.RecordedIdentity), so a head whose
// first differs records what the base refuses. A pull request that edits an applied migration in
// place, a comment included, is caught here, before any database boots it. A base file whose
// version the head has no file for is refused as well: every database that ran it keeps a version
// no file of the head names.
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
			// The first line is the first identity, which the case above holds; each later one is an
			// identity the base accepts, whose text a release before the base applied. A head that
			// accepts that identity at another place refuses no database for it, but every edit keeps
			// the lines before it in their order.
			line, what := chainLine(file, chain), "the base's replaces line"
			if chain == len(file.Replaces) {
				what = "the base's own identity"
			}
			switch {
			case line == migration.Identity() || slices.Contains(migration.Replaces, line):
				check.Refusals = append(check.Refusals, fmt.Errorf(
					"%s is %s, and does not keep %s -- replaces: %s at its place in the base's order: every edit keeps the replaces lines before it, in their order, then the one naming the file it edits, and adds its own after them; leave the file as the base has it, or make its first replaces lines these, in this order: %s",
					migration.Name, migration.Identity(), what, line, keptLines(file)))
			case migration.acceptsText(line.Checksum):
				check.Refusals = append(check.Refusals, fmt.Errorf(
					"%s is %s, and names the text of %s -- replaces: %s only under another file name, so it does not accept that identity: a database the base accepts may record version %d as %s, and it refuses this head at its next boot; leave the file as the base has it, or make its first replaces lines these, in this order: %s",
					migration.Name, migration.Identity(), what, line, file.Version, line, keptLines(file)))
			case chain == len(file.Replaces):
				// The head's own text is the first identity when it declares nothing; any line
				// changes that text, so the first identity must then be declared first.
				add := "add this line after the base's replaces lines: -- replaces: " + line.String()
				if len(migration.Replaces) == 0 {
					add = "add these two lines, in this order: -- replaces: " + first.String() + " and -- replaces: " + line.String()
				}
				check.Refusals = append(check.Refusals, fmt.Errorf(
					"%s is %s, and a database the base migrated may have applied the base's text of version %d, %s, which this head's file does not accept: that database refuses this head at its next boot; leave the file as the base has it, or %s",
					migration.Name, migration.Identity(), file.Version, line, add))
			default:
				check.Refusals = append(check.Refusals, fmt.Errorf(
					"%s is %s, and drops the base's replaces line -- replaces: %s: a database that a release of that text created from empty applied it under version %d, the base accepts that database, and it refuses this head at its next boot; leave the file as the base has it, or make its first replaces lines these, in this order: %s",
					migration.Name, migration.Identity(), line, file.Version, keptLines(file)))
			}
		default:
			check.Declared++
		}
	}
	return check
}

// chainLine is line i of base's chain, the replaces lines a head file that differs from base
// begins with: base's own replaces lines, in their order, then base's identity.
func chainLine(base Migration, i int) Identity {
	if i < len(base.Replaces) {
		return base.Replaces[i]
	}
	return base.Identity()
}

// brokenChain is the index of the first line of base's chain after its first (chainLine) that
// head's replaces lines do not hold at the same index, or -1 where they begin with the whole
// chain. The first line is the identity a runner records (Migration.RecordedIdentity), which
// CheckEdits compares on its own: a head that declares nothing and is the base's first file holds
// it too. A base that declares nothing has a chain of one line, its own identity.
func brokenChain(base, head Migration) int {
	for i := 1; i <= len(base.Replaces); i++ {
		if i >= len(head.Replaces) || head.Replaces[i] != chainLine(base, i) {
			return i
		}
	}
	return -1
}

// keptLines writes base's chain (chainLine) as replaces lines: base's own, in their order, then
// the one naming base itself.
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
