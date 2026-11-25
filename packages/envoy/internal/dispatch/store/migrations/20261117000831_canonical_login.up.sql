-- 20261117000831_canonical_login.up.sql — model.CanonicalLogin, spelled once in SQL.
--
-- A statement that compares a stored login with people.email has to put the stored
-- login in the form people.email holds, which is model.CanonicalLogin's: strings.TrimSpace, which
-- trims every character Unicode gives the White_Space property, then fold.ASCII, which lowers A-Z
-- and nothing else. lower(btrim(x)), which two readers spelled it as, is neither: btrim trims the
-- space alone, so an owner wrapped in a tab or a no-break space finds no people row, and lower
-- folds Unicode, so U+212A KELVIN SIGN reads as another person's k. canonical_login is that form,
-- and the readers that compare a stored owner with people.email call it (store.revokeAddressTokensSQL,
-- api.personalTokenActor). store.TestCanonicalLoginIsModelCanonicalLogin holds it to the Go function
-- over every White_Space character and the letters the two folds disagree on.
--
-- The trimmed set is Unicode's White_Space property as Go's unicode.IsSpace reads it: U+0009-000D,
-- U+0020, U+0085, U+00A0, U+1680, U+2000-200A, U+2028, U+2029, U+202F, U+205F, U+3000. The
-- function is immutable, so an index may use it. It creates a function and touches no row.
--
-- agent_tokens_canonical_owner (migration 20261119020200) is an index on canonical_login(owner),
-- and an expression index keeps the values it computed when its immutable function is replaced: a
-- later migration that redefines canonical_login must run
-- `REINDEX INDEX CONCURRENTLY agent_tokens_canonical_owner` in the same release.
--
-- This header was edited in place three times after the migration was pushed, its statement
-- unchanged; the lines below name the texts before each edit, so a database that applied any of
-- them still boots (the migrator reads them as a set, after the first). They follow the order of
-- the latest pushed text (a60be5e8c9's, which named the first alone): the first text, that one,
-- then 0b1a91ae29's, which that edit had left out.
-- replaces: 20261117000831_canonical_login.up.sql sha256:0a29bb5beeda046999c2058366050535556e100c2419c681172d80f285ede617
-- replaces: 20261117000831_canonical_login.up.sql sha256:f53f70b610cae2141bf633d77fb3cd8ee863fea01343a34264608f75418ac3c8
-- replaces: 20261117000831_canonical_login.up.sql sha256:dc8cf936efc08e9c640aa9157067624195262980d46c5059e38750efa4dfe7d1
create function canonical_login(login text) returns text
  language sql immutable strict parallel safe
  return translate(
    btrim(login, E'\t\n\x0b\x0c\r ' || chr(133) || chr(160) || chr(5760)
      || chr(8192) || chr(8193) || chr(8194) || chr(8195) || chr(8196) || chr(8197) || chr(8198)
      || chr(8199) || chr(8200) || chr(8201) || chr(8202)
      || chr(8232) || chr(8233) || chr(8239) || chr(8287) || chr(12288)),
    'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz');
