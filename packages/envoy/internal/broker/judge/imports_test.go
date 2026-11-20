package judge

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// providerSDKs are the module paths of the provider SDKs a model call could be made with directly,
// so "no provider SDK" is a list and not one name: a package that swapped one of them for another
// would still be making its own model calls.
var providerSDKs = []string{
	"github.com/anthropics/anthropic-sdk-go",
	"github.com/openai/openai-go",
	"github.com/sashabaranov/go-openai",
	"google.golang.org/genai",
	"github.com/google/generative-ai-go",
	"cloud.google.com/go/aiplatform",
	"github.com/aws/aws-sdk-go-v2/service/bedrock",
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime",
}

// The judge reaches its model through agentloop alone, the module's one client of the model
// gateway: no file of the package imports a provider's SDK, and model.go, the file that asks the
// model, imports agentloop and no net/http, since a provider call made with the standard library
// would import no SDK at all. The imports are read from the package's own files, the tests'
// aside, whatever their build tags.
func TestTheJudgeAsksItsModelThroughAgentloopAlone(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the package's files: %v", err)
	}
	imports := map[string][]string{}
	fileSet := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse the imports of %s: %v", path, err)
		}
		for _, spec := range file.Imports {
			imported := strings.Trim(spec.Path.Value, `"`)
			imports[imported] = append(imports[imported], path)
		}
	}
	if !slices.Contains(imports["github.com/sjawhar/envoy/agentloop"], "model.go") {
		t.Errorf("model.go does not import agentloop; the package's files import it in %v", imports["github.com/sjawhar/envoy/agentloop"])
	}
	for imported, importers := range imports {
		for _, sdk := range providerSDKs {
			if imported == sdk || strings.HasPrefix(imported, sdk+"/") {
				t.Errorf("%v import the provider SDK %s; the judge asks its model through agentloop alone", importers, imported)
			}
		}
	}
	if slices.Contains(imports["net/http"], "model.go") {
		t.Errorf("model.go imports net/http; the file that asks the model makes no HTTP call of its own")
	}
}
