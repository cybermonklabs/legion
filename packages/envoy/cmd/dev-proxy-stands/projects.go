// packages/envoy/cmd/dev-proxy-stands/projects.go
// The two stands the Slack limit's check runs on (scripts/airlock-check.sh slack-limit): the
// Dispatch stand's project-access routes, which the broker's project-access client asks whether a
// project is private, whether a person may see it, what kind of member a person is and which Slack
// channels a private project's owner named; and the Slack stand, which a Limited grant's post
// reaches through the airlock.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
)

// projectAccessFile is what --project-access-file holds: the projects by key, the people by
// lowercase login and their member kind ("full" or "restricted"), and the one bearer the routes
// answer, which the broker reads from BROKER_PROJECT_ACCESS_TOKEN_FILE. For example:
//
//	{"rows": {"PRIV": {"visibility": "private", "owner": "owner@airlock.test",
//	                   "listed": ["person@airlock.test"], "slack_channels": ["C0ALLOWED1"]}},
//	 "people": {"person@airlock.test": "full"},
//	 "token": "<bearer>"}
type projectAccessFile struct {
	Rows   map[string]projectAccessRow `json:"rows"`
	People map[string]string           `json:"people"`
	Token  string                      `json:"token"`
}

// projectAccessRow is one project of the file, each field projectaccesstest.Row's of the same
// name.
type projectAccessRow struct {
	Visibility    string   `json:"visibility"`
	Owner         string   `json:"owner"`
	Listed        []string `json:"listed"`
	Repositories  []string `json:"repositories"`
	Resolved      []string `json:"resolved"`
	SlackChannels []string `json:"slack_channels"`
}

// readProjectAccessFile reads path as a projectAccessFile and answers the routes it holds. A file
// it cannot read, one that is not exactly one such object with no other key, and one that names
// no bearer are refused naming the flag, so a stack whose file is wrong does not start.
func readProjectAccessFile(path string) (http.Handler, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--project-access-file: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var file projectAccessFile
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("--project-access-file %s is no project-access file: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("--project-access-file %s holds more than one JSON object", path)
	}
	if file.Token == "" {
		return nil, fmt.Errorf("--project-access-file %s names no token, the one bearer its routes answer", path)
	}
	rows := make(map[string]projectaccesstest.Row, len(file.Rows))
	for key, row := range file.Rows {
		rows[key] = projectaccesstest.Row{
			Visibility: row.Visibility, Owner: row.Owner, Listed: row.Listed,
			Repositories: row.Repositories, Resolved: row.Resolved, SlackChannels: row.SlackChannels,
		}
	}
	return projectaccesstest.NewHandler(rows, file.People, file.Token), nil
}

// accessRoutesPrefix opens every path of Dispatch's access routes, the project route and the
// person route alike.
const accessRoutesPrefix = "/api/v1/access/"

// accessAnswer is the Dispatch stand's answer: routes answers every request under
// accessRoutesPrefix and judge everything else, which is the judge's reads of an issue and an
// ask. With no routes it is judge alone, and the stand answers no access route at all.
func accessAnswer(routes http.Handler, judge proxyrulestest.Answer) proxyrulestest.Answer {
	if routes == nil {
		return judge
	}
	return proxyrulestest.Answers(func(w http.ResponseWriter, r *http.Request, _ proxyrulestest.Request) bool {
		if !strings.HasPrefix(r.URL.Path, accessRoutesPrefix) {
			return false
		}
		routes.ServeHTTP(w, r)
		return true
	}, judge)
}

// slackPostPath is the one Slack method the stand answers, the one a Limited grant may call.
const slackPostPath = "/api/chat.postMessage"

// slackAnswer answers a POST to slackPostPath {"ok":true}, as Slack answers a post it made, and
// leaves every other request to the stand's own body. The stand records each request whatever it
// answers, so a check reads the post's Authorization and its channel back at the recording route.
func slackAnswer(w http.ResponseWriter, _ *http.Request, seen proxyrulestest.Request) bool {
	if seen.Method != http.MethodPost || seen.Path != slackPostPath {
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.WriteString(w, "{\"ok\":true}\n")
	return true
}
