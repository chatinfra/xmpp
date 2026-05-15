package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

func requireYAMLStdout(t *testing.T, stdout, stderr, schemaPath string) any {
	t.Helper()
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("stdout is empty; want YAML output")
	}
	if stderr != "" {
		t.Fatalf("stderr = %q; want empty stderr for success", stderr)
	}
	doc := decodeYAML(t, stdout)
	if schemaPath != "" {
		validateYAMLSchema(t, doc, schemaPath)
	}
	return doc
}

func requireYAMLError(t *testing.T, stdout, stderr, schemaPath string) any {
	t.Helper()
	if stdout != "" {
		t.Fatalf("stdout = %q; want empty stdout for error", stdout)
	}
	if strings.TrimSpace(stderr) == "" {
		t.Fatalf("stderr is empty; want YAML error output")
	}
	doc := decodeYAML(t, stderr)
	if schemaPath != "" {
		validateYAMLSchema(t, doc, schemaPath)
	}
	return doc
}

func decodeYAML(t *testing.T, raw string) any {
	t.Helper()
	var value any
	if err := yaml.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("YAML invalid: %v\n%s", err, raw)
	}
	return normalizeYAML(value)
}

func validateYAMLSchema(t *testing.T, doc any, schemaPath string) {
	t.Helper()
	schemaFile := filepath.Join(yamlTestModuleRoot(t), "spec", "outputs", filepath.FromSlash(schemaPath))
	raw, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatalf("read schema %s: %v", schemaFile, err)
	}
	var schemaDoc any
	if err := yaml.Unmarshal(raw, &schemaDoc); err != nil {
		t.Fatalf("schema YAML invalid %s: %v", schemaFile, err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	url := "file://" + filepath.ToSlash(schemaFile)
	if err := compiler.AddResource(url, normalizeYAML(schemaDoc)); err != nil {
		t.Fatalf("add schema %s: %v", schemaFile, err)
	}
	schema, err := compiler.Compile(url)
	if err != nil {
		t.Fatalf("compile schema %s: %v", schemaFile, err)
	}
	if err := schema.Validate(doc); err != nil {
		t.Fatalf("YAML does not match schema %s: %v\ndoc=%#v", schemaFile, err, doc)
	}
}

func yamlTestModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found from %s", dir)
		}
		dir = parent
	}
}

func normalizeYAML(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = normalizeYAML(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[fmt.Sprint(key)] = normalizeYAML(item)
		}
		return out
	case []any:
		for i, item := range typed {
			typed[i] = normalizeYAML(item)
		}
		return typed
	default:
		return value
	}
}
