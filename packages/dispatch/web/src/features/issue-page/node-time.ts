/**
 * When a node was made, in the page's own short form (R9, R34): the day
 * and the month, "5 Nov", "14 Nov", as the approved pictures draw it at the right of a row, and
 * the year after them for a day of another year than the reader's, "5 Nov 2025". The day is the
 * reader's own, in the time zone their browser keeps.
 *
 * The month is spelled from the page's own table rather than by `Intl`, whose English short
 * forms differ by locale data ("Sep" and "Sept"), so the same instant draws the same words on
 * every browser.
 */

const MONTHS = [
  "Jan",
  "Feb",
  "Mar",
  "Apr",
  "May",
  "Jun",
  "Jul",
  "Aug",
  "Sep",
  "Oct",
  "Nov",
  "Dec",
] as const;

/** `at` as the page's short day, or `""` for a time it cannot read. */
export function shortDay(at: string, now: Date = new Date()): string {
  const day = new Date(at);
  if (Number.isNaN(day.getTime())) return "";
  const words = `${day.getDate()} ${MONTHS[day.getMonth()] ?? ""}`;
  return day.getFullYear() === now.getFullYear() ? words : `${words} ${day.getFullYear()}`;
}
