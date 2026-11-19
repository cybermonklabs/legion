/**
 * Whether a client admits the value of a setting that names an http or https URL, before any
 * request: such a value may carry a user and a password, since the config accepts such a URL, and
 * requests still go to the URL as configured. No line prints what this answers: a line names a
 * configured URL through `urlForText` or `requestForText` (`@legion/contracts/log-text`), which
 * print no part of a URL that is not plain. The value is admitted only when the URL parser reads
 * it as an http or https URL with a host, and undefined is answered for any other value, of which
 * a refusal names the length alone (`unshownSettingUrl`). Any other reading cannot be trusted to
 * have found the password: the parser reads `user:password@host` and `mailto:user:password@host`
 * with no user part at all, the password in the path, and its error for a value it refuses holds
 * the value whole.
 *
 * Nor is the parser trusted to have found the whole user part. It ends one at the first `/`, `?`,
 * `#` or `\`, so a token that holds one (base64 does) reads as a host taken from the token and a
 * path, query or fragment that holds the rest. The user part the parser found is taken out, and a
 * value in which a raw `@` is still left is admitted not at all, since even its host can be a part
 * of the token. Each caller that admits a setting's URL asks this first and refuses such a value
 * before any request, so no lookup of a host taken from a token goes out either. What is left can
 * still name a host the parser took from a password (`https://user:p@ssword` reads with the host
 * `ssword`), which is why no line prints it.
 *
 * A URL that carries neither user nor password comes back as written. One that carries them comes
 * back as the URL parser writes it: its scheme and host lower-cased and a default port dropped,
 * and no `/` added after a bare origin.
 *
 * A value that holds a control character (a tab, a line feed, any byte below a space, or DEL) is
 * shown not at all: the parser drops a tab and a line feed where it reads them, so a line that
 * showed the value as written would carry them into the line.
 *
 * A caller whose setting names a URL of another scheme (a NATS server, a Postgres database) passes
 * the `schemes` it takes, as the parser writes them (`nats:`), and the same rule holds for each.
 * Such a value comes back as the parser writes it and without its query and fragment: this
 * function cannot tell which parameters of another protocol hold a secret, and libpq reads a
 * password from `?password=`.
 */
export function shownSettingUrl(
  value: string,
  schemes: readonly string[] = HTTP_SCHEMES
): string | undefined {
  const reading = readSettingUrl(value, schemes);
  return "shown" in reading ? reading.shown : undefined;
}

/**
 * The words a refusal names a setting's value by when `shownSettingUrl` shows none of it: its
 * length, and the cause `shownSettingUrl`'s checks found, of the same `schemes`, with what to
 * correct. They hold no part of the value.
 */
export function unshownSettingUrl(
  value: string,
  schemes: readonly string[] = HTTP_SCHEMES
): string {
  const reading = readSettingUrl(value, schemes);
  const length = `its value is ${value.length} characters long`;
  return "cause" in reading ? `${length}; ${reading.cause}` : length;
}

const HTTP_SCHEMES: readonly string[] = ["http:", "https:"];

const USER_PART_ADVICE =
  "if it carries a user part, write each @, /, ?, # and \\ in that part percent-encoded (@ as %40, / as %2F), or remove the user part";

/** `shownSettingUrl`'s checks of `value`, in order: what it shows, or the first cause it shows
 *  none, in words that hold no part of the value. */
function readSettingUrl(
  value: string,
  schemes: readonly string[]
): { readonly shown: string } | { readonly cause: string } {
  for (let i = 0; i < value.length; i++) {
    const code = value.charCodeAt(i);
    if (code < 0x20 || code === 0x7f) {
      return {
        cause:
          "it holds a control character (a line ending or a tab, often one left from a file); remove it",
      };
    }
  }
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    return {
      cause: value.includes("@")
        ? `it does not parse as a URL; ${USER_PART_ADVICE}`
        : "it does not parse as a URL",
    };
  }
  if (!schemes.includes(parsed.protocol)) {
    const starts = schemes.map((scheme) => `${scheme}//`);
    const last = starts.pop();
    return {
      cause: `it does not start with ${starts.length > 0 ? `${starts.join(", ")} or ${last}` : last}`,
    };
  }
  if (parsed.host === "") return { cause: "it names no host" };
  // What is left once the user part the parser found is taken out: the value as written when it
  // has none, else as the parser writes it, with no `/` after a bare origin the value did not have.
  let left = value;
  if (parsed.username !== "" || parsed.password !== "") {
    parsed.username = "";
    parsed.password = "";
    const written = parsed.toString();
    left = written.endsWith("/") && !value.endsWith("/") ? written.slice(0, -1) : written;
  }
  if (left.includes("@")) {
    return {
      cause:
        "it holds an @ outside its user part (in its path, query or fragment); if its user part holds a /, ?, # or \\, write each of them in it percent-encoded (/ as %2F, ? as %3F, # as %23, \\ as %5C), else write that @ as %40",
    };
  }
  if (parsed.protocol === "http:" || parsed.protocol === "https:") return { shown: left };
  parsed.search = "";
  parsed.hash = "";
  return { shown: parsed.toString() };
}
