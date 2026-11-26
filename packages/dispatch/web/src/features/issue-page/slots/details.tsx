import { useQuery } from "@tanstack/react-query";
import { type ReactNode, type RefCallback, useCallback, useState } from "react";
import { createPortal } from "react-dom";

import { primarySpec } from "../../../api/issue-cache";
import { userStateQuery } from "../../../api/queries";
import { labelsControlName } from "../../issue/IssueLabels";
import { stateForIssue } from "../../issue/pins";
import { useMargin } from "../../margin/margin-context";
import { useKeymap } from "../../shell/keymap";
import { DETAILS_ROW_ATTRIBUTE, useOpenDetailsAt } from "../details-rows";
import type { IssueField } from "../IssueFieldForms";
import { IssueMarginCards, IssueMarginPanel } from "../IssueMargin";
import { DETAILS_ATTRIBUTE, useFillMarginSlot } from "../margin-slot";
import { NoTooltips } from "../NoTooltips";
import { ANSWER_LINES, CONTROL_LABELS } from "../page-words";
import type { SlotProps } from "../slot-props";
import { useMarkComposePlace } from "../text-comments/TextMargin";
import { usableControl } from "../usable-control";
import { useIssueHeaderWrites } from "../useIssueHeaderWrites";

/** The key `l`'s name in the list of keys. */
const LABELS_CONTROL_NAME = "Edit labels";

/**
 * Details' properties: the one panel `IssueMarginPanel` draws, filled into the shell's Details
 * drawer.
 *
 * The panel is drawn once for the life of the page into its own stage, an element this slot
 * holds, and the stage is appended to the host the drawer draws (`IssueMarginCards`). So the
 * panel keeps one React parent while Details closes and opens, and nothing in it - an open
 * editor, a list the reader opened - is thrown away. The drawer's own cards, the threads of an
 * open document and its composer, stand above the sections only while a document's editor is
 * on the page (`useMargin().documentBridge`, which `ProofDocument.tsx` sets): the panel holds no
 * list of the conversation, which is the outline.
 *
 * Where the page draws the column of comments on text, which takes a block's editor's composes
 * (`useMarkComposePlace`), the editor's threads and its composer are comments on text that stand
 * in that column, and Details draws none of them (`EditorCards`).
 *
 * Slots share no state, so this slot keeps which of the two editors of its rows is open and
 * makes its own set of the issue's writes; the header's writes reach the same keyed line of the
 * issue (`useIssueHeaderWrites.ts`). It binds `l`, which opens Details at the Labels row with
 * the labels' control pressed.
 */
export function DetailsSlot({ issue }: SlotProps): ReactNode {
  const state = useQuery(userStateQuery()).data;
  const writes = useIssueHeaderWrites(issue, stateForIssue(state, issue.key));
  // The one editor open in a row of Details - the parent's or the route's - which the rows'
  // own controls and the add menu open.
  const [editingField, setEditingField] = useState<IssueField | null>(null);
  // The panel's stage, one for the life of the page (above): state and not a memo, since React
  // may throw a memo's value away, and a second stage would draw the panel again and close an
  // open editor.
  const [stage] = useState(() => {
    const element = document.createElement("div");
    // No box of its own: the panel is a child of the host that holds it.
    element.className = "contents";
    return element;
  });
  // The drawer's `aside` the stage sits in, where a control elsewhere on the page asks for a row
  // of the panel and which Add watches (`IssueMarginPanel`): read as the host takes the stage,
  // since the panel is drawn before any host holds it.
  const [drawerBox, setDrawerBox] = useState<HTMLElement | null>(null);
  // The host takes the stage as it is drawn, and lets go of it only while it still holds it.
  const holdPanel = useCallback(
    (host: HTMLElement | null) => {
      if (host === null) return;
      host.append(stage);
      setDrawerBox(host.closest<HTMLElement>(`aside[${DETAILS_ATTRIBUTE}]`));
      return () => {
        if (stage.parentElement !== host) return;
        stage.remove();
        setDrawerBox(null);
      };
    },
    [stage]
  );
  const openDetailsAt = useOpenDetailsAt();
  useKeymap("issue", [
    {
      // The labels' control is in Details' Labels row: the key opens Details there and presses
      // it, as a reader does.
      id: "labels",
      keys: "l",
      label: LABELS_CONTROL_NAME,
      run: () => openDetailsAt("labels", true),
      when: () => {
        // The control, by the name it has for the issue as it is now (`labelsControlName`):
        // "Add a label to CORE-1" while it has no label, "Edit labels" once it has one, and
        // "Saving labels of CORE-1" while a save of them is out, which the panel's labels read
        // from these same writes. While that save is out the control is busy, `aria-disabled` and
        // refusing to open, so `l` acts only while the control takes a press (`usableControl`),
        // which counts no refusal from Details being closed: the key opens Details at the row
        // first. A closed Development is a refusal: it keeps the row on the page but `hidden`
        // (`details-leaves.tsx`'s `PropertySection`), so `l` is off while Development is closed.
        const name = labelsControlName(issue.key, {
          labels: issue.labels ?? [],
          saving: writes.labels.isPending,
          words: { ANSWER_LINES, CONTROL_LABELS },
        });
        return usableControl(
          document.querySelector(
            `[${DETAILS_ROW_ATTRIBUTE}="labels"] button[aria-label="${CSS.escape(name)}"]`
          )
        );
      },
    },
  ]);
  return (
    <>
      {createPortal(
        <NoTooltips>
          <IssueMarginPanel
            documentArtifact={primarySpec(issue)}
            drawerBox={drawerBox}
            editingField={editingField}
            isClosed={issue.closed_at !== null}
            issue={issue}
            onEditingField={setEditingField}
            state={state}
            writes={writes}
          />
        </NoTooltips>,
        stage
      )}
      <DetailsFill holdPanel={holdPanel} />
    </>
  );
}

/**
 * Details' fill of the shell's margin slot: the host that holds the panel's stage, with the
 * margin's own cards above it while a document's editor is on the page. It reads the margin's
 * context, which changes with every hover, selection and placement of the margin, so it is a
 * component of its own and those changes draw it alone, not the panel.
 */
function DetailsFill({ holdPanel }: { holdPanel: RefCallback<HTMLElement> }): null {
  const documentUp = useMargin().documentBridge !== undefined;
  const fill = useCallback(
    (conversation: ReactNode) => (
      <IssueMarginCards
        conversation={documentUp ? conversation : undefined}
        // The editor's own cards stand in the column of comments on text wherever the page draws
        // one (`EditorCards`), as every comment on text does. The override is optional, as the
        // prop is, so it stands after the attribute above and takes nothing while no editor is up.
        {...((documentUp ? { conversation: <EditorCards>{conversation}</EditorCards> } : {}) as {
          readonly conversation?: ReactNode;
        })}
        panelHost={holdPanel}
      />
    ),
    [documentUp, holdPanel]
  );
  useFillMarginSlot(fill);
  return null;
}

/**
 * The margin's own cards of the document's editor on the page - its threads and its composer -
 * which Details draws only while no column of comments on text takes the editor's composes
 * (`useMarkComposePlace`): where one does, they are comments on text, drawn in the column like
 * every other (R5, R6), and the editor opens its composer there.
 */
function EditorCards({ children }: { readonly children: ReactNode }): ReactNode {
  return useMarkComposePlace() === undefined ? children : null;
}
