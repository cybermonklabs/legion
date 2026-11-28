import { describe, expect, test } from "bun:test";
import { printable, printableJson, printableLine } from "../runner-log";

// The rows of Go's TestPrintable (packages/envoy/internal/runnerlog/runnerlog_test.go), each with
// the text Go's runnerlog.Printable prints for it, so the two print one form. Go's row of a byte
// that is not UTF-8 has no row here: a string of this language holds UTF-16 code units, never a
// byte, and its nearest case, a lone surrogate, has its own row below.
describe("printable", () => {
  test("prints each row of Go's Printable as Go prints it", () => {
    const rows: readonly (readonly [name: string, text: string, printed: string])[] = [
      [
        "an ordinary path",
        "/checkout/.dispatch/architecture/api.md",
        "/checkout/.dispatch/architecture/api.md",
      ],
      ["a name that is not ASCII", "café.md", "café.md"],
      ["a line feed", "x\n::error::forged", "x\\n::error::forged"],
      ["a carriage return", "y\r::warning::forged", "y\\r::warning::forged"],
      ["a tab", "a\tb", "a\\tb"],
      ["an escape", "a\u001b[31mb", "a\\x1b[31mb"],
      ["a delete", "a\u007fb", "a\\x7fb"],
      ["a C1 next line", "a\u0085b", "a\\u0085b"],
      ["a command of the second form", "##[", "##\\x5b"],
      ["three number signs", "###[", "###\\x5b"],
      ["two commands of the second form", "##[##[", "##\\x5b##\\x5b"],
      ["a command of the second form inside a name", "x##[error]y", "x##\\x5berror]y"],
      ["a soft hyphen", "#\u00ad#[error]y", "#\\u00ad#[error]y"],
      ["a zero-width joiner", "#\u200d#[error]y", "#\\u200d#[error]y"],
      ["a byte order mark", "\ufeff::error::forged", "\\ufeff::error::forged"],
      ["a right-to-left override", "a\u202eb.md", "a\\u202eb.md"],
      ["a line separator", "x\u2028::error::y.md", "x\\u2028::error::y.md"],
      ["a paragraph separator", "x\u2029::error::y.md", "x\\u2029::error::y.md"],
    ];
    expect(rows.map(([name, text]) => [name, printable(text)])).toEqual(
      rows.map(([name, , printed]) => [name, printed])
    );
  });

  // strconv.Quote's other forms: the short escapes \a, \b, \f and \v, \xNN for the rest of C0,
  // and \UNNNNNNNN for a format character outside the basic plane.
  test("writes every escape as strconv.Quote writes it", () => {
    expect(printable("\u0000\u0007\b\f\u000b\u001f")).toBe("\\x00\\a\\b\\f\\v\\x1f");
    expect(printable("a\u{e0001}b")).toBe("a\\U000e0001b");
    expect(printable('a\u{1f642}b \\ "')).toBe('a\u{1f642}b \\ "');
  });

  test("writes a lone surrogate as its code unit", () => {
    expect(printable("a\ud800b\udc00c")).toBe("a\\ud800b\\udc00c");
  });
});

// A line whose first words are text the command did not write has no fixed prefix to start it,
// as each line runnerlog.Say writes has: a "::" at its start, after its leading white space, is
// written with its first ":" as \x3a, and anything else is printable's text.
describe("printableLine", () => {
  test("writes the first colon of a line that starts with :: as \\x3a", () => {
    const rows: readonly (readonly [text: string, printed: string])[] = [
      ["::error::x: refused", "\\x3a:error::x: refused"],
      ["  ::add-mask::x", "  \\x3a:add-mask::x"],
      ["\u00a0\u3000::stop-commands::t", "\u00a0\u3000\\x3a:stop-commands::t"],
      ["\t::error::x", "\\t::error::x"],
      ["\n::error::x", "\\n::error::x"],
      [":x and a::b", ":x and a::b"],
      ["CHECK_NOT_IN_FILES: ::error::x", "CHECK_NOT_IN_FILES: ::error::x"],
      ["##[group]x", "##\\x5bgroup]x"],
    ];
    expect(rows.map(([text]) => printableLine(text))).toEqual(rows.map(([, printed]) => printed));
  });
});

// JSON text that a JSON reader reads back as it was, with no raw character printable escapes and
// no "##[": JSON.stringify already escapes C0, and the rest is written as \uNNNN, the "[" of a
// "##[" included, as Go's JSON lines write it (`##\u005b`, cmd/ci-report and spec-check checks).
describe("printableJson", () => {
  test("escapes what JSON.stringify leaves, and reads back as the same value", () => {
    const value = {
      observed: "a\u007f\u0085\u00ad\u200d\u2028\u2029\u{e0001}##[group]b\n\r\u001b",
      "##[key": ["###[", "x##[##[y"],
    };
    const json = JSON.stringify(value, null, 2);
    const printed = printableJson(json);
    expect(printed).toBe(
      [
        "{",
        '  "observed": "a\\u007f\\u0085\\u00ad\\u200d\\u2028\\u2029\\udb40\\udc01##\\u005bgroup]b\\n\\r\\u001b",',
        '  "##\\u005bkey": [',
        '    "###\\u005b",',
        '    "x##\\u005b##\\u005by"',
        "  ]",
        "}",
      ].join("\n")
    );
    expect(JSON.parse(printed)).toEqual(value);
  });

  test("leaves JSON that holds none of them as it is", () => {
    const json = JSON.stringify({ landed_without_answer: "IDEMPOTENCY_KEY_COMMITTED", é: "🙂" });
    expect(printableJson(json)).toBe(json);
  });
});
