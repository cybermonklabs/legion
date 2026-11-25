import { mentionDrawn } from "@legion/contracts";
import type { EditorReferences, OfferedReference, ReferenceTarget } from "@legion/proof-editor";
import { type ReactNode, useCallback, useEffect, useMemo, useState } from "react";

import { isPartyRoute } from "../refs/party-references";
import { useReferenceLabel, useReferenceTarget } from "../refs/reference-target";
import { RecordIssue } from "../refs/role-holders";
import { type DispatchReferenceRoute, referenceHref, shortForm } from "../refs/routes";
import { readTextReferences, type TextSource } from "../refs/text-references";
import { useTextReferences } from "../refs/useTextReferences";
import type { EditorHandle } from "./editor";

/**
 * What a document editor shows for the references its own text holds. The live editor and a past
 * version both draw them, and both read them the one way the dashboard reads what a text cites
 * (`features/refs/text-references.ts`), so a bare issue key, a `#id`, a bare `dispatch://`
 * reference and a link whose text is its own address resolve under exactly the rules a rendered
 * body resolves them under. A party's mention - a link whose words open with the mention sign and
 * whose address names a party (`@legion/contracts`'s `referenceReading`) - reads as that sign and
 * the party's current name (`mentionDrawn`), as it does in a rendered body (`RefLink`); a link
 * whose text its author wrote another way keeps those words. Each title links to its target's
 * page, and the editor opens it through the page's link opener, as it opens every link of the
 * document (`features/refs/link-open.ts`).
 *
 * The editor owns the positions and this owns the grammar: the plugin hands each text block's own
 * text to `scan`, which is the one reader, and offers the distinct references it found, each a
 * key and whether a mention names it (`OfferedReference`), and this resolves them - their
 * existence, their document's question map, the whole id of a prefix, whether a mention's
 * address names a party - and hands back a title for each: a plain candidate's in `targets` and
 * a mention's in `mentions`. The document's stored text never changes (`@legion/proof-editor`'s
 * `src/reference-decorations.ts`).
 */

/** The empty answers, shared so an editor whose text cites nothing keeps one identity across
 *  renders and never hands the plugin a fresh value that says the same thing. */
const NO_OFFERS: readonly OfferedReference[] = [];
const NO_NAMES: Readonly<Record<string, ReferenceName>> = {};

/** One reference the editor's text holds, as the host resolved it. */
interface ResolvedReference {
  /** The key the editor offered it by. */
  readonly words: string;
  readonly reference: string;
  readonly route: DispatchReferenceRoute;
  readonly length: number | undefined;
  /** Whether the editor offered it as a party's mention, which reads as the mention sign and the
   *  party's name and is answered in `EditorReferences.mentions`. */
  readonly mention: boolean;
}

/** What one reference's target answered: its current name, and whether Dispatch answered that it
 *  exists with no name of its own - a session it holds a record of that the registry does not
 *  name. */
interface ReferenceName {
  readonly title: string | undefined;
  readonly known: boolean;
}

/** One resolved reference's name, reported upward as its queries answer. The queries are the
 *  ones every other surface reads a reference's title through, so a title the hover card has
 *  already read costs the editor nothing, and the words are the label every link to a reference
 *  shows (`useReferenceLabel`): a deleted post's deleted line, an edited post's ` · edited`. */
function ReferenceTitle({
  onName,
  reference,
  route,
}: {
  onName(reference: string, name: ReferenceName | undefined): void;
  reference: string;
  route: DispatchReferenceRoute;
}): null {
  const target = useReferenceTarget(route, { text: false });
  const { known } = target;
  // The editor titles a reference standing as its own address alone, which reads by the naming
  // rule (`useReferenceLabel`).
  const { text: title } = useReferenceLabel(route, target);
  useEffect(() => {
    onName(reference, title === undefined && !known ? undefined : { known, title });
  }, [known, onName, reference, title]);
  // A reference the text no longer holds takes its name with it.
  useEffect(() => () => onName(reference, undefined), [onName, reference]);
  return null;
}

/** The title the editor draws over `resolved`, or undefined while it draws none. An issue reads
 *  as its key and title, and a mention as a rendered body draws one (`@legion/contracts`'s
 *  `mentionDrawn`): the mention sign and the party's name. A target Dispatch answered exists
 *  with no title of its own - a session it holds a record of that the registry does not name -
 *  is drawn as a link in the words of its reference, as a comment draws it, and its mention as
 *  the sign and the session's short form, so its author's words never pass for a current name;
 *  every other target is drawn once its title answers, and never without one. */
function drawnTitle(
  { mention, reference, route }: ResolvedReference,
  name: ReferenceName | undefined
): string | undefined {
  const title = name?.title;
  if (mention) {
    return name === undefined ? undefined : mentionDrawn(name, shortForm(route));
  }
  if (title !== undefined) {
    return route.kind === "issue" ? `${route.key} ${title}` : title;
  }
  return name?.known === true ? reference : undefined;
}

/**
 * Feeds `editor` the references its text holds and their titles, and renders nothing of its own.
 * `source` is the record the text is - the document itself - whose question map the server
 * answers for, so a `#id` in the editor resolves exactly as it does in a rendered body
 * (`useTextReferences`). Text of no record resolves no `#id`.
 */
export function DocumentReferences({
  editor,
  source,
}: {
  editor: EditorHandle | undefined;
  source: TextSource | undefined;
}): ReactNode {
  // The references the editor last offered: every candidate its reader found in the document,
  // which is what this resolves - never the markdown the page loaded, which a typed reference is
  // not in.
  const [offered, setOffered] = useState<readonly OfferedReference[]>(NO_OFFERS);
  const [names, setNames] = useState<Readonly<Record<string, ReferenceName>>>(NO_NAMES);

  const onKeys = useCallback((keys: readonly OfferedReference[]) => {
    setOffered((current) =>
      current.length === keys.length &&
      current.every(
        (one, index) => one.key === keys[index]?.key && one.mention === keys[index]?.mention
      )
        ? current
        : [...keys]
    );
  }, []);
  const onName = useCallback((reference: string, name: ReferenceName | undefined) => {
    setNames((current) => {
      const now = current[reference];
      if (now?.title === name?.title && now?.known === name?.known) {
        return current;
      }
      const next = { ...current };
      if (name === undefined) {
        delete next[reference];
      } else {
        next[reference] = name;
      }
      return next;
    });
  }, []);
  // Every candidate the one reader finds, resolved or not: what a candidate resolves to is the
  // host's answer below, so facts the editor never sees cannot change where a candidate is.
  const scan = useCallback(
    (text: string) =>
      readTextReferences(text).references.map((reference) => ({
        end: reference.end,
        key: reference.text,
        start: reference.start,
      })),
    []
  );

  // A mention's key is its link's whole address, which needs no fact to read and is a mention
  // only where it names a party; every other key is read by the one reader with its facts.
  const { mentioned, readable } = useMemo(() => {
    const mentions: ResolvedReference[] = [];
    const others: string[] = [];
    for (const { key, mention } of offered) {
      if (!mention) {
        others.push(key);
        continue;
      }
      const [found] = readTextReferences(key).references;
      if (
        found?.reference !== undefined &&
        found.route !== undefined &&
        isPartyRoute(found.route)
      ) {
        mentions.push({
          length: undefined,
          mention: true,
          reference: found.reference,
          route: found.route,
          words: key,
        });
      }
    }
    return { mentioned: mentions, readable: others };
  }, [offered]);

  // Each candidate is read as the whole mention it was, on its own: the editor's words never
  // become one text, so nothing another word holds can end this one (`readTextCandidates`).
  const { reading } = useTextReferences(readable, source);
  const resolved = useMemo(() => {
    const found: ResolvedReference[] = [...mentioned];
    for (const reference of reading.references) {
      if (reference.reference === undefined || reference.route === undefined) {
        continue;
      }
      found.push({
        length: reference.named,
        mention: false,
        reference: reference.reference,
        route: reference.route,
        words: reference.text,
      });
    }
    return found;
  }, [mentioned, reading]);

  const { mentions, targets } = useMemo(() => {
    const answers = {
      mentions: new Map<string, ReferenceTarget | null>(),
      targets: new Map<string, ReferenceTarget | null>(),
    };
    for (const { key, mention } of offered) {
      (mention ? answers.mentions : answers.targets).set(key, null);
    }
    for (const one of resolved) {
      const title = drawnTitle(one, names[one.reference]);
      if (title === undefined) {
        continue;
      }
      const { length, reference, route } = one;
      // The page the title links to, which a new tab opens; a click opens the reference itself
      // through the page's link opener (`features/refs/link-open.ts`), which reads the spec's
      // slug a pinned block of it needs, as this path does not. A person opens no page, so their
      // name is drawn as words and no link (R5).
      const path = referenceHref(route);
      (one.mention ? answers.mentions : answers.targets).set(one.words, {
        href: path === undefined ? undefined : reference,
        length,
        path,
        title,
        titleKey: route.kind === "issue" ? route.key : undefined,
      });
    }
    return answers;
  }, [names, offered, resolved]);

  const references: EditorReferences = useMemo(
    () => ({ mentions, onKeys, scan, targets }),
    [mentions, onKeys, scan, targets]
  );
  useEffect(() => {
    editor?.setReferences(references);
    return () => editor?.setReferences(null);
  }, [editor, references]);

  // One mount per reference, since a title is a query of its own; a reference two candidates
  // write differently is read once.
  const read = useMemo(() => {
    const distinct = new Map<string, DispatchReferenceRoute>();
    for (const { reference, route } of resolved) {
      if (!distinct.has(reference)) {
        distinct.set(reference, route);
      }
    }
    return [...distinct];
  }, [resolved]);
  // Each title reads its role by the issue the document is on, as a rendered body of the same
  // text does (`RecordIssue`).
  return (
    <RecordIssue.Provider value={source?.issue}>
      {read.map(([reference, route]) => (
        <ReferenceTitle key={reference} onName={onName} reference={reference} route={route} />
      ))}
    </RecordIssue.Provider>
  );
}
