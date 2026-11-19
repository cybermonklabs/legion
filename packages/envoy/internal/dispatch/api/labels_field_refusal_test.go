package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestALabelRefusalNamesTheFieldTheRequestSent holds the labels refusal to the field each
// request used: a create's and a patch's `labels`, a patch's `labels_add`, and the issue list's
// repeated `label` query parameter. Each row sends one good label and one 41 long, past the 40 a
// label holds, and wants the refusal to name that field and the second item.
func TestALabelRefusalNamesTheFieldTheRequestSent(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Label refusals",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[model.Issue](t, created)
	long := strings.Repeat("x", 41)
	cases := []struct {
		name string
		send func() (int, string)
		want string
	}{
		{name: "create labels", want: "labels[1] is 41 long; a label is 1 to 40 long", send: func() (int, string) {
			r := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "CORE", "title": "Labelled", "labels": []string{"ok", long},
			}, "alice")
			return r.Code, r.Body.String()
		}},
		{name: "patch labels", want: "labels[1] is 41 long; a label is 1 to 40 long", send: func() (int, string) {
			r := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
				"labels": []string{"ok", long},
			}, "alice")
			return r.Code, r.Body.String()
		}},
		{name: "patch labels_add", want: "labels_add[1] is 41 long; a label is 1 to 40 long", send: func() (int, string) {
			r := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
				"labels_add": []string{"ok", long},
			}, "alice")
			return r.Code, r.Body.String()
		}},
		{name: "list label", want: "label[1] is 41 long; a label is 1 to 40 long", send: func() (int, string) {
			query := url.Values{"project": {"CORE"}, "label": {"ok", long}}
			r := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?"+query.Encode(), nil, "alice")
			return r.Code, r.Body.String()
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := c.send()
			if code != http.StatusBadRequest || !strings.Contains(body, `"code":"LABELS_INPUT"`) || !strings.Contains(body, c.want) {
				t.Fatalf("status=%d body=%s, want 400 LABELS_INPUT naming %q", code, body, c.want)
			}
		})
	}
}
