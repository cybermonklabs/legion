package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// listingCase is one of the three listings a commit's conditional reads list: its read through the
// client, the key GitHub lists its objects under, the members its readers read as a JSON scalar,
// the members they read as an object with the members read of each, and the mapping its readers
// make of one object.
type listingCase struct {
	name    string
	key     string
	read    func(*Client) (ObjectPage, error)
	scalars []string
	objects map[string][]string
	mapping func(map[string]any) any
}

var listingCases = []listingCase{
	{
		name: "check runs",
		key:  "check_runs",
		read: func(client *Client) (ObjectPage, error) {
			return client.CommitCheckRuns(context.Background(), "installation-token", "acme", "web", "c0ffee", 1, "")
		},
		scalars: []string{"id", "name", "head_sha", "status", "conclusion", "html_url", "started_at", "completed_at"},
		objects: map[string][]string{"app": {"id", "slug"}, "output": {"title", "summary"}},
		mapping: func(object map[string]any) any {
			observation, ok := contracts.GithubCIObservation("check_run", object, "acme", "web", "7")
			return [2]any{observation, ok}
		},
	},
	{
		name: "check suites",
		key:  "check_suites",
		read: func(client *Client) (ObjectPage, error) {
			return client.CommitCheckSuites(context.Background(), "installation-token", "acme", "web", "c0ffee", 1, "")
		},
		scalars: []string{"id", "head_sha", "status", "conclusion", "updated_at"},
		objects: map[string][]string{"app": {"id", "slug"}},
		mapping: func(object map[string]any) any {
			observation, ok := contracts.GithubCIObservation("check_suite", object, "acme", "web", "7")
			return [2]any{observation, ok}
		},
	},
	{
		name: "commit statuses",
		key:  "statuses",
		read: func(client *Client) (ObjectPage, error) {
			return client.CommitStatuses(context.Background(), "installation-token", "acme", "web", "c0ffee", 1, "")
		},
		scalars: []string{"context", "state", "created_at", "target_url", "description"},
		mapping: func(object map[string]any) any {
			observation, ok := contracts.GithubStatusObservation(object, "acme", "web", "7", "c0ffee")
			return [2]any{observation, ok}
		},
	},
}

// listedBodies is a page of each listing as GitHub answers it, each object with members its
// readers do not read beside the ones they do: one as GitHub sends it, one whose members GitHub
// answered null, one that leaves members out, ones whose members are of a kind GitHub never sends
// there, and a null item.
var listedBodies = map[string]string{
	"check runs": `{"total_count":7,"check_runs":[` +
		`{"id":4001,"name":"unit","node_id":"CR_1","head_sha":"c0ffee","external_id":"",` +
		`"url":"https://api.github.com/repos/acme/web/check-runs/4001","html_url":"https://github.com/acme/web/runs/4001",` +
		`"details_url":"https://github.com/acme/web/actions/runs/1/job/4001","status":"completed","conclusion":"success",` +
		`"started_at":"2026-11-17T05:00:00Z","completed_at":"2026-11-17T05:03:00Z",` +
		`"output":{"title":"Tests","summary":"All 12 passed","text":"every test's line","annotations_count":0},` +
		`"check_suite":{"id":21},"app":{"id":15368,"slug":"github-actions","name":"GitHub Actions","owner":{"login":"github"}},` +
		`"pull_requests":[{"number":7}]},` +
		`{"id":4002,"name":"lint","head_sha":"c0ffee","html_url":null,"status":"in_progress","conclusion":null,` +
		`"started_at":"2026-11-17T05:00:00Z","completed_at":null,"output":{"title":null,"summary":null},"app":null},` +
		`{"id":4003,"name":"tester","head_sha":"c0ffee","status":"completed"},` +
		`{"id":"4004","name":"build","head_sha":"c0ffee","status":"completed","conclusion":["success"],` +
		`"html_url":{"href":"https://github.com"},"started_at":7,"completed_at":true,"app":"github-actions",` +
		`"output":{"title":3,"summary":["s"]}},` +
		`{"id":4005.5,"name":7,"head_sha":"c0ffee","status":"completed","output":"done"},` +
		`{"id":4006,"name":"architect","head_sha":"c0ffee","status":"completed","conclusion":"success",` +
		`"app":{"id":"15368","slug":5}},` +
		`null]}`,
	"check suites": `{"total_count":6,"check_suites":[` +
		`{"id":21,"node_id":"CS_1","head_branch":"feature","head_sha":"c0ffee","status":"completed","conclusion":"success",` +
		`"url":"https://api.github.com/repos/acme/web/check-suites/21","before":"0000000","after":"c0ffee",` +
		`"pull_requests":[{"number":7}],"app":{"id":15368,"slug":"github-actions","owner":{"login":"github"}},` +
		`"created_at":"2026-11-17T05:00:00Z","updated_at":"2026-11-17T05:03:00Z","latest_check_runs_count":2,` +
		`"head_commit":{"id":"c0ffee","message":"fix the build"}},` +
		`{"id":22,"head_sha":"c0ffee","status":"queued","conclusion":null,"updated_at":null,"app":null},` +
		`{"id":true,"head_sha":"c0ffee","status":"completed","conclusion":7,"updated_at":["2026"],"app":{"id":"15368"}},` +
		`{"id":23.5,"head_sha":"c0ffee","status":"completed","app":{"id":false,"slug":null}},` +
		`{"head_sha":"c0ffee"},` +
		`null]}`,
	"commit statuses": `{"state":"failure","sha":"c0ffee","total_count":7,"statuses":[` +
		`{"url":"https://api.github.com/repos/acme/web/statuses/c0ffee","avatar_url":"https://avatars.example.com/u/1",` +
		`"id":1,"node_id":"SC_1","state":"success","description":"built","target_url":"https://ci.example.com/1",` +
		`"context":"ci/build","created_at":"2026-11-17T05:00:00Z","updated_at":"2026-11-17T05:00:00Z",` +
		`"creator":{"login":"ci-bot","id":9}},` +
		`{"id":2,"state":"FAILURE","description":null,"target_url":null,"context":"ci/lint","created_at":"2026-11-17T05:01:00Z"},` +
		`{"state":"error","context":"ci/deploy"},` +
		`{"state":"failure","context":"ci/odd","created_at":12,"target_url":{"href":"x"},"description":["d"]},` +
		`{"state":"pending","context":"ci/slow"},` +
		`{"state":"success","context":7,"description":true},` +
		`null],"repository":{"full_name":"acme/web"}}`,
}

// Each listing's objects, read through the client from a page GitHub answers, hold every member
// their readers read as encoding/json's own map of that page held it - a value of each kind,
// null, and a member left out alike - and no member no reader reads. An object or an array where
// a reader reads a scalar, and anything but an object or null where a reader reads an object, is
// held as nothing, of which no reader takes a value either. Each reader's mapping of an object
// (contracts.GithubCIObservation, contracts.GithubStatusObservation) is the one it made of
// encoding/json's map, and a null item is a nil object.
func TestListedObjectsHoldEveryMemberTheirReadersRead(t *testing.T) {
	for _, listing := range listingCases {
		t.Run(listing.name, func(t *testing.T) {
			body := listedBodies[listing.name]
			client, _ := startPollGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, body)
			})
			page, err := listing.read(client)
			if err != nil {
				t.Fatalf("the read: %v", err)
			}
			var answer map[string]any
			if err := json.Unmarshal([]byte(body), &answer); err != nil {
				t.Fatalf("the page as a map: %v", err)
			}
			items := answer[listing.key].([]any)
			if len(page.Objects) != len(items) {
				t.Fatalf("the read answered %d objects, want the page's %d", len(page.Objects), len(items))
			}
			for i, item := range items {
				want, _ := item.(map[string]any)
				got := page.Objects[i]
				where := fmt.Sprintf("item %d", i)
				if want == nil {
					if got != nil {
						t.Errorf("%s, null on the page, is %v, want a nil object", where, got)
					}
					continue
				}
				requireReadMembers(t, where, got, want, listing.scalars, listing.objects)
				if gotMapped, wantMapped := listing.mapping(got), listing.mapping(want); !reflect.DeepEqual(gotMapped, wantMapped) {
					t.Errorf("%s maps to %+v, want %+v, encoding/json's map's", where, gotMapped, wantMapped)
				}
			}
		})
	}
}

// requireReadMembers fails t unless got holds exactly the members of want that scalars and
// objects name, each as want holds it, but for a member of a kind no reader takes a value of
// there, which got must not hold.
func requireReadMembers(t *testing.T, where string, got, want map[string]any, scalars []string, objects map[string][]string) {
	t.Helper()
	for name := range got {
		if _, isObject := objects[name]; !isObject && !slices.Contains(scalars, name) {
			t.Errorf("%s holds %s, which no reader reads", where, name)
		}
	}
	for _, name := range scalars {
		wantValue, wantHeld := want[name]
		switch wantValue.(type) {
		case map[string]any, []any:
			wantValue, wantHeld = nil, false
		}
		gotValue, gotHeld := got[name]
		if gotHeld != wantHeld || !reflect.DeepEqual(gotValue, wantValue) {
			t.Errorf("%s's %s is %#v (held %v), want %#v (held %v)", where, name, gotValue, gotHeld, wantValue, wantHeld)
		}
	}
	for name, members := range objects {
		wantValue, wantHeld := want[name]
		gotValue, gotHeld := got[name]
		switch wantValue := wantValue.(type) {
		case nil:
			if gotHeld != wantHeld || gotValue != nil {
				t.Errorf("%s's %s is %#v (held %v), want null (held %v)", where, name, gotValue, gotHeld, wantHeld)
			}
		case map[string]any:
			gotObject, isObject := gotValue.(map[string]any)
			if !isObject {
				t.Errorf("%s's %s is %#v, want an object", where, name, gotValue)
				continue
			}
			requireReadMembers(t, where+"'s "+name, gotObject, wantValue, members, nil)
		default:
			if gotHeld {
				t.Errorf("%s's %s is %#v, want nothing for GitHub's %#v", where, name, gotValue, wantValue)
			}
		}
	}
}

// A page encoding/json would not decode into maps is refused as it was: a body that is no JSON, a
// list that is no list, an item that is no object, and a number a float64 cannot hold. The
// refusal quotes no value of the answer.
func TestListedObjectsRefuseWhatTheMapRefused(t *testing.T) {
	for _, listing := range listingCases {
		for _, body := range []string{
			`not json`,
			`{"` + listing.key + `":{"id":1}}`,
			`{"` + listing.key + `":[7]}`,
			`{"` + listing.key + `":[{"` + listing.scalars[0] + `":1e400}]}`,
		} {
			t.Run(listing.name+" "+body, func(t *testing.T) {
				var old map[string][]map[string]any
				if json.Unmarshal([]byte(body), &old) == nil {
					t.Fatalf("encoding/json decoded %s into maps; the case is no refusal", body)
				}
				client, _ := startPollGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
					fmt.Fprint(w, body)
				})
				page, err := listing.read(client)
				if err == nil {
					t.Fatalf("the read answered %+v, want the page refused", page)
				}
				for _, quoted := range []string{"not json", "1e400"} {
					if strings.Contains(err.Error(), quoted) {
						t.Errorf("the refusal %q quotes the answer's %s", err, quoted)
					}
				}
			})
		}
	}
}
