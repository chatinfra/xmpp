package main

import (
	"strings"
	"testing"
)

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

func TestHelpDocumentsRequiredEnvironment(t *testing.T) {
	for _, token := range []string{
		"Usage:",
		"XMPP_JID",
		"XMPP_PASS",
		"OPENCODE_BASE_URL",
		"OPENCODE_DIRECTORY",
		"OPENCODE_AGENT",
		"XMPPD_STATE_DIR",
	} {
		if !strings.Contains(xmppdHelp, token) {
			t.Fatalf("help output missing %q:\n%s", token, xmppdHelp)
		}
	}
}
