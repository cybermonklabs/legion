import { afterEach, expect, test } from "bun:test";

import { theFirstBatchIsAwaited } from "../doc/first-batch";
import { forgetTheReach, reachingTheChrome, theChromeIsHere } from "./reaching-the-chrome";

// The press and the gate are one page's state, so each row leaves both as it found them: no
// press kept, and nothing awaited.
afterEach(() => {
  forgetTheReach();
  theFirstBatchIsAwaited(false);
});

// The drop's own job: a press neither part of this issue's chrome could do - a closed issue's
// Status pick, which refuses input - is let go once both parts have mounted past it, on the same
// issue. Kept past that, the next mount of either part does it: the header that mounts again on
// this page, or the next issue's header on a move through the cache. Nothing of the page's own
// leaving is in this row (`forgetTheReach`, `IssuePage.tsx`), so the drop is the only thing that
// lets the press go here.
test("a press neither part of the chrome could do is let go once both parts have mounted", () => {
  theFirstBatchIsAwaited(true);
  let tried = 0;
  const act = () => {
    tried += 1;
    return false;
  };

  reachingTheChrome(act);
  theChromeIsHere("header");
  theChromeIsHere("tabs");
  theChromeIsHere("header");

  // Once as it was pressed, once from the header's commit, once from the tab strip's - and not
  // from the header that mounted after both had.
  expect(tried).toBe(3);
});
