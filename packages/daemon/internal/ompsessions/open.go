package ompsessions

// OpenRead is the opening message of each read-only transaction of the client, Written's,
// FindByID's and Read's, for inTransaction to send as one simple-protocol Query message (pgx's
// BeginTx with TxOptions.BeginQuery, which sends it through Exec with no arguments). The server runs
// the whole message before it waits for the next one, so a left idle_in_transaction_session_timeout
// cannot end the connection between BEGIN and the SET LOCAL that sets it back. It sets each entry
// LeftSettings marks as set in the opening message, and no other
// (TestTheOpeningsSetEachEntryTheListMarksAndNoOther), in this order: BEGIN with READ COMMITTED and
// READ ONLY, which set the transaction's isolation level and access mode; application_name, so the
// server's log names each later statement of the transaction, a refused one included; exit_on_error,
// so a refusal later in the transaction ends no backend; the three timeouts; the search_path pin,
// restrict_nonsystem_relation_kind and row_security off; every other SET LOCAL entry in the list's
// order; on PostgreSQL 17 and later, a stop of the timer that a left transaction_timeout started at
// BEGIN, since a new value stops a running timer only at 0; and last the set_config statement, which
// sets each entry some supported server lacks to its reset value where the server has it, as SET
// LOCAL … TO DEFAULT would, and sets nothing where it does not. Each value ends with the transaction.
// It is plain SQL in one raw string literal, one statement on each line, so a reader copies it from
// this file and sends it with psql -c as it stands (TestTheOpeningsArePlainSQLThatPsqlSendsAsTheyStand).
const OpenRead = `BEGIN ISOLATION LEVEL READ COMMITTED READ ONLY;
SET LOCAL application_name = 'legion-session-client';
SET LOCAL exit_on_error TO DEFAULT;
SET LOCAL statement_timeout TO DEFAULT;
SET LOCAL lock_timeout TO DEFAULT;
SET LOCAL idle_in_transaction_session_timeout TO DEFAULT;
SET LOCAL search_path = pg_catalog, pg_temp;
SET LOCAL restrict_nonsystem_relation_kind = 'view, foreign-table';
SET LOCAL row_security = off;
SET LOCAL array_nulls TO DEFAULT;
SET LOCAL backend_flush_after TO DEFAULT;
SET LOCAL client_encoding = 'UTF8';
SET LOCAL client_min_messages TO DEFAULT;
SET LOCAL commit_siblings TO DEFAULT;
SET LOCAL constraint_exclusion TO DEFAULT;
SET LOCAL cpu_index_tuple_cost TO DEFAULT;
SET LOCAL cpu_operator_cost TO DEFAULT;
SET LOCAL cpu_tuple_cost TO DEFAULT;
SET LOCAL debug_parallel_query TO DEFAULT;
SET LOCAL debug_print_plan TO DEFAULT;
SET LOCAL default_table_access_method TO DEFAULT;
SET LOCAL default_tablespace TO DEFAULT;
SET LOCAL default_toast_compression TO DEFAULT;
SET LOCAL effective_cache_size TO DEFAULT;
SET LOCAL effective_io_concurrency TO DEFAULT;
SET LOCAL enable_async_append TO DEFAULT;
SET LOCAL enable_bitmapscan TO DEFAULT;
SET LOCAL enable_gathermerge TO DEFAULT;
SET LOCAL enable_hashagg TO DEFAULT;
SET LOCAL enable_hashjoin TO DEFAULT;
SET LOCAL enable_incremental_sort TO DEFAULT;
SET LOCAL enable_indexonlyscan TO DEFAULT;
SET LOCAL enable_indexscan TO DEFAULT;
SET LOCAL enable_material TO DEFAULT;
SET LOCAL enable_memoize TO DEFAULT;
SET LOCAL enable_mergejoin TO DEFAULT;
SET LOCAL enable_nestloop TO DEFAULT;
SET LOCAL enable_parallel_append TO DEFAULT;
SET LOCAL enable_parallel_hash TO DEFAULT;
SET LOCAL enable_partition_pruning TO DEFAULT;
SET LOCAL enable_partitionwise_aggregate TO DEFAULT;
SET LOCAL enable_partitionwise_join TO DEFAULT;
SET LOCAL enable_presorted_aggregate TO DEFAULT;
SET LOCAL enable_seqscan TO DEFAULT;
SET LOCAL enable_sort TO DEFAULT;
SET LOCAL enable_tidscan TO DEFAULT;
SET LOCAL from_collapse_limit TO DEFAULT;
SET LOCAL geqo TO DEFAULT;
SET LOCAL geqo_effort TO DEFAULT;
SET LOCAL geqo_generations TO DEFAULT;
SET LOCAL geqo_pool_size TO DEFAULT;
SET LOCAL geqo_seed TO DEFAULT;
SET LOCAL geqo_selection_bias TO DEFAULT;
SET LOCAL geqo_threshold TO DEFAULT;
SET LOCAL gin_fuzzy_search_limit TO DEFAULT;
SET LOCAL gin_pending_list_limit TO DEFAULT;
SET LOCAL hash_mem_multiplier TO DEFAULT;
SET LOCAL jit TO DEFAULT;
SET LOCAL jit_above_cost TO DEFAULT;
SET LOCAL jit_expressions TO DEFAULT;
SET LOCAL jit_inline_above_cost TO DEFAULT;
SET LOCAL jit_optimize_above_cost TO DEFAULT;
SET LOCAL jit_tuple_deforming TO DEFAULT;
SET LOCAL join_collapse_limit TO DEFAULT;
SET LOCAL log_parameter_max_length_on_error TO DEFAULT;
SET LOCAL maintenance_io_concurrency TO DEFAULT;
SET LOCAL maintenance_work_mem TO DEFAULT;
SET LOCAL max_parallel_maintenance_workers TO DEFAULT;
SET LOCAL max_parallel_workers TO DEFAULT;
SET LOCAL max_parallel_workers_per_gather TO DEFAULT;
SET LOCAL min_parallel_index_scan_size TO DEFAULT;
SET LOCAL min_parallel_table_scan_size TO DEFAULT;
SET LOCAL parallel_leader_participation TO DEFAULT;
SET LOCAL parallel_setup_cost TO DEFAULT;
SET LOCAL parallel_tuple_cost TO DEFAULT;
SET LOCAL plan_cache_mode TO DEFAULT;
SET LOCAL random_page_cost TO DEFAULT;
SET LOCAL seq_page_cost TO DEFAULT;
SET LOCAL synchronous_commit = on;
SET LOCAL temp_buffers TO DEFAULT;
SET LOCAL temp_tablespaces TO DEFAULT;
SET LOCAL transform_null_equals TO DEFAULT;
SET LOCAL work_mem TO DEFAULT;
SET LOCAL role TO DEFAULT;
SELECT pg_catalog.set_config(s.name, '0', true) FROM pg_catalog.pg_settings s WHERE s.name = 'transaction_timeout';
SELECT pg_catalog.set_config(s.name, s.reset_val, true) FROM pg_catalog.pg_settings s WHERE s.name IN ('cloudsql_session_read_only', 'google_db_advisor.ai_rewritten_query_return_count', 'google_db_advisor.debug_option', 'google_db_advisor.enable_ai_perfagent_advice', 'google_db_advisor.enable_ai_tune_sql', 'google_db_advisor.enable_llm_call_in_background_job', 'google_db_advisor.enable_workload_capture', 'google_db_advisor.flash_llm_model_id', 'google_db_advisor.llm_model_id', 'google_db_advisor.max_index_width', 'google_db_advisor.max_num_cached_query_costs', 'google_db_advisor.max_num_indexable_columns', 'google_db_advisor.max_statement_length', 'google_db_advisor.max_storage_size_in_mb', 'google_db_advisor.min_workload_cost_saving_ratio', 'google_db_advisor.recommendation_max_time_in_seconds', 'google_db_advisor.recommend_indexes_on_partitions', 'google_db_advisor.reset_query_rewrite_stats', 'google_db_advisor.test_hook', 'google_db_advisor.top_k_slowest_statements', 'google_db_advisor.without_cost_comparison', 'google_db_advisor.without_rewrite_rules', 'google_job_scheduler.debug_option', 'google_job_scheduler.enable_task_scheduler', 'google_job_scheduler.max_parallel_workers_per_job', 'io_combine_limit', 'transaction_timeout')`

// OpenWrite is the opening message of each read-write transaction of the client, CreateTables',
// Import's, Append's and Replace's, for inTransaction to send as one simple-protocol Query message as
// OpenRead is sent. It holds OpenRead's statements in OpenRead's order, but for two: BEGIN names READ
// WRITE, and row_security is set TO DEFAULT, the default of the login and the database, where a read
// sets it off. It is plain SQL in one raw string literal, one statement on each line, so a reader
// copies it from this file and sends it with psql -c as it stands.
const OpenWrite = `BEGIN ISOLATION LEVEL READ COMMITTED READ WRITE;
SET LOCAL application_name = 'legion-session-client';
SET LOCAL exit_on_error TO DEFAULT;
SET LOCAL statement_timeout TO DEFAULT;
SET LOCAL lock_timeout TO DEFAULT;
SET LOCAL idle_in_transaction_session_timeout TO DEFAULT;
SET LOCAL search_path = pg_catalog, pg_temp;
SET LOCAL restrict_nonsystem_relation_kind = 'view, foreign-table';
SET LOCAL row_security TO DEFAULT;
SET LOCAL array_nulls TO DEFAULT;
SET LOCAL backend_flush_after TO DEFAULT;
SET LOCAL client_encoding = 'UTF8';
SET LOCAL client_min_messages TO DEFAULT;
SET LOCAL commit_siblings TO DEFAULT;
SET LOCAL constraint_exclusion TO DEFAULT;
SET LOCAL cpu_index_tuple_cost TO DEFAULT;
SET LOCAL cpu_operator_cost TO DEFAULT;
SET LOCAL cpu_tuple_cost TO DEFAULT;
SET LOCAL debug_parallel_query TO DEFAULT;
SET LOCAL debug_print_plan TO DEFAULT;
SET LOCAL default_table_access_method TO DEFAULT;
SET LOCAL default_tablespace TO DEFAULT;
SET LOCAL default_toast_compression TO DEFAULT;
SET LOCAL effective_cache_size TO DEFAULT;
SET LOCAL effective_io_concurrency TO DEFAULT;
SET LOCAL enable_async_append TO DEFAULT;
SET LOCAL enable_bitmapscan TO DEFAULT;
SET LOCAL enable_gathermerge TO DEFAULT;
SET LOCAL enable_hashagg TO DEFAULT;
SET LOCAL enable_hashjoin TO DEFAULT;
SET LOCAL enable_incremental_sort TO DEFAULT;
SET LOCAL enable_indexonlyscan TO DEFAULT;
SET LOCAL enable_indexscan TO DEFAULT;
SET LOCAL enable_material TO DEFAULT;
SET LOCAL enable_memoize TO DEFAULT;
SET LOCAL enable_mergejoin TO DEFAULT;
SET LOCAL enable_nestloop TO DEFAULT;
SET LOCAL enable_parallel_append TO DEFAULT;
SET LOCAL enable_parallel_hash TO DEFAULT;
SET LOCAL enable_partition_pruning TO DEFAULT;
SET LOCAL enable_partitionwise_aggregate TO DEFAULT;
SET LOCAL enable_partitionwise_join TO DEFAULT;
SET LOCAL enable_presorted_aggregate TO DEFAULT;
SET LOCAL enable_seqscan TO DEFAULT;
SET LOCAL enable_sort TO DEFAULT;
SET LOCAL enable_tidscan TO DEFAULT;
SET LOCAL from_collapse_limit TO DEFAULT;
SET LOCAL geqo TO DEFAULT;
SET LOCAL geqo_effort TO DEFAULT;
SET LOCAL geqo_generations TO DEFAULT;
SET LOCAL geqo_pool_size TO DEFAULT;
SET LOCAL geqo_seed TO DEFAULT;
SET LOCAL geqo_selection_bias TO DEFAULT;
SET LOCAL geqo_threshold TO DEFAULT;
SET LOCAL gin_fuzzy_search_limit TO DEFAULT;
SET LOCAL gin_pending_list_limit TO DEFAULT;
SET LOCAL hash_mem_multiplier TO DEFAULT;
SET LOCAL jit TO DEFAULT;
SET LOCAL jit_above_cost TO DEFAULT;
SET LOCAL jit_expressions TO DEFAULT;
SET LOCAL jit_inline_above_cost TO DEFAULT;
SET LOCAL jit_optimize_above_cost TO DEFAULT;
SET LOCAL jit_tuple_deforming TO DEFAULT;
SET LOCAL join_collapse_limit TO DEFAULT;
SET LOCAL log_parameter_max_length_on_error TO DEFAULT;
SET LOCAL maintenance_io_concurrency TO DEFAULT;
SET LOCAL maintenance_work_mem TO DEFAULT;
SET LOCAL max_parallel_maintenance_workers TO DEFAULT;
SET LOCAL max_parallel_workers TO DEFAULT;
SET LOCAL max_parallel_workers_per_gather TO DEFAULT;
SET LOCAL min_parallel_index_scan_size TO DEFAULT;
SET LOCAL min_parallel_table_scan_size TO DEFAULT;
SET LOCAL parallel_leader_participation TO DEFAULT;
SET LOCAL parallel_setup_cost TO DEFAULT;
SET LOCAL parallel_tuple_cost TO DEFAULT;
SET LOCAL plan_cache_mode TO DEFAULT;
SET LOCAL random_page_cost TO DEFAULT;
SET LOCAL seq_page_cost TO DEFAULT;
SET LOCAL synchronous_commit = on;
SET LOCAL temp_buffers TO DEFAULT;
SET LOCAL temp_tablespaces TO DEFAULT;
SET LOCAL transform_null_equals TO DEFAULT;
SET LOCAL work_mem TO DEFAULT;
SET LOCAL role TO DEFAULT;
SELECT pg_catalog.set_config(s.name, '0', true) FROM pg_catalog.pg_settings s WHERE s.name = 'transaction_timeout';
SELECT pg_catalog.set_config(s.name, s.reset_val, true) FROM pg_catalog.pg_settings s WHERE s.name IN ('cloudsql_session_read_only', 'google_db_advisor.ai_rewritten_query_return_count', 'google_db_advisor.debug_option', 'google_db_advisor.enable_ai_perfagent_advice', 'google_db_advisor.enable_ai_tune_sql', 'google_db_advisor.enable_llm_call_in_background_job', 'google_db_advisor.enable_workload_capture', 'google_db_advisor.flash_llm_model_id', 'google_db_advisor.llm_model_id', 'google_db_advisor.max_index_width', 'google_db_advisor.max_num_cached_query_costs', 'google_db_advisor.max_num_indexable_columns', 'google_db_advisor.max_statement_length', 'google_db_advisor.max_storage_size_in_mb', 'google_db_advisor.min_workload_cost_saving_ratio', 'google_db_advisor.recommendation_max_time_in_seconds', 'google_db_advisor.recommend_indexes_on_partitions', 'google_db_advisor.reset_query_rewrite_stats', 'google_db_advisor.test_hook', 'google_db_advisor.top_k_slowest_statements', 'google_db_advisor.without_cost_comparison', 'google_db_advisor.without_rewrite_rules', 'google_job_scheduler.debug_option', 'google_job_scheduler.enable_task_scheduler', 'google_job_scheduler.max_parallel_workers_per_job', 'io_combine_limit', 'transaction_timeout')`
