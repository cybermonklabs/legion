import { expect, test } from "bun:test";

import { stripComments } from "./strip-comments";

/** `width` spaces: what a comment of that many characters leaves on its line. */
const blank = (width: number) => " ".repeat(width);

test("a comment's characters become spaces and its newlines stay", () => {
  const source = [
    "const a = 1; // one",
    "/* two",
    "   lines */ const b = 2;",
    "/** doc */",
    "const c = 3;",
  ].join("\n");
  expect(stripComments(source, "probe.ts")).toBe(
    [
      `const a = 1; ${blank(6)}`,
      blank(6),
      `${blank(11)} const b = 2;`,
      blank(10),
      "const c = 3;",
    ].join("\n")
  );
});

test("a string holding `/*`, `//` and `*/` is code, and the comments around it are not", () => {
  const source = [
    'const accept = "image/*";',
    'const home = "https://example.com/inbox"; const after = 1;',
    'const close = "*/"; // a real comment',
    "/** A later doc comment, whose close the old pattern ran to from `image/*`. */",
    "const single = 'a // b /* c */';",
  ].join("\n");
  expect(stripComments(source, "probe.ts")).toBe(
    [
      'const accept = "image/*";',
      'const home = "https://example.com/inbox"; const after = 1;',
      `const close = "*/"; ${blank(17)}`,
      blank(78),
      "const single = 'a // b /* c */';",
    ].join("\n")
  );
});

test("a template literal holding them is code, and a comment in its expression is a comment", () => {
  const source = [
    "const path = `/x/*/y // z */`;",
    `const both = \`a /* b \${count /* real */} // c */ d\`;`,
    "const lines = `one //",
    "two */ three`;",
  ].join("\n");
  expect(stripComments(source, "probe.ts")).toBe(
    [
      "const path = `/x/*/y // z */`;",
      `const both = \`a /* b \${count ${blank(10)}} // c */ d\`;`,
      "const lines = `one //",
      "two */ three`;",
    ].join("\n")
  );
});

test("a regular-expression literal holding them is code, and a division is not one", () => {
  const source = [
    "const opener = /\\/\\*|\\/\\//g; const after = 1;",
    "const glob = /[/*]+/; // a real comment",
    "const half = total / 2; // another",
    "const closer = /\\*\\//;",
  ].join("\n");
  expect(stripComments(source, "probe.ts")).toBe(
    [
      "const opener = /\\/\\*|\\/\\//g; const after = 1;",
      `const glob = /[/*]+/; ${blank(17)}`,
      `const half = total / 2; ${blank(10)}`,
      "const closer = /\\*\\//;",
    ].join("\n")
  );
});

test("JSX text holding `//` is text, and a comment in braces is a comment", () => {
  const source = [
    "export const Link = () => (",
    '  <a href="https://example.com/a">',
    "    see https://example.com/b // or /* this */",
    "    {/* a real comment */}",
    "  </a>",
    ");",
  ].join("\n");
  expect(stripComments(source, "probe.tsx")).toBe(
    [
      "export const Link = () => (",
      '  <a href="https://example.com/a">',
      "    see https://example.com/b // or /* this */",
      `    {${blank(20)}}`,
      "  </a>",
      ");",
    ].join("\n")
  );
});

test("a `.ts` file reads a generic arrow function as one, not as JSX", () => {
  const source = "const same = <T>(value: T): T => value; // a real comment";
  expect(stripComments(source, "probe.ts")).toBe(
    `const same = <T>(value: T): T => value; ${blank(17)}`
  );
});
