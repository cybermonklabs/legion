import { type QueryClient, type QueryKey, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useMemo, useSyncExternalStore } from "react";

import { rowsOf } from "../../api/event-refresh";
import type { ArtifactText, Comment, DisplayTree } from "../../api/types";
import { type ComposerOwner, commentListKey } from "../conversation/composer-model";
import { postDeletion } from "../refs/post-marks";
import { wordsOf } from "../refs/tree-fragment";
import { liveTextQueryOptions } from "./live-text-query";

/** The words of a document a draft was written for: the document, the words as the editor named
 *  the reader's selection of them, and the pending inserts they lie in (a margin compose's
 *  `ComposerAnchor`): an empty list where they lie in none, and no list at all on a draft or held
 *  send an older build kept, which never reads as an empty one. */
export interface QuotedWords {
  readonly artifact: string;
  readonly insertMarks?: readonly string[];
  readonly quote: string;
}

/** What the page can say now of the words a draft was written for (`useQuoteInDocument`): the
 *  document still holds them (`"held"`), they left it (`"gone"`), no read that can tell has
 *  landed yet (`"loading"`), or each such read failed (`"unchecked"`). Only `"held"` draws them. */
export type QuoteInDocument = "gone" | "held" | "loading" | "unchecked";

/** What a line drawn in place of a draft's quote says of the words, for each state that draws
 *  none of them: the composer's "The words this draft was for …", and the leads of a kept draft,
 *  "… for other words, which …" and "… for words that …". */
export const UNDRAWN_QUOTE_WORDS = {
  gone: "are no longer in the document",
  loading: "are still loading",
  unchecked: "cannot be checked now",
} as const satisfies Record<Exclude<QuoteInDocument, "held">, string>;

/** `text` with each run of whitespace read as one space. The editor names a selection's words with
 *  a line break between two blocks (`textBetween` in `dispatch-action-bar.ts`), where the
 *  document's words hold one space (`wordsOf`), so the two are compared in this form. */
function spaced(text: string): string {
  return text.replace(/\s+/gu, " ").trim();
}

/** Each text read's words (`documentWords`), worked out once for the read whichever surfaces ask:
 *  the open compose and each line that leads a kept draft read the same one. */
const wordsOfRead = new WeakMap<ArtifactText, string>();

/** The document's words as its text read gives them (`wordsOf` of `markdown_tree`); null for a
 *  read that carries no tree. */
function documentWords(text: ArtifactText): string | null {
  const tree = text.markdown_tree as DisplayTree | undefined;
  if (tree === undefined) return null;
  let words = wordsOfRead.get(text);
  if (words === undefined) {
    words = spaced(wordsOf(tree));
    wordsOfRead.set(text, words);
  }
  return words;
}

/** Whether `queryKey` names a comment list: an issue's (`["comments", key, …]`) or a document's
 *  (`["artifact", id, "comments"]`), the reads of the comments family a delete's correction writes
 *  its mark into (`markCorrectedPost`), which the server answers with the deleted comment in its
 *  place. */
function isCommentList(queryKey: QueryKey): boolean {
  return queryKey[0] === "comments" || (queryKey[0] === "artifact" && queryKey[2] === "comments");
}

/** Whether a comment list of `client`'s cache (`isCommentList`) lists, deleted, a comment
 *  anchored in the document `artifact` by one of `marks`: the comment of one of the inserts the
 *  words lie in. */
function holdsDeletedInsert(
  client: QueryClient,
  artifact: string,
  marks: readonly string[]
): boolean {
  return client
    .getQueryCache()
    .getAll()
    .some(
      ({ queryKey, state }) =>
        isCommentList(queryKey) &&
        rowsOf<Comment>(state.data).some(
          (comment) =>
            comment.anchor !== null &&
            comment.anchor.artifact_id === artifact &&
            marks.includes(comment.anchor.mark_id) &&
            postDeletion(comment) !== null
        )
    );
}

/** What the page's comment lists say of the words: a list holds deleted the comment of an insert
 *  they lie in (`holdsDeletedInsert`); else the margin's own list, `margin`, holds an answer, so
 *  the page has read the comments and holds none of them deleted; no list can answer for them -
 *  the margin's read failed with no answer, there is no such list to read, or the words name no
 *  list of inserts at all; or the list holds no answer yet. */
type ListedWords = "deleted" | "failed" | "read" | "unread";

function listedWords(
  client: QueryClient,
  artifact: string,
  marks: readonly string[] | undefined,
  margin: QueryKey | undefined
): ListedWords {
  // Words that name no list of inserts (`marks` undefined, not empty: a draft or held send an
  // older build kept, or a selection the editor held no document for) may lie in one whose
  // comment is deleted, and a list cannot show that it holds none of theirs deleted: only a read
  // of the document's text checks them.
  if (marks === undefined) return "failed";
  if (marks.length > 0 && holdsDeletedInsert(client, artifact, marks)) return "deleted";
  const state = margin === undefined ? undefined : client.getQueryState(margin);
  if (state?.data !== undefined) return "read";
  return margin === undefined || state?.status === "error" ? "failed" : "unread";
}

/**
 * Which of the four states of `QuoteInDocument` the words a draft was written for (`words.quote`)
 * are in, in the document `words` names, as the page holds them now: held, gone, loading or
 * unchecked. This read asks for nothing itself.
 *
 * The words can leave the document while the draft waits. A selection inside another comment's
 * pending insert suggestion quotes that comment's words, and the comment's delete rejects the
 * insert and takes them out of the document: they are then a deleted post's words, which no
 * surface may draw. So a draft whose words the document no longer holds is drawn without them.
 *
 * The first fact, which decides alone: the words lie in a pending insert (`words.insertMarks`)
 * whose comment a comment list of the page's cache holds deleted (`holdsDeletedInsert`). The
 * delete's correction writes that into every such list the page holds (`markCorrectedPost`), and
 * the server answers them with the deleted comment in its place, so the page knows it whatever it
 * has read of the document's text, and however old that read is: gone then.
 *
 * Otherwise, words that left the document by any other edit: the live text read every open
 * document keeps (`liveTextQueryOptions`, `ProofDocument`), which the page reads again after an
 * event of a comment anchored there (`eventRefreshTargets`). For words that name a list of
 * inserts, only a read that landed after `anchoredAt`, when the draft's compose took the words,
 * says they left. No keystroke and no update from a collaborator reads the text again, so a read
 * from before then can lack words the reader had just typed and selected, which are on screen;
 * this build names a list for every selection the editor makes (`insertMarksIn`), so such words
 * name one. A draft with no such time - one an older release kept - counts as taken before every
 * read. Words that name no list of inserts - a draft or held send an older build kept, or a
 * selection the editor held no document for - are judged by the read's words alone, however old
 * it is: they may lie in an insert whose comment is deleted, and a composer that mounts at them
 * with an empty field takes its mount as their time, later than a read that lacks them. Gone
 * only where the page holds such a read of the document's text and it does not hold the words.
 *
 * Until then the words are drawn only once the page holds an answer that can check them: that
 * text read, or the comment list the margin of the draft's `owner` reads (`useMarginItems`),
 * which holds none of the inserts deleted. The list answers only for words that name the inserts
 * they lie in, an empty list too (`insertMarksIn`): a draft or held send an older build kept
 * names no list, and its words may lie in an insert whose comment is deleted, which no list can
 * show, so for it only the text read answers. A page that has just loaded - a reload, with drafts
 * the tab kept - holds neither, and a delete's correction drops the text read
 * (`dropUnmarkedText`): while the page reads them the words are loading, and once each read that
 * can answer has failed with no answer they cannot be checked now. Neither draws the words; the
 * draft's own words stay.
 */
export function useQuoteInDocument(
  { artifact, insertMarks, quote }: QuotedWords,
  anchoredAt: number | undefined,
  owner: ComposerOwner
): QuoteInDocument {
  const client = useQueryClient();
  const margin = owner.kind === "session" ? undefined : commentListKey(owner);
  // What the comment lists say changes only with one of them, and a change to one sends an
  // event for it.
  const subscribe = useCallback(
    (changed: () => void) =>
      client.getQueryCache().subscribe(({ query }) => {
        if (isCommentList(query.queryKey)) {
          changed();
        }
      }),
    [client]
  );
  const listed = useSyncExternalStore(subscribe, () =>
    listedWords(client, artifact, insertMarks, margin)
  );
  const {
    data: words,
    dataUpdatedAt,
    isLoadingError: textFailed,
  } = useQuery({
    ...liveTextQueryOptions(artifact),
    enabled: false,
    select: documentWords,
  });
  return useMemo((): QuoteInDocument => {
    if (listed === "deleted") return "gone";
    if (words !== undefined) {
      return words === null ||
        (insertMarks !== undefined && anchoredAt !== undefined && dataUpdatedAt <= anchoredAt) ||
        words.includes(spaced(quote))
        ? "held"
        : "gone";
    }
    if (listed === "read") return "held";
    return textFailed && listed === "failed" ? "unchecked" : "loading";
  }, [anchoredAt, dataUpdatedAt, insertMarks, listed, quote, textFailed, words]);
}
