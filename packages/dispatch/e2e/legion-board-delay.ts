import type { Page } from "@playwright/test";

/** A /ws/live frame as JSON.parse returns it, before its fields are checked. */
interface RawFrame {
  readonly type?: unknown;
  readonly sub?: unknown;
  readonly view?: { readonly kind?: unknown } | null;
}

interface LiveFrame {
  readonly type: string | undefined;
  readonly sub: string | undefined;
  readonly viewKind: string | undefined;
}

function stringOf(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

function frameOf(message: unknown): LiveFrame | undefined {
  try {
    const parsed = JSON.parse(String(message)) as RawFrame | null;
    if (typeof parsed !== "object" || parsed === null) return undefined;
    return {
      type: stringOf(parsed.type),
      sub: stringOf(parsed.sub),
      viewKind: stringOf(parsed.view?.kind),
    };
  } catch {
    return undefined;
  }
}

/** Holds the first `snapshot` frame of every `legion` subscription on /ws/live for `ms`. */
export async function delayLegionBoard(page: Page, ms: number): Promise<void> {
  await page.routeWebSocket(/\/ws\/live$/u, (route) => {
    const legionSubscriptions = new Set<string>();
    const delayedSnapshots = new Set<string>();
    const server = route.connectToServer();

    route.onMessage((message) => {
      const frame = frameOf(message);
      if (frame?.type === "subscribe" && frame.viewKind === "legion" && frame.sub !== undefined)
        legionSubscriptions.add(frame.sub);
      server.send(message);
    });

    server.onMessage((message) => {
      const frame = frameOf(message);
      if (
        frame?.type === "snapshot" &&
        frame.sub !== undefined &&
        legionSubscriptions.has(frame.sub) &&
        !delayedSnapshots.has(frame.sub)
      ) {
        delayedSnapshots.add(frame.sub);
        setTimeout(() => route.send(message), ms);
        return;
      }
      route.send(message);
    });
  });
}
