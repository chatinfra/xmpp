package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

const xmppdHelp = `xmppd - bridge XMPP messages to an opencode agent

Usage:
  xmppd
  xmppd --help

Required environment:
  XMPP_JID                 XMPP account JID for the bridge
  XMPP_PASS                XMPP account password for the bridge
  OPENCODE_BASE_URL        opencode API base URL (or OPENCODE_PORT)
  OPENCODE_DIRECTORY       opencode working directory
  OPENCODE_AGENT           opencode agent ID (or AGENT_ID)
  XMPPD_STATE_DIR          directory for sessions.json and status.json

Optional environment:
  XMPP_PLAINTEXT           allow plaintext XMPP connections when true
  OPENCODE_PROMPT_TIMEOUT  prompt timeout as a Go duration
`

func main() {
	if wantsHelp(os.Args[1:]) {
		fmt.Fprint(os.Stdout, xmppdHelp)
		return
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
