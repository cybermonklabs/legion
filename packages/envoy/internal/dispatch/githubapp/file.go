package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
)

// mediaTypeObject asks the contents read for one object whatever the path holds. Under it GitHub
// answers a file of over 1 MB with its size, the encoding "none" and no content, where its default
// JSON answer refuses such a file outright, so a file past the bound is told by its size alone.
const mediaTypeObject = "application/vnd.github.object+json"

// File reads the file at path in owner/repo at commit sha with one contents read under an
// installation token that reads contents (`GET /repos/{owner}/{repo}/contents/{path}?ref={sha}`),
// each part of path escaped on its own. The bound is notes.MaxFileSize, the bound of every file
// Dispatch reads of a repository: a file GitHub lists over it is notes.ErrFileTooLarge from that
// size, before any content is decoded, and so is content that decodes past it; the answer is read
// within blobResponseLimit, which base64 of a file at the bound fits. A 403 or 404 is
// ErrRepositoryUnreadable, as every repository read's is (CheckRepositoryResponse): GitHub answers
// 404 both for a path the commit does not hold and for a repository the installation cannot see.
// An answer that is no file (a directory, a symlink, a submodule) or not base64 is an error. path
// comes from the caller's own reading of GitHub's text, so errors show it as ShownBody does.
func (c *Client) File(ctx context.Context, token, owner, repo, sha, path string) ([]byte, error) {
	if c == nil {
		return nil, ErrNoAppKey
	}
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	repoFull := owner + "/" + repo
	target := c.base + repoPath(owner, repo) + "/contents/" + strings.Join(parts, "/") + "?ref=" + url.QueryEscape(sha)
	body, status, header, err := c.request(ctx, c.TokenScope(token), http.MethodGet, target, "Bearer "+token, mediaTypeObject, "", nil, blobResponseLimit)
	if err != nil {
		return nil, err
	}
	if err := CheckRepositoryResponse(status, header, body, repoFull, target); err != nil {
		return nil, err
	}
	var answer struct {
		Type     string `json:"type"`
		Size     int64  `json:"size"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("decode %s of %s at %s: %w", ShownBody(path), repoFull, sha, ShownErr(err))
	}
	if answer.Type != "file" {
		return nil, fmt.Errorf("%s of %s at %s is a %s, not a file", ShownBody(path), repoFull, sha, ShownBody(answer.Type))
	}
	if err := notes.CheckSize(ShownBody(path), answer.Size); err != nil {
		return nil, err
	}
	return decodeContent(fmt.Sprintf("%s of %s at %s", ShownBody(path), repoFull, sha), path, answer.Encoding, answer.Content)
}
