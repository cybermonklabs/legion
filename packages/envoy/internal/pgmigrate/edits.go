package pgmigrate

import (
	"fmt"
	"io"
)

// EditCheck is CheckEdits' answer for one store's set: how many files the base holds were compared
// with the head's file of the same version, and how each compared.
type EditCheck struct {
	Store string
	// Compared is the number of the base's forward migration files.
	Compared int
	// Unchanged is the number whose head file has the base file's identity.
	Unchanged int
	// Declared is the number whose head file differs and accepts what a database the base migrated
	// records of it: the identity, and the text the base's file holds.
	Declared int
	// Refusals name each base file whose head file a database the base migrated refuses, and each
	// the head no longer has a file of the version for.
	Refusals []error
}

// CheckEdits compares each forward migration of base, the set a base commit holds as Load gives
// it, with the head's file of the same version (head, as Load gives it). A database the base's
// runner migrated records, of each version, the identity a runner records of the base's file
// (Migration.RecordedIdentity) and, where it applied the file, the base file's text. The head's
// file must accept both, as Reconcile judges them, or the runner refuses that database at its next
// boot: its first replaces line must name the base's first identity, the base file itself where
// the base declares none, and its own text or a replaces line must name the base file's text. A
// pull request that edits an applied migration in place, a comment included, is caught here,
// before any database boots it. A base file whose version the head has no file for is refused as
// well: every database that ran it keeps a version no file of the head names.
func CheckEdits(store string, base, head []Migration) *EditCheck {
	byVersion := make(map[int]Migration, len(head))
	for _, migration := range head {
		byVersion[migration.Version] = migration
	}
	check := &EditCheck{Store: store, Compared: len(base)}
	for _, file := range base {
		first := file.RecordedIdentity()
		migration, ok := byVersion[file.Version]
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
		case !migration.acceptsText(file.Checksum):
			// The head's own text is the first identity when it declares nothing; any line changes
			// that text, so the first identity must then be declared first.
			add := "add this line after its first replaces line: -- replaces: " + file.Identity().String()
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
