import { afterEach, beforeAll, describe, test } from "bun:test";
import { CHECKOUT_PLUGINS, type Cleanup, ompNatsGuard } from "@legion/pi-shared/test/omp-harness";
import { approvalWords, captureCases, type Words } from "./approval-message-capture";

// The cases of check e4b07a29 in CI (plan task t-13): the driver scripts/e2e/approval-message.sh
// runs on the pod's own model route, run here on the pinned Oh My Pi with this checkout's two
// plugins and a stand-in model (provider `anthropic`, claude-opus-5-5 and claude-sonnet-5, no
// compat line, its base URL the stand-in's), in both load orders: pi-envoy then pi-legion, the
// sandbox launch's, and pi-legion then pi-envoy; then the whole list once more on
// claude-sonnet-5, a model that takes no system message inside the conversation, in the sandbox
// launch's order. The entry cases send the first entry file of each of the six paths, as the
// daemon's approval-path test writes it. `ompNatsGuard` skips the file without LEGION_TEST_OMP
// and LEGION_TEST_NATS_URL, and fails it on GitHub Actions without them; the stand-in route's
// words come from the daemon itself, through `go run` in packages/daemon.
const guard = ompNatsGuard();

/** A case runs one to four lives of a pane, each step bounded by the driver's own wait. */
const CASE_MS = 600_000;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

let words: Words = new Map();
beforeAll(async () => {
  if (!guard.skip) words = await approvalWords();
}, 300_000);

const runs = [
  { title: "pi-envoy loaded first (the sandbox launch's order)", reverse: false, model: "opus" },
  { title: "pi-legion loaded first", reverse: true, model: "opus" },
  { title: "claude-sonnet-5, pi-envoy loaded first", reverse: false, model: "sonnet" },
] as const;

for (const { title, reverse, model } of runs) {
  describe.skipIf(guard.skip)(title, () => {
    for (const one of captureCases("each-path")) {
      test(
        one.name,
        async () => {
          const { binary, nats } = guard.setup();
          await one.run({
            binary,
            nats,
            route: { kind: "standin" },
            plugins: CHECKOUT_PLUGINS,
            reverse,
            words,
            cleanup,
            note: () => undefined,
            sent: [],
            model,
          });
        },
        CASE_MS
      );
    }
  });
}
