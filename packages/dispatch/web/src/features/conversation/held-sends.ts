import { DELIVERY_CAPABILITIES } from "@legion/contracts";
import { ASK_URGENCIES } from "@legion/contracts/dispatch-tools";
import {
  hashKey,
  type MutationKey,
  partialMatchKey,
  type QueryClient,
  useQueryClient,
} from "@tanstack/react-query";
import { type Dispatch, type SetStateAction, useEffect, useRef, useState } from "react";

import { api } from "../../api/client";
import { agentMessagesQuery } from "../../api/queries";
import type { Actor } from "../../api/types";
import { pastDeadlineKey } from "../../hooks/useSending";
import { newSendKey } from "../../lib/send-key";
import { partyMention } from "../refs/party-references";
import { SEND_DEADLINE_MS, SendStoppedError } from "../writes/keyed-write";
import { type Shape, shape } from "../writes/tab-storage";
import {
  defineWriteKind,
  ofKind,
  useWriteSelection,
  type Write,
  type Writes,
  writesOf,
} from "../writes/write-store";
import { commentListKey, type ReplyTarget } from "./composer-model";
import {
  type Address,
  type CarriedAddress,
  type CarriedBlockIds,
  requestFor,
  SendMovedError,
  type SentDraft,
  type SentRequest,
  sameAddress,
  saveCommentEdit,
  sendAttempt,
} from "./send-request";

/** A held send's draft as its composer restores it: the text, a suggestion's replacement, and
 *  an ask's fields. */
export interface HeldDraft {
  readonly ask: SentRequest["ask"];
  readonly draft: SentDraft;
}

/** A composer's send as the write store holds it (`composerSend`): its name, which names its
 *  target - an issue's Conversation, a thread, an agent session, a margin compose's document - the
 *  request it went out as, and the draft its composer last held: the one sent, and, once refused,
 *  whatever the reader wrote over it before the composer unmounted. */
export interface ComposerSend {
  readonly mutationKey: readonly string[];
  readonly request: SentRequest;
  readonly draft: HeldDraft;
  /** The request a send the reader stopped went out as, while they have carried its draft to
   *  another address (`HeldSends.carry`): `request` is then the draft's at that address, under a
   *  key nothing has gone out under, and this one waits for the draft to come back, where the
   *  send is this one again, its key with it. */
  readonly stoppedSend?: SentRequest;
}

/** A comment edit as a send keeps it across a reload: which comment it saves, the ids its press
 *  carried (`SentRequest.edit.carried`), and the body it was written over (`base`), since the
 *  host that gave the page its save and the blocks the editor followed are both gone with the
 *  page. A record the build before this one kept holds the comment's id alone: that build kept no
 *  ids, so its edit restores carrying none, and the server matches its blocks by their text. */
type KeptEdit =
  | string
  | { readonly base?: string; readonly carried?: CarriedBlockIds; readonly id: string };

/** A send's request as it is kept across a reload: an edit by what it saves, not how, and its
 *  draft as the shape an older record may still hold (`KeptDraft`). */
type KeptSentRequest = Omit<SentRequest, "draft" | "edit"> & {
  readonly draft: KeptDraft;
  readonly edit: KeptEdit | null;
};

/** A composer's send as it is kept across a reload: each draft as the shape an older record may
 *  still hold (`KeptDraft`). */
interface KeptComposerSend {
  readonly draft: { readonly ask: SentRequest["ask"]; readonly draft: KeptDraft };
  readonly mutationKey: readonly string[];
  readonly request: KeptSentRequest;
  readonly stoppedSend?: KeptSentRequest;
}

/**
 * A composer's send that has not landed, as its composers read it: waiting its turn behind an
 * earlier send to its session, never sent (`waiting`, which the reader may discard), out
 * (`sending`, which counts one sent before that waits to go again), out past its deadline
 * (`late`), or refused and not yet sent again or discarded. It belongs to the composer's target,
 * named by the composer's send name, not to the composer: whatever unmounts the composer, it
 * stays, and a composer that mounts under the same name shows it.
 */
export type HeldSend = {
  readonly id: number;
  readonly mutationKey: readonly string[];
  readonly request: SentRequest;
  readonly draft: HeldDraft;
  /** When Send or Retry was pressed for it, which a reload keeps: a send kept for longer than
   *  `KEPT_SEND_RESUME_MS` is not made again on its own. */
  readonly pressedAt: number;
  /** Whether the person's sign-out in another tab holds it: nothing of it goes out until that tab
   *  says how the sign-out ended (`Write.signingOutElsewhere`). */
  readonly signingOutElsewhere: boolean;
} & (
  | { readonly status: "waiting" }
  | { readonly status: "sending" }
  | { readonly status: "late"; readonly stopping: boolean }
  | { readonly status: "refused"; readonly refusal: Error }
);

/** Whether `held` is a late send the reader may stop: it is being tried again under its key until
 *  Dispatch decides it, which a lasting outage can hold off for good. */
export function stoppable(held: HeldSend | undefined): boolean {
  return held?.status === "late" && !held.stopping;
}

/** What became of a held send: it landed, the server refused it, or the reader dropped its
 *  refusal. */
export interface HeldSendOutcome {
  readonly kind: "discarded" | "landed" | "refused";
  readonly send: HeldSend;
}

/** What a send that landed wrote, refreshed whether or not its composer is still on screen: the
 *  caches it is named by its own request. */
function refreshAfter(client: QueryClient, { anchor, owner }: SentRequest): void {
  if (owner.kind === "session") {
    void client.invalidateQueries({ queryKey: agentMessagesQuery(owner.sessionId).queryKey });
    return;
  }
  void client.invalidateQueries({ queryKey: commentListKey(owner) });
  void client.invalidateQueries({ queryKey: ["inbox"] });
  if (anchor !== undefined) {
    void client.invalidateQueries({ queryKey: ["artifact", anchor.artifact, "blocks"] });
  }
  if (owner.kind === "issue") {
    void client.invalidateQueries({ queryKey: ["events", owner.issueKey] });
    void client.invalidateQueries({ queryKey: ["issue", owner.issueKey] });
    // The Conversation lists the issue's messages itself, so a message the reader just wrote is
    // a new card in that list; a comment is one already, through the issue's comments above.
    void client.invalidateQueries({ queryKey: ["issue-messages", owner.issueKey] });
  } else {
    void client.invalidateQueries({ queryKey: ["artifact", owner.artifactId] });
    void client.invalidateQueries({ queryKey: ["project", owner.project, "artifacts"] });
  }
}

/** An ask's fields (`SentRequest.ask`). This shape and the draft's, the anchor's, the kind's, the
 *  owner's and the reply's below are what a composer's draft and its address are kept by: here,
 *  and in the drafts a composer keeps for a reload (`unsent-drafts.ts`). */
export const keptAsk = shape.record({
  multiple: shape.boolean,
  options: shape.array(
    shape.record({ description: shape.optional(shape.string), label: shape.string })
  ),
  urgency: shape.oneOf(...ASK_URGENCIES),
});

/** A mention as the build before this one kept it beside the draft: where its cosmetic words
 *  stand in the body, the delivery address they stood for, and those words. */
interface KeptMention {
  readonly end: number;
  readonly start: number;
  readonly target: string;
  readonly text: string;
}

/** A kept draft: its text, a suggestion's replacement, and the mentions a record the build
 *  before this one kept holds beside them. */
export interface KeptDraft extends SentDraft {
  readonly mentions?: readonly KeptMention[];
}

/** The text of a draft and a suggestion's replacement (`SentDraft`), as it is read back. Who it
 *  addresses is in its text (R4), so this build keeps nothing of the kind beside it; an older
 *  record's `mentions` are read all the same, because its body holds only the cosmetic
 *  `@Planner` its composer drew and the targets stood in that array, so dropping them would
 *  resend the draft to nobody with nothing said about it (`restoredDraft`). */
export const keptDraft = shape.record({
  body: shape.string,
  mentions: shape.optional(
    shape.array(
      shape.record({
        end: shape.number,
        start: shape.number,
        target: shape.string,
        text: shape.string,
      })
    )
  ),
  replacement: shape.string,
}) satisfies Shape<KeptDraft>;

const keptDelivery = shape.oneOf(...DELIVERY_CAPABILITIES);

/** The parent's author a reply quotes (`ReplyTarget.actor`), as Dispatch recorded it. */
const keptActor = shape.either(
  shape.record({ id: shape.string, kind: shape.oneOf("user") }),
  shape.record({
    id: shape.string,
    kind: shape.oneOf("session"),
    origin: shape.optional(
      shape.record({
        cwd: shape.optional(shape.string),
        host: shape.optional(shape.string),
        machine: shape.optional(shape.string),
        pane: shape.optional(shape.string),
        session_title: shape.optional(shape.string),
        tmux: shape.optional(shape.string),
      })
    ),
    owner: shape.optional(shape.string),
    service: shape.optional(shape.string),
  }),
  shape.record({ id: shape.string, kind: shape.oneOf("system") })
) satisfies Shape<Actor>;

/** The ids a kept edit carries, as its press worked them out (`carriedIdsOf`). */
const keptCarried = shape.record({
  base_revision: shape.number,
  blocks: shape.array(shape.record({ from: shape.number, id: shape.string })),
}) satisfies Shape<CarriedBlockIds>;

/** A kept edit: the comment it saves with the ids its press carried and the body it was written
 *  over, or, from the build before those ids were kept, that comment's id alone. */
const keptEdit = shape.either(
  shape.string,
  shape.record({
    base: shape.optional(shape.string),
    carried: shape.optional(keptCarried),
    id: shape.string,
  })
) satisfies Shape<KeptEdit>;

/** The words of a document a margin compose is about (`ComposerAnchor`): the mark the selection
 *  bar wrote over them, or, on a closed issue's document, which takes no mark, the quote and which
 *  reading of it the reader selected (`quoteAnchor`), where the server writes the mark. */
export const keptAnchor = shape.either(
  shape.record({
    artifact: shape.string,
    mark_id: shape.string,
    // The words the reader had around their selection and how many readings of them their
    // block held, which the recovery from `ANCHOR_MISSING` needs after a reload as much as
    // the quote beside it (`ComposerAnchor.placement`).
    placement: shape.optional(
      shape.record({
        blockId: shape.nullable(shape.string),
        prefix: shape.string,
        readings: shape.number,
        suffix: shape.string,
      })
    ),
    quote: shape.string,
  }),
  shape.record({ artifact: shape.string, occurrence: shape.number, quote: shape.string })
);

/** The kind a draft is written as (`ComposerKind`). */
export const keptKind = shape.oneOf("ask", "comment", "suggestion");

/** Whom a draft is written for (`ComposerOwner`). */
export const keptOwner = shape.either(
  shape.record({ issueKey: shape.string, kind: shape.oneOf("issue") }),
  shape.record({
    artifactId: shape.string,
    kind: shape.oneOf("artifact"),
    project: shape.string,
  }),
  shape.record({ kind: shape.oneOf("session"), sessionId: shape.string })
);

/** The reply a draft answers (`ReplyTarget`). */
export const keptReplyTo = shape.record({
  actor: keptActor,
  excerpt: shape.string,
  id: shape.string,
  mentions: shape.optional(
    shape.array(shape.record({ target: shape.string, title: shape.string }))
  ),
  parentKind: shape.oneOf("ask", "comment", "message"),
  thread: shape.optional(shape.record({ delivery: keptDelivery, target: shape.string })),
  to: shape.optional(shape.string),
});

const keptRequest = shape.record({
  anchor: shape.optional(keptAnchor),
  ask: keptAsk,
  draft: keptDraft,
  edit: shape.nullable(keptEdit),
  kind: keptKind,
  owner: keptOwner,
  pickedDelivery: shape.optional(keptDelivery),
  plainDelivery: keptDelivery,
  replyTo: shape.nullable(keptReplyTo),
  sendKey: shape.string,
}) satisfies Shape<KeptSentRequest>;

const keptComposerSend = shape.record({
  draft: shape.record({ ask: keptAsk, draft: keptDraft }),
  mutationKey: shape.array(shape.string),
  request: keptRequest,
  stoppedSend: shape.optional(keptRequest),
}) satisfies Shape<KeptComposerSend>;

/** A `MutationKey` of a composer's: a send name is built of strings alone, so it is kept as it is
 *  and read back the same. */
function sendName(mutationKey: MutationKey): readonly string[] {
  return mutationKey.map((part) => (typeof part === "string" ? part : JSON.stringify(part)));
}

/** Holds `mutationKey` in TanStack's mutation cache while `answer` is out, so a host's hold on the
 *  name (`useSending`) counts a send from the task that starts it on, whether or not its composer
 *  is still mounted. Run whether or not the browser says it is online (`networkMode: "always"`):
 *  a mutation TanStack paused offline would hold nothing. */
function hold(client: QueryClient, mutationKey: MutationKey, answer: () => Promise<unknown>): void {
  client
    .getMutationCache()
    .build(client, { mutationFn: answer, mutationKey, networkMode: "always" })
    .execute(undefined)
    .catch(() => {});
}

/** A kept edit as the send it belongs to saves it again: through the default save - the host
 *  that gave the page its save is gone with it - carrying the ids its press carried, so every
 *  attempt under the send's own key, however many reloads it outlives, sends the very body
 *  Dispatch judged under that key. A record from the build before those ids were kept carries
 *  none: that build kept no ids, and the server matches the edit's blocks by their text. */
function restoredEdit(edit: KeptEdit): NonNullable<SentRequest["edit"]> {
  return typeof edit === "string"
    ? { carried: undefined, id: edit, save: saveCommentEdit }
    : { base: edit.base, carried: edit.carried, id: edit.id, save: saveCommentEdit };
}

/** `request` as it is kept across a reload: its edit by what it saves. */
function keptRequestOf({ edit, ...request }: SentRequest): KeptSentRequest {
  return {
    ...request,
    edit: edit === undefined ? null : { base: edit.base, carried: edit.carried, id: edit.id },
  };
}

/** A kept request as the send it belongs to makes it again (`restoredEdit`, `restoredDraft`). */
function restoredRequest({ draft, edit, ...request }: KeptSentRequest): SentRequest {
  return {
    ...request,
    draft: restoredDraft(draft),
    edit: edit === null ? undefined : restoredEdit(edit),
  };
}

/** A kept draft as its composer takes it up again. A record the build before this one kept
 *  holds a `mentions` array and a body carrying only the cosmetic `@Name` that build drew, so
 *  each of those is written back as the party's mention (`[@Name](address)`, R4), its name
 *  written as literal words as a pick writes one (`partyLink`), which reads as the party's
 *  current name: the draft then reads as who it addresses and resends to them, where
 *  dropping the array would have delivered it to nobody with nothing said about it. A span is
 *  rewritten only while it still reads as the `@Name` that build accepted - the rule that build
 *  read its own mentions by (`survivingMentions`) - and the spans are written from the last to
 *  the first, so each is still the one its offsets named.
 *
 *  What is read back is a record, not a draft this build wrote: a target is written into the
 *  body only as the link `partyMention` makes of a delivery address (`party-references.ts`),
 *  so a target of no route's form keeps the words it stood as rather than becoming Markdown of
 *  its own, and offsets are rewritten only where they are a span of this text - `slice` counts
 *  a negative one from the end, and a resumed send whose `wire` is null sends the body this
 *  writes. Every store that keeps a draft across a reload reads it back through this: a held
 *  send's, and the drafts a composer keeps for an address (`unsent-drafts.ts`). */
export function restoredDraft({ body, mentions, replacement }: KeptDraft): SentDraft {
  if (mentions === undefined) {
    return { body, replacement };
  }
  const written = [...mentions]
    .sort((one, other) => other.start - one.start)
    .reduce((text, mention) => {
      const { end, start } = mention;
      // The words a mention was typed as, matched against the draft and drawn nowhere.
      const words = `@${mention.text}`;
      if (!Number.isInteger(start) || !Number.isInteger(end) || start < 0 || end > text.length) {
        return text;
      }
      if (text.slice(start, end) !== words) {
        return text;
      }
      const link = partyMention(mention.target, mention.text) ?? words;
      return `${text.slice(0, start)}${link}${text.slice(end)}`;
    }, body);
  return { body: written, replacement };
}

/**
 * A composer's send, one write of the store per press (`Writes`), kept across a reload with its
 * name, its request and its draft, and the request a stopped send went out as while its draft is
 * carried elsewhere (`stoppedSend`). A send to one session is ordered on that session
 * (`session:<id>`), so two presses reach it in press order, each once. A send that lands
 * refreshes what it wrote whether or not a composer is mounted (`refreshAfter`). Each send also
 * holds its name in TanStack's mutation cache - under `pastDeadlineKey` once past its deadline -
 * so a host's hold on the name (`useSending`) counts it; its `untilDeadline` hold lets go at the
 * deadline.
 */
export const composerSend = defineWriteKind<ComposerSend, KeptComposerSend>({
  keep: ({ draft, mutationKey, request, stoppedSend }) => ({
    draft,
    mutationKey,
    request: keptRequestOf(request),
    stoppedSend: stoppedSend === undefined ? undefined : keptRequestOf(stoppedSend),
  }),
  kept: keptComposerSend,
  landed: (client, write) => refreshAfter(client, write.detail.request),
  name: "composer",
  restore: ({ draft, mutationKey, request, stoppedSend }) => ({
    draft: { ask: draft.ask, draft: restoredDraft(draft.draft) },
    mutationKey,
    request: restoredRequest(request),
    stoppedSend: stoppedSend === undefined ? undefined : restoredRequest(stoppedSend),
  }),
  resume: ({ request }, wire) =>
    wire === null ? sendAttempt(request) : (bounds) => api.send(wire, bounds),
  started: (client, write, settled, late) => {
    const name = write.detail.mutationKey;
    if (late) {
      hold(client, pastDeadlineKey(name), () => settled);
      return;
    }
    // Past its deadline the request is not refused: it goes on under `pastDeadlineKey`, held
    // before the name lets go, so a hold on the name never drops between them.
    hold(client, name, () => {
      const { promise, reject, resolve } = Promise.withResolvers<unknown>();
      const deadline = setTimeout(() => {
        hold(client, pastDeadlineKey(name), () => settled);
        resolve(undefined);
      }, SEND_DEADLINE_MS);
      settled.then(resolve, reject).finally(() => clearTimeout(deadline));
      return promise;
    });
  },
});

const views = new WeakMap<Write, HeldSend>();

/** `write`, a composer's send, as its composers read it: one value while the write stands. A
 *  stopped send whose draft the reader has carried to another address reads as stopped there in
 *  words of its own (`SendMovedError`): the first write may have been delivered where it went. */
function heldSend(write: Write<ComposerSend>): HeldSend | undefined {
  let held = views.get(write);
  if (held !== undefined) return held;
  const base = {
    draft: write.detail.draft,
    id: write.id,
    mutationKey: write.detail.mutationKey,
    pressedAt: write.pressedAt,
    request: write.detail.request,
    signingOutElsewhere: write.signingOutElsewhere,
  };
  switch (write.status) {
    case "made":
      return undefined;
    case "queued":
      held = write.sent ? { ...base, status: "sending" } : { ...base, status: "waiting" };
      break;
    case "sending":
      held = { ...base, status: "sending" };
      break;
    case "late":
      held = { ...base, status: "late", stopping: write.stopping };
      break;
    case "refused":
      held = {
        ...base,
        refusal:
          write.detail.stoppedSend === undefined
            ? write.refusal
            : new SendMovedError(write.detail.stoppedSend),
        status: "refused",
      };
      break;
  }
  views.set(write, held);
  return held;
}

/** Every composer's send `rows` holds, as its composers read them. */
function heldIn(rows: readonly Write[]): HeldSend[] {
  const held: HeldSend[] = [];
  for (const row of rows) {
    const write = ofKind(row, composerSend);
    const view = write === undefined ? undefined : heldSend(write);
    if (view !== undefined) held.push(view);
  }
  return held;
}

/** The send `rows` holds under `mutationKey`, if any. */
function heldUnder(rows: readonly Write[], mutationKey: MutationKey): HeldSend | undefined {
  const name = hashKey(sendName(mutationKey));
  return heldIn(rows)
    .filter((held) => hashKey(held.mutationKey) === name)
    .at(-1);
}

/** `detail`, the record of a refused send whose refusal is `refusal`, carried to `to` by the
 *  reader's own move (`HeldSends.carry`): the request goes there, unless the send was stopped,
 *  whose key is used only at its own address. `at` is the words and kind of a margin compose the
 *  reader takes the draft into (`carryInto`), which is another address of the same owner. */
function carriedTo(
  { stoppedSend, ...detail }: ComposerSend,
  refusal: Error,
  to: CarriedAddress,
  at?: Pick<SentRequest, "anchor" | "kind">
): ComposerSend {
  const carried = { ...detail.request, ...to, ...at };
  const stopped = stoppedSend ?? (refusal instanceof SendStoppedError ? detail.request : undefined);
  if (stopped === undefined) return { ...detail, request: carried };
  if (sameAddress(stopped, carried)) return { ...detail, request: stopped };
  return {
    ...detail,
    request: { ...carried, sendKey: stoppedSend === undefined ? newSendKey() : carried.sendKey },
    stoppedSend: stopped,
  };
}

/** Whether the refused send `held` holds already what a keep of `draft` would write over it
 *  (`HeldSends.keep`): the same text and replacement, and the same ask - whether it takes more
 *  than one answer, its urgency, and each option's id, label and description, in order. Compared
 *  by value, since a composer builds each keep afresh from its fields. */
function holdsAlready(held: HeldSend, { ask, draft }: HeldDraft): boolean {
  const options = held.draft.ask.options;
  return (
    draft.body === held.draft.draft.body &&
    draft.replacement === held.draft.draft.replacement &&
    ask.multiple === held.draft.ask.multiple &&
    ask.urgency === held.draft.ask.urgency &&
    ask.options.length === options.length &&
    ask.options.every(
      (option, at) =>
        option.id === options[at]?.id &&
        option.label === options[at]?.label &&
        option.description === options[at]?.description
    )
  );
}

/**
 * The app's held sends, as its composers read and make them, over the one write store
 * (`writesOf`): one send per composer's name, which a composer that mounts under that name shows,
 * out or refused, whatever unmounted the one that made it.
 */
export class HeldSends {
  readonly #writes: Writes;

  constructor(writes: Writes) {
    this.#writes = writes;
  }

  /** Calls `listener` whenever any write the store holds changes. */
  subscribe(listener: () => void): () => void {
    return this.#writes.subscribe(listener);
  }

  get(mutationKey: MutationKey): HeldSend | undefined {
    return heldUnder(this.#writes.rows(), mutationKey);
  }

  /** Whether a send under `mutationKey` is out now - from the task that starts it on. */
  sendingNow(mutationKey: MutationKey): boolean {
    const held = this.get(mutationKey);
    return held !== undefined && held.status !== "refused";
  }

  /** Tells `listener` what becomes of every held send from now on. */
  onOutcome(listener: (outcome: HeldSendOutcome) => void): () => void {
    return this.#writes.onOutcome((outcome) => {
      const write = ofKind(outcome.write, composerSend);
      const send = write === undefined ? undefined : heldSend(write);
      if (send !== undefined) listener({ kind: outcome.kind, send });
    });
  }

  /** Sends `request` under `mutationKey` and holds it until it lands - unless a send under that
   *  name is out already, which refuses this one: a composer holds one draft. A refused send's
   *  entry gives way to the new one, its Retry, which is no Discard (`Writes.supersede`): a
   *  margin compose's mark stays in the document for it. The refused draft lives at its send's
   *  address, or at the one the reader carried it to (`carry`), so only a request to that address
   *  is its Retry (`sameAddress`): one from a composer its host has moved to another - another
   *  row's or thread's address, a Reply on a message - is refused too, and the refusal stands. */
  send(mutationKey: MutationKey, request: SentRequest): boolean {
    const held = this.get(mutationKey);
    if (held !== undefined && (held.status !== "refused" || !sameAddress(held.request, request))) {
      return false;
    }
    if (held !== undefined) this.#writes.supersede(held.id);
    const { owner } = request;
    this.#writes
      .run({
        detail: {
          draft: { ask: request.ask, draft: request.draft },
          mutationKey: sendName(mutationKey),
          request,
        },
        issue: owner.kind === "issue" ? owner.issueKey : undefined,
        key: request.sendKey,
        kind: composerSend,
        label: "Message",
        presentation: "composer",
        target:
          owner.kind === "session" && request.edit === undefined
            ? `session:${owner.sessionId}`
            : undefined,
        // A comment's edit is its `PATCH`; every other send is a `POST` (`sendAttempt`).
        method: request.edit === undefined ? "POST" : "PATCH",
        write: sendAttempt(request),
      })
      .catch(() => {});
    return true;
  }

  /** Stops trying the late send under `mutationKey` (Stop trying). Says whether it stopped one. */
  stop(mutationKey: MutationKey): boolean {
    const held = this.get(mutationKey);
    return held !== undefined && this.#writes.stop(held.id);
  }

  /** Drops the refused send under `mutationKey`, its draft and refusal with it, and says whether
   *  there was one. */
  discard(mutationKey: MutationKey): boolean {
    const held = this.get(mutationKey);
    return held !== undefined && this.#writes.discard(held.id);
  }

  /** Keeps what the reader wrote over a refusal. Its composer keeps its draft as it unmounts, as
   *  the page goes, and a short wait after it mounts as well as after each change, so one that
   *  mounts over a refusal keeps the very draft the send holds. A keep of what the send holds
   *  already writes nothing (`holdsAlready`): writing it again would only rewrite the entry and
   *  draw everything that reads the store again, for no change. */
  keep(mutationKey: MutationKey, draft: HeldDraft): void {
    const held = this.get(mutationKey);
    if (held?.status !== "refused") return;
    if (holdsAlready(held, draft)) return;
    this.#writes.amend(held.id, composerSend, (detail) => ({ ...detail, draft }));
  }

  /** Re-addresses the refused send under `mutationKey`: a margin compose's kind switch after a
   *  refusal retypes its mark, and its Retry sends to the retyped one. */
  readdress(mutationKey: MutationKey, request: SentRequest): void {
    const held = this.get(mutationKey);
    if (held?.status === "refused") {
      this.#writes.amend(held.id, composerSend, (detail) => ({ ...detail, request }));
    }
  }

  /** Carries the refused send under `mutationKey` from `from` to `to`, where the reader moved its
   *  composer with a control of the composer's own - Cancel reply, an Agents row's issue pick: the
   *  draft goes with that move, and its refusal with it, so its Retry and its next Send go from
   *  there. Only a refusal whose draft lives at `from`, the address the composer showed before
   *  the move, goes along (`sameAddress`): one the host moved the composer off stays with the
   *  address it was written for. A refusal Dispatch gave judged the send's key, so that Retry is
   *  a new write anyway. A send the reader stopped may have been delivered where it went, so its
   *  key is used only there: carried away, it keeps the request it went out as (`stoppedSend`)
   *  and the draft goes on under a key nothing has gone out under, its refusal saying the first
   *  may have been delivered (`SendMovedError`); carried back, it is that request again, under
   *  its own key. A move of the host's carries nothing (`send`). */
  carry(mutationKey: MutationKey, from: Address, to: CarriedAddress): void {
    const held = this.get(mutationKey);
    if (held?.status !== "refused" || !sameAddress(held.request, from)) return;
    this.#writes.amend(held.id, composerSend, (detail) => carriedTo(detail, held.refusal, to));
  }

  /** Carries the refused send under `mutationKey` to `to` as `carry` does, and into the composer
   *  named `into`: the reader's own move ended the composer the send was made in - the phone
   *  thread composer's Cancel reply - and `into` is the one that shows `to`, the docked composer
   *  at the issue's own address, which has taken the send's draft into its field. A composer
   *  takes a carried draft only while it holds no send of its own, out or refused
   *  (`DraftIntake`), so the send carried in is the only one under `into`. The margin carries a
   *  stopped compose set aside (`setAside`) into the newer compose that took its place this way,
   *  at its words and in its kind (`at`), when the reader presses Use my draft there. */
  carryInto(
    mutationKey: MutationKey,
    into: MutationKey,
    to: CarriedAddress,
    at?: Pick<SentRequest, "anchor" | "kind">
  ): void {
    const held = this.get(mutationKey);
    if (held?.status !== "refused") return;
    this.#writes.amend(held.id, composerSend, (detail) => ({
      ...carriedTo(detail, held.refusal, to, at),
      mutationKey: sendName(into),
    }));
  }

  /** Moves the refused send under `mutationKey` to the name `into`, and nothing else: its
   *  request, its key, its draft and its refusal stay as they are, since no address moves. The
   *  margin sets a stopped compose aside this way when a newer compose on its document takes its
   *  place, under a name no composer sends from (`marginComposeKeptKey`), and puts it back under
   *  its compose's own name the same way once no compose is open there (`useMarginSheet`). */
  setAside(mutationKey: MutationKey, into: MutationKey): void {
    const held = this.get(mutationKey);
    if (held?.status !== "refused") return;
    this.#writes.amend(held.id, composerSend, (detail) => ({
      ...detail,
      mutationKey: sendName(into),
    }));
  }

  /** Sends the refused send under `mutationKey` again as a press of its own. Every Retry but a
   *  mounted composer's is this one body - a session row's, and the margin's own re-send once it
   *  has written a refused compose's mark again and re-addressed the send to it - so what those
   *  carry is decided in one place: the request the store holds now, so the retry names the mark
   *  the margin wrote, and the draft the store holds, which the composer writes what the reader
   *  wrote over the refusal into. A composer on screen builds its own Retry from the fields the
   *  reader has in front of them instead (`MentionComposer`'s `sentRequest`), through the same
   *  `requestFor`. Unedited, that is the refused request itself, picked delivery mode included:
   *  it goes under its own key where the send ended without Dispatch's verdict, so Dispatch
   *  makes one message of the two. Edited, it is another message, in the mode this one went in,
   *  under a key of its own. A refusal Dispatch gave is its verdict on that send's key, so that
   *  attempt too goes out under a key of its own (`requestFor`): a second body under the
   *  answered key would be replayed that verdict or refused outright. */
  retry(mutationKey: MutationKey): void {
    const held = this.get(mutationKey);
    if (held?.status !== "refused") return;
    const { sendKey: _refused, ...addressed } = held.request;
    this.send(
      mutationKey,
      requestFor(held.request, held.refusal, {
        ...addressed,
        ask: held.draft.ask,
        draft: held.draft.draft,
      })
    );
  }
}

const facades = new WeakMap<Writes, HeldSends>();

/** The held sends that live with `client`: its write store's composer sends. */
export function heldSends(client: QueryClient): HeldSends {
  const writes = writesOf(client);
  let facade = facades.get(writes);
  if (facade === undefined) {
    facade = new HeldSends(writes);
    facades.set(writes, facade);
  }
  return facade;
}

export function useHeldSends(): HeldSends {
  return heldSends(useQueryClient());
}

/** The held send under `mutationKey`, re-rendering when it changes. */
export function useHeldSend(mutationKey: MutationKey | undefined): HeldSend | undefined {
  return useWriteSelection(mutationKey === undefined ? "" : hashKey(mutationKey), (rows) =>
    mutationKey === undefined ? undefined : heldUnder(rows, mutationKey)
  );
}

/** Every held send whose name starts with `prefix`, re-rendering only when one of them changes. */
export function useHeldSendsUnder(prefix: MutationKey): readonly HeldSend[] {
  return useWriteSelection(hashKey(prefix), (rows) =>
    heldIn(rows).filter((held) => partialMatchKey(held.mutationKey, prefix))
  );
}

/** Every held send to session `sessionId`, from any of its composers - the live view's presses,
 *  its Agents row's - in press order, re-rendering only when one of them changes. */
export function useHeldSendsTo(sessionId: string): readonly HeldSend[] {
  return useWriteSelection(`session:${sessionId}`, (rows) =>
    heldIn(rows).filter(
      ({ request: { edit, owner } }) =>
        owner.kind === "session" && owner.sessionId === sessionId && edit === undefined
    )
  );
}

/** Calls `listener` each time a held send lands or is refused while the caller is mounted. */
export function useHeldSendOutcomes(listener: (outcome: HeldSendOutcome) => void): void {
  const store = useHeldSends();
  const latest = useRef(listener);
  latest.current = listener;
  useEffect(() => store.onOutcome((outcome) => latest.current(outcome)), [store]);
}

/** The reply target a composer named `mutationKey` must show for a Reply tap on `target`: the
 *  comment or message a refusal is already held there, when one is and it names a different id,
 *  so the reader's Reply elsewhere redirects onto it instead of silently overwriting its draft -
 *  the docked composer's and the phone thread composer's `beginReply` both read this. The caller's
 *  own `sendingNow` guard must run first and refuse the tap while a send under `mutationKey` is
 *  out: an entry this reads can then only be refused or absent. */
export function redirectedReplyTarget(
  store: HeldSends,
  mutationKey: MutationKey,
  target: ReplyTarget
): ReplyTarget {
  const heldReply = store.get(mutationKey)?.request.replyTo;
  return heldReply !== undefined && heldReply !== null && heldReply.id !== target.id
    ? heldReply
    : target;
}

/** A host's reply for its composer under `mutationKey`, as state: it starts as the reply the send
 *  the store holds there - out, or refused - was sent with, so a host that mounts again shows that
 *  send under the reply it answers, and as none otherwise. */
export function useHeldReplyTo(
  mutationKey: MutationKey
): [ReplyTarget | null, Dispatch<SetStateAction<ReplyTarget | null>>] {
  const store = useHeldSends();
  return useState<ReplyTarget | null>(() => store.get(mutationKey)?.request.replyTo ?? null);
}
