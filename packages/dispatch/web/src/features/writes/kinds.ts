/**
 * Every kind of write the page makes (`defineWriteKind`), loaded with the app's shell rather than
 * with the page that makes it, so a reload's kept writes find their kind when the store takes them
 * up (`Writes.resume`) whichever page the reader reloads on: a composer's send, a broadcast, a
 * pick of an issue's priority, assignee or repository, a pin, a table epic creation, a priority's
 * title or description and a new priority, and every plain write (`write-store.ts`).
 */
import "../agents/broadcast-queue";
import "../conversation/held-sends";
import "../issue/issue-field-writes";
import "../issue/pin-write";
import "../priorities/priority-writes";
import "../project/issues-table-recovery";
