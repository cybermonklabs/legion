/**
 * Upstream `mermaid`, the second renderer of Dispatch's diagram Web Worker: it draws a `timeline`,
 * a `mindmap`, a `pie` and every other type beautiful-mermaid has no layout for, beside the six
 * families beautiful-mermaid draws (`mermaid-render.worker.ts` routes a source to one of the two).
 *
 * Nothing imports a value of this module statically: the worker reaches it with
 * `await import(...)` and takes only a type from it otherwise, which is erased, so a reader
 * whose page holds only the families beautiful-mermaid draws never fetches mermaid, d3,
 * cytoscape or this module's chunk -- the DOM shim, which this module owns, among it.
 *
 * mermaid and d3 read `document` while their own modules evaluate, so `installDomShim` runs
 * before the import rather than before the first render.
 *
 * A source is drawn in two steps rather than one (`MermaidRenderer`), because the page bounds a
 * download and a layout differently: everything Mermaid fetches for a source is fetched in
 * `prepare`, so `draw` is the layout alone. The source's own parse is neither a download nor
 * the layout, and `prepare` leaves it to `draw`: nothing here reads the source's own config,
 * since `layout`, the one config key that could have sent a render for a chunk of its own, is
 * on the `secure` list mermaid is configured with below.
 *
 * Every colour below is a literal, measured against the two page backgrounds Dispatch draws on
 * (`#f8fafc` light, `#020618` dark), except the seven keys mermaid accepts a CSS custom property
 * in: those read `--mermaid-*` off the page (`web/src/styles.css`), which is how a label and a
 * border that sit on the page follow the reader's colour scheme. mermaid throws
 * `Unsupported color format: "var(--x)"` for a custom property in any key it derives a colour
 * from or fades, which is every other one.
 */
import { failedChunkFetch } from "./mermaid-chunk-fetch";
import { installDomShim, LAYOUT_WIDTH } from "./mermaid-dom-shim";
import { MERMAID_SOURCES, type MermaidSource } from "./mermaid-sources";

/** The longest source Mermaid is asked to draw. */
export const MERMAID_MAX_SOURCE_CHARS = 50_000;

/**
 * Draws one source to SVG markup, still unsanitized, in the two steps the page bounds
 * differently: `prepare` downloads everything that source's render will need, under the load
 * bound, and `draw` is then the layout alone, under the render bound
 * (`mermaid-render.worker.ts`, `mermaid-diagrams.ts`).
 */
export interface MermaidRenderer {
  /**
   * Downloads what `source` needs and nothing else: its type's definition chunk and that
   * type's layout engine, both by drawing the smallest source of its type and throwing the
   * drawing away (`warmLayoutFor`). `beganDownload` is called once, before that warm render,
   * and the one arm covers both downloads: they are two dynamic imports mermaid makes inside
   * one `render`, with nothing between them to re-arm on. A source whose type has no stand-in
   * is parsed instead, which is the one other download here, and that parse is armed the same
   * way.
   *
   * Nothing of `source` is read for its config: `layout` is on the renderer's `secure` list,
   * so a source chooses no layout engine and the warm needs to carry nothing of the source's
   * own text. The only parse a reader pays for is `draw`'s.
   *
   * It throws what Mermaid throws for a source Mermaid refuses -- a parse error, an unknown
   * type, a source past `MERMAID_MAX_SOURCE_CHARS` -- which is the refusal the reader is
   * shown, and a chunk the browser could not fetch, which is no answer about the source at all
   * (`failedChunkFetch`, `mermaid-render.worker.ts`). A warm that failed for any other reason
   * is swallowed: the render that follows is what answers for the source.
   */
  prepare(source: string, beganDownload: () => void): Promise<void>;
  /** Draws `source`, which `prepare` has already loaded everything for. */
  draw(source: string): Promise<string>;
}

/** The label a mark carries: `slate-50`, the lightest swatch of Dispatch's palette. Every mark it
 *  labels -- a colour slot's `MARK_FILLS` entry, `primaryColor` and the two beside it, a note --
 *  is a literal that is the same in both colour schemes, so this one value reads on all of them
 *  (4.55:1 at worst, 9.90:1 at best). The one mark it does not label is `MARK_FILL_LAST`. */
const LABEL_ON_MARK = "#f8fafc";

/** A line with nothing but the page behind it -- a flowchart's edges, the pie's rim, the
 *  timeline's axis: `slate-500`, the one swatch that reads against both page backgrounds
 *  (4.55:1 on `#f8fafc`, 4.23:1 on `#020618`), so it needs no second value per scheme. */
const LINE_ON_PAGE = "#62748e";

/** The fill of a mark, by colour slot. mermaid draws a slot's colour as both the fill of its mark
 *  (a timeline's event, a mindmap's node, a pie's slice) and the stroke of the edge that leaves
 *  it, with the page behind that edge, so a slot's luminance has to sit in the band that is dark
 *  enough for `LABEL_ON_MARK` to read on it (4.5:1) and light enough to read against the dark page
 *  (3:1). Dispatch's palette holds exactly five swatches in that band, one per hue; slot 5 and up
 *  repeat the cycle, because a sixth section on one diagram is rarer than an unreadable label. */
const MARK_FILLS = [
  // sky-700: label 5.60:1; against the page 5.60:1 light, 3.44:1 dark.
  "#0069a8",
  // emerald-700: label 5.13:1; against the page 5.13:1 light, 3.76:1 dark.
  "#007a55",
  // amber-700: label 4.81:1; against the page 4.81:1 light, 4.01:1 dark.
  "#bb4d00",
  // rose-700: label 5.76:1; against the page 5.76:1 light, 3.34:1 dark.
  "#c70036",
  // slate-500: label 4.55:1; against the page 4.55:1 light, 4.23:1 dark. The same swatch a line
  // on the page is drawn in, which is why the fifth slot's edges vanish into their own marks.
  LINE_ON_PAGE,
];

/** mermaid's colour slots, `cScale0` to `cScale11` (`THEME_COLOR_LIMIT`, which no theme variable
 *  below changes). */
const COLOR_SLOTS = 12;

/** The last slot's own fill: `white`. Every `.lineWrapper line` rule a timeline writes carries the
 *  last slot's label colour, and the last rule wins, so that one label colour draws the timeline's
 *  axis over the page and has to be `LINE_ON_PAGE` rather than `LABEL_ON_MARK`. The only other
 *  thing that reads it is a twelfth section's own label, so the twelfth slot's mark is filled
 *  white, where `LINE_ON_PAGE` reads at 4.76:1. */
const MARK_FILL_LAST = "#ffffff";

/** The mark mermaid derives every border and backdrop from: `slate-700`, the fill of a node, a
 *  state, an actor, a gantt task and a note. `LABEL_ON_MARK` reads on it at 9.90:1. It sits
 *  1.95:1 from the dark page, which is what `--mermaid-node-border` answers for: in the dark
 *  scheme that border is a lighter slate. */
const MARK_FILL_PRIMARY = "#314158";

/** The keys mermaid takes a CSS custom property in, bound to Dispatch's own properties: seven of
 *  its own and wardley's nested one. The page defines each property per colour scheme
 *  (`styles.css`), and a custom property inherits into the open shadow root the diagram is
 *  mounted in, which is how it reaches the drawing. */
const PAGE_PALETTE = {
  // Text with the page behind it: a diagram's labels outside any mark, the pie's title and
  // legend. `slate-900` light, `slate-100` dark, the page's own text colours.
  textColor: "var(--mermaid-text-color)",
  // The border of a node, drawn where its fill meets the page.
  nodeBorder: "var(--mermaid-node-border)",
  // A node's own label, which sits on that node's fill.
  nodeTextColor: "var(--mermaid-node-text-color)",
  // A diagram's title, drawn with nothing but the page behind it.
  titleColor: "var(--mermaid-title-color)",
  // The border of every mark mermaid derives from `primaryColor` (a state, a gantt task, a
  // requirement), drawn where that mark meets the page.
  primaryBorderColor: "var(--mermaid-primary-border-color)",
  // The page's own font, so a diagram's labels read as the body text around them.
  fontFamily: "var(--mermaid-font-family)",
  // 16px, which is also what the worker's shim falls back to while it measures a label (the
  // page's custom properties do not resolve in the worker), so the boxes mermaid lays out hold
  // the text the page paints.
  fontSize: "var(--mermaid-font-size)",
  // wardley paints its axis labels, its stage labels, its title and its notes with
  // `themeVariables.wardley?.axisTextColor ?? themeVariables.primaryTextColor`
  // (`wardleyDiagram-*.mjs`), and that fallback is `LABEL_ON_MARK`, the near-white a label on a
  // mark carries. These labels have the transparent canvas behind them rather than a mark, so
  // the fallback leaves them invisible on the light page; they are text on the page like any
  // other, and read through the same property as `textColor`. mermaid passes the value straight
  // to a `fill` attribute, deriving and fading nothing from it, so a custom property holds here.
  wardley: { axisTextColor: "var(--mermaid-text-color)" },
};

/** The colours that cannot follow the reader's scheme, and so are literals measured against both
 *  page backgrounds: the ones mermaid derives every other colour from, and the two a diagram
 *  fades. A mark is filled dark and labelled `LABEL_ON_MARK` in both schemes: `primaryColor` and
 *  the two beside it carry every border and backdrop mermaid derives, and a custom property
 *  throws there, so the pair that has to read is the mark's fill and its label, never the mark
 *  and the page. The border that does meet the page is `primaryBorderColor`, which does follow
 *  it. */
const MARK_PALETTE = {
  // The canvas stays transparent, as `.dispatch-mermaid` makes beautiful-mermaid's drawing: a
  // diagram sits on the reader's page rather than on a sheet of its own. Every backdrop mermaid
  // fills with this one -- a composite state's, an xychart's -- is transparent for the same
  // reason, and the text on it reads against the page through `textColor`.
  background: "transparent",
  // The mark every border and backdrop is derived from (`MARK_FILL_PRIMARY`).
  primaryColor: MARK_FILL_PRIMARY,
  primaryTextColor: LABEL_ON_MARK,
  // sky-800: the second mark (a sequence activation, a gantt alternating section). Label 7.18:1.
  secondaryColor: "#00598a",
  secondaryTextColor: LABEL_ON_MARK,
  // emerald-800: the third mark (an error backdrop, a journey's rectangle). Label 7.27:1.
  tertiaryColor: "#006045",
  tertiaryTextColor: LABEL_ON_MARK,
  // Every edge and arrowhead mermaid draws between marks, over the page.
  lineColor: LINE_ON_PAGE,
  arrowheadColor: LINE_ON_PAGE,
  // An edge's label carries no chip of its own, so it sits on the page and reads through
  // `textColor`, which follows the scheme; an opaque chip could not, being a literal here.
  edgeLabelBackground: "transparent",
  // A subgraph's backdrop, which sits on the page and carries its title: `slate-500` at a
  // quarter, the one literal that reads in both schemes through both of the ways a diagram uses
  // it. A flowchart fills the backdrop with this value as given, where the title reads 12.55:1
  // light and 14.46:1 dark; `block-beta` fades it to half (`fade(clusterBkg, 0.5)`), where the
  // title reads 8.79:1 and 9.94:1 and the backdrop stands 1.94:1 and 1.85:1 off the page. It
  // cannot be a custom property, because khroma's `fade` throws on one, and it cannot be
  // `transparent` either: khroma drops the alpha, so `fade` turns it into half-opaque black,
  // which leaves a block diagram's title at 4.32:1 on the light page.
  clusterBkg: `${LINE_ON_PAGE}40`,
  // That backdrop's border, drawn over the page, and faded the same way by `block-beta`
  // (`fade(clusterBorder, 0.2)`), so it is a literal for the same reason.
  clusterBorder: LINE_ON_PAGE,
  // A note is a mark like any other, and its text is the label on it.
  noteBkgColor: MARK_FILL_PRIMARY,
  noteTextColor: LABEL_ON_MARK,
  // The label on every mark drawn from a colour slot (`cScaleLabel0` and up read this), which is
  // a timeline's event, a mindmap's node and a journey's task.
  scaleLabelColor: LABEL_ON_MARK,
  // A gantt bar's text sits on the bar, which is `primaryColor`; the text outside a bar keeps
  // `textColor` and reads against the page.
  taskTextColor: LABEL_ON_MARK,
  taskTextLightColor: LABEL_ON_MARK,
  // A finished gantt bar is drawn in the line colour rather than mermaid's `lightgrey`, which no
  // scheme's text reads on.
  doneTaskBkgColor: LINE_ON_PAGE,
  doneTaskBorderColor: LINE_ON_PAGE,
  // The pie's slices are the colour slots again, and every stroke it draws -- the rim, and each
  // slice's own outline, whose outer arc has the page behind it -- is the line colour.
  pieStrokeColor: LINE_ON_PAGE,
  pieOuterStrokeColor: LINE_ON_PAGE,
  pieSectionTextColor: LABEL_ON_MARK,
  // Opaque slices. mermaid's own 0.7 would leave a slice's label sitting on the page as much as
  // on the slice, where no contrast between the two is defined.
  pieOpacity: "1",
  // A mindmap's root is filled from `git0` and labelled from `gitBranchLabel0`, not from a colour
  // slot, so the root carries the first slot's fill and the same label colour as every other node.
  git0: MARK_FILLS[0],
  gitBranchLabel0: LABEL_ON_MARK,
};

/** `cScale0`-`cScale11`, `cScaleInv0`-`cScaleInv11` and `pie1`-`pie12`: one entry per colour slot,
 *  which mermaid reads by name rather than from a list. */
function colorSlots(): Record<string, string> {
  const slots: Record<string, string> = {};
  for (let slot = 0; slot < COLOR_SLOTS; slot += 1) {
    const fill = MARK_FILLS[slot % MARK_FILLS.length];
    // The last slot is the one exception (see `MARK_FILL_LAST`), and only for `cScale`: the pie
    // labels every slice with one colour, so a white twelfth slice would carry an unreadable one.
    slots[`cScale${slot}`] = slot === COLOR_SLOTS - 1 ? MARK_FILL_LAST : fill;
    slots[`pie${slot + 1}`] = fill;
    // The line mermaid draws inside a mark of this slot (a timeline event's rule, a mindmap
    // node's) and under it, where the page shows through.
    slots[`cScaleInv${slot}`] = LINE_ON_PAGE;
  }
  // The last slot's label, which is also the timeline axis's stroke (see `MARK_FILL_LAST`).
  slots[`cScaleLabel${COLOR_SLOTS - 1}`] = LINE_ON_PAGE;
  return slots;
}

/** The ids handed to `mermaid.render`. Each call takes its own: the id names the drawing's own
 *  `<style>` and its markers, and the shim's document outlives every call. A warm render takes
 *  its id from a counter of its own, so the drawings a reader sees stay numbered from zero
 *  whatever was warmed before them. */
let nextRenderId = 0;
let nextWarmId = 0;

/** The diagram types this worker has already warmed and seen draw (`warmLayoutFor`), so the
 *  second source of a type pays for no second warm render. A warm is recorded only once it has
 *  drawn: one that failed is tried again by the next source of its type rather than leaving
 *  that type cold for the life of the worker. */
const warmedTypes = new Set<string>();

/** The one load, kept so a second source pays for neither the import nor `initialize` again. */
let loading: Promise<MermaidRenderer> | null = null;

/** The globals this worker's DOM shim defined, installing it where nothing has yet. This module
 *  is the one owner of that install: the worker reads the list from here, to take those globals
 *  away around a beautiful-mermaid call (`mermaid-render.worker.ts`, `withoutADom`), rather than
 *  reaching past this module into the shim it loads. */
export function mermaidShimGlobals(): readonly string[] {
  return installDomShim().globalsDefined;
}

async function load(): Promise<MermaidRenderer> {
  // Before the import, not before the first render: mermaid and d3 read `document` as their own
  // modules evaluate.
  mermaidShimGlobals();
  const { default: mermaid } = await import("mermaid");
  mermaid.initialize({
    // Nothing in a worker scans a page for diagrams; the renderer below is handed one source at a
    // time, and a scan of the shim's own document would draw whatever the last call left behind.
    startOnLoad: false,
    // Mermaid's own sanitizer does not run in this worker at all: it is DOMPurify, and
    // `DOMPurify.isSupported` reads `document.implementation`, which the shim's linkedom
    // document does not define, so `sanitize` hands back what it was given. The gate on this
    // path is the page's own sanitizer, which every drawing goes through before it is mounted
    // (`mermaidSvgElement`, `mermaid-diagrams.ts`). The level is still set, because a source
    // cannot raise it -- mermaid deletes every key of `secure` from a directive -- so a source
    // that reaches a host where DOMPurify does run is sanitized there rather than taking a
    // looser level of its own.
    securityLevel: "strict",
    // An HTML label is laid out by the browser, which a worker has none of, and it can carry a
    // remote `<img>` that fetches at render and again at mount. The nested key closes the
    // swimlane label, which reads `flowchart.htmlLabels` and emits an HTML label without it.
    htmlLabels: false,
    flowchart: { htmlLabels: false },
    // A refusal is the page's to draw (`mermaid-diagrams.ts` writes the status line); without
    // this mermaid leaves its own error drawing in the document and answers with it.
    suppressErrorRendering: true,
    // Without this every marker, clip path and gradient id inside a drawing is random, so one
    // source draws different markup on every call; counted ids make two drawings of a source
    // comparable, apart from the id this module hands each call.
    deterministicIds: true,
    // gantt sizes itself to its host element's `offsetWidth`, which no worker has, so it is laid
    // out at the width the shim's `screen` is (`LAYOUT_WIDTH`).
    gantt: { useWidth: LAYOUT_WIDTH },
    // What a source's own front matter or directive may not change. mermaid's default list
    // protects `secure` itself and the size limits; the keys below it are the palette, the
    // sanitizer's own level, and the three that would otherwise make a source fetch inside
    // `render`. A source-level `themeCSS` carrying `url(https://...)` would reach the
    // drawing's `<style>` and fetch at render and again at mount. Read from the installed
    // package rather than restated here, so a key mermaid adds to its own list stays
    // protected. mermaid deletes each of these from a source's config before it reads it, and
    // `sanitize` walks nested objects, so a key written inside a diagram's own section
    // (`flowchart: { layout: ... }`) goes the same way as one written at the top.
    //
    // `layout` names the engine a diagram is laid out by, which mermaid imports as a chunk of
    // its own inside `render` (`dagre`, `swimlane`, `cose-bilkent`, `elk` with the ELK
    // algorithms, the last 1,513,480 bytes built). A source that chose one would download it
    // under the reader's five seconds, since the warm above draws the type's stand-in and the
    // stand-in has no such choice. No requirement here asks Dispatch to honour a source's
    // layout choice, so the key is refused rather than carried into the warm: the warm and the
    // render then agree by construction, both taking the bundle's own default for the type.
    // What it costs an author is measured: a source whose front matter or directive names
    // `dagre`, `cose-bilkent`, `elk` or `swimlane` is laid out by its type's default engine
    // instead, which is what every source that names none already gets. A type that chooses
    // its own engine is untouched, because that choice is no config key of the source's:
    // `flowchart-elk`'s own detector sets it, a swimlane and a state diagram carry it in
    // mermaid's default config, and a mindmap reads `mindmap.layoutAlgorithm`.
    //
    // The last two are the only keys left that make a source fetch inside `render`. A `$$...$$`
    // label draws `MathML is unsupported in this environment.` in this worker, because the
    // shim's linkedom `window` defines no `MathMLElement`, unless the source turns one of them
    // on -- and then mermaid imports katex (261,763 bytes built) inside the render, under the
    // reader's five seconds, which is the download this issue took off that bound everywhere
    // else. Warming it instead would mean a second kind of warm source, carrying `$$...$$` and
    // the key, for every type that lays out a label, and 261,763 bytes of the load bound spent
    // on notation no requirement here asks for. So the keys go here, and what it costs an
    // author is measured: a source that sets one and holds `$$...$$` draws
    // `MathML is unsupported in this environment.` where it drew typeset math, which is what
    // every source that sets neither already draws.
    secure: [
      ...(mermaid.mermaidAPI.defaultConfig.secure ?? []),
      "themeCSS",
      "themeVariables",
      "fontFamily",
      "theme",
      "layout",
      "legacyMathML",
      "forceLegacyMathML",
    ],
    // base is the one theme that takes its whole palette from `themeVariables`: it derives every
    // colour from the keys below, where `default` and the two dark themes mix in colours of their
    // own. Its `calculate` re-applies each given key after its derivations have run, so every
    // literal below reaches the drawing unchanged.
    theme: "base",
    themeVariables: { ...PAGE_PALETTE, ...MARK_PALETTE, ...colorSlots() },
  });

  /**
   * Downloads what a diagram of `standIn`'s type needs and has not been downloaded in this
   * worker yet -- its definition chunk and its layout engine -- by drawing the smallest source
   * of that type there is (`mermaid-sources.ts`) and throwing the drawing away. One
   * `beganDownload` covers both: they are two dynamic imports of one render, which cannot be
   * re-armed between.
   *
   * Mermaid registers its layout engines as dynamic imports of a chunk of its own
   * (`registerDefaultLayoutLoaders`: `dagre`, `swimlane`, `cose-bilkent` and `elk` with the ELK
   * algorithms, the last 1,513,480 bytes built), and loads the one a diagram asks for inside
   * `render`, after the worker would otherwise have said it is drawing. Nothing public loads
   * one on its own: `registerLayoutLoaders` is exported, but the registered loaders are
   * private to that chunk and the only thing that runs one is a render. So a render of a
   * stand-in is what warms it, rather than an import of the chunk by its own hash-named path,
   * which would go silently missing on the next Mermaid upgrade.
   *
   * It is a render of the *same type*, rather than of a type looked up in a table of layouts,
   * because which layout a type uses is Mermaid's to choose and moves between versions: 12.3.0
   * lays a flowchart, a state, a class, an ER, a usecase, a requirement and an agentflow
   * diagram out with `elk`, a mindmap with `cose-bilkent` and a swimlane with `swimlanes`, and
   * the remaining 28 types load no layout engine at all. The table of stand-ins is held level
   * with the library by a row of its own (`mermaid-diagrams.test.tsx`), so a type a Mermaid
   * upgrade adds cannot reach here with no stand-in.
   *
   * It is the stand-in *as `mermaid-sources.ts` holds it*: nothing of the reader's source is
   * put into the text rendered here, and no directive is built by joining strings. `layout` is
   * on the `secure` list above, so no source chooses an engine of its own; the warm and the
   * render reach the same one because both take the bundle's own default for that type.
   *
   * A warm is recorded only once it has drawn. One that failed warms nothing and refuses
   * nothing -- the reader's own render is what answers for the source -- and the next source of
   * its type warms again, rather than running cold under the render bound for the life of the
   * worker. The one failure it does not swallow is a chunk the browser could not fetch: the
   * render below would fail on the same fetch, and that is no answer about the source
   * (`mermaid-render.worker.ts` answers `rendererUnavailable` for it).
   */
  async function warmLayoutFor(standIn: MermaidSource, beganDownload: () => void): Promise<void> {
    if (warmedTypes.has(standIn.id)) return;
    beganDownload();
    try {
      await mermaid.render(`dispatch-mermaid-warm-${nextWarmId++}`, standIn.source);
    } catch (error) {
      if (failedChunkFetch(error)) throw error;
      return;
    }
    warmedTypes.add(standIn.id);
  }

  return {
    async prepare(source: string, beganDownload: () => void) {
      guardLength(source);
      // Synchronous, and it reads the source's front matter and its directives itself, so the
      // type is known without parsing a character of the body; a source whose first line names
      // no type it draws is refused here, as `UnknownDiagramError`, before anything is
      // fetched. The config handed to it is the site's, which is what mermaid detects a type
      // with inside `parse` (`processAndSetConfigs`): no detector registered in 12.3.0 reads
      // it, and it is passed so one that did would read here what it reads there.
      const diagramType = mermaid.detectType(source, mermaid.mermaidAPI.getSiteConfig());
      const standIn = MERMAID_SOURCES.find((entry) => entry.id === diagramType);
      if (standIn === undefined) {
        // Only a Mermaid upgrade can reach this, and a row of its own fails on it
        // (`mermaid-diagrams.test.tsx`). With no stand-in to warm with, the source's own parse
        // is what downloads its type's definition chunk -- `mermaid.core.mjs` holds 37 of those
        // as dynamic imports, each otherwise fetched inside the first render of its type -- so
        // that download is still under the load bound, as every other one is.
        beganDownload();
        await mermaid.parse(source);
        return;
      }
      await warmLayoutFor(standIn, beganDownload);
    },
    async draw(source: string) {
      const { svg } = await mermaid.render(`dispatch-mermaid-${nextRenderId++}`, source);
      return svg;
    },
  };
}

/** Refuses a source past `MERMAID_MAX_SOURCE_CHARS` before Mermaid reads a character of it.
 *  Past that mermaid draws a diagram whose only text is `Maximum text size in diagram exceeded`
 *  instead of throwing, and the reader would get that picture in place of a reason. The message
 *  is what a reader reads after `Unable to render Mermaid diagram: `. */
function guardLength(source: string): void {
  if (source.length > MERMAID_MAX_SOURCE_CHARS) {
    throw new Error(
      `it is longer than ${MERMAID_MAX_SOURCE_CHARS.toLocaleString("en-US")} characters`
    );
  }
}

/**
 * Loads the shim and Mermaid, configures Mermaid once, and answers the renderer.
 *
 * A rejection is not kept. The load is one dynamic import of Mermaid's chunks, which a redeploy
 * or a dropped connection can fail; a rejected promise left here would refuse every later
 * source in this worker with the same reason, where the page's contract is that a renderer that
 * cannot load is no reader's answer at all (`mermaid-render.worker.ts` answers
 * `rendererUnavailable`, and `mermaid-diagrams.ts` stores nothing for it). So the next source
 * tries the import again.
 */
export async function loadMermaidRenderer(): Promise<MermaidRenderer> {
  loading ??= load();
  try {
    return await loading;
  } catch (error) {
    loading = null;
    throw error;
  }
}
