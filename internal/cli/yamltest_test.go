package cli

import (
	"bytes"
	"fmt"
	"io"
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

func requireYAMLStreamStdout(t *testing.T, stdout, stderr, schemaPath string) []any {
	t.Helper()
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("stdout is empty; want YAML stream output")
	}
	if stderr != "" {
		t.Fatalf("stderr = %q; want empty stderr for success", stderr)
	}
	docs := decodeYAMLStream(t, stdout)
	for _, doc := range docs {
		validateYAMLSchema(t, doc, schemaPath)
	}
	return docs
}

func TestListenOutputSchemaAndDiscovery(t *testing.T) {
	docs := decodeYAMLStream(t, "from: bob@example.test\nbody: first\n---\nto: alice@example.test/mobile\ntype: chat\nbody: second\n")
	if len(docs) != 2 {
		t.Fatalf("documents = %d; want 2", len(docs))
	}
	for _, doc := range docs {
		validateYAMLSchema(t, doc, "listen.schema.yaml")
	}

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"schemas"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	schemasDoc := requireYAMLStdout(t, stdout.String(), stderr.String(), "schemas.schema.yaml")
	if !hasSchemaEntry(t, schemasDoc, "listen", "spec/outputs/listen.schema.yaml") {
		t.Fatalf("schemas output missing listen entry: %#v", schemasDoc)
	}

	stdout.Reset()
	stderr.Reset()
	if err := Run([]string{"help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	helpDoc := requireYAMLStdout(t, stdout.String(), stderr.String(), "help.schema.yaml")
	if !hasCommandEntry(t, helpDoc, "listen", "listen") {
		t.Fatalf("help output missing listen command: %#v", helpDoc)
	}
	if !hasSchemaID(t, helpDoc, "listen") {
		t.Fatalf("help output missing listen schema id: %#v", helpDoc)
	}
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

func decodeYAMLStream(t *testing.T, raw string) []any {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	var docs []any
	for {
		var value any
		err := decoder.Decode(&value)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("YAML stream invalid: %v\n%s", err, raw)
		}
		if value == nil {
			continue
		}
		docs = append(docs, normalizeYAML(value))
	}
	return docs
}

func hasSchemaEntry(t *testing.T, doc any, id, path string) bool {
	t.Helper()
	root, ok := doc.(map[string]any)
	if !ok {
		t.Fatalf("schemas doc = %#v; want map", doc)
	}
	schemas, ok := root["schemas"].([]any)
	if !ok {
		t.Fatalf("schemas field = %#v; want list", root["schemas"])
	}
	for _, item := range schemas {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("schema entry = %#v; want map", item)
		}
		if entry["id"] == id && entry["path"] == path {
			return true
		}
	}
	return false
}

func hasCommandEntry(t *testing.T, doc any, name, schema string) bool {
	t.Helper()
	root, ok := doc.(map[string]any)
	if !ok {
		t.Fatalf("help doc = %#v; want map", doc)
	}
	commands, ok := root["commands"].([]any)
	if !ok {
		t.Fatalf("commands field = %#v; want list", root["commands"])
	}
	for _, item := range commands {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("command entry = %#v; want map", item)
		}
		if entry["name"] == name && entry["schema"] == schema {
			return true
		}
	}
	return false
}

func hasSchemaID(t *testing.T, doc any, id string) bool {
	t.Helper()
	root, ok := doc.(map[string]any)
	if !ok {
		t.Fatalf("help doc = %#v; want map", doc)
	}
	schemas, ok := root["schemas"].([]any)
	if !ok {
		t.Fatalf("schemas field = %#v; want list", root["schemas"])
	}
	for _, item := range schemas {
		if item == id {
			return true
		}
	}
	return false
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
