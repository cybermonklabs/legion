/**
 * What the page keeps in the tab for a reload, and how it reads it back: the held writes
 * (`write-storage.ts`) and the draft stores (`draft-store.ts`) each keep their entries in the
 * page's `sessionStorage` under a prefix of their own, as JSON of one versioned format stamped with
 * the login of the person it is kept for, and drop on load every entry of another person's or of
 * another format. What is read back is trusted only once its shape is checked (`shape`).
 */

/** The page's sessionStorage, or undefined where the browser refuses it the page. */
export function pageStorage(): Storage | undefined {
  try {
    return window.sessionStorage;
  } catch {
    return undefined;
  }
}

/** A value's shape, checked: what a kept record is parsed by before it is trusted, and a type
 *  guard, so each is held to the type it stands for (`satisfies Shape<T>`) and nothing is cast. */
export type Shape<T> = (value: unknown) => value is T;

/** The type a shape checks for. */
export type ShapeOf<S> = S extends Shape<infer T> ? T : never;

/** The shapes a kept record is checked by: a record has each named field in its shape (a missing
 *  one reads as undefined, and others are not read), and `either` fits a value that one of its
 *  shapes fits. */
export const shape = {
  // `T[]`, not `readonly T[]`: `Array.isArray` proves a real array, and a kept record whose shape
  // is checked against a request body the route table types (`CarriedBlockIds`) needs the mutable
  // one — `readonly T[]` does not fit it, while `T[]` fits both.
  array:
    <T>(item: Shape<T>): Shape<T[]> =>
    (value): value is T[] =>
      Array.isArray(value) && value.every(item),
  boolean: ((value) => typeof value === "boolean") as Shape<boolean>,
  either:
    <S extends readonly Shape<unknown>[]>(...shapes: S): Shape<ShapeOf<S[number]>> =>
    (value): value is ShapeOf<S[number]> =>
      shapes.some((fits) => fits(value)),
  nullable:
    <T>(inner: Shape<T>): Shape<T | null> =>
    (value): value is T | null =>
      value === null || inner(value),
  number: ((value) => typeof value === "number" && Number.isFinite(value)) as Shape<number>,
  oneOf:
    <const V extends readonly (boolean | number | string | null)[]>(
      ...values: V
    ): Shape<V[number]> =>
    (value): value is V[number] =>
      values.includes(value as V[number]),
  optional:
    <T>(inner: Shape<T>): Shape<T | undefined> =>
    (value): value is T | undefined =>
      value === undefined || inner(value),
  record:
    <F extends Readonly<Record<string, Shape<unknown>>>>(
      fields: F
    ): Shape<{ readonly [K in keyof F]: ShapeOf<F[K]> }> =>
    (value): value is { readonly [K in keyof F]: ShapeOf<F[K]> } =>
      typeof value === "object" &&
      value !== null &&
      !Array.isArray(value) &&
      Object.entries(fields).every(([name, field]) => field(Reflect.get(value, name))),
  string: ((value) => typeof value === "string") as Shape<string>,
  unknown: ((_value) => true) as Shape<unknown>,
} as const;

/** The names of the entries of `storage` that start with `prefix`, read before any is removed. */
export function entryNames(storage: Storage, prefix: string): string[] {
  return Array.from({ length: storage.length }, (_, index) => storage.key(index)).filter(
    (name): name is string => name?.startsWith(prefix) === true
  );
}

/** What `storage` holds under `name`, read as JSON, or undefined where it holds nothing readable:
 *  an entry to check against its format's shape before it is read as what it keeps. */
export function readEntry(storage: Storage, name: string): unknown {
  try {
    return JSON.parse(storage.getItem(name) ?? "null");
  } catch {
    return undefined;
  }
}

/** Removes the entry `name` from `storage`, where the page has one. */
export function removeEntry(storage: Storage | undefined, name: string): void {
  try {
    storage?.removeItem(name);
  } catch {
    // A storage that refuses even a removal holds nothing this page wrote.
  }
}
