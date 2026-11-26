import recorded from "./__fixtures__/graph-edit.json";

/** The instant each recorded time is read as. */
const RECORDED_AT = "2026-01-01T00:00:00Z";

/**
 * The server's own answers the graph-edit cases read, `__fixtures__/graph-edit.json`, with each
 * time as one RFC 3339 instant. The Go test that records the file writes every time as `<time>`,
 * so the file is byte for byte what each run records (`graph_edit_web_fixtures_test.go`, which CI
 * compares with the file), and the file is never edited by hand; the page parses each time it
 * draws or orders by (`lib/timestamps.ts`), which a placeholder is not. Only a value that is a
 * whole `<time>` is read so: a time inside a refusal's words stays as recorded.
 */
export const fixture: typeof recorded = JSON.parse(
  JSON.stringify(recorded).replaceAll('"<time>"', JSON.stringify(RECORDED_AT))
);
