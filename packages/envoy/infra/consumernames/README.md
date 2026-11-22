# `envoy-infra consumer-names`

`envoy-infra consumer-names` (package `consumernames`) is the operator's side of the fresh
JetStream consumer names. A creator in the fresh mode gives each consumer of `ENVOY_NOTIFICATIONS`
a name of its own, `<base>~<nuid>`, which it creates once and never again, and selects it through a
record kept under the consumer's logical name. In the fixed mode, the default and every release's
before it, a logical name has one physical name, its fixed name. A family's records are its store:

| Family | Its consumers' fixed names | Fresh names | Its store |
| --- | --- | --- | --- |
| `session` | each session's `s-<session id>`; each listener's durable `listener-<machine id>` | `session~<nuid>`; `listener-<machine id>~<nuid>` | the KV bucket `envoy_session_consumers` (stream `KV_envoy_session_consumers`): a session's record under its session id, a listener durable's under `durable.<durable name>`, each byte outside `[-_a-zA-Z0-9]` written `=` and two hex digits |
| `dispatch` | Dispatch's two durables, `delivery-events` and `delivery-jobs` | `<fixed name>~<nuid>` | the `intake_consumers` table of Dispatch's database |
| `daemon/<project key>` | a Legion daemon's `legion-go-<project key>-<kind>`, kind `dispatch`, `github`, `wake` or `ready` | `<fixed name>~<nuid>` | the `intake_consumers` table of that daemon's own database |

Each store holds a sentinel beside its records: the bucket's key `envoy.bucket`, a table's row
`envoy.sentinel/<family>` (`envoy.sentinel/dispatch`, `envoy.sentinel/daemon/<project key>`). Its
`cutover` marker reads `done` once `convert` (or, for a daemon store made later, `enroll`) has
converted the family, and `rolled_back` once `restore --finish` has rolled it back. The secrets
broker keeps a cutover row for each family beside the markers.

The commands move every family from the fixed names to fresh ones (the cutover), recover a lost
bucket, roll every family back to the fixed names, enrol a daemon store made after the cutover,
write the bucket's sentinel again, and delete the names the records retired. Each runs as the
deployment's one operator identity, whose NATS grant is the writer hold's phase's, and reads the
hold row first: `status` and `backup` run only while the row is `released`, `prepare` only while
it is `prepare` under `--hold`, and every other command only while it is `held` under `--hold`, in
a hold whose workflow names it. The hold row, its journal and its workflows are `envoy-infra
hold`'s ([../README.md](../README.md#envoy-infra-hold)).

## Flags

```text
envoy-infra consumer-names status          [--hold ID]
envoy-infra consumer-names backup          --out DIR
envoy-infra consumer-names prepare         --hold ID
envoy-infra consumer-names create-bucket   --hold ID [--replicas N]
envoy-infra consumer-names create-bucket   --hold ID --after-loss [--replay-from SEQ] [--backup DIR] [--replicas N]
envoy-infra consumer-names replay          --hold ID --key K --from SEQ [--final]
envoy-infra consumer-names convert         --hold ID
envoy-infra consumer-names restore         --hold ID --prepare
envoy-infra consumer-names restore         --hold ID --key K --from SEQ
envoy-infra consumer-names restore         --hold ID --finish
envoy-infra consumer-names cleanup         --hold ID [--gone PROJECT-KEY] [--final]
envoy-infra consumer-names replace         --hold ID --listener MACHINE-ID [--final]
envoy-infra consumer-names enroll          --hold ID --daemon PROJECT-KEY
envoy-infra consumer-names repair-sentinel --hold ID
```

### The hold's and the hub's flags

These are `writerhold.HubFlags`, the flags `envoy-infra hold` and `envoy-infra buckets` take.

| Flag | Default | Which commands need it |
| --- | --- | --- |
| `--broker-url U` | none | every command: the secrets broker, whose hold routes keep the hold row, the journal, the hold's records and the cutover rows |
| `--token-file F` | none | every command: the operator's ServiceAccount token, read at each use, for the broker's routes and as the operator's NATS credential |
| `--verify-keys K` | none | every command: `BROKER_NATS_HOLD_SIGNING_KEYS`, the keys every row and record is verified under |
| `--nats URL` | none | every command, once for each of the hub's client URLs |
| `--signing-key-secret projects/P/secrets/operator-only-envoy-hold-signing` | none | every command but `status` |
| `--kube-namespace`, `--statefulset`, `--configmap` | `envoy`, `envoy-nats`, `envoy-nats-config` | every held command: the hub's Kubernetes objects, reached through the in-cluster configuration or KUBECONFIG's, beside the Compute API; `cleanup`, `cleanup --gone` and `enroll` read the daemon stores' addresses through the same client (the Deployments of namespace `legion` and the ConfigMap `envoy-operator/envoy-operator-retired-stores`; [The daemon stores' addresses](#the-daemon-stores-addresses)), and so does `status` for a daemon store whose database answers that it does not exist |
| `--probe-token-file F` | none | `restore --finish`, which refuses to run without it: the hold probe's token |

Their values are the ones `envoy-infra hold` takes. `--minters-report` is accepted, as every
command of the hold accepts it, and no `consumer-names` command reads it. Below, `<hold flags>`
stands for `--broker-url`, `--token-file`, `--verify-keys`, `--signing-key-secret` and each
`--nats`, and the Kubernetes flags where their defaults are not the hub's. `status` takes them all
and reads no signing key; it reads Kubernetes only for the daemon stores' addresses, for a daemon
store whose database answers that it does not exist.

### The stores' flags

| Flag | Its file |
| --- | --- |
| `--dispatch-database-file F` | first line: Dispatch's database URL, with the operator's credential |
| `--daemon-database PROJECT-KEY=F` | once for each daemon, each project once. First line: the daemon's database URL, with the operator's credential. Second line: the connection name of the Cloud SQL instance that URL reaches through the operator's own proxy |

Each line is read with its surrounding white space cut. A file whose first line is empty is
refused, naming the flag; the URL is never echoed. Every command opens the URL on the first line.
Every command that reads a daemon store reads it at the one source of its address (below), so it
reads the second line of that daemon's file too: `envoy-infra streams census`, `cleanup`, `cleanup
--gone`, `convert`, `enroll`, `replay` of a daemon's key and `restore`. `status` reads it only for a
daemon store whose database answers that it does not exist, so a file of one line serves `status`
otherwise.
Below, `<store flags>` stands for `--dispatch-database-file` and one `--daemon-database` for each
daemon:

```text
postgres://<operator user>:<password>@127.0.0.1:5432/<database>
example-project:example-region:example-instance
```

### The daemon stores' addresses

Each Legion daemon runs as a Deployment in namespace `legion` that carries three annotations:

| Annotation | Value |
| --- | --- |
| `envoy-infra.consumer-names/project` | the daemon's project key |
| `envoy-infra.consumer-names/store-database` | the name of the daemon's database |
| `envoy-infra.consumer-names/store-instance` | the connection name of the Cloud SQL instance that holds it |

A project retired after the cutover runs no Deployment, and its broker cutover row `daemon/P`
stays. The deployment renders its store's address in the ConfigMap `envoy-operator-retired-stores`
of namespace `envoy-operator` instead, as two data keys for each retired project P:

| Data key | Value |
| --- | --- |
| `P.database` | the name of the retired daemon's database |
| `P.instance` | the connection name of the Cloud SQL instance that holds it |

A key of neither form makes every reading of the addresses fail, naming the key, and so does a
read of the ConfigMap that Kubernetes refuses. The deployment renders the ConfigMap always, with no
data when no project is retired; a retired private project has no entry. A ConfigMap that does not
exist holds no retired project either.

`envoy-infra streams census`, `cleanup`, `cleanup --gone` and `enroll` read a daemon's store only
at the address its one source names, and `status` calls a store gone only at that address: the
address rule. The store is unreconciled, and none of them reads it, unless exactly one source names
the project, a Deployment or a retired project's entry and never both, that source names both the
database and the instance, no other source names the same database on the same instance, the
`--daemon-database` file's URL names that database, and the file's second line is that instance.
Each refusal names the project and the source: `Deployment legion/<name>` or `ConfigMap
envoy-operator/envoy-operator-retired-stores`. At that address, a server that answers SQLSTATE
3D000 (the database does not exist) makes the store gone. Every other answer leaves it
unreconciled: a server that does not answer, any other SQLSTATE (57P01 and 57P03, which a dying
server answers in the seconds after its instance is deleted, among them), and a database with no
`intake_consumers` table.

### Output and exit codes

A command prints what it did on standard output, and its refusal on standard error as
`envoy-infra consumer-names <word>: <what>`, or `envoy-infra consumer-names <word>: UNKNOWN <what>`.

| Code | Meaning |
| --- | --- |
| 0 | done |
| 1 | refused, naming what |
| 2 | a usage error, or UNKNOWN: a reading that is not whole, which is never a pass |

A usage error echoes no value: a stray word is named by its position, a malformed value by its
flag, and a flag value that holds an nkey seed is refused naming the flag alone.

## Reading the state: `status`

```sh
envoy-infra consumer-names status --broker-url U --token-file F --verify-keys K --nats URL... \
  --dispatch-database-file F [--daemon-database PROJECT-KEY=F]... [--hold ID]
```

`status` runs only while the hold row is `released`, so run it before a hold and after one; it
takes `<hold flags>` too, and reads no signing key. `--hold` takes any hold's id; the `row:` line
names the latest. It reads the hold row; with `--hold`, that hold's journal; the broker's cutover
rows; every consumer name of `ENVOY_NOTIFICATIONS`; every key of the selection bucket, by sequence
(`STREAM.INFO` and `STREAM.MSG.GET`, the two reads the released grant holds on it); Dispatch's
table, always; and the table of each daemon that a `--daemon-database`, a broker cutover row or a
consumer on the stream, fixed or fresh, names. It changes nothing. Each Postgres store needs its
flag, and reads its file's first line: a store no flag names makes the reading UNKNOWN, and the
UNKNOWN line names each such store and its flag (`daemon/P's store: no --daemon-database P=<file>
names it`), so a first run tells which daemons the next needs. Only for a daemon store whose
database answers that it does not exist does it read the file's second line and the daemon stores'
addresses in Kubernetes (below). While a hold is set, `envoy-infra hold status --hold ID` prints
the row and that hold's journal instead. What `status` reads before a hold can change before
`convert` runs: `prepare` lists the fixed names again, `create-bucket` makes the bucket, and every
client runs until `hold set`.

It prints, in this order:

| Line | Meaning |
| --- | --- |
| `row: gen G, phase P, hold "H"` | the hold row: its generation, its phase, and the hold it names |
| `hold H: workflow W, record gen G, complete C, hold_gen N` | with `--hold H`: that hold's journal, its workflow, its record's generation, `complete true` once a final command is done, and the row's generation when it completed |
| `  N. COMMAND STATE` | each journal entry: the command and `intent`, `done` or `stop` |
| `cutover F: STATE (hold H, hold_gen N)` | the broker's cutover row of family F, `done` or `rolled_back`, the hold that wrote it, and the `hold_gen` the broker recorded with it |
| `store F: missing` | F's store does not exist: the bucket, or the family's `intake_consumers` table |
| `store F: gone: database D on instance I (the operator's file states S): ANSWER` | daemon F's store is gone: its database answers SQLSTATE 3D000 at exactly the address F's one source names (its Deployment, or a retired project's entry). No `BLOCKER` line; see below |
| `store F (WHERE): no sentinel` | the store exists and holds no sentinel |
| `store F (WHERE): cutover=C boundary=B replay_from=R orphans=[...] hold=H` | the store's sentinel. WHERE is the bucket's stream or `intake_consumers`; `cutover` and `hold` read `-` when empty, `boundary` and `replay_from` `0`, `orphans` `[]` |
| `store F (WHERE): sentinel UNKNOWN, its value is malformed` | the bucket's sentinel holds a value that is no exact store state; `status` then exits 2 |
| `record F K: current NAME (on the stream), start S, ...` | each record: its current (`-` for none) marked `(on the stream)` or `(absent)`, an open operation (`op KIND candidate C old O`; its `BLOCKER` line adds the owner and the time it was reserved), its start, its last floor read (`last_floor N of NAME on stream ID`), its retired names, each marked `(on the stream)` or `(absent)`, and a rollback's `restore target T start S phase P` |
| `record F K: deleted` | K's last write in the bucket is a delete or purge marker |
| `record F K: UNKNOWN, its value is malformed` | K's value is no exact record; `status` then exits 2 |
| `BLOCKER ...` | each finding below |
| `blockers: N` | the count, printed only when the reading is whole |

Exit codes: 0 when the reading is whole, whatever the blockers; 1 refused, naming what: the row not
`released` (`status runs while the hold row is released, and it is <phase> (hold "H")`), or a
client it could not open, such as the broker's; 2 a usage error, or UNKNOWN. A failed read of the
row, the journal, the cutover rows, the hub or the consumer names stops it there. A store no flag
names, a store read that failed and a malformed value do not: it prints every family it read and
every `BLOCKER` line, then exits 2 with no `blockers:` line.

A daemon store whose database answers SQLSTATE 3D000 (the database does not exist) is read again
by the address rule ([The daemon stores' addresses](#the-daemon-stores-addresses)), with the
addresses read through the hold's Kubernetes flags. When the server at the address the store's one
source names answers 3D000, `status` prints the store `gone` and goes on, so a broker cutover row
that names a deleted project does not keep every reading UNKNOWN; no command removes that row.
Any other answer the rule finds (an address no one source names, that is not that source's, or
that another source names too, a server that answers otherwise) keeps the store UNKNOWN, naming
why: `daemon/P's store answers SQLSTATE 3D000, and the census's rule does not confirm it gone:
...`. A gone store's consumers stay on the stream until `cleanup --gone P` removes them. An UNKNOWN
reading's `BLOCKER` lines are each true of the store they name; what it does not show is the stores
it could not read.

### The `BLOCKER` lines

A `BLOCKER` line names what keeps a family from being converted and consistent. Some are the normal
state before a cutover, which the cutover's own commands clear. A purge marker reads as a delete
marker. What each means for `convert`:

| `BLOCKER` line | What it is | What `convert` does with it |
| --- | --- | --- |
| `F: its store is missing` | the bucket, or F's `intake_consumers` table, does not exist | `session`: `create-bucket` makes the bucket inside the cutover hold; without it `convert` stops. `dispatch` or `daemon/P`: `convert` stops when F is one of its families, until the database's migration makes the table (Dispatch's `20261121002206_intake_consumers`, the daemon's `20261121001600_intake_consumers`) |
| `F: its store has no sentinel` | the store holds no sentinel | `session`: `convert` stops. `create-bucket` writes the sentinel into a bucket that holds no key but hold barrier keys, this hold's or another's; a bucket that holds records and no sentinel is UNKNOWN to it, and `repair-sentinel` stops on a missing sentinel. `dispatch` or `daemon/P`: no stop; `convert` writes the sentinel |
| `session K: its record's last write is a delete marker` | K's record is lost | when K's fixed name is in its set, `convert` gives K no start and stops once every other name is converted, naming `replay --key K`. Run `replay --key K --from SEQ` in the cutover hold before `convert`, and it does not stop on K. The cutover's census is UNKNOWN on any deleted record and removes nothing |
| `F K: its current NAME is not on ENVOY_NOTIFICATIONS` | the record selects a consumer the stream does not hold | no stop. `replay --key K` clears it: the record then selects no current |
| `F K: an open KIND (candidate C, old O, owner W, reserved T)` | a creator, `convert` or `replace` left an operation in flight | `convert` settles it first, for a name of its set, and goes on. An open replace whose candidate and old name are both gone, and whose configuration `convert`'s saved set does not hold, is UNKNOWN |
| `F K: its family's creator refuses the record: WHY` | the record names a consumer outside K's family: its current, a retired name or an open operation's candidate is no `<prefix>~<nuid>` name of K, or its old name is neither such a name nor K's fixed name (`selection.CheckFamily`) | not one of `convert`'s checks; the census and `cleanup` refuse such a record |
| `F: the fixed name NAME is on ENVOY_NOTIFICATIONS` | a consumer by its fixed name | what `convert` converts. Before a first cutover there is one for each logical name. When `dispatch` is one of its families, `convert` converts both of Dispatch's durables: a `delivery-events` or `delivery-jobs` line missing here means that durable is not on the stream, and if `prepare` does not list it either, `convert` gives it no start, and stops late, unless Dispatch's hold token or its record holds one. The hold token is the `duty_holds` row `delivery-intake-<durable>` of Dispatch's database, whose token ends ` start=<seq>` when it holds one. `prepare` prints only a count, so run `replay --key <durable> --from SEQ` in the cutover hold before `convert` for each durable with no line here: `convert` takes the lowest of every start it knows, so the replay's start never raises another, and it does not stop on that durable |

SEQ, for `replay`, is the first stream sequence the name's fresh consumer is to deliver: a lower one
delivers messages again, a higher one skips them. `replay` of a Dispatch or daemon key needs that
store's flag. No sentinel that decodes as a store state stops `convert`: it writes each family's
marker `done` over it, keeping the other fields it reads. A sentinel that does not decode is
UNKNOWN at its store check; after a cutover, [a sentinel repair](#a-sentinel-repair) writes the
session one again. `convert` and `status` read each record with the selection library's decode
(`selection.DecodeKVValue`): a record that does not decode is UNKNOWN when `convert` reaches it,
after it has saved its set. `replay --key K` writes K's record anew over any value under K, one
that does not decode included, retiring none of that value's names and naming it (`replaced a
value that is no record of it: ...`); over a record that decodes it also clears an open operation,
retiring its names but K's fixed name. So give each record `status` marks malformed a `replay --key
K --from SEQ` before `convert`, or after `convert` stops on it, then run `convert` again. The
cutover rows do not stop `convert`: it writes each of its families' rows `done`.

`status`, the census's reader and `cleanup` check each record as its family's creator does, and
list or refuse one the creator refuses. A record `restore --finish` left selects the key's fixed
name as its current, which a creator in the fresh mode refuses; these readers take that shape
alone, its current equal to its restore target, the key's fixed name, at phase `created`, and check
its other names as any record's. So after a rollback `cleanup` deletes the fresh names the rollback
retired and keeps the fixed name.

Before a first cutover, with one session, one listener and one daemon `P`, `status` reads, for
example:

```text
row: gen 7, phase released, hold "<the last hold>"
store session: missing
store dispatch (intake_consumers): no sentinel
store daemon/P (intake_consumers): no sentinel
BLOCKER session: its store is missing
BLOCKER dispatch: its store has no sentinel
BLOCKER daemon/P: its store has no sentinel
BLOCKER dispatch: the fixed name delivery-events is on ENVOY_NOTIFICATIONS
BLOCKER dispatch: the fixed name delivery-jobs is on ENVOY_NOTIFICATIONS
BLOCKER daemon/P: the fixed name legion-go-P-wake is on ENVOY_NOTIFICATIONS
BLOCKER session: the fixed name listener-host-a is on ENVOY_NOTIFICATIONS
BLOCKER session: the fixed name s-<session id> is on ENVOY_NOTIFICATIONS
blockers: 8
```

Inside the cutover hold each of these is cleared before `convert` needs it: `create-bucket` makes
the session store, and `convert` writes the Dispatch and daemon sentinels and converts each fixed
name. A reading after the cutover's hold shows, for each family `convert` named, its `store` line
at `cutover=done` and its `cutover` row `done`; `convert` ended on a listing that held no fixed
name.

## What blocks `convert`

`convert` refuses or stops (exit 1), or is UNKNOWN (exit 2), before it changes anything, at each
of these, in this order. The first four leave the journal as it was. `status` shows the stores of
step 5, and none of its other checks; steps 2 to 4 show only under the hold, in `envoy-infra hold
status --hold ID` and in each command's refusal.

1. A usage error (exit 2): `--hold`, `--signing-key-secret`, `--broker-url`, `--token-file`,
   `--verify-keys` or `--nats` missing; a flag that is not `convert`'s (`--key`, `--from`,
   `--final`, `--replicas` and the others are refused by name); a malformed `--daemon-database`.
2. The hold row (exit 1): `convert runs while the hold row is held, and it is <phase> (hold "H")`,
   or `the hold row is held under hold "H", not under --hold "ID"`.
3. The held re-read ([The held commands](#the-held-commands)): a refusal exits 1, an UNKNOWN 2. A
   client still connected besides the operator and the auth service refuses it; a declared server
   that `hold set` did not restart in this hold is UNKNOWN.
4. The admission (exit 1), as `holdrecord: convert in <workflow>: <why>`: a hold of any workflow
   but `cutover` (`the workflow does not admit this command`); `hold set` not done (`a required
   command has no done entry: hold set`); another command's intent pending (`another command's
   intent is pending: <command>`); `convert` done already, or the census done (`the command is
   already done`); the hold ended by `hold release` or `hold abort`.
5. After its intent, its own checks:
   - no cohort record: a stop, recorded `unchanged`: `hold ID has no cohort record: consumer-names
     prepare --hold ID did not run before hold set`. No rerun can write the record in this hold:
     end the hold (`hold release`, or `hold abort` when another command has changed something) and
     start again from `hold prepare`;
   - the hub's reach, the declared servers, the meta barrier and its listing, and the info of each
     fixed name it lists: a refusal or an UNKNOWN, each a stop (below);
   - the stores of its families: the `session` family always; `dispatch` when
     `--dispatch-database-file` is given or its set holds `delivery-events` or `delivery-jobs`;
     each daemon whose `--daemon-database` is given or whose fixed names its set holds. Its set is
     every fixed name the listing holds, every member of the cohort record the listing no longer
     holds, and both of Dispatch's durables when `dispatch` is a family. It reads each daemon store
     a `--daemon-database` file names first by the address rule, and writes it through the
     connection the rule opened there. A stop, recorded `unchanged`, names the stores that cannot
     hold the conversion:
     - `daemon/P's store is unreconciled, so convert writes none of its records: <why>`, for each
       store the address rule leaves unreconciled (no table among them), and `daemon/P's store is
       gone at the address its Deployment names (...): remove its consumers with cleanup --gone P
       in a consumer-cleanup hold, and give convert no --daemon-database for it`, for each it finds
       gone;
     - `the selection bucket envoy_session_consumers cannot hold the session family's records
       (...): consumer-names create-bucket --hold ID comes before convert`, for a missing bucket
       or a bucket with no sentinel;
     - `Dispatch has fixed names to convert and its store cannot hold them (...): give
       --dispatch-database-file <file>`, for no flag or a file it cannot read;
     - `Dispatch has fixed names to convert and its store, which --dispatch-database-file names,
       has no intake_consumers table (...): Dispatch's boot makes the table with its migration
       20261121002206_intake_consumers, so deploy the Dispatch release that carries it, then run
       convert again`, for a missing table when the flag is given;
     - `the Legion daemon of project P has fixed names to convert and its store cannot hold them
       (...): give --daemon-database P=<file>, and boot the project once so its migration makes
       intake_consumers`, for a daemon no flag names.

     Any other failed read of a store is UNKNOWN, a stop recorded `unknown: ...` (below): a
     Postgres sentinel that does not decode. So before `hold prepare`, remove a gone daemon's
     consumers with `cleanup --gone` in a `consumer-cleanup` hold, and give `convert` no flag for
     it: with no flag and no name of it on the stream, it is none of `convert`'s families. When the
     address rule leaves that store unreconciled (no single source names the project with both its
     database and its instance), `cleanup --gone` removes nothing, and no command of this package
     removes those consumers.

Then `convert` saves its set, the hold's record of kind `convert` and key `set`: its first change.
It runs the held re-read again before its first change to the hub. After that, a stop records the
set and what `convert` made of it, so `hold release` is refused, and the way out is `convert`'s
rerun or `hold abort`. Two exits come only after it has converted names:

- a logical name with no safe start, a stop once every other name is converted: `no safe start for
  K (<why>); ...: give each one with consumer-names replay --key <K> --from <seq> --hold ID, then
  run convert --hold ID again`. A name has none when its record's last write is a delete marker,
  or its fixed name is gone and neither a cohort floor, Dispatch's hold token nor a start its
  record saved gives one;
- a listing that still holds a fixed name after 8 listings, each converting what it lists: UNKNOWN.

Every exit after `convert`'s intent ends in its stop entry, an UNKNOWN's too, so no intent is left
pending for its rerun alone. A refusal's stop records what every run of `convert` in the hold left,
as above. An UNKNOWN's stop records `unknown: read <what the run read>; unread <what it could not
read>; <what the run changed>`, never `unchanged`: a reading that is not whole cannot say what it
left there. `hold release` is refused over it, and the way out is `convert`'s rerun, which goes on
from the saved set if one was saved and is done once what made it UNKNOWN is gone, or `hold
abort`, whose intent carries the stop's record. Among them: a session sentinel that does not
decode, the open-replace case, a record `convert` cannot decode, the 8 listings, and a done entry
whose write fails, which may have landed.

## The held commands

Every command but `status`, `backup` and `prepare` is held. It reads the hold row, which must be
`held` under `--hold`, then runs the held re-read, then writes its intent in the hold's journal,
the decision and the entry one write, and ends with its done entry or a stop entry: every exit
after its intent writes its stop, a refusal's, an UNKNOWN's and a failed done entry's alike, so no
command leaves its intent pending but a run cut short (a kill, or a broker it can no longer reach).
`convert`, `cleanup`, `cleanup --gone`, `enroll` and `create-bucket --after-loss` run the re-read
again before their first change to the hub or a store.

The held re-read reads again what `hold set` read, on the command's own connection; a refusal or an
UNKNOWN of any step stops the command before it writes anything, or, for the re-read again after
the intent, ends it in its stop:

- the hold's journal;
- the render's precondition, which names the auth service's user, and every declared server;
- each declared pod must be one `hold set` restarted in this hold, by the `server_id.<pod>` its
  done entry records: UNKNOWN otherwise;
- every protected stream by name, absent only where the workflow allows it (the selection bucket in
  `cutover` and `selection-recovery`);
- the writer census over the protected streams present, read again up to 10 times, 3 s apart,
  while it is not whole;
- the report: every connection of every declared server must be the command's own or the auth
  service's; it reads the servers' routes, leafnodes and gateways too.

Each read of the selection bucket that a held command acts on comes after the hold's bucket
barrier: the put of `hold.barrier.<hold id>`, with the stream's leader read before it and after
the read. `convert`'s store check, which only asks whether the bucket and its sentinel are there,
reads the sentinel directly.

The journal's rules, from the hold's table (`internal/holdrecord`):

- Every command needs a done `hold set` in its hold, and only one command is past its intent at a
  time: another command's pending intent refuses every other command, `hold release` and `hold
  abort` included.
- A command not marked repeatable below is refused once it has a done entry; a run that stopped or
  crashed is admitted again.
- A stop entry records what every run of its command in this hold left outside the journal, or
  `unchanged` when none changed anything; an earlier run cut after its intent counts as a change,
  whatever the stopped run read. A run that put this hold's barrier key in the bucket
  counts the key as its change, unless the run then deleted the bucket: so a stop after the run
  put the key is not `unchanged`, and one before it can be. `convert` puts it only after it has
  saved its set (its store check reads the bucket's sentinel directly), and `cleanup --gone` and
  `enroll` never put it.
- The stop of an UNKNOWN exit records `unknown: read <what the run read since its intent>; unread
  <what it could not read>; <what the run changed>`, and how each earlier run of the command in the
  hold ended; never `unchanged`, since a reading that is not whole cannot say what it left. A done
  entry whose write fails is UNKNOWN too (`whether <command>'s done entry was written`): it may
  have landed, and then the stop's write is refused, naming it.
- `hold release` ends a hold whose record is complete, or one with no done entry but `hold set`'s,
  no stop that recorded anything but `unchanged`, and a stop as each command's latest entry.
  Otherwise the way out is the stopped command's rerun, or `hold abort`, which every one of these
  workflows admits once a command's latest entry is a stop and no intent is pending.
- A final command's done entry completes the record. Any other command's intent or done entry makes
  it incomplete again, so the last run of a repeatable command after completion carries `--final`.
- After completion the table admits only what can be followed by the final command again:
  `selection-recovery`'s `replay` and `replay --final`, and each `consumer-cleanup` command.

| Command | Hold row | Workflow | Journal name | Table |
| --- | --- | --- | --- | --- |
| `status` | `released` | none | none | reads only |
| `backup` | `released` | none | none | writes only `--out DIR` |
| `prepare` | `prepare` under `--hold` | the cutover's, before `hold set` | none | rerun merges |
| `create-bucket` | `held` | `cutover` | `create-bucket` | once |
| `replay` | `held` | `cutover`; `selection-recovery` after `create-bucket --after-loss` | `replay` | repeatable |
| `convert` | `held` | `cutover` | `convert` | once |
| `envoy-infra streams census` | `held` | `cutover`, after `convert` | `streams census` | final |
| `create-bucket --after-loss` | `held` | `selection-recovery` | `create-bucket --after-loss` | final |
| `replay --final` | `held` | `selection-recovery`, after `create-bucket --after-loss` | `replay --final` | final, repeatable |
| `restore --prepare` | `held` | `rollback` | `restore --prepare` | once |
| `restore --key` | `held` | `rollback`, after `restore --prepare` | `restore --key` | repeatable |
| `restore --finish` | `held` | `rollback`, after `restore --prepare` | `restore --finish` | final |
| `cleanup` | `held` | `consumer-cleanup` | `consumer-names cleanup` | repeatable; with `--final`, final |
| `cleanup --gone` | `held` | `consumer-cleanup` | `consumer-names cleanup --gone` | repeatable; with `--final`, final |
| `replace` | `held` | `consumer-cleanup` | `consumer-names replace` | repeatable; with `--final`, final |
| `enroll` | `held` | `enroll` | `consumer-names enroll` | final |
| `repair-sentinel` | `held` | `sentinel-repair` | `consumer-names repair-sentinel` | final |

`prepare` checks only the row's phase and hold id, not the workflow. A `consumer-cleanup` command
run with `--final` is named in the journal with `--final` added, as `consumer-names replace --final`.

## The commands

### `prepare`

`prepare --hold ID <hold flags>`, under the `prepare` row of the cutover's hold, while every client
still runs. It reads `ENVOY_NOTIFICATIONS`' first sequence as the cutover boundary B (at least 1),
lists every fixed name, raises the inactive threshold of each that has one shorter than 7 days to
7 days through a consumer update, so no fixed name is removed for inactivity before `convert` reads
it, reads each one's ack floor, and writes the hold's cohort record: B, the stream's identity, and
for each fixed name its family, logical name, floor and the threshold it had. It writes no journal
entry. A rerun merges with the record an earlier run wrote: the lower B, every member of either,
the lower floor of each, and the threshold the first run read. It prints
`hold ID: cohort of N fixed names (M thresholds raised to 168h0m0s), boundary B`. `hold abort` of a
hold still in `prepare` deletes the cohort record.

### `create-bucket`

`create-bucket --hold ID [--replicas N] <hold flags>`, in a `cutover` hold. It refuses before its
intent when the hold has no cohort record (`consumer-names prepare writes it, with the boundary B
the bucket's sentinel holds`). Its copies are `--replicas` (1 to 5), else `ENVOY_NATS_REPLICAS`,
else 1. With the bucket missing, it creates `envoy_session_consumers` (one value a key, kept for
good, on file storage) and writes its sentinel with B and the hold's id, and prints `created
envoy_session_consumers at N copies, its sentinel with boundary B`. A bucket it finds present, on
its first run or a rerun, it adopts only when the bucket's stream has the configuration
`create-bucket` sends and the bucket holds no key but hold barrier keys, this hold's or another's
(it writes the sentinel: `adopted envoy_session_consumers, which held no key: ...`), or only its
sentinel with this hold's B (`adopted ..., whose sentinel already holds this hold's boundary B`).
Any other bucket is UNKNOWN (`selection bucket envoy_session_consumers exists and is not this
hold's`, or a line naming each field of its configuration that differs), and `create-bucket` never
deletes it. So run it when `status` printed `store session: missing`, or `store session (...): no
sentinel` with no `record session` line. Every hold's barrier key, `hold.barrier.<hold id>`, is no
key of the bucket's to `create-bucket` and `create-bucket --after-loss`: a bucket with no sentinel
that holds only another hold's barrier key, as a run of that hold leaves it when it stops after its
barrier and before the sentinel, is the empty bucket each takes. A bucket whose sentinel `status`
prints, such as the one a rollback leaves, needs no `create-bucket`: `convert` writes its own B and
hold into that sentinel.
A bucket that holds records and no sentinel is UNKNOWN to `create-bucket`, `repair-sentinel` stops
on it, and `create-bucket --after-loss` takes it only when each of its keys holds a walk's value
(`--backup`): it adopts a bucket holding all of the walk's records, and deletes and loads again one
holding some of them.

### `backup`

`backup --out DIR <hold flags>`, with the row `released`, outside any hold. It refuses a `DIR` that
exists, empty or not, before it reads the row. It reads the bucket's stream by sequence with the
released grant's two reads, writes the copy into a directory of its own beside `DIR` and moves it
to `DIR` only once every file is written: `manifest.json`, `records.jsonl` and `SHA256SUMS`, in
format `envoy-kv-records-v1`. It prints `envoy_session_consumers: the walk of
KV_envoy_session_consumers, N records through sequence S, written to DIR; backup sha256 H`, where H
is the sha256 of `SHA256SUMS`, which a recovery from `DIR` records. It changes nothing on the hub.

### `create-bucket --after-loss`

`create-bucket --hold ID --after-loss [--replay-from SEQ] [--backup DIR] [--replicas N] <hold
flags>`, in a `selection-recovery` hold, after the bucket was lost. `--backup DIR` is a copy
`backup` wrote, or a jsm.go snapshot of `KV_envoy_session_consumers` (`backup.json` and its
archive); it is read and hashed before the intent. `--replay-from` defaults to the notification
stream's first sequence (at least 1): while the sentinel's replay mode lasts, a key with no record
starts there.

- Before any restore it writes the hold's selection record: `replay_from`, the backup's sha256
  (`none` without one), its format and the hold. A rerun takes its inputs from that record and
  refuses a `--replay-from` or `--backup` that names others; a rerun of a recovery that names a
  backup needs that `--backup` again while it still has to load it.
- With the bucket absent it restores a snapshot (a cut restore's leftover is deleted and the
  restore run once more; a second cut stops it, leaving no partial stream), or creates the bucket
  and loads a walk's records, or creates it empty. Each value it loads is checked to be exactly a
  value of its key's kind; a restored snapshot that fails is deleted again and the command stops.
- A bucket present is adopted only when it is this recovery's: its sentinel names these inputs; it
  holds all of the walk's records and no sentinel; it holds the snapshot's archive and since then
  only this hold's barrier; it is empty with no backup; or it holds another recovery's sentinel,
  whose lower `replay_from` is kept (a run that would raise it stops). The leftover of this
  recovery's own cut load or restore, a bucket with no sentinel that holds some of the walk's
  records, or nothing after a snapshot of some, is deleted and made again. A hold's barrier key,
  this hold's or another's, is none of the bucket's keys in these rules but the snapshot's. Anything
  else stops it: `selection bucket exists and is not this recovery's`.
- It writes the sentinel in replay mode: `cutover` the broker's `session` cutover row's state (else
  the one the bucket's or the walk's sentinel held), the boundary that sentinel held, `replay_from`,
  the orphans (each `session~` consumer no record selects, listed after the load), the backup's
  sha256 and the hold. Over another recovery's sentinel it keeps that sentinel's fields, with the
  lower `replay_from` and the orphans of both. Replay mode ends when the listener's sweep finds
  none of the orphans on the stream: it then clears `replay_from` and the orphans.
- Its postcheck reads every protected stream against the reading before its first change; a write
  in the bucket it did not make stops it.

It prints `envoy_session_consumers: <outcome>; replay from N, backup H; postcheck passed`. Its done
entry completes the hold's record. A stop names what the run left.

### `replay`

`replay --hold ID --key K --from SEQ [--final] <hold flags> [<store flags>]` writes K's record with
start SEQ and no current, keeping its retired names and retiring its current and any open
operation's candidate and old name, but K's fixed name, which no record retires. A value under K
that is no record of K (a document a reply subject stored there, garbage, or another key's record
copied there) it writes over, retiring nothing of it. In a `cutover` hold it gives a name a start
before `convert`, or one `convert` found no safe start for or stopped on as malformed; in a
`selection-recovery` hold it gives a logical name a start other than the recovery's, and `--final`
completes the record again. K is a session id, a listener durable's key (`durable.listener-<machine
id>`, escaped as the bucket keys it), one of Dispatch's durables, or a daemon's
`legion-go-<project key>-<kind>`; a bare `listener-<machine id>` is refused, naming its key. It
refuses before its intent a SEQ past the stream's last sequence + 1, a session or listener key
while the bucket is missing (`create-bucket makes first`), a Postgres record it cannot read for
another reason than that it is no record of K, and a daemon's key whose store, named by
`--daemon-database`, the address rule leaves unreconciled or finds gone; it writes a daemon's store
through the connection the rule opened. It prints `<family>: K's record starts at SEQ with no
current; retired: ...`, followed by `; replaced a value that is no record of it: <why>` when it
wrote over one. A stop names what the run left.

### `convert`

`convert --hold ID <hold flags> <store flags>`, in a `cutover` hold. [What blocks
`convert`](#what-blocks-convert) lists its refusals. It saves its set, then converts each name of
it. A record with an open operation is settled first; an open replace of the fixed name whose fresh
name never landed has the fixed name deleted before a fresh name is created at its saved start, so
no logical name holds two consumer slots. A record that already selects a fresh name is left as it
is, its fixed name deleted (never retired) if the stream still holds it. A name whose fixed name is
present is reserved with start S, the lowest of the fixed name's ack floor + 1, the start
Dispatch's hold token saved (`duty_holds` row `delivery-intake-<durable>`) and any start the record
holds, clamped as a creator clamps a start past the stream's end; the reserved record names the
fixed name only as its open replace's old name, with no current. Then the fixed name is deleted
and the fresh name created at S with the fixed name's configuration (the inactive threshold
`prepare` read, not the raised one) and published as the record's current. A failed delete of the
fixed name stops `convert`, the fixed name retired nowhere. A name whose
fixed name is gone gets a record with no current, starting at the lowest of the cohort's floor + 1,
the hold token's start and the record's start. It lists again until a listing holds no fixed name,
then writes each family's broker cutover row `done` and its store's marker `done`: the bucket's
with this cutover's B and hold, each daemon's with the stream's last sequence + 1 and the stream's
identity. It prints `hold ID: converted N of M fixed names; the final listing holds no fixed name;
cutover done for F, ...`. A rerun settles what a cut run left and goes on from the saved set.

### `envoy-infra streams census`, in the cutover

The census is the cutover's final command, admitted once `convert` is done; its done entry
completes the hold. It runs only once every `--family` it is given has a broker cutover row `done`.
Its own flags and its removals are in [`envoy-infra streams`](../README.md#envoy-infra-streams);
this package gives it three readers:

- the session consumers' records: each name a session's or a listener durable's record holds (its
  current, its open operation's candidate, its retired names), which the census keeps. A record
  whose last write is a delete marker, a durable key of no listener, a malformed value, or a walk
  that is not whole makes the census UNKNOWN, and it removes nothing;
- the fresh names the listener, Dispatch and daemon records hold: a `<base>~<nuid>` no record holds
  is a crashed create's orphan, which the census removes and names. `--family dispatch` needs
  `--dispatch-database-file`; each `--family daemon/P` needs `--daemon-database P=F`, read by the
  address rule. A store it cannot read, or a daemon store left unreconciled, makes the census
  UNKNOWN, and it removes nothing. A daemon store gone has its consumers left, each printed `LEFT`;
- each daemon store's stream identity, written to `ENVOY_NOTIFICATIONS`' identity where it differs,
  with no write while any store is unreconciled. It prints `CHANGED daemon/P: stream_id "old" ->
  "new"`, `GONE daemon/P: database D on instance I (the operator's file states I): <answer>`, and
  `RECONCILED daemon/P: stream_id "id" unchanged` (or `no stream_id recorded,`). A store left
  unreconciled makes the census UNKNOWN, with no done entry.

Exit codes: 0 clean, 1 a failure or a refusal, 2 UNKNOWN or a usage error.

### `restore --prepare`, `restore --key`, `restore --finish`

The rollback, in a `rollback` hold. Its families are `session`, `dispatch` when
`--dispatch-database-file` is given, each `--daemon-database`, and every family a broker cutover
row names, whose store flag must then be given. Each command reads each daemon store a
`--daemon-database` file names first by the address rule, and writes it through the connection
the rule opened. A store left unreconciled is UNKNOWN, and nothing is written. A store gone at its
address does not block the rollback: its family is left out, nothing is written there and its
cutover row stays as it is (`cleanup --gone` removes its consumers); each command prints
`restore: daemon/P: store gone: <why>; restore writes nothing there and leaves its cutover row as
it is`, and its done entry names it (`gone.daemon/P`).

- `restore --prepare --hold ID <hold flags> <store flags>` stops when the broker's `session`
  cutover row reads anything but `done`; with no `session` row it rolls back a convert that never
  finished. It saves its set (the hold's record of kind `convert`, key `restore`) before any
  change: for each logical name its target, the fixed name; its start S (an open operation's saved
  start; else, its current present, the current's ack floor + 1; else its last floor + 1 when read
  from that current on this stream; else its record's start; else none); and the target's
  configuration (the target's own, else its current's, candidate's or old name's, else the one a
  cutover's `convert` saved), and the cutover's hold. Then for each name it writes the record's
  restore field at phase `planned` (`no_start` with no start), keeps the target when its ack
  floor + 1 is at most S and deletes it otherwise, deletes and retires its current, candidate and
  old name, and writes phase `kept` or `source_deleted`. Its done entry records each declared
  server's id. It prints `restore --prepare: N names: K kept, S source_deleted, U with no start`,
  and for each name with none `  F/L has no start: give it one with restore --key <K> --from <seq>
  --hold ID`.
- `restore --key K --from SEQ --hold ID <hold flags> <store flags>` gives a name `--prepare` saved
  with no start its start, then settles it as `--prepare` does. K is a listener durable's key, any
  family's fixed name, or a session id. It refuses a name `--prepare` did not save, and one whose
  record holds another start already. It prints `restore --key: F/L restores T at SEQ (PHASE)`.
- `restore --finish --hold ID --probe-token-file P <hold flags> <store flags>` stops, restarting
  nothing, while any name has no settled start. Then it restarts, proves and barriers every
  declared server again, and requires each server's id to differ from the one `--prepare`
  recorded. It adopts each target present that passes its floor check or sits at its start, deletes
  one that does neither (and restarts the servers once more before it creates anything), refuses a
  target it created earlier that now has another start, creates each absent target at its start
  with its saved configuration (a listener's push durable with a new deliver inbox), and writes every
  family's broker cutover row and store marker `rolled_back`. It prints `  F/L: T created at S` for
  each create and `restore --finish: N names restored, C targets created at their starts; every
  family reads rolled_back`. Its done entry completes the hold.

Each stop names each record it left at each phase, each consumer it deleted, each target it
created and each marker it set; a rerun goes on from the phase each record holds.

### `cleanup`

`cleanup --hold ID [--final] <hold flags> <store flags>`, in a `consumer-cleanup` hold. It reads
every store before it deletes anything: the bucket, Dispatch's table, and each daemon's that a
broker cutover row or `--daemon-database` names. So it needs a `--daemon-database` file for every
daemon row the cutover records, a retired or dropped project's too: a store no flag names stops
it, naming each such row, recorded `unchanged`. It reads each daemon's store by the address rule:
a store gone at its address holds no record to clean, and it prints `cleanup daemon/P: store gone:
<why>; it holds no record to clean` and goes on, its done entry naming it (`gone.daemon/P`); a
store the rule leaves unreconciled is UNKNOWN. A malformed value in the bucket is UNKNOWN too, and
it deletes nothing, its stop recording `unknown: ...` and naming the barrier key it put. It
deletes each retired name the server holds and the record does not still select (its current, its
open operation's candidate or old name), and drops from the record each retired name the server
reports absent. It deletes no name by a pattern. It prints `cleanup F K: deleted [...], dropped
from retired [...]` for each record it changed, then `cleanup: N records with retired names, D
names deleted, X dropped from retired`. A delete or a write that failed stops it, recording what
it deleted.

### `cleanup --gone`

`cleanup --gone P --daemon-database P=F --hold ID [--final] <hold flags>`, in a `consumer-cleanup`
hold, removes the consumers of daemon P whose store is gone, which the census leaves. It reads the
daemon stores' addresses again and reads P's store by the address rule: only SQLSTATE 3D000 at
exactly the address P's one source names, its Deployment or its retired entry, makes it gone. A
store that answers, one left unreconciled, and a listing it cannot read each stop it, removing
nothing. Then it deletes every consumer of `ENVOY_NOTIFICATIONS` whose logical name is one of P's,
`legion-go-P-<kind>`, fixed or fresh. It prints `cleanup --gone daemon/P: removed
ENVOY_NOTIFICATIONS consumer NAME: store gone: <why>` for each, then `cleanup --gone daemon/P: N
consumers removed; store gone: <why>`.

### `replace`

`replace --listener MACHINE-ID --hold ID [--final] <hold flags>`, in a `consumer-cleanup` hold,
gives one listener's durable a fresh name at one past its ack floor. It refuses before its intent a
machine id `core.CheckMachineID` refuses, and one whose durable's fresh name would pass NATS's
255-byte limit. It stops when the bucket is missing, when its sentinel does not read `cutover`
`done` (the durable is then its fixed name, which `replace` does not take), when the record selects
no current, and when the stream does not hold the current. Otherwise it settles an open operation
on the record, reserves the replace at the current's ack floor + 1, creates the candidate from the
current's configuration with no idle heartbeat, no flow control, explicit acks and a new deliver
inbox, publishes it, retires and deletes the old name, and deletes each retired name the server
still holds. It prints `replace: <durable> (record <key>): OLD replaced by NEW at stream sequence
S; OLD retired; deleted N retired name(s) the server held`. With the fixed names, recreate the
durable as [the deployment's README](../../deploy/README.md#recreating-a-listeners-durable) says.

### `enroll`

`enroll --daemon P --daemon-database P=F --hold ID <hold flags>`, in an `enroll` hold, marks a
daemon store made after the cutover converted. Before its intent it refuses a `--daemon` other than
the one an earlier run of `enroll` in this hold named, naming both, since the journal admits a
rerun by its command alone: rerun the first, then end the hold and enroll the other in a hold of
its own. It reads P's store first by the address rule, and marks that store alone, through the
connection the rule opened at that address: a store the rule leaves unreconciled (among them a
source of P that names a database and instance another source names too, and a store with no
`intake_consumers` table) or finds gone stops it, naming why and the source, before it reads or
writes anything else. It stops, naming what it found, when:

- the stream lists a consumer of one of P's logical names, fixed or fresh;
- the store holds a checkpoint (`consumer_positions`) or a selection row, or lacks its
  `consumer_positions` table;
- the store's marker reads `done` or `rolled_back` from another hold, or the broker holds a cutover
  row for `daemon/P` from another hold;
- the stream carries no identity, or this hold's marker records another one.

Otherwise, after the held re-read again, it writes the store's marker `cutover` `done` with the hold,
the boundary (the stream's last sequence + 1) and the stream's identity, then the broker's cutover
row `daemon/P` `done`, and prints `enroll: daemon/P: the store's marker and the broker's cutover
record read done under hold ID; boundary B, stream identity S`. Its done entry completes the hold.
A rerun keeps what this hold wrote. A read or write whose outcome it cannot know is UNKNOWN: its
stop records `unknown: ...` and each write whose answer was lost, so `hold release` is refused and
the way out is its rerun or `hold abort`.

### `repair-sentinel`

`repair-sentinel --hold ID <hold flags>`, in a `sentinel-repair` hold, writes the bucket's sentinel
`envoy.bucket` again from the broker's records alone. Any client whose grant holds a JetStream API
request can store the server's answer under that key through a reply subject, and every session
consumer call then fails closed. Before its intent it reads what the records give: the broker's
`session` cutover row gives `cutover` (`done` or `rolled_back`); for `done` the hold that row names,
and for `rolled_back` the cutover hold the rollback's `restore --prepare` saved, gives `hold`; and
that hold's cohort record gives the boundary. No `session` row, or one in another state, refuses
it; a missing set or cohort record is UNKNOWN. After its intent, behind the bucket barrier, it reads
the key's last message: a missing key, a delete or purge marker, and a selection recovery's
sentinel (one with `replay_from`, orphans or a backup's sha256) each stop it, since those are
`create-bucket --after-loss`'s. A value equal to the records' is left as it is; any other, malformed
or not, is written over. It prints `repair-sentinel: envoy.bucket of KV_envoy_session_consumers
reads the broker's records: cutover C, boundary B, hold H (it held <what>)`.

## Procedures

Each procedure's commands run in this order. `hold set --workflow W` prints `prepared hold ID`;
`hold prepare` prints the hold's id alone.

### The cutover

```sh
envoy-infra consumer-names status <hold flags> <store flags>
id=$(envoy-infra hold prepare --workflow cutover --reason R --minters-report M <hold flags>)
envoy-infra consumer-names prepare --hold "$id" <hold flags>
envoy-infra hold set --hold "$id" --reason R --minters-report M --probe-token-file P <hold flags>
envoy-infra consumer-names create-bucket --hold "$id" <hold flags>    # only when the bucket is missing or empty: see create-bucket
envoy-infra consumer-names replay --hold "$id" --key K --from SEQ <hold flags> [<store flags>]   # each deleted record, each Dispatch durable with no fixed-name line, and each record status marks malformed or with an open operation (see The BLOCKER lines)
envoy-infra consumer-names convert --hold "$id" <hold flags> <store flags>
envoy-infra streams census --hold "$id" --reason R --family session --family dispatch \
  --family daemon/PROJECT-KEY <store flags> <the census's own flags> <hold flags>
envoy-infra hold release --hold "$id" --reason R <hold flags>
envoy-infra consumer-names status <hold flags> <store flags>
```

Give the census one `--family` for each family `convert`'s last line names. When `convert` stops
naming `no safe start for K`, run `consumer-names replay --hold "$id" --key K --from SEQ` for each,
then `convert` again. After any other stop, clear what it names and rerun the command, or end the
hold: `hold release --hold "$id" --reason R` while every stop recorded `unchanged` and no command
but `hold set` is done (a done `create-bucket` counts as a change), else `hold abort --hold "$id"
--reason R`.

### The rollback

```sh
envoy-infra consumer-names status <hold flags> <store flags>
envoy-infra hold set --workflow rollback --reason R --minters-report M --probe-token-file P <hold flags>
envoy-infra consumer-names restore --hold ID --prepare <hold flags> <store flags>
envoy-infra consumer-names restore --hold ID --key K --from SEQ <hold flags> <store flags>   # each name --prepare printed with no start
envoy-infra consumer-names restore --hold ID --finish --probe-token-file P <hold flags> <store flags>
envoy-infra hold release --hold ID --reason R <hold flags>
```

The bucket keeps the cutover's boundary and hold in its sentinel, so a later cutover needs no
`create-bucket`.

### A selection recovery

```sh
# Before any loss, with the row released, as often as wanted:
envoy-infra consumer-names backup --out DIR <hold flags>
# After the bucket is lost (status prints "store session: missing"), or holds records and no sentinel (see create-bucket):
envoy-infra hold set --workflow selection-recovery --reason R --minters-report M --probe-token-file P <hold flags>
envoy-infra consumer-names create-bucket --hold ID --after-loss [--backup DIR] [--replay-from SEQ] <hold flags>
envoy-infra consumer-names replay --hold ID --key K --from SEQ [--final] <hold flags> [<store flags>]   # only for a name that needs another start; the last with --final
envoy-infra hold release --hold ID --reason R <hold flags>
```

### A sentinel repair

When `status` prints `store session (KV_envoy_session_consumers): sentinel UNKNOWN, its value is
malformed`, or a `store session` line that disagrees with the broker's `cutover session` row: after
a cutover its `cutover` and `hold` are that row's state and hold, and after a rollback
`cutover=rolled_back` with the hold of the cutover it rolled back; `boundary` is that cutover's B.
A sentinel with `replay_from` above 0 is a selection recovery's, which `repair-sentinel` leaves to
`create-bucket --after-loss`. `repair-sentinel` needs the broker's `session` cutover row, so a
session sentinel that does not decode before any cutover has no repair in this package; nor has a
Postgres store's sentinel that does not decode, which `status` reports in its UNKNOWN line as `F's
sentinel: ...`.

```sh
envoy-infra hold set --workflow sentinel-repair --reason R --minters-report M --probe-token-file P <hold flags>
envoy-infra consumer-names repair-sentinel --hold ID <hold flags>
envoy-infra hold release --hold ID --reason R <hold flags>
envoy-infra consumer-names status <hold flags> <store flags>
```

### An enrolment

For a project whose daemon started after the cutover. Its Deployment carries the three annotations,
and the daemon has run once with `consumer_names: fresh`, so its migrations have made
`intake_consumers` and `consumer_positions`; in that mode it refuses every consumer call until the
marker reads `done`.

```sh
envoy-infra hold set --workflow enroll --reason R --minters-report M --probe-token-file P <hold flags>
envoy-infra consumer-names enroll --hold ID --daemon PROJECT-KEY --daemon-database PROJECT-KEY=F <hold flags>
envoy-infra hold release --hold ID --reason R <hold flags>
```

Every census after it names the daemon too: `--family daemon/PROJECT-KEY`, its consumers' shapes
(`--daemon-consumer`), and `--daemon-database PROJECT-KEY=F`, so its fresh names are read and
kept; a fresh name of a declared shape that no record the census read holds is removed.

### A consumer cleanup

```sh
envoy-infra hold set --workflow consumer-cleanup --reason R --minters-report M --probe-token-file P <hold flags>
envoy-infra consumer-names cleanup --hold ID <hold flags> <store flags>
envoy-infra consumer-names cleanup --hold ID --gone PROJECT-KEY --daemon-database PROJECT-KEY=F <hold flags>   # each daemon whose store is gone
envoy-infra consumer-names replace --hold ID --listener MACHINE-ID <hold flags>                                # each durable to replace
envoy-infra consumer-names cleanup --hold ID --final <hold flags> <store flags>
envoy-infra hold release --hold ID --reason R <hold flags>
```

Any of the three commands may run first and again; the last run carries `--final`.

## The creators' naming mode

Each creator reads its mode once, at its start: the listener and Dispatch from
`ENVOY_CONSUMER_NAMES`, a Legion daemon from `legion.yaml`'s `consumer_names`, `fixed` when unset.
In the fresh mode a creator refuses every consumer call until its store's marker reads `done`. In
the fixed mode a creator refuses to start once its store's marker reads `done`, and starts on a
marker that reads `rolled_back` or none. So once `convert` has marked a family `done`, its creators
start in the fresh mode, and once `restore --finish` has marked it `rolled_back`, in the fixed
mode.
