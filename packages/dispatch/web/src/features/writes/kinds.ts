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
 * the take-up, so it never goes out ahead of the writes the take-up puts in its line
 * (`Writes.run`). Nothing else waits for this chunk (`Writes.resume`).
 */
import "../agents/broadcast-queue";
import "../conversation/held-sends";
import "../issue/issue-field-writes";
import "../issue/pin-write";
import "../priorities/priority-writes";
import "../project/issues-table-recovery";
import { kindsRegistered } from "./write-store";

// Every import above has defined its kind by now, so a take-up still waiting ends here,
// whichever load evaluated this module: the take-up's own ask, or another load once that ask
// failed (`kindsRegistered`).
kindsRegistered();
