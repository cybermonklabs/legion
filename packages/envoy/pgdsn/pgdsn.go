// Package pgdsn is a Postgres DSN as a refusal or an error prints it: the setting that holds it,
// the address it points at and the driver's detail, never its credential. The Legion daemon (its
// record store and Oh My Pi's session database), the secrets broker and Dispatch each read their
// DSN through it: Refusal at boot, before any connection, and ConnectError for every failure to
// parse, open or reach the database.
package pgdsn

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5/pgconn"
)

// Refusal is why dsn, the value of the setting named setting, is refused before Postgres is asked,
// or nil. It refuses these shapes, each of which Postgres or pgx would print with part of its
// credential, and names setting alone, never the value, and says what to change:
//   - a postgres:// or postgresql:// URL holding an unencoded "@" after pgx's user part
//     (atPastUserinfo). Such an "@" is where a password holding an "@" or a "/" leaves part of
//     itself, read as the host, the port, the database or a query, which every connect error
//     prints;
//   - a value that names the scheme but is no such URL (schemeMistyped): pgx reads only a
//     lower-case postgres:// or postgresql:// at its first byte as a URL, so it reads
//     `POSTGRES://u:<pw>@h/x?sslmode=require`, ` postgres://…` and `postgres:u:<pw>@h` as one
//     keyword setting up to the "=", which it sends the server as a parameter's name, and a server
//     that admits the client prints that name whole;
//   - any other value with a key no keyword or parameter name has (keyMalformed): a key holding
//     anything but ASCII letters, digits, "_", "." and, after its first character, "$" (which
//     Postgres takes in a custom setting's name). pgx sends such a key to the server the same way,
//     so `postgresql+psycopg2://u:<pw>@h/x?sslmode=…`, `postgress://…`, `pgsql://…`,
//     `postgres//…`, `postgres;//…`, `jdbc:postgresql://…` and a BOM before `postgres:` print
//     whole too;
//   - a URL whose query holds a key of no such shape (queryKeyMalformed), read as pgx reads it,
//     the spaces at its ends dropped (`&https://u:<pw>%40h=1`): pgx sends it to the server as a
//     parameter's name, which the server prints;
//   - a value of any key but a password (password, sslpassword) and the four printed values below,
//     holding "://" and then an "@" anywhere after it, a URL with a user name or a password
//     (valueHoldsURLCredential), as pgx reads it from a keyword DSN or, decoded, from a URL's
//     query: the server echoes a runtime parameter's value it refuses, given as a keyword
//     (`datestyle=https://u:<pw>@h`) or inside options (`options='-c datestyle=https://u:<pw>@h'`);
//     the four printed values are refused for `://` alone (valueHoldsURL, below). Nothing after
//     the "://" ends the URL: a password holding a "/", "?" or "#" (`https://u:<head>/<tail>@h`,
//     its `%2F` in a URL's query, a quoted "?" or "#") or a space (`%20`, a tab, an escaped or quoted
//     space) reaches the server whole, and the rule cannot tell such a password's tail from the
//     URL's path. So a URL value with an "@" in its path (`https://ci.example/@team`) and two
//     settings of options, a URL and then an address (`-c app.url=https://ci.example -c
//     app.owner=ops@acme.example`), are refused too;
//   - a value other than a password holding "://" anywhere (valueHoldsURL), where the value is
//     one of the four pgx's connect error or the server prints: the user, the database (dbname or
//     database), the host or the hostaddr, as pgx reads it from a keyword DSN or, decoded, from a
//     URL's user info, host list, path or query. It is a URL, whatever stands before its scheme
//     (`dbname=' pg://u:<pw>@h'`, `dbname=jdbc:postgresql://u:<pw>@h/x`, `?dbname=pg://u:<pw>%40h`);
//   - a user, so read, holding a ":" (userHoldsColon), where a password written after the user name
//     stands (`user=app:<pw>`, `?user=app:<pw>`);
//   - a host or hostaddr, so read, holding an "@" or an "=", or a ":" in an entry of its list that
//     is no IP address and no socket directory, which begins with "/" (hostHoldsCredential): a
//     user name and password written before the host, or a password= missing the space before it
//     (`host=127.0.0.1password=<pw>`, `host=app:<pw>`), which the connect error and the dev
//     sign-in fence name as the host;
//   - a database, so read, holding an "@" (databaseHoldsAt), where a user name and password
//     written before a host stand (`dbname=app:<pw>@db/app`, `dbname=<pw>@db`,
//     `postgres://h/u:<pw>%40db`).
//
// A password (password, sslpassword) is held to none of these: pgx never prints sslpassword, and
// ConnectError masks a password such as `Postgres:Admin1` wherever one prints. Every other key but
// the four printed values (user, dbname or database, host, hostaddr) is held to the URL-credential
// rule alone, so `application_name=https://ci.example/run/1` and `options='-c app.url=https://…'`
// load. A Cloud SQL socket (`host=/cloudsql/acme-postgres:us-central1:db`), an IPv6 host or
// hostaddr, a host list, a port given apart from the host (keyword `port=`, a URL's `host:port`)
// and an IAM user (`sa@project.iam`) are kept. A keyword DSN with none of these (`host=…
// password=…`) is never refused: its password is a value of its own, which ConnectError masks.
//
// One shape stays: a user holding a password and an "@" but no ":" (`user=<pw>@host`) has the
// shape of a Cloud SQL IAM user name (`sa@project.iam`), and is kept.
func Refusal(setting, dsn string) error {
	switch refusalOf(dsn) {
	case atPast:
		return fmt.Errorf("%s cannot be read: it holds an \"@\" after its user name and password, where Postgres reads"+
			" part of its user name, password or database name as its host, port, database or query and prints it;"+
			" write each \"@\" in a password, a user name or a query value other than a database name as %%40, and a \"/\" in a"+
			" password as %%2F; a database name holds no \"@\"", setting)
	case schemeMistyped:
		return fmt.Errorf("%s cannot be read: it names the postgres scheme but does not begin with postgres:// or postgresql://"+
			" in lower case, with nothing before it, so Postgres would read it as one keyword setting and can print it whole;"+
			" write the scheme as postgres:// or postgresql://", setting)
	case keyMalformed:
		return fmt.Errorf("%s cannot be read: it is no postgres:// or postgresql:// URL, and a key of it holds a character"+
			" no keyword or parameter name has (only ASCII letters, digits, \"_\", \".\" and, after the first, \"$\"), so Postgres"+
			" would send that key to the server as a parameter's name and can print it whole; write the DSN as a postgres:// URL"+
			" or as keyword=value pairs", setting)
	case queryKeyMalformed:
		return fmt.Errorf("%s cannot be read: a key of its query holds a character no keyword or parameter name has (only"+
			" ASCII letters, digits, \"_\", \".\" and, after the first, \"$\"), which Postgres sends the server as a parameter's"+
			" name and the server can print whole; name each setting of the query by its keyword", setting)
	case valueHoldsURLCredential:
		return fmt.Errorf("%s cannot be read: a value of it other than a password holds a URL with a user name or a password"+
			" (\"://\" and then an \"@\" anywhere after it, since a password can hold a \"/\", \"?\", \"#\" or space), which"+
			" Postgres can print whole, as the server prints a setting's value it refuses; give the setting no credential", setting)
	case valueHoldsURL:
		return fmt.Errorf("%s cannot be read: a value of it other than a password, its user, database, host or hostaddr,"+
			" holds \"://\", a URL, which Postgres does not read as one and prints whole, a password in it included; write the"+
			" DSN as a postgres:// URL, or give the database, the user and the host by name", setting)
	case userHoldsColon:
		return fmt.Errorf("%s cannot be read: its user holds a \":\", where a password written after the user name stands,"+
			" and Postgres prints the user; write the password as password= and the user alone", setting)
	case hostHoldsCredential:
		return fmt.Errorf("%s cannot be read: its host or hostaddr holds an \"@\", an \"=\", or a \":\" in an entry that is"+
			" no IP address or socket directory, where a user name and password written before the host, or a password="+
			" with no space before it, stand, and Postgres prints the host; write user= and password= apart from the host", setting)
	case databaseHoldsAt:
		return fmt.Errorf("%s cannot be read: its database name holds an \"@\", where a user name and password written"+
			" before a host stand, and Postgres prints the database name; give the database by its name alone, and the"+
			" user, the password and the host apart", setting)
	}
	return nil
}

// refusal is the shape Refusal refuses a DSN for, or kept.
type refusal int

const (
	kept refusal = iota
	atPast
	schemeMistyped
	keyMalformed
	queryKeyMalformed
	valueHoldsURLCredential
	valueHoldsURL
	userHoldsColon
	hostHoldsCredential
	databaseHoldsAt
)

// refusalOf is the shape Refusal refuses dsn for, or kept. A URL is a value that begins, at its
// first byte, with a lower-case postgres:// or postgresql://, as pgx reads one (parseURLSettings),
// read for an "@" past its user part and then, decoded (urlSettings), for its printed values; any
// other value that begins, after leading space, with "postgres:" or "postgresql:" in any case is
// a mistyped scheme; and any other value is a keyword DSN, read as pgx reads one
// (keywordSettings), refused for a key that is no keyword's shape and then for its printed values.
// A URL's query is refused, before its printed values, for a key that is no keyword's shape.
// Both forms' printed values are held to the one rule set (printedRefusal).
func refusalOf(dsn string) refusal {
	if _, _, _, isURL := cutURL(dsn); isURL {
		if atPastUserinfo(dsn) {
			return atPast
		}
		settings := urlSettings(dsn)
		for _, setting := range settings {
			if !keywordShape.MatchString(setting.key) {
				return queryKeyMalformed
			}
		}
		return printedRefusal(settings)
	}
	if trimmed := strings.ToLower(strings.TrimLeftFunc(dsn, unicode.IsSpace)); strings.HasPrefix(trimmed, "postgres:") || strings.HasPrefix(trimmed, "postgresql:") {
		return schemeMistyped
	}
	settings := keywordSettings(dsn)
	for _, setting := range settings {
		if !keywordShape.MatchString(setting.key) {
			return keyMalformed
		}
	}
	return printedRefusal(settings)
}

// printedRefusal is the shape settings are refused for by the values pgx's connect error or the
// server prints, or kept. A password (password, sslpassword) is read by no rule: pgx prints
// neither, and ConnectError masks a password where one prints. The four values the connect error
// names and the server echoes are each held to their own rules: the user, the database (dbname or
// database, which pgx reads alike), the host and the hostaddr. One of them holding "://" anywhere
// is a URL (valueHoldsURL); a user holding ":" stands a password after the user name
// (userHoldsColon); a host or hostaddr that is no host list (hostList) stands a credential before
// or in the host (hostHoldsCredential); a database holding "@" stands a user name and password
// before a host (databaseHoldsAt). Every other key's value is refused only when it holds a URL with
// a user name or a password (urlCredential, valueHoldsURLCredential): the server echoes a runtime
// parameter's value it refuses, as a keyword or inside options, and a URL without one
// (`application_name=https://ci.example/run/1`) loads.
func printedRefusal(settings []keywordSetting) refusal {
	for _, setting := range settings {
		key, value := setting.key, setting.value
		switch key {
		case "password", "sslpassword":
			continue
		case "user", "dbname", "database", "host", "hostaddr":
		default:
			if urlCredential.MatchString(value) {
				return valueHoldsURLCredential
			}
			continue
		}
		switch {
		case strings.Contains(value, "://"):
			return valueHoldsURL
		case key == "user" && strings.Contains(value, ":"):
			return userHoldsColon
		case (key == "host" || key == "hostaddr") && !hostList(value):
			return hostHoldsCredential
		case (key == "dbname" || key == "database") && strings.Contains(value, "@"):
			return databaseHoldsAt
		}
	}
	return kept
}

// urlCredential is a URL with a user name or a password in a value: "://", then an "@" anywhere
// after it. Nothing ends it: a password holding a "/", "?" or "#" ends url.Parse's authority
// early, its tail read as the path, query or fragment, pgx decodes a password's %20 and %2F before
// the value reaches the server, and a tab or a quoted or escaped space stands in a value as
// written, so the "@" after any of them is still the URL's user info's.
var urlCredential = regexp.MustCompile(`(?s)://.*@`)

// hostList says whether value reads as a host list pgx dials and no credential stands in: no "@"
// or "=" anywhere, and a ":" only in an entry (pgx splits the list at ",") that is a socket
// directory, which begins with "/" (a Cloud SQL socket, `/cloudsql/acme-postgres:us-central1:db`),
// or an IP address (an IPv6 one, `::1`, `fe80::1%eth0`).
func hostList(value string) bool {
	if strings.ContainsAny(value, "@=") {
		return false
	}
	for _, entry := range strings.Split(value, ",") {
		if !strings.Contains(entry, ":") || strings.HasPrefix(entry, "/") {
			continue
		}
		if _, err := netip.ParseAddr(entry); err != nil {
			return false
		}
	}
	return true
}

// keywordShape is the shape of every libpq keyword and Postgres parameter name: ASCII letters,
// digits, "_" and ".", and after the first character "$", which Postgres takes in a custom
// setting's name (guc.c's valid_custom_variable_name). A name holding a letter outside ASCII
// (`app.naïve`), which guc.c's own check admits, is refused as libpq refuses it, since libpq is
// the rule every split of this repository's DSN readers holds to (pg-url.json).
var keywordShape = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.$]*$`)

// keywordSetting is one key=value pair of a DSN as pgx reads it: of a keyword DSN
// (keywordSettings), or of a URL, its user info, host list, database and query each read as the
// keyword it stands for (urlSettings).
type keywordSetting struct{ key, value string }

// keywordSettings reads dsn's key=value pairs as pgx does (parseKeywordValueSettings): leading
// ASCII space dropped, each key the text up to the next "=" with ASCII space trimmed, each value
// up to the next ASCII space, or single-quoted, with a backslash taking the next character
// literally. Text with no "=" left is one more key, as the shape check reads it; pgx refuses it.
// The read stops at a quoted value with no closing quote, which pgx refuses too.
func keywordSettings(dsn string) []keywordSetting {
	const space = " \t\n\r\v\f"
	var settings []keywordSetting
	s := strings.TrimLeft(dsn, space)
	for s != "" {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return append(settings, keywordSetting{key: strings.Trim(s, space)})
		}
		key := strings.Trim(s[:eq], space)
		s = strings.TrimLeft(s[eq+1:], space)
		var value strings.Builder
		if strings.HasPrefix(s, "'") {
			s = s[1:]
			end := 0
			for ; end < len(s) && s[end] != '\''; end++ {
				if s[end] == '\\' && end+1 < len(s) {
					end++
				}
				value.WriteByte(s[end])
			}
			if end == len(s) {
				return append(settings, keywordSetting{key: key, value: value.String()})
			}
			s = s[end+1:]
		} else {
			end := 0
			for ; end < len(s) && !strings.ContainsRune(space, rune(s[end])); end++ {
				if s[end] == '\\' {
					end++
					if end == len(s) {
						break
					}
				}
				value.WriteByte(s[end])
			}
			s = s[end:]
		}
		settings = append(settings, keywordSetting{key: key, value: value.String()})
		s = strings.TrimLeft(s, space)
	}
	return settings
}

// urlSettings reads the URL dsn's settings as pgx does (parseURLSettings), each percent-decoded:
// the user and the password of its user info (cutURL's, split at its first ":"), its host list
// (each entry up to a ":", "/", "?" or "," outside brackets, an IPv6 address in brackets, a port
// after a ":" left out), the database its path names up to a "?", and every key=value pair of its
// query, split at "&" and then at "=". A part that does not decode is read as written: pgx
// refuses such a DSN, and ConnectError prints none of it.
func urlSettings(dsn string) []keywordSetting {
	userinfo, hasUserinfo, rest, _ := cutURL(dsn)
	var settings []keywordSetting
	if hasUserinfo {
		user, password, _ := strings.Cut(userinfo, ":")
		settings = append(settings, keywordSetting{"user", urlDecoded(user)}, keywordSetting{"password", urlDecoded(password)})
	}
	var hosts []string
	for {
		if after, bracketed := strings.CutPrefix(rest, "["); bracketed {
			host, tail, _ := strings.Cut(after, "]")
			hosts, rest = append(hosts, host), tail
		} else {
			end := strings.IndexAny(rest, ":/?,")
			if end < 0 {
				end = len(rest)
			}
			hosts, rest = append(hosts, rest[:end]), rest[end:]
		}
		if strings.HasPrefix(rest, ":") {
			end := strings.IndexAny(rest, "/?,")
			if end < 0 {
				end = len(rest)
			}
			rest = rest[end:]
		}
		next, more := strings.CutPrefix(rest, ",")
		if !more {
			break
		}
		rest = next
	}
	settings = append(settings, keywordSetting{"host", urlDecoded(strings.Join(hosts, ","))})
	if path, ok := strings.CutPrefix(rest, "/"); ok {
		database, query, hasQuery := strings.Cut(path, "?")
		settings = append(settings, keywordSetting{"database", urlDecoded(database)})
		rest = ""
		if hasQuery {
			rest = "?" + query
		}
	}
	if query, ok := strings.CutPrefix(rest, "?"); ok && query != "" {
		// pgx reads pairs until nothing is left after an "&", so one "&" at the end ends the query
		// (`?sslmode=disable&`); two in a row, or one in the middle with nothing before the next,
		// pgx refuses as a pair with no "=".
		for _, pair := range strings.Split(strings.TrimSuffix(query, "&"), "&") {
			key, value, _ := strings.Cut(pair, "=")
			settings = append(settings, keywordSetting{urlDecoded(key), urlDecoded(value)})
		}
	}
	return settings
}

// urlDecoded is part, a URL DSN's, decoded as pgx's uriDecode decodes it: the spaces at its ends
// dropped (`?x=1& user=…`, `?user =…` and `?host= ::1` name the user and the host), then
// percent-decoded ("+" is no space), or as written, its ends dropped, where it does not decode.
func urlDecoded(part string) string {
	part = strings.Trim(part, " ")
	if decoded, err := url.PathUnescape(part); err == nil {
		return decoded
	}
	return part
}

// atPastUserinfo reports whether dsn is a postgres:// or postgresql:// URL holding an unencoded "@"
// after pgx's user part. pgx, like libpq, ends the userinfo at the first "@" before the first "/"
// (parseURLSettings), and only a "/" stops that search. So a password holding an "@" leaves the
// rest of itself after that first "@": in the host list (`postgres://u:<head>@<tail>@db/x`), in the
// database (`postgres://u:<head>@<tail>/<more>@db/x`) or in the query (`…@<tail>?k=<more>@db/x`).
// A password holding a "/" ends the search before its own "@", so pgx reads no userinfo at all,
// the user name and the password's head as the host and the port, and the rest as the database or
// the query (`postgres://u:4821/<tail>@db/x`). Either way an unencoded "@" stands after pgx's user
// part, which is what this reads: every "@" after the userinfo pgx cuts, or every "@" when it cuts
// none. A query value holding an "@" (`?user=sa@project.iam`) and a database name holding one are
// refused too: the rule cannot tell them from a password's tail. %40 serves a query value, since
// pgx decodes it; a database name holding an "@", written either way, is refused all the same
// (databaseHoldsAt). A "?" or "#" in a password stands before its "@", which pgx reads as the
// password's.
func atPastUserinfo(dsn string) bool {
	_, _, rest, isURL := cutURL(dsn)
	return isURL && strings.Contains(rest, "@")
}

// cutURL is dsn cut as pgx cuts a URL (parseURLSettings): whether it is one, a value that begins
// at its first byte with a lower-case postgres:// or postgresql://; its user info, the text after
// the scheme up to the first "@" ahead of the first "/", and whether it has one (only a "/" stops
// that search); and the rest, after that "@", or after the scheme when it has none. It is the one
// cut refusalOf, atPastUserinfo, urlSettings and dsnSecrets read, so they cannot disagree on where
// the user info ends.
func cutURL(dsn string) (userinfo string, hasUserinfo bool, rest string, isURL bool) {
	rest, isURL = strings.CutPrefix(dsn, "postgresql://")
	if !isURL {
		if rest, isURL = strings.CutPrefix(dsn, "postgres://"); !isURL {
			return "", false, "", false
		}
	}
	if i := strings.IndexAny(rest, "@/"); i >= 0 && rest[i] == '@' {
		return rest[:i], true, rest[i+1:], true
	}
	return "", false, rest, true
}

// ConnectError is err, a failure to parse, open or reach the Postgres dsn names, as an operator
// reads it, setting naming where dsn came from:
//
//   - A dsn Refusal refuses is named "the configured Postgres", with no driver detail: the
//     driver's text, or the server's, names the host, the database or a parameter's name, and part
//     of the credential stands there.
//   - A dsn pgx cannot parse (*pgconn.ParseConfigError) is named by setting alone, with no driver
//     detail: pgx quotes the DSN, and its own masking of a keyword DSN's password is best-effort
//     (`password = <secret>`, with spaces around the "=", prints whole).
//   - Any other failure names the address dsn points at and the driver's detail, with the password
//     taken out where a driver prints one (redactPassword).
//
// The error unwraps to err, so a caller still reads the driver's error by its type.
func ConnectError(setting, dsn string, err error) error {
	var unparsed *pgconn.ParseConfigError
	switch {
	case refusalOf(dsn) != kept:
		return &connectError{text: "connect to " + spilledAddress + ": the driver's error is not printed: it names the host, the database or a parameter's name, and part of the DSN's credential stands there", err: err}
	case errors.As(err, &unparsed):
		return &connectError{text: "connect to Postgres: " + setting + " cannot be parsed as a Postgres DSN; the driver's error is not printed, since it quotes the DSN", err: err}
	}
	address, password := target(dsn)
	return &connectError{text: fmt.Sprintf("connect to Postgres at %s: %s", address, redactPassword(err.Error(), append(dsnSecrets(dsn), password))), err: err}
}

// spilledAddress is how ConnectError names the Postgres of a DSN it cannot show without a
// credential.
const spilledAddress = "the configured Postgres"

// connectError is ConnectError's error: its text, and the driver's error it stands for.
type connectError struct {
	text string
	err  error
}

func (e *connectError) Error() string { return e.text }
func (e *connectError) Unwrap() error { return e.err }

// redactPassword hides a password where a driver would print one. First it masks each of secrets,
// the passwords the DSN carries as pgx read them (dsnSecrets, and the password pgx's parse found,
// which PGPASSWORD may set), wherever one follows a password= (in any case, with space around the
// "=" or not) or stands between the : and @ of a URL. Only in those positions: a password equal to
// the user or the database name is the ordinary devbox DSN (user, password and database all
// `legion`), and blanking every occurrence would hide the fields an operator reads to diagnose a
// refusal. Then, after every password= left, it masks the value that follows, read by libpq's own
// rules (passwordSetting), so a keyword DSN missing a space (`user=apppassword=<secret>`, a user
// name pgx and the server print whole) shows none of it:
//   - a value in single quotes, whole, to its closing quote, two single quotes in a row and a
//     backslash each taking the next character (`password='<secret>'`), or to the end of the text
//     when no quote closes it, and a value in double quotes alike;
//   - an unquoted value, a backslash taking the next character, to the next space, quote or
//     backtick (`password=<secret>`, as the server's `role "…password=<secret>"` and pgx's
//     “ `user=…password=<secret>` “ end it);
//   - and when more of the value follows that value, after one quote or more or a backtick
//     (`password=zq'<secret>`, and two quotes in a row before it) or straight after a closing quote
//     (`password='zq'<secret>`), everything after it, to the end of the text.
//
// A secret pgx read with an escaped space (`user=apppassword=zq\ <secret>`, printed as
// `apppassword=zq <secret>`) is masked whole by the first step.
func redactPassword(detail string, secrets []string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		after := regexp.MustCompile(`(?i)(password\s*=\s*)` + regexp.QuoteMeta(secret))
		detail = after.ReplaceAllString(detail, "${1}xxxxx")
		detail = strings.ReplaceAll(detail, ":"+secret+"@", ":xxxxx@")
	}
	return passwordSetting.ReplaceAllString(detail, "${1}xxxxx")
}

// passwordSetting is a password= and the value redactPassword masks after it, read by libpq's
// rules: a single-quoted run (two single quotes in a row or a backslash taking the next
// character), a double-quoted one alike, or an unquoted run, a backslash taking the next
// character, to a space, quote or backtick; and, when more of the value follows the run (a value
// character, after quotes or a backtick or none), the rest of the text.
var passwordSetting = regexp.MustCompile(`(?i)(password\s*=\s*)(?:'(?:''|\\[\s\S]|[^'\\])*'?|"(?:""|\\[\s\S]|[^"\\])*"?|(?:\\[\s\S]|[^\s'"` + "`" + `\\])*)(?:['"` + "`" + `]*[^\s'"` + "`" + `][\s\S]*)?`)

// dsnSecrets is every password dsn carries, as pgx reads it: its password and sslpassword (a
// keyword DSN's values, or a URL's user info password and query values, decoded), and the rest of
// any other value after a password= in it (`user=apppassword=<secret>`, a user missing the space
// before password=), which pgx and the server print as part of the user or the database.
func dsnSecrets(dsn string) []string {
	settings := keywordSettings(dsn)
	if _, _, _, isURL := cutURL(dsn); isURL {
		settings = urlSettings(dsn)
	}
	var secrets []string
	for _, setting := range settings {
		switch at := embeddedPassword.FindStringIndex(setting.value); {
		case setting.key == "password" || setting.key == "sslpassword":
			secrets = append(secrets, setting.value)
		case at != nil:
			secrets = append(secrets, setting.value[at[1]:])
		}
	}
	return secrets
}

// embeddedPassword is a password= inside another setting's value, as passwordSetting reads one.
var embeddedPassword = regexp.MustCompile(`(?i)password\s*=\s*`)

// target reads the address a DSN points at and the password it carries. pgx understands both the
// URL and the keyword/value forms; url.Parse answers when pgx's parse fails here though the failure
// ConnectError reports is another (a pool or a ping), so even then the operator learns the host.
// Neither reads part of a password as the host: they differ only for a DSN Refusal refuses, which
// ConnectError names before it asks target.
func target(dsn string) (string, string) {
	if config, err := pgconn.ParseConfig(dsn); err == nil && config.Host != "" {
		return net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))), config.Password
	}
	if parsed, err := url.Parse(dsn); err == nil && parsed.Host != "" {
		password, _ := parsed.User.Password()
		return parsed.Host, password
	}
	return spilledAddress, ""
}
