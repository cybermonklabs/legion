import { expect, test } from "bun:test";

import { Session, startIssue } from "./issue-drafts-fixture";

// The title draft of both issue headers is one hook, and the bugs found in it have each been one
// particular order of typing, saving, escaping and answers from Dispatch. The orders a review or a
// sweep of the pages found are here, each as its own test, so a fix can be read against them by
// name; useIssueDrafts.exploration.test.ts plays every order up to five events long and checks,
// after every event, the seven things decisions 316, 324 and 332 say a reader can rely on.

// The three orders below were found in review, each a way the hook sent or showed a title the
// reader had not chosen.

test("an escape while a saved title is out, then an end with nothing typed, sends no second title", () => {
  const session = new Session();
  session.type("a");
  session.end();
  session.escape();
  session.end();
  session.land();
  expect(session.titlesSent()).toEqual(["a"]);
});

test("a draft typed back to the title out behind a queued save lets that save go as the write lands, and the end of the edit sends the draft", () => {
  const session = new Session();
  session.type("a");
  session.end();
  session.type("ab");
  session.end();
  session.type("a");
  session.land();
  expect({ title: session.drafts.title, sent: session.titlesSent() }).toEqual({
    title: "a",
    sent: ["a", "ab"],
  });
  session.land();
  session.end();
  expect(session.titlesSent()).toEqual(["a", "ab", "a"]);
});

test("the issue's title typed back, queued and refused gives way to another reader's title and a route save sends only the route", () => {
  const session = new Session();
  session.type("a");
  session.end();
  session.type(startIssue.title);
  session.end();
  session.refuse();
  session.otherReader("Plan the review");
  const title = session.drafts.title;
  const before = session.received.length;
  session.step(() => session.drafts.writeRoute("role:a"));
  session.step(() => session.drafts.requestRouteSubmit());
  expect({ title, routeSave: session.received.slice(before) }).toEqual({
    title: "Plan the review",
    routeSave: [{ route: "role:a" }],
  });
});

// A write that lands with no answer is passed the issue as it was (`next ?? issue` in both
// headers), which still holds the old title until the page reads the issue again. The field the
// write carried takes the value it carried, so the draft stays the title it sent and reads as not
// changed, rather than going back to the old title until that read lands.
test("a title write that lands with no answer keeps the title it sent", () => {
  const session = new Session();
  session.type("a");
  session.end();
  session.step(() => {
    session.drafts.onIssueSuccess(session.issue, { title: "a" });
    session.drafts.onIssueSettled(null);
  });
  expect({
    title: session.drafts.title,
    changed: session.drafts.titleDirty,
    sent: session.titlesSent(),
  }).toEqual({ title: "a", changed: false, sent: ["a"] });
});

// The order a sweep of the pages found (decision 324): a title saved behind the write out, and the
// editor opened again to type on with no end. The landing sends the saved title, the reader's
// last ended choice, and the field keeps what the reader is still typing. That title goes at the
// end of the edit, once the saved title's own write has landed.
test("a title typed on behind a queued save is not sent as the write ahead lands, and the end of the edit sends it", () => {
  const session = new Session();
  session.type("a");
  session.end();
  session.type("ab");
  session.end();
  // One character typed onto the end, with no end of the edit.
  session.type("abc");
  session.land();
  const landed = { title: session.drafts.title, sent: session.titlesSent() };
  session.land();
  session.end();
  expect({ landed, ended: session.titlesSent() }).toEqual({
    landed: { title: "abc", sent: ["a", "ab"] },
    ended: ["a", "ab", "abc"],
  });
});

// Typing ends no edit, so the issue's own title typed back behind a queued save, with the edit
// still open, neither chooses it nor withdraws the save (decision 324): the save goes as the write
// ahead of it lands, and the issue's title goes once the reader ends the edit on it.
test("the issue's title typed back behind a queued save, with no end, lets the save go as the write ahead lands, and the end of the edit sends it", () => {
  const session = new Session();
  session.type("a");
  session.end();
  session.type("ab");
  session.end();
  session.type(startIssue.title);
  session.land();
  expect({ title: session.drafts.title, sent: session.titlesSent() }).toEqual({
    title: startIssue.title,
    sent: ["a", "ab"],
  });
  session.land();
  session.end();
  expect(session.titlesSent()).toEqual(["a", "ab", startIssue.title]);
});

// A title saved while a status write is out waits behind it, one write of the issue's fields at a
// time. Another reader sets the issue to that very title meanwhile, so it has nothing left to
// send: sent as the status write lands, it would be a second write of a title the issue holds.
// The field stays the reader's ended choice all the same. Of the answers (a landing, a refusal,
// another reader's title), only the settle of the title's own write hands a typed or ended field
// back to the issue, never another reader's title (decision 332); the reader's own keystroke back
// to the issue's title, an end that chose nothing and an escape do too. No title write settles
// here and the reader makes none of those, so the field keeps that title as another reader sets
// the next one, and the end of the next edit would send it over theirs. Dispatch's hook before
// this change keeps the title in the field too, but sends it again as the status write lands:
// [status, title].
test("a title queued behind another field's write is not sent once another reader sets the issue to it, and the field keeps it, the reader's ended choice, through that reader's next title", () => {
  const session = new Session();
  session.step(() => session.drafts.requestStatusSubmit("done"));
  session.type("X");
  session.end();
  session.otherReader("X");
  const landed = { ...session.issue, status: "done" };
  session.step(() => {
    session.drafts.onIssueSuccess(landed, { status: "done" });
    session.drafts.onIssueSettled(null);
  });
  session.otherReader("Plan the review");
  expect({ title: session.drafts.title, sent: session.received }).toEqual({
    title: "X",
    sent: [{ status: "done" }],
  });
});

// A title saved behind a status write waits there while the reader types on, and another reader
// sets the issue to the reader's text and then to a title of their own. The draft stays the
// reader's text: moved on to the other reader's title, it showed a title the reader never typed,
// and the end of the edit would have sent it over the issue's.
test("a draft with a title queued behind another field's write keeps the reader's text as another reader's titles arrive", () => {
  const session = new Session();
  session.step(() => session.drafts.requestStatusSubmit("done"));
  session.type("t1");
  session.end();
  session.type("t2");
  session.otherReader("t2");
  session.otherReader("o1");
  const shown = session.drafts.title;
  session.land();
  session.land();
  expect({ shown, title: session.drafts.title, sent: session.titlesSent() }).toEqual({
    shown: "t2",
    title: "t2",
    sent: ["t1"],
  });
});
