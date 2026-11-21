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
create function canonical_login(login text) returns text
  language sql immutable strict parallel safe
  return translate(
    btrim(login, E'\t\n\x0b\x0c\r ' || chr(133) || chr(160) || chr(5760)
      || chr(8192) || chr(8193) || chr(8194) || chr(8195) || chr(8196) || chr(8197) || chr(8198)
      || chr(8199) || chr(8200) || chr(8201) || chr(8202)
      || chr(8232) || chr(8233) || chr(8239) || chr(8287) || chr(12288)),
    'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz');
