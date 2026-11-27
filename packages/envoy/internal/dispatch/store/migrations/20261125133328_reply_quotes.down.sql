-- 20261125133328_reply_quotes.down.sql
--
-- The rollback an operator runs by hand: the four columns. It loses the words each reply quoted
-- and which of their matches they were; each reply keeps the block it names (replies_to_block),
-- so it reads as a reply to the whole block. Run it only against a build that reads neither
-- column.
alter table messages drop column replies_to_quote, drop column replies_to_occurrence;
alter table comments drop column replies_to_quote, drop column replies_to_occurrence;
