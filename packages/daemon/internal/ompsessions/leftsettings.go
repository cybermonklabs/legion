package ompsessions

// Kind is what the client does about one entry of LeftSettings: one of four.
type Kind string

const (
	// SetInOpening is an entry OpenRead and OpenWrite set, by SET LOCAL or by their last
	// statement, set_config, which is SET LOCAL's function form.
	SetInOpening Kind = "set in the opening message"
	// SetByBegin is an entry the openings' BEGIN clause sets.
	SetByBegin Kind = "set by the BEGIN clause"
	// NoLongerADependency is an entry a statement of the client once depended on, and none does now.
	NoLongerADependency Kind = "a dependency the code no longer has"
	// Exempt is an entry that cannot change a result, an error or the durability of a statement of
	// the client, or that acts only while no transaction of the client runs on the connection.
	Exempt Kind = "exempt"
)

// Form is how the openings set a SetInOpening entry.
type Form string

const (
	// SetLocal is a statement SET LOCAL <name> <value> of its own.
	SetLocal Form = "SET LOCAL"
	// SetConfig is a name in the IN list of the openings' last statement, SELECT
	// pg_catalog.set_config(s.name, s.reset_val, true) FROM pg_catalog.pg_settings s WHERE s.name
	// IN (…), which sets each parameter the server has as SET LOCAL … TO DEFAULT does, and sets
	// nothing for one it lacks.
	SetConfig Form = "set_config"
)

// Servers names the supported servers that have a parameter, where not every one does.
type Servers string

// The kinds of supported server that have a parameter not every supported server has: Cloud SQL for
// PostgreSQL 16 alone, or PostgreSQL 17 or 18 and every later release.
const (
	CloudSQLOnly         Servers = "Cloud SQL only"
	PostgreSQL17AndLater Servers = "PostgreSQL 17 and later"
	PostgreSQL18AndLater Servers = "PostgreSQL 18 and later"
)

// LeftSetting is one entry of LeftSettings.
type LeftSetting struct {
	// Name is the parameter's name as pg_settings spells it, "role" for the current role, or
	// "temporary table <table>" for a temporary table at the name of a store table.
	Name string
	// Kind is what the client does about the entry.
	Kind Kind
	// Form is how the openings set a SetInOpening entry, and "" for an entry of another kind.
	Form Form
	// ReadValue and WriteValue are the text after SET LOCAL <Name> in OpenRead and in OpenWrite:
	// "TO DEFAULT" for a SetConfig entry, whose statement sets the parameter's reset value, and ""
	// for an entry the openings do not set.
	ReadValue, WriteValue string
	// Reason is why the entry has its kind. An Exempt entry's starts with one of the two reasons
	// an entry may be exempt for, and says why it holds for the package's statements.
	Reason string
	// Left is the session-level statement that leaves the entry on a server connection.
	Left string
	// Servers names the supported servers that have the parameter, "" where every one does.
	Servers Servers
}

// The two reasons an entry may be Exempt for. Each exempt entry's Reason is one of them followed
// by why it holds for the package's own statements.
const (
	cannotChange = "It cannot change a result, an error or the durability of a statement of the client: "
	actsBetween  = "It acts only while no transaction of the client runs on the connection: "
)

// The reasons several entries share.
const (
	planReason = "It changes how the server plans a statement of the client, and so how long the statement runs " +
		"though not what it returns: a write holds its SELECT … FOR UPDATE row lock while its statements run, and a " +
		"slower plan holds it longer."
	memoryReason = "It changes how much memory a sort, a hash or an index build of a statement of the client may " +
		"use, or where what does not fit goes: a statement that spills writes temporary files, which temp_file_limit " +
		"can refuse, and runs longer while a write holds its SELECT … FOR UPDATE row lock."
	ioReason = "It changes how fast the server reads, writes or flushes the pages a statement of the client " +
		"touches, though not what it reads or writes: a write holds its SELECT … FOR UPDATE row lock while its " +
		"statements run, and a slower write holds it longer."
	undocumentedReason = "Google's Cloud SQL documentation (https://cloud.google.com/sql/docs/postgres/flags and " +
		"the pages it links) does not say what it does, so no exemption of it can be shown: the set_config " +
		"statement sets it back on a server that has it."
	noBackslash = "no string literal of a statement of the client holds a backslash, and the client binds every " +
		"other value it sends, so each literal reads the same under every value."
	noDates = "it acts only on the text of date, time and interval values, and no statement of the client sends " +
		"or reads one: a session's time is the bigint mtime_ms, which the client computes in Go."
	noLocaleFormatting = "it acts only on the to_char family of functions and on money values, and no statement " +
		"of the client uses either."
	passwordOnly  = "it acts only when CREATE ROLE or ALTER ROLE sets a password, and no statement of the client sets one."
	logicalOnly   = "it acts only in logical decoding and logical replication, and no statement of the client runs either."
	vacuumOnly    = "it acts only in VACUUM and ANALYZE, and no statement of the client runs either."
	noXML         = "it acts only on XML values, and no statement of the client sends or reads one."
	tcpKeepalives = "it acts only on the keepalive probes the server sends on an idle TCP connection, which the " +
		"kernel of a client that is there answers, so on such a connection it ends nothing (PostgreSQL 16 " +
		"documentation, https://www.postgresql.org/docs/16/runtime-config-connection.html)."
	beginIsolation = "The BEGIN of each opening names ISOLATION LEVEL READ COMMITTED, which takes the place of the " +
		"session's default isolation level."
	beginAccess = "The BEGIN of each opening names READ ONLY or READ WRITE, which takes the place of the " +
		"session's default access mode."
	beginDeferrable = "DEFERRABLE acts only on a SERIALIZABLE READ ONLY transaction, and the BEGIN of each opening " +
		"names ISOLATION LEVEL READ COMMITTED, under which it does nothing."
	temporaryReason = "Every statement of the client names each table with its schema, public.<table> or " +
		"pg_catalog.<catalog>, and pg_temp is last on the search_path the openings pin, so no statement reads or " +
		"writes a temporary table at a store table's name."
)

// temporaryTable is how an entry names a temporary table at a store table's name.
const temporaryTable = "temporary table "

// leftBy is the left statement SET <name> = <value>, which lasts for the session.
func leftBy(name, value string) string { return "SET " + name + " = " + value }

// setLocal is an entry both openings set by SET LOCAL <name> <value>, left by SET <name> = <left>.
func setLocal(name, value, reason, left string) LeftSetting {
	return LeftSetting{Name: name, Kind: SetInOpening, Form: SetLocal, ReadValue: value, WriteValue: value, Reason: reason, Left: leftBy(name, left)}
}

// toDefault is an entry both openings set by SET LOCAL <name> TO DEFAULT, which keeps a default of
// the login or the database.
func toDefault(name, reason, left string) LeftSetting {
	return setLocal(name, "TO DEFAULT", reason, left)
}

// setConfig is an entry some supported server lacks, which the openings' set_config statement sets
// to its reset value on a server that has it.
func setConfig(name, reason, left string, servers Servers) LeftSetting {
	return LeftSetting{Name: name, Kind: SetInOpening, Form: SetConfig, ReadValue: "TO DEFAULT", WriteValue: "TO DEFAULT",
		Reason: reason, Left: leftBy(name, left), Servers: servers}
}

// byBegin is an entry the openings' BEGIN clause sets, left by the statement left.
func byBegin(name, reason, left string) LeftSetting {
	return LeftSetting{Name: name, Kind: SetByBegin, Reason: reason, Left: left}
}

// noLonger is an entry no statement of the client depends on now, left by the statement left.
func noLonger(name, reason, left string) LeftSetting {
	return LeftSetting{Name: name, Kind: NoLongerADependency, Reason: reason, Left: left}
}

// exempt is an entry that cannot change a result, an error or the durability of a statement of the
// client, for the reason why.
func exempt(name, why, left string) LeftSetting {
	return LeftSetting{Name: name, Kind: Exempt, Reason: cannotChange + why, Left: leftBy(name, left)}
}

// exemptBetween is an entry that acts only while no transaction of the client runs, for the reason why.
func exemptBetween(name, why, left string) LeftSetting {
	return LeftSetting{Name: name, Kind: Exempt, Reason: actsBetween + why, Left: leftBy(name, left)}
}

// on is setting on the supported servers servers alone.
func (s LeftSetting) on(servers Servers) LeftSetting {
	s.Servers = servers
	return s
}

// temporary is the entry of a temporary table at the store table table's name.
func temporary(table string) LeftSetting {
	return noLonger(temporaryTable+table, temporaryReason, "CREATE TEMPORARY TABLE "+table+" (path text)")
}

// LeftSettings is the package's list of the class of settings another client can leave: each
// setting of context user that a login without superuser can leave on a server connection of a
// session database's pool, on PostgreSQL 16, on Cloud SQL for PostgreSQL 16 and on PostgreSQL 18,
// the current role, and a temporary table at the name of each of the five tables of Oh My Pi's
// session store. A client of a pool in transaction mode can send SET, SET ROLE or CREATE TEMPORARY
// TABLE and go, and the next client of the same server connection gets what it left. The list holds
// the names of Oh My Pi's own list for its store,
// packages/coding-agent/src/session/sql-session-postgres-settings.ts at
// v18.18.6-sami.20261122-091943, in its order: PostgreSQL 16's names, then the names Cloud SQL adds,
// then the names PostgreSQL 18 adds, then role and the temporary tables. The parameters are those of
// testdata/settings, the captures that build holds in
// packages/coding-agent/test/session/fixtures/postgres-settings, copied byte for byte. Each kind
// starts from Oh My Pi's list, and each reason is written for this package's own statements.
//
// Each entry is one of four kinds:
//
//   - SetInOpening: OpenRead and OpenWrite set it, by SET LOCAL with its ReadValue or WriteValue,
//     or, for a parameter some supported server lacks, by their last statement,
//     set_config(s.name, s.reset_val, true), which sets it as SET LOCAL … TO DEFAULT does where
//     the server has it and sets nothing where it does not. A SET LOCAL of a parameter the server
//     lacks would fail the whole opening. The value ends with the transaction, so the next client
//     of the connection does not get it, and TO DEFAULT keeps a default of the login or the
//     database.
//   - SetByBegin: the openings' BEGIN ISOLATION LEVEL READ COMMITTED READ ONLY (READ WRITE in a
//     write) sets it.
//   - NoLongerADependency: a statement of the client once depended on it, and none does now.
//   - Exempt: it cannot change a result, an error or the durability of a statement of the client,
//     or it acts only while no transaction of the client runs on the connection. Its Reason starts
//     with one of the two and says why it holds for the package's statements: notTables joins
//     pg_class and pg_namespace, Read runs UNION ALL … ORDER BY, FindByID runs right() and ORDER
//     BY … LIMIT 1, the writes run INSERT … ON CONFLICT, SELECT … FOR UPDATE, UPDATE and DELETE,
//     and CreateTables runs CREATE TABLE IF NOT EXISTS.
//
// A setting that changes how the server plans a statement of the client, how much memory it may
// use and where it spills what does not fit, or how fast it reads, writes, flushes or compresses
// pages, is set TO DEFAULT, never exempt: it returns the same rows, but a slower statement of a write
// holds its SELECT … FOR UPDATE row lock longer, and a sort or a hash that spills to temporary files
// can be refused by temp_file_limit or written to a tablespace a left value names. A planner setting
// that acts only on a shape no statement of the client has (a cursor, WITH RECURSIVE, DISTINCT, GROUP
// BY, a join of a table with itself) changes no plan of the client's, and is exempt.
//
// Each entry's Left is the session-level statement that TestTheClientHoldsUnderEveryLeftSetting
// sends to leave the entry on the server connection the client then gets: Oh My Pi's left value for
// the name where its list has one and the test needs no other (statement_timeout, lock_timeout and
// idle_in_transaction_session_timeout leave 50ms, which a held row lock or a pause outlasts;
// application_name leaves 'left' and search_path 'pg_catalog'), else a value a login without
// superuser may set that differs from the default.
var LeftSettings = []LeftSetting{
	setLocal("application_name", "= 'legion-session-client'", "It names each statement of the client in the "+
		"server's log and in pg_stat_activity, a refused one included; the name ends with the transaction.", "'left'"),
	toDefault("array_nulls", "It changes how the server reads the text of an array value: off, an unquoted NULL "+
		"in it reads as the string NULL.", "off"),
	toDefault("backend_flush_after", ioReason, "256"),
	exempt("backslash_quote", noBackslash, "off"),
	noLonger("bytea_output", "No statement of the client reads or writes a bytea value: it binds and reads each "+
		"session's content as text, and its other columns are text and bigint.", leftBy("bytea_output", "'escape'")),
	exempt("check_function_bodies", "it acts only on CREATE FUNCTION and CREATE PROCEDURE, and no statement of the "+
		"client creates a function or a procedure.", "off"),
	exempt("client_connection_check_interval", "it only checks, while a statement runs, that the client's socket is "+
		"still connected, and ends the statement only where it is not, so on a connection whose client is there it "+
		"ends nothing (PostgreSQL 16 documentation, https://www.postgresql.org/docs/16/runtime-config-connection.html).",
		"'10ms'"),
	setLocal("client_encoding", "= 'UTF8'", "pgx sends and reads text as UTF-8: under another encoding the server "+
		"converts the text it reads and writes, and refuses a character that encoding lacks, so a session's content "+
		"would not be kept byte for byte.", "'LATIN1'"),
	toDefault("client_min_messages", "It decides which notices the server sends the client: at a debug level it "+
		"sends many with each statement.", "debug5"),
	toDefault("commit_siblings", "Where commit_delay is above 0, which only a superuser sets, a commit first waits "+
		"commit_delay when at least this many other transactions are open: a lower value makes a write's commit "+
		"wait while it holds its row locks.", "0"),
	toDefault("constraint_exclusion", planReason, "on"),
	toDefault("cpu_index_tuple_cost", planReason, "1000"),
	toDefault("cpu_operator_cost", planReason, "1000"),
	toDefault("cpu_tuple_cost", planReason, "1000"),
	exempt("createrole_self_grant", "it acts only when CREATE ROLE runs, and no statement of the client creates a "+
		"role.", "'set, inherit'"),
	exempt("cursor_tuple_fraction", "it plans only a cursor that DECLARE opens, and no statement of the client "+
		"declares one.", "0.0001"),
	exempt("DateStyle", noDates, "'SQL, DMY'"),
	exempt("debug_logical_replication_streaming", logicalOnly, "immediate"),
	toDefault("debug_parallel_query", planReason, "on"),
	exempt("debug_pretty_print", "it only lays out the trees the debug_print settings write to the server log.", "off"),
	exempt("debug_print_parse", "it only writes each statement's parse tree to the server log, where a value the "+
		"client binds shows as a parameter and not as the value.", "on"),
	toDefault("debug_print_plan", "It writes each statement's plan to the server log, and a custom plan holds each "+
		"value the statement binds: the plan of a write of a large session can pass 1 GB, which the server cannot "+
		"allocate, and the write fails.", "on"),
	exempt("debug_print_rewritten", "it only writes each statement's rewritten parse tree to the server log, where "+
		"a value the client binds shows as a parameter and not as the value.", "on"),
	exempt("default_statistics_target", "it acts only when ANALYZE gathers statistics, and no statement of the "+
		"client runs ANALYZE.", "1"),
	toDefault("default_table_access_method", "CreateTables' CREATE TABLE IF NOT EXISTS takes the access method of "+
		"each table it creates from it.", "'heap'"),
	toDefault("default_tablespace", "CreateTables' CREATE TABLE IF NOT EXISTS creates each table in it, and a "+
		"tablespace the login may not create in refuses the statement.", "'pg_global'"),
	exempt("default_text_search_config", "it acts only on text search functions, and no statement of the client "+
		"calls one.", "'pg_catalog.german'"),
	toDefault("default_toast_compression", "It chooses how the server compresses a long value a write stores: every "+
		"method gives back the same value, but in its own time, and a write holds its SELECT … FOR UPDATE row lock "+
		"while it compresses.", "'lz4'"),
	byBegin("default_transaction_deferrable", beginDeferrable, leftBy("default_transaction_deferrable", "on")),
	byBegin("default_transaction_isolation", beginIsolation, leftBy("default_transaction_isolation", "'serializable'")),
	byBegin("default_transaction_read_only", beginAccess, leftBy("default_transaction_read_only", "on")),
	toDefault("effective_cache_size", planReason, "'8kB'"),
	toDefault("effective_io_concurrency", ioReason, "0"),
	toDefault("enable_async_append", planReason, "off"),
	toDefault("enable_bitmapscan", planReason, "off"),
	toDefault("enable_gathermerge", planReason, "off"),
	toDefault("enable_hashagg", planReason, "off"),
	toDefault("enable_hashjoin", planReason, "off"),
	toDefault("enable_incremental_sort", planReason, "off"),
	toDefault("enable_indexonlyscan", planReason, "off"),
	toDefault("enable_indexscan", planReason, "off"),
	toDefault("enable_material", planReason, "off"),
	toDefault("enable_memoize", planReason, "off"),
	toDefault("enable_mergejoin", planReason, "off"),
	toDefault("enable_nestloop", planReason, "off"),
	toDefault("enable_parallel_append", planReason, "off"),
	toDefault("enable_parallel_hash", planReason, "off"),
	toDefault("enable_partition_pruning", planReason, "off"),
	toDefault("enable_partitionwise_aggregate", planReason, "on"),
	toDefault("enable_partitionwise_join", planReason, "on"),
	toDefault("enable_presorted_aggregate", planReason, "off"),
	toDefault("enable_seqscan", planReason, "off"),
	toDefault("enable_sort", planReason, "off"),
	toDefault("enable_tidscan", planReason, "off"),
	exempt("escape_string_warning", "it only adds a warning for a backslash in a string literal, and no literal of a "+
		"statement of the client holds one.", "off"),
	toDefault("exit_on_error", "On, any error ends the session, and with it the connection and the transaction: the "+
		"openings set it right after application_name, so a refusal later in the opening, or one of a statement of "+
		"the work that the client reads, such as 42P01, ends no backend.", "on"),
	exempt("extra_float_digits", "it changes only the text of floating-point values, and no statement of the client "+
		"reads one: it reads text, name, bigint, integer and char values.", "-15"),
	toDefault("from_collapse_limit", planReason, "1"),
	toDefault("geqo", planReason, "off"),
	toDefault("geqo_effort", planReason, "1"),
	toDefault("geqo_generations", planReason, "1"),
	toDefault("geqo_pool_size", planReason, "2"),
	toDefault("geqo_seed", planReason, "1"),
	toDefault("geqo_selection_bias", planReason, "1.5"),
	toDefault("geqo_threshold", planReason, "2"),
	toDefault("gin_fuzzy_search_limit", "It caps how many rows a GIN index scan returns, so a read through a GIN "+
		"index on a store table could miss rows.", "1"),
	toDefault("gin_pending_list_limit", "A write into a table with a GIN index moves the index's pending list into "+
		"the index when the list passes this size, while the write holds its row locks.", "'64kB'"),
	toDefault("hash_mem_multiplier", memoryReason, "1"),
	exempt("icu_validation_level", "it acts only when CREATE COLLATION or CREATE DATABASE checks an ICU locale, and "+
		"no statement of the client sends either.", "error"),
	toDefault("idle_in_transaction_session_timeout", "It ends the session when a transaction of the client waits "+
		"longer than this between two of its statements, as it does while the caller works out its next one.", "'50ms'"),
	exemptBetween("idle_session_timeout", "it ends a session that is idle \"but not within an open transaction\" "+
		"(PostgreSQL 16 documentation, https://www.postgresql.org/docs/16/runtime-config-client.html).", "'5s'"),
	exempt("IntervalStyle", noDates, "iso_8601"),
	toDefault("jit", planReason, "off"),
	toDefault("jit_above_cost", planReason, "0"),
	toDefault("jit_expressions", planReason, "off"),
	toDefault("jit_inline_above_cost", planReason, "0"),
	toDefault("jit_optimize_above_cost", planReason, "0"),
	toDefault("jit_tuple_deforming", planReason, "off"),
	toDefault("join_collapse_limit", planReason, "1"),
	exempt("lc_monetary", noLocaleFormatting, "'C'"),
	exempt("lc_numeric", noLocaleFormatting, "'C'"),
	exempt("lc_time", noLocaleFormatting, "'C'"),
	exempt("local_preload_libraries", "the server loads these libraries only when a connection starts, so a value "+
		"set in a session loads nothing.", "''"),
	toDefault("lock_timeout", "It cancels a statement of the client that waits longer than this for a lock, as a "+
		"write waits for a row another writer holds FOR UPDATE.", "'50ms'"),
	exempt("logical_decoding_work_mem", logicalOnly, "'64kB'"),
	toDefault("log_parameter_max_length_on_error", "At -1 the server keeps a text copy of each value a statement "+
		"binds for its error messages: the copy of a large session's content can pass 1 GB, which the server cannot "+
		"allocate, and the write fails.", "-1"),
	toDefault("maintenance_io_concurrency", ioReason, "0"),
	toDefault("maintenance_work_mem", memoryReason, "'1MB'"),
	toDefault("max_parallel_maintenance_workers", planReason, "8"),
	toDefault("max_parallel_workers", planReason, "0"),
	toDefault("max_parallel_workers_per_gather", planReason, "8"),
	toDefault("min_parallel_index_scan_size", planReason, "0"),
	toDefault("min_parallel_table_scan_size", planReason, "0"),
	toDefault("parallel_leader_participation", planReason, "off"),
	toDefault("parallel_setup_cost", planReason, "0"),
	toDefault("parallel_tuple_cost", planReason, "0"),
	exempt("password_encryption", passwordOnly, "md5"),
	toDefault("plan_cache_mode", planReason, "force_generic_plan"),
	noLonger("quote_all_identifiers", "No statement of the client reads a name the server quotes: notTables reads "+
		"relname and relkind from pg_class as they are stored, and no statement calls quote_ident, format or a "+
		"pg_get_*def function, or casts to regclass.", leftBy("quote_all_identifiers", "on")),
	toDefault("random_page_cost", planReason, "1000"),
	exempt("recursive_worktable_factor", "it plans only WITH RECURSIVE, and no statement of the client has a WITH "+
		"clause.", "0.001"),
	setLocal("restrict_nonsystem_relation_kind", "= 'view, foreign-table'", "It refuses each statement that would "+
		"read or write a view or a foreign table, so a view another login made at a store table's name runs none of "+
		"its code; PostgreSQL has it from 16.4 on, the oldest server the client supports.", "''"),
	{
		Name: "row_security", Kind: SetInOpening, Form: SetLocal, ReadValue: "= off", WriteValue: "TO DEFAULT",
		Reason: "Off in a read, a statement on a table with a row-security policy fails rather than run the policy's " +
			"code, which runs as the reader; a write takes the default of the login and the database.",
		Left: leftBy("row_security", "off"),
	},
	exempt("scram_iterations", passwordOnly, "1"),
	setLocal("search_path", "= pg_catalog, pg_temp", "Every function, operator and type a statement of the client "+
		"names resolves in pg_catalog, and each table is named with its schema: no schema another login can create "+
		"in is on the path, and pg_temp is last.", "'pg_catalog'"),
	toDefault("seq_page_cost", planReason, "1000"),
	exempt("standard_conforming_strings", noBackslash, "off"),
	toDefault("statement_timeout", "It cancels a statement of the client that runs longer than this, as a Read of a "+
		"large session or a write that waits for a row lock can.", "'50ms'"),
	exempt("stats_fetch_consistency", "it acts only on reads of the cumulative statistics views, and no statement of "+
		"the client reads one: the catalogs it reads are pg_class, pg_namespace and pg_settings.", "snapshot"),
	exempt("synchronize_seqscans", "it changes only where a scan of a large table starts, and so the order of the "+
		"rows the scan returns, and each statement of the client that returns more than one row of a table orders "+
		"them all: Read by kind and start_offset, FindByID by mtime_ms and path, notTables by relname.", "off"),
	setLocal("synchronous_commit", "= on", "Off, a commit returns before its WAL is flushed, so a write the client "+
		"reports can be lost in a crash of the server.", "off"),
	exempt("tcp_keepalives_count", tcpKeepalives, "1"),
	exempt("tcp_keepalives_idle", tcpKeepalives, "1"),
	exempt("tcp_keepalives_interval", tcpKeepalives, "1"),
	exempt("tcp_user_timeout", "it ends a TCP connection only when data the server sent stays unacknowledged that "+
		"long, and the kernel of a client that is there acknowledges it (PostgreSQL 16 documentation, "+
		"https://www.postgresql.org/docs/16/runtime-config-connection.html).", "'10s'"),
	toDefault("temp_buffers", "It sizes the buffers of temporary tables. No statement of the client reads or writes "+
		"a temporary table, and the openings set it back with the memory settings all the same; PostgreSQL resets "+
		"it to its default without its check, so the SET LOCAL is not refused after the session used a temporary "+
		"table under another value.", "100"),
	toDefault("temp_tablespaces", memoryReason, "'pg_default'"),
	exempt("TimeZone", noDates, "'Pacific/Kiritimati'"),
	exempt("timezone_abbreviations", noDates, "'India'"),
	exempt("trace_notify", "it only writes debug output for LISTEN and NOTIFY, and no statement of the client sends "+
		"either.", "on"),
	exempt("trace_sort", "it only writes a line about each sort to the server log, and changes neither the sort nor "+
		"its rows.", "on"),
	byBegin("transaction_deferrable", beginDeferrable, "SET SESSION CHARACTERISTICS AS TRANSACTION DEFERRABLE"),
	byBegin("transaction_isolation", beginIsolation, "SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL SERIALIZABLE"),
	byBegin("transaction_read_only", beginAccess, "SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY"),
	toDefault("transform_null_equals", "On, the server reads x = NULL in a statement's text as x IS NULL.", "on"),
	exempt("vacuum_buffer_usage_limit", vacuumOnly, "'128kB'"),
	exempt("vacuum_cost_delay", vacuumOnly, "100"),
	exempt("vacuum_cost_limit", vacuumOnly, "1"),
	exempt("vacuum_cost_page_dirty", vacuumOnly, "10000"),
	exempt("vacuum_cost_page_hit", vacuumOnly, "10000"),
	exempt("vacuum_cost_page_miss", vacuumOnly, "10000"),
	exempt("vacuum_failsafe_age", vacuumOnly, "0"),
	exempt("vacuum_freeze_min_age", vacuumOnly, "0"),
	exempt("vacuum_freeze_table_age", vacuumOnly, "0"),
	exempt("vacuum_multixact_failsafe_age", vacuumOnly, "0"),
	exempt("vacuum_multixact_freeze_min_age", vacuumOnly, "0"),
	exempt("vacuum_multixact_freeze_table_age", vacuumOnly, "0"),
	exempt("wal_sender_timeout", "it acts only on a WAL sender, a connection that streams replication, and no "+
		"connection of the client streams any.", "'1ms'"),
	exempt("wal_skip_threshold", "it acts only when wal_level is minimal, where it chooses between WAL and a sync of "+
		"the files to make a table created in the transaction durable at its commit: the table is durable either "+
		"way, and CreateTables creates its tables empty.", "0"),
	toDefault("work_mem", memoryReason, "'64kB'"),
	exempt("xmlbinary", noXML, "hex"),
	exempt("xmloption", noXML, "document"),

	exempt("cloudsql.enable_google_ml_integration", "it lets the functions of the google_ml_integration extension "+
		"call Vertex AI, and no statement of the client calls one.", "off").on(CloudSQLOnly),
	setConfig("cloudsql_session_read_only", "On, Cloud SQL refuses each write of the session, and at locked no "+
		"statement can turn it off: the set_config statement sets it back where Cloud SQL lets it, and where it "+
		"does not, the opening or the first write is refused.", "on", CloudSQLOnly),
	setConfig("google_db_advisor.ai_rewritten_query_return_count", undocumentedReason, "10", CloudSQLOnly),
	setConfig("google_db_advisor.debug_option", undocumentedReason, "1", CloudSQLOnly),
	setConfig("google_db_advisor.enable_ai_perfagent_advice", undocumentedReason, "on", CloudSQLOnly),
	setConfig("google_db_advisor.enable_ai_tune_sql", undocumentedReason, "on", CloudSQLOnly),
	setConfig("google_db_advisor.enable_llm_call_in_background_job", undocumentedReason, "on", CloudSQLOnly),
	setConfig("google_db_advisor.enable_workload_capture", undocumentedReason, "off", CloudSQLOnly),
	setConfig("google_db_advisor.flash_llm_model_id", undocumentedReason, "'another-model'", CloudSQLOnly),
	setConfig("google_db_advisor.llm_model_id", undocumentedReason, "'another-model'", CloudSQLOnly),
	setConfig("google_db_advisor.max_index_width", undocumentedReason, "1", CloudSQLOnly),
	setConfig("google_db_advisor.max_num_cached_query_costs", undocumentedReason, "200000", CloudSQLOnly),
	setConfig("google_db_advisor.max_num_indexable_columns", undocumentedReason, "1", CloudSQLOnly),
	setConfig("google_db_advisor.max_statement_length", undocumentedReason, "0", CloudSQLOnly),
	setConfig("google_db_advisor.max_storage_size_in_mb", undocumentedReason, "1", CloudSQLOnly),
	setConfig("google_db_advisor.min_workload_cost_saving_ratio", undocumentedReason, "0", CloudSQLOnly),
	setConfig("google_db_advisor.recommendation_max_time_in_seconds", undocumentedReason, "0", CloudSQLOnly),
	setConfig("google_db_advisor.recommend_indexes_on_partitions", undocumentedReason, "on", CloudSQLOnly),
	setConfig("google_db_advisor.reset_query_rewrite_stats", undocumentedReason, "on", CloudSQLOnly),
	setConfig("google_db_advisor.test_hook", undocumentedReason, "1", CloudSQLOnly),
	setConfig("google_db_advisor.top_k_slowest_statements", undocumentedReason, "1", CloudSQLOnly),
	setConfig("google_db_advisor.without_cost_comparison", undocumentedReason, "on", CloudSQLOnly),
	setConfig("google_db_advisor.without_rewrite_rules", undocumentedReason, "on", CloudSQLOnly),
	setConfig("google_job_scheduler.debug_option", undocumentedReason, "1", CloudSQLOnly),
	setConfig("google_job_scheduler.enable_task_scheduler", undocumentedReason, "on", CloudSQLOnly),
	setConfig("google_job_scheduler.max_parallel_workers_per_job", undocumentedReason, "0", CloudSQLOnly),
	exempt("google_ml_integration.enable_model_support", "it turns on model endpoint management in the "+
		"google_ml_integration extension, and no statement of the client calls a function of it.", "off").on(CloudSQLOnly),

	exempt("enable_distinct_reordering", "it plans only DISTINCT, and no statement of the client has one: Read's "+
		"UNION ALL keeps every row.", "off").on(PostgreSQL18AndLater),
	exempt("enable_group_by_reordering", "it plans only GROUP BY, and no statement of the client has one.", "off").
		on(PostgreSQL17AndLater),
	exempt("enable_self_join_elimination", "it acts only on a join of a table with itself, and the one join of the "+
		"client, notTables', joins pg_class with pg_namespace.", "off").on(PostgreSQL18AndLater),
	exempt("file_copy_method", "it acts only on CREATE DATABASE and ALTER DATABASE … SET TABLESPACE, and no statement "+
		"of the client sends either.", "clone").on(PostgreSQL18AndLater),
	setConfig("io_combine_limit", ioReason, "1", PostgreSQL17AndLater),
	exempt("md5_password_warnings", "it only adds a warning when CREATE ROLE or ALTER ROLE sets an MD5 password, and "+
		"no statement of the client sets a password.", "off").on(PostgreSQL18AndLater),
	setConfig("transaction_timeout", "It ends the session when a transaction of the client runs longer than this. "+
		"BEGIN starts its timer with the session's value, and a new value stops a running timer only at 0, so the "+
		"openings set it to 0 just before the set_config statement sets it back.", "'100ms'", PostgreSQL17AndLater),
	exempt("vacuum_max_eager_freeze_failure_rate", vacuumOnly, "0").on(PostgreSQL18AndLater),
	exempt("vacuum_truncate", vacuumOnly, "off").on(PostgreSQL18AndLater),

	{
		Name: "role", Kind: SetInOpening, Form: SetLocal, ReadValue: "TO DEFAULT", WriteValue: "TO DEFAULT",
		Reason: "A role another client set runs the client's statements: one without rights on the store tables " +
			"refuses them, and one with other rights runs them as itself.",
		Left: "SET ROLE left_no_rights",
	},
	temporary("omp_session_files"),
	temporary("omp_session_files_parts"),
	temporary("omp_session_leases"),
	temporary("omp_session_files_blobs"),
	temporary("omp_session_files_refs"),
}
