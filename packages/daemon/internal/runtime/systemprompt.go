package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// SystemPromptFile is the name of the file a launch's system prompt is written to, whose path is
// the one value of the agent's `--append-system-prompt` (Oh My Pi) or `--append-system-prompt-file`
// (Claude Code): a pod's launcher writes it into the generation's private directory
// (runtime/sandbox), and a tmux launch under the daemon's secrets directory (SystemPromptPath).
//
// The prompt travels as a file because one argv string is bounded: Linux refuses any single
// argument longer than 131072 bytes with its NUL (MAX_ARG_STRLEN), and a deployment's
// instructions alone can bring a root architect's prompt there. A path is a few dozen bytes
// however long the prompt grows.
const SystemPromptFile = "system-prompt.md"

// ComposePrompt is the one text a launch's agent is started with: the role prompt files
// concatenated, then the addressing sentence, then the deployment instructions, then one text per
// arm the launch's unit was drawn into, in the order the project's file declares the experiments
// (PromptParts.VariantInstructionsPaths), separated by a blank line, each file's trailing newlines
// dropped. Every launch composes it here, in the daemon, from the daemon's own files: both
// runtimes and `legion controller start` write the result to a file and hand the agent that
// file's path.
func ComposePrompt(parts PromptParts) (string, error) {
	var role strings.Builder
	for _, path := range parts.RolePromptPaths {
		text, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("prompt file: %w", err)
		}
		role.Write(text)
	}
	fragments := []string{strings.TrimRight(role.String(), "\n")}
	if parts.Addressing != "" {
		fragments = append(fragments, parts.Addressing)
	}
	for _, path := range slices.Concat([]string{parts.DeploymentInstructionsPath}, parts.VariantInstructionsPaths) {
		if path == "" {
			continue
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("prompt file: %w", err)
		}
		fragments = append(fragments, strings.TrimRight(string(text), "\n"))
	}
	return strings.Join(fragments, "\n\n"), nil
}

// SystemPrompt is spec's composed prompt (ComposePrompt), refused as PromptTooLarge when it is
// larger than the context the deployment gives the role's model (SpawnSpec.ModelContextBytes,
// `model_context_bytes`). A runtime calls it before it writes or starts anything of the launch.
func SystemPrompt(spec SpawnSpec) (string, error) {
	text, err := ComposePrompt(spec.Prompt)
	if err != nil {
		return "", err
	}
	if spec.ModelContextBytes > 0 && len(text) > spec.ModelContextBytes {
		return "", &PromptTooLarge{Role: spec.Role, Bytes: len(text), Bound: spec.ModelContextBytes}
	}
	return text, nil
}

// PromptTooLarge is a launch refused because its role's composed system prompt is larger than a
// bound that still applies to it: the context the deployment gives the role's model
// (`model_context_bytes`, SystemPrompt), or, in a pod, the one start frame that carries the prompt
// to the pod's launcher (Frame set: the frame's size, Bound then being the most a launcher reads in
// one frame). Nothing of the launch was started, and every try composes the same text from the
// same files until the deployment changes them, so supervise charges it no launch failure and
// fails the claim at once with this sentence, which names the role, the prompt's size and the
// bound (launchNow).
type PromptTooLarge struct {
	Role  claim.Role
	Bytes int
	Bound int
	Frame int
}

func (e *PromptTooLarge) Error() string {
	if e.Frame == 0 {
		return fmt.Sprintf("the %s's system prompt is %d bytes, larger than the %d bytes model_context_bytes.%s gives its model's context",
			e.Role, e.Bytes, e.Bound, e.Role)
	}
	return fmt.Sprintf("the %s's system prompt is %d bytes, and the start frame that carries it to its pod's launcher would be %d bytes, larger than the %d bytes a launcher reads in one frame",
		e.Role, e.Bytes, e.Frame, e.Bound)
}

// SystemPromptPath is where a tmux launch writes its claim's system prompt:
// `<state_dir>/secrets/<claim>-system-prompt.md`, named for the claim token, so the daemon prunes
// it with the claim's other files (SecretsDir), and rewritten by every launch of the claim once
// its previous process is gone.
func SystemPromptPath(stateDir string, token claim.Token) string {
	return SecretFilePath(stateDir, string(token)+"-"+SystemPromptFile)
}

// WriteSystemPrompt writes text, a composed system prompt, to path, owner-only, in a directory it
// makes owner-only when missing. The file is replaced whole (replaceFile), so an agent reading it
// never reads it half written. Oh My Pi reads a value it cannot open as the prompt's text itself,
// so the file is written before the command that names it runs.
func WriteSystemPrompt(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("write the system prompt file: %w", err)
	}
	if err := replaceFile(path, text); err != nil {
		return fmt.Errorf("write the system prompt file: %w", err)
	}
	return nil
}
