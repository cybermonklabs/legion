package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ownersPromptText is what the daemon's copy of the prompt of a role of a plan holds: the prompt,
// between a line before it and a line after it that name it as the owner's, so the agent reads that
// text as the owner's, between Legion's headless mechanics before it and Legion's worker part after
// it, which hold where the two differ. Both lines carry a mark made from the prompt's own hash, so
// a prompt cannot hold the closing line of its own copy and end the block early: a text that held
// it would hash to another mark.
func ownersPromptText(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	mark := hex.EncodeToString(sum[:8])
	closing := "End of the role prompt the issue's owner wrote (" + mark + ")."
	return "The text from here to the line \"" + closing + "\" is the prompt of your role, as the architect that owns this issue " +
		"wrote it in its plan. Where it and Legion's own instructions differ, Legion's instructions hold.\n\n" + prompt + "\n\n" + closing + "\n"
}

// planRolePromptFile is the daemon's own copy of the prompt of a role an owner's plan defines:
// <stateDir>/plan-roles/<sha256 of the copy's text, in hex>.md, with mode 0600, holding the prompt
// between the lines that name it as the owner's (ownersPromptText). A launch reads a role's
// prompt from a file (runtime.PromptParts.RolePromptPaths), and a copy named by its text is the
// one file every version that gives the role that text shares: two versions of one prompt read
// one file, and a changed prompt is another file. A copy in place is used again only while its
// text hashes to its name. A copy a crash cut short, empty or holding part of the text, hashes
// to another name, so it is written again before the launch uses it. The write goes through a
// file of its own beside it, synced, and a rename, so a reader finds the whole text or the copy
// it replaces, and a crash after the rename leaves the whole text.
func planRolePromptFile(stateDir, prompt string) (string, error) {
	text := ownersPromptText(prompt)
	sum := sha256.Sum256([]byte(text))
	dir := filepath.Join(stateDir, "plan-roles")
	path := filepath.Join(dir, hex.EncodeToString(sum[:])+".md")
	held, err := os.ReadFile(path)
	if err == nil && sha256.Sum256(held) == sum {
		return path, nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("the copy of a plan role's prompt: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("the copy of a plan role's prompt: %w", err)
	}
	staged, err := os.CreateTemp(dir, ".prompt-*")
	if err != nil {
		return "", fmt.Errorf("the copy of a plan role's prompt: %w", err)
	}
	defer os.Remove(staged.Name())
	if _, err := staged.WriteString(text); err != nil {
		staged.Close()
		return "", fmt.Errorf("the copy of a plan role's prompt: %w", err)
	}
	if err := staged.Sync(); err != nil {
		staged.Close()
		return "", fmt.Errorf("the copy of a plan role's prompt: %w", err)
	}
	if err := staged.Close(); err != nil {
		return "", fmt.Errorf("the copy of a plan role's prompt: %w", err)
	}
	if err := os.Rename(staged.Name(), path); err != nil {
		return "", fmt.Errorf("the copy of a plan role's prompt: %w", err)
	}
	return path, nil
}
