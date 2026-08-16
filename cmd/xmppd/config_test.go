package main

import (
	"context"
	"strings"
	"testing"
)

func TestConfigRequiresActiveAccountBeforeStartup(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	cfg.AccountStatus = "PENDING"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must be ACTIVE") {
		t.Fatalf("pending account validation err=%v", err)
	}
	x := &fakeXMPP{}
	bridge := NewBridgeWithClients(cfg, nil, x, newFakeOpencode())
	if err := bridge.Run(context.Background()); err == nil || x.connected {
		t.Fatalf("pending account started bridge: err=%v connected=%t", err, x.connected)
	}
	cfg.AccountStatus = "ACTIVE"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestUUIDRoomNicknameAndBoundMUCHostAreExact(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	if got := expectedRoomJID(cfg.TenantID, "Conference.Example.Com."); got != cfg.RoomJID {
		t.Fatalf("room JID=%q want %q", got, cfg.RoomJID)
	}
	if got := expectedRoomNickname(cfg.AgentID); got != cfg.RoomNickname {
		t.Fatalf("nickname=%q want %q", got, cfg.RoomNickname)
	}
	cfg.RoomJID = "agents-aaaaaaaaaaaa4aaa8aaaaaaaaaaaaaaa@example.com"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "bound mucHost") {
		t.Fatalf("wrong-host room validation err=%v", err)
	}
}

func TestImmutableLifecycleIDAndMutableSelectorCannotCollapse(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	cfg.AgentName = "renamed-agent"
	bridge := NewBridgeWithClients(cfg, nil, &fakeXMPP{}, newFakeOpencode())
	if bridge.cfg.AgentID != "11111111-1111-4111-8111-111111111111" || bridge.cfg.AgentName != "renamed-agent" || bridge.cfg.AgentID == bridge.cfg.AgentName {
		t.Fatalf("bridge identity fields=%+v", bridge.cfg)
	}
	if bridge.status.RoomNickname != expectedRoomNickname(bridge.cfg.AgentID) {
		t.Fatalf("nickname derived from mutable selector: %+v", bridge.status)
	}
}
