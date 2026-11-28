/**
 * The upload matrix: every name an upload is sent under crossed with every path it is read from.
 * It is the matrix the tester ran through the head's own command under a live AskGate record
 * (dispatch://INFRA-4071/artifact/tester-tools-r20-tgz-b64-txt, gate16/matrix.py), with the four
 * paths review 6012684631 added at e0d2289b, and two names on which Go's `strings.TrimSpace` and
 * JavaScript's `trim` part (`GO_TRIM_SPLIT`, packages/contracts/src/go-blank.ts): `a.md` and
 * U+0085, which Dispatch stores as `a.md`, and `a.md` and U+FEFF, which it stores as sent.
 *
 * Two tests read it, so the two cannot drift apart: the CLI's ("on the tester's matrix",
 * src/__tests__/dispatch-execute.test.ts), which holds that AskGate and the type `dispatch
 * artifact` declares agree on each pair, and AskGate's pre-check's ("the pre-check holds every
 * upload the CLI's gate holds", packages/pi-envoy/extensions/askgate/askgate.test.ts), which
 * holds that the pre-check lets no pair the CLI holds run unread.
 */

/** The names an upload is sent under. */
export const UPLOAD_MATRIX_NAMES: readonly string[] = [
  "a.md",
  "a.MD",
  "a.Markdown",
  "a.md.1",
  "a.md.2026.11",
  "a.MARKDOWN.0",
  "a.mdown",
  "a.mkd",
  "a.txt",
  "a.txt.1",
  "a.json",
  "a.jsonl",
  "a.c++",
  "a.tar.gz",
  "a.данные",
  "a.md\u200b",
  "a.\uff4d\uff44",
  "a.m d",
  "a.",
  "a.md/",
  "a v1.2",
  "a",
  `a.${"x".repeat(32)}`,
  `a.${"x".repeat(33)}`,
  "a.md ",
  " a.md",
  "a.\u0661",
  "a.md.\u0661",
  "a.md..1",
  "a.md.1.",
  "dir/a.md",
  "a.md\u0085",
  "a.md\uFEFF",
];

/**
 * The paths an upload is read from, each relative to the working directory the test runs it in.
 * Each test adds `<cwd>/notes.md/.`, the absolute path of its own working directory's notes.md.
 * `notes.md/`, `notes.md/.` and `notes.md/x/..` resolve to notes.md, `../notes.md/` to the
 * notes.md one directory up, and `a/b/..` and `notes.md/..` to the directories a and the working
 * directory itself, which no upload can read.
 */
export const UPLOAD_MATRIX_PATHS: readonly string[] = [
  "x.md",
  "x.txt",
  "x.MD",
  "x.mkd",
  "dir/.md",
  "notes.md/",
  "notes.md/.",
  "notes.md/x/..",
  ".md",
  "cases.txt",
  "../notes.md/",
  "a/b/..",
  "notes.md/..",
];
