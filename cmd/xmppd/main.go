package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const xmppdHelp = `xmppd — bridge XMPP messages to an opencode agent

USAGE
  xmppd
  xmppd ctl <list|send> ...
  xmppd --help

COMMANDS
  ctl  List current peers or send through the runtime-user-local control socket.

ENVIRONMENT
  XMPP_JID                 XMPP account JID for the bridge
  XMPP_PASS                XMPP account password for the bridge
  OPENCODE_BASE_URL        opencode API base URL (or OPENCODE_PORT)
  OPENCODE_DIRECTORY       opencode working directory
  OPENCODE_AGENT_ID        immutable agent lifecycle UUID (or AGENT_ID)
  OPENCODE_AGENT_NAME      mutable opencode agent selector (or OPENCODE_AGENT)
  XMPP_ACCOUNT_STATUS      must be ACTIVE before daemon startup
  XMPP_TENANT_ID           immutable tenant UUID
  XMPP_MUC_HOST            bound Ejabberd mucHost
  XMPP_ROOM_JID            exact UUID-derived tenant room JID
  XMPP_ROOM_NICKNAME       exact UUID-derived agent nickname
  XMPPD_ROOM_ENABLED       request room mode only when true
  XMPPD_ADMISSION_PATH     admission snapshot (default: state-dir/admission.json)
  CHATINFRA_INTERNAL_API_BASE_URL  authenticated admission-check API
  CHATINFRA_API_TOKEN      protected instance-scoped admission token
  XMPPD_STATE_DIR          directory for state and control.sock
  XMPP_PLAINTEXT           allow plaintext XMPP connections when true
  OPENCODE_PROMPT_TIMEOUT  prompt timeout as a Go duration

OUTPUT
  stdout: reserved for help text only.
  stderr: runtime logs use the standard log-line format with the "xmppd:" prefix.
  ctl: compact terminal text on stdout and diagnostics on stderr; --json is unsupported.

EXAMPLES
  xmppd
  xmppd ctl list --state-dir "$XMPPD_STATE_DIR"
`

func main() {
	args := os.Args[1:]
	if wantsHelp(args) {
		fmt.Fprint(os.Stdout, xmppdHelp)
		return
	}
	if len(args) == 2 && args[0] == "help" && args[1] == "ctl" {
		_, _ = io.WriteString(os.Stdout, xmppdCtlHelp)
		return
	}
	if len(args) > 0 && args[0] == "ctl" {
		if err := runCtl(args[1:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "xmppd ctl: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := validateDaemonArgs(args); err != nil {
		fmt.Fprintf(os.Stderr, "xmppd: %v\n", err)
		os.Exit(1)
	}
	logger := log.New(os.Stderr, "xmppd: ", log.LstdFlags|log.LUTC)
	cfg, err := ConfigFromEnv()
	if err != nil {
		logger.Printf("configuration error: %v", err)
		os.Exit(1)
	}
	bridge, err := NewBridge(cfg, logger)
	if err != nil {
		logger.Printf("initialization error: %v", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := bridge.Run(ctx); err != nil {
		logger.Printf("fatal error: %v", err)
		os.Exit(1)
	}
}

func validateDaemonArgs(args []string) error {
	if len(args) == 0 {
		return nil
	}
	for _, arg := range args {
		if arg == "--json" || strings.HasPrefix(arg, "--json=") {
			return errors.New("--json is not supported")
		}
	}
	return fmt.Errorf("unknown argument %q", args[0])
}

func wantsHelp(args []string) bool {
	if len(args) != 1 {
		return false
	}
	switch args[0] {
	case "--help", "-h", "help":
		return true
	default:
		return false
	}
}
