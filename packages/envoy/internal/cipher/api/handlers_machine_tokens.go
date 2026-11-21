// handlers_machine_tokens.go: the two routes Dispatch's switch of a project to private calls.
//
// GET /v1/admin/machine-tokens?project=<KEY> answers how many live tokens a developer box holds
// that list one project. Dispatch reads it before it completes a project's switch to private: the
// exchange mints no such token for a project Dispatch answers closed to developer boxes
// (internal/cipher/oauth's projectAccess), and Dispatch makes the project private only once none
// minted before is still live. With slack_channels, the switch's own call, it first records the
// project and those channels in the store of private projects (store.Store.RecordPrivateProject),
// so the broker fences them from public credentials while the switch can still commit.
//
// POST /v1/admin/private-projects/forget forgets such a record (store.Store.ForgetPrivateProject):
// Dispatch sends it for a switch that did not commit and for a project made public.
//
// Both are routes a listed service calls with its own ServiceAccount token (accountAuth), behind
// the power only Dispatch's caller name holds, and neither reads anything of Dispatch.
package api

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/store"
)

// machineTokensResponse is GET /v1/admin/machine-tokens's answer.
type machineTokensResponse struct {
	// Project is the key the request named.
	Project string `json:"project"`
	// LiveTokens is how many live access tokens act as their person and list the project: a
	// machine login's trades and a person's sign-ins on a machine, which are a developer box's.
	LiveTokens int `json:"live_tokens"`
	// LastExpiresAt is when the last of them expires, null when none is live: the latest a
	// switch to private waits for, unless they are revoked first.
	LastExpiresAt *time.Time `json:"last_expires_at"`
	// Record is the number the store gave the project's record, when the request recorded it
	// (slack_channels present), and absent otherwise.
	Record *int64 `json:"record,omitempty"`
}

// recordChannel is a Slack conversation id a record may name, as the Slack proxy reads one: C, D
// or G, then at least 8 capital letters or digits.
var recordChannel = regexp.MustCompile(`^[CDG][A-Z0-9]{8,}$`)

// maxRecordChannels is how many channels one record names: Dispatch's own bound on a project's list.
const maxRecordChannels = 16

// countMachineTokens answers how many live developer-box tokens list the project ?project= names
// (store.Store.LiveDeveloperBoxTokens). The key is read as the exchange reads a project
// (oauthwire.ParseProjects): one key, or 400 PROJECT_INPUT. A key no token lists, one Dispatch
// does not hold among them, answers 0: the broker holds no copy of Dispatch's projects, and the
// one caller that may ask, Dispatch, knows which it holds.
//
// With slack_channels present (the ids joined by commas, empty for none), the switch's own call,
// it first records the project and those channels (store.Store.RecordPrivateProject) and answers
// the record's number beside the count. It refuses 400 RECORD_INPUT, recording nothing, a channel
// that is no conversation id, more than 16, and a deadline_seconds that is missing, no whole
// number of seconds or over store.MaxPrivateSwitchDeadline. A record the store does not write is
// 503 DATABASE_UNAVAILABLE and no count, which Dispatch takes as a count it could not read.
func (s *server) countMachineTokens(w http.ResponseWriter, r *http.Request, _ string) {
	query := r.URL.Query()
	projects, err := oauthwire.ParseProjects(query.Get("project"))
	if err != nil || len(projects) != 1 {
		writeError(w, http.StatusBadRequest, "PROJECT_INPUT", "project must be one project key, as Dispatch writes one")
		return
	}
	project := projects[0]
	var record *int64
	if listed, present := query["slack_channels"]; present {
		channels, problem := recordInput(listed, query["deadline_seconds"])
		if problem != "" {
			writeError(w, http.StatusBadRequest, "RECORD_INPUT", problem)
			return
		}
		number, err := s.deps.Enroll.Store.RecordPrivateProject(r.Context(), project, channels)
		if errors.Is(err, store.ErrPrivateProjectInput) {
			writeError(w, http.StatusBadRequest, "RECORD_INPUT", "the project or its channels are not ones Dispatch sends: "+err.Error())
			return
		}
		if err != nil {
			writeUnavailable(w, "DATABASE_UNAVAILABLE", "record the private project", err)
			return
		}
		record = &number
	}
	live, err := s.deps.Enroll.Store.LiveDeveloperBoxTokens(r.Context(), project)
	if err != nil {
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "count the live developer-box tokens", err)
		return
	}
	answer := machineTokensResponse{Project: project, LiveTokens: live.Live, Record: record}
	if !live.LastExpiresAt.IsZero() {
		last := live.LastExpiresAt.UTC()
		answer.LastExpiresAt = &last
	}
	writeJSON(w, http.StatusOK, answer)
}

// recordInput reads a record's slack_channels and deadline_seconds values, answering the channels
// or what is wrong with them: one value each, the channels conversation ids, at most
// maxRecordChannels, and the deadline a whole number of seconds from 1 to
// store.MaxPrivateSwitchDeadline.
func recordInput(listed, deadline []string) ([]string, string) {
	if len(listed) != 1 {
		return nil, "slack_channels must be given once"
	}
	channels := []string{}
	if listed[0] != "" {
		channels = strings.Split(listed[0], ",")
	}
	if len(channels) > maxRecordChannels {
		return nil, "slack_channels names more than 16 channels"
	}
	for _, channel := range channels {
		if !recordChannel.MatchString(channel) {
			return nil, "slack_channels holds a value that is no Slack conversation id"
		}
	}
	if len(deadline) != 1 {
		return nil, "deadline_seconds must be given once with slack_channels"
	}
	seconds, err := strconv.Atoi(deadline[0])
	if err != nil || seconds < 1 || time.Duration(seconds)*time.Second > store.MaxPrivateSwitchDeadline {
		return nil, "deadline_seconds must be a whole number of seconds from 1 to " + strconv.Itoa(int(store.MaxPrivateSwitchDeadline/time.Second))
	}
	return channels, ""
}

// forgetPrivateProjectRequest is POST /v1/admin/private-projects/forget's body: the project, and
// the record a switch that did not commit left, or null for none, which a switch to public and a
// switch whose outcome is unknown send.
type forgetPrivateProjectRequest struct {
	Project string `json:"project"`
	Record  *int64 `json:"record"`
}

// forgetPrivateProject forgets the record the body names (store.Store.ForgetPrivateProject), which
// decides on its numbers and the database's time. It answers 200 {"outcome": "forgotten"} or
// {"outcome": "superseded"}, which Dispatch takes as delivered; 409 RECORD_IN_FLIGHT with
// retry_after_seconds, the whole seconds until the record's hold ends as this process's clock
// reads it, at least 1, while a forget naming no record meets a record whose switch can still
// commit; 400 PROJECT_INPUT for a body that names no one project key or a record that is not
// positive; and 503 DATABASE_UNAVAILABLE.
func (s *server) forgetPrivateProject(w http.ResponseWriter, r *http.Request, _ string) {
	var body forgetPrivateProjectRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "PROJECT_INPUT", "the body must be {\"project\": <key>, \"record\": <number or null>}")
		return
	}
	projects, err := oauthwire.ParseProjects(body.Project)
	if err != nil || len(projects) != 1 || (body.Record != nil && *body.Record <= 0) {
		writeError(w, http.StatusBadRequest, "PROJECT_INPUT", "project must be one project key, as Dispatch writes one, and record a positive number or null")
		return
	}
	answer, err := s.deps.Enroll.Store.ForgetPrivateProject(r.Context(), projects[0], body.Record)
	if err != nil {
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "forget the private project", err)
		return
	}
	if answer.Outcome == store.ForgetInFlight {
		writeRetryRefusal(w, http.StatusConflict, "RECORD_IN_FLIGHT",
			"the project's record belongs to a switch to private that may still commit; send the forget again after retry_after_seconds",
			max(1, int64(math.Ceil(time.Until(answer.RetryAt).Seconds()))))
		return
	}
	writeJSON(w, http.StatusOK, forgetPrivateProjectResponse{Outcome: string(answer.Outcome)})
}

// forgetPrivateProjectResponse is POST /v1/admin/private-projects/forget's 200 answer.
type forgetPrivateProjectResponse struct {
	// What the forget did: forgotten, the record is gone or none was held, or superseded, a later
	// switch recorded the project since and its record stays. Dispatch takes either as the forget
	// delivered.
	Outcome string `json:"outcome"`
}
