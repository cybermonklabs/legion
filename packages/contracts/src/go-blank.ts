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
 * and `goTrimSpace` below. It trims a role's name as the Envoy listener trims a claim's, in
 * `packages/pi-envoy/extensions/envoy.ts`'s session-role read and the Envoy plugin's
 * `listenerRoleName` (`packages/pi-envoy/src/listener-role-name.ts`), and each value Dispatch
 * trims with `strings.TrimSpace` as Dispatch trims it: an upload's name (`uploadStoredName`,
 * `./dispatch-tools.ts`) for its extension (`uploadExtensionOf`), for the held-name check
 * (`artifactUnderName`, `packages/envoy-client/src/dispatch-commands/upload-checks.ts`) and for
 * AskGate's rules check and breaker (`uploadName` and `callKeys`,
 * `packages/pi-envoy/extensions/askgate/`); the host session id the `dispatch` commands send
 * (`packages/envoy-client/src/dispatch-commands/`), which Dispatch trims on the routes that read
 * it with `strings.TrimSpace` (`dispatch open-asks`, `follow` and `read --message`) and refuses
 * as no session id when it holds whitespace on the routes that read it with `requestSessionID`
 * (`dispatch answers` and a picture upload), so an id blank to Go is no session id before any
 * request; a GitHub repository as `refs.ExternalRepo` reads its owner and name (`canonicalRepo`,
 * `./repo.ts`), for a reference `dispatch check` sends (`canonicalExternalIssueRef`,
 * `packages/envoy-client/src/dispatch-commands/check.ts`) and for the default repository a
 * command takes from the working directory's git remote (`parseGitHubRemoteUrl`,
 * `packages/envoy-client/src/dispatch-cwd.ts`, whose own trim is of the remote URL git prints,
 * which is never sent); an ask's question and an ask block's text (`questionAsk` and the block
 * scan, `packages/envoy-client/src/askgate-new-asks.ts`); and the reasons the tool specs refuse
 * as blank before they send (`./dispatch-tools.ts`).
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

/**
 * `text` with the characters `goBlankPattern` holds cut from both ends, as Go's
 * `strings.TrimSpace` cuts them, which is not what `trim` cuts (`GO_TRIM_SPLIT`). Each end is
 * walked one UTF-16 unit at a time and each unit tested alone, so the pattern never sees more
 * than one character and the time is linear in the blanks cut. Every character of the set is a
 * single unit, and half a surrogate pair is none of them.
 */
export function goTrimSpace(text: string): string {
  let start = 0;
  let end = text.length;
  while (start < end && goBlankPattern.test(text.charAt(start))) start += 1;
  while (end > start && goBlankPattern.test(text.charAt(end - 1))) end -= 1;
  return text.slice(start, end);
}
