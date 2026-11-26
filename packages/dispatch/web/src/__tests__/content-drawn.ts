import { whenContentDrawn } from "../features/shell/content-drawn";

/** "ended" if no hold on the shell's extras is on within `ms` from now, else "still on". */
export async function holdWithin(ms: number): Promise<"ended" | "still on"> {
  const late = Promise.withResolvers<"still on">();
  const timer = setTimeout(() => late.resolve("still on"), ms);
  try {
    return await Promise.race([whenContentDrawn().then(() => "ended" as const), late.promise]);
  } finally {
    clearTimeout(timer);
  }
}
