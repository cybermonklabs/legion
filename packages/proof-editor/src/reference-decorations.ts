import {
  HARD_BREAK_PIECE,
  referenceReading,
  type WordsPiece,
  wordsFormatting,
  wordsPieceUnder,
} from "@legion/contracts";
import type { Mark, Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { type EditorState, Plugin, PluginKey, type Transaction } from "@milkdown/kit/prose/state";
import {
  AddMarkStep,
  RemoveMarkStep,
  ReplaceAroundStep,
  ReplaceStep,
} from "@milkdown/kit/prose/transform";
import { Decoration, DecorationSet, type EditorView } from "@milkdown/kit/prose/view";
import { $prose } from "@milkdown/kit/utils";

/** One reference the host's reader finds in a run of the editor's own text: `start` is the index
 *  of its first character, `end` one past its last, and `key` what the host resolves it by
 *  (`EditorReferences.targets`). Two candidates with the same key name the same target. */
export interface ReferenceCandidate {
  readonly start: number;
  readonly end: number;
  readonly key: string;
}

/** One distinct reference the document's candidates name, as the editor offers it to the host
 *  (`EditorReferences.onKeys`): `key` the reader's key for it, and `mention` whether a mention
 *  names it - a link whose words are a mention's (`@legion/contracts`'s `referenceReading`: they
 *  open with the mention sign, with no code among them and no formatting over a part of them)
 *  and whose whole address is that key - which the host answers in `EditorReferences.mentions`
 *  rather than in `targets`. A key both a mention and a candidate of another kind name is
 *  offered once as each. */
export interface OfferedReference {
  readonly key: string;
  readonly mention: boolean;
}

/** What a candidate resolves to: `href` is the `dispatch://` reference the drawn title carries as
 *  `data-dispatch-href` (what the editor opens it by, `./dispatch-link-open.ts`, and what the
 *  host's hover card reads), `path` the host's own address of the target, which the title links
 *  to as its `href`, so a browser opens it in a new tab and Tab reaches it - both absent for a
 *  target that opens no page, a person, whom no page shows, whose title is drawn as words and no
 *  link; `title` the words the editor draws in place of the stored text, `titleKey` an issue key
 *  `title` starts with, drawn in semibold, and `length` how many of the candidate's characters,
 *  from its start, the title stands for, when that is fewer than all of them: the rest stay
 *  drawn as stored text (a `#q2-` whose question is q2 draws its title for `#q2` and keeps the
 *  `-`, so no character the reference does not name is ever hidden from the author who typed
 *  it). */
export interface ReferenceTarget {
  readonly href?: string;
  readonly path?: string;
  readonly title: string;
  readonly titleKey?: string;
  readonly length?: number;
}

/** The host's side of the editor's reference titles, the whole of it in one value, as
 *  `ChangeDecorations` carries a whole change set: the reader that says what a run of text cites,
 *  what each candidate resolves to, and the callback that asks for the rest.
 *  `ProofEditorHandle.setReferences` replaces the last one. */
export interface EditorReferences {
  /** Every reference candidate in `text`, in order and not overlapping. The text is one text
   *  block's, with code and a link's own text masked out; the same text must answer the same
   *  candidates, since the editor re-reads a block only when an edit touches it. */
  scan(text: string): readonly ReferenceCandidate[];
  /** What each candidate key resolves to; a key absent from the map or mapped to `null` stays
   *  drawn as the words the author wrote. */
  readonly targets: ReadonlyMap<string, ReferenceTarget | null>;
  /** What a mention of each key draws (`OfferedReference.mention`), by the same terms as
   *  `targets`: the host answers a key only where its address names a party it draws by name,
   *  as `@legion/contracts`'s `mentionName` writes one, and every other mention keeps the words
   *  its author wrote. */
  readonly mentions: ReadonlyMap<string, ReferenceTarget | null>;
  /** Called with the distinct references the document's candidates name, sorted by key and a
   *  key's plain candidate before its mention, whenever that set changes, so the host resolves
   *  exactly what the document holds. */
  onKeys(keys: readonly OfferedReference[]): void;
}

/** One candidate in the document. Kept as an undrawn inline decoration so mapping carries its
 *  range through an edit outside its text block. */
interface CandidateSpec {
  readonly key: string;
  /** Whether the candidate is a mention, which `EditorReferences.mentions` answers. */
  readonly mention: boolean;
  /** The marks its title is drawn inside: those of its first character. */
  readonly marks: readonly Mark[];
}

/** A candidate of one text block, in the block's own offsets, and whether it is a mention. */
interface BlockCandidate extends ReferenceCandidate {
  readonly mention: boolean;
}

/** What the host answered for the document's candidates: `EditorReferences` without its reader. */
type ReferenceAnswers = Pick<EditorReferences, "mentions" | "targets">;

interface ReferenceDecorations {
  /** What the host last set, or `null` while it has set nothing. */
  readonly references: EditorReferences | null;
  readonly candidates: DecorationSet;
  /** The distinct references `candidates` name, sorted as `EditorReferences.onKeys` reports them;
   *  the same array while that set is unchanged. */
  readonly keys: readonly OfferedReference[];
  readonly focused: boolean;
  /** Whether the reader edits the view: its titles are no Tab stop then (`titleWidget`). */
  readonly editable: boolean;
  /** The ranges (`from:to`) of the resolved candidates the selection touches, joined by spaces. */
  readonly revealed: string;
  readonly decorations: DecorationSet;
}

type ReferenceChange =
  | { readonly kind: "references"; readonly references: EditorReferences | null }
  | { readonly kind: "focus"; readonly focused: boolean }
  | { readonly kind: "access"; readonly editable: boolean };

const referencesKey = new PluginKey<ReferenceDecorations>("dispatch-reference-decorations");

/** The class that hides a resolved candidate's stored text while its title is drawn. */
const HIDDEN_TEXT_CLASS = "dispatch-reference-text";
/** The class of the drawn title. */
const TITLE_CLASS = "dispatch-reference";
/** Stands in for the characters of a link's text and an inline node other than a hard break: no
 *  reference can start right after it or run through it, and a key right before it ends there, as
 *  the `[` or `)` of the link's stored Markdown would. */
const LINK_MASK = "<";
/** Stands in for code's characters, as its stored backtick does: a reference may start or end
 *  beside it, never inside it. */
const CODE_MASK = "`";

const NO_ANSWERS: ReferenceAnswers = { mentions: new Map(), targets: new Map() };

/** What stands in for a run of code's text, as its stored backtick does; undefined for text read
 *  as written. A link's text is masked by the walk below, which keeps the run itself. */
function maskFor(marks: readonly Mark[]): string | undefined {
  return marks.some((mark) => mark.type.spec.code === true) ? CODE_MASK : undefined;
}

function linkOf(marks: readonly Mark[]): Mark | undefined {
  return marks.find((mark) => mark.type.name === "link");
}

/** A run of text one link mark covers, in the block's own offsets, and the formatting over each
 *  text node of it (`@legion/contracts`'s `wordsPieceUnder`), which says how its words are
 *  formatted: words formatted in part are their author's, and a code span's `@` is the code's
 *  characters, never a mention's sign. */
interface LinkRun {
  readonly mark: Mark;
  readonly start: number;
  readonly pieces: WordsPiece[];
  end: number;
  text: string;
}

/** The candidates of one text block whose content starts at `start`. The text read is the
 *  block's with code and links masked, so it is as long as the block's own text and a
 *  candidate's offsets are its positions. A link is read by the rule every reader of a record
 *  reads one by (`@legion/contracts`'s `referenceReading`): one whose text is its own address -
 *  the autolink a document stores as `[dispatch://…](dispatch://…)` - is one candidate of that
 *  address; one whose words are a mention - they open with the mention sign, with no code among
 *  them and no formatting or hard break over a part of them - is one mention of that address,
 *  which the host draws as the party's current name or leaves as the words; and one whose text
 *  its author wrote any other way keeps those words and is never drawn over. A hard break holds
 *  no mark, so a link whose words one cuts is a run of text on each side of it with the same
 *  link, which is read as the one link it is (`HARD_BREAK_PIECE`). */
function blockCandidates(
  block: ProseMirrorNode,
  start: number,
  references: EditorReferences
): Decoration[] {
  let text = "";
  const runs: LinkRun[] = [];
  block.forEach((child, offset, index) => {
    if (!child.isText) {
      const leaf = child.type.spec.leafText?.(child);
      text += leaf?.length === child.nodeSize ? leaf : LINK_MASK.repeat(child.nodeSize);
      const last = runs.at(-1);
      const next = block.maybeChild(index + 1);
      if (
        child.type.name === "hardbreak" &&
        last !== undefined &&
        last.end === offset &&
        next?.isText === true &&
        last.mark.isInSet(next.marks)
      ) {
        last.end += child.nodeSize;
        last.text += " ";
        last.pieces.push(HARD_BREAK_PIECE);
      }
      return;
    }
    const childText = child.text ?? "";
    const link = linkOf(child.marks);
    if (link !== undefined) {
      const last = runs.at(-1);
      if (last !== undefined && last.end === offset && last.mark.eq(link)) {
        last.end += childText.length;
        last.text += childText;
        last.pieces.push(wordsPieceUnder(child.marks));
      } else {
        runs.push({
          end: offset + childText.length,
          mark: link,
          pieces: [wordsPieceUnder(child.marks)],
          start: offset,
          text: childText,
        });
      }
      text += LINK_MASK.repeat(childText.length);
      return;
    }
    const mask = maskFor(child.marks);
    text += mask === undefined ? childText : mask.repeat(childText.length);
  });

  const found: BlockCandidate[] = [];
  let reached = 0;
  for (const candidate of references.scan(text)) {
    if (
      candidate.start < reached ||
      candidate.end <= candidate.start ||
      candidate.end > text.length
    ) {
      continue;
    }
    reached = candidate.end;
    found.push({ ...candidate, mention: false });
  }
  for (const run of runs) {
    const href = run.mark.attrs.href;
    if (typeof href !== "string") continue;
    // Whether the address names a party is the host's to say, so every link whose words are a
    // mention's is offered as one.
    const reads = referenceReading({
      address: href,
      party: true,
      words: run.text,
      wordsFormatting: wordsFormatting(run.pieces),
    });
    if (reads === "words") continue;
    // The whole address and nothing else: the reader must read the link's destination as one
    // reference running end to end.
    const [only, ...others] = references.scan(href);
    if (only !== undefined && others.length === 0 && only.start === 0 && only.end === href.length) {
      found.push({ end: run.end, key: only.key, mention: reads === "mention", start: run.start });
    }
  }

  return found.map((candidate) => {
    const spec: CandidateSpec = {
      key: candidate.key,
      mention: candidate.mention,
      // The marks of its first character. A link read as a reference - its text its own address,
      // or its words a mention - keeps its link mark, so the title is drawn inside that link,
      // which stays the one link there (`titleWidget`).
      marks: block.childAfter(candidate.start).node?.marks ?? [],
    };
    return Decoration.inline(start + candidate.start, start + candidate.end, {}, spec);
  });
}

/** Every candidate in the text blocks `from` to `to` touches, code blocks excepted. */
function candidatesBetween(
  doc: ProseMirrorNode,
  from: number,
  to: number,
  references: EditorReferences
): Decoration[] {
  const found: Decoration[] = [];
  doc.nodesBetween(from, to, (node, pos) => {
    if (!node.isTextblock) return true;
    if (node.type.spec.code !== true) found.push(...blockCandidates(node, pos + 1, references));
    return false;
  });
  return found;
}

function allCandidates(doc: ProseMirrorNode, references: EditorReferences): DecorationSet {
  return DecorationSet.create(doc, candidatesBetween(doc, 0, doc.content.size, references));
}

/** The ranges of the document after `transaction` that its steps wrote: replaced content and the
 *  text a mark step changed. `undefined` when a step's reach is unknown, which re-reads it all. */
function changedRanges(transaction: Transaction): Array<[number, number]> | undefined {
  const ranges: Array<[number, number]> = [];
  for (const [index, step] of transaction.steps.entries()) {
    if (
      !(step instanceof ReplaceStep) &&
      !(step instanceof ReplaceAroundStep) &&
      !(step instanceof AddMarkStep) &&
      !(step instanceof RemoveMarkStep)
    ) {
      return undefined;
    }
    const after = transaction.mapping.slice(index);
    ranges.push([after.map(step.from, -1), after.map(step.to, 1)]);
  }
  return ranges;
}

/** The distinct references `candidates` name, sorted by key and a key's plain candidate before
 *  its mention (`EditorReferences.onKeys`); `previous` while it names the same ones. */
function candidateKeys(
  candidates: DecorationSet,
  previous: readonly OfferedReference[]
): readonly OfferedReference[] {
  const seen = new Set<string>();
  const keys: OfferedReference[] = [];
  for (const candidate of candidates.find()) {
    const { key, mention } = candidate.spec as CandidateSpec;
    const id = `${mention ? 1 : 0}${key}`;
    if (seen.has(id)) continue;
    seen.add(id);
    keys.push({ key, mention });
  }
  keys.sort((left, right) =>
    left.key === right.key
      ? Number(left.mention) - Number(right.mention)
      : left.key < right.key
        ? -1
        : 1
  );
  const same =
    keys.length === previous.length &&
    keys.every(
      (offered, index) =>
        offered.key === previous[index]?.key && offered.mention === previous[index]?.mention
    );
  return same ? previous : keys;
}

/** What the host answered for the candidate `spec` describes: a mention's answer is in
 *  `mentions`, every other candidate's in `targets`. */
function answerFor(
  answers: ReferenceAnswers,
  spec: CandidateSpec
): ReferenceTarget | null | undefined {
  return (spec.mention ? answers.mentions : answers.targets).get(spec.key);
}

/** The candidates after `transaction`: the previous ones mapped through it, with each text block
 *  an edit touched read again. */
function nextCandidates(
  candidates: DecorationSet,
  transaction: Transaction,
  references: EditorReferences
): DecorationSet {
  const { doc } = transaction;
  const ranges = changedRanges(transaction);
  if (ranges === undefined) {
    return allCandidates(doc, references);
  }
  let next = candidates.map(transaction.mapping, doc);
  for (const [rangeFrom, rangeTo] of ranges) {
    // One position wider each way, so a block a deletion joined or split is read whole.
    const from = Math.max(0, Math.min(rangeFrom, rangeTo) - 1);
    const to = Math.min(doc.content.size, Math.max(rangeFrom, rangeTo) + 1);
    const blocks: Array<[number, number]> = [];
    doc.nodesBetween(from, to, (node, pos) => {
      if (!node.isTextblock) return true;
      blocks.push([pos, pos + node.nodeSize]);
      return false;
    });
    for (const [blockFrom, blockTo] of blocks) {
      next = next.remove(next.find(blockFrom, blockTo));
      next = next.add(doc, candidatesBetween(doc, blockFrom, blockTo, references));
    }
  }
  return next;
}

/** Where a resolved candidate's title ends: `length` characters into it, and never past its end,
 *  so a target naming more characters than the candidate holds - its words shortened by an edit
 *  the host has not answered for yet - hides no text beyond the words it was read from. */
function drawnEnd(candidate: Decoration, target: ReferenceTarget): number {
  return target.length === undefined
    ? candidate.to
    : Math.min(candidate.to, candidate.from + target.length);
}

/** Each resolved candidate of `candidates` between `from` and `to` whose title touches `from` (a
 *  caret inside it or at either end, or a range overlapping it), in the order `find` gives them,
 *  with its target and where its title ends (`drawnEnd`). */
function* titlesTouching(
  candidates: DecorationSet,
  answers: ReferenceAnswers,
  from: number,
  to: number
): Generator<{ candidate: Decoration; end: number; target: ReferenceTarget }> {
  for (const candidate of candidates.find(from, to)) {
    const target = answerFor(answers, candidate.spec as CandidateSpec);
    if (!target) continue;
    const end = drawnEnd(candidate, target);
    if (end > candidate.from && from <= end) yield { candidate, end, target };
  }
}

/** The ranges each resolved candidate's title stands for that the selection touches (a caret
 *  inside or at either end, or a range overlapping one), while the editor has focus; empty
 *  otherwise. */
function revealedRanges(
  state: EditorState,
  candidates: DecorationSet,
  answers: ReferenceAnswers,
  focused: boolean
): string {
  if (!focused) return "";
  const ranges: string[] = [];
  for (const range of state.selection.ranges) {
    for (const { candidate, end } of titlesTouching(
      candidates,
      answers,
      range.$from.pos,
      range.$to.pos
    )) {
      ranges.push(`${candidate.from}:${end}`);
    }
  }
  return ranges.sort().join(" ");
}

/** The resolved reference whose title stands for text holding or touching `pos` (a caret inside
 *  it or at either end), the title `revealedRanges` gives way to there, if any: what Alt+Enter
 *  opens beside a bare key (`./dispatch-link-open.ts`). */
export function referenceAt(state: EditorState, pos: number): ReferenceTarget | undefined {
  const value = referencesKey.getState(state);
  if (value === undefined || value.references === null) return undefined;
  for (const { target } of titlesTouching(value.candidates, value.references, pos, pos)) {
    return target;
  }
  return undefined;
}

/** The title drawn over a resolved candidate. Over bare text it is a link to the target's own
 *  address (`path`), so a browser opens it in a new tab and, in a view the reader cannot edit,
 *  Tab reaches it; while the reader edits the view it is out of the tab order, since a link
 *  inside a document being edited is opened with Alt+Enter from the text, never tabbed to. Over a
 *  link read as a reference it is drawn inside that link (`marks`), which is the link the reader
 *  clicks and tabs to, so it carries no address of its own: an `a` with no `href` is no link, and
 *  no link stands inside another. Beside that link instead, it left the link holding only hidden
 *  text, a stop of Tab that drew nothing. A target that opens no page (no `href`) is drawn as the
 *  same words and no link. */
function titleWidget(
  target: ReferenceTarget,
  editable: boolean,
  inLink: boolean
): (view: EditorView) => HTMLElement {
  return (view) => {
    const document = view.dom.ownerDocument;
    const { href, path, title, titleKey } = target;
    const drawn = document.createElement(href === undefined ? "span" : "a");
    drawn.className = TITLE_CLASS;
    if (href !== undefined) {
      if (!inLink && path !== undefined) {
        drawn.setAttribute("href", path);
        if (editable) drawn.tabIndex = -1;
      }
      drawn.dataset.dispatchHref = href;
    }
    if (titleKey !== undefined && titleKey !== "" && title.startsWith(titleKey)) {
      const key = document.createElement("strong");
      key.textContent = titleKey;
      drawn.append(key, title.slice(titleKey.length));
    } else {
      drawn.textContent = title;
    }
    // A press on the title neither moves the caret nor focuses the editor, so the title stays
    // drawn until the click lands and the editor opens it (`./dispatch-link-open.ts`); a middle
    // press pastes nothing.
    drawn.addEventListener("mousedown", (event) => event.preventDefault());
    return drawn;
  };
}

function drawnDecorations(
  doc: ProseMirrorNode,
  candidates: DecorationSet,
  answers: ReferenceAnswers,
  revealed: string,
  editable: boolean
): DecorationSet {
  if (answers.targets.size === 0 && answers.mentions.size === 0) return DecorationSet.empty;
  const shown = new Set(revealed === "" ? [] : revealed.split(" "));
  const decorations: Decoration[] = [];
  for (const candidate of candidates.find()) {
    const spec = candidate.spec as CandidateSpec;
    const target = answerFor(answers, spec);
    if (!target) continue;
    const to = drawnEnd(candidate, target);
    if (to <= candidate.from || shown.has(`${candidate.from}:${to}`)) continue;
    decorations.push(
      Decoration.inline(candidate.from, to, { class: HIDDEN_TEXT_CLASS }),
      Decoration.widget(
        candidate.from,
        titleWidget(
          target,
          editable,
          spec.marks.some((mark) => mark.type.name === "link")
        ),
        {
          ignoreSelection: true,
          // Widgets with the same key are interchangeable, so ProseMirror keeps the drawn one.
          key: JSON.stringify([target, spec.marks, editable]),
          marks: spec.marks,
          side: 1,
          stopEvent: () => true,
        }
      )
    );
  }
  return DecorationSet.create(doc, decorations);
}

function sameTargets(
  left: ReadonlyMap<string, ReferenceTarget | null>,
  right: ReadonlyMap<string, ReferenceTarget | null>
): boolean {
  if (left.size !== right.size) return false;
  for (const [key, target] of left) {
    const other = right.get(key);
    if (other === undefined) return false;
    if (target === null || other === null) {
      if (target !== other) return false;
    } else if (
      target.href !== other.href ||
      target.path !== other.path ||
      target.title !== other.title ||
      target.titleKey !== other.titleKey ||
      target.length !== other.length
    ) {
      return false;
    }
  }
  return true;
}

/** Whether two sets of references say the same thing, so a host that hands over a fresh value on
 *  every render of its own redraws nothing. */
function sameReferences(left: EditorReferences, right: EditorReferences): boolean {
  return (
    left.scan === right.scan &&
    left.onKeys === right.onKeys &&
    sameTargets(left.targets, right.targets) &&
    sameTargets(left.mentions, right.mentions)
  );
}

function dispatchReferenceChange(view: EditorView, change: ReferenceChange): void {
  if (view.isDestroyed) return;
  view.dispatch(view.state.tr.setMeta(referencesKey, change).setMeta("addToHistory", false));
}

/**
 * The editor's reference titles, drawn without a change to the stored text. The host's reader
 * reads each text block (code blocks, code spans and a link's own text left out) and says what
 * each candidate's key resolves to, and what each mention draws; both reach the plugin through
 * `setReferences`, as a host's change marks reach `./editor-change-decorations.ts`, because a
 * host class written on editor DOM is read back as an edit. A resolved candidate is drawn as its
 * target's title, a link to the target's own address carrying `data-dispatch-href` (words and no
 * link for a target that opens no page), with its stored text hidden; while the focused editor's
 * selection touches it, the stored text shows instead, so an author edits what is stored. A
 * change of access (`setReferenceAccess`) redraws the titles into or out of the tab order. An
 * edit re-reads only the text blocks it touched; mapping carries the other candidates.
 */
export const referenceDecorationsPlugin = $prose(
  () =>
    new Plugin<ReferenceDecorations>({
      key: referencesKey,
      state: {
        init: () => ({
          candidates: DecorationSet.empty,
          decorations: DecorationSet.empty,
          editable: true,
          focused: false,
          keys: [],
          references: null,
          revealed: "",
        }),
        apply(transaction, value, _previous, state) {
          const change = transaction.getMeta(referencesKey) as ReferenceChange | undefined;
          const focused = change?.kind === "focus" ? change.focused : value.focused;
          const editable = change?.kind === "access" ? change.editable : value.editable;
          const references = change?.kind === "references" ? change.references : value.references;
          let candidates = value.candidates;
          if (references === null) {
            candidates = DecorationSet.empty;
          } else if (references.scan !== value.references?.scan) {
            candidates = allCandidates(state.doc, references);
          } else if (transaction.docChanged) {
            candidates = nextCandidates(value.candidates, transaction, references);
          }
          const answers = references ?? NO_ANSWERS;
          const revealed = revealedRanges(state, candidates, answers, focused);
          if (
            candidates === value.candidates &&
            references === value.references &&
            revealed === value.revealed &&
            editable === value.editable
          ) {
            return focused === value.focused ? value : { ...value, focused };
          }
          return {
            candidates,
            decorations: drawnDecorations(state.doc, candidates, answers, revealed, editable),
            editable,
            focused,
            keys:
              candidates === value.candidates ? value.keys : candidateKeys(candidates, value.keys),
            references,
            revealed,
          };
        },
      },
      view(view) {
        let keys = referencesKey.getState(view.state)?.keys ?? [];
        return {
          update(updated) {
            const state = referencesKey.getState(updated.state);
            if (state === undefined || state.keys === keys) return;
            keys = state.keys;
            state.references?.onKeys(keys);
          },
        };
      },
      props: {
        decorations: (state: EditorState) => referencesKey.getState(state)?.decorations,
        handleDOMEvents: {
          focus(view) {
            dispatchReferenceChange(view, { focused: true, kind: "focus" });
            return false;
          },
          blur(view) {
            dispatchReferenceChange(view, { focused: false, kind: "focus" });
            return false;
          },
        },
      },
    })
);

/** Draws the references `references` resolves over the document, in place of whatever was drawn
 *  before; `null` draws none. A value saying what the current one says changes nothing. */
export function setReferences(view: EditorView, references: EditorReferences | null): void {
  // A host writes its references from an effect, and an effect of a child runs after the effect
  // of the parent that tore this editor down (INFRA-2409), so the write can arrive here after
  // the view is gone; `dispatchReferenceChange` reads the same flag.
  if (view.isDestroyed) return;
  const current = referencesKey.getState(view.state);
  if (current === undefined || current.references === references) return;
  if (
    current.references !== null &&
    references !== null &&
    sameReferences(current.references, references)
  ) {
    return;
  }
  dispatchReferenceChange(view, { kind: "references", references });
}

/** Tells the titles whether the reader edits the view (`'edit'` access), which takes them out of
 *  the tab order, and redraws them where that changed. */
export function setReferenceAccess(view: EditorView, editable: boolean): void {
  if (view.isDestroyed || referencesKey.getState(view.state)?.editable === editable) return;
  dispatchReferenceChange(view, { editable, kind: "access" });
}
