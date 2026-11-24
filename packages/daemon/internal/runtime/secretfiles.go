package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SecretFile is one secret a process reads through a `<Variable>_FILE` pointer: the variable the
// pointer is named after, the file's path, and the value.
type SecretFile struct {
	Variable, Path, Value string
}

// WriteSecretFiles writes each secret to its 0600 file in the 0700 SecretsDir under stateDir,
// re-applying the directory's mode on every write — a directory's mkdir mode is masked and ignored
// when it exists (secrets.ts:83-98). Each file is replaced whole (ReplaceFile), so a live process
// reading it while it is rewritten reads the old value or the new, never an empty or partial one. A
// failed write names the file's variable. The caller owns the files' lifetimes.
func WriteSecretFiles(stateDir string, files []SecretFile) error {
	dir := SecretsDir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for _, file := range files {
		if err := ReplaceFile(file.Path, file.Value); err != nil {
			return fmt.Errorf("write the %s file: %w", file.Variable, err)
		}
	}
	return nil
}

// ReplaceFile writes value to a 0600 temporary file in path's directory and renames it over path,
// which swaps the whole file in one step: there is no moment where path is truncated and not yet
// written. It is also what the tmux runtime writes a pane's own files with, in a directory the
// pane can write (the Claude Code MCP configuration): the rename replaces a file, a fifo or a link
// at path without opening it, following it or waiting on it, and a directory at path, which no
// rename of a file replaces, is answered as the rename's error. The temporary file is removed when
// any step fails.
func ReplaceFile(path, value string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(value)
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}

// WriteSecretFile writes value as SecretFilePath(stateDir, name), a file a process is handed by
// its path, never the value, and returns that path: the Dispatch bearer every pane reads through
// its `_FILE` pointer, `legion controller start`'s controller secret, and a claim's system prompt
// (WriteSystemPrompt). variable names the file in a failed write's error.
func WriteSecretFile(stateDir, name, variable, value string) (string, error) {
	path := SecretFilePath(stateDir, name)
	if err := WriteSecretFiles(stateDir, []SecretFile{{Variable: variable, Path: path, Value: value}}); err != nil {
		return "", err
	}
	return path, nil
}

// DispatchTokenFileName is the daemon-held Dispatch bearer every pane reads through
// DISPATCH_TOKEN_FILE, as the shipped daemon names it under `<state_dir>/secrets`.
const DispatchTokenFileName = "dispatch-token"

// WriteDispatchTokenFile writes the Dispatch bearer as DispatchTokenFileName, returning the path
// every pane receives as DISPATCH_TOKEN_FILE. The value never enters tmux's argv or a pane's
// environment.
func WriteDispatchTokenFile(stateDir, token string) (string, error) {
	return WriteSecretFile(stateDir, DispatchTokenFileName, "DISPATCH_TOKEN", token)
}
