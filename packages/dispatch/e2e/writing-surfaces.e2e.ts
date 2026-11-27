import { test } from "./test";
import { BUS_BOXES, coveredBoxes } from "./writing-boxes";
import { driveWritingBox } from "./writing-drivers";

/**
 * The check that every box has the picker: the same picker in every box a person writes in.
 *
 * One `test()` per covered box of `DISPATCH_WRITING_BOXES`, generated from that list, so a box
 * the list gains with no way to drive it fails a row of its own and a driver for a box the list
 * no longer holds fails every row. In each box the three doors open over the same rows in each
 * door's own order, the words of a list with no rows are the same, Enter takes the highlighted
 * row and the pick lands at the caret with the words after it untouched, and the line under the
 * box says what a pick does on that box's write - whether a picked agent or role is told, that a
 * picked person is not, and, where nobody picked is told, what does - in words the rows write
 * out rather than read from the list the box reads them from (`hintWords`). Which people a box
 * lists is the one thing its own row decides (`issueRows`), and each box is held to its row's
 * answer.
 *
 * The check that delivery is unchanged is held in the same rows: each box whose write delivers is
 * sent through its own send control, which its driver hands to `expectRow`, and the fake Envoy
 * listener takes exactly one frame for the party picked in it, in the mode the composer's rule
 * gives, read again once the server has settled the delivery (`expectSend`). A send can close
 * the box or end the reply a later step starts, so each driver that goes on after one waits for
 * the send to land first, and says what it waits on. Each row a driver holds is recorded
 * (`heldRows`), and every box's test ends by holding that record to the box's covered rows of
 * the list, so a row with no driver fails as a box with no driver does.
 *
 * The boxes whose answer goes by the outbox (`BUS_BOXES`, e2e/writing-boxes.ts) are not run
 * here: their answer reaches a party only where the stack has a bus, which no project of this
 * file's has, so the same test with the same driver runs them in `chromium-bus`
 * (e2e/answer-parties.bus.e2e.ts), and their answer is held to the copies the bus carried.
 *
 * `e2e/writing-boxes.ts` holds the world and the checks, and `e2e/writing-drivers.ts` how each
 * box is reached and the test one box is held to (`driveWritingBox`), which both files run.
 * Three boxes draw the picker in an editor of their own rather than in the shared text field,
 * and carry what is theirs alone: the document body, where a pick goes through the editor's own
 * transaction into the document's stored text and reaches another reader of it; a priority's
 * description, drawn in the same editor, where a pick goes into the priority's stored
 * description; and the agent live conversation, where `/` still opens the session's commands and
 * Enter takes a row rather than sending the message.
 */

test.setTimeout(120_000);

for (const box of coveredBoxes().filter((covered) => !BUS_BOXES.includes(covered))) {
  // SLOW(>=2s): each row seeds the world - a project, two issues, a document, a live session and
  // the threads its box answers - loads the page its box is on, and opens the picker seven times,
  // each open its own read of the registry, the issue's people and the search route; a box whose
  // write delivers then sends once and waits for the fake listener's frame.
  test(`${box}: three doors over the same rows, the same words, a pick at the caret and its own hint`, async ({
    browser,
  }) => {
    await driveWritingBox(browser, box);
  });
}
