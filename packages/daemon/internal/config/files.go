package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sjawhar/envoy/fileerr"
)

// DeploymentInstructionsFile is the copy of the operator's instructions every pane's prompt reads,
// under the state directory.
const DeploymentInstructionsFile = "deployment-instructions.md"

// PlanGuidanceFile is the copy of the operator's plan guidance an owner's prompt reads, under the
// state directory.
const PlanGuidanceFile = "plan-guidance.md"

// DeploymentInstructionsCopy is the copy of the operator's instructions under stateDir: the one
// path WriteDeploymentInstructions writes and every launch's prompt reads.
func DeploymentInstructionsCopy(stateDir string) string {
	return filepath.Join(stateDir, DeploymentInstructionsFile)
}

// PlanGuidanceCopy is the copy of the operator's plan guidance under stateDir: the one path
// WritePlanGuidance writes and every owner's launch reads.
func PlanGuidanceCopy(stateDir string) string {
	return filepath.Join(stateDir, PlanGuidanceFile)
}

// ExperimentsDir is the directory under the state directory holding the copies of every declared
// variant's instructions, a directory per experiment: a launch's prompt reads the copy of each
// arm its unit was drawn into. It is a directory per experiment rather than one flat
// `<experiment>-<variant>.md` because no name may hold `/` while both may hold `-`
// (config.experimentNamePattern), so a flat name would let two pairs collide.
const ExperimentsDir = "experiments"

// MemoryInstructionsFile is the copy of the `memory` block's instructions every role of a
// memory-group tree's prompt reads, under the state directory: the name it has had since before
// any experiment was declared, which the block's arm keeps (Variant.MemoryBlock).
const MemoryInstructionsFile = "memory-instructions.md"

// VariantInstructionsFile is the copy of one declared variant's instructions file under the state
// directory: `<stateDir>/memory-instructions.md` for the arm the `memory` block declares
// (Variant.MemoryBlock), and `<stateDir>/experiments/<experiment>/<variant>.md` for an arm of the
// `experiments` key.
func VariantInstructionsFile(stateDir, experiment string, variant Variant) string {
	if variant.MemoryBlock {
		return filepath.Join(stateDir, MemoryInstructionsFile)
	}
	return filepath.Join(stateDir, ExperimentsDir, experiment, variant.Name+".md")
}

// ReadSecretPointer is the trimmed contents of the file a pointer key names — `variable` is the
// key as the operator wrote it (`envoy_token_file`, `operator_token_file`), so the refusal reads
// in their words. A set pointer is authoritative: a missing, unreadable, or blank file is a
// refusal naming the key, never a fallback to another source. A file that could not be read is
// named by the key and why alone (unreadable), and a blank one by its path too. The contents never
// appear in an error.
func ReadSecretPointer(variable, file string) (string, error) {
	contents, err := os.ReadFile(file)
	if err != nil {
		return "", unreadable(variable, err)
	}
	secret := strings.TrimSpace(string(contents))
	if secret == "" {
		return "", fmt.Errorf("%s names %s, which is empty", variable, file)
	}
	return secret, nil
}

// unreadable is a secret's file that could not be read, refused naming variable, the key or
// variable that names it, and why in fileerr's words (`it does not exist`), never the path or err's
// own text, which repeats it: a pointer is where a person who pasted the secret itself puts it, an
// nkey seed or a token, and such a refusal printed it whole, twice. It unwraps to err, so
// errors.Is(err, fs.ErrNotExist) still holds. fileerr is the daemon's one reading of such an
// error.
func unreadable(variable string, err error) error {
	return fmt.Errorf("%s names a file that could not be read: %w", variable, fileerr.Err(err))
}

// ReadPrivateSecretPointer is ReadSecretPointer for a secret a group- or other-readable copy of
// would be a second way in — the operator bearer, which buys a controller capability and opens every
// operator route, and the NATS nkey seed file `legion controller start` hands its Oh My Pi —
// `variable` is the key or flag as the operator wrote it (`operator_token_file`,
// `--operator-token-file`, `nats_nkey_seed_file`). The file is opened once, and the open descriptor
// must be a regular file whose mode grants its group and others nothing before its trimmed,
// non-empty contents are read from that same descriptor, so nothing swapped in between a check and
// a read is ever read. The open does not block, so a FIFO is refused rather than waited on. The
// contents never appear in an error.
func ReadPrivateSecretPointer(variable, file string) (string, error) {
	return readSecretFile(variable, file, func(os.FileInfo) secretMode { return ownerOnly })
}

// ReadGroupSecretPointer is ReadPrivateSecretPointer for the NATS nkey seed the daemon reads, whose
// group may read it when root owns it and the reader is not root: a daemon running as a non-root
// uid in a pod reads a kubelet-mounted Secret file, always root's, only through the pod's fsGroup.
// Such a file is refused only when its group can write it or others can touch it at all. Any other
// file — the reader's own seed on a shared host, or another user's — is held to
// ReadPrivateSecretPointer's 0600. The owner and mode come from the descriptor the contents are
// read from, so the file judged is the file read.
func ReadGroupSecretPointer(variable, file string) (string, error) {
	return readSecretFile(variable, file, func(info os.FileInfo) secretMode {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid == mountOwner && readerUID() != int(mountOwner) {
			return groupMount
		}
		return ownerOnly
	})
}

// readerUID is the uid reading the file: when it is root, a root-owned file is its own, held to
// 0600. Tests replace it.
var readerUID = os.Geteuid

// mountOwner is the uid a kubelet's Secret files belong to, root; tests replace it.
var mountOwner uint32 = 0

// secretMode is the mode a secret file must keep: forbidden holds the permission bits refused, and
// says and fix word the refusal.
type secretMode struct {
	forbidden os.FileMode
	says, fix string
}

var (
	ownerOnly  = secretMode{0o077, "is readable by its group or others", "chmod 0600 it"}
	groupMount = secretMode{0o027, "is writable by its group or open to others", "chmod g-w,o-rwx it, or mount it with defaultMode: 0440"}
)

// readSecretFile is ReadPrivateSecretPointer's open, check, and read, refusing the bits of the mode
// modeFor answers for the open file. A file that could not be opened, examined or read is named by
// variable and why alone (unreadable); one that was opened is named by its path too.
func readSecretFile(variable, file string, modeFor func(os.FileInfo) secretMode) (string, error) {
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", unreadable(variable, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", unreadable(variable, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s names %s, which is not a regular file", variable, file)
	}
	if m, mode := modeFor(info), info.Mode().Perm(); mode&m.forbidden != 0 {
		return "", fmt.Errorf("%s %s %s (mode %#o); %s", variable, file, m.says, mode, m.fix)
	}
	contents, err := io.ReadAll(f)
	if err != nil {
		return "", unreadable(variable, err)
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return "", fmt.Errorf("%s names %s, which is empty", variable, file)
	}
	return token, nil
}

// ReadDeploymentInstructions is the operator's instructions file, refused naming the operator's
// path when it is missing, unreadable, or blank: a configured file with nothing in it is a
// misconfiguration, not an empty fragment. It writes nothing, so `legion controller start` runs it
// before the one daemon call it makes.
func ReadDeploymentInstructions(instructionsPath string) ([]byte, error) {
	contents, err := os.ReadFile(instructionsPath)
	if err != nil {
		return nil, fmt.Errorf("instructions file %s could not be read: %w", instructionsPath, err)
	}
	if strings.TrimSpace(string(contents)) == "" {
		return nil, fmt.Errorf("instructions file %s is empty", instructionsPath)
	}
	return contents, nil
}

// MaterializeDeploymentInstructions reads the operator's instructions file and writes
// `# Deployment instructions (<legionID>)`, a blank line, and its contents verbatim to
// `<stateDir>/deployment-instructions.md`, returning that path — the file every launch's composed
// system prompt ends with (runtime.ComposePrompt), so a role is never handed the operator's own
// path, which a pod cannot read. legionID is `project` as the operator wrote it.
//
// A file ReadDeploymentInstructions refuses writes nothing.
func MaterializeDeploymentInstructions(instructionsPath, stateDir, legionID string) (string, error) {
	contents, err := ReadDeploymentInstructions(instructionsPath)
	if err != nil {
		return "", err
	}
	if err := WriteDeploymentInstructions(contents, stateDir, legionID); err != nil {
		return "", err
	}
	return DeploymentInstructionsCopy(stateDir), nil
}

// WriteDeploymentInstructions is MaterializeDeploymentInstructions' write, of contents
// ReadDeploymentInstructions answered, to DeploymentInstructionsCopy: boot reads the file with
// every other refusal before it writes anything, and writes this copy before the first pane
// opens; the daemon writes it again whenever a launch reads an edited file. The copy is replaced
// whole, so a launch composing its prompt while it is written reads the old copy or the new one,
// never half of one.
func WriteDeploymentInstructions(contents []byte, stateDir, legionID string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory %s: %w", stateDir, err)
	}
	target := DeploymentInstructionsCopy(stateDir)
	body := fmt.Sprintf("# Deployment instructions (%s)\n\n%s", legionID, contents)
	if err := writeAtomically(target, body); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	return nil
}

// PlanGuidanceMaxBytes is the most a plan guidance file may hold. Every owner reads the whole
// guidance at every launch, after Legion's own parts of the owner's prompt, which are about 90 KB
// today; under Kubernetes the composed prompt travels in one launcher start frame of at most
// shimwire.MaxFrameBytes (1 MiB) with the deployment's instructions and each arm's, and an owner
// prompt past it is refused at every owner launch. 64 KiB keeps the guidance a small part of both,
// about 16,000 tokens of standing text, which is far more than a guidance of a few points needs.
const PlanGuidanceMaxBytes = 64 << 10

// ReadPlanGuidance is the operator's plan guidance file (`plan_guidance`), refused naming the
// operator's path when it is missing, unreadable, or blank: a configured file with nothing in it is
// a misconfiguration, not an empty guidance. A path that is not a regular file (a directory, a FIFO,
// a device such as /dev/zero) and a file over PlanGuidanceMaxBytes are refused naming the key, so
// boot never reads forever and no owner launch is refused for a prompt the guidance made too long.
// It writes nothing, so boot reads it with every other refusal before it writes anything, and
// `legion config check` refuses what boot refuses.
func ReadPlanGuidance(path string) ([]byte, error) {
	// O_NONBLOCK so that the open of a FIFO with no writer returns, and the Stat below refuses it.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("plan guidance file %s could not be read: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("plan guidance file %s could not be read: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("plan_guidance names %s, which is not a regular file", path)
	}
	contents, err := io.ReadAll(io.LimitReader(f, PlanGuidanceMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("plan guidance file %s could not be read: %w", path, err)
	}
	if len(contents) > PlanGuidanceMaxBytes {
		return nil, fmt.Errorf("plan_guidance names %s, which holds more than %d bytes, the most an owner's prompt takes of it", path, PlanGuidanceMaxBytes)
	}
	if strings.TrimSpace(string(contents)) == "" {
		return nil, fmt.Errorf("plan guidance file %s is empty", path)
	}
	return contents, nil
}

// WritePlanGuidance writes `# Plan guidance of this deployment (<legionID>)`, a blank line,
// Legion's sentence that the guidance binds no plan, a blank line, and contents, which
// ReadPlanGuidance answered, verbatim to PlanGuidanceCopy, returning that path. Boot writes it
// before the first pane opens, so no launch reads the operator's own path, and the daemon writes it
// again whenever a launch reads an edited file, as it does the instructions' copies
// (instructionsCopies in internal/daemon). The copy is replaced whole, so an owner's launch
// composing its prompt while it is written reads the old copy or the new one, never half of one.
// Under Kubernetes the copy is on the daemon's own disk, which no pod reaches, and the composed
// prompt goes to the pod in its start command. Under tmux every pane runs as the daemon's Unix
// user, so a pane can rewrite the copy, and the next owner launch reads what it wrote until the
// daemon next reads an edit of the operator's file, as with the deployment instructions' copy.
func WritePlanGuidance(contents []byte, stateDir, legionID string) (string, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("create state directory %s: %w", stateDir, err)
	}
	target := PlanGuidanceCopy(stateDir)
	body := fmt.Sprintf("# Plan guidance of this deployment (%s)\n\nLegion requires none of this guidance and refuses no plan for a step it lacks.\n\n%s", legionID, contents)
	if err := writeAtomically(target, body); err != nil {
		return "", fmt.Errorf("write %s: %w", target, err)
	}
	return target, nil
}

// ReadMemoryInstructions is the project's memory instructions file
// (`projects.<KEY>.memory.instructions`, the memory arm of the experiment the block is), refused
// naming the operator's path when it is missing, unreadable, or blank: a configured file with
// nothing in it is a misconfiguration, not an empty fragment. It writes nothing, so boot reads it
// with every other refusal before it writes anything. It refuses in the block's own words rather
// than ReadVariantInstructions', since the documentation quotes them and the block is refused in
// no new way; an arm of `experiments.memory`, which is the `experiments` key's, is read by
// ReadVariantInstructions as every arm of that key is.
func ReadMemoryInstructions(instructionsPath string) ([]byte, error) {
	contents, err := os.ReadFile(instructionsPath)
	if err != nil {
		return nil, fmt.Errorf("memory instructions file %s could not be read: %w", instructionsPath, err)
	}
	if strings.TrimSpace(string(contents)) == "" {
		return nil, fmt.Errorf("memory instructions file %s is empty", instructionsPath)
	}
	return contents, nil
}

// ReadVariantInstructions is one declared variant's instructions file
// (`projects.<KEY>.experiments.<name>.variants[].instructions`), refused naming the operator's
// path, with the experiment and the arm that name it, when it is missing, unreadable, or blank: a
// configured key with nothing to append is a misconfiguration, not an empty fragment. It writes
// nothing, so boot reads every variant's file with every other refusal before it writes anything.
// The `memory` block's own `instructions` is read by ReadMemoryInstructions instead.
func ReadVariantInstructions(experiment, variant, instructionsPath string) ([]byte, error) {
	contents, err := os.ReadFile(instructionsPath)
	if err != nil {
		return nil, fmt.Errorf("instructions file %s of experiment %s variant %s could not be read: %w", instructionsPath, experiment, variant, err)
	}
	if strings.TrimSpace(string(contents)) == "" {
		return nil, fmt.Errorf("instructions file %s of experiment %s variant %s is empty", instructionsPath, experiment, variant)
	}
	return contents, nil
}

// ReadHookPrompt is the words of one hook's `prompt_file` (Hook.PromptFile), refused naming the
// hook and the operator's path when the file is missing, unreadable, or blank: a configured file
// with nothing in it is a misconfiguration, not an empty prompt, which is this loader's rule for
// every path key. It writes nothing, so boot reads it with every other refusal before it writes
// anything. The words come back without the whitespace around them, since a file's last newline
// is no part of what the hook says.
func ReadHookPrompt(hook, promptPath string) (string, error) {
	contents, err := os.ReadFile(promptPath)
	if err != nil {
		return "", fmt.Errorf("hook %s prompt_file %s could not be read: %w", hook, promptPath, err)
	}
	words := strings.TrimSpace(string(contents))
	if words == "" {
		return "", fmt.Errorf("hook %s prompt_file %s is empty", hook, promptPath)
	}
	return words, nil
}

// WriteVariantInstructions writes the copy of contents ReadVariantInstructions answered (or, for
// the `memory` block's arm, ReadMemoryInstructions), one per variant that names a file, at
// VariantInstructionsFile: boot reads every such file with every other refusal before it writes
// anything, and writes these copies before the first pane opens; the daemon writes one again
// whenever a launch reads an edited file. Each copy is replaced whole, so a launch composing its
// prompt while it is written reads the old copy or the new one, never half of one. The reads and
// the write are separate, where the deployment's own has a Materialize pair as well, because only
// the daemon writes a variant's copy and boot refuses before it writes. The path is no answer of
// its own: a reader of the copy builds it with VariantInstructionsFile, as a launch's arms do
// (experiment.Launch). The copy is headed `# Memory instructions (<legionID>)` for the block's
// arm, as the block's copy was before any experiment was declared, and
// `# <experiment>/<variant> instructions (<legionID>)` for an arm of the `experiments` key.
func WriteVariantInstructions(contents []byte, stateDir, legionID, experiment string, variant Variant) error {
	target := VariantInstructionsFile(stateDir, experiment, variant)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("create state directory %s: %w", filepath.Dir(target), err)
	}
	header := fmt.Sprintf("# %s/%s instructions (%s)", experiment, variant.Name, legionID)
	if variant.MemoryBlock {
		header = fmt.Sprintf("# Memory instructions (%s)", legionID)
	}
	if err := writeAtomically(target, header+"\n\n"+string(contents)); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	return nil
}
