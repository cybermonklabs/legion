import { expect, test } from "bun:test";
import { render } from "@testing-library/react";

import { issueStatuses, statusText } from "../features/project/board-model";
import { pageStatusDoneText, pageStatusIdleText, pageStatusProgressText } from "../theme/classes";
import { StatusGlyph } from "./StatusGlyph";
import { statusGlyph, statusGlyphElement, statusTone } from "./status-glyph";

/** Each group's shapes, as tag, fill and stroke: a tick for done, a ring with its left half
 *  filled for the work that moves, an empty ring for every other status. */
const TICK = [["path", "none", "currentColor"]];
const HALF = [
  ["circle", "none", "currentColor"],
  ["path", "currentColor", null],
];
const RING = [["circle", "none", "currentColor"]];

/** Every status Dispatch knows, with the shape and the colour of its group. */
const GROUPS: readonly [
  status: string,
  glyph: string,
  shapes: (string | null)[][],
  tone: string,
][] = [
  ["done", "✓", TICK, pageStatusDoneText],
  ["in_progress", "◐", HALF, pageStatusProgressText],
  ["testing", "◐", HALF, pageStatusProgressText],
  ["needs_review", "◐", HALF, pageStatusProgressText],
  ["triage", "○", RING, pageStatusIdleText],
  ["backlog", "○", RING, pageStatusIdleText],
  ["icebox", "○", RING, pageStatusIdleText],
  ["todo", "○", RING, pageStatusIdleText],
  ["retro", "○", RING, pageStatusIdleText],
];

/** What a drawn mark shows a reader and assistive technology. */
function markOf(svg: Element | null | undefined) {
  return {
    glyph: svg?.getAttribute("data-status-glyph"),
    hidden: svg?.getAttribute("aria-hidden") ?? null,
    name: svg?.getAttribute("aria-label") ?? null,
    role: svg?.getAttribute("role") ?? null,
    shapes: Array.from(svg?.children ?? [], (shape) => [
      shape.tagName.toLowerCase(),
      shape.getAttribute("fill"),
      shape.getAttribute("stroke"),
    ]),
    sized: svg?.classList.contains("size-[1em]"),
    tag: svg?.tagName.toLowerCase(),
    text: svg?.textContent,
    tone: [pageStatusDoneText, pageStatusIdleText, pageStatusProgressText].filter((tone) =>
      svg?.classList.contains(tone)
    ),
  };
}

test("the groups cover every status Dispatch knows", () => {
  expect(GROUPS.map(([status]) => status).sort()).toEqual([...issueStatuses].sort());
});

// C2: the React component and the DOM builder draw one mark from one module's data.
test("each status draws its group's shape and colour, named by its words, in React and in the DOM", () => {
  for (const [status, glyph, shapes, tone] of GROUPS) {
    const expected = {
      glyph,
      hidden: null,
      name: statusText(status),
      role: "img",
      shapes,
      sized: true,
      tag: "svg",
      text: "",
      tone: [tone],
    };
    expect([status, statusGlyph(status), statusTone(status)]).toEqual([status, glyph, tone]);
    const view = render(<StatusGlyph status={status} />);
    try {
      expect(view.container.children).toHaveLength(1);
      expect({ status, ...markOf(view.container.firstElementChild) }).toEqual({
        status,
        ...expected,
      });
      expect(view.getByRole("img", { name: statusText(status) })).toBe(
        view.container.firstElementChild as HTMLElement
      );
    } finally {
      view.unmount();
    }
    const built = statusGlyphElement(document, status);
    expect(built.namespaceURI).toBe("http://www.w3.org/2000/svg");
    expect({ status, ...markOf(built) }).toEqual({ status, ...expected });
    // One markup for both, so the editor's titles and the rows draw exactly what React draws.
    const drawn = render(<StatusGlyph status={status} />);
    try {
      expect(built.outerHTML).toBe(drawn.container.innerHTML);
    } finally {
      drawn.unmount();
    }
  }
});

test("a status Dispatch does not know draws the empty ring, named by the status as written", () => {
  const view = render(<StatusGlyph status="blocked" />);
  try {
    expect(markOf(view.container.firstElementChild)).toMatchObject({
      glyph: "○",
      name: "blocked",
      role: "img",
      shapes: RING,
      tone: [pageStatusIdleText],
    });
  } finally {
    view.unmount();
  }
  expect(markOf(statusGlyphElement(document, "blocked"))).toMatchObject({
    glyph: "○",
    name: "blocked",
    shapes: RING,
  });
});

// The issue page's status pill writes the status's words beside its mark, so the mark there is
// hidden from assistive technology rather than read twice.
test("a decorative mark is hidden from assistive technology and names nothing", () => {
  const view = render(<StatusGlyph decorative status="in_progress" />);
  try {
    expect(markOf(view.container.firstElementChild)).toMatchObject({
      glyph: "◐",
      hidden: "true",
      name: null,
      role: null,
      shapes: HALF,
    });
  } finally {
    view.unmount();
  }
  expect(markOf(statusGlyphElement(document, "in_progress", { decorative: true }))).toMatchObject({
    hidden: "true",
    name: null,
    role: null,
  });
});
