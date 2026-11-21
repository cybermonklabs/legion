import { expect, test } from "bun:test";
import {
  DELIVERY_DUPLICATE_WINDOW_MS,
  NO_LIVE_SESSION,
  RECEIPT_TIMEOUT_CAUSE,
  STORE_UNCONFIRMED_CAUSE,
} from "@legion/contracts";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { answeredWithErrorGuidance, duplicateText, safeRetryGuidance } from "./delivery";
import { type TargetedMessageAttempt, TargetedMessageCard } from "./TargetedMessageCard";

function attempt(overrides: Partial<TargetedMessageAttempt> = {}): TargetedMessageAttempt {
  return {
    attempt: 1,
    // Inside the stream's duplicate window, where a same-mode retry can still be promised safe.
    createdAt: new Date(Date.now() - 60_000).toISOString(),
    delivery: "steer",
    state: "failed",
    ...overrides,
  };
}

function card(props: {
  attempts: readonly TargetedMessageAttempt[];
  capabilities?: readonly string[];
  onRetry?: (delivery: "aside" | "btw" | "steer") => void;
  targetName?: string;
}) {
  const capabilities = props.capabilities ?? ["aside", "btw", "steer"];
  return (
    <ul>
      <TargetedMessageCard
        body={<p>Ship it.</p>}
        canAside={capabilities.includes("aside")}
        canBtw={capabilities.includes("btw")}
        canSteer={capabilities.includes("steer")}
        deliveries={props.attempts}
        header={null}
        onRetry={props.onRetry ?? (() => {})}
        targetName={props.targetName ?? "planner"}
        turnID="turn-1"
      />
    </ul>
  );
}

// LEGION-271, acceptance 7. The retry row offered only BTW and steer, so retrying an aside
// attempt was always a mode change - a different idempotency key, a second delivery - offered
// under a headline promising the retry was safe. Retry re-sends in the attempt's own mode.
test("Retry re-sends an aside attempt as an aside", async () => {
  const user = userEvent.setup();
  const sent: string[] = [];
  const view = render(
    card({ attempts: [attempt({ delivery: "aside" })], onRetry: (mode) => void sent.push(mode) })
  );
  try {
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(sent).toEqual(["aside"]);
  } finally {
    view.unmount();
  }
});

test("Retry re-sends a steer attempt as a steer, and the mode changes stay available", async () => {
  const user = userEvent.setup();
  const sent: string[] = [];
  const view = render(
    card({ attempts: [attempt({ delivery: "steer" })], onRetry: (mode) => void sent.push(mode) })
  );
  try {
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await user.click(screen.getByRole("button", { name: "Use BTW instead" }));
    expect(sent).toEqual(["steer", "btw"]);
  } finally {
    view.unmount();
  }
});

// Every other button in this row is disabled with a visible reason when the recipient does not
// advertise its mode, so Retry cannot be the one that offers a send failing at resolution.
test("Retry is disabled with a visible reason when the recipient dropped the attempt's mode", () => {
  const view = render(
    card({ attempts: [attempt({ delivery: "aside" })], capabilities: ["btw", "steer"] })
  );
  try {
    expect(screen.getByRole("button", { name: "Retry" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByText("planner does not support Aside.")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// A reason that names a way out names only one the session takes: an aside-only session refuses
// a BTW as well, so its "Use Send instead" is not told to use BTW, while a session that takes
// BTW is.
for (const [capabilities, reason] of [
  [["aside"], "planner does not support Send."],
  [["aside", "btw"], "planner does not support Send — use BTW."],
] as const) {
  test(`a session advertising ${capabilities.join(" and ")} reads "${reason}"`, () => {
    const view = render(card({ attempts: [attempt({ delivery: "aside" })], capabilities }));
    try {
      expect(screen.getByText(reason)).toBeTruthy();
    } finally {
      view.unmount();
    }
  });
}

// LEGION-271, acceptance 7's duplicate half. An attempt that reached the listener and changed
// nothing must not read as an ordinary send, or the history claims a delivery that never
// happened.
test("a duplicate attempt says the listener already had the message", () => {
  const view = render(
    card({ attempts: [attempt({ delivery: "steer", state: "sent", duplicate: true })] })
  );
  try {
    expect(screen.getByText(duplicateText)).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("an ordinary sent attempt still reads as sent", () => {
  const view = render(card({ attempts: [attempt({ delivery: "steer", state: "sent" })] }));
  try {
    expect(screen.getByText("Sent to planner (Send)")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// LEGION-271. The stream recognises a repeated delivery for exactly its duplicate window; past
// that a same-mode retry publishes a second frame. The card promises a safe retry, and offers the
// button the promise describes, only inside that window.
test("a failed attempt inside the duplicate window promises a safe retry and offers it", () => {
  const failure = `${NO_LIVE_SESSION} s1`;
  const view = render(card({ attempts: [attempt({ error: failure })] }));
  try {
    // A cause with no trailing punctuation gets a separator, and a failure that never reached
    // the listener is not told a different mode would deliver it "again".
    expect(
      screen.getByText(`Failed: ${failure}. ${safeRetryGuidance("card", failure)}`)
    ).toBeTruthy();
    expect(screen.queryByText(/sending in a different mode/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// Only a send the listener may already have published earns the mode-change clause: that is the
// one case where another mode genuinely delivers the message a second time.
test("a receipt timeout is told what a mode change would do", () => {
  const view = render(card({ attempts: [attempt({ error: RECEIPT_TIMEOUT_CAUSE })] }));
  try {
    expect(
      screen.getByText(
        `Failed: ${RECEIPT_TIMEOUT_CAUSE} ${safeRetryGuidance("card", RECEIPT_TIMEOUT_CAUSE)}`
      )
    ).toBeTruthy();
    expect(screen.getByText(/sending in a different mode delivers it again/)).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// A send the listener answered 503 because it could not confirm the store may have been delivered
// too. Its card names that cause in its own words, not the receipt timeout's, and gives it the same
// mode-change warning. The control is a cause that cannot have landed: no warning.
test("a send whose store the listener could not confirm is named as such and told what a mode change would do", () => {
  const view = render(card({ attempts: [attempt({ error: STORE_UNCONFIRMED_CAUSE })] }));
  try {
    expect(
      screen.getByText(
        `Failed: ${STORE_UNCONFIRMED_CAUSE} ${safeRetryGuidance("card", STORE_UNCONFIRMED_CAUSE)}`
      )
    ).toBeTruthy();
    expect(screen.getByText(/could not confirm it was stored/)).toBeTruthy();
    expect(screen.queryByText(/didn't answer within the send window/)).not.toBeInTheDocument();
    expect(screen.getByText(/sending in a different mode delivers it again/)).toBeTruthy();
  } finally {
    view.unmount();
  }
  const control = render(card({ attempts: [attempt({ error: `${NO_LIVE_SESSION} s1` })] }));
  try {
    expect(
      screen.queryByText(/sending in a different mode delivers it again/)
    ).not.toBeInTheDocument();
  } finally {
    control.unmount();
  }
});

test("a failed attempt past the duplicate window makes no promise and offers no Retry", () => {
  const stale = attempt({
    createdAt: new Date(Date.now() - DELIVERY_DUPLICATE_WINDOW_MS - 60_000).toISOString(),
    error: `${NO_LIVE_SESSION} s1`,
  });
  const view = render(card({ attempts: [stale] }));
  try {
    expect(screen.getByText(`Failed: ${NO_LIVE_SESSION} s1`)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    // The mode-change actions stay: they are a different key and genuinely deliver.
    expect(screen.getByRole("button", { name: "Use BTW instead" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// A Retry of an attempt the session answered with an error repeats the key the stream already
// stored, so a session the listener pushes to from the stream is never handed it. The card
// offers the mode change, a new key, and names it only while that row is on screen.
test("an attempt the session answered with an error points at a mode change instead of Retry", () => {
  const answered = attempt({
    answeredWithError: true,
    delivery: "btw",
    error: "side turn failed",
  });
  const view = render(card({ attempts: [answered] }));
  try {
    expect(
      screen.getByText(`Failed: side turn failed. ${answeredWithErrorGuidance("card")}`)
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Use Send instead" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// A BTW recorded duplicate is still outstanding - the agent holds it and owes an answer - so the
// card must keep saying an answer is expected rather than reporting it as merely delivered.
test("a duplicate BTW still reads as waiting on an answer", () => {
  const view = render(
    card({ attempts: [attempt({ delivery: "btw", state: "sent", duplicate: true })] })
  );
  try {
    expect(screen.getByText(/Asking planner \(BTW\)/)).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// sjawhar/legion#2230. A role-routed message to a role whose Legion pod its daemon released is
// kept by the listener for the role's next session, for one hour. The card says so, rather than
// calling the role a stopped session that gets it within 72 hours, and offers no Retry: the
// listener took the message, and a repeat would keep a second copy for the same role.
test("a queued attempt kept for a role reads as kept for the role's next session, with no Retry", () => {
  const role = "role:legion-infra-infra-204-planner";
  const view = render(
    card({
      attempts: [attempt({ delivery: "aside", keptForRole: true, state: "queued" })],
      targetName: role,
    })
  );
  try {
    expect(
      screen.getByText(
        `Kept for ${role}: the role's next session gets it if one takes the role within one hour`
      )
    ).toBeTruthy();
    expect(screen.queryByText(/is stopped/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  } finally {
    view.unmount();
  }
});

// The session wording stays for a queued attempt stored for a session, and for an earlier
// attempt the history lists beneath a later one, each attempt read on its own.
test("a queued attempt stored for a session keeps the session wording, beside an earlier kept one", () => {
  const view = render(
    card({
      attempts: [
        attempt({ attempt: 1, keptForRole: true, state: "queued", targetName: "role:planner" }),
        attempt({ attempt: 2, state: "queued" }),
      ],
    })
  );
  try {
    expect(
      screen.getByText(
        "Queued: planner is stopped; it gets the message if it runs again within 72 hours"
      )
    ).toBeTruthy();
    expect(
      screen.getByText(
        /^Attempt 1: Kept for role:planner: the role's next session gets it if one takes the role within one hour$/
      )
    ).toBeTruthy();
  } finally {
    view.unmount();
  }
});
