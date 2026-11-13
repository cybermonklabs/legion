import type { ReactNode } from "react";

import type { Artifact } from "../../api/types";
import { NODE_TYPE_NAMES, PRESET_NAMES, type Preset } from "./page-words";

/** The preset each of the two documents belongs to, which is what names its panel: the Checks
 *  preset keeps the verification document open, and the Plan preset the plan. */
const PRESET_OF: Readonly<Record<"plan" | "verification", Preset>> = {
  plan: "plan",
  verification: "checks",
};

/** One of the two role-document panels, Checks and Plan. They differ only in the address they
 *  answer and the component that frames the document; the panel itself and its lazy-mount
 *  guard are one thing, written once.
 *
 *  The Checks preset gives no frame: its checks are nodes of the tree, which draws each one
 *  once (R15), and the document is here because the preset keeps it open - its toolbar carries
 *  the version, the diff and the approval (R16), and a row being edited takes its editor.
 *
 *  The editor itself comes in as `children`, drawn for the document `drawn-documents.ts` says
 *  this panel holds: the panel decides nothing about which documents are open, so it can never
 *  disagree with the page about where a document's one editor is (`StagedDocument`).
 *
 *  It is a named panel and no `tabpanel`: the page has no tablist for one to belong to, and
 *  the name it carried was the id of a tab control nothing renders any more, which left the
 *  panel announced with no name at all (R21). The name is the preset's own, with what the
 *  panel holds. */
export function RoleDocumentPanel({
  Tab,
  activated,
  active,
  artifact,
  children,
  issueKey,
  tab,
}: {
  /** What frames the document under this address; without one the document is all the panel
   *  draws. */
  Tab?: (props: {
    artifact: Artifact | undefined;
    children?: ReactNode;
    issueKey: string;
  }) => ReactNode;
  activated: boolean;
  active: boolean;
  /** The document this preset is about, which its frame reads; the editor for it, where the
   *  page draws one, is `children`. */
  artifact: Artifact | undefined;
  children?: ReactNode;
  issueKey: string;
  tab: "verification" | "plan";
}): ReactNode {
  return (
    <section
      aria-hidden={!active}
      aria-label={`${PRESET_NAMES[PRESET_OF[tab]]} ${NODE_TYPE_NAMES.document}`}
      hidden={!active}
      id={`issue-${tab}-panel`}
    >
      {!activated ? null : Tab === undefined ? (
        children
      ) : (
        <Tab artifact={artifact} issueKey={issueKey}>
          {children}
        </Tab>
      )}
    </section>
  );
}
