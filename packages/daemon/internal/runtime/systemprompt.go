package runtime

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// SystemPromptFile is the name of the file a launch's system prompt is written to, whose path is
// the one value of the agent's `--append-system-prompt` (Oh My Pi) or `--append-system-prompt-file`
// (Claude Code): a pod's launcher writes it into the generation's private directory
// (runtime/sandbox), and a tmux launch and `legion controller start` under the secrets directory
// (WriteSystemPrompt).
//
// The prompt travels as a file because one argv string is bounded: Linux refuses any single
// argument longer than 131072 bytes with its NUL (MAX_ARG_STRLEN), and a deployment's
// instructions alone can bring a root architect's prompt there. A path is a few dozen bytes
// however long the prompt grows. Every other place that passes the path refers here for why.
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

// PromptTooLarge is a pod's launch refused because the one start frame that carries its role's
// composed system prompt to the pod's launcher, Frame bytes long, would be longer than the
// shimwire.MaxFrameBytes a launcher reads in one frame: the one bound left on a prompt once it no
// longer rides an argument, and one the daemon must meet itself. The daemon sets no bound of its
// own on a model's context, which it cannot read; Oh My Pi answers a request too large for its
// model. Nothing of the launch was started, and every try composes the same text from the same
// files until the deployment changes them, or, for a role of the issue's plan, until a later
// version of the plan changes its prompt, so supervise charges it no launch failure and fails the
// claim at once with this sentence, which names the role, the prompt's size and the limit
// (promptRefused).
type PromptTooLarge struct {
	Role  claim.Role
	Bytes int
	Frame int
}

func (e *PromptTooLarge) Error() string {
	return fmt.Sprintf("the %s's system prompt is %d bytes, and the start frame that carries it to its pod's launcher would be %d bytes, larger than the %d bytes a launcher reads in one frame",
		e.Role, e.Bytes, e.Frame, shimwire.MaxFrameBytes)
}

// WriteSystemPrompt writes text, a composed system prompt, to `<state_dir>/secrets/<owner>-system-prompt.md`
// as every secret file under stateDir is written (WriteSecretFile: 0600 in the 0700 secrets
// directory, replaced whole), and returns that path, the one the caller hands the agent. A tmux
// launch names the file for its claim token, so the daemon prunes it with the claim's other files
// (SecretsDir) and every launch of the claim rewrites it once its previous process is gone;
// `legion controller start` names it for its run (controllerStart), since two runs of it may
// overlap. It is written before the command that names it runs: Oh My Pi reads a value with no
// newline as a file once, at startup, and takes one it cannot open as the prompt's text itself,
// saying nothing.
func WriteSystemPrompt(stateDir, owner, text string) (string, error) {
	return WriteSecretFile(stateDir, owner+"-"+SystemPromptFile, "system prompt", text)
}
