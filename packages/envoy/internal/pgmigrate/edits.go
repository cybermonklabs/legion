package pgmigrate

import (
	"fmt"
	"io"
	"strings"
)

// BaseFile is a forward migration file as a base commit holds it: its name and its whole text.
type BaseFile struct {
	Name string
	Text []byte
}

// EditCheck is CheckEdits' answer for one store's set: how many files the base holds were compared
// with the head's file of the same version, and how each compared.
type EditCheck struct {
	Store string
	// Compared is the number of the base's forward migration files.
	Compared int
	// Unchanged is the number whose head file has the base file's identity.
	Unchanged int
	// Declared is the number whose head file differs and declares the base file's identity in a
	// replaces line, which a database that ran the base's file accepts.
	Declared int
	// Refusals name each base file whose head file differs and declares no replaces line naming
	// it, and each the head no longer has a file of the version for.
	Refusals []error
}

// CheckEdits compares each forward migration file of base, the files a base commit holds, with the
// head's file of the same version (head, as Load gives it). A database may already have run the
// base's file, so a head file of other text or another name must declare the base file's identity
// in a replaces line, or the runner refuses that database at its next boot (Reconcile). It must
// also keep the identity a runner records of the version (Migration.RecordedIdentity) as the
// base's file has it: its first replaces line names the base's first one, or the base file itself
// where the base declares none, so every release of the chain accepts what any of them records. A
// pull request that edits an applied migration in place, a comment included, is caught here,
// before any database boots it. A base file whose version the head has no file for is refused as
// well: every database that ran it keeps a version no file of the head names.
func CheckEdits(store string, base []BaseFile, head []Migration) (*EditCheck, error) {
	byVersion := make(map[int]Migration, len(head))
	for _, migration := range head {
		byVersion[migration.Version] = migration
	}
	check := &EditCheck{Store: store}
	for _, file := range base {
		stem, ok := strings.CutSuffix(file.Name, ".up.sql")
		if !ok {
			continue
		}
		version, err := parseVersion(stem)
		if err != nil {
			return nil, fmt.Errorf("the base's migration %s: %w", file.Name, err)
		}
		baseFile := Migration{Version: version, Name: file.Name, Checksum: checksumOf(file.Text)}
		if baseFile.Replaces, err = readReplaces(file.Name, version, string(file.Text)); err != nil {
			return nil, fmt.Errorf("the base's migration %s: %w", file.Name, err)
		}
		check.Compared++
		baseIdentity, first := baseFile.Identity(), baseFile.RecordedIdentity()
		migration, ok := byVersion[version]
		switch {
		case !ok:
			check.Refusals = append(check.Refusals, fmt.Errorf(
				"the base holds %s and this head has no migration of version %d: every database that ran it keeps a version no file of this head names; keep the file",
				baseIdentity, version))
		case migration.Identity() == baseIdentity:
			check.Unchanged++
		case !containsIdentity(migration.Replaces, baseIdentity):
			check.Refusals = append(check.Refusals, fmt.Errorf(
				"%s is %s, and the base holds %s under version %d: a database that ran the base's file refuses this head at its next boot; leave the file as the base has it, or keep its replaces lines and add this line after them in %s: -- replaces: %s",
				migration.Name, migration.Identity(), baseIdentity, version, migration.Name, baseIdentity))
		case migration.RecordedIdentity() != first:
			check.Refusals = append(check.Refusals, fmt.Errorf(
				"%s's first replaces line names %s, and a runner records version %d as %s, the identity the base's file gives it: a release before this one refuses what this head records; keep every replaces line the base's %s has, in its order, before the new one",
				migration.Name, migration.RecordedIdentity(), version, first, file.Name))
		default:
			check.Declared++
		}
	}
	return check, nil
}

// containsIdentity says whether identities holds identity.
func containsIdentity(identities []Identity, identity Identity) bool {
	for _, candidate := range identities {
		if candidate == identity {
			return true
		}
	}
	return false
}

// Write prints the check, one fact to a line. A check that compared no file says so: it saw
// nothing, which is no pass.
func (c *EditCheck) Write(w io.Writer) {
	if c.Compared == 0 {
		fmt.Fprintf(w, "edited-migrations: %s: NOTHING COMPARED: the base holds no forward migration of this store\n", c.Store)
		return
	}
	fmt.Fprintf(w, "edited-migrations: %s: compared the %d migrations the base holds: %d unchanged, %d edited declaring the base's identity, %d refused\n",
		c.Store, c.Compared, c.Unchanged, c.Declared, len(c.Refusals))
	for _, refusal := range c.Refusals {
		fmt.Fprintf(w, "edited-migrations: %s: REFUSED %v\n", c.Store, refusal)
	}
}
