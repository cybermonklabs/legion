import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Ask } from "../../api/types";
import {
  calloutSuccessBodyText,
  linkHoverText,
  linkText,
  pageInkText,
  pageLineBorder,
  pageWordsText,
  quoteAccentBorder,
  quoteBodyText,
  successQuoteAccentBorder,
  textPrimaryOnSuccessCallout,
} from "../../theme/classes";
import { blockFragment } from "../refs/block-reference";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import type { AskPalette, TextMarks } from "./ask-text";

/** The colours of the anchor's document link and of its quote by the palette the ask body's
 *  host hands (`AskPalette`), on the open body (`surface`) and in the answered record
 *  (`success`), as `issue-page/DecisionActions.tsx` keeps its own: `main`'s are the classes
 *  every surface of today's pages draws, and on the new issue page the document's name is the
 *  ink, as every link of the ask body is, and the quote the page's words inside its rule, as
 *  the page's own Change quotes a decision. */
const TONES = {
  main: {
    success: {
      link: textPrimaryOnSuccessCallout,
      quote: `${successQuoteAccentBorder} ${calloutSuccessBodyText}`,
    },
    surface: {
      link: `${linkText} ${linkHoverText}`,
      quote: `${quoteAccentBorder} ${quoteBodyText}`,
    },
  },
  page: {
    success: { link: pageInkText, quote: `${pageLineBorder} ${pageWordsText}` },
    surface: { link: pageInkText, quote: `${pageLineBorder} ${pageWordsText}` },
  },
} as const;

/** The document named by an ask's quote anchor, including its stable block fragment when known. */
export function AskAnchorLink({ ask }: { ask: Ask }): ReactNode {
  const { anchor, anchor_artifact: artifact } = ask;
  if (anchor === null || artifact === undefined) {
    return null;
  }
  const fragment =
    anchor.block_id === undefined || anchor.block_id === null ? "" : blockFragment(anchor.block_id);
  if (ask.issue_key !== null) {
    const route = artifact.primary
      ? buildIssuePath({ key: ask.issue_key, kind: "spec" })
      : buildIssuePath({ key: ask.issue_key, kind: "artifact", slug: artifact.slug });
    return (
      <Link className="underline" to={`${route}${fragment}`}>
        {artifact.name}
      </Link>
    );
  }
  return (
    <Link
      className="underline"
      to={`${buildProjectPath({ kind: "document", project: artifact.project, slug: artifact.slug })}${fragment}`}
    >
      {artifact.name}
    </Link>
  );
}

/** The document link and quoted passage that give an anchored ask its context: the document's
 *  name and its words, which a host that hands `marks` (`AskBodyProps.marks`) gets inside the
 *  mark of a value, in the palette the host hands (`TONES`). */
export function AskAnchorHeader({
  ask,
  inBlock,
  marks,
  palette = "main",
  quoteSpacing = "compact",
  tone,
}: {
  ask: Ask;
  inBlock: boolean;
  marks?: TextMarks;
  /** The palette it draws in (`TONES`): `main`'s, unless the ask body's host hands the page's. */
  palette?: AskPalette;
  quoteSpacing?: "compact" | "roomy";
  tone: "success" | "surface";
}): ReactNode {
  const { anchor, anchor_artifact: artifact } = ask;
  if (inBlock || anchor === null) {
    return null;
  }
  const tones = TONES[palette][tone];
  return (
    <>
      {artifact === undefined ? null : (
        <p
          className={
            tone === "success"
              ? `mb-2 font-medium ${tones.link}`
              : `mb-2 text-sm font-medium ${tones.link}`
          }
          {...marks?.authorText}
        >
          <AskAnchorLink ask={ask} />
        </p>
      )}
      <blockquote
        className={`${quoteSpacing === "roomy" ? "mb-3" : "mb-2"} border-l-2 pl-3 ${tones.quote}`}
        {...marks?.authorText}
      >
        {/* The document's own words, as they are: the reading list's `anchor-quote`. */}
        {anchor.quote}
      </blockquote>
    </>
  );
}
