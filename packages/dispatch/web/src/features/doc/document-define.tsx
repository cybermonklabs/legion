// Define in a document (INFRA-3231's R7, INFRA-3552's R4): the reader selects words of a
// document and presses Define on the selection bar, `d`, or the editor's own chord; what the
// words mean there is drawn beside them (`DefinePopover`). The live document and a past version
// both offer it, wherever they are drawn, for a document of an issue and of a project alike.
import { type ReactNode, type RefObject, useCallback, useRef, useState } from "react";

import type { Artifact, Ask } from "../../api/types";
import type { DefinitionPlace } from "../inbox/ask-rewrite";
import { DefinePopover, type SelectedWords, selectedWordsIn } from "../inbox/DefinePopover";
import type { KeyBinding } from "../shell/keymap";
import type { EditorHandle } from "./editor";

/** Where words of `artifact` stand: the document itself, by its id, which Dispatch reads at its
 *  latest version. Only a document has words to define: a file, an image and an agent
 *  conversation's upload offer no Define, as `POST /api/v1/definitions` refuses them. */
export function documentPlace(
  artifact: Pick<Artifact, "id" | "kind" | "session_id"> | undefined
): DefinitionPlace | undefined {
  if (artifact === undefined || artifact.kind !== "doc" || artifact.session_id !== null) {
    return undefined;
  }
  return { document: artifact.id };
}

/** The decision block the selection starts in, by its block id, where it starts in one. */
function selectedBlock(root: HTMLElement | null): string | undefined {
  const selection = window.getSelection();
  if (root === null || selection === null || selection.rangeCount === 0) return undefined;
  const start = selection.getRangeAt(0).startContainer;
  const element = start instanceof Element ? start : start.parentElement;
  const frame = element?.closest<HTMLElement>('[data-proof-block-type="ask"][data-block-id]');
  return frame != null && root.contains(frame)
    ? (frame.getAttribute("data-block-id") ?? undefined)
    : undefined;
}

/**
 * The editor's `onDefine` for the document drawn under `root`, and the answer it opens. Words in
 * one of the document's decision blocks are words of its ask (`asks`, by `block_id`), which get
 * an ask's definition, the asking agent's answer with it; every other word is the document's.
 * `define` is one function for the view's life, since the editor is built with it: it reads
 * `place` and `asks` as they stand when it is pressed.
 */
export function useDocumentDefine(
  root: RefObject<HTMLElement | null>,
  place: DefinitionPlace | undefined,
  asks: readonly Pick<Ask, "block_id" | "id">[] = []
): { define: () => void; popover: ReactNode } {
  const [shown, setShown] = useState<
    { readonly place: DefinitionPlace; readonly words: SelectedWords } | undefined
  >(undefined);
  const latest = useRef({ asks, place });
  latest.current = { asks, place };
  const define = useCallback(() => {
    const selected = selectedWordsIn(root.current);
    const { asks: current, place: where } = latest.current;
    if (selected === null || where === undefined) return;
    const block = selectedBlock(root.current);
    const ask = block === undefined ? undefined : current.find((one) => one.block_id === block);
    setShown({ place: ask === undefined ? where : { ask_id: ask.id }, words: selected });
  }, [root]);
  return {
    define,
    popover:
      shown === undefined ? null : (
        <DefinePopover
          column={shown.words.column}
          onClose={() => setShown(undefined)}
          place={shown.place}
          rect={shown.words.rect}
          words={shown.words.words}
        />
      ),
  };
}

/** `d`, on the Define button's own path (`defineSelection`), while words of the document drawn
 *  under `root` are selected and no text field has the focus. */
export function defineKey(
  root: RefObject<HTMLElement | null>,
  editor: () => EditorHandle | undefined
): KeyBinding {
  return {
    id: "define-selection",
    keys: "d",
    label: "Define selected words",
    // The palette's own field takes the selection, so a row of it would define nothing.
    palette: false,
    run: () => editor()?.defineSelection(),
    when: () => editor() !== undefined && selectedWordsIn(root.current) !== null,
  };
}
