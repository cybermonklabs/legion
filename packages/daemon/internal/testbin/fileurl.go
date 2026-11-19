package testbin

import (
	"fmt"
	"strings"
)

// FileURL is path as Oh My Pi reports a module loaded from it: its import.meta.url, the file: URL
// url.pathToFileURL makes, which percent-encodes '%', '\', a control or non-ASCII byte, and each
// character of the URL path set (a space, '"', '#', '<', '>', '?', '`', '{', '}'). Measured on the
// pinned build: a directory named `tmp with space` comes back as `tmp%20with%20space`. A test's
// stand-in Oh My Pi reports paths this way, or a reader that parses the URL, as the boot gate does,
// cuts a path at a '?' its temporary directory holds. The stand-ins' shell url functions write the
// same encoding for the ASCII paths a test's temporary directory holds.
func FileURL(path string) string {
	var b strings.Builder
	b.WriteString("file://")
	for i := range len(path) {
		if c := path[i]; c <= ' ' || c >= 0x7f || strings.IndexByte("\"#%<>?\\`{}", c) >= 0 {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// FileURLShell is a POSIX sh function, `url <path>`, for a test's stand-in Oh My Pi: it prints
// `file://` and the path encoded byte for byte as FileURL encodes it, a control or non-ASCII byte
// included. It runs no other program, so a stand-in whose PATH names no sed, as an image's
// environment may, still reports the whole path; it reads the path byte by byte under LC_ALL=C in
// a subshell of its own, so the caller's locale is left as it was.
const FileURLShell = `url() (
	LC_ALL=C
	url_rest=$1
	printf 'file://'
	while [ -n "$url_rest" ]; do
		url_c=${url_rest%"${url_rest#?}"}
		url_rest=${url_rest#?}
		case $url_c in
		'"' | '#' | '%' | '<' | '>' | '?' | '\' | ` + "'`'" + ` | '{' | '}') printf '%%%02X' "'$url_c" ;;
		[[:graph:]]) printf '%s' "$url_c" ;;
		*) printf '%%%02X' "'$url_c" ;;
		esac
	done
)
`
