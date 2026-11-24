package leakguardtest

// GhReadNotFound is what gh prints, exit 1, for the guard's REST visibility read of a repository
// GitHub answers 404 for: captured on 2026-11-19 at 22:13:36Z in an INFRA issue pod with
//
//	PUBLIC_LEAK_CHECK_VISIBILITY_READ=<repo> GH_HOST=github.com NO_COLOR=1 CLICOLOR_FORCE=0 \
//	  gh api --hostname github.com repos/<repo> --include --jq .visibility
//
// for a repository that does not exist. It is gh's standard output and standard error as the two
// print together, byte for byte: the status line, ended by a bare line feed; each header, ended by
// CRLF; a blank line; the body, with no line end of its own, so gh's error line follows it on the
// same line. Nothing in it is replaced: the capture names no repository and no account. A stand-in
// gh prints it and exits 1, as gh did.
const GhReadNotFound = "HTTP/2.0 404 Not Found\n" + ghReadHeaders +
	`{"message":"Not Found","documentation_url":"https://docs.github.com/rest/repos/repos#get-a-repository","status":"404"}` +
	"gh: Not Found (HTTP 404)\n"

// GhReadServerError is GhReadNotFound turned into a 500, which GitHub cannot be made to answer on
// demand: the status line `HTTP/2.0 500 Internal Server Error`, the same headers, the body
// `{"message":"Server Error"}`, and gh's error line for it, `gh: Server Error (HTTP 500)`. A stand-in
// gh prints it and exits 1.
const GhReadServerError = "HTTP/2.0 500 Internal Server Error\n" + ghReadHeaders +
	`{"message":"Server Error"}` +
	"gh: Server Error (HTTP 500)\n"

// ghReadHeaders are the headers of GhReadNotFound's capture, each ended by CRLF, and the blank line
// after them, which GhReadServerError shares.
const ghReadHeaders = "Access-Control-Allow-Origin: *\r\n" +
	"Access-Control-Expose-Headers: ETag, Link, Location, Retry-After, X-GitHub-OTP, X-RateLimit-Limit, X-RateLimit-Remaining, " +
	"X-RateLimit-Used, X-RateLimit-Resource, X-RateLimit-Reset, X-OAuth-Scopes, X-Accepted-OAuth-Scopes, X-Poll-Interval, " +
	"X-GitHub-Media-Type, X-GitHub-SSO, X-GitHub-Request-Id, Deprecation, Sunset, Warning\r\n" +
	"Content-Security-Policy: default-src 'none'\r\n" +
	"Content-Type: application/json; charset=utf-8\r\n" +
	"Date: Thu, 19 Nov 2026 22:13:36 GMT\r\n" +
	"Referrer-Policy: origin-when-cross-origin, strict-origin-when-cross-origin\r\n" +
	"Server: github.com\r\n" +
	"Strict-Transport-Security: max-age=31536000; includeSubdomains; preload\r\n" +
	"Vary: Accept-Encoding, Accept, X-Requested-With\r\n" +
	"X-Accepted-Github-Permissions: metadata=read\r\n" +
	"X-Content-Type-Options: nosniff\r\n" +
	"X-Frame-Options: deny\r\n" +
	"X-Github-Api-Version-Selected: 2022-11-28\r\n" +
	"X-Github-Edge-Region: westus2\r\n" +
	"X-Github-Media-Type: github.v3; format=json\r\n" +
	"X-Github-Request-Id: F206:11682F:23BE0F:379CCB:6AFF748F\r\n" +
	"X-Ratelimit-Limit: 15000\r\n" +
	"X-Ratelimit-Remaining: 14896\r\n" +
	"X-Ratelimit-Reset: 1795126699\r\n" +
	"X-Ratelimit-Resource: core\r\n" +
	"X-Ratelimit-Used: 104\r\n" +
	"X-Xss-Protection: 0\r\n" +
	"\r\n"
