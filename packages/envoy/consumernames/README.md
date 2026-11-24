# consumernames

`github.com/sjawhar/envoy/consumernames` gives each JetStream consumer a physical name of its own at
each creation, behind a logical name its readers know, so no consumer name is ever created twice.

nats-server returns from a consumer delete while the old consumer's writer can still write its
state file, and a create of the same name writes the same file. The late write can then fail the
create (10012) or give the new consumer the old one's state, so after a restart the new consumer
skips what it was created to deliver. A fresh name sends every late write to a directory no
consumer reads.

## Names

| Family | Logical name | Fixed name (the fixed mode) | Physical name of each creation (the fresh mode) |
| --- | --- | --- | --- |
| Session consumer | the session id | `s-<session id>` | `session~<random>` |
| The listener's own durable | `listener-<machine id>` | the logical name | `listener-<machine id>~<random>` |
| Dispatch's durables | `delivery-events`, `delivery-jobs` | the logical name | `<logical>~<random>` |
| A Legion daemon's durables | `legion-go-<project>-<kind>` | the logical name | `<logical>~<random>` |

- `FreshName(prefix)` is `<prefix>~<random>`, where `<random>` is 22 characters of `[0-9A-Za-z]`,
  each drawn from `crypto/rand` (about 131 bits), so no name says anything of the next. A `~` is
  in no fixed name, so no fresh name is a fixed one.
- `Config.Prefix(logical)` is the part before the `~`: `session` for a session consumer, the logical
  name itself for a durable. `LogicalOf` cuts a durable's physical name at its last `~`. It does not
  give a session's logical name, which is the consumer's metadata (below).
- **Metadata.** Every fresh consumer carries `envoy.logical=<logical name>` (`LogicalMetadataKey`).
  A session consumer also carries `envoy.session_id=<session id>`.
- **Logical names.** `CheckKey` refuses a logical name before anything is written: an empty one,
  one holding `.`, `*`, `>`, `/`, `\`, white space or a control character (`ErrInvalidKey`), and
  one of 254 bytes or more, whose fixed name the server refuses (`ErrKeyTooLong`, with the server's
  own text). The listener's durable is kept under `DurableKey(durable)`: `durable.` and the name,
  with each byte outside `[-_a-zA-Z0-9]` written `=` and two hex digits. It is a key no session id
  can be.

## The record

Each logical name has one selection record (`Record`), kept with conditional writes on its
revision:

| Field | Meaning |
| --- | --- |
| `key` | the key the record is stored under (below); a read refuses a record carrying any other |
| `current` | the selected physical name, or empty |
| `op` | the operation in flight: `create`, `replace` or `delete`, with its owner, candidate, old name, reservation time, `floor_recorded`, `create_within` (the bound the reserving process's create of the candidate runs under, `Config.CreateBound`), and `start_pending` for a replace whose start is read from its old name after the reservation (the record's saved start stands until then, and stays if the replace closes without a create) |
| `start` | the saved start, written before any destructive step |
| `last_floor` | the current consumer's last ack floor, with the name and the stream identity it was read on |
| `retired` | names never selected again; only these are ever deleted by a cleanup |
| `abandoned` | the retired candidates whose create may still have been in flight when Step 0 or a retry retired them, each with the time until which its client still waits for it: the retirement and the operation's `create_within` |
| `restore` | a rollback in progress |

- **Lifetime.** A record is never deleted and never expires. Publishing a new `current` clears
  `last_floor`. A replaced predecessor whose delete the server confirmed leaves `retired` again;
  one whose delete failed stays there for the cleanup. A cleanup, and each retirement of a
  candidate, drops a retired name the server does not hold, except an abandoned candidate read
  absent before its time and the dropper's `Config.SettleAge` (2 s) after it have passed: its
  create can still land within its bound, and the consumer it makes must stay named, so the next
  cleanup deletes it. Names ends each create once the family's `Config.CreateBound` has passed
  since the operation's reservation, its `Config.Reserved` hook's time included, and sends no
  create past it: the bound its requests run under, the session family's consumer requests' 10 s,
  the listener's durable calls' 10 s. A retirement comes after the reservation, so its `until`, on
  the retirer's clock, is no earlier than the end of the client's wait for the create. The dropper
  reads `until` on its own clock, which can be another host's, and the settle age is the skew it
  allows. Two stated limits remain. `CreateBound` is only the client's deadline: nats-server
  serves a queued API request whenever it reaches it, so a create the server applies past the
  bound its client waited leaves a consumer no record names. So does a dropper whose clock is
  ahead of the retirer's by more than the settle age, when the create lands after the dropper
  dropped its name. Neither loses a message or deletes a consumer it should not: such a consumer
  holds a slot of the stream's consumer limit until the cutover hold's streams census removes it,
  or its own inactive threshold does. That threshold is no bound on a replicated hub: a session
  consumer's is at least 1 h but restarts at every leader election of the consumer
  (`bus.SessionConsumerInactiveThreshold`), and the listener's durable's is 7 days. A reader that
  deletes retired names itself (the operator's cleanup) drops each with
  `Record.DropRetired(name, deleted, now)`, the rule `Cleanup` drops by: it takes the name out of
  `retired` and `abandoned` together, so the record stays one the stores read, and keeps an
  abandoned name read absent before its `until` and the default settle age after it.
- **Where records live, and their keys.** A store reads and writes only its own family's
  sentinel and records:
  - The session family keeps its records in the KV bucket `envoy_session_consumers`
    (`SessionBucket`, `NewKVStore(request)`): one key per session id, the listener durables under
    `DurableKey`, and the sentinel under `envoy.bucket` (`SentinelKey`). Each read takes the key's
    last message from the stream's leader, so a delete or purge marker is seen as one.
  - Dispatch and the daemons keep theirs in the Postgres table `intake_consumers`
    (`IntakeConsumersTable`), through the store of the package `consumernames/pgstore`
    (`pgstore.New(family, db)`). It is a package of its own, the one that links a Postgres driver,
    so a binary that keeps only the KV store, the listener, links none. Several families can share
    one database, as several projects' daemons do. So a record of logical name L in family F is
    the row `F/L` (`PGKey`), and F's sentinel is the row `envoy.sentinel/F` (`PGSentinelKey`): for
    example `daemon/<project>/legion-go-<project>-github` and `envoy.sentinel/daemon/<project>`, or
    `dispatch/delivery-events` and `envoy.sentinel/dispatch`. Its `List` returns only the family's
    records, never a sentinel. `pgstore.New`, `DaemonFamily(project)` and `CheckStoreFamily`
    refuse any family but `session`, `dispatch` and `daemon/<project>` with a project that is not
    empty and holds no `/`, white space or control character (`ErrInvalidFamily`, naming it): a
    store of `daemon` alone would list every project's records, and one of `daemon/p/b` would
    write into `daemon/p`'s. Every store encodes and decodes its records and sentinel with
    `EncodeRecord`, `DecodeRecord`, `EncodeSentinel` and `DecodeSentinel`.
- **The sentinel** (`StoreState`) names its store's family (`session`, `dispatch` or
  `daemon/<project>`) and holds the cutover marker (`cutover`: empty, `done` or `rolled_back`), the
  cutover boundary B (`boundary`), the stream's identity, and a recovery's inputs (`replay_from`,
  `orphans`, the backup's sha256, the hold). Each store's `WriteSentinel` writes its own family
  itself when the state names none, and refuses a state that names another (`ErrSentinelFamily`,
  naming both), so no caller writes another family's sentinel, or one its store cannot read.
- **Strict read.** A value is a record only when it is one JSON object of the record's fields and
  nothing after it, carrying the key it was read under, with every field well formed. Any principal
  with a JetStream API grant can have the server store its answer under any key of the bucket
  through a request's reply subject, and one with a direct get can copy another key's message
  there whole. So any other value is `ErrMalformedRecord` for that key alone. Under the sentinel's
  key any value but the store's own exact sentinel is `ErrMalformedSentinel`: a sentinel that
  names another family or none (`ErrSentinelFamily`), one under another key, and one with a field
  this build does not know among them. It is never `ErrNoSentinel`, which is an absent sentinel
  alone, so a creator in the fixed mode that reads `ErrNoSentinel` as no marker reads a malformed
  one as a failed read.

  `DecodeKVValue(key, data) (record, state, isSentinel, err)` is the session bucket's decode, the
  one `KVStore`'s `Read` and `State` call: under `envoy.bucket` the session family's sentinel
  (`isSentinel` true), under any other key that key's record, and anything else
  `ErrMalformedSentinel` or `ErrMalformedRecord`, naming the key. A reader of the bucket's raw
  messages, such as an operator's walk of the stream, decodes each value with it as the store
  does. It reads values only: a message whose `KV-Operation` header marks a delete or purge is the
  reader's to answer, as `ErrRecordDeleted`, or `ErrNoSentinel` under `envoy.bucket`, before it
  decodes anything.
- **The family fence.** A record's current, retired names and candidate must be
  `<Prefix(logical)>~<random>` names of its own key. Its operation's old name may also be the key's
  fixed name, which the cutover replaces. Anything else is `ErrMalformedRecord`, naming the field.
  Every creator applies the fence to every record it reads.

  The prefix, the fixed name, the metadata and the key rule come from one table in this package
  (`Naming`, `FamilyNaming`): `SessionNaming()` (`session~<random>`, fixed `s-<session id>`, metadata
  `envoy.logical` and `envoy.session_id` both the session id, keys `CheckKey` passes),
  `DurableNaming()` for a key `DurableKey` made (`<durable>~<random>`, fixed and `envoy.logical` the
  durable's name, keys `DurableKey` made of a listener's durable, `listener-<machine id>`), and
  `LogicalNaming()` for Dispatch's and a daemon's durables (`<logical>~<random>`, fixed and
  `envoy.logical` the logical name). Every creator builds its `Config.Prefix`, `Config.Fixed`,
  `Config.Metadata` and `Config.CheckKey` from it.
  `FamilyNaming(family, logical)` takes only the family's own keys: Dispatch's store only
  `delivery-events` and `delivery-jobs` (`DispatchDeliveryDurable`, `DispatchJobDurable`), a
  daemon's store only `legion-go-<project>-<kind>` of its own project for the kinds `dispatch`,
  `github`, `wake` and `ready`, and the session store a key `CheckKey` passes or the `DurableKey` of
  a listener's durable (`ListenerDurablePrefix`, the prefix `bus.ListenerDurable` names it by). Any
  other key is `ErrInvalidKey`, naming it, and the `Naming` it gives carries the family's rule
  (`Naming.CheckKey`), so a `Names` built from it refuses another family's key before it reads the
  record. The fence judges a record's names by its key's own prefix, so without the key rule a
  record of a daemon's store keyed by the listener's durable, or a session-store record keyed by
  `DurableKey` of a daemon's durable, would name that durable as its own.
  `CheckFamily(family, logical, record)` is the fence by that table, the one a reader of the store
  uses (an operator's status, census or cleanup, in a module that cannot import the creators'),
  and it answers what the family's creator answers. Its nil says only that the key is of the
  family and the record's names are of its key's family. The record must come from a store read or
  `DecodeKVValue`, which check its key and fields, and a name it gives must pass the owner check
  (`OwnsFamily`) before anything reads, binds, updates or deletes it: every session's prefix is
  `session`, so a record of one session can name another session's consumer and pass the fence.
- **The owner check.** A consumer a record names is used only when the consumer's own metadata
  names the key (`Config.Metadata`), or it is the key's fixed name. Every creator checks it before
  every read, adoption and delete of such a name, so a record that names another key's consumer is
  malformed, and nothing reads, binds, updates, adopts or deletes that consumer for it.
  `OwnsFamily(family, logical, info)` is the same check by the table, the one a reader that is no
  creator asks before it deletes a consumer a record names; it and each creator's own check are
  one implementation over one table.

## The protocol (`Names`)

- **Step 0, first in every call:** an open operation is settled. A published replace of the key's
  fixed name whose delete the server has not confirmed (its candidate already the current) deletes
  that fixed name first, before anything else, and only then closes; a delete that fails is the
  call's error and keeps the replace open. Otherwise its candidate, if the server holds it, is
  published (adopted). If not, once the operation is 2 s old (`Config.SettleAge`), the candidate is
  retired and a fresh one is created at the saved start. A call that creates nothing (`Resolve`,
  `Delete`, `Cleanup`) retires the candidate and closes the operation instead, but closes a
  convert's replace of the fixed name only once that name is gone: with a saved start it deletes
  the fixed name first, a failed delete being the call's error with the replace kept open; with no
  saved start it leaves the replace open for a call that creates.
- **Adoption.** A candidate this call did not see created (Step 0, or a create whose answer was
  lost) is published only when it is as the family creates it (`ErrUnexpectedConsumer` otherwise,
  naming it, and the operation stays open):
  - its start is the one the operation reserved: the native policy of a native first create, else
    a start by sequence no later than the saved start;
  - every setting no update changes is as the family's create gives it (`Config.Adopt`;
    `bus.Client.ConsumerAdoptable` compares the server's own answer with the family's whole create
    configuration). That means explicit acks, no start time, instant replay, the stream's storage,
    no deliver group, no flow control, no heartbeat, no pause, neither direct nor sourcing. For a
    session it also means a pull consumer at the server's default max waiting; for the listener's
    durable, a push consumer delivering to a reply inbox under the adopting connection's own inbox
    prefix (`_INBOX.`, or an edge listener's `_INBOX_envoy_edge_<machine id>`), where its create
    names one. The comparison is an allowlist: a setting the server answers that this build does
    not know is refused too.
  - A paused candidate is refused, never corrected. nats-server v2.15.1 sets a consumer's own
    pause back on every create or update of a consumer that exists (server/jetstream_api.go
    :5261-5264, server/jetstream_cluster.go :12678-12680), so only a `CONSUMER.PAUSE` request
    changes it, and no listener grant holds that request. Its operation stays open, as for any
    refused candidate, until an operator repairs the key.

  Every setting an update changes is then written back to the family's create (the filters, max
  batch, headers only, ack wait and the others; `bus.CorrectedConfig`), in place, before anything is
  delivered. A read of the candidate that fails is returned as itself, not as a refusal.
- **Create** (no current): the start is the record's `start`, else the family's first-create
  rule, and for a session key with no record no later than the cutover boundary B. The call
  reserves the operation and the start in one conditional write, creates the candidate once, then
  publishes it. `Config.NativeFirstCreate` keeps a family's native first create (the daemon's
  deliver policy new) for a logical name that never existed.
- **Replace:** reserve, create at the saved start, publish, retire and delete the predecessor. A
  predecessor that is the key's fixed name (a convert's replace) is deleted and never retired: the
  replace stays open, naming it, until the server confirms its delete, so a failed delete is
  retried by the next call's Step 0.
- **Delete:** reserve, read and save the floor (one that cannot be read stops the call), delete,
  clear the current.
- **At a full cap:** the retired names the server holds are deleted, those it does not hold are
  dropped by `DropRetired`'s rule, and the create is tried once more with a fresh candidate. The
  refused candidate is not retired: the server answered its create, so it names no consumer, now
  or later. The cap is never updated (`Config.NoDelete` answers at once for a family that cannot
  delete).
- **A missing current:** a record whose current the server no longer holds is created again at its
  `last_floor` plus one when that floor was read from that consumer on this stream, else at its
  saved start. With neither, the call is refused (`ErrNoSafeStart`). It never searches the
  server's other consumers and never uses a first-create rule.
- **Ambiguous answers:** an unanswered create is checked with an info: present means adopt (as
  above), missing means retire. No name is sent to the server's create twice.
- **A bounded record:** each retirement of a candidate (Step 0's, or a retry's after an unanswered
  create) also drops the retired names the server no longer holds, by `DropRetired`'s rule. So a
  key refused at every call, at a full cap or by a create the server keeps refusing, keeps only the
  candidates retired less than their create bound and the settle age ago, and each call reads only
  those.

## The two modes

Each process reads `ENVOY_CONSUMER_NAMES` (`ModeEnvVar`) once at its start, exactly
(`ParseMode`):

- **`fixed`** (also when unset): the family's code before this package, with its fixed names,
  unchanged. A process changes mode only by a restart with a new value. A reconnect never changes
  it, and nothing a store holds selects it. A creator in the fixed mode reads its store's cutover
  marker and refuses to run fixed names over a store the cutover converted. The listener does
  this per NATS connection; see `packages/envoy/README.md`.
- **`fresh`**: the protocol above. Every call is refused (`ErrCutoverNotRun`) until the store's
  sentinel says `cutover = done` (`Admit`).
- Any other value, a padded one or another case among them, refuses the start, naming the
  setting and the value.

## Missing selection state

In the fresh mode lost state is a refusal at the first call, never a fall back to fixed names. A
missing marker cannot tell a store before its cutover from one whose state was lost after it.

| State | Error |
| --- | --- |
| the bucket or table is missing | `ErrNoStore` (only the server's own "stream not found", 10059; a read or a write no server answers is a failed read or write, wrapping `nats.ErrNoResponders`, as a timeout is, and never a refusal) |
| the sentinel is absent (no row; in the bucket, no message or a delete or purge marker) | `ErrNoSentinel` |
| the sentinel key holds anything but the store's own exact sentinel: malformed, another family's, under another key, or with a field this build does not know | `ErrMalformedSentinel` (`ErrSentinelFamily` too for another family's) |
| the cutover has not run, or was rolled back | `ErrCutoverNotRun` |
| a key whose last message is a KV delete or purge marker | `ErrRecordDeleted` |
| a key holding something other than its own record | `ErrMalformedRecord` |
| a missing current with no safe start | `ErrNoSafeStart` |
| an open create's candidate that another principal made first, with the key's own metadata and a setting the family's create never gives and no update changes | `ErrUnexpectedConsumer`, naming the candidate and the setting. A session's registrations are refused for that session alone. The listener's own durable stops the listener at every start (`subscribe refused, shutting down`, as a refused durable does today). An accepted residual: it fails closed. The remedy is the operator's `envoy-infra consumer-names replace --listener` for the durable, and a replay of the key (`consumer-names replay`) for a session |
| in the fixed mode, a cutover marker read answered "stream not found" (10059) by a cluster member that has not yet applied the bucket's create | read as no bucket, so no marker. A stated limit: the session bucket is created, recreated and restored only inside a writer hold, while every creator is held, and the hold's restart round waits for the stream before it releases them, so no fixed-mode reader meets that answer outside a hold |
| in the fixed mode, the listener beside Dispatch, whose user holds no read of the bucket at this build | its refused read is no marker, so the gate opens, and after the session family's convert deletes its `listener-<machine id>` it creates that name again: a create of the same name. An ordering requirement: the convert runs only once that listener's grant reads the bucket (Dispatch's fresh-names change adds the read), so its read says `done` and it stops. In the fresh mode its start's durable check is refused and it exits |

Two more stated limits. A hub whose JetStream limits set `max_request_batch` has the server fill
`max_batch` on every pull consumer again after each correction, so the listener would send an
update on every `Ensure` and never keep a listed consumer; envoy-infra's `gcp:ClusteredNatsHub`
has no such input, and the deployment sets none. A refused paused candidate stays on the server
once the operator's replay has overwritten its operation, named by no record and holding a slot
of the stream's consumer limit, until the cutover hold's streams census removes it as an orphan.

A key never written, while the sentinel is present, is a new name, and the family's first-create
rule applies. A family can refuse that too (`Config.MissingRecord`): Dispatch's two durables, and a
daemon's checkpoint with no record. `IsSelectionStateLost` reports the lost-state errors together.

**What this does not stop.** A NATS user that receives every reply on the account (`_INBOX.>`, as
the Legion daemon's does) can read a pending candidate's name and the messages the listener pulls;
that is the user's grant, not this package's. A login that shares a family's Postgres database can
write rows under another family's keys, and a well-formed record there is read (its consumer still
passes the owner check only with the key's own metadata); that is the database's access, not this
package's.

## Recovery

Recovery is an operator's step inside a writer hold, never a rebuild from the stream's consumers:
no call of this package reads another consumer to guess a start.

- **A lost or malformed key:** the operator writes the key's record with a start and no current
  (a replay). The next call creates a fresh name at that start.
- **A lost bucket:** the operator creates it again, from a backup or with a replay boundary.
  While `replay_from` is set, a key with no record starts there. The sentinel lists the orphans
  the recovery found, and a shared listener's sweep ends replay mode once none of them exists
  (`KVStore.EndReplay`); an edge listener's does not, since its grant writes no sentinel.
- **A malformed sentinel:** the operator rewrites it. Until then the fresh mode refuses every call,
  and the fixed mode reads again rather than run fixed names over a marker it cannot read.
