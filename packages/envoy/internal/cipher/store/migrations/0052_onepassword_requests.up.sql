-- packages/envoy/internal/cipher/store/migrations/0052_onepassword_requests.up.sql
-- onepassword_requests: the 1Password requests the whole deployment made, by hour, which every
-- replica's 1Password store adds to and reads before a call (onepassword.NewBudget): one row an
-- hour, and a spend deletes the rows past 48 hours. A deployment on another store never writes it.
-- It is a new table, so the release before this one, which knows nothing of it, runs as it did.
create table if not exists onepassword_requests (
  hour timestamptz primary key,
  requests bigint not null check (requests >= 0)
);
