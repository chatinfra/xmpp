package cli

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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

func TestRootHelpHasSections(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "long flag", args: []string{"--help"}},
		{name: "help command", args: []string{"help"}},
		{name: "bare", args: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runCLI(t, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q; want empty", stderr)
			}
			for _, header := range []string{"USAGE", "COMMANDS", "FLAGS", "OUTPUT", "EXAMPLES"} {
				if !hasHeader(stdout, header) {
					t.Fatalf("help missing %s header:\n%s", header, stdout)
				}
			}
		})
	}
}

func TestRootHelpListsAllCommands(t *testing.T) {
	stdout, stderr, err := runCLI(t, "help")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q; want empty", stderr)
	}
	for _, cmd := range []string{"connect", "send", "recv", "listen", "ping", "schemas"} {
		if !commandLineHasSummary(stdout, cmd) {
			t.Fatalf("COMMANDS section missing %q with summary:\n%s", cmd, stdout)
		}
	}
}

func TestRootHelpFlagsShowEnvDefaults(t *testing.T) {
	stdout, stderr, err := runCLI(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q; want empty", stderr)
	}
	for flagName, want := range map[string]string{
		"--jid":      "XMPP_JID",
		"--port":     "XMPP_PORT",
		"--password": "XMPP_PASS",
		"--host":     "XMPP_HOST",
		"--resource": "XMPP_RESOURCE",
	} {
		line := lineContaining(sectionLines(stdout, "FLAGS"), flagName)
		if line == "" || !strings.Contains(line, want) {
			t.Fatalf("FLAGS line for %s = %q; want inline %s in:\n%s", flagName, line, want, stdout)
		}
	}
}

func TestRootHelpIsNotOldYAMLShape(t *testing.T) {
	stdout, stderr, err := runCLI(t, "help")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q; want empty", stderr)
	}

	var doc any
	if err := yaml.Unmarshal([]byte(stdout), &doc); err != nil {
		return
	}
	root, ok := normalizeYAML(doc).(map[string]any)
	if !ok {
		return
	}
	for _, key := range []string{"commands", "flags", "schemas"} {
		if _, ok := root[key]; ok {
			t.Fatalf("help parsed as old YAML key %q: %#v\n%s", key, root, stdout)
		}
	}
}

func TestPerCommandHelp(t *testing.T) {
	for _, cmd := range []string{"connect", "send", "recv", "listen", "ping", "schemas"} {
		for _, args := range [][]string{{"help", cmd}, {cmd, "--help"}} {
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				stdout, stderr, err := runCLI(t, args...)
				if err != nil {
					t.Fatalf("Run(%v) error = %v; stderr=%s", args, err, stderr)
				}
				if stderr != "" {
					t.Fatalf("stderr = %q; want empty", stderr)
				}
				for _, header := range []string{"USAGE", "FLAGS", "OUTPUT", "EXAMPLES"} {
					if !hasHeader(stdout, header) {
						t.Fatalf("%s help missing %s header:\n%s", cmd, header, stdout)
					}
				}
				if !strings.Contains(stdout, "→ "+cmd) {
					t.Fatalf("%s help missing schema reference:\n%s", cmd, stdout)
				}
			})
		}
	}
}

func TestListenHelpDocumentsTimeoutZero(t *testing.T) {
	stdout, stderr, err := runCLI(t, "listen", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q; want empty", stderr)
	}
	if !strings.Contains(stdout, "--timeout 0 listens until SIGINT/SIGTERM") {
		t.Fatalf("listen help missing indefinite timeout behavior:\n%s", stdout)
	}
}

func TestUnknownCommandHelpReturnsYAMLErrorWithHint(t *testing.T) {
	for _, args := range [][]string{{"help", "bogus"}, {"bogus", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			stdout, stderr, err := runCLI(t, args...)
			if err == nil {
				t.Fatalf("Run(%v) succeeded; want error", args)
			}
			doc := requireYAMLError(t, stdout, stderr, "error.schema.yaml")
			envelope, ok := doc.(map[string]any)["error"].(map[string]any)
			if !ok {
				t.Fatalf("error envelope missing: %#v", doc)
			}
			if envelope["code"] != "unknown_command" {
				t.Fatalf("code = %#v; want unknown_command", envelope["code"])
			}
			hint, _ := envelope["hint"].(string)
			if !strings.Contains(hint, "xmpp help") {
				t.Fatalf("hint = %q; want xmpp help", hint)
			}
		})
	}
}

func TestSchemasCommandStillDiscoversListenSchema(t *testing.T) {
	stdout, stderr, err := runCLI(t, "schemas")
	if err != nil {
		t.Fatal(err)
	}
	schemasDoc := requireYAMLStdout(t, stdout, stderr, "schemas.schema.yaml")
	requireSchemaDiscoveryPaths(t, schemasDoc)
	if !hasSchemaEntry(t, schemasDoc, "listen", "spec/outputs/listen.schema.yaml") {
		t.Fatalf("schemas output missing listen entry: %#v", schemasDoc)
	}
	if hasSchemaEntry(t, schemasDoc, "help", "spec/outputs/help.schema.yaml") {
		t.Fatalf("schemas output advertises terminal help schema: %#v", schemasDoc)
	}
}

func TestConfigErrorsAreYAML(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run([]string{"connect"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected config error")
	}
	requireYAMLError(t, stdout.String(), stderr.String(), "error.schema.yaml")
}

func TestListenEmitsMultipleMessagesOnOneConnection(t *testing.T) {
	server := startFakeXMPPServer(t, fakeXMPPOptions{stanzas: []string{
		messageStanza("bob@example.test", "first"),
		messageStanza("carol@example.test", "second"),
	}})

	var stdout, stderr bytes.Buffer
	if err := Run(server.listenArgs(t, "100ms"), &stdout, &stderr); err != nil {
		t.Fatalf("Run(listen) error = %v; stderr=%s", err, stderr.String())
	}
	server.wait(t)
	if got := server.connections.Load(); got != 1 {
		t.Fatalf("connections = %d; want 1", got)
	}
	docs := requireYAMLStreamStdout(t, stdout.String(), stderr.String(), "listen.schema.yaml")
	requireMessageBodies(t, docs, []string{"first", "second"})
}

func TestListenSkipsEmptyBodyMessages(t *testing.T) {
	server := startFakeXMPPServer(t, fakeXMPPOptions{stanzas: []string{
		messageStanza("bob@example.test", "before"),
		`<message from='bob@example.test' to='alice@example.test/mobile' type='chat'><active xmlns='http://jabber.org/protocol/chatstates'/></message>`,
		messageStanza("bob@example.test", "after"),
	}})

	var stdout, stderr bytes.Buffer
	if err := Run(server.listenArgs(t, "100ms"), &stdout, &stderr); err != nil {
		t.Fatalf("Run(listen) error = %v; stderr=%s", err, stderr.String())
	}
	server.wait(t)
	docs := requireYAMLStreamStdout(t, stdout.String(), stderr.String(), "listen.schema.yaml")
	requireMessageBodies(t, docs, []string{"before", "after"})
}

func TestListenTimeoutExitsZeroWithoutErrorEnvelope(t *testing.T) {
	server := startFakeXMPPServer(t, fakeXMPPOptions{})

	var stdout, stderr bytes.Buffer
	started := time.Now()
	if err := Run(server.listenArgs(t, "80ms"), &stdout, &stderr); err != nil {
		t.Fatalf("Run(listen) error = %v; stderr=%s", err, stderr.String())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("listen timeout took %s; want prompt cancellation", elapsed)
	}
	server.wait(t)
	if stdout.String() != "" {
		t.Fatalf("stdout = %q; want empty stdout", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q; want empty stderr", stderr.String())
	}
}

func TestListenSignalCancellationExitsZeroAndClosesConnection(t *testing.T) {
	cancelReady := make(chan context.CancelFunc, 1)
	original := listenSignalContext
	listenSignalContext = func(ctx context.Context) (context.Context, context.CancelFunc) {
		signalCtx, cancel := context.WithCancel(ctx)
		cancelReady <- cancel
		return signalCtx, cancel
	}
	t.Cleanup(func() { listenSignalContext = original })

	server := startFakeXMPPServer(t, fakeXMPPOptions{onReady: func() {
		cancel := <-cancelReady
		cancel()
	}})

	var stdout, stderr bytes.Buffer
	started := time.Now()
	if err := Run(server.listenArgs(t, "0"), &stdout, &stderr); err != nil {
		t.Fatalf("Run(listen) error = %v; stderr=%s", err, stderr.String())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("signal cancellation took %s; want prompt cancellation", elapsed)
	}
	server.wait(t)
	select {
	case <-server.closedStream:
	default:
		t.Fatal("server did not observe clean stream close")
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q; want empty stdout", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q; want empty stderr", stderr.String())
	}
}

func TestListenAuthFailureReturnsYAMLError(t *testing.T) {
	server := startFakeXMPPServer(t, fakeXMPPOptions{authFailure: true})

	var stdout, stderr bytes.Buffer
	err := Run(server.listenArgs(t, "1s"), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected listen auth failure")
	}
	server.wait(t)
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
		{"listen", "listen.schema.yaml", map[string]any{"from": "bob@example.test", "body": "hello"}},
		{"ping", "ping.schema.yaml", map[string]any{"id": "ping_1", "ok": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			if err := writeYAMLResult(&stdout, tc.value); err != nil {
				t.Fatal(err)
			}
			requireYAMLStdout(t, stdout.String(), "", tc.schema)
		})
	}
}

func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := Run(args, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func hasHeader(text, header string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == header {
			return true
		}
	}
	return false
}

func commandLineHasSummary(text, command string) bool {
	line := lineContaining(sectionLines(text, "COMMANDS"), command)
	if line == "" {
		return false
	}
	fields := strings.Fields(line)
	return len(fields) > 1 && fields[0] == command
}

func sectionLines(text, header string) []string {
	var lines []string
	inSection := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == header {
			inSection = true
			continue
		}
		if inSection && isSectionHeader(trimmed) {
			break
		}
		if inSection && trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

func isSectionHeader(line string) bool {
	switch line {
	case "USAGE", "COMMANDS", "FLAGS", "ENVIRONMENT", "OUTPUT", "EXAMPLES", "SEE ALSO":
		return true
	default:
		return false
	}
}

func lineContaining(lines []string, token string) string {
	for _, line := range lines {
		if strings.Contains(line, token) {
			return line
		}
	}
	return ""
}

type fakeXMPPOptions struct {
	stanzas     []string
	authFailure bool
	onReady     func()
}

type fakeXMPPServer struct {
	addr         string
	done         chan error
	connections  atomic.Int32
	closedStream chan struct{}
}

func startFakeXMPPServer(t *testing.T, opts fakeXMPPOptions) *fakeXMPPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeXMPPServer{addr: listener.Addr().String(), done: make(chan error, 1), closedStream: make(chan struct{})}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			server.done <- err
			return
		}
		server.connections.Add(1)
		_ = listener.Close()
		server.done <- server.serve(conn, opts)
	}()
	return server
}

func (s *fakeXMPPServer) listenArgs(t *testing.T, timeout string) []string {
	t.Helper()
	host, port, err := net.SplitHostPort(s.addr)
	if err != nil {
		t.Fatal(err)
	}
	return []string{"--host", host, "--port", port, "--jid", "alice@example.test/mobile", "--password", "secret", "--plaintext", "--timeout", timeout, "listen"}
}

func (s *fakeXMPPServer) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-s.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fake XMPP server did not finish")
	}
}

func (s *fakeXMPPServer) serve(conn net.Conn, opts fakeXMPPOptions) error {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(conn)
	if _, err := readUntilMarker(reader, "<stream:stream"); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, `<stream:stream xmlns:stream='http://etherx.jabber.org/streams'><stream:features><mechanisms xmlns='urn:ietf:params:xml:ns:xmpp-sasl'><mechanism>PLAIN</mechanism></mechanisms></stream:features>`); err != nil {
		return err
	}
	if _, err := readUntilMarker(reader, "</auth>"); err != nil {
		return err
	}
	if opts.authFailure {
		_, err := io.WriteString(conn, `<failure xmlns='urn:ietf:params:xml:ns:xmpp-sasl'/>`)
		return err
	}
	if _, err := io.WriteString(conn, `<success xmlns='urn:ietf:params:xml:ns:xmpp-sasl'/>`); err != nil {
		return err
	}
	if _, err := readUntilMarker(reader, "<stream:stream"); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, `<stream:stream xmlns:stream='http://etherx.jabber.org/streams'><stream:features><bind xmlns='urn:ietf:params:xml:ns:xmpp-bind'/></stream:features>`); err != nil {
		return err
	}
	if _, err := readUntilMarker(reader, "</iq>"); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, `<iq type='result' id='bind_1'><bind xmlns='urn:ietf:params:xml:ns:xmpp-bind'><jid>alice@example.test/mobile</jid></bind></iq>`); err != nil {
		return err
	}
	if _, err := readUntilMarker(reader, "<presence/>"); err != nil {
		return err
	}
	if opts.onReady != nil {
		opts.onReady()
	}
	for _, stanza := range opts.stanzas {
		if _, err := io.WriteString(conn, stanza); err != nil {
			return err
		}
	}
	if _, err := readUntilMarker(reader, "</stream:stream>"); err != nil {
		return err
	}
	close(s.closedStream)
	return nil
}

func readUntilMarker(reader *bufio.Reader, marker string) (string, error) {
	var b strings.Builder
	for {
		r, _, err := reader.ReadRune()
		if err != nil {
			return b.String(), err
		}
		b.WriteRune(r)
		if strings.Contains(b.String(), marker) {
			return b.String(), nil
		}
	}
}

func messageStanza(from, body string) string {
	return `<message from='` + from + `' to='alice@example.test/mobile' type='chat'><body>` + body + `</body></message>`
}

func requireMessageBodies(t *testing.T, docs []any, want []string) {
	t.Helper()
	if len(docs) != len(want) {
		t.Fatalf("documents = %d; want %d (%#v)", len(docs), len(want), docs)
	}
	for i, doc := range docs {
		fields, ok := doc.(map[string]any)
		if !ok {
			t.Fatalf("doc %d = %#v; want map", i, doc)
		}
		if fields["body"] != want[i] {
			t.Fatalf("doc %d body = %#v; want %q", i, fields["body"], want[i])
		}
	}
}
