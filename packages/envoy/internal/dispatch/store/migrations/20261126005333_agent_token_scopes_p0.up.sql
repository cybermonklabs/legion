-- 20261126005333_agent_token_scopes_p0.up.sql — a personal agent token may hold `p0`.
--
-- INFRA-5637: a write that makes an issue's own priority 0 (P0) needs the scope `p0`. A person's
-- own sign-in holds it, and a personal token holds it only where its maker gave it, at the mint
-- or at a later change of its scopes (api/agent_tokens.go). The token's set lives in
-- agent_tokens.scopes (20261103070013_agent_token_scopes), whose check admits `person` and
-- `approve` alone, so the check gains `p0`. A token may hold `p0` alone, or beside the others;
-- `approve` still needs `person`.
--
-- The check is dropped and added again in one statement, so no moment passes with the column
-- unchecked, and the new one admits everything the old one did. Its name stays
-- agent_tokens_scopes_check, which api's TestNoMintHoldsLegionStatus reads. Widening it is safe
-- beside the release before this migration, which reads the column without asking what words it
-- may hold. That release writes `p0` in one case, while the release rolls: a mint that names no
-- scopes gives the new token its caller's whole set (its callerScopes), so a mint, by a token that
-- holds `p0`, that reaches a server of that release gives the new token `p0` too, and the wider
-- check takes it. No credential gets a scope its maker does not hold, and the window ends when the
-- last server of that release stops (internal/dispatch/store/README.md).
--
-- The column's default stays `array['person','approve']`, and this release's mint that names no
-- scopes gives every scope its maker holds but `p0`. No row is backfilled and no row is rewritten,
-- so no stored token gains `p0`; a person gives it to a token in Settings. agent_read_tokens' check
-- does not change: a read-only token holds `read`, and `person` where it reads as its person,
-- never `p0`.
--
-- The add records the check in the catalog and scans no stored row (`not valid`), which is the
-- rule for a constraint a migration adds to a table that already has rows
-- (packages/envoy/cmd/dispatch/README.md): both subcommands are catalog-only (`drop constraint`
-- without `cascade`, `add constraint … check … not valid`), so the statement takes ACCESS
-- EXCLUSIVE on agent_tokens for the catalog write alone, under the runner's lock_timeout, and its
-- hold does not grow with the table. The check still refuses every row written or updated after
-- it. That no stored row would violate it is what the census beside this file shows, and no
-- release validates it: the check it replaces is narrower, so every row the table holds already
-- satisfies the new one. Its identifier is the UTC moment it was written, as `migrationid`
-- requires of every migration past the numbered series, which closed at 0161
-- (cmd/dispatch/README.md, "Database migrations").
alter table agent_tokens
    drop constraint agent_tokens_scopes_check,
    add constraint agent_tokens_scopes_check
        check (scopes <@ array['person','approve','p0'] and ('approve' <> all(scopes) or 'person' = any(scopes))) not valid;
