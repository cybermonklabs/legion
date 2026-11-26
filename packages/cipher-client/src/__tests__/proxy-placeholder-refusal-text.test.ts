import { expect, test } from "bun:test";
import { isKnownBrokerRefusal } from "../refusal-text";
import { BrokerRefusedError } from "../token-errors";

// The broker's refusal of a placeholder trade, as its token endpoint writes it on the wire
// (oauth.Provider.grantApprove's hint after the library's sentence): a machine asking for the
// approve capability's placeholder, or a person's sign-in on a broker that serves no approve
// capability. `dispatch review` on such a box prints the refusal it reads, so a refusal that is
// withheld leaves the person with "refused <person>'s sign-in: HTTP 400 invalid_request: (its text
// is withheld)", which reads as a broken sign-in, where the broker said which of the two it is.
const LIBRARY =
  "The request is missing a required parameter, includes an invalid parameter value, includes a parameter more than once, or is otherwise malformed.";
const HINT =
  "Only a person's sign-in is traded for the approve capability's proxy placeholder, on a broker that serves it.";
const sent = { person: "ada@example.com", session: "ses_placeholder" };

test("the broker's refusal of a placeholder trade prints whole", () => {
  const description = `${LIBRARY} ${HINT}`;
  expect(isKnownBrokerRefusal(description, sent)).toBe(true);
  expect(new BrokerRefusedError(400, "invalid_request", description, "probe", sent).said).toBe(
    description
  );
});

test("control: a refusal of another wording is still withheld", () => {
  const description = `${LIBRARY} ${HINT.replace("approve", "aprove")}`;
  expect(isKnownBrokerRefusal(description, sent)).toBe(false);
});
