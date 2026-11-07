/**
 * `@legion/proof-editor/mermaid`: the DOM half of a Mermaid diagram, for Dispatch's Markdown bodies
 * and for the editor's mermaid code blocks alike. It sanitizes the markup either renderer returns,
 * mounts the result in a shadow root so its styles stay inside it, and declares the contract by
 * which the editor asks its host to draw a diagram. It imports nothing from upstream, so a page
 * imports it without beautiful-mermaid or ELK; the render itself runs in the host's Web Worker.
 *
 * One thing to know before writing a unit row of the `"html"` parse: happy-dom 20.14.0's HTML
 * parser compares a `</style>` end tag with the element's upper-case `tagName`, which an SVG
 * element carries in lower case, so an svg `<style>` loses its text and every element after it.
 * A Mermaid drawing opens with its `<style>`, so a row under happy-dom reads markup with that
 * element taken out, and the whole drawing is proven by a row in a browser.
 */

const SVG_NAMESPACE = "http://www.w3.org/2000/svg";
/** The namespace a browser lays a `foreignObject`'s content out in, whatever namespace the parse
 *  that read the markup assigned it. */
const XHTML_NAMESPACE = "http://www.w3.org/1999/xhtml";

/** Which parser reads the markup. There is no default, so every call site says which renderer's
 *  markup it holds and none inherits the wrong one.
 *  - `"xml"`: beautiful-mermaid's markup (`@legion/proof-editor/mermaid-render`), an XML
 *    document, read by the `image/svg+xml` parser and checked for a `parsererror`.
 *  - `"html"`: upstream Mermaid's markup, read the way the browser mounts it. Mermaid serializes
 *    a `foreignObject`'s content with an HTML serializer, so a label holding a void `<br>` or
 *    `<img>` is not well-formed XML and an XML parse of it fails outright. An HTML parse reports
 *    no `parsererror` node, so the root is tested by what the body holds instead. */
export type MermaidParse = "xml" | "html";

/** Which allowlist a node is matched against and which namespace it is kept in: the drawing, or
 *  the XHTML subtree a kept `foreignObject` opens. */
type Subtree = "svg" | "xhtml";

const NAMESPACES: Record<Subtree, string> = {
  svg: SVG_NAMESPACE,
  xhtml: XHTML_NAMESPACE,
};

/** The elements the two renderers emit, measured over one diagram of each type each of them
 *  draws — beautiful-mermaid's six (flowchart, state, sequence, class, ER, xychart) and every
 *  type Mermaid registers, drawn with `htmlLabels: false` — plus the text and description
 *  elements an SVG may carry. Anything else is removed with its subtree. An XML name is
 *  case-sensitive, so an element matches by its exact local name: `<RECT>` is no `<rect>`. An
 *  HTML parse has already folded a name to the one the browser gives it, so the same exact match
 *  reads `<FOREIGNOBJECT>` as `foreignObject` there, as a browser does.
 *
 *  Mermaid is what the last eight are for:
 *  - `linearGradient` and `stop`: the gradient 13 of its drawings paint a shape with, among them
 *    flowchart, class, ER, state and sankey, whose links are gradients and vanish without them.
 *  - `filter` and `feDropShadow`: the drop shadow 12 of them carry, a sequence diagram's among
 *    them.
 *  - `clipPath` (one drawing) and `switch` (one).
 *  - `symbol`: defined by c4 and sequence. No `<use>` of any measured drawing references one, so
 *    it is admitted for the Mermaid version that does; it costs nothing, because `<use>` and
 *    every `href` stay removed, so nothing a `symbol` holds can be drawn or fetched.
 *  - `foreignObject`: where journey and eventmodeling put their labels, which are HTML. */
const ALLOWED_ELEMENTS: ReadonlySet<string> = new Set([
  "svg",
  "defs",
  "g",
  "marker",
  "path",
  "line",
  "polyline",
  "polygon",
  "rect",
  "circle",
  "ellipse",
  "text",
  "tspan",
  "title",
  "desc",
  "style",
  "linearGradient",
  "stop",
  "filter",
  "feDropShadow",
  "clipPath",
  "switch",
  "symbol",
  "foreignObject",
]);

/** The elements a kept `foreignObject` may hold: Mermaid's label markup, and nothing else. This
 *  list is load-bearing, because an author's own markup is not escaped on this path —
 *  eventmodeling inserts its label as HTML, so an author's `<p>` reaches the subtree whole. No
 *  sanitizer of Mermaid's own ran over it first: in the worker that draws it, `document` is the
 *  shim's linkedom document, which defines no `implementation`, so DOMPurify's `isSupported` is
 *  false and its `sanitize` answers the markup it was handed, unchanged. This allowlist and the
 *  attribute rules below it are therefore the only things that take a script, a handler, an
 *  `<img>` or an `<a href>` out of a label. Nobody may widen either in the belief that Mermaid
 *  already filtered what reaches them.
 *
 *  A node here is matched by local name whatever namespace the parse gave it: Mermaid writes
 *  `xmlns="http://www.w3.org/1999/xhtml"` on a `foreignObject`'s own child only, and the
 *  attribute list below drops that attribute, while the same drawing made under another DOM
 *  carries no `xmlns` at all. Each kept node is then rebuilt in the XHTML namespace. */
const ALLOWED_XHTML_ELEMENTS: ReadonlySet<string> = new Set(["div", "span", "b", "br", "code"]);

/** The attributes a node inside a `foreignObject` may carry: the two Mermaid's labels use.
 *  Everything else goes, its `xmlns` and any `href` with it. The rules that remove what executes
 *  or fetches still apply to these two, so a `style` holding `url(https://…)` goes here as it
 *  goes anywhere else. */
const ALLOWED_XHTML_ATTRIBUTES: ReadonlySet<string> = new Set(["class", "style"]);

/**
 * Turns the markup a renderer returns into an `<svg>` of `document`, sanitized by
 * `sanitizeMermaidSvg` rather than by upstream's private `sanitizeSvg`, with the editor's
 * accessible name. `parse` names the renderer's markup, as it does there. Markup that is not an
 * SVG document throws `Error("Invalid SVG output")`.
 */
export function mermaidSvgElement(
  markup: string,
  document: Document,
  parse: MermaidParse
): SVGSVGElement {
  const svg = sanitizeMermaidSvg(markup, document, parse);
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", "Rendered Mermaid diagram");
  return svg;
}

/** A drawn source: the sanitized diagram (shared; a consumer inserts a clone, as `showMermaidSvg`
 *  does), the full status text of a refusal, or `unavailable` when no renderer can draw the source
 *  (the host's worker failed to start or load), so the block shows its source as code, with no
 *  status line. */
export type MermaidDrawing = { svg: SVGSVGElement } | { error: string } | { unavailable: true };

/** Draws `source` for the editor: calls `draw` at most once and never after the returned cancel
 *  runs — synchronously inside the call when the drawing is cached or no renderer can run
 *  (`unavailable`), later when the renderer answers or fails. Returns the cancel. */
export type MermaidDrawer = (source: string, draw: (drawing: MermaidDrawing) => void) => () => void;

/**
 * Mounts `svg` (cloned into `host`'s document) with a `<style>` holding `sheet` in `host`'s open
 * shadow root, replacing whatever the root held, so neither the diagram's own `<style>` nor
 * `sheet` reaches the page and the page's rules do not reach the diagram. The root is created
 * once and reused: `attachShadow` throws on a second call, and a block redraws into the same
 * element on every edit.
 */
export function showMermaidSvg(host: HTMLElement, svg: SVGSVGElement, sheet: string): void {
  const document = host.ownerDocument;
  const root = host.shadowRoot ?? host.attachShadow({ mode: "open" });
  const style = document.createElement("style");
  style.textContent = sheet;
  root.replaceChildren(style, document.importNode(svg, true));
}

/**
 * Parses SVG markup with the parser `parse` names and returns its `<svg>` imported into
 * `document`, in the SVG namespace and with a kept `foreignObject`'s content in the XHTML
 * namespace. Markup that is not an SVG document — one that does not parse as XML, or whose root
 * is not the SVG namespace's `svg` by its exact, case-sensitive name, or, under `"html"`, whose
 * body does not hold exactly one element named `svg` — throws `Error("Invalid SVG output")`.
 *
 * Stricter than upstream's private `sanitizeSvg`, which blocks five tags, keeps every `href` but
 * a `javascript:` one and keeps the Google Fonts `@import`: this SVG comes from a comment or a
 * document, which any person or agent writes. So only the elements the renderers emit survive,
 * matched by exact local name; every `on*` and `href` attribute goes, matched case-insensitively,
 * which is stricter than XML asks; every attribute but a `data-*` one whose value holds a CSS
 * escape (`\`) or a function that fetches (`url(…)` other than a local `url(#id)`,
 * `image-set(…)`, `image(…)`, `cross-fade(…)`, `src(…)`, `element(…)`) goes, which covers the
 * `fill:url(https://…)` an author's `classDef` or `style` paint becomes in beautiful-mermaid's
 * markup, where that line is written into presentation attributes, and its escaped spelling
 * `fill:\75rl(https://…)`; a kept `foreignObject` holds nothing but `ALLOWED_XHTML_ELEMENTS`
 * carrying `ALLOWED_XHTML_ATTRIBUTES`, so no `<img>`, `<a>` or `href` survives in a label; and
 * each kept `<style>` is rebuilt from the rules `document`'s own CSS parser reads in it, less
 * every rule that fetches, which is where the same `classDef` paint lands in upstream Mermaid's
 * markup, and the whole sheet goes instead where what that parser left still fetches or still
 * holds an `@import` (`sanitizedSheet`). The parser and every node come from `document` and its
 * window, never from the global scope, so the engine that reads a sheet here is the engine that
 * reads it when the page mounts it.
 */
export function sanitizeMermaidSvg(
  markup: string,
  document: Document,
  parse: MermaidParse
): SVGSVGElement {
  const view = document.defaultView;
  if (view === null) throw new Error("sanitizeMermaidSvg needs a document that has a window");
  const root = parse === "xml" ? xmlRoot(view, markup) : htmlRoot(view, markup);
  return document.adoptNode(sanitizeElement(root, view, parse, "svg")) as SVGSVGElement;
}

/** The root of an XML parse: the markup must parse, and its root must be the SVG namespace's
 *  `svg` by its exact, case-sensitive name. */
function xmlRoot(view: Window & typeof globalThis, markup: string): Element {
  const parsed = new view.DOMParser().parseFromString(markup, "image/svg+xml");
  const root: Element = parsed.documentElement;
  if (
    parsed.getElementsByTagName("parsererror").length > 0 ||
    root.namespaceURI !== SVG_NAMESPACE ||
    root.localName !== "svg"
  ) {
    throw new Error("Invalid SVG output");
  }
  return root;
}

/** The root of an HTML parse, which reports no `parsererror` node of its own and takes any markup
 *  it is given: the body must hold exactly one element, and its local name — already the one the
 *  browser's own parse gives it — must be `svg`. The namespace the parse assigned is not read
 *  here, nor trusted below: `sanitizeElement` puts the drawing in the SVG namespace. */
function htmlRoot(view: Window & typeof globalThis, markup: string): Element {
  const body = new view.DOMParser().parseFromString(markup, "text/html").body;
  const root = body.firstElementChild;
  if (body.children.length !== 1 || root === null || root.localName !== "svg") {
    throw new Error("Invalid SVG output");
  }
  return root;
}

/** A `url(#id)` reference to an element of the same SVG, unquoted or quoted. */
const LOCAL_URL = /url\s*\(\s*(["']?)#[^"'()\s]*\1\s*\)/gi;
/** A CSS function that loads a resource. A value holding a backslash is refused outright, since
 *  CSS decodes escapes in a function's name (`\75rl(` is `url(`) and no output of either renderer
 *  needs one outside a `data-*` attribute. */
const FETCHING_FUNCTION = /\\|(?:url|image-set|image|cross-fade|src|element)\s*\(/i;

/** Whether `text` holds a function that fetches, once the local `url(#id)` references a drawing
 *  makes to its own markers and gradients are taken out of it. */
function fetches(text: string): boolean {
  return FETCHING_FUNCTION.test(text.replace(LOCAL_URL, ""));
}

/** The `@import` at-keyword, searched for over the whole of what the parser left. The engine
 *  below drops every `@import` rule of the text it is given, because `replaceSync` is specified
 *  to; this is the second gate, so that an engine that read the sheet differently from the one
 *  that will mount it cannot carry an import past this module. Nothing else here would see one:
 *  a quoted target is no fetching function. An escaped spelling (`@\69mport`, which a browser
 *  reads as this one) needs no pattern of its own, since a backslash refuses the sheet. */
const IMPORT_RULE = /@import/i;

/**
 * `sheet` as the window's own CSS parser reads it: the rules that parser yields, each in its
 * own serialization, less every rule that fetches — and nothing at all where what is left still
 * fetches or still holds an `@import`.
 *
 * None of the author's text is copied through. `view.CSSStyleSheet` is the engine that reads
 * this sheet when the page mounts it, so where a token ends, what a comment hides, what an
 * escape spells and what `@import` means are its answers rather than ours. Three rounds of
 * INFRA-925 answered them in a reader of our own and each reader was got past: an escaped
 * `\75rl(`, an unquoted `url(` holding the `;` that was taken for the end of a rule, and a
 * comment holding one quote, which opened a string that ran on to the quote opening an
 * `@import`'s target, so the keyword was never tested. A rule the engine did not understand it
 * drops, and a declaration it did not understand with it, so what reaches the page is the part
 * of the author's text that engine read — never text that passed because our reader did not
 * read it either.
 *
 * A rule goes whole rather than in part, by the test the attribute pass makes
 * (`FETCHING_FUNCTION`, once the local `url(#id)` references a drawing makes to its own markers
 * and gradients are taken out): `cssText` serializes an `@media`, `@supports` or `@layer` with
 * the rules nested inside it and an `@font-face` with its `src`, so one test over it reaches
 * what a walk of the top level alone would miss, and `image-set("https://…" 1x)` fetches with
 * no `url(` anywhere in it. The cost is the author's own rule: a `classDef` whose paint fetches
 * loses the colour beside it too.
 *
 * Then the whole sheet is refused where anything that fetches, any backslash or any `@import`
 * is left in the serialization — one linear pass over text the parser has already bounded.
 * The engine serializes a selector's odd characters as escapes, so an author's own class name
 * can refuse the sheet it is written into; that costs that author their drawing's styling and
 * no other reader anything.
 *
 * What reaches a sheet differs between the renderers, which is why the fetch rule has to run
 * here as well as over attributes: beautiful-mermaid writes an author's `classDef` or `style`
 * line into presentation attributes, where the attribute rule filters it, while Mermaid writes
 * it into the drawing's own `<style>` (`createCssStyles`) without escaping it. So
 * `classDef evil mask-image:url(https://evil.example/m.png)` is drawn as a rule of the sheet,
 * matching a `<g class="node evil …">` of the same drawing, and fetches as soon as the drawing
 * is mounted. Mermaid's own DOMPurify does not run in the worker (see `ALLOWED_XHTML_ELEMENTS`),
 * so this is the only gate on that text.
 *
 * An author reaches the start of a sheet, not only the middle of one: the page reads Mermaid's
 * markup with the HTML parser (`htmlRoot`), which in SVG content ends the drawing's `<style>`
 * at an author's own `</style>` and opens the element after it, so a `classDef` carrying
 * `</style><style>` writes a second sheet of its own that the allowlist keeps and this function
 * reads whole.
 *
 * The page's main thread pays this cost for every drawing it mounts (`answerOf`,
 * `mermaid-diagrams.ts`), outside the worker's five-second bound, so it has to grow with the
 * sheet's length: a regular expression reading a declaration as `property:value` did not, and a
 * 20,000-character class name a `classDef` can write, or a run of `:` a `classDef` value can
 * write, froze the tab of every reader of the comment that held it (INFRA-925 review).
 */
function sanitizedSheet(view: Window & typeof globalThis, sheet: string): string {
  const parsed = new view.CSSStyleSheet();
  parsed.replaceSync(sheet);
  let kept = "";
  for (const rule of parsed.cssRules) {
    if (!fetches(rule.cssText)) kept += `${rule.cssText}\n`;
  }
  return fetches(kept) || IMPORT_RULE.test(kept) ? "" : kept;
}

/** Sanitizes `element` and its subtree in place, and answers it in the namespace `subtree`
 *  belongs in — itself where the parse already put it there, else the rebuilt node that took its
 *  place in the tree. `view` is the window the markup was parsed with, whose CSS parser reads a
 *  kept `<style>` (`sanitizedSheet`): the parse's own document has no window of its own. */
function sanitizeElement(
  element: Element,
  view: Window & typeof globalThis,
  parse: MermaidParse,
  subtree: Subtree
): Element {
  const kept = inNamespace(element, NAMESPACES[subtree]);
  for (const attribute of Array.from(kept.attributes)) {
    const name = attribute.name.toLowerCase();
    const localName = attribute.localName.toLowerCase();
    const fetching = !localName.startsWith("data-") && fetches(attribute.value);
    if (
      name.startsWith("on") ||
      localName.startsWith("on") ||
      localName === "href" ||
      fetching ||
      (subtree === "xhtml" && !ALLOWED_XHTML_ATTRIBUTES.has(localName))
    ) {
      kept.removeAttributeNode(attribute);
    }
  }
  if (kept.localName === "style") {
    kept.textContent = sanitizedSheet(view, kept.textContent ?? "");
    return kept;
  }
  const below: Subtree = kept.localName === "foreignObject" ? "xhtml" : subtree;
  for (const child of Array.from(kept.children)) {
    if (allows(child, parse, below)) sanitizeElement(child, view, parse, below);
    else child.remove();
  }
  return kept;
}

/** Whether a child stays where it was found. Inside a `foreignObject` the match is by local name
 *  alone, since the namespace a label's markup is parsed into differs between DOMs and parsers.
 *  In the drawing, an XML parse's namespaces are the document's own, so an element outside the
 *  SVG namespace is no part of the drawing and goes; an HTML parse assigns a namespace by its own
 *  rules, so there only the name is read. */
function allows(child: Element, parse: MermaidParse, subtree: Subtree): boolean {
  if (subtree === "xhtml") return ALLOWED_XHTML_ELEMENTS.has(child.localName);
  if (parse === "xml" && child.namespaceURI !== SVG_NAMESPACE) return false;
  return ALLOWED_ELEMENTS.has(child.localName);
}

/** `element` itself when the parse already put it in `namespace`, else a copy of it in that
 *  namespace, holding the same attributes and the same children and standing in its place. The
 *  parse is not trusted with this: a browser's HTML parse switches to XHTML inside a
 *  `foreignObject` where happy-dom's leaves the content in the SVG namespace, and an XML parse of
 *  a drawing made under a DOM that writes no `xmlns` on a label does the same. A namespace
 *  declaration is not copied over: the copy's namespace is its own, and a stale `xmlns` could
 *  only contradict it. */
function inNamespace(element: Element, namespace: string): Element {
  if (element.namespaceURI === namespace) return element;
  const copy = element.ownerDocument.createElementNS(namespace, element.localName);
  for (const attribute of Array.from(element.attributes)) {
    if (attribute.name === "xmlns" || attribute.name.startsWith("xmlns:")) continue;
    copy.setAttribute(attribute.name, attribute.value);
  }
  while (element.firstChild !== null) copy.appendChild(element.firstChild);
  element.parentNode?.replaceChild(copy, element);
  return copy;
}
