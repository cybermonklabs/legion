/**
 * Text Go's `strings.TrimSpace` empties: no characters at all, or only characters Go's
 * `unicode.IsSpace` reports - `\t`, `\n`, `\v`, `\f`, `\r`, a space, U+0085 and U+00A0, and the
 * Unicode `White_Space` property above Latin-1, which is U+1680, U+2000 to U+200A, U+2028,
 * U+2029, U+202F, U+205F and U+3000 (`IsSpace` in Go's `unicode/graphic.go`, reading the
 * `_White_Space` table of its `unicode/tables.go`).
 *
 * A client that must call text blank where a Go server does tests it against this pattern, never
 * against `trim`, which disagrees with Go on two characters.
 *
 * The set is written out because it is Go's and not JavaScript's: walked over every Unicode
 * scalar, `String.prototype.trim` and `strings.TrimSpace` differ on exactly two characters.
 * `trim` removes U+FEFF, which Go keeps, so `trim` calls U+FEFF blank where a Go server reads
 * content; Go removes U+0085, which `trim` keeps, so `trim` calls U+0085 content where a Go
 * server reads blank. `GO_TRIM_SPLIT` below is the one place that pair is written out, and the
 * lines that turn on it read it or cite this comment rather than repeating it. Its readers:
 * `dispatch artifact` (`packages/envoy-client/src/dispatch-commands/documents.ts`), against the
 * Dispatch server's blank content; a rating's `planReason` (`./legion-api.ts`, and the `legion`
 * tool's `plan_reason` in `packages/pi-shared/src/legion-phase-ops.ts`), against the daemon's;
 * and the Envoy plugin's `listenerRoleName` (`packages/pi-envoy/src/listener-role-name.ts`), which
 * trims a role's name as the Envoy listener trims a claim's.
 *
 * `packages/envoy/internal/dispatch/text/refs.go` records a split of the same class in another
 * pair of sets ("The whitespace is spelled out rather than written `\s`, which means another
 * set in each language: JavaScript's also matches a vertical tab and U+FEFF"). That pair is the
 * set both readers of references spell out, `[\t\n\f\r\p{Z}]`, and JavaScript's `\s`, which
 * differ in a vertical tab and U+FEFF.
 *
 * `packages/contracts/fixtures/go-unicode-isspace.json` holds the 25 scalars `unicode.IsSpace`
 * reports. This pattern is held to that list over every scalar by `goBlankPattern` > `calls
 * blank exactly the scalars the committed list holds` (`./go-blank.test.ts`), and Go's own
 * predicate by TestUnicodeIsSpaceReportsTheCommittedScalars in
 * `packages/envoy/internal/dispatch/text/whitespace_fixture_test.go`: a character either side
 * gains or loses reddens its test, and the list and this pattern are corrected together.
 */
export const goBlankPattern =
  /^[\t\n\v\f\r \u0085\u00A0\u1680\u2000-\u200A\u2028\u2029\u202F\u205F\u3000]*$/u;

/** The two characters on which `String.prototype.trim` and Go's `strings.TrimSpace` differ:
 * U+0085, blank to Go and content to `trim`, and U+FEFF, content to Go and blank to `trim`. */
export const GO_TRIM_SPLIT = ["\u0085", "\uFEFF"] as const;
