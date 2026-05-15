package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestJSONFlagRejectedWithYAMLError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run([]string{"--json"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected --json to be rejected")
	}
	doc := requireYAMLError(t, stdout.String(), stderr.String(), "error.schema.yaml")
	envelope, ok := doc.(map[string]any)["error"].(map[string]any)
	if !ok {
		t.Fatalf("error envelope missing: %#v", doc)
	}
	code, _ := envelope["code"].(string)
	message, _ := envelope["message"].(string)
	if code == "" || !strings.Contains(message, "--json") {
		t.Fatalf("unsupported --json envelope = %#v", envelope)
	}
}

func TestHelpOptimizedForDiscovery(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run([]string{"--help"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	requireYAMLStdout(t, out.String(), errOut.String(), "help.schema.yaml")
	out.Reset()
	errOut.Reset()
	if err := Run([]string{"schemas"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	requireYAMLStdout(t, out.String(), errOut.String(), "schemas.schema.yaml")
}

func TestConfigErrorsAreYAML(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run([]string{"connect"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected config error")
	}
	requireYAMLError(t, stdout.String(), stderr.String(), "error.schema.yaml")
}

func TestSuccessSchemas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
		value  any
	}{
		{"connect", "connect.schema.yaml", map[string]any{"connected": true}},
		{"send", "send.schema.yaml", map[string]any{"sent": true, "to": "bob@example.test"}},
		{"recv", "recv.schema.yaml", map[string]any{"from": "bob@example.test", "body": "hello"}},
		{"ping", "ping.schema.yaml", map[string]any{"id": "ping_1", "ok": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			if err := write(&stdout, false, tc.value, ""); err != nil {
				t.Fatal(err)
			}
			requireYAMLStdout(t, stdout.String(), "", tc.schema)
		})
	}
}
