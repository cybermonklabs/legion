package runtime

import (
	"testing"
)

// textsArm is an arm of the experiment `shorter` that names a folder of texts, and textsOf is the
// Texts of a launch of it.
var textsArm = Variant{Experiment: "shorter", Name: "folder", Texts: "/etc/legion/texts/shorter"}

func textsOf(arm Variant) *Texts {
	return &Texts{Folder: arm.Texts, Fingerprint: "ab12", Copy: "/state/texts/legion-omp-legion-43-tester/2/folder",
		Experiment: arm.Experiment, Variant: arm.Name,
		Fallback: TextsFallback{Prompt: PromptParts{RolePromptPaths: []string{"/roles/tester.md"}}}}
}

// The controller runs in no tree, so it is drawn into no arm and reads no arm's folder of texts:
// a controller's spec that carries one is refused before a runtime touches anything, as one that
// carries an arm is. A worker's spec reads a folder only as an arm it runs in names it, since its
// fallback is that arm dropped (Texts.Fallback): a folder no arm of the launch names, or one its
// arm names under another path, is refused, and the folder its arm names is taken.
func TestAControllerCarryingTextsIsRefused(t *testing.T) {
	owned := map[string]bool{"LEGION_BOOT_TOKEN_FILE": true}
	controller := controllerSpec()
	controller.Texts = textsOf(textsArm)
	if err := ValidateSpawnSpec(controller, owned); err == nil ||
		err.Error() != "spawn legion-omp-controller: the controller runs in no tree, so it reads no arm's folder of texts (got /etc/legion/texts/shorter)" {
		t.Fatalf("ValidateSpawnSpec of a controller carrying a folder of texts = %v, want its refusal", err)
	}

	for _, tc := range []struct {
		name     string
		variants []Variant
	}{
		{"a worker in no arm", nil},
		{"a worker in another arm of the experiment", []Variant{{Experiment: "shorter", Name: "control"}}},
		{"a worker whose arm names another folder", []Variant{{Experiment: "shorter", Name: "folder", Texts: "/etc/legion/texts/other"}}},
		{"a worker in the arm of another experiment", []Variant{{Experiment: "longer", Name: "folder", Texts: textsArm.Texts}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := spawnSpec()
			spec.Variants, spec.Texts = tc.variants, textsOf(textsArm)
			want := "spawn legion-omp-legion-43-tester: the folder of texts /etc/legion/texts/shorter of arm folder of experiment shorter is the folder of no arm the launch runs in"
			if err := ValidateSpawnSpec(spec, owned); err == nil || err.Error() != want {
				t.Fatalf("ValidateSpawnSpec = %v, want %q", err, want)
			}
		})
	}

	spec := spawnSpec()
	spec.Variants, spec.Texts = []Variant{{Experiment: "longer", Name: "on", ConfigFiles: []string{"/etc/legion/longer.yml"}}, textsArm}, textsOf(textsArm)
	if err := ValidateSpawnSpec(spec, owned); err != nil {
		t.Fatalf("a worker carrying the folder of the arm it runs in was refused: %v", err)
	}
}
