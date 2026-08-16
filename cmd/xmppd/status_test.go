package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestStatusSchemaVersion2SerializesExactFields(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	originalSource := buildSourceDigest
	buildSourceDigest = strings.Repeat("c", 64)
	t.Cleanup(func() { buildSourceDigest = originalSource })
	status, err := newStatusFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if status.SchemaVersion != 2 || status.BuildSourceDigest != strings.Repeat("c", 64) || status.RoomJID != cfg.RoomJID || status.RoomNickname != cfg.RoomNickname {
		t.Fatalf("status=%+v", status)
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"schemaVersion", "processInvocationId", "buildSourceDigest", "buildBinaryDigest", "xmppConnected",
		"roomState", "roomJid", "roomNickname", "peerCount", "peersUpdatedAt", "admissionGeneration",
		"gateGeneration", "gateEvidenceDigest", "admissionExpiresAt", "lastInboundAt", "lastReplyAt",
		"lastErrorCode", "lastError", "activeSessionCount", "startedAt", "updatedAt",
	}
	if len(fields) != len(want) {
		t.Fatalf("status fields=%v", fields)
	}
	for _, key := range want {
		if _, exists := fields[key]; !exists {
			t.Fatalf("status missing %s: %s", key, data)
		}
	}
	for _, key := range []string{"peersUpdatedAt", "admissionGeneration", "gateGeneration", "gateEvidenceDigest", "admissionExpiresAt", "lastInboundAt", "lastReplyAt", "lastErrorCode", "lastError"} {
		if fields[key] != nil {
			t.Fatalf("initial %s=%v; want null", key, fields[key])
		}
	}
	if err := status.Validate(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if statusHeartbeatInterval > 30*time.Second || statusStaleAfter != 90*time.Second {
		t.Fatalf("heartbeat=%s stale=%s", statusHeartbeatInterval, statusStaleAfter)
	}
}

func TestStatusErrorBoundIsUTF8AndHasNoSuffix(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	status, err := newStatusFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bridge := &Bridge{cfg: cfg, status: status, store: NewStateStore(cfg.StateDir), logger: testLogger()}
	bridge.recordError("opencode_failed", strings.Repeat("界", 300))
	if bridge.status.LastError == nil || bridge.status.LastErrorCode == nil {
		t.Fatalf("status=%+v", bridge.status)
	}
	message := *bridge.status.LastError
	if len([]byte(message)) > 512 || !utf8.ValidString(message) || strings.HasSuffix(message, "...") {
		t.Fatalf("bounded status error bytes=%d valid=%t value=%q", len([]byte(message)), utf8.ValidString(message), message)
	}
	if err := bridge.status.Validate(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestProcessStartResetsEphemeralStatusAndChangesInvocation(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	t.Setenv("INVOCATION_ID", "first-invocation")
	first, err := newStatusFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first.XMPPConnected = true
	first.PeerCount = 2
	first.PeersUpdatedAt = &now
	first.LastInboundAt = &now
	first.LastReplyAt = &now
	first.LastErrorCode = stringPointer("xmpp_disconnected")
	first.LastError = stringPointer("old")
	if err := NewStateStore(cfg.StateDir).SaveStatus(first); err != nil {
		t.Fatal(err)
	}

	t.Setenv("INVOCATION_ID", "second-invocation")
	second, err := newStatusFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if second.ProcessInvocationID != "second-invocation" || second.ProcessInvocationID == first.ProcessInvocationID || second.XMPPConnected || second.PeerCount != 0 || second.PeersUpdatedAt != nil || second.LastInboundAt != nil || second.LastReplyAt != nil || second.LastErrorCode != nil || second.LastError != nil {
		t.Fatalf("new process retained ephemeral status: %+v", second)
	}
}

func TestStatusFreshnessAndBinaryDigest(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	status, err := newStatusFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	status.UpdatedAt = time.Now().UTC().Add(-statusStaleAfter - time.Nanosecond)
	if err := status.Validate(time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale status err=%v", err)
	}
	digest, err := computeBinaryDigest()
	if err != nil || !isSHA256(digest) {
		t.Fatalf("binary digest=%q err=%v", digest, err)
	}
}

func TestStatusFileModeIsRuntimeUserLocal(t *testing.T) {
	stateDir := testStateDir(t)
	cfg := testConfig(stateDir)
	status, err := newStatusFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewStateStore(stateDir).SaveStatus(status); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(stateDir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("status mode=%o", info.Mode().Perm())
	}
}

func TestConcurrentStatusPublicationCannotRegressFinalState(t *testing.T) {
	stateDir := testStateDir(t)
	cfg := testConfig(stateDir)
	status, err := newStatusFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bridge := &Bridge{cfg: cfg, status: status, store: NewStateStore(stateDir), logger: testLogger()}
	const writes = 64
	var workers sync.WaitGroup
	for index := range writes {
		workers.Add(1)
		go func() {
			defer workers.Done()
			bridge.mu.Lock()
			bridge.status.ActiveSessionCount = index
			bridge.mu.Unlock()
			bridge.flushStatus()
		}()
	}
	workers.Wait()
	bridge.mu.Lock()
	want := bridge.status.ActiveSessionCount
	bridge.mu.Unlock()
	var published StatusFile
	readJSON(t, filepath.Join(stateDir, "status.json"), &published)
	if published.ActiveSessionCount != want {
		t.Fatalf("published status regressed: got=%d want=%d", published.ActiveSessionCount, want)
	}
}
