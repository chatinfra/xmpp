package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
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
}

func (e commandError) Error() string { return e.Message }

type errorEnvelope struct {
	Error commandError `json:"error"`
}

func Run(args []string, stdout, stderr io.Writer) error {
	if err := rejectJSONFlag(args); err != nil {
		emitError(stderr, options{}, err)
		return err
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
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
	if rest[0] == "schemas" || rest[0] == "schema" {
		return writeYAML(stdout, schemaDiscovery())
	}

	cmd := rest[0]
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	client, err := newClient(opts)
	if err != nil {
		emitError(stderr, opts, err)
		return err
	}
	if err := client.Connect(ctx); err != nil {
		emitError(stderr, opts, err)
		return err
	}
	defer client.Close()
	if !opts.noPresence {
		_ = client.SendPresence()
	}

	if err := runCommand(ctx, client, opts, cmd, rest[1:], stdout); err != nil {
		emitError(stderr, opts, err)
		return err
	}
	return nil
}

func rejectJSONFlag(args []string) error {
	for _, arg := range args {
		if arg == "--json" || strings.HasPrefix(arg, "--json=") {
			return coded("unsupported_flag", "--json is not supported; xmpp emits YAML output by default")
		}
	}
	return nil
}

func schemaDiscovery() map[string]any {
	ids := []string{"help", "schemas", "error", "connect", "send", "recv", "ping"}
	schemas := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		schemas = append(schemas, map[string]string{"id": id, "path": "spec/outputs/" + id + ".schema.yaml"})
	}
	return map[string]any{"tool": "xmpp", "schemas": schemas}
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
		return write(out, false, map[string]any{"connected": true}, "connected and authenticated\n")
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
		return write(out, false, map[string]any{"sent": true, "to": to}, fmt.Sprintf("sent message to %s\n", to))
	case "recv", "receive":
		msg, err := client.ReceiveMessage(ctx)
		if err != nil {
			return err
		}
		return write(out, false, msg, renderMessage(msg))
	case "ping":
		to := opts.to
		if len(args) > 0 {
			to = args[0]
		}
		result, err := client.Ping(ctx, to)
		if err != nil {
			return err
		}
		return write(out, false, result, fmt.Sprintf("ping id=%s ok=%t\n", result.ID, result.OK))
	default:
		return coded("unknown_command", fmt.Sprintf("unknown command %q", cmd))
	}
}

func write(out io.Writer, asJSON bool, value any, human string) error {
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
		ce = commandError{Code: codedErr.Code, Message: redact(codedErr.Message, opts.password)}
	}
	_ = writeYAML(stderr, errorEnvelope{Error: ce})
}

func coded(code, message string) error { return commandError{Code: code, Message: message} }

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
	return writeYAML(out, map[string]any{
		"tool":    "xmpp",
		"summary": "Minimal XMPP client optimized for agents and terminal discovery",
		"usage":   "xmpp [global flags] <command> [command args]",
		"flags": []map[string]string{
			{"name": "--host HOST", "summary": "XMPP server host; default XMPP_HOST or JID domain"},
			{"name": "--port N", "summary": "XMPP client port; default 5222 or XMPP_PORT"},
			{"name": "--jid JID", "summary": "account JID local@domain[/resource]; default XMPP_JID"},
			{"name": "--password PASS", "summary": "account password; default XMPP_PASS"},
			{"name": "--resource NAME", "summary": "resource to bind"},
			{"name": "--to JID", "summary": "recipient for send or ping"},
			{"name": "--body TEXT", "summary": "message body for send; stdin fallback"},
			{"name": "--timeout D", "summary": "connect/read timeout"},
			{"name": "--plaintext", "summary": "disable STARTTLS for local/dev servers"},
			{"name": "--no-presence", "summary": "do not send available presence after login"},
		},
		"commands": []map[string]string{
			{"name": "connect", "summary": "connect, authenticate, bind resource, then exit", "schema": "connect"},
			{"name": "send", "summary": "send a chat message", "schema": "send"},
			{"name": "recv", "summary": "wait for one message with a non-empty body", "schema": "recv"},
			{"name": "ping", "summary": "send XEP-0199 ping", "schema": "ping"},
			{"name": "schemas", "summary": "list output schemas", "schema": "schemas"},
		},
		"schemas": []string{"help", "schemas", "error", "connect", "send", "recv", "ping"},
	})
}
