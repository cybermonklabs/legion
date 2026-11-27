package githubrate

import (
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// echoedToken is an installation token's form short enough for a login's 39 characters, as
// whatever answers in GitHub's place can write a run of the token the read carried. It holds
// eyJhbGci, the start of every JWS, the marker the redaction gate reads its evidence by: the
// credential such an answer echoes here is the App's JWT the list carried. It is a login's
// length and holds a login's characters, so only the token-form check refuses it.
const echoedToken = "ghs_" + "eyJhbGcipv7q2d58"

// A reading's line names GitHub's words from GET /app/installations only in the shape GitHub gives
// them, and whatever answers in GitHub's place can write the JWT the list carried there: an owner
// or a slug that is no login, or holds a token's form, is named notALogin (owner, owner-token,
// slug); a permission GitHub does not document is left out and counted (permission-name); an
// access GitHub does not document is named notDocumented (permission-access). The line holds no
// run of the probe, and still names the installation, its documented permissions and what was
// left out. The twin shows each reading holds the probe as the list wrote it. Captures
// githubrate.reading-words/<case>.
func TestAReadingLineNamesGitHubsWordsOnlyInTheirShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reading Reading
		held    string
		// line is what the line must name of the case's values.
		line map[string]any
	}{
		{"owner", Reading{Owner: redacttest.Words}, redacttest.Words, map[string]any{"owner": notALogin}},
		{"owner-token", Reading{Owner: echoedToken}, echoedToken, map[string]any{"owner": notALogin}},
		{"slug", Reading{AppSlug: redacttest.Words + "[bot]"}, redacttest.Words, map[string]any{"app_slug": notALogin}},
		{"permission-name", Reading{Permissions: map[string]string{redacttest.Words: "read", "metadata": "read"}}, redacttest.Words,
			map[string]any{"permissions": map[string]any{"metadata": "read"}, "permissions_undocumented": 1}},
		{"permission-access", Reading{Permissions: map[string]string{"contents": redacttest.Words, "metadata": "read"}}, redacttest.Words,
			map[string]any{"permissions": map[string]any{"contents": notDocumented, "metadata": "read"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reading := tc.reading
			reading.Program, reading.InstallationID, reading.Installations = "dispatch", "101", 1
			listed := slices.Concat([]string{reading.Owner, reading.AppSlug}, slices.Collect(maps.Keys(reading.Permissions)), slices.Collect(maps.Values(reading.Permissions)))
			if !slices.Contains(listed, tc.held) && !slices.Contains(listed, tc.held+"[bot]") {
				t.Fatalf("the reading holds no %q, so the case cannot see a leak", tc.held)
			}
			var out logtest.Buffer
			Write(slog.New(slog.NewJSONHandler(&out, nil)), reading)
			written := out.String()
			redacttest.RequireNoPiece(t, "githubrate.reading-words/"+tc.name, written, echoedToken)
			lines := logtest.JSONLines(t, written)
			if len(lines) != 1 {
				t.Fatalf("Write wrote %d lines, want one:\n%s", len(lines), written)
			}
			for key, want := range tc.line {
				if got := lines[0][key]; !sameJSON(got, want) {
					t.Errorf("the line names %s %v, want %v", key, got, want)
				}
			}
			if got := lines[0]["installation_id"]; got != "101" {
				t.Errorf("the line names installation %v, want 101", got)
			}
		})
	}
}

// A clean reading's line names its slug, its owner and its permissions as GitHub listed them, an
// App's bot account among the logins and admin among the accesses, and counts nothing left out.
func TestAReadingLineNamesCleanWordsAsGitHubListedThem(t *testing.T) {
	var out logtest.Buffer
	Write(slog.New(slog.NewJSONHandler(&out, nil)), Reading{
		Program: "daemon", AppSlug: "example-app", Owner: "example-app[bot]", InstallationID: "101",
		Permissions: map[string]string{"contents": "write", "metadata": "read", "repository_projects": "admin"},
	})
	line := logtest.JSONLines(t, out.String())[0]
	if line["app_slug"] != "example-app" || line["owner"] != "example-app[bot]" {
		t.Errorf("the line names slug %v and owner %v, want them as listed", line["app_slug"], line["owner"])
	}
	if want := map[string]any{"contents": "write", "metadata": "read", "repository_projects": "admin"}; !sameJSON(line["permissions"], want) {
		t.Errorf("the line names permissions %v, want %v", line["permissions"], want)
	}
	if counted, ok := line["permissions_undocumented"]; ok {
		t.Errorf("the line counts %v permissions left out, where it left none out", counted)
	}
}

// A refusal of a REST answer's rate-limit headers names each header, and whatever answers in
// GitHub's place can write the call's token there: a value is named as the number it parses as,
// or as notANumber, and a counter as one of the two a reading reads, or as notACounter. None of
// the probe prints in the refusal (resource, used, limit, reset), which still names the header it
// refuses; a negative count is named as the number. Captures githubrate.core-bucket/<case>.
func TestACoreCounterRefusalNamesNoHeaderAsItCame(t *testing.T) {
	clean := http.Header{}
	clean.Set("X-RateLimit-Resource", Core)
	clean.Set("X-RateLimit-Used", "7")
	clean.Set("X-RateLimit-Limit", "5000")
	clean.Set("X-RateLimit-Reset", "1900000000")
	for _, tc := range []struct{ name, header, says string }{
		{"resource", "X-RateLimit-Resource", "names the " + notACounter + " counter, not core"},
		{"used", "X-RateLimit-Used", "X-RateLimit-Used count: " + notANumber},
		{"limit", "X-RateLimit-Limit", "X-RateLimit-Limit: " + notANumber},
		{"reset", "X-RateLimit-Reset", "X-RateLimit-Reset epoch second: " + notANumber},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := clean.Clone()
			headers.Set(tc.header, redacttest.Words)
			if !strings.Contains(headers.Get(tc.header), redacttest.Token) {
				t.Fatalf("the header %s holds no probe, so the case cannot see a leak", tc.header)
			}
			_, err := CoreBucket(headers)
			if err == nil {
				t.Fatalf("CoreBucket read %s=%q as a reading", tc.header, redacttest.Words)
			}
			redacttest.RequireNone(t, "githubrate.core-bucket/"+tc.name, err.Error())
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("CoreBucket = %v, want it to name %q", err, tc.says)
			}
		})
	}
	negative := clean.Clone()
	negative.Set("X-RateLimit-Used", "-1")
	if _, err := CoreBucket(negative); err == nil || !strings.Contains(err.Error(), `count: "-1"`) {
		t.Errorf("CoreBucket of a negative used count = %v, want it named as the number", err)
	}
}

// sameJSON reports whether got, a member of a line as encoding/json decoded it, is want as
// encoding/json writes it.
func sameJSON(got, want any) bool {
	gotJSON, gotErr := json.Marshal(got)
	wantJSON, wantErr := json.Marshal(want)
	return gotErr == nil && wantErr == nil && string(gotJSON) == string(wantJSON)
}
