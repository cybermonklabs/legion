/**
 * Every kind of write the page makes (`defineWriteKind`), so a reload's kept writes find their
 * kind when the store takes them up (`Writes.resume`) whichever page the reader reloads on: a
 * composer's send, a broadcast, a pick of an issue's priority, assignee or repository, a pin, a
 * table epic creation, a priority's title or description and a new priority, and every plain
 * write (`write-store.ts`).
 *
 * A chunk of its own, not in the entry (`loadWriteKinds`, `write-hosts.tsx`): the take-up is the
 * one reader of the kinds a page has not imported itself, since a component that makes a write
 * imports its kind's module, which registers the kind as it is evaluated. The app asks for this
 * chunk as the person signs in, and the take-up waits until its kinds are registered, so it never
 * drops a kept write for want of its kind; every write pressed meanwhile but a sign-out waits for
 * the take-up while the page stays, and when the page goes only those whose line holds a kept
 * write the take-up will send wait (`Writes.run`). Nothing else waits for this chunk
 * (`Writes.resume`).
 *
 * The composer's kind is finished here. Its definition in the entry (`held-sends.ts`'s
 * `composerSend`) reads back only a send this build kept, whose drafts each hold whether their
 * text names a party (`OutgoingDraft`): a send a build before this one kept holds no such
 * answer, and working it out takes the grammar of references, which this chunk may import and
 * the entry may not. So this module defines the kind again under the same name, with the entry's
 * hooks and a reader of those older sends too, which works the answer out from each draft's text
 * as that build's composer would have at the press. It replaces the entry's definition by name
 * as this module is evaluated (`defineWriteKind`), before `kindsRegistered` below ends a waiting
 * take-up, so every kept send a take-up restores is restored by this one; every press, before or
 * after, makes its write with the same hooks.
 */
import "../agents/broadcast-queue";
import {
  type ComposerSend,
  composerSend,
  type KeptComposerSend,
  type KeptDraft,
  keptComposerSendOf,
  keptDraft,
  keptOutgoingDraft,
  restoredComposerSend,
  restoredDraft,
} from "../conversation/held-sends";
import type { OutgoingDraft } from "../conversation/send-request";
import "../issue/issue-field-writes";
import "../issue/pin-write";
import "../priorities/priority-writes";
import "../project/issues-table-recovery";
import { outgoingDraft } from "../refs/party-references";
import { shape } from "./tab-storage";
import { defineWriteKind, kindsRegistered } from "./write-store";

/** A composer's send as this build or a build before it kept it. */
type KeptByAnyBuild = KeptComposerSend<OutgoingDraft | KeptDraft>;

/** A kept draft as the send holds it (`OutgoingDraft`): this build's as it was kept, and a build
 *  before this one's with its old mentions written back (`restoredDraft`) and whether that text
 *  names a party read from it (`outgoingDraft`). */
function outgoingOf(draft: OutgoingDraft | KeptDraft): OutgoingDraft {
  return keptOutgoingDraft(draft) ? draft : outgoingDraft(restoredDraft(draft));
}

/** A kept request with its draft as the send holds it (`outgoingOf`). */
function outgoingRequestOf({
  draft,
  ...request
}: KeptByAnyBuild["request"]): KeptComposerSend["request"] {
  return { ...request, draft: outgoingOf(draft) };
}

defineWriteKind<ComposerSend, KeptByAnyBuild>({
  ...composerSend,
  kept: keptComposerSendOf(shape.either(keptOutgoingDraft, keptDraft)),
  restore: ({ draft, mutationKey, request, stoppedSend }) =>
    restoredComposerSend({
      draft: { ask: draft.ask, draft: outgoingOf(draft.draft) },
      mutationKey,
      request: outgoingRequestOf(request),
      stoppedSend: stoppedSend === undefined ? undefined : outgoingRequestOf(stoppedSend),
    }),
});

// Every import above has defined its kind by now, so a take-up still waiting ends here,
// whichever load evaluated this module: the take-up's own ask, or another load once that ask
// failed (`kindsRegistered`).
kindsRegistered();
