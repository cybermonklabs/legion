import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { type EditorState, Plugin, PluginKey } from "@milkdown/kit/prose/state";
import { Decoration, DecorationSet, type EditorView } from "@milkdown/kit/prose/view";
import { $prose } from "@milkdown/kit/utils";

/** One element the editor draws below a top-level paragraph a host's test accepted, for the host
 *  to draw into: the paragraph's text, and the paragraph's place among the accepted ones, from 0. */
export interface LineHost {
  readonly element: HTMLElement;
  readonly index: number;
  readonly text: string;
}

/** What a host asks the editor to draw below its lines: which paragraph texts take a host, and
 *  who is told the hosts, in document order, whenever they change. */
export interface EditorLineHosts {
  readonly accepts: (text: string) => boolean;
  readonly onHosts: (hosts: readonly LineHost[]) => void;
}

interface LineHostsState {
  readonly hosts: EditorLineHosts | null;
  readonly lines: readonly { readonly position: number; readonly text: string }[];
  readonly decorations: DecorationSet;
}

const lineHostsKey = new PluginKey<LineHostsState>("dispatch-line-hosts");
const noLineHosts: LineHostsState = { hosts: null, lines: [], decorations: DecorationSet.empty };

/** The element of each host, by its key, so a redraw of the same line keeps the element a host
 *  draws into. */
const elements = new WeakMap<EditorView, Map<string, HTMLElement>>();

/** The key of a line's host: its place and its text, so a line that moves or changes its text is
 *  a new host. */
const hostKey = (index: number, text: string) => `${index}\u0000${text}`;

/** The positions just after each top-level paragraph whose whole text `accepts`. */
function acceptedLines(
  doc: ProseMirrorNode,
  hosts: EditorLineHosts | null
): LineHostsState["lines"] {
  if (hosts === null) return [];
  const lines: { position: number; text: string }[] = [];
  doc.forEach((node, offset) => {
    if (node.type.name !== "paragraph") return;
    const text = node.textContent.trim();
    if (text !== "" && hosts.accepts(text)) lines.push({ position: offset + node.nodeSize, text });
  });
  return lines;
}

function decorate(doc: ProseMirrorNode, state: Omit<LineHostsState, "decorations">) {
  return DecorationSet.create(
    doc,
    state.lines.map(({ position, text }, index) =>
      Decoration.widget(
        position,
        (view) => {
          const byKey = elements.get(view) ?? new Map<string, HTMLElement>();
          elements.set(view, byKey);
          const key = hostKey(index, text);
          const element = byKey.get(key) ?? view.dom.ownerDocument.createElement("div");
          element.dataset.lineHost = String(index);
          element.contentEditable = "false";
          byKey.set(key, element);
          return element;
        },
        {
          ignoreSelection: true,
          key: hostKey(index, text),
          side: 1,
          // What the host draws is its own: the editor reads no event and no edit out of it.
          stopEvent: () => true,
        }
      )
    )
  );
}

/**
 * A host's own elements, drawn below the document's top-level paragraphs whose text its test
 * accepts, without changing the document: the paragraph keeps its text, which the writer edits,
 * and the host draws below it. A list of issues a document holds is drawn this way. Each element
 * is a widget decoration computed from the document, so no class or node is written into the
 * editor's own DOM; the host is told every element, its text and its place, after each change of
 * the set.
 */
export const lineHostsPlugin = $prose(
  () =>
    new Plugin<LineHostsState>({
      key: lineHostsKey,
      state: {
        init: () => noLineHosts,
        apply(transaction, value) {
          const hosts = transaction.getMeta(lineHostsKey) as EditorLineHosts | null | undefined;
          if (hosts === undefined && !transaction.docChanged) return value;
          const next = hosts === undefined ? value.hosts : hosts;
          const lines = acceptedLines(transaction.doc, next);
          const decorations = decorate(transaction.doc, { hosts: next, lines });
          return { hosts: next, lines, decorations };
        },
      },
      props: {
        decorations: (state: EditorState) =>
          (lineHostsKey.getState(state) ?? noLineHosts).decorations,
      },
      view: () => {
        let told = "";
        let toldTo: EditorLineHosts | null = null;
        return {
          update(view) {
            const state = lineHostsKey.getState(view.state) ?? noLineHosts;
            const byKey = elements.get(view) ?? new Map<string, HTMLElement>();
            const hosts = state.lines.flatMap(({ text }, index) => {
              const element = byKey.get(hostKey(index, text));
              return element === undefined ? [] : [{ element, index, text }];
            });
            // The elements no line draws any more go, so the map holds only the drawn ones.
            const drawn = new Set(state.lines.map(({ text }, index) => hostKey(index, text)));
            for (const key of byKey.keys()) if (!drawn.has(key)) byKey.delete(key);
            const signature = hosts.map(({ index, text }) => hostKey(index, text)).join("\u0001");
            if (state.hosts !== null && (signature !== told || state.hosts !== toldTo)) {
              told = signature;
              toldTo = state.hosts;
              state.hosts.onHosts(hosts);
            }
          },
        };
      },
    })
);

/** Asks the editor to draw a host below each line `hosts` accepts, in place of the last one;
 *  `null` draws none. */
export function setLineHosts(view: EditorView, hosts: EditorLineHosts | null): void {
  // A host writes its hosts from an effect, which can run after the editor's teardown.
  if (view.isDestroyed) return;
  view.dispatch(view.state.tr.setMeta(lineHostsKey, hosts).setMeta("addToHistory", false));
}
