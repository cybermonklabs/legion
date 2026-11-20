import { type ReferenceReading, referenceReading, type WordsFormatting } from "@legion/contracts";
import { useEffect, useState } from "react";

import {
  linkMarkOf,
  linkWordsAt,
  loadedEngine,
  type MarkdownEngine,
  warmMarkdownRenderer,
} from "./markdown-engine";
import { isPartyRoute, linkOf } from "./party-references";
import type { DispatchReferenceRoute } from "./routes";
import type { TextReference } from "./text-references";

/**
 * The shared Markdown engine once it has loaded, and undefined until then. A component that
 * reads a text's words through the parse, rather than draws the text, asks for the load when it
 * mounts and draws again when the load lands; an engine that cannot load leaves it undefined,
 * which is the load state the component shows (`markdown-engine.ts` reports the cause).
 */
export function useLoadedEngine(): MarkdownEngine | undefined {
  const [engine, setEngine] = useState(loadedEngine);
  useEffect(() => {
    if (engine !== undefined) {
      return;
    }
    let live = true;
    warmMarkdownRenderer().then(
      () => {
        if (live) setEngine(loadedEngine());
      },
      () => undefined
    );
    return () => {
      live = false;
    };
  }, [engine]);
  return engine;
}

/**
 * The words of the inline link `written` - the link's own source, `[words](address)` - and how
 * they are formatted, as every reader of a parsed text reads them (`markdown-engine.ts`'s
 * `linkWordsAt`, by `@legion/contracts`'s `wordsFormatting`): the parser undoes each escape,
 * decodes each entity and pairs formatting as CommonMark pairs it, so a draft reads a link the
 * way the record written from it will. A link's syntax is local to its paragraph, so the link
 * alone parses as it does inside the whole text, but for words that run onto a line a container
 * prefixes (`> `), whose prefix the link alone keeps. Undefined where the parse holds no link.
 */
export function parsedLinkWords(
  engine: MarkdownEngine,
  written: string
): { words: string; formatting: WordsFormatting } | undefined {
  let read: { words: string; formatting: WordsFormatting } | undefined;
  engine.parse(written)?.descendants((node, _position, parent, index) => {
    if (read !== undefined) {
      return false;
    }
    const link = linkMarkOf(node);
    if (link !== undefined && parent !== null) {
      read = linkWordsAt(parent, index, link);
    }
    return read === undefined;
  });
  return read;
}

/** A reference of a text nothing has parsed yet - a draft - as the record written from it will
 *  read it: `words`, the words of the link whose address it is (`party-references.ts`'s
 *  `linkOf`) read through the parse (`parsedLinkWords`), or its address where it stands on its
 *  own; what they read as, by the one rule every reader takes (`@legion/contracts`'s
 *  `referenceReading`); and whether the link is a picture's syntax, which the record draws as the
 *  picture rather than as a link. */
export interface DraftReading {
  readonly picture: boolean;
  readonly reads: ReferenceReading;
  readonly words: string;
}

/** How `reference`, read out of `body` by the shared reader (`readTextReferences`), reads in the
 *  record written from `body` (`DraftReading`): what a draft's pill draws. */
export function draftReading(
  engine: MarkdownEngine,
  body: string,
  reference: Pick<TextReference, "end" | "start" | "text"> & {
    readonly route: DispatchReferenceRoute;
  }
): DraftReading {
  const link = linkOf(body, reference);
  const parsed = link === undefined ? undefined : parsedLinkWords(engine, link.markdown);
  return {
    picture: link?.picture === true,
    reads: referenceReading({
      address: reference.text,
      party: isPartyRoute(reference.route),
      words: parsed?.words,
      wordsFormatting: parsed?.formatting,
    }),
    words: parsed?.words ?? reference.text,
  };
}
