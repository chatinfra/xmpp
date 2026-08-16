package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chatinfra/xmpp/internal/xmpp"
)

type recordingAdmissionChecker struct {
	checks      []AdmissionCheck
	roomAllowed bool
	mismatch    bool
	expiresAt   time.Time
}

type fetchingAdmissionChecker struct {
	snapshot AdmissionSnapshot
	checker  *recordingAdmissionChecker
}

type delayedAdmissionChecker struct {
	delay  time.Duration
	result AdmissionCheckResult
}

type blockingAdmissionChecker struct {
	started   chan struct{}
	release   chan struct{}
	expiresAt time.Time
}

func (c delayedAdmissionChecker) Check(context.Context, AdmissionCheck) (AdmissionCheckResult, error) {
	time.Sleep(c.delay)
	return c.result, nil
}

func (c blockingAdmissionChecker) Check(_ context.Context, check AdmissionCheck) (AdmissionCheckResult, error) {
	close(c.started)
	<-c.release
	return AdmissionCheckResult{
		Version:            admissionSnapshotVersion,
		Allowed:            true,
		DirectAllowed:      true,
		RoomAllowed:        true,
		Generation:         check.Generation,
		GateGeneration:     check.GateGeneration,
		GateEvidenceDigest: check.GateEvidenceDigest,
		ExpiresAt:          c.expiresAt,
	}, nil
}

func (c *fetchingAdmissionChecker) Snapshot(context.Context) (AdmissionSnapshot, error) {
	return c.snapshot, nil
}

func (c *fetchingAdmissionChecker) Check(ctx context.Context, check AdmissionCheck) (AdmissionCheckResult, error) {
	return c.checker.Check(ctx, check)
}

func (c *recordingAdmissionChecker) Check(_ context.Context, check AdmissionCheck) (AdmissionCheckResult, error) {
	c.checks = append(c.checks, check)
	generation := check.Generation
	if c.mismatch {
		generation = "99999999-9999-4999-8999-999999999999"
	}
	return AdmissionCheckResult{
		Version:            admissionSnapshotVersion,
		Allowed:            true,
		DirectAllowed:      true,
		RoomAllowed:        c.roomAllowed,
		Generation:         generation,
		GateGeneration:     check.GateGeneration,
		GateEvidenceDigest: check.GateEvidenceDigest,
		ExpiresAt:          c.expiresAt,
	}, nil
}

func TestAdmissionLeaseExpiresAndInvalidationLoadsNewGeneration(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	snapshot := testAdmissionSnapshot(cfg, true)
	snapshot.GeneratedAt = now
	snapshot.ExpiresAt = now.Add(15 * time.Second)
	mustWriteAdmissionSnapshot(cfg.StateDir, snapshot)
	checker := &recordingAdmissionChecker{roomAllowed: true, expiresAt: now.Add(15 * time.Second)}
	authority := NewAdmissionAuthority(cfg, checker)
	authority.now = func() time.Time { return now }

	first, err := authority.Acquire(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExpiresAt.Sub(now) != 15*time.Second || len(checker.checks) != 1 {
		t.Fatalf("first lease=%+v checks=%d", first, len(checker.checks))
	}
	if _, err := authority.Acquire(context.Background(), true); err != nil || len(checker.checks) != 1 {
		t.Fatalf("cached lease err=%v checks=%d", err, len(checker.checks))
	}

	authority.Invalidate()
	snapshot.Generation = "44444444-4444-4444-8444-444444444444"
	snapshot.GeneratedAt = now.Add(time.Second)
	snapshot.ExpiresAt = now.Add(15 * time.Second)
	mustWriteAdmissionSnapshot(cfg.StateDir, snapshot)
	checker.expiresAt = now.Add(15 * time.Second)
	second, err := authority.Acquire(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if second.Snapshot.Generation != snapshot.Generation || len(checker.checks) != 2 {
		t.Fatalf("second lease=%+v checks=%+v", second, checker.checks)
	}

	authority.now = func() time.Time { return now.Add(16 * time.Second) }
	if _, err := authority.Acquire(context.Background(), false); admissionErrorCode(err) != "admission_expired" {
		t.Fatalf("expired lease err=%v code=%s", err, admissionErrorCode(err))
	}
}

func TestAdmissionRejectsLeaseThatExpiresDuringAuthorityCheck(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	now := time.Now().UTC()
	snapshot := testAdmissionSnapshot(cfg, true)
	snapshot.GeneratedAt = now
	snapshot.ExpiresAt = now.Add(20 * time.Millisecond)
	mustWriteAdmissionSnapshot(cfg.StateDir, snapshot)
	checker := delayedAdmissionChecker{
		delay: 40 * time.Millisecond,
		result: AdmissionCheckResult{
			Version:            admissionSnapshotVersion,
			Allowed:            true,
			DirectAllowed:      true,
			RoomAllowed:        true,
			Generation:         snapshot.Generation,
			GateGeneration:     snapshot.GateGeneration,
			GateEvidenceDigest: snapshot.GateEvidenceDigest,
			ExpiresAt:          now.Add(time.Minute),
		},
	}
	authority := NewAdmissionAuthority(cfg, checker)
	if _, err := authority.Acquire(context.Background(), true); admissionErrorCode(err) != "admission_expired" {
		t.Fatalf("expired in-flight lease err=%v code=%s", err, admissionErrorCode(err))
	}
}

func TestAdmissionInvalidationCannotBeOvertakenByInFlightAcquire(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	now := time.Now().UTC()
	snapshot := testAdmissionSnapshot(cfg, true)
	snapshot.GeneratedAt = now
	snapshot.ExpiresAt = now.Add(maximumAdmissionLease)
	mustWriteAdmissionSnapshot(cfg.StateDir, snapshot)
	started := make(chan struct{})
	release := make(chan struct{})
	authority := NewAdmissionAuthority(cfg, blockingAdmissionChecker{
		started:   started,
		release:   release,
		expiresAt: snapshot.ExpiresAt,
	})
	authority.now = func() time.Time { return now }

	acquired := make(chan error, 1)
	go func() {
		_, err := authority.Acquire(context.Background(), true)
		acquired <- err
	}()
	<-started
	invalidationStarted := make(chan struct{})
	invalidated := make(chan struct{})
	go func() {
		close(invalidationStarted)
		authority.Invalidate()
		close(invalidated)
	}()
	<-invalidationStarted
	select {
	case <-invalidated:
		t.Fatal("invalidation completed before the in-flight acquisition")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}
	<-invalidated
	authority.mu.Lock()
	lease := authority.lease
	authority.mu.Unlock()
	if lease != nil {
		t.Fatalf("in-flight acquisition republished an invalidated lease: %+v", lease)
	}
}

func TestAdmissionFetchesAndAtomicallyPublishesCurrentSnapshot(t *testing.T) {
	stateDir := testStateDir(t)
	cfg := testConfig(stateDir)
	now := time.Now().UTC()
	snapshot := testAdmissionSnapshot(cfg, true)
	snapshot.GeneratedAt = now
	snapshot.ExpiresAt = now.Add(maximumAdmissionLease)
	checker := &fetchingAdmissionChecker{
		snapshot: snapshot,
		checker:  &recordingAdmissionChecker{roomAllowed: true, expiresAt: snapshot.ExpiresAt},
	}
	authority := NewAdmissionAuthority(cfg, checker)
	authority.now = func() time.Time { return now }

	lease, err := authority.Acquire(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Snapshot.Generation != snapshot.Generation {
		t.Fatalf("lease snapshot=%+v", lease.Snapshot)
	}
	published, err := loadAdmissionSnapshot(cfg.AdmissionPath)
	if err != nil {
		t.Fatal(err)
	}
	if published.Generation != snapshot.Generation {
		t.Fatalf("published snapshot=%+v", published)
	}
	info, err := os.Stat(cfg.AdmissionPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("admission mode=%#o", info.Mode().Perm())
	}
}

func TestAdmissionRejectsGateMismatchAndStaleAutonomousRestart(t *testing.T) {
	stateDir := testStateDir(t)
	cfg := testConfig(stateDir)
	cfg.RoomEnabled = true
	mustWriteAdmissionSnapshot(stateDir, testAdmissionSnapshot(cfg, true))
	checker := &recordingAdmissionChecker{roomAllowed: true, mismatch: true, expiresAt: time.Now().UTC().Add(10 * time.Second)}
	x := &fakeXMPP{}
	bridge := NewBridgeWithClients(cfg, nil, x, newFakeOpencode())
	bridge.authority = NewAdmissionAuthority(cfg, checker)

	err := bridge.Run(context.Background())
	if admissionErrorCode(err) != "gate_mismatch" {
		t.Fatalf("Run error=%v code=%s", err, admissionErrorCode(err))
	}
	if x.connected || len(x.joins) != 0 {
		t.Fatalf("stale environment connected=%t joins=%v", x.connected, x.joins)
	}
	var status StatusFile
	readJSON(t, filepath.Join(stateDir, "status.json"), &status)
	if status.RoomState != "disabled" || status.LastErrorCode == nil || *status.LastErrorCode != "gate_mismatch" {
		t.Fatalf("status=%+v", status)
	}
}

func TestAdmissionGateClosedPermitsDirectOnlyWithoutRoomJoin(t *testing.T) {
	stateDir := testStateDir(t)
	cfg := testConfig(stateDir)
	cfg.RoomEnabled = true
	mustWriteAdmissionSnapshot(stateDir, testAdmissionSnapshot(cfg, false))
	x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Type: xmpp.DirectChatMessageType, Body: "hello"}}}
	bridge := NewBridgeWithClients(cfg, nil, x, newFakeOpencode("session-1"))
	bridge.authority = NewAdmissionAuthority(cfg, fakeAdmissionChecker{roomAllowed: false})
	if err := bridge.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !x.connected || len(x.joins) != 0 || len(x.sent) != 1 {
		t.Fatalf("connected=%t joins=%v sent=%+v", x.connected, x.joins, x.sent)
	}
	if x.sent[0].metadata.Hop != 0 || x.sent[0].metadata.OriginAgentID != cfg.AgentID {
		t.Fatalf("human-triggered metadata=%+v", x.sent[0].metadata)
	}
}

func TestAdmissionSnapshotStrictSchemaAndBounds(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	snapshot := testAdmissionSnapshot(cfg, true)
	snapshot.ExpiresAt = snapshot.GeneratedAt.Add(maximumAdmissionLease + time.Nanosecond)
	if err := validateAdmissionSnapshot(snapshot, cfg, snapshot.GeneratedAt); admissionErrorCode(err) != "admission_invalid" {
		t.Fatalf("overlong snapshot err=%v", err)
	}

	data, err := json.Marshal(testAdmissionSnapshot(cfg, true))
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"credential":"must-not-be-admitted"}`)...)
	if err := os.WriteFile(cfg.AdmissionPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAdmissionSnapshot(cfg.AdmissionPath); admissionErrorCode(err) != "admission_invalid" {
		t.Fatalf("unknown-field snapshot err=%v", err)
	}
}

func TestHTTPAdmissionCheckerNeverReturnsProtectedResponseBody(t *testing.T) {
	checker := &HTTPAdmissionChecker{endpoint: "http://127.0.0.1:1", token: "top-secret", client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("top-secret transport detail")
	})}}
	_, err := checker.Check(context.Background(), AdmissionCheck{})
	if err == nil || strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("checker error leaked token/detail: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestAdmissionPathDefaultsInsideStateDir(t *testing.T) {
	stateDir := testStateDir(t)
	t.Setenv("XMPP_JID", "agent@example.test")
	t.Setenv("XMPP_PASS", "secret")
	t.Setenv("OPENCODE_BASE_URL", "http://127.0.0.1:4096")
	t.Setenv("OPENCODE_DIRECTORY", "/work")
	t.Setenv("OPENCODE_AGENT_ID", "11111111-1111-4111-8111-111111111111")
	t.Setenv("OPENCODE_AGENT_NAME", "mutable-name")
	t.Setenv("XMPPD_STATE_DIR", stateDir)
	t.Setenv("XMPP_ACCOUNT_STATUS", "ACTIVE")
	t.Setenv("XMPP_TENANT_ID", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	t.Setenv("XMPP_MUC_HOST", "Conference.Example.Test.")
	t.Setenv("XMPP_ROOM_JID", "agents-aaaaaaaaaaaa4aaa8aaaaaaaaaaaaaaa@conference.example.test")
	t.Setenv("XMPP_ROOM_NICKNAME", "agent-11111111111141118111111111111111")
	t.Setenv("CHATINFRA_INTERNAL_API_BASE_URL", "https://api.example.test")
	t.Setenv("CHATINFRA_API_TOKEN", "secret-token")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentID == cfg.AgentName || cfg.AgentName != "mutable-name" || cfg.AdmissionPath != filepath.Join(stateDir, "admission.json") {
		t.Fatalf("identity/name/path not separated: %+v", cfg)
	}
}
