// Package consumernames gives each JetStream consumer a physical name of its own at each
// creation, behind a logical name its readers know, so no consumer name is ever created twice.
//
// nats-server returns from a consumer delete while the old consumer's writer can still write
// its state file, and a create of the same name writes the same file: the late write can then
// fail the create (10012) or give the new consumer the old one's state, and the new consumer
// skips messages after a restart. A fresh name sends every late write to a directory no
// consumer reads.
//
// A logical name's selection record (Record) holds the physical name its readers use
// (Record.Current), the operation in flight (Op), the start saved before any destructive step
// (Record.Start), the current consumer's last ack floor (Floor), the names never selected again
// (Record.Retired), and a rollback in progress (Restore). A Store keeps the records with
// conditional writes: the session family keeps them in the KV bucket SessionBucket (KVStore),
// Dispatch and the Legion daemon in Postgres (PGStore). Names runs the protocol over a Store and
// the family's consumers (Server): Step 0 settles an open operation first, then Ensure creates,
// Replace replaces and Delete deletes, and a current the server no longer holds is created
// again at a start the record proves safe, or refused (ErrNoSafeStart).
//
// The protocol runs only in the fresh mode (ModeFresh), which a process reads once at its start
// from ModeEnvVar (ParseMode). The fixed mode, the default, is the family's own code before this
// package, with its fixed names, unchanged. In the fresh mode every call is refused until the
// store's sentinel says the cutover is done (Admit), and a missing store, sentinel or key is a
// refusal, never a fall back to fixed names.
package consumernames
