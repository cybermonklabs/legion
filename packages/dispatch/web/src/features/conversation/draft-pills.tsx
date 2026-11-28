import type { ReferenceReading } from "@legion/contracts";
import { type ReactNode, useMemo, useState } from "react";

import { useSettled } from "../../hooks/useSettled";
import {
  badgeMed,
  controlHoverBorder,
  referencePillBorder,
  surfaceRecessedBg,
} from "../../theme/classes";
import { draftReading, useLoadedEngine } from "../refs/parsed-references";
import { inLinkWords } from "../refs/party-references";
import { RefLink } from "../refs/RefLink";
import { issueSummaryQuery, summarySpec, useReferenceQuery } from "../refs/reference-target";
import { RecordIssue } from "../refs/role-holders";
import { type DispatchReferenceRoute, referenceHref } from "../refs/routes";
import { readTextReferences, type TextSource, textSourceOf } from "../refs/text-references";
import { SEARCH_DEBOUNCE_MS } from "../refs/useRefPicker";
import { useTextReferences } from "../refs/useTextReferences";
import type { ComposerAnchor, ComposerOwner, ReplyTarget } from "./composer-model";

/** One pill under the draft: a reference as a reader of the stored text will read it - what it
 *  names, the page it opens, the words the draft wrote it in and what those words read as
 *  (`RefLink`), and the issue the draft will be written on, which a role reference is read
 *  against (`RecordIssue`). */
interface ReferencePill {
  readonly href: string;
  readonly issue: string | undefined;
  readonly reads: ReferenceReading;
  readonly reference: string;
  readonly route: DispatchReferenceRoute;
  readonly text: string;
}

/** The source a draft's text will have once it is written (`draftSource`): the record itself, or
 *  the issue whose spec it is written on, which only that issue's summary names. */
export type DraftSource = TextSource | { readonly needsSpecOf: string } | undefined;

/**
 * The record a `#id` in the draft resolves against once the draft is written, as the server will
 * read it (`refs.ScopeOf`, through `GET /api/v1/references/questions`): a comment or an ask on a
 * document is that document; a reply to a comment is in its parent's scope; a comment or an ask
 * anchored to a document's text is that document; and anything else written on an issue - a
 * comment, an ask, a reply to a message - is its issue's spec, whose id the issue's summary
 * names (`needsSpecOf`). A message to one session belongs to no document and resolves no `#id`,
 * as the stored message does not. An edit reads none: it is a compact box, which shows no pills.
 * This is the one decision: the pills read the spec's summary only where it answers that. Each
 * source names the issue the draft is written on, and a project document's names none.
 */
export function draftSource(
  owner: ComposerOwner,
  replyTo: ReplyTarget | null,
  anchor: ComposerAnchor | undefined
): DraftSource {
  if (owner.kind === "session") return undefined;
  if (owner.kind === "artifact") {
    return { id: owner.artifactId, issue: undefined, kind: "artifact" };
  }
  if (replyTo?.parentKind === "comment") {
    return { id: replyTo.id, issue: owner.issueKey, kind: "comment" };
  }
  if (anchor?.node !== undefined) {
    return { needsSpecOf: owner.issueKey };
  }
  if (replyTo === null && anchor !== undefined) {
    return { id: anchor.artifact, issue: owner.issueKey, kind: "artifact" };
  }
  return { needsSpecOf: owner.issueKey };
}

/**
 * The pills under a draft, read as a reader of the stored text will read it: through the one
 * reader and the facts the renderer's link pass is held to (`useTextReferences`), with the record
 * the write will have (`draftSource`), so a bare key whose issue exists and a `#id` its document
 * declares get the pill the stored record's link is, and one that names nothing gets none. A
 * host that shows no pills - a compact box, an inline reply or an edit - passes an empty draft,
 * which asks nothing.
 *
 * They read the draft once the typing rests, as the picker's search does: a key typed a letter at
 * a time (`CORE-1`, `CORE-12`, `CORE-123`) is one read of the key the reader stopped at, not one
 * for every key it passes through. A draft emptied - sent, discarded - takes its pills with it at
 * once. Only a draft that writes a `#id` on an issue's own spec reads which document that is, and
 * that read is a reference's (`useReferenceQuery`): one that failed is not asked again because a
 * composer mounts with such a draft, or because a draft comes to name a `#id` after it mounted.
 *
 * One pill per reference the renderer would link - none in code and none in another link's
 * words, where no renderer links one (`inLinkWords`), and none for a picture, which the record
 * draws as the picture - at its first appearance, however often the body repeats it.
 */
export function useDraftPills(draft: string, source: DraftSource): readonly ReferencePill[] {
  const text = useSettled(draft, draft === "" ? 0 : SEARCH_DEBOUNCE_MS);
  const writesQuestion = useMemo(() => readTextReferences(text).needs.questions, [text]);
  const specOf =
    source !== undefined && "needsSpecOf" in source && writesQuestion
      ? source.needsSpecOf
      : undefined;
  const issueSummary = useReferenceQuery(
    specOf === undefined ? undefined : issueSummaryQuery(specOf)
  );
  const { reading } = useTextReferences(
    text,
    source !== undefined && "needsSpecOf" in source
      ? textSourceOf("artifact", summarySpec(issueSummary?.data)?.id, source.needsSpecOf)
      : source
  );
  // The issue the draft is written on, which every pill's reference is read against.
  const issue =
    source === undefined ? undefined : "needsSpecOf" in source ? source.needsSpecOf : source.issue;
  const { engine } = useLoadedEngine();
  return useMemo(() => {
    // The pills read the draft through the parse (`draftReading`), so none is drawn until the
    // engine has loaded: a load state, as a question's label reads raw until its engine loads.
    if (engine === undefined) return [];
    const pills: ReferencePill[] = [];
    const seen = new Set<string>();
    for (const found of reading.references) {
      if (
        found.reference === undefined ||
        found.route === undefined ||
        found.inCode ||
        inLinkWords(text, found) ||
        seen.has(found.reference)
      ) {
        continue;
      }
      // The one gate between a reference the writer typed and an `href` (`routes.ts`'s
      // `appHref`): a reference that opens no page of this dashboard - a person, whom nothing
      // shows - gets no pill, rather than one whose empty `href` would navigate to this very
      // page and take the draft with it.
      const drawn = draftReading(engine, text, { ...found, route: found.route });
      const href = referenceHref(found.route);
      if (drawn.picture || href === undefined) {
        continue;
      }
      seen.add(found.reference);
      pills.push({
        href,
        issue,
        reads: drawn.reads,
        reference: found.reference,
        route: found.route,
        text: drawn.words,
      });
    }
    return pills;
  }, [engine, issue, text, reading]);
}

/** A pill drawn by the renderer's own portal content (`RefLink`) inside the pill's link, so it
 *  reads as the same reference reads in the stored record: the target's current name, a party's
 *  mention as `@` and that name, an author's own words as they are, and words with no link once
 *  Dispatch answers that the target does not exist. `RefLink` keeps the link's href as its own
 *  reading has it - a pinned block of a spec once the issue names the spec's slug. It is read on
 *  the issue the draft is written on, as the stored record will be (`RecordIssue`), so a role
 *  that issue's collaborators record reads as the stored record's link does. */
function DraftReferencePill({ pill }: { pill: ReferencePill }): ReactNode {
  const [anchor, setAnchor] = useState<HTMLAnchorElement | null>(null);
  return (
    <a
      className={`rounded-full border px-2 py-1 text-xs font-medium ${referencePillBorder} ${surfaceRecessedBg} ${badgeMed.text} ${controlHoverBorder}`}
      href={pill.href}
      ref={setAnchor}
    >
      <RecordIssue.Provider value={pill.issue}>
        <RefLink
          anchor={anchor ?? undefined}
          reads={pill.reads}
          route={pill.route}
          text={pill.text}
        />
      </RecordIssue.Provider>
    </a>
  );
}

/** The draft's pills (`useDraftPills`), or nothing where it names nothing a pill opens. */
export function DraftPills({ pills }: { pills: readonly ReferencePill[] }): ReactNode {
  if (pills.length === 0) return null;
  return (
    <section aria-label="References" className="flex flex-wrap gap-2">
      {pills.map((pill) => (
        <DraftReferencePill key={pill.reference} pill={pill} />
      ))}
    </section>
  );
}
