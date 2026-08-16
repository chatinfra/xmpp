package main

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/chatinfra/xmpp/internal/xmpp"
)

const (
	peerAgentID  = "55555555-5555-4555-8555-555555555555"
	peerNickname = "agent-55555555555545558555555555555555"
)

func TestRoomAndDirectExchangeUsesExplicitAddressingAndOneHop(t *testing.T) {
	stateDir := testStateDir(t)
	cfg := testConfig(stateDir)
	cfg.RoomEnabled = true
	snapshot := roomAdmissionSnapshot(cfg)
	mustWriteAdmissionSnapshot(stateDir, snapshot)
	correlationRoom := "66666666-6666-4666-8666-666666666666"
	correlationDirect := "77777777-7777-4777-8777-777777777777"
	events := []xmpp.Event{
		{Presence: &xmpp.OccupantPresence{RoomJID: cfg.RoomJID, Nickname: cfg.RoomNickname, RealJID: cfg.XMPP.JID, Affiliation: "member", Available: true, Self: true}},
		{Presence: &xmpp.OccupantPresence{RoomJID: cfg.RoomJID, Nickname: "human", RealJID: "bob@example.com/phone", Affiliation: "member", Available: true}},
		{Presence: &xmpp.OccupantPresence{RoomJID: cfg.RoomJID, Nickname: peerNickname, RealJID: "peer@example.com/mobile", Affiliation: "member", Available: true}},
		{Message: &xmpp.Message{From: cfg.RoomJID + "/human", Type: xmpp.GroupchatMessageType, Body: cfg.RoomNickname + ": human room"}},
		{Message: &xmpp.Message{From: cfg.RoomJID + "/" + peerNickname, Type: xmpp.GroupchatMessageType, Body: cfg.RoomNickname + ": peer room", AgentMessage: &xmpp.AgentMessageMetadata{Correlation: correlationRoom, OriginAgentID: peerAgentID, Hop: 0}}},
		{Message: &xmpp.Message{From: "peer@example.com/mobile", Type: xmpp.DirectChatMessageType, Body: "peer direct", AgentMessage: &xmpp.AgentMessageMetadata{Correlation: correlationDirect, OriginAgentID: peerAgentID, Hop: 0}}},
		{Message: &xmpp.Message{From: "peer@example.com/mobile", Type: xmpp.DirectChatMessageType, Body: "hop one must stop", AgentMessage: &xmpp.AgentMessageMetadata{Correlation: correlationDirect, OriginAgentID: peerAgentID, Hop: 1}}},
		{Message: &xmpp.Message{From: cfg.RoomJID + "/human", Type: xmpp.GroupchatMessageType, Body: "unaddressed"}},
		{Message: &xmpp.Message{From: cfg.RoomJID + "/human", Type: xmpp.GroupchatMessageType, Body: cfg.RoomNickname + ": delayed", Delay: &xmpp.Delay{Namespace: "urn:xmpp:delay", Stamp: time.Now().UTC()}}},
		{Message: &xmpp.Message{From: cfg.RoomJID + "/" + cfg.RoomNickname, Type: xmpp.GroupchatMessageType, Body: cfg.RoomNickname + ": echo"}},
		{Message: &xmpp.Message{From: "ownerless@example.com/service", Type: xmpp.DirectChatMessageType, Body: "ownerless"}},
		{Message: &xmpp.Message{From: "foreign@other.example/phone", Type: xmpp.DirectChatMessageType, Body: "foreign"}},
	}
	x := &fakeXMPP{events: events}
	oc := newFakeOpencode("room-session", "direct-session")
	bridge := NewBridgeWithClients(cfg, nil, x, oc)
	bridge.authority = NewAdmissionAuthority(cfg, fakeAdmissionChecker{roomAllowed: true})
	if err := bridge.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(x.joins) != 1 || x.joins[0] != cfg.RoomJID+"/"+cfg.RoomNickname {
		t.Fatalf("room joins=%v", x.joins)
	}
	if len(x.sent) != 3 {
		t.Fatalf("sent=%+v prompts=%+v; rejected traffic should not reply", x.sent, oc.prompts)
	}
	var humanRoom, peerRoom, peerDirect *sentMessage
	for index := range x.sent {
		sent := &x.sent[index]
		switch {
		case sent.messageType == xmpp.GroupchatMessageType && sent.metadata.Hop == 0:
			humanRoom = sent
		case sent.messageType == xmpp.GroupchatMessageType && sent.metadata.Hop == 1:
			peerRoom = sent
		case sent.messageType == xmpp.DirectChatMessageType:
			peerDirect = sent
		}
	}
	if humanRoom == nil || humanRoom.metadata.OriginAgentID != cfg.AgentID || humanRoom.to != cfg.RoomJID {
		t.Fatalf("human room response=%+v", humanRoom)
	}
	if peerRoom == nil || peerRoom.metadata.Correlation != correlationRoom || peerRoom.metadata.OriginAgentID != peerAgentID || peerRoom.body != peerNickname+": reply:peer room" {
		t.Fatalf("peer room response=%+v", peerRoom)
	}
	if peerDirect == nil || peerDirect.metadata.Correlation != correlationDirect || peerDirect.metadata.Hop != 1 || peerDirect.to != "peer@example.com/mobile" {
		t.Fatalf("peer direct response=%+v", peerDirect)
	}
	if oc.createCount() != 2 {
		t.Fatalf("room/direct sessions were not separate: creates=%d prompts=%v", oc.createCount(), oc.promptSessions())
	}
}

func TestDuplicateCorrelationAndOwnRootOriginAreRejected(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	cfg.RoomEnabled = true
	snapshot := roomAdmissionSnapshot(cfg)
	mustWriteAdmissionSnapshot(cfg.StateDir, snapshot)
	correlation := "88888888-8888-4888-8888-888888888888"
	valid := xmpp.AgentMessageMetadata{Correlation: correlation, OriginAgentID: peerAgentID, Hop: 0}
	ownRoot := xmpp.AgentMessageMetadata{Correlation: "99999999-9999-4999-8999-999999999999", OriginAgentID: cfg.AgentID, Hop: 0}
	spoofedRoot := xmpp.AgentMessageMetadata{Correlation: "aaaaaaaa-9999-4999-8999-999999999999", OriginAgentID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", Hop: 0}
	x := &fakeXMPP{events: []xmpp.Event{
		{Presence: &xmpp.OccupantPresence{RoomJID: cfg.RoomJID, Nickname: cfg.RoomNickname, RealJID: cfg.XMPP.JID, Affiliation: "member", Available: true, Self: true}},
		{Presence: &xmpp.OccupantPresence{RoomJID: cfg.RoomJID, Nickname: peerNickname, RealJID: "peer@example.com/mobile", Affiliation: "member", Available: true}},
		{Message: &xmpp.Message{From: "peer@example.com/mobile", Type: xmpp.DirectChatMessageType, Body: "first", AgentMessage: &valid}},
		{Message: &xmpp.Message{From: "peer@example.com/laptop", Type: xmpp.DirectChatMessageType, Body: "duplicate", AgentMessage: &valid}},
		{Message: &xmpp.Message{From: "peer@example.com/tablet", Type: xmpp.DirectChatMessageType, Body: "own root", AgentMessage: &ownRoot}},
		{Message: &xmpp.Message{From: "peer@example.com/tablet", Type: xmpp.DirectChatMessageType, Body: "spoofed root", AgentMessage: &spoofedRoot}},
	}}
	bridge := NewBridgeWithClients(cfg, nil, x, newFakeOpencode("session"))
	bridge.authority = NewAdmissionAuthority(cfg, fakeAdmissionChecker{roomAllowed: true})
	if err := bridge.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(x.sent) != 1 || x.sent[0].body != "reply:first" || x.sent[0].metadata.Hop != 1 {
		t.Fatalf("sent=%+v", x.sent)
	}
}

func TestPeersRemainHiddenUntilCurrentSelfPresenceAndDoNotSurviveRejoin(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	cfg.RoomEnabled = true
	snapshot := roomAdmissionSnapshot(cfg)
	mustWriteAdmissionSnapshot(cfg.StateDir, snapshot)
	bridge := NewBridgeWithClients(cfg, nil, &fakeXMPP{}, newFakeOpencode())
	bridge.authority = NewAdmissionAuthority(cfg, fakeAdmissionChecker{roomAllowed: true})
	lease, err := bridge.refreshAdmission(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.requestRoomJoin(); err != nil {
		t.Fatal(err)
	}

	peerPresence := xmpp.OccupantPresence{
		RoomJID: cfg.RoomJID, Nickname: peerNickname, RealJID: "peer@example.com/mobile", Affiliation: "member", Available: true,
	}
	bridge.handlePresence(peerPresence)
	if bridge.isCurrentPeer("peer@example.com") || bridge.status.PeerCount != 0 {
		t.Fatalf("peer exposed before self-presence: status=%+v peers=%+v", bridge.status, bridge.peers)
	}

	bridge.handlePresence(xmpp.OccupantPresence{
		RoomJID: cfg.RoomJID, Nickname: cfg.RoomNickname, RealJID: cfg.XMPP.JID, Affiliation: "member", Available: true, Self: true,
	})
	if !bridge.isCurrentPeer("peer@example.com") || bridge.status.RoomState != "joined" {
		t.Fatalf("peer not exposed after self-presence: status=%+v peers=%+v", bridge.status, bridge.peers)
	}

	closed := lease
	closed.RoomAllowed = false
	bridge.applyLease(closed)
	bridge.applyLease(lease)
	if bridge.isCurrentPeer("peer@example.com") || len(bridge.occupants) != 0 || bridge.selfPresent {
		t.Fatalf("stale presence survived gate closure/reopen: occupants=%+v peers=%+v", bridge.occupants, bridge.peers)
	}
}

func TestNonConflictRoomJoinErrorIsFailedAndNotRetried(t *testing.T) {
	stateDir := testStateDir(t)
	cfg := testConfig(stateDir)
	cfg.RoomEnabled = true
	mustWriteAdmissionSnapshot(stateDir, roomAdmissionSnapshot(cfg))
	x := &fakeXMPP{events: []xmpp.Event{{Presence: &xmpp.OccupantPresence{
		RoomJID: cfg.RoomJID, Nickname: cfg.RoomNickname, Type: "error", ErrorCondition: "forbidden", Available: false,
	}}}}
	bridge := NewBridgeWithClients(cfg, nil, x, newFakeOpencode())
	bridge.authority = NewAdmissionAuthority(cfg, fakeAdmissionChecker{roomAllowed: true})
	if err := bridge.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(x.joins) != 1 {
		t.Fatalf("non-retryable join failure retried: joins=%v", x.joins)
	}
	if err := bridge.requestRoomJoin(); err != nil || len(x.joins) != 1 {
		t.Fatalf("non-retryable join failure was immediately retried: err=%v joins=%v", err, x.joins)
	}
	var status StatusFile
	readJSON(t, filepath.Join(stateDir, "status.json"), &status)
	if status.RoomState != "failed" || status.LastErrorCode == nil || *status.LastErrorCode != "room_join_failed" {
		t.Fatalf("status=%+v", status)
	}
}

func TestNicknameSquattingFailsWithoutAlternateNickname(t *testing.T) {
	stateDir := testStateDir(t)
	cfg := testConfig(stateDir)
	cfg.RoomEnabled = true
	mustWriteAdmissionSnapshot(stateDir, roomAdmissionSnapshot(cfg))
	x := &fakeXMPP{events: []xmpp.Event{{Presence: &xmpp.OccupantPresence{
		RoomJID: cfg.RoomJID, Nickname: cfg.RoomNickname, RealJID: "squatter@example.com/phone", Affiliation: "member", Available: true,
	}}}}
	bridge := NewBridgeWithClients(cfg, nil, x, newFakeOpencode())
	bridge.authority = NewAdmissionAuthority(cfg, fakeAdmissionChecker{roomAllowed: true})
	if err := bridge.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(x.joins) != 1 {
		t.Fatalf("daemon tried alternate nickname: joins=%v", x.joins)
	}
	var status StatusFile
	readJSON(t, filepath.Join(stateDir, "status.json"), &status)
	if status.RoomState != "failed" || status.LastErrorCode == nil || *status.LastErrorCode != "nickname_conflict" || status.PeerCount != 0 {
		t.Fatalf("status=%+v", status)
	}
}

func TestUnavailablePresenceAndDisconnectClearPeers(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	cfg.RoomEnabled = true
	status, err := newStatusFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bridge := &Bridge{cfg: cfg, store: NewStateStore(cfg.StateDir), status: status, peers: map[string]Peer{"peer@example.com": {AgentID: peerAgentID, JID: "peer@example.com", Nickname: peerNickname}}, occupants: map[string]roomOccupant{}, workers: map[string]*sessionWorker{}, seen: map[string]struct{}{}, logger: testLogger()}
	bridge.status.RoomState = "joined"
	bridge.status.PeerCount = 1
	bridge.setConnected(false)
	if bridge.status.RoomState != "pending" || bridge.status.PeerCount != 0 || bridge.status.PeersUpdatedAt == nil {
		t.Fatalf("disconnect did not reset room state: %+v", bridge.status)
	}
}

func roomAdmissionSnapshot(cfg Config) AdmissionSnapshot {
	snapshot := testAdmissionSnapshot(cfg, true)
	snapshot.Users = []string{"bob@example.com"}
	snapshot.Agents = []AdmissionAgent{
		{AgentID: cfg.AgentID, BareJID: normalizeBareJID(cfg.XMPP.JID), Nickname: cfg.RoomNickname},
		{AgentID: peerAgentID, BareJID: "peer@example.com", Nickname: peerNickname},
	}
	sort.Slice(snapshot.Agents, func(i, j int) bool { return snapshot.Agents[i].BareJID < snapshot.Agents[j].BareJID })
	return snapshot
}

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }
