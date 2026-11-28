import { expect, test } from "bun:test";

import type { DisplayTree, SearchOwner, SearchResult } from "../../api/types";
import {
  firstHighlightTerm,
  groupResults,
  hitIsItsIssue,
  issueHitTitle,
  kindLabel,
  optionId,
  ownerWords,
  snippetCut,
  snippetFromSentence,
  snippetWithoutDirectives,
  stepActive,
  withoutDirectiveSyntax,
} from "./search-model";

/** An issue owner as the one flat owner record carries it: the route table leaves `kind` a plain
 *  string and makes every arm's own field optional, and an issue owner is the one naming `key`. */
type IssueOwner = SearchOwner & { kind: "issue" };

function result(
  id: string,
  owner: IssueOwner,
  kind: SearchResult["kind"] = "document"
): SearchResult {
  return {
    href: `/issues/${owner.key}`,
    id,
    kind,
    owner,
    rank: 1,
    snippet: "result",
  };
}

test("groups results by their first ranked owner while preserving server result order", () => {
  const legionTwo: IssueOwner = {
    key: "LEGION-2",
    kind: "issue",
    status: "done",
    title: "First issue",
  };
  const legionThree: IssueOwner = {
    key: "LEGION-3",
    kind: "issue",
    status: "todo",
    title: "Second issue",
  };
  const results = [
    result("document-2", legionTwo),
    result("comment-3", legionThree, "comment"),
    result("comment-2", legionTwo, "comment"),
  ];
  expect(groupResults(results)).toEqual([
    { owner: legionTwo, results: [results[0], results[2]] },
    { owner: legionThree, results: [results[1]] },
  ]);
});

test("names an issue owner by its key and title, and a project document by its project and name", () => {
  expect(ownerWords({ key: "LEGION-2", kind: "issue", status: "todo", title: "Orrery" })).toEqual({
    key: "LEGION-2",
    name: "Orrery",
  });
  expect(
    ownerWords({
      artifact_id: "document-2",
      kind: "document",
      name: "Navigation design",
      project: "CORE",
      slug: "navigation-design",
    })
  ).toEqual({ key: "CORE", name: "Navigation design" });
});

test("a hit that is the issue itself shows its title once: the marked snippet where it is the whole title", () => {
  const owner: IssueOwner = {
    key: "DEMO-30",
    kind: "issue",
    status: "in_progress",
    title: "API: rate limit headers",
  };
  const issue = result("DEMO-30", owner, "issue");
  expect(hitIsItsIssue(issue)).toBe(true);
  // Its other hits - a document, a comment, an ask, a message - name the issue they belong to.
  for (const kind of ["document", "comment", "ask", "message"]) {
    expect(hitIsItsIssue(result(`${kind}-1`, owner, kind))).toBe(false);
  }
  // The snippet is the whole title: it shows, with its marks.
  expect(issueHitTitle(issue, [{ mark: false, text: "API: rate limit headers" }])).toBeUndefined();
  // The headline cut the title to the window around the match: the whole title shows instead,
  // the word the window marked marked at its place in it (visual review n2, 13-filing-line).
  expect(
    issueHitTitle(issue, [
      { mark: false, text: "rate limit " },
      { mark: true, text: "headers" },
    ])
  ).toEqual([
    { mark: false, text: "API: rate limit " },
    { mark: true, text: "headers" },
  ]);
  // A window with spaces at its ends still stands in the title by its words.
  expect(
    issueHitTitle(issue, [
      { mark: false, text: " rate " },
      { mark: true, text: "limit" },
      { mark: false, text: " headers " },
    ])
  ).toEqual([
    { mark: false, text: "API: rate " },
    { mark: true, text: "limit" },
    { mark: false, text: " headers" },
  ]);
  // A window the title does not hold as written marks nothing.
  expect(issueHitTitle(issue, [{ mark: true, text: "rate-limit" }])).toEqual([
    { mark: false, text: "API: rate limit headers" },
  ]);
});

// Rule 1: no marker text (review N2, 13-filing-line). A snippet is a window of a body, and a
// window that starts or ends inside a Proof directive leaves its fence or its attributes cut,
// which the server's reader reads as no directive: they reach the snippet's tree as paragraph
// text, a soft break between them a space (`pmdoc/parse.go`). Their syntax comes out of the text,
// the words of a quoted value stay, and nothing else of the text - its Markdown is the tree's -
// is touched.
test("a snippet's text loses a cut directive's fence and attributes, and keeps the words of its values", () => {
  expect(withoutDirectiveSyntax('table. ::: :::plan_task{#t1 serves="A page is')).toBe(
    "table. A page is"
  );
  expect(
    withoutDirectiveSyntax(':::decision{#d2 title="One chip" by="alice"} We keep one chip. :::')
  ).toBe("One chip alice We keep one chip.");
  expect(withoutDirectiveSyntax(":::note[Why] We keep it.")).toBe("Why We keep it.");
  // A window that began inside the attributes starts with their tail, inside a value or not.
  expect(withoutDirectiveSyntax('serves="A page is"} The plan loads.')).toBe(
    "A page is The plan loads."
  );
  expect(withoutDirectiveSyntax('page is"} The plan loads.')).toBe("page is The plan loads.");
  // Prose with colons, braces or quotes and no directive is left as it is.
  for (const prose of ['Ratio 1:2, a {b} and "c".', "snake_case **stays** as written"]) {
    expect(withoutDirectiveSyntax(prose)).toBe(prose);
  }
});

test("a snippet tree loses a cut directive's syntax across its text nodes, its marks kept", () => {
  const tree: DisplayTree = {
    content: [
      {
        content: [
          { text: 'table. ::: :::plan_task{#t1 serves="', type: "text" },
          {
            marks: [{ attrs: { href: "/issues/CORE-1" }, type: "link" }],
            text: "A page",
            type: "text",
          },
          { text: " is ready once its ", type: "text" },
          { marks: [{ type: "searchHighlight" }], text: "plan", type: "text" },
          { text: " loads", type: "text" },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  expect(snippetWithoutDirectives(tree)).toEqual({
    content: [
      {
        content: [
          { text: "table. ", type: "text" },
          {
            marks: [{ attrs: { href: "/issues/CORE-1" }, type: "link" }],
            text: "A page",
            type: "text",
          },
          { text: " is ready once its ", type: "text" },
          { marks: [{ type: "searchHighlight" }], text: "plan", type: "text" },
          { text: " loads", type: "text" },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  });
  // A paragraph that was nothing but a closing fence keeps no empty text node.
  const closing: DisplayTree = {
    content: [{ content: [{ text: ":::", type: "text" }], type: "paragraph" }],
    type: "doc",
  };
  expect(snippetWithoutDirectives(closing)).toEqual({
    content: [{ content: [], type: "paragraph" }],
    type: "doc",
  });
  // A tree with no directive syntax is the very tree, so its drawing is the one it had.
  const plain: DisplayTree = {
    content: [{ content: [{ text: "Plain words.", type: "text" }], type: "paragraph" }],
    type: "doc",
  };
  expect(snippetWithoutDirectives(plain)).toBe(plain);
  expect(snippetWithoutDirectives(undefined)).toBeUndefined();
});

// A window the server cut can begin on the tail of the sentence before the match ("table. A page
// is…", 13-filing-line), which reads as a fragment (review A2): the line starts at the first word
// of the next sentence. A window that starts with a capital, starts on a matched word or a link,
// or holds nothing past the tail, starts where it does.
test("a cut snippet that begins on a sentence's tail starts at the next sentence's first word", () => {
  const paragraph = (...content: object[]): DisplayTree =>
    ({ content: [{ content, type: "paragraph" }], type: "doc" }) as DisplayTree;
  const link = { attrs: { href: "/issues/CORE-1" }, type: "link" };
  expect(
    snippetFromSentence(paragraph({ text: "table. A page is read once it is drawn", type: "text" }))
  ).toEqual(paragraph({ text: "A page is read once it is drawn", type: "text" }));
  // The tail is a text of its own, before a link: the link starts the line.
  expect(
    snippetFromSentence(
      paragraph(
        { text: 'route table." ', type: "text" },
        { marks: [link], text: "A page", type: "text" }
      )
    )
  ).toEqual(paragraph({ marks: [link], text: "A page", type: "text" }));
  for (const kept of [
    paragraph({ text: "Table. A page is read", type: "text" }),
    paragraph({ marks: [{ type: "searchHighlight" }], text: "table. A page", type: "text" }),
    paragraph({ text: "no sentence ends here", type: "text" }),
    paragraph({ text: "table. ", type: "text" }),
  ]) {
    expect(snippetFromSentence(kept)).toBe(kept);
  }
  expect(snippetFromSentence(undefined)).toBeUndefined();
});

// The server says where its headline window cut the text a snippet was read out of, on the
// snippet tree's doc (`pmdoc.SnippetCutStartAttr`, `pmdoc.SnippetCutEndAttr`), each end only where
// it was cut: no count of the snippet's own words can say it, since ts_headline counts the
// text-search parser's tokens. A tree that says nothing, and a hit with no tree, is the whole
// text, and nothing but `true` says an end was cut.
test("a snippet's cut is the ends its tree's doc says the server's window cut, and none where it says nothing", () => {
  const doc = (attrs?: object): DisplayTree =>
    ({
      ...(attrs === undefined ? {} : { attrs }),
      content: [
        { content: [{ text: "table. Draw each page and read", type: "text" }], type: "paragraph" },
      ],
      type: "doc",
    }) as DisplayTree;
  expect(snippetCut(doc({ cutEnd: true, cutStart: true }))).toEqual({ end: true, start: true });
  expect(snippetCut(doc({ cutStart: true }))).toEqual({ end: false, start: true });
  expect(snippetCut(doc({ cutEnd: true }))).toEqual({ end: true, start: false });
  expect(snippetCut(doc())).toEqual({ end: false, start: false });
  expect(snippetCut(doc({ cutEnd: "true", cutStart: 1 }))).toEqual({ end: false, start: false });
  expect(snippetCut(undefined)).toEqual({ end: false, start: false });
});

test("uses the first positive websearch term for document highlighting", () => {
  expect(firstHighlightTerm('"merge queue" -daemon')).toBe("merge");
  expect(firstHighlightTerm("-daemon OR astrolabe")).toBe("astrolabe");
  expect(firstHighlightTerm("-only")).toBeUndefined();
});

test("wraps active result navigation at either end of the result list", () => {
  expect(stepActive(0, -1, 3)).toBe(2);
  expect(stepActive(2, 1, 3)).toBe(0);
  expect(stepActive(0, 1, 0)).toBe(0);
});

test("labels result kinds and creates stable option IDs", () => {
  const document = result("document-2", {
    key: "LEGION-2",
    kind: "issue",
    status: "todo",
    title: "Issue",
  });

  expect(kindLabel("document")).toBe("doc");
  expect(optionId(document)).toBe("search-option-document-document-2");
});
