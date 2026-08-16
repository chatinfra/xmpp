package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	buildSourceDigest = strings.Repeat("b", 64)
	binaryDigest = func() (string, error) {
		return strings.Repeat("a", 64), nil
	}
	os.Exit(m.Run())
}

// testStateDir returns an isolated temporary directory for one test.
//
// It exists instead of t.TempDir() because these directories host the xmppd
// AF_UNIX control socket, and Linux limits sun_path to 108 bytes. t.TempDir()
// derives its directory name from the test name and nests it under $TMPDIR, so
// "<TMPDIR>/<LongTestName><random>/001/control.sock" overflows that limit under
// a deep CI workspace temp root and fails with "bind: invalid argument".
func testStateDir(t *testing.T) string {
	t.Helper()
	base := strings.TrimSpace(os.Getenv("SUPER_TMP_DIR"))
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", base, err)
	}
	directory, err := os.MkdirTemp(base, "xmppd-")
	if err != nil {
		t.Fatalf("MkdirTemp(%q): %v", base, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func TestWantsHelp(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "long", args: []string{"--help"}, want: true},
		{name: "short", args: []string{"-h"}, want: true},
		{name: "subcommand", args: []string{"help"}, want: true},
		{name: "none", args: nil},
		{name: "extra", args: []string{"--help", "extra"}},
		{name: "unknown", args: []string{"--version"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wantsHelp(tt.args); got != tt.want {
				t.Fatalf("wantsHelp(%v)=%t want %t", tt.args, got, tt.want)
			}
		})
	}
}

func TestDaemonRejectsUnknownRootArgumentsAndJSON(t *testing.T) {
	if err := validateDaemonArgs(nil); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"unknown"}, {"--json"}, {"--json=true"}} {
		if err := validateDaemonArgs(args); err == nil {
			t.Fatalf("args=%v were accepted", args)
		}
	}
}

func TestXmppdHelpHasSections(t *testing.T) {
	stdout := runMainForHelp(t, "--help")
	for _, header := range []string{"USAGE", "ENVIRONMENT", "OUTPUT", "EXAMPLES"} {
		if !hasHelpHeader(stdout, header) {
			t.Fatalf("help output missing %s header:\n%s", header, stdout)
		}
	}
}

func TestHelpDocumentsEnvironment(t *testing.T) {
	stdout := runMainForHelp(t, "--help")
	environment := strings.Join(helpSectionLines(stdout, "ENVIRONMENT"), "\n")
	for _, token := range []string{
		"XMPP_JID",
		"XMPP_PASS",
		"OPENCODE_BASE_URL",
		"OPENCODE_DIRECTORY",
		"OPENCODE_AGENT_ID",
		"OPENCODE_AGENT_NAME",
		"XMPP_ACCOUNT_STATUS",
		"XMPP_TENANT_ID",
		"XMPP_MUC_HOST",
		"XMPP_ROOM_JID",
		"XMPP_ROOM_NICKNAME",
		"XMPPD_ROOM_ENABLED",
		"XMPPD_ADMISSION_PATH",
		"CHATINFRA_INTERNAL_API_BASE_URL",
		"CHATINFRA_API_TOKEN",
		"XMPPD_STATE_DIR",
		"XMPP_PLAINTEXT",
		"OPENCODE_PROMPT_TIMEOUT",
	} {
		if !strings.Contains(environment, token) {
			t.Fatalf("ENVIRONMENT section missing %q:\n%s", token, stdout)
		}
	}
}

func TestHelpAliasesProduceIdenticalStdout(t *testing.T) {
	want := runMainForHelp(t, "--help")
	for _, arg := range []string{"help", "-h"} {
		if got := runMainForHelp(t, arg); got != want {
			t.Fatalf("xmppd %s help differs\nwant:\n%s\ngot:\n%s", arg, want, got)
		}
	}
}

func TestCtlHelpUsesTerminalSectionsAndDocumentsStdin(t *testing.T) {
	for _, args := range [][]string{{"ctl", "--help"}, {"help", "ctl"}} {
		stdout := runMainForArgs(t, args...)
		for _, header := range []string{"USAGE", "FLAGS", "OUTPUT", "EXAMPLES"} {
			if !hasHelpHeader(stdout, header) {
				t.Fatalf("ctl help missing %s:\n%s", header, stdout)
			}
		}
		if !strings.Contains(stdout, "< BODY") || strings.Contains(stdout, "--body") || strings.Contains(stdout, "--json") {
			t.Fatalf("ctl help does not enforce stdin/no-json contract:\n%s", stdout)
		}
	}
}

func runMainForHelp(t *testing.T, arg string) string {
	return runMainForArgs(t, arg)
}

func runMainForArgs(t *testing.T, args ...string) string {
	t.Helper()
	oldArgs := os.Args
	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"xmppd"}, args...)
	os.Stdout = writer
	defer func() {
		os.Args = oldArgs
		os.Stdout = oldStdout
		_ = reader.Close()
	}()

	main()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func hasHelpHeader(text, header string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == header {
			return true
		}
	}
	return false
}

func helpSectionLines(text, header string) []string {
	var lines []string
	inSection := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == header {
			inSection = true
			continue
		}
		if inSection && isXmppdSectionHeader(trimmed) {
			break
		}
		if inSection && trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

func isXmppdSectionHeader(line string) bool {
	switch line {
	case "USAGE", "ENVIRONMENT", "OUTPUT", "EXAMPLES":
		return true
	default:
		return false
	}
}
