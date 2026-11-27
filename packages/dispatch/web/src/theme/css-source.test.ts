import { describe, expect, test } from "bun:test";

import {
  declarationsWithTrailingComment,
  ruleDeclarations,
  templateExpressions,
  topLevelBlocks,
  withoutComments,
} from "./css-source";

describe("css-source", () => {
  test("keeps a /* inside a string intact without cutting the rule after it", () => {
    const css = [
      '.quoted { content: "/* keep"; }',
      ".after { color: #0f172b; } /* slate-900 */",
      "/* real comment */",
      ".escaped { content: 'a \\' /* still string'; color: #f8fafc; /* slate-50 */ }",
    ].join("\n");

    const stripped = withoutComments(css);
    expect(stripped).toContain('.quoted { content: "/* keep"; }');
    expect(stripped).toContain(".after { color: #0f172b; }");
    expect(stripped).toContain("'a \\' /* still string'");
    expect(stripped).not.toContain("real comment");
    expect(stripped).not.toContain("slate-900");

    expect(declarationsWithTrailingComment(css)).toEqual([
      { comment: undefined, property: "content", value: '"/* keep"' },
      { comment: undefined, property: "color", value: "#0f172b" },
      { comment: undefined, property: "content", value: "'a \\' /* still string'" },
      { comment: "slate-50", property: "color", value: "#f8fafc" },
    ]);
  });

  test("removes a comment between two rules and counts no colour inside it", () => {
    const css = [
      ".first { color: #0f172b; /* slate-900 */ }",
      "/* between rules: mentions #123456 and rgb(1, 2, 3) */",
      ".second { background: #f8fafc; /* slate-50 */ }",
    ].join("\n");

    const stripped = withoutComments(css);
    expect(stripped).not.toContain("#123456");
    expect(stripped).not.toContain("rgb(1, 2, 3)");
    expect(stripped).toContain(".first");
    expect(stripped).toContain(".second");

    expect(declarationsWithTrailingComment(css)).toEqual([
      { comment: "slate-900", property: "color", value: "#0f172b" },
      { comment: "slate-50", property: "background", value: "#f8fafc" },
    ]);
  });

  test("reads a declaration with its trailing comment and ignores comments on other lines", () => {
    const css = [
      "@media (prefers-color-scheme: dark) {",
      "  :root {",
      "    /* leading comment with #abcdef */",
      "    color: /* inline #112233 */ #f1f5f9; /* slate-100 */",
      "    font-weight: 600;",
      "    /* comment on next line is not trailing */",
      "    background: #020618; /* slate-950 */",
      "  }",
      "}",
    ].join("\n");

    expect(declarationsWithTrailingComment(css)).toEqual([
      { comment: "slate-100", property: "color", value: "#f1f5f9" },
      { comment: undefined, property: "font-weight", value: "600" },
      { comment: "slate-950", property: "background", value: "#020618" },
    ]);
  });

  test('keeps a ; inside url("data:…;base64,…") and unquoted url(…) inside one declaration', () => {
    const css = [
      ".icon {",
      '  mask: url("data:image/svg+xml;base64,PHN2Zy8+") center / 100% no-repeat; /* tick-mask */',
      "  background-image: url(data:image/svg+xml;base64,QUJD/*keep*/); /* unquoted */",
      "  color: #0f172b; /* slate-900 */",
      "}",
    ].join("\n");

    const stripped = withoutComments(css);
    expect(stripped).toContain('url("data:image/svg+xml;base64,PHN2Zy8+")');
    expect(stripped).toContain("url(data:image/svg+xml;base64,QUJD/*keep*/)");
    expect(stripped).not.toContain("tick-mask");

    expect(declarationsWithTrailingComment(css)).toEqual([
      {
        comment: "tick-mask",
        property: "mask",
        value: 'url("data:image/svg+xml;base64,PHN2Zy8+") center / 100% no-repeat',
      },
      {
        comment: "unquoted",
        property: "background-image",
        value: "url(data:image/svg+xml;base64,QUJD/*keep*/)",
      },
      { comment: "slate-900", property: "color", value: "#0f172b" },
    ]);
  });

  test("keeps a brace inside a string from closing or opening a block", () => {
    const css = [
      ".brace {",
      '  content: "}";',
      "  --open: '{';",
      "  color: #0f172b; /* slate-900 */",
      "}",
    ].join("\n");

    expect(declarationsWithTrailingComment(css)).toEqual([
      { comment: undefined, property: "content", value: '"}"' },
      { comment: undefined, property: "--open", value: "'{'" },
      { comment: "slate-900", property: "color", value: "#0f172b" },
    ]);
  });

  test("splits a sheet into its top-level blocks past a brace or a /* inside a string or a url", () => {
    const css = [
      '@import "a;b.css";',
      '/* { */ .a::after { content: "}"; }',
      "@media (min-width: 1px) { .b { content: '{'; } }",
      ".c { background: url(data:image/svg+xml;utf8,<svg/>}) no-repeat; }",
      '.d { content: "/*"; } .e { color: red; }',
    ].join("\n");

    expect(topLevelBlocks(css)).toEqual([
      { body: ' content: "}"; ', prelude: ".a::after" },
      { body: " .b { content: '{'; } ", prelude: "@media (min-width: 1px)" },
      { body: " background: url(data:image/svg+xml;utf8,<svg/>}) no-repeat; ", prelude: ".c" },
      { body: ' content: "/*"; ', prelude: ".d" },
      { body: " color: red; ", prelude: ".e" },
    ]);
  });

  test("reads the declarations inside one rule, past a ; or a } in a string, the last with no ;", () => {
    const body =
      ' font-family: "A;}"; /* c */ src: url(data:font/woff2;base64,AAAA) format("woff2"); unicode-range: U+0100 ';

    expect(ruleDeclarations(body)).toEqual([
      { comment: "c", property: "font-family", value: '"A;}"' },
      {
        comment: undefined,
        property: "src",
        value: 'url(data:font/woff2;base64,AAAA) format("woff2")',
      },
      { comment: undefined, property: "unicode-range", value: "U+0100" },
    ]);
  });

  test("balances a template expression against the braces and the templates inside it", () => {
    // biome-ignore-start lint/suspicious/noTemplateCurlyInString: the fixture is source text, whose template literals these are.
    const source = [
      "// `${in} a comment`",
      'const plain = "${in} a string";',
      "const braces = `${f({ a })}-500`;",
      "const nested = `${a ? `x-${b}` : c}`;",
      "const address = `https://example.test/${path}`;",
    ].join("\n");
    const found = templateExpressions(source).map(({ start, end }) => source.slice(start, end));

    expect(found).toEqual(["${f({ a })}", "${a ? `x-${b}` : c}", "${b}", "${path}"]);
    // biome-ignore-end lint/suspicious/noTemplateCurlyInString: the fixture is source text, whose template literals these are.
  });
});
