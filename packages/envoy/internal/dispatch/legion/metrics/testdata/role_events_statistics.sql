-- The planner's statistics of events and of its indexes over a log of a million events beside the
-- three issues' 150 (1,000,150 rows), as TestTheRoleEventsReadReadsTheIssuesRowsByIssue read them
-- back when it wrote that log itself: run at 1a37a9ffe738 on PostgreSQL 18.7, after its
-- `analyze events`, `pg_dump --statistics-only --table=public.events` of its database. Kept from
-- that dump, verbatim: the relation statistics of events and of each of its indexes, and the
-- attribute statistics of the columns roleEventsStatement reads (actor, created_at, issue_key) and
-- of events_session_actor_created_at_idx's expression, actor->>'id'. The test states each
-- relation's relpages again after this file, as the pages the relation holds (it says why).

SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events',
	'relpages', '20412'::integer,
	'reltuples', '1.00015e+06'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_attribute_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events',
	'attname', 'actor',
	'inherited', 'f'::boolean,
	'null_frac', '0'::real,
	'avg_width', '39'::integer,
	'n_distinct', '5005'::real,
	'most_common_vals', '{"{\"id\": \"s-1524\", \"kind\": \"session\"}","{\"id\": \"s-3021\", \"kind\": \"session\"}","{\"id\": \"s-1821\", \"kind\": \"session\"}","{\"id\": \"s-1940\", \"kind\": \"session\"}","{\"id\": \"s-2139\", \"kind\": \"session\"}","{\"id\": \"s-3792\", \"kind\": \"session\"}","{\"id\": \"s-4878\", \"kind\": \"session\"}","{\"id\": \"s-1762\", \"kind\": \"session\"}","{\"id\": \"s-2913\", \"kind\": \"session\"}","{\"id\": \"s-3152\", \"kind\": \"session\"}","{\"id\": \"s-3693\", \"kind\": \"session\"}","{\"id\": \"s-389\", \"kind\": \"session\"}","{\"id\": \"s-4020\", \"kind\": \"session\"}","{\"id\": \"s-972\", \"kind\": \"session\"}"}'::text,
	'most_common_freqs', '{0.00053333334,0.00053333334,0.0005,0.0005,0.0005,0.0005,0.0005,0.00046666668,0.00046666668,0.00046666668,0.00046666668,0.00046666668,0.00046666668,0.00046666668}'::real[],
	'histogram_bounds', '{"{\"id\": \"alice\", \"kind\": \"user\"}","{\"id\": \"s-1041\", \"kind\": \"session\"}","{\"id\": \"s-1092\", \"kind\": \"session\"}","{\"id\": \"s-1141\", \"kind\": \"session\"}","{\"id\": \"s-1192\", \"kind\": \"session\"}","{\"id\": \"s-1236\", \"kind\": \"session\"}","{\"id\": \"s-1283\", \"kind\": \"session\"}","{\"id\": \"s-1322\", \"kind\": \"session\"}","{\"id\": \"s-1369\", \"kind\": \"session\"}","{\"id\": \"s-1410\", \"kind\": \"session\"}","{\"id\": \"s-1452\", \"kind\": \"session\"}","{\"id\": \"s-1499\", \"kind\": \"session\"}","{\"id\": \"s-1546\", \"kind\": \"session\"}","{\"id\": \"s-1587\", \"kind\": \"session\"}","{\"id\": \"s-1631\", \"kind\": \"session\"}","{\"id\": \"s-1679\", \"kind\": \"session\"}","{\"id\": \"s-172\", \"kind\": \"session\"}","{\"id\": \"s-1759\", \"kind\": \"session\"}","{\"id\": \"s-1808\", \"kind\": \"session\"}","{\"id\": \"s-1856\", \"kind\": \"session\"}","{\"id\": \"s-1898\", \"kind\": \"session\"}","{\"id\": \"s-1938\", \"kind\": \"session\"}","{\"id\": \"s-1982\", \"kind\": \"session\"}","{\"id\": \"s-2024\", \"kind\": \"session\"}","{\"id\": \"s-2068\", \"kind\": \"session\"}","{\"id\": \"s-2110\", \"kind\": \"session\"}","{\"id\": \"s-2155\", \"kind\": \"session\"}","{\"id\": \"s-2200\", \"kind\": \"session\"}","{\"id\": \"s-2249\", \"kind\": \"session\"}","{\"id\": \"s-2293\", \"kind\": \"session\"}","{\"id\": \"s-2336\", \"kind\": \"session\"}","{\"id\": \"s-2388\", \"kind\": \"session\"}","{\"id\": \"s-2433\", \"kind\": \"session\"}","{\"id\": \"s-248\", \"kind\": \"session\"}","{\"id\": \"s-2518\", \"kind\": \"session\"}","{\"id\": \"s-2568\", \"kind\": \"session\"}","{\"id\": \"s-2614\", \"kind\": \"session\"}","{\"id\": \"s-2657\", \"kind\": \"session\"}","{\"id\": \"s-2703\", \"kind\": \"session\"}","{\"id\": \"s-2750\", \"kind\": \"session\"}","{\"id\": \"s-2794\", \"kind\": \"session\"}","{\"id\": \"s-2833\", \"kind\": \"session\"}","{\"id\": \"s-2876\", \"kind\": \"session\"}","{\"id\": \"s-292\", \"kind\": \"session\"}","{\"id\": \"s-2963\", \"kind\": \"session\"}","{\"id\": \"s-3001\", \"kind\": \"session\"}","{\"id\": \"s-3048\", \"kind\": \"session\"}","{\"id\": \"s-309\", \"kind\": \"session\"}","{\"id\": \"s-3132\", \"kind\": \"session\"}","{\"id\": \"s-3178\", \"kind\": \"session\"}","{\"id\": \"s-3223\", \"kind\": \"session\"}","{\"id\": \"s-3269\", \"kind\": \"session\"}","{\"id\": \"s-3315\", \"kind\": \"session\"}","{\"id\": \"s-3359\", \"kind\": \"session\"}","{\"id\": \"s-34\", \"kind\": \"session\"}","{\"id\": \"s-3442\", \"kind\": \"session\"}","{\"id\": \"s-3488\", \"kind\": \"session\"}","{\"id\": \"s-3532\", \"kind\": \"session\"}","{\"id\": \"s-3575\", \"kind\": \"session\"}","{\"id\": \"s-3619\", \"kind\": \"session\"}","{\"id\": \"s-3665\", \"kind\": \"session\"}","{\"id\": \"s-3713\", \"kind\": \"session\"}","{\"id\": \"s-3758\", \"kind\": \"session\"}","{\"id\": \"s-381\", \"kind\": \"session\"}","{\"id\": \"s-3854\", \"kind\": \"session\"}","{\"id\": \"s-3904\", \"kind\": \"session\"}","{\"id\": \"s-3952\", \"kind\": \"session\"}","{\"id\": \"s-3997\", \"kind\": \"session\"}","{\"id\": \"s-4037\", \"kind\": \"session\"}","{\"id\": \"s-408\", \"kind\": \"session\"}","{\"id\": \"s-4131\", \"kind\": \"session\"}","{\"id\": \"s-4177\", \"kind\": \"session\"}","{\"id\": \"s-4221\", \"kind\": \"session\"}","{\"id\": \"s-427\", \"kind\": \"session\"}","{\"id\": \"s-4314\", \"kind\": \"session\"}","{\"id\": \"s-436\", \"kind\": \"session\"}","{\"id\": \"s-440\", \"kind\": \"session\"}","{\"id\": \"s-4444\", \"kind\": \"session\"}","{\"id\": \"s-4491\", \"kind\": \"session\"}","{\"id\": \"s-4538\", \"kind\": \"session\"}","{\"id\": \"s-4584\", \"kind\": \"session\"}","{\"id\": \"s-4631\", \"kind\": \"session\"}","{\"id\": \"s-4677\", \"kind\": \"session\"}","{\"id\": \"s-4721\", \"kind\": \"session\"}","{\"id\": \"s-4765\", \"kind\": \"session\"}","{\"id\": \"s-4812\", \"kind\": \"session\"}","{\"id\": \"s-4862\", \"kind\": \"session\"}","{\"id\": \"s-4909\", \"kind\": \"session\"}","{\"id\": \"s-4955\", \"kind\": \"session\"}","{\"id\": \"s-4998\", \"kind\": \"session\"}","{\"id\": \"s-540\", \"kind\": \"session\"}","{\"id\": \"s-588\", \"kind\": \"session\"}","{\"id\": \"s-633\", \"kind\": \"session\"}","{\"id\": \"s-68\", \"kind\": \"session\"}","{\"id\": \"s-727\", \"kind\": \"session\"}","{\"id\": \"s-772\", \"kind\": \"session\"}","{\"id\": \"s-813\", \"kind\": \"session\"}","{\"id\": \"s-862\", \"kind\": \"session\"}","{\"id\": \"s-906\", \"kind\": \"session\"}","{\"id\": \"s-951\", \"kind\": \"session\"}","{\"id\": \"s-INFRA-3-50\", \"kind\": \"session\", \"origin\": {\"session_title\": \"Legion implementer · INFRA-3\"}}"}'::text,
	'correlation', '-0.0020898085'::real
);
SELECT * FROM pg_catalog.pg_restore_attribute_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events',
	'attname', 'created_at',
	'inherited', 'f'::boolean,
	'null_frac', '0'::real,
	'avg_width', '8'::integer,
	'n_distinct', '-1'::real,
	'histogram_bounds', '{"2026-11-16 16:58:24.011472+00","2026-11-16 19:33:28.011472+00","2026-11-16 22:34:53.011472+00","2026-11-17 01:05:39.011472+00","2026-11-17 03:56:50.011472+00","2026-11-17 06:26:47.011472+00","2026-11-17 09:25:28.011472+00","2026-11-17 12:00:28.011472+00","2026-11-17 15:03:47.011472+00","2026-11-17 17:55:55.011472+00","2026-11-17 20:38:29.011472+00","2026-11-17 23:14:42.011472+00","2026-11-18 02:19:02.011472+00","2026-11-18 05:10:30.011472+00","2026-11-18 08:04:17.011472+00","2026-11-18 10:43:41.011472+00","2026-11-18 13:21:46.011472+00","2026-11-18 16:00:55.011472+00","2026-11-18 18:45:49.011472+00","2026-11-18 21:38:38.011472+00","2026-11-19 00:40:20.011472+00","2026-11-19 03:13:05.011472+00","2026-11-19 06:10:44.011472+00","2026-11-19 08:52:39.011472+00","2026-11-19 11:45:45.011472+00","2026-11-19 14:33:31.011472+00","2026-11-19 17:30:07.011472+00","2026-11-19 20:13:15.011472+00","2026-11-19 22:51:47.011472+00","2026-11-20 01:33:16.011472+00","2026-11-20 04:18:14.011472+00","2026-11-20 07:10:24.011472+00","2026-11-20 10:01:50.011472+00","2026-11-20 12:45:08.011472+00","2026-11-20 15:22:41.011472+00","2026-11-20 18:13:33.011472+00","2026-11-20 20:51:31.011472+00","2026-11-20 23:17:12.011472+00","2026-11-21 01:53:31.011472+00","2026-11-21 04:39:34.011472+00","2026-11-21 07:30:43.011472+00","2026-11-21 10:18:16.011472+00","2026-11-21 13:03:55.011472+00","2026-11-21 15:42:05.011472+00","2026-11-21 18:47:38.011472+00","2026-11-21 21:29:54.011472+00","2026-11-22 00:14:00.011472+00","2026-11-22 03:09:39.011472+00","2026-11-22 05:55:57.011472+00","2026-11-22 08:35:16.011472+00","2026-11-22 11:17:09.011472+00","2026-11-22 14:15:32.011472+00","2026-11-22 16:55:39.011472+00","2026-11-22 19:41:49.011472+00","2026-11-22 22:31:34.011472+00","2026-11-23 01:23:28.011472+00","2026-11-23 04:06:08.011472+00","2026-11-23 06:46:05.011472+00","2026-11-23 09:22:03.011472+00","2026-11-23 12:10:20.011472+00","2026-11-23 15:11:58.011472+00","2026-11-23 17:56:04.011472+00","2026-11-23 20:36:41.011472+00","2026-11-23 23:38:32.011472+00","2026-11-24 02:35:35.011472+00","2026-11-24 05:31:18.011472+00","2026-11-24 08:03:17.011472+00","2026-11-24 11:11:18.011472+00","2026-11-24 14:07:53.011472+00","2026-11-24 16:44:07.011472+00","2026-11-24 19:28:11.011472+00","2026-11-24 22:09:01.011472+00","2026-11-25 00:47:31.011472+00","2026-11-25 03:32:14.011472+00","2026-11-25 06:34:25.011472+00","2026-11-25 09:12:04.011472+00","2026-11-25 11:57:24.011472+00","2026-11-25 14:50:48.011472+00","2026-11-25 17:40:50.011472+00","2026-11-25 20:16:18.011472+00","2026-11-25 23:00:42.011472+00","2026-11-26 01:42:13.011472+00","2026-11-26 04:40:21.011472+00","2026-11-26 07:21:11.011472+00","2026-11-26 09:49:07.011472+00","2026-11-26 12:44:00.011472+00","2026-11-26 15:47:46.011472+00","2026-11-26 18:28:22.011472+00","2026-11-26 21:19:08.011472+00","2026-11-27 00:17:39.011472+00","2026-11-27 03:04:09.011472+00","2026-11-27 05:32:17.011472+00","2026-11-27 08:32:52.011472+00","2026-11-27 11:22:04.011472+00","2026-11-27 13:53:38.011472+00","2026-11-27 16:44:39.011472+00","2026-11-27 19:40:57.011472+00","2026-11-27 22:28:34.011472+00","2026-11-28 01:15:42.011472+00","2026-11-28 03:56:19.011472+00","2026-11-28 06:44:55.011472+00"}'::text,
	'correlation', '-0.9992024'::real
);
SELECT * FROM pg_catalog.pg_restore_attribute_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events',
	'attname', 'issue_key',
	'inherited', 'f'::boolean,
	'null_frac', '0'::real,
	'avg_width', '10'::integer,
	'n_distinct', '10008'::real,
	'most_common_vals', '{INFRA-165,INFRA-2190,INFRA-2800,INFRA-2932,INFRA-4444,INFRA-6258,INFRA-6356,INFRA-7066,INFRA-7207,INFRA-7834}'::text,
	'most_common_freqs', '{0.00033333333,0.00033333333,0.00033333333,0.00033333333,0.00033333333,0.00033333333,0.00033333333,0.00033333333,0.00033333333,0.00033333333}'::real[],
	'histogram_bounds', '{INFRA-1,INFRA-1087,INFRA-1178,INFRA-1274,INFRA-1357,INFRA-1454,INFRA-1543,INFRA-1636,INFRA-1718,INFRA-1807,INFRA-1900,INFRA-199,INFRA-2079,INFRA-2166,INFRA-2256,INFRA-2343,INFRA-2438,INFRA-2523,INFRA-2614,INFRA-2712,INFRA-2807,INFRA-2889,INFRA-2975,INFRA-3061,INFRA-3150,INFRA-3241,INFRA-3335,INFRA-3431,INFRA-3514,INFRA-361,INFRA-3707,INFRA-3806,INFRA-3896,INFRA-3981,INFRA-4075,INFRA-4163,INFRA-4256,INFRA-4341,INFRA-4434,INFRA-4522,INFRA-4613,INFRA-4705,INFRA-4794,INFRA-4888,INFRA-4983,INFRA-5065,INFRA-5163,INFRA-5251,INFRA-5340,INFRA-5429,INFRA-5518,INFRA-5605,INFRA-5704,INFRA-5794,INFRA-5876,INFRA-597,INFRA-6066,INFRA-6153,INFRA-6242,INFRA-6327,INFRA-6405,INFRA-6489,INFRA-6581,INFRA-6666,INFRA-675,INFRA-6842,INFRA-6930,INFRA-7029,INFRA-712,INFRA-7206,INFRA-7299,INFRA-7387,INFRA-7479,INFRA-7570,INFRA-7667,INFRA-7753,INFRA-785,INFRA-7934,INFRA-8022,INFRA-812,INFRA-8214,INFRA-8302,INFRA-8387,INFRA-8478,INFRA-8565,INFRA-8652,INFRA-8750,INFRA-8841,INFRA-8935,INFRA-9026,INFRA-9110,INFRA-9200,INFRA-9299,INFRA-9384,INFRA-9473,INFRA-9563,INFRA-9650,INFRA-9735,INFRA-9820,INFRA-9917,INFRA-9999}'::text,
	'correlation', '0.8138798'::real
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_artifact_id_seq_key',
	'relpages', '808'::integer,
	'reltuples', '1.00015e+06'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_artifact_renamed',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_artifact_version_number',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_ask_answered_by_user',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_ask_follower',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_ask_payload_id',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_ask_state',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_backlog_moves',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_comment_created_id',
	'relpages', '3853'::integer,
	'reltuples', '1.00015e+06'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_comment_state',
	'relpages', '8914'::integer,
	'reltuples', '1.00015e+06'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_created_at',
	'relpages', '6885'::integer,
	'reltuples', '1.00015e+06'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_issue_activity',
	'relpages', '10134'::integer,
	'reltuples', '1.00015e+06'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_issue_claim',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_issue_key_seq_key',
	'relpages', '4642'::integer,
	'reltuples', '1.00015e+06'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_issue_state',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_message_state',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_outbox_fresh',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_outbox_owner',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_outbox_retry',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_person_words_destinations',
	'relpages', '2'::integer,
	'reltuples', '34'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_person_words_with_declines',
	'relpages', '2'::integer,
	'reltuples', '34'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_pkey',
	'relpages', '2745'::integer,
	'reltuples', '1.00015e+06'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_post_text_changes',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_project_key_seq_key',
	'relpages', '808'::integer,
	'reltuples', '1.00015e+06'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_project_setting',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_repo_project',
	'relpages', '1'::integer,
	'reltuples', '0'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_relation_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_session_actor_created_at_idx',
	'relpages', '5153'::integer,
	'reltuples', '1.000117e+06'::real,
	'relallvisible', '0'::integer,
	'relallfrozen', '0'::integer
);
SELECT * FROM pg_catalog.pg_restore_attribute_stats(
	'version', '180007'::integer,
	'schemaname', 'public',
	'relname', 'events_session_actor_created_at_idx',
	'attnum', '1'::smallint,
	'inherited', 'f'::boolean,
	'null_frac', '0'::real,
	'avg_width', '9'::integer,
	'n_distinct', '5004'::real,
	'most_common_vals', '{s-1524,s-3021,s-1821,s-1940,s-2139,s-3792,s-4878,s-1762,s-2913,s-3152,s-3693,s-389,s-4020,s-972}'::text,
	'most_common_freqs', '{0.0005333511,0.0005333511,0.0005000167,0.0005000167,0.0005000167,0.0005000167,0.0005000167,0.00046668222,0.00046668222,0.00046668222,0.00046668222,0.00046668222,0.00046668222,0.00046668222}'::real[],
	'histogram_bounds', '{s-0,s-1042,s-1092,s-1141,s-1192,s-1236,s-1283,s-1322,s-1369,s-1410,s-1452,s-1499,s-1546,s-1587,s-1631,s-1679,s-172,s-1759,s-1808,s-1857,s-1898,s-1939,s-1982,s-2024,s-2068,s-2110,s-2155,s-2200,s-2249,s-2293,s-2336,s-2388,s-2433,s-248,s-2518,s-2568,s-2614,s-2657,s-2704,s-2750,s-2794,s-2833,s-2876,s-292,s-2963,s-3001,s-3048,s-309,s-3132,s-3178,s-3223,s-327,s-3315,s-3359,s-34,s-3442,s-3488,s-3532,s-3575,s-3619,s-3665,s-3713,s-3758,s-381,s-3854,s-3904,s-3952,s-3997,s-4038,s-408,s-4131,s-4177,s-4221,s-427,s-4314,s-436,s-440,s-4444,s-4491,s-4538,s-4584,s-4631,s-4677,s-4721,s-4765,s-4812,s-4862,s-4909,s-4955,s-4998,s-540,s-588,s-633,s-68,s-727,s-772,s-813,s-862,s-906,s-951,s-INFRA-3-50}'::text,
	'correlation', '-0.0019900242'::real
);
