import type { WriteRequest } from "../api/client";
import type { KeptFiles } from "../features/writes/kept-files";
import { type KeptWrite, WriteStorage } from "../features/writes/write-storage";
import { plainWrite } from "../features/writes/write-store";

/** A browser's kept upload files, in memory: what IndexedDB holds for the page across a reload,
 *  with `rows` the files it holds now. */
export interface MemoryFiles extends KeptFiles {
  readonly rows: Map<string, File>;
}

export function memoryFiles(): MemoryFiles {
  const rows = new Map<string, { login: string; file: File }>();
  const files = new Map<string, File>();
  return {
    delete: async (key) => {
      rows.delete(key);
      files.delete(key);
    },
    get: async (key) => rows.get(key)?.file,
    keepOnly: async (login) => {
      for (const [key, row] of rows) {
        if (row.login !== login) {
          rows.delete(key);
          files.delete(key);
        }
      }
    },
    put: async (key, login, file) => {
      rows.set(key, { file, login });
      files.set(key, file);
      return true;
    },
    rows: files,
  };
}

/** A `beforeunload` target that counts its listeners: the page asks to confirm leaving exactly
 *  while it holds one. */
export class UnloadTarget extends EventTarget {
  listeners = 0;

  override addEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject | null,
    options?: AddEventListenerOptions | boolean
  ): void {
    if (type === "beforeunload") this.listeners += 1;
    super.addEventListener(type, listener, options);
  }

  override removeEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject | null,
    options?: EventListenerOptions | boolean
  ): void {
    if (type === "beforeunload") this.listeners -= 1;
    super.removeEventListener(type, listener, options);
  }

  /** Whether leaving now asks the reader to confirm: a listener cancelled the event. */
  asks(): boolean {
    const leaving = new Event("beforeunload", { cancelable: true });
    this.dispatchEvent(leaving);
    return leaving.defaultPrevented;
  }
}

/** One browser tab's sessionStorage, which a reload keeps, and the pages it holds in turn: each
 *  `page` is the kept writes of one load of the app, the last one after a reload. */
export interface KeptTab {
  readonly storage: Storage;
  /** The kept writes of the next page load, asking on `unload` to confirm leaving. */
  page: (unload?: EventTarget) => WriteStorage;
  /** What the tab keeps for the write under `key`, as written. */
  kept: (key: string) => unknown;
  /** Ends every page, its kept sends with it. */
  close: () => void;
}

export function keptTab(): KeptTab {
  const items = new Map<string, string>();
  const storage: Storage = {
    clear: () => items.clear(),
    getItem: (name) => items.get(name) ?? null,
    key: (index) => [...items.keys()][index] ?? null,
    get length() {
      return items.size;
    },
    removeItem: (name) => {
      items.delete(name);
    },
    setItem: (name, value) => {
      items.set(name, String(value));
    },
  };
  const pages: WriteStorage[] = [];
  return {
    close: () => {
      for (const page of pages) page.retire();
    },
    kept: (key) => {
      const value = items.get(`dispatch.write:${key}`);
      return value === undefined ? undefined : JSON.parse(value);
    },
    page: (unload = new UnloadTarget()) => {
      const page = new WriteStorage(storage, unload);
      pages.push(page);
      return page;
    },
    storage,
  };
}

/** A status pick of CORE-1, as the issue page sends one. */
export const patch: WriteRequest = {
  body: { status: "done" },
  method: "PATCH",
  path: "/api/v1/issues/CORE-1",
};

/** The fields of a kept write a row names (`keepWrite`), its request as the page made it. */
type KeptFields = Omit<KeptWrite, "carried" | "key" | "keyed" | "request"> & {
  readonly request: WriteRequest | null;
};

/** Keeps in `storage`, a tab's sessionStorage, under `key`, one write of alice's as a page that
 *  went away left it: a status pick of CORE-1 out within its deadline, pressed now, as the
 *  request it last went out as, unless `fields` names otherwise. */
export function keepWrite(storage: Storage, key: string, fields: Partial<KeptFields> = {}): void {
  storage.setItem(
    `dispatch.write:${key}`,
    JSON.stringify({
      detail: null,
      key,
      kind: plainWrite.name,
      label: "CORE-1 set to done",
      login: "alice",
      presentation: "indicator",
      pressedAt: Date.now(),
      request: patch,
      sent: true,
      state: { status: "sending" },
      target: null,
      version: 1,
      ...fields,
    })
  );
}

/** The names of the writes the page's own tab keeps across a reload, in its `sessionStorage`. */
export function keptWrites(): string[] {
  return Array.from({ length: window.sessionStorage.length }, (_, index) =>
    window.sessionStorage.key(index)
  ).filter((name): name is string => name?.startsWith("dispatch.write:") === true);
}

/** A v7 key minted at `at`, as `newSendKey` mints one: its first twelve hex digits are that
 *  millisecond, which `keyExpired` reads back. */
export function keyMinted(at: number): string {
  const hex = at.toString(16).padStart(12, "0");
  return `${hex.slice(0, 8)}-${hex.slice(8)}-7abc-9def-0123456789ab`;
}
