// Define in a document (prototype, `e2e/ask-card-demo/README.md`): the reader selects words of a
// document and presses Define on the selection bar, or `d`; what the words mean there is drawn
// beside them (`DefinePopover`). The live document and a past version both offer it, wherever
// they are drawn.
import { type ReactNode, type RefObject, useCallback, useState } from "react";

import type { Artifact } from "../../api/types";
import type { DefinitionPlace } from "../inbox/ask-rewrite";
import { DefinePopover, type SelectedWords, selectedWordsIn } from "../inbox/DefinePopover";
import type { KeyBinding } from "../shell/keymap";
import type { EditorHandle } from "./editor";

/** Where words of `artifact` stand, at `version` where the text shown is one: an issue's
 *  document. A project's document has no issue to read meanings from, so it offers no Define. */
export function documentPlace(
  artifact: Pick<Artifact, "issue_key" | "slug"> | undefined,
  version: number | null | undefined
): DefinitionPlace | undefined {
  if (artifact?.issue_key == null) return undefined;
  return {
    issue: artifact.issue_key,
    document: artifact.slug,
    ...(version == null ? {} : { version }),
  };
}

/** The editor's `onDefine` for the document drawn under `root`, and the answer it opens. */
export function useDocumentDefine(
  root: RefObject<HTMLElement | null>,
  place: DefinitionPlace | undefined
): { define: () => void; popover: ReactNode } {
  const [shown, setShown] = useState<SelectedWords | undefined>(undefined);
  const define = useCallback(() => {
    const selected = selectedWordsIn(root.current);
    if (selected !== null) setShown(selected);
  }, [root]);
  return {
    define,
    popover:
      shown === undefined || place === undefined ? null : (
        <DefinePopover
          column={shown.column}
          onClose={() => setShown(undefined)}
          place={place}
          rect={shown.rect}
          words={shown.words}
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
