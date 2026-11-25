import { expect, test } from "bun:test";
import { sentAs } from "../../test/refusal-corpus";
import { isKnownBrokerRefusal } from "../refusal-text";
import { BrokerRefusedError } from "../token-errors";

// A job entry's trade that asks for the scope read is refused 400 invalid_request, and the
// broker's hint says what to do: ask without that scope (packages/envoy/internal/cipher/oauth,
// exchange.go's decide). Every other read trade's refusal is printed whole, so this one is too:
// the broker's refusal corpus holds it as the broker sends it, and the client prints it rather
// than "(its text is withheld)", which is all a job running `dispatch api` with
// DISPATCH_TOKEN_BROKER_READ=1 read at cybermonklabs/legion#5323's head 2543b5eb.
test("a job entry's read trade is refused in words the client prints whole", () => {
  const description = sentAs(
    "The scope 'read' narrows a token to reading, and a job entry's token is minted whole, with no scope: ask for it without that scope."
  );
  expect(isKnownBrokerRefusal(description, {})).toBe(true);
  expect(new BrokerRefusedError(400, "invalid_request", description, "probe", {}).said).toBe(
    description
  );
});
