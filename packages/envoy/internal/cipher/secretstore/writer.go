package secretstore

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

// SignIn is how a store's writer signed in, as the agent-secrets CLI says it.
type SignIn interface {
	// Identity is "<source>: <principal>", the line a form prints before its first store call.
	Identity() string
	// ChooseAnother is the clause, starting "choose another", a refusal adds: how to sign in as
	// someone else.
	ChooseAnother() string
	// RerunPrefix is the shell assignments a printed command needs to run under the same sign-in,
	// "" or ending in a space; it never holds a key or a token.
	RerunPrefix() string
	// PersonEmail is the person the sign-in is, for the owner tag a create writes.
	PersonEmail(ctx context.Context) (string, error)
}

// AccountGuard is a writer that can tell whether its sign-in is in the account the broker's
// settings name, before any write.
type AccountGuard interface {
	RequireAccount(ctx context.Context) error
}

// Failures reads a store's errors for the CLI's messages.
type Failures interface {
	// AccessDenied reports whether err is the store refusing the caller's credential.
	AccessDenied(err error) bool
	// DefiniteRefusal reports whether err is the store answering before it acted: nothing was
	// written.
	DefiniteRefusal(err error) bool
	// MissingPermission is the permission err's refusal names, "" when it names none.
	MissingPermission(err error) string
	// TagReadPermission and ValueReadPermission name what a reader needs to read a secret's tags
	// and its value, as a message names them.
	TagReadPermission() string
	ValueReadPermission() string
}

// Failure is a store call's failure as the CLI prints it: the call, the words saying what went
// wrong, and the permission it lacked, never the service's own text (Err), which may quote a
// value or a policy; Err stays reachable through Unwrap.
type Failure struct {
	Call, Words, Permission string
	Err                     error
}

func (f Failure) Error() string {
	msg := f.Call + ": " + f.Words
	if f.Permission != "" {
		msg += " (needs " + f.Permission + ")"
	}
	return msg
}

func (f Failure) Unwrap() error { return f.Err }

// DiscardOutcome is what a failed Create.Begin or a Create.Discard left in the store. A discard
// answers an error exactly when its outcome is one of the kept ones, the error naming what stays
// and the command that clears or checks it; Begin's own failure is an error whatever its outcome.
// The CLI reads it to decide whether the broker rereads the name.
type DiscardOutcome int

const (
	// MadeNothing: nothing was made, so there was nothing to remove (a store whose create holds
	// the value, or a create the store refused before acting).
	MadeNothing DiscardOutcome = iota
	// DeletedEmpty: the empty secret this create made is deleted, or was already gone.
	DeletedEmpty
	// KeptEmpty: the store refused the delete; this create's empty secret stays.
	KeptEmpty
	// KeptMayHoldValue: the secret holds a version, whether this create or another writer added
	// it, or this create's add may still land, so it stays.
	KeptMayHoldValue
	// KeptOthers: the name is held by a secret this create did not make, or one that changed before
	// the delete, so it is left as it is.
	KeptOthers
	// KeptUnknown: what the name holds could not be read, or the secret's versions could not be
	// listed, so it stays.
	KeptUnknown
)

// Create is one secret's create, from before its first store call until its value is stored or
// its discard has run. A store that makes the secret before its value (Google Cloud) makes it in
// Begin and adds the value in Store; one that makes it with its value does it all in Store.
type Create interface {
	// Begin makes what must exist before the value is read; MadeNothing for a store that makes the
	// secret with its value. On failure it settles what it made itself.
	Begin(ctx context.Context) (DiscardOutcome, error)
	// Store writes value as the secret's first value.
	Store(ctx context.Context, value string) error
	// Discard removes what Begin made, after Begin succeeded and Store did not.
	Discard(ctx context.Context) (DiscardOutcome, error)
	// Unsettled names what may be left in the store after a failure whose outcome is open, and the
	// command that checks it, "" when nothing is.
	Unsettled() string
	// MadeBeforeValue reports whether Begin makes the secret before its value.
	MadeBeforeValue() bool
}

// HeldTags is a secret's tags as one read of the store held them, with that read's etag, which a
// conditional tag write sends so it lands only on the tags it was checked against ("" where the
// store's tag writes take no condition), and its Source, what Read takes, so a retag's value read
// costs no Describe of its own.
type HeldTags struct {
	Tags   map[string]string
	Etag   string
	Source string
}

// TagCheck checks a secret's tags after a write. A store whose tag write takes no condition reads
// the tags back after it and answers TagsLanded{check(after)} when check refuses them.
type TagCheck func(after map[string]string) error

// ErrTagsChanged is a tag write another write raced: it changed the secret after the read the
// write was made against. A store with conditional tag writes refuses the write, so nothing is
// written; one whose tag calls take no condition finds the race by reading the tags back, where its
// own write has already landed beside the other writer's, and marks it so (TagsLanded).
var ErrTagsChanged = errors.New("another write changed the secret after this retag read it")

// TagsLanded is a tag write's error that came after the write itself landed. Its text is the
// error's own; it only marks the error, so a message can say the write landed (Landed).
type TagsLanded struct{ Err error }

func (e TagsLanded) Error() string { return e.Err.Error() }

// Unwrap is the error, so errors.Is still finds ErrTagsChanged through it.
func (e TagsLanded) Unwrap() error { return e.Err }

// TagsUnknown is a tag write's error that leaves open whether the write landed: the store answered
// with no definite refusal, which may follow a call it carried out. It has no Unwrap: the store's
// error is in its text alone, so no reader of a refusal takes an open outcome for one.
type TagsUnknown struct{ Err error }

func (e TagsUnknown) Error() string {
	return e.Err.Error() + "; the store's answer leaves open whether this write landed"
}

// Landed reports whether err, a tag write's error, came after the write landed (TagsLanded).
func Landed(err error) bool {
	var landed TagsLanded
	return errors.As(err, &landed)
}

// Unknown reports whether err, a tag write's error, leaves open whether the write landed
// (TagsUnknown).
func Unknown(err error) bool {
	var unknown TagsUnknown
	return errors.As(err, &unknown)
}

// ErrNoRestore is a store with no call that restores a deleted secret.
var ErrNoRestore = errors.New("the store has no call that restores a deleted secret")

// RunChild starts cmd in a process group of its own, kills that group when cmd's context ends or a
// signal ends the form, and waits for it. The CLI passes its own to every writer it opens.
type RunChild func(cmd *exec.Cmd) error

// Settings are the broker's GET /v1/settings as the CLI read them: the store, its prefix, and the
// store's own fields; Words prints a field's value, or nothing for an address that prints none.
type Settings struct {
	Store, Prefix string
	Fields        map[string]string
	Words         func(string) string
}

// WriterOptions are what the CLI hands every writer it opens.
type WriterOptions struct {
	Run RunChild
}

// ProfileHint is a store's clause of the --profile refusal.
type ProfileHint struct {
	// Rank is where the clause goes: Google Cloud 10, AWS 20, 1Password 30.
	Rank int
	// Clause starts lower case and ends with no stop.
	Clause string
}

// Writer is what the agent-secrets CLI writes a store with, signed in as its caller.
type Writer interface {
	Store
	SignIn
	Failures
	// PrepareCreate is the create of the secret id with tags, made by Begin and Store.
	PrepareCreate(id string, tags map[string]string) Create
	// Set writes a new current value.
	Set(ctx context.Context, id, value string) error
	// CurrentTags reads the secret's tags for a tag write.
	CurrentTags(ctx context.Context, id string) (HeldTags, error)
	// SetTags writes tags, keeping every other tag, against held; check reads the tags back where
	// the store's tag writes take no condition.
	SetTags(ctx context.Context, id string, held HeldTags, tags map[string]string, check TagCheck) error
	// RemoveTags removes keys, keeping every other tag, against held.
	RemoveTags(ctx context.Context, id string, held HeldTags, keys []string, check TagCheck) error
	// Delete schedules the secret for deletion and answers when the store removes it for good,
	// the zero time where it keeps it archived with no end.
	Delete(ctx context.Context, id string) (time.Time, error)
	// Restore brings a deleted secret back, or answers an error wrapping ErrNoRestore and leaves
	// it deleted.
	Restore(ctx context.Context, id string) error
	Close() error
}
