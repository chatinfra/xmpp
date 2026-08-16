package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
	"github.com/chatinfra/xmpp/internal/xmpp"
)

type options struct {
	host       string
	port       int
	jid        string
	password   string
	resource   string
	to         string
	body       string
	timeout    time.Duration
	plaintxt   bool
	noPresence bool
}

type commandError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (e commandError) Error() string { return e.Message }

type errorEnvelope struct {
	Error commandError `json:"error"`
}

var listenSignalContext = func(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}

func Run(args []string, stdout, stderr io.Writer) error {
	if err := rejectJSONFlag(args); err != nil {
		emitError(stderr, options{}, err)
		return err
	}
	if len(args) == 0 || (len(args) == 1 && isHelpFlag(args[0])) {
		return printUsage(stdout)
	}
	opts, rest, err := parse(args)
	if err != nil {
		emitError(stderr, opts, err)
		return err
	}
	if len(rest) == 0 {
		err := coded("missing_command", "missing command")
		emitError(stderr, opts, err)
		return err
	}
	if handled, err := handleHelp(rest, stdout); handled {
		if err != nil {
			emitError(stderr, opts, err)
		}
		return err
	}
	if rest[0] == "schemas" || rest[0] == "schema" {
		return writeYAML(stdout, schemaDiscovery())
	}

	cmd := rest[0]
	ctx, cancel := commandContext(cmd, opts.timeout)
	defer cancel()

	client, err := newClient(opts)
	if err != nil {
		emitError(stderr, opts, err)
		return err
	}
	if err := client.Connect(ctx); err != nil {
		if commandCancelledCleanly(cmd, ctx) {
			return nil
		}
		emitError(stderr, opts, err)
		return err
	}
	defer client.Close()
	if !opts.noPresence {
		_ = client.SendPresence()
	}

	if err := runCommand(ctx, client, opts, cmd, rest[1:], stdout); err != nil {
		if commandCancelledCleanly(cmd, ctx) {
			return nil
		}
		emitError(stderr, opts, err)
		return err
	}
	return nil
}

func handleHelp(rest []string, stdout io.Writer) (bool, error) {
	if len(rest) == 0 {
		return true, printUsage(stdout)
	}
	if rest[0] == "help" {
		if len(rest) == 1 {
			return true, printUsage(stdout)
		}
		return true, printCommandHelp(stdout, rest[1])
	}
	if hasHelpFlag(rest[1:]) {
		return true, printCommandHelp(stdout, rest[0])
	}
	return false, nil
}

func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if isHelpFlag(arg) {
			return true
		}
	}
	return false
}

func isHelpFlag(arg string) bool { return arg == "--help" || arg == "-h" }

func commandContext(cmd string, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx := context.Background()
	cancel := func() {}
	if isListenCommand(cmd) {
		ctx, cancel = listenSignalContext(ctx)
	}
	if !isListenCommand(cmd) || timeout > 0 {
		deadlineCtx, deadlineCancel := context.WithTimeout(ctx, timeout)
		return deadlineCtx, func() {
			deadlineCancel()
			cancel()
		}
	}
	return ctx, cancel
}

func commandCancelledCleanly(cmd string, ctx context.Context) bool {
	return isListenCommand(cmd) && ctx.Err() != nil
}

func isListenCommand(cmd string) bool { return cmd == "listen" }

func rejectJSONFlag(args []string) error {
	for _, arg := range args {
		if arg == "--json" || strings.HasPrefix(arg, "--json=") {
			return coded("unsupported_flag", "--json is not supported; xmpp emits YAML output by default")
		}
	}
	return nil
}

func schemaDiscovery() map[string]any {
	ids := []string{"schemas", "error", "connect", "send", "recv", "listen", "ping"}
	return map[string]any{"tool": "xmpp", "schemas": schemaEntries(ids...)}
}

func schemaEntries(ids ...string) []map[string]string {
	schemas := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		schemas = append(schemas, map[string]string{"id": id, "path": "spec/outputs/" + id + ".schema.yaml"})
	}
	return schemas
}

func parse(args []string) (options, []string, error) {
	env := xmpp.ConfigFromEnv()
	opts := options{host: env.Host, port: env.Port, jid: env.JID, password: env.Password, resource: env.Resource, timeout: 15 * time.Second}
	fs := flag.NewFlagSet("xmpp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.host, "host", opts.host, "XMPP server host; default XMPP_HOST or JID domain")
	fs.IntVar(&opts.port, "port", opts.port, "XMPP client port; default 5222 or XMPP_PORT")
	fs.StringVar(&opts.jid, "jid", opts.jid, "JID local@domain[/resource]; default XMPP_JID")
	fs.StringVar(&opts.password, "password", opts.password, "XMPP password; default XMPP_PASS")
	fs.StringVar(&opts.resource, "resource", opts.resource, "resource; default XMPP_RESOURCE or xmpp-go")
	fs.StringVar(&opts.to, "to", "", "recipient JID for send/ping")
	fs.StringVar(&opts.body, "body", "", "message body for send")
	fs.DurationVar(&opts.timeout, "timeout", opts.timeout, "connect/read timeout")
	fs.BoolVar(&opts.plaintxt, "plaintext", false, "disable STARTTLS; only for local/dev servers")
	fs.BoolVar(&opts.noPresence, "no-presence", false, "do not send available presence after login")
	if err := fs.Parse(args); err != nil {
		return opts, nil, err
	}
	return opts, fs.Args(), nil
}

func newClient(opts options) (*xmpp.Client, error) {
	return xmpp.New(xmpp.Config{
		Host:      opts.host,
		Port:      opts.port,
		JID:       opts.jid,
		Password:  opts.password,
		Resource:  opts.resource,
		Plaintext: opts.plaintxt,
		Timeout:   opts.timeout,
	})
}

func runCommand(ctx context.Context, client *xmpp.Client, opts options, cmd string, args []string, out io.Writer) error {
	switch cmd {
	case "connect", "login":
		return writeYAMLResult(out, map[string]any{"connected": true})
	case "send":
		to := opts.to
		body := opts.body
		if len(args) > 0 {
			to = args[0]
		}
		if len(args) > 1 {
			body = args[1]
		}
		if to == "" {
			return errors.New("send requires --to or positional recipient JID")
		}
		if body == "" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			body = string(data)
		}
		if body == "" {
			return errors.New("send requires --body, positional body, or stdin")
		}
		if err := client.SendMessage(to, body); err != nil {
			return err
		}
		return writeYAMLResult(out, map[string]any{"sent": true, "to": to})
	case "recv", "receive":
		msg, err := client.ReceiveMessage(ctx)
		if err != nil {
			return err
		}
		return writeYAMLResult(out, msg)
	case "listen":
		first := true
		return client.StreamMessages(ctx, func(msg *xmpp.Message) error {
			if !first {
				if _, err := io.WriteString(out, "---\n"); err != nil {
					return err
				}
			}
			first = false
			return writeYAML(out, msg)
		})
	case "ping":
		to := opts.to
		if len(args) > 0 {
			to = args[0]
		}
		result, err := client.Ping(ctx, to)
		if err != nil {
			return err
		}
		return writeYAMLResult(out, result)
	default:
		return coded("unknown_command", fmt.Sprintf("unknown command %q", cmd))
	}
}

func writeYAMLResult(out io.Writer, value any) error {
	return writeYAML(out, value)
}

func writeYAML(out io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	data, err = yaml.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}

func emitError(stderr io.Writer, opts options, err error) {
	if err == nil {
		return
	}
	ce := commandError{Code: "command_failed", Message: redact(err.Error(), opts.password)}
	var codedErr commandError
	if errors.As(err, &codedErr) {
		ce = commandError{Code: codedErr.Code, Message: redact(codedErr.Message, opts.password), Hint: redact(codedErr.Hint, opts.password)}
	}
	_ = writeYAML(stderr, errorEnvelope{Error: ce})
}

func coded(code, message string) error { return commandError{Code: code, Message: message} }

func codedWithHint(code, message, hint string) error {
	return commandError{Code: code, Message: message, Hint: hint}
}

func redact(message, secret string) string {
	if secret == "" {
		return message
	}
	return strings.ReplaceAll(message, secret, "[REDACTED]")
}

func renderMessage(msg *xmpp.Message) string {
	return fmt.Sprintf("from=%s to=%s type=%s body=%s\n", emptyDash(msg.From), emptyDash(msg.To), emptyDash(msg.Type), msg.Body)
}

func emptyDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

func printUsage(out io.Writer) error {
	_, err := io.WriteString(out, rootHelp)
	return err
}

func printCommandHelp(out io.Writer, cmd string) error {
	text, ok := commandHelp[cmd]
	if !ok {
		return codedWithHint("unknown_command", fmt.Sprintf("unknown command %q", cmd), "Run \"xmpp help\" to list available commands.")
	}
	_, err := io.WriteString(out, text)
	return err
}

const rootHelp = `xmpp — minimal XMPP client optimized for agents and terminal discovery

USAGE
  xmpp [global flags] <command> [command args]
  xmpp help [command]
  xmpp <command> --help

COMMANDS
  connect  Connect, authenticate, bind a resource, then exit.
  send     Send a chat message.
  recv     Wait for one message with a non-empty body.
  listen   Stream messages with non-empty bodies until timeout or signal.
  ping     Send an XEP-0199 ping.
  schemas  List structured output schema IDs and file paths.

FLAGS
  --host HOST       XMPP server host (default: XMPP_HOST or JID domain)
  --port N          XMPP client port (default: XMPP_PORT or 5222)
  --jid JID         account JID local@domain[/resource] (default: XMPP_JID)
  --password PASS   account password (default: XMPP_PASS)
  --resource NAME   resource to bind (default: XMPP_RESOURCE or xmpp-go)
  --to JID          recipient for send or ping (default: none)
  --body TEXT       message body for send; stdin fallback when omitted (default: stdin)
  --timeout D       connect/read timeout as a Go duration (default: 15s)
  --plaintext       disable STARTTLS for local/dev servers (default: false)
  --no-presence     do not send available presence after login (default: false)

OUTPUT
  stdout: commands emit YAML documents matching their schema ID; listen emits a YAML stream.
  stderr: failures emit the standard YAML error envelope with code and message.
  discovery: run "xmpp schemas" for structured schema IDs and file paths.

EXAMPLES
  xmpp --jid alice@example.test --password "$XMPP_PASS" --to bob@example.test --body "hello" send
  xmpp --jid alice@example.test --password "$XMPP_PASS" --timeout 0 listen

SEE ALSO
  xmpp schemas
`

var commandHelp = map[string]string{
	"connect": `USAGE
  xmpp [connection flags] connect

FLAGS
  --host HOST       XMPP server host (default: XMPP_HOST or JID domain)
  --port N          XMPP client port (default: XMPP_PORT or 5222)
  --jid JID         account JID local@domain[/resource] (default: XMPP_JID)
  --password PASS   account password (default: XMPP_PASS)
  --resource NAME   resource to bind (default: XMPP_RESOURCE or xmpp-go)
  --timeout D       connect/read timeout as a Go duration (default: 15s)
  --plaintext       disable STARTTLS for local/dev servers (default: false)
  --no-presence     do not send available presence after login (default: false)

OUTPUT
  stdout: YAML connect result document with connected: true; see "xmpp schemas" → connect.
  stderr: standard YAML error envelope with code and message.

EXAMPLES
  xmpp --jid alice@example.test --password "$XMPP_PASS" connect
`,
	"send": `USAGE
  xmpp [connection flags] [--to JID] [--body TEXT] send [recipient] [body]

FLAGS
  --host HOST       XMPP server host (default: XMPP_HOST or JID domain)
  --port N          XMPP client port (default: XMPP_PORT or 5222)
  --jid JID         account JID local@domain[/resource] (default: XMPP_JID)
  --password PASS   account password (default: XMPP_PASS)
  --resource NAME   resource to bind (default: XMPP_RESOURCE or xmpp-go)
  --to JID          recipient JID; may be replaced by first positional arg (default: none)
  --body TEXT       message body; stdin fallback when omitted (default: stdin)
  --timeout D       connect/read timeout as a Go duration (default: 15s)
  --plaintext       disable STARTTLS for local/dev servers (default: false)
  --no-presence     do not send available presence after login (default: false)

OUTPUT
  stdout: YAML send result document with sent and to fields; see "xmpp schemas" → send.
  stderr: standard YAML error envelope with code and message.

EXAMPLES
  xmpp --jid alice@example.test --password "$XMPP_PASS" --to bob@example.test --body "hello" send
`,
	"recv": `USAGE
  xmpp [connection flags] recv

FLAGS
  --host HOST       XMPP server host (default: XMPP_HOST or JID domain)
  --port N          XMPP client port (default: XMPP_PORT or 5222)
  --jid JID         account JID local@domain[/resource] (default: XMPP_JID)
  --password PASS   account password (default: XMPP_PASS)
  --resource NAME   resource to bind (default: XMPP_RESOURCE or xmpp-go)
  --timeout D       connect/read timeout as a Go duration (default: 15s)
  --plaintext       disable STARTTLS for local/dev servers (default: false)
  --no-presence     do not send available presence after login (default: false)

OUTPUT
  stdout: YAML message document for one received body; see "xmpp schemas" → recv.
  stderr: standard YAML error envelope with code and message.

EXAMPLES
  xmpp --jid alice@example.test --password "$XMPP_PASS" --timeout 30s recv
`,
	"listen": `USAGE
  xmpp [connection flags] listen

FLAGS
  --host HOST       XMPP server host (default: XMPP_HOST or JID domain)
  --port N          XMPP client port (default: XMPP_PORT or 5222)
  --jid JID         account JID local@domain[/resource] (default: XMPP_JID)
  --password PASS   account password (default: XMPP_PASS)
  --resource NAME   resource to bind (default: XMPP_RESOURCE or xmpp-go)
  --timeout D       listen deadline as a Go duration; --timeout 0 listens until SIGINT/SIGTERM (default: 15s)
  --plaintext       disable STARTTLS for local/dev servers (default: false)
  --no-presence     do not send available presence after login (default: false)

OUTPUT
  stdout: YAML stream of message documents, one per non-empty body; see "xmpp schemas" → listen.
  stderr: standard YAML error envelope with code and message.

EXAMPLES
  xmpp --jid alice@example.test --password "$XMPP_PASS" --timeout 0 listen
`,
	"ping": `USAGE
  xmpp [connection flags] [--to JID] ping [target]

FLAGS
  --host HOST       XMPP server host (default: XMPP_HOST or JID domain)
  --port N          XMPP client port (default: XMPP_PORT or 5222)
  --jid JID         account JID local@domain[/resource] (default: XMPP_JID)
  --password PASS   account password (default: XMPP_PASS)
  --resource NAME   resource to bind (default: XMPP_RESOURCE or xmpp-go)
  --to JID          ping target; defaults to the account domain when omitted (default: none)
  --timeout D       connect/read timeout as a Go duration (default: 15s)
  --plaintext       disable STARTTLS for local/dev servers (default: false)
  --no-presence     do not send available presence after login (default: false)

OUTPUT
  stdout: YAML ping result document with id and ok fields; see "xmpp schemas" → ping.
  stderr: standard YAML error envelope with code and message.

EXAMPLES
  xmpp --jid alice@example.test --password "$XMPP_PASS" --to example.test ping
`,
	"schemas": `USAGE
  xmpp schemas

FLAGS
  (none)

OUTPUT
  stdout: YAML schema discovery document with tool and schemas fields; see "xmpp schemas" → schemas.
  stderr: standard YAML error envelope with code and message.

EXAMPLES
  xmpp schemas
`,
}
