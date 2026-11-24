package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The workflow BROKER_EXCHANGE_WORKFLOWS lists here: a run of release.yaml on main of
// example-org/example-repo, given project and person.
const (
	workflowRepository = "example-org/example-repo"
	workflowPath       = ".github/workflows/release.yaml"
	workflowRef        = "refs/heads/main"
	workflowSession    = "example-release"
)

// TestTheBinaryExchangesAWorkflowRunsGitHubToken runs the REAL broker binary with a workflow
// listed in BROKER_EXCHANGE_WORKFLOWS and a stand-in for GitHub's issuer, and trades a run's
// token minted by that issuer: it is answered an access token whose introspection names the
// session the request named, the entry's person and its one project; and the same token
// presented again is refused, because a run mints a token per request and the exchange takes each
// once. No other binary test drives the github identity provider. The control,
// TestTheBinaryRefusesAWorkflowRunOfAnotherRef, lists the workflow on another ref, and the same
// trade is refused.
func TestTheBinaryExchangesAWorkflowRunsGitHubToken(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	githubKey := github.PublishKey(t, "github-key")
	key := proofKey(t)
	apiToken := accountToken(t, cluster, clusterKey, apiAccount)
	broker := startBroker(t, brokerOptions{Env: append(exchangeEnv(t, databaseURL, cluster, github, longAccessSeconds),
		"BROKER_EXCHANGE_WORKFLOWS="+workflowRepository+"="+workflowPath+"="+workflowRef+"="+project+"="+person)})
	broker.AwaitRecord(t, "token exchange", map[string]any{"state": "on", "github": true, "workflows": 1})
	live := newLiveBroker(broker.Addr, key)
	runToken := mintRunToken(t, github, githubKey, workflowRef)

	status, body := live.exchange(t, workflowForm(runToken))
	accessToken, _ := body["access_token"].(string)
	if status != http.StatusOK || accessToken == "" {
		t.Fatalf("the exchange of a listed workflow run's token = %d %v, want 200 with an access token", status, body)
	}
	status, body = live.introspect(t, accessToken, apiToken)
	if status != http.StatusOK || body["active"] != true {
		t.Fatalf("introspection of the run's access token = %d %v, want 200 with the token live", status, body)
	}
	for _, field := range []struct {
		name      string
		got, want any
	}{
		{"session", body["session"], workflowSession},
		{"person", body["person"], person},
		{"projects", fmt.Sprint(body["projects"]), "[" + project + "]"},
		{"project_scope", body["project_scope"], oauthwire.ProjectScopeNamed},
	} {
		if field.got != field.want {
			t.Errorf("introspection answered %s %v, want %v", field.name, field.got, field.want)
		}
	}

	status, body = live.exchange(t, workflowForm(runToken))
	if status == http.StatusOK || body["access_token"] != nil {
		t.Fatalf("the same run token presented again = %d %v, want it refused: a run's token is taken once", status, body)
	}
}

// TestTheBinaryRefusesAWorkflowRunOfAnotherRef is the control of the test above: the same
// workflow listed on another ref, and the run's trade is refused invalid_request with no token.
func TestTheBinaryRefusesAWorkflowRunOfAnotherRef(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	cluster.PublishKey(t, "cluster-key")
	githubKey := github.PublishKey(t, "github-key")
	broker := startBroker(t, brokerOptions{Env: append(exchangeEnv(t, databaseURL, cluster, github, longAccessSeconds),
		"BROKER_EXCHANGE_WORKFLOWS="+workflowRepository+"="+workflowPath+"=refs/heads/release="+project+"="+person)})
	broker.AwaitRecord(t, "token exchange", map[string]any{"state": "on", "github": true, "workflows": 1})
	status, body := newLiveBroker(broker.Addr, proofKey(t)).exchange(t, workflowForm(mintRunToken(t, github, githubKey, workflowRef)))
	if status != http.StatusBadRequest || body["error"] != "invalid_request" || body["access_token"] != nil {
		t.Fatalf("the exchange of a run on a ref no entry lists = %d %v, want 400 invalid_request and no token", status, body)
	}
}

// mintRunToken is a token GitHub's issuer mints for one run of the listed workflow on gitRef,
// for the workflow audience, with a jti of its own.
func mintRunToken(t *testing.T, github *oidctest.Issuer, key *oidctest.Key, gitRef string) string {
	t.Helper()
	run := workflowRepository + "/" + workflowPath + "@" + gitRef
	claims := github.Claims("repo:"+workflowRepository+":ref:"+gitRef, workflowAudience)
	claims["jti"] = "run-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	claims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	claims["exp"] = time.Now().Add(10 * time.Minute).Unix()
	claims["repository"], claims["workflow_ref"], claims["job_workflow_ref"], claims["ref"] = workflowRepository, run, run, gitRef
	return github.Mint(t, key, claims)
}

// workflowForm is the trade of a run's token for an access token under workflowSession.
func workflowForm(runToken string) url.Values {
	form := exchangeForm(runToken, workflowSession)
	form.Set("subject_token_type", oauthwire.SubjectTokenTypeGitHub)
	return form
}
