package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chatinfra/xmpp/internal/opencode"
	"github.com/chatinfra/xmpp/internal/xmpp"
)

func TestBridgeCreatesSessionAndRepliesToFullJID(t *testing.T) {
	stateDir := testStateDir(t)
	x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Body: "hello"}}}
	oc := newFakeOpencode("ses-1")
	bridge := testBridge(stateDir, x, oc)
	if err := bridge.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !x.connected || !x.presence || !x.closed {
		t.Fatalf("xmpp lifecycle connected=%t presence=%t closed=%t", x.connected, x.presence, x.closed)
	}
	if got := oc.promptSessions(); len(got) != 1 || got[0] != "ses-1" {
		t.Fatalf("prompt sessions=%v", got)
	}
	if len(x.sent) != 1 || x.sent[0].to != "bob@example.com/phone" || x.sent[0].body != "reply:hello" {
		t.Fatalf("sent=%+v", x.sent)
	}
}

func TestBridgeReusesSessionForSameBareJID(t *testing.T) {
	stateDir := testStateDir(t)
	x := &fakeXMPP{messages: []*xmpp.Message{
		{From: "bob@example.com/phone", Body: "one"},
		{From: "bob@example.com/laptop", Body: "two"},
	}}
	oc := newFakeOpencode("ses-1")
	bridge := testBridge(stateDir, x, oc)
	if err := bridge.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if oc.createCount() != 1 {
		t.Fatalf("create count=%d", oc.createCount())
	}
	if got := oc.promptSessions(); len(got) != 2 || got[0] != "ses-1" || got[1] != "ses-1" {
		t.Fatalf("prompt sessions=%v", got)
	}
}

func TestBridgePersistsAndReloadsSessions(t *testing.T) {
	stateDir := testStateDir(t)
	firstXMPP := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Body: "hello"}}}
	firstOpenCode := newFakeOpencode("ses-1")
	if err := testBridge(stateDir, firstXMPP, firstOpenCode).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sessions SessionsFile
	readJSON(t, filepath.Join(stateDir, "sessions.json"), &sessions)
	entry := sessions.Sessions["bob@example.com"]
	if sessions.Version != 2 || entry.ID != "ses-1" || entry.Directory != "/repo" {
		t.Fatalf("sessions=%+v", sessions)
	}

	secondXMPP := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/tablet", Body: "again"}}}
	secondOpenCode := newFakeOpencode()
	if err := testBridge(stateDir, secondXMPP, secondOpenCode).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if secondOpenCode.createCount() != 0 {
		t.Fatalf("create count after reload=%d", secondOpenCode.createCount())
	}
	if got := secondOpenCode.promptSessions(); len(got) != 1 || got[0] != "ses-1" {
		t.Fatalf("prompt sessions=%v", got)
	}
}

func TestBridgeDoesNotReuseCrossDirectorySession(t *testing.T) {
	stateDir := testStateDir(t)
	if err := NewStateStore(stateDir).SaveSessions(map[string]SessionEntry{
		"bob@example.com": {ID: "ses-old", Directory: "/old-repo"},
	}); err != nil {
		t.Fatal(err)
	}
	x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Body: "hello"}}}
	oc := newFakeOpencode("ses-new")
	if err := testBridge(stateDir, x, oc).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if oc.createCount() != 1 {
		t.Fatalf("create count=%d", oc.createCount())
	}
	if got := oc.promptSessions(); len(got) != 1 || got[0] != "ses-new" {
		t.Fatalf("prompt sessions=%v", got)
	}
	var sessions SessionsFile
	readJSON(t, filepath.Join(stateDir, "sessions.json"), &sessions)
	entry := sessions.Sessions["bob@example.com"]
	if entry.ID != "ses-new" || entry.Directory != "/repo" {
		t.Fatalf("sessions=%+v", sessions)
	}
}

func TestBridgeIgnoresLegacyDirectorylessSessions(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "wrapped string", body: `{"sessions":{"bob@example.com":"ses-old"}}`},
		{name: "bare string map", body: `{"bob@example.com":"ses-old"}`},
		{name: "object without directory", body: `{"version":2,"sessions":{"bob@example.com":{"id":"ses-old"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := testStateDir(t)
			if err := os.WriteFile(filepath.Join(stateDir, "sessions.json"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Body: "hello"}}}
			oc := newFakeOpencode("ses-new")
			if err := testBridge(stateDir, x, oc).Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if oc.createCount() != 1 {
				t.Fatalf("create count=%d", oc.createCount())
			}
			if got := oc.promptSessions(); len(got) != 1 || got[0] != "ses-new" {
				t.Fatalf("prompt sessions=%v", got)
			}
			var sessions SessionsFile
			readJSON(t, filepath.Join(stateDir, "sessions.json"), &sessions)
			entry := sessions.Sessions["bob@example.com"]
			if sessions.Version != 2 || entry.ID != "ses-new" || entry.Directory != "/repo" {
				t.Fatalf("sessions=%+v", sessions)
			}
		})
	}
}

func TestBridgeSerializesPromptsForOneSession(t *testing.T) {
	stateDir := testStateDir(t)
	x := &fakeXMPP{messages: []*xmpp.Message{
		{From: "bob@example.com/phone", Body: "one"},
		{From: "bob@example.com/phone", Body: "two"},
	}}
	oc := newFakeOpencode("ses-1")
	started := make(chan promptCall, 2)
	releaseFirst := make(chan struct{})
	oc.promptFunc = func(ctx context.Context, sessionID, text string) (opencode.AssistantResponse, error) {
		started <- promptCall{sessionID: sessionID, text: text}
		if text == "one" {
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return opencode.AssistantResponse{}, ctx.Err()
			}
		}
		return opencode.AssistantResponse{SessionID: sessionID, Text: "reply:" + text}, nil
	}
	done := make(chan error, 1)
	go func() { done <- testBridge(stateDir, x, oc).Run(context.Background()) }()
	first := <-started
	if first.text != "one" {
		t.Fatalf("first prompt=%+v", first)
	}
	select {
	case second := <-started:
		t.Fatalf("second prompt started before first completed: %+v", second)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseFirst)
	second := <-started
	if second.text != "two" {
		t.Fatalf("second prompt=%+v", second)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBridgeRecreatesRejectedSession(t *testing.T) {
	stateDir := testStateDir(t)
	if err := NewStateStore(stateDir).SaveSessions(map[string]SessionEntry{
		"bob@example.com": {ID: "ses-old", Directory: "/repo"},
	}); err != nil {
		t.Fatal(err)
	}
	x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Body: "hello"}}}
	oc := newFakeOpencode("ses-new")
	oc.promptFunc = func(ctx context.Context, sessionID, text string) (opencode.AssistantResponse, error) {
		if sessionID == "ses-old" {
			return opencode.AssistantResponse{}, &opencode.StaleSessionError{SessionID: sessionID}
		}
		return opencode.AssistantResponse{SessionID: sessionID, Text: "fresh"}, nil
	}
	if err := testBridge(stateDir, x, oc).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := oc.promptSessions(); len(got) != 2 || got[0] != "ses-old" || got[1] != "ses-new" {
		t.Fatalf("prompt sessions=%v", got)
	}
	var sessions SessionsFile
	readJSON(t, filepath.Join(stateDir, "sessions.json"), &sessions)
	entry := sessions.Sessions["bob@example.com"]
	if entry.ID != "ses-new" || entry.Directory != "/repo" {
		t.Fatalf("sessions=%+v", sessions)
	}
}

func TestBridgeRecreatesPoisonedSessionOnceAndReplies(t *testing.T) {
	stateDir := testStateDir(t)
	if err := NewStateStore(stateDir).SaveSessions(map[string]SessionEntry{
		"bob@example.com": {ID: "ses-old", Directory: "/repo"},
	}); err != nil {
		t.Fatal(err)
	}
	x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Body: "hello"}}}
	oc := newFakeOpencode("ses-new")
	oc.promptFunc = func(ctx context.Context, sessionID, text string) (opencode.AssistantResponse, error) {
		if sessionID == "ses-old" {
			return opencode.AssistantResponse{}, poisonedSessionError(sessionID)
		}
		return opencode.AssistantResponse{SessionID: sessionID, Text: "fresh"}, nil
	}
	if err := testBridge(stateDir, x, oc).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if oc.createCount() != 1 {
		t.Fatalf("create count=%d", oc.createCount())
	}
	if got := oc.promptSessions(); len(got) != 2 || got[0] != "ses-old" || got[1] != "ses-new" {
		t.Fatalf("prompt sessions=%v", got)
	}
	if len(x.sent) != 1 || x.sent[0].body != "fresh" {
		t.Fatalf("sent=%+v", x.sent)
	}
	var status StatusFile
	readJSON(t, filepath.Join(stateDir, "status.json"), &status)
	if status.LastReplyAt == nil || status.LastInboundAt == nil {
		t.Fatalf("status=%+v", status)
	}
	var sessions SessionsFile
	readJSON(t, filepath.Join(stateDir, "sessions.json"), &sessions)
	entry := sessions.Sessions["bob@example.com"]
	if entry.ID != "ses-new" || entry.Directory != "/repo" {
		t.Fatalf("sessions=%+v", sessions)
	}
}

func TestBridgeRecreateRetryIsBoundedAndContinues(t *testing.T) {
	stateDir := testStateDir(t)
	if err := NewStateStore(stateDir).SaveSessions(map[string]SessionEntry{
		"bob@example.com": {ID: "ses-old", Directory: "/repo"},
	}); err != nil {
		t.Fatal(err)
	}
	x := &fakeXMPP{messages: []*xmpp.Message{
		{From: "bob@example.com/phone", Body: "first"},
		{From: "bob@example.com/phone", Body: "second"},
	}}
	oc := newFakeOpencode("ses-new", "ses-next")
	oc.promptFunc = func(ctx context.Context, sessionID, text string) (opencode.AssistantResponse, error) {
		if text == "first" {
			return opencode.AssistantResponse{}, poisonedSessionError(sessionID)
		}
		return opencode.AssistantResponse{SessionID: sessionID, Text: "second reply"}, nil
	}
	if err := testBridge(stateDir, x, oc).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if oc.createCount() != 2 {
		t.Fatalf("create count=%d", oc.createCount())
	}
	if got := oc.promptSessions(); len(got) != 3 || got[0] != "ses-old" || got[1] != "ses-new" || got[2] != "ses-next" {
		t.Fatalf("prompt sessions=%v", got)
	}
	if len(x.sent) != 1 || x.sent[0].body != "second reply" {
		t.Fatalf("sent=%+v", x.sent)
	}
	var status StatusFile
	readJSON(t, filepath.Join(stateDir, "status.json"), &status)
	if status.LastInboundAt == nil || status.LastReplyAt == nil || status.LastErrorCode != nil || status.LastError != nil {
		t.Fatalf("status=%+v", status)
	}
	var sessions SessionsFile
	readJSON(t, filepath.Join(stateDir, "sessions.json"), &sessions)
	entry := sessions.Sessions["bob@example.com"]
	if entry.ID != "ses-next" || entry.Directory != "/repo" {
		t.Fatalf("sessions=%+v", sessions)
	}
}

func TestBridgeRetriesTransientSessionCreation(t *testing.T) {
	originalInitialDelay := sessionRetryInitialDelay
	originalMaxElapsed := sessionRetryMaxElapsed
	sessionRetryInitialDelay = time.Millisecond
	sessionRetryMaxElapsed = 100 * time.Millisecond
	t.Cleanup(func() {
		sessionRetryInitialDelay = originalInitialDelay
		sessionRetryMaxElapsed = originalMaxElapsed
	})

	stateDir := testStateDir(t)
	x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Body: "hello"}}}
	oc := newFakeOpencode("ses-1")
	oc.createErrors = []error{&opencode.HTTPError{StatusCode: 500, Status: "500 Internal Server Error", Method: "POST", Path: "/session"}}

	if err := testBridge(stateDir, x, oc).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if oc.createCount() != 2 {
		t.Fatalf("create count=%d", oc.createCount())
	}
	if got := oc.promptSessions(); len(got) != 1 || got[0] != "ses-1" {
		t.Fatalf("prompt sessions=%v", got)
	}
	if len(x.sent) != 1 || x.sent[0].body != "reply:hello" {
		t.Fatalf("sent=%+v", x.sent)
	}
}

func TestSessionCreationRetriesStopAtAdmissionLeaseExpiry(t *testing.T) {
	originalInitialDelay := sessionRetryInitialDelay
	originalMaxElapsed := sessionRetryMaxElapsed
	sessionRetryInitialDelay = time.Millisecond
	sessionRetryMaxElapsed = time.Minute
	t.Cleanup(func() {
		sessionRetryInitialDelay = originalInitialDelay
		sessionRetryMaxElapsed = originalMaxElapsed
	})

	stateDir := testStateDir(t)
	cfg := testConfig(stateDir)
	snapshot := testAdmissionSnapshot(cfg, false)
	snapshot.ExpiresAt = snapshot.GeneratedAt.Add(40 * time.Millisecond)
	mustWriteAdmissionSnapshot(stateDir, snapshot)
	x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Type: xmpp.DirectChatMessageType, Body: "hello"}}}
	oc := newFakeOpencode()
	for range 100 {
		oc.createErrors = append(oc.createErrors, &opencode.HTTPError{StatusCode: 500, Status: "500 Internal Server Error", Method: "POST", Path: "/session"})
	}
	bridge := NewBridgeWithClients(cfg, nil, x, oc)
	bridge.authority = NewAdmissionAuthority(cfg, fakeAdmissionChecker{roomAllowed: false})

	if err := bridge.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if oc.createCount() == 0 || len(oc.promptSessions()) != 0 || len(x.sent) != 0 {
		t.Fatalf("expired retry crossed admission boundary: creates=%d prompts=%v sent=%v", oc.createCount(), oc.promptSessions(), x.sent)
	}
}

func TestPromptIsRecheckedAfterSessionCreation(t *testing.T) {
	stateDir := testStateDir(t)
	cfg := testConfig(stateDir)
	snapshot := testAdmissionSnapshot(cfg, false)
	snapshot.ExpiresAt = snapshot.GeneratedAt.Add(30 * time.Millisecond)
	mustWriteAdmissionSnapshot(stateDir, snapshot)
	x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Type: xmpp.DirectChatMessageType, Body: "hello"}}}
	oc := newFakeOpencode()
	oc.createFunc = func(context.Context) (opencode.Session, error) {
		time.Sleep(50 * time.Millisecond)
		return opencode.Session{ID: "late-session"}, nil
	}
	bridge := NewBridgeWithClients(cfg, nil, x, oc)
	bridge.authority = NewAdmissionAuthority(cfg, fakeAdmissionChecker{roomAllowed: false})

	if err := bridge.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(oc.promptSessions()) != 0 || len(x.sent) != 0 {
		t.Fatalf("prompt ran after admission expiry: prompts=%v sent=%v", oc.promptSessions(), x.sent)
	}
}

func TestConcurrentSessionPublicationKeepsEveryMapping(t *testing.T) {
	stateDir := testStateDir(t)
	cfg := testConfig(stateDir)
	bridge := NewBridgeWithClients(cfg, nil, &fakeXMPP{}, newFakeOpencode())
	bridge.sessions = map[string]SessionEntry{}
	const sessionCount = 64
	var workers sync.WaitGroup
	for index := range sessionCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := bridge.recreateSession(context.Background(), fmt.Sprintf("peer-%03d@example.test", index)); err != nil {
				t.Errorf("recreate session: %v", err)
			}
		}()
	}
	workers.Wait()
	var sessions SessionsFile
	readJSON(t, filepath.Join(stateDir, "sessions.json"), &sessions)
	if len(sessions.Sessions) != sessionCount {
		t.Fatalf("published sessions=%d want=%d", len(sessions.Sessions), sessionCount)
	}
}

func TestBridgeLogsOpencodeErrorsAndUpdatesStatus(t *testing.T) {
	stateDir := testStateDir(t)
	x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Body: "hello"}}}
	oc := newFakeOpencode("ses-1")
	oc.promptFunc = func(context.Context, string, string) (opencode.AssistantResponse, error) {
		return opencode.AssistantResponse{}, errors.New("opencode boom")
	}
	var logs bytes.Buffer
	bridge := testBridgeWithLogger(stateDir, log.New(&logs, "", 0), x, oc)
	if err := bridge.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(x.sent) != 0 {
		t.Fatalf("sent replies=%+v", x.sent)
	}
	if bytes.Contains(logs.Bytes(), []byte("opencode boom")) || !bytes.Contains(logs.Bytes(), []byte("opencode prompt failed")) {
		t.Fatalf("logs should be bounded and redacted: %s", logs.String())
	}
	var status StatusFile
	readJSON(t, filepath.Join(stateDir, "status.json"), &status)
	if status.LastErrorCode == nil || *status.LastErrorCode != "opencode_failed" || status.LastError == nil || *status.LastError != "OpenCode prompt failed" || status.LastInboundAt == nil || status.ActiveSessionCount != 1 {
		t.Fatalf("status=%+v", status)
	}
	if status.LastReplyAt != nil {
		t.Fatalf("last reply should be empty: %+v", status.LastReplyAt)
	}
}

func TestBridgeStatusReflectsConnectedAndReplyTimestamps(t *testing.T) {
	stateDir := testStateDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	processed := make(chan struct{})
	x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Body: "hello"}}, blockUntilContext: true}
	oc := newFakeOpencode("ses-1")
	oc.afterPrompt = func() { close(processed) }
	bridge := testBridge(stateDir, x, oc)
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx) }()
	select {
	case <-processed:
	case <-time.After(time.Second):
		t.Fatal("prompt not processed")
	}
	running := waitForStatus(t, filepath.Join(stateDir, "status.json"), func(status StatusFile) bool {
		return status.XMPPConnected && status.LastInboundAt != nil && status.LastReplyAt != nil
	})
	if !running.XMPPConnected || running.LastInboundAt == nil || running.LastReplyAt == nil {
		t.Fatalf("running status=%+v", running)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var stopped StatusFile
	readJSON(t, filepath.Join(stateDir, "status.json"), &stopped)
	if stopped.XMPPConnected {
		t.Fatalf("stopped status=%+v", stopped)
	}
}

func TestBridgeSendsChatStatesAroundSuccessfulPrompt(t *testing.T) {
	stateDir := testStateDir(t)
	x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Body: "hello"}}}
	oc := newFakeOpencode("ses-1")
	bridge := testBridge(stateDir, x, oc)
	if err := bridge.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"state:bob@example.com/phone:active",
		"state:bob@example.com/phone:composing",
		"message:bob@example.com/phone:reply:hello",
		"state:bob@example.com/phone:active",
	}
	if got := x.operations(); !equalStrings(got, want) {
		t.Fatalf("operations=%v, want %v", got, want)
	}
}

func TestBridgeClearsChatStateOnOpencodeError(t *testing.T) {
	stateDir := testStateDir(t)
	x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Body: "hello"}}}
	oc := newFakeOpencode("ses-1")
	oc.promptFunc = func(context.Context, string, string) (opencode.AssistantResponse, error) {
		return opencode.AssistantResponse{}, errors.New("opencode boom")
	}
	bridge := testBridge(stateDir, x, oc)
	if err := bridge.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if sent := x.sentMessages(); len(sent) != 0 {
		t.Fatalf("sent replies=%+v", sent)
	}
	want := []string{
		"state:bob@example.com/phone:active",
		"state:bob@example.com/phone:composing",
		"state:bob@example.com/phone:active",
	}
	if got := x.operations(); !equalStrings(got, want) {
		t.Fatalf("operations=%v, want %v", got, want)
	}
}

func testBridge(stateDir string, x *fakeXMPP, oc *fakeOpencode) *Bridge {
	return testBridgeWithLogger(stateDir, log.New(&bytes.Buffer{}, "", 0), x, oc)
}

func testBridgeWithLogger(stateDir string, logger *log.Logger, x *fakeXMPP, oc *fakeOpencode) *Bridge {
	cfg := testConfig(stateDir)
	snapshot := testAdmissionSnapshot(cfg, false)
	mustWriteAdmissionSnapshot(stateDir, snapshot)
	bridge := NewBridgeWithClients(cfg, logger, x, oc)
	bridge.authority = NewAdmissionAuthority(cfg, fakeAdmissionChecker{roomAllowed: false})
	return bridge
}

func testConfig(stateDir string) Config {
	return Config{
		XMPP:               xmpp.Config{JID: "agent@example.com/xmppd", Password: "secret", Plaintext: true},
		OpencodeBaseURL:    "http://127.0.0.1:2721",
		OpencodeDirectory:  "/repo",
		AgentID:            "11111111-1111-4111-8111-111111111111",
		AgentName:          "build-agent",
		StateDir:           stateDir,
		PromptTimeout:      time.Second,
		AccountStatus:      "ACTIVE",
		TenantID:           "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		MUCHost:            "conference.example.com",
		RoomJID:            "agents-aaaaaaaaaaaa4aaa8aaaaaaaaaaaaaaa@conference.example.com",
		RoomNickname:       "agent-11111111111141118111111111111111",
		AdmissionPath:      filepath.Join(stateDir, "admission.json"),
		InternalAPIBaseURL: "https://api.example.test",
		InternalAPIToken:   "test-token",
	}
}

type fakeXMPP struct {
	mu                sync.Mutex
	messages          []*xmpp.Message
	events            []xmpp.Event
	blockUntilContext bool
	connected         bool
	presence          bool
	closed            bool
	sent              []sentMessage
	states            []sentChatState
	ops               []string
	joins             []string
}

type sentMessage struct {
	to, messageType, body string
	metadata              xmpp.AgentMessageMetadata
}
type sentChatState struct {
	to    string
	state xmpp.ChatState
}

func (f *fakeXMPP) Connect(context.Context) error {
	f.connected = true
	return nil
}

func (f *fakeXMPP) SendPresence() error {
	f.presence = true
	return nil
}

func (f *fakeXMPP) JoinMUC(roomJID, nickname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joins = append(f.joins, roomJID+"/"+nickname)
	return nil
}

func (f *fakeXMPP) StreamEvents(ctx context.Context, yield func(xmpp.Event) error) error {
	for _, event := range f.events {
		if err := yield(event); err != nil {
			return err
		}
	}
	for _, msg := range f.messages {
		copy := *msg
		if copy.Type == "" {
			copy.Type = xmpp.DirectChatMessageType
		}
		if err := yield(xmpp.Event{Message: &copy}); err != nil {
			return err
		}
	}
	if f.blockUntilContext {
		<-ctx.Done()
	}
	return nil
}

func (f *fakeXMPP) SendAgentMessage(to, messageType, body string, metadata xmpp.AgentMessageMetadata) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMessage{to: to, messageType: messageType, body: body, metadata: metadata})
	f.ops = append(f.ops, "message:"+to+":"+body)
	return "message-1", nil
}

func (f *fakeXMPP) SendChatState(to string, state xmpp.ChatState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = append(f.states, sentChatState{to: to, state: state})
	f.ops = append(f.ops, "state:"+to+":"+string(state))
	return nil
}

func (f *fakeXMPP) Close() error {
	f.closed = true
	return nil
}

func (f *fakeXMPP) sentMessages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.sent...)
}

func (f *fakeXMPP) operations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

type fakeOpencode struct {
	mu           sync.Mutex
	sessions     []string
	created      int
	createErrors []error
	createFunc   func(context.Context) (opencode.Session, error)
	prompts      []promptCall
	promptFunc   func(context.Context, string, string) (opencode.AssistantResponse, error)
	afterPrompt  func()
}

type promptCall struct{ sessionID, text string }

func poisonedSessionError(sessionID string) error {
	return &opencode.HTTPError{
		StatusCode: 500,
		Status:     "500 Internal Server Error",
		Method:     "POST",
		Path:       "/session/" + sessionID + "/message?directory=%2Frepo",
		Body:       `{"name":"UnknownError"}`,
	}
}

func newFakeOpencode(sessions ...string) *fakeOpencode {
	return &fakeOpencode{sessions: sessions}
}

func (f *fakeOpencode) CreateSession(ctx context.Context) (opencode.Session, error) {
	f.mu.Lock()
	f.created++
	createFunc := f.createFunc
	if createFunc != nil {
		f.mu.Unlock()
		return createFunc(ctx)
	}
	defer f.mu.Unlock()
	if len(f.createErrors) > 0 {
		err := f.createErrors[0]
		f.createErrors = f.createErrors[1:]
		return opencode.Session{}, err
	}
	if len(f.sessions) == 0 {
		return opencode.Session{ID: "ses-created"}, nil
	}
	sessionID := f.sessions[0]
	f.sessions = f.sessions[1:]
	return opencode.Session{ID: sessionID}, nil
}

func (f *fakeOpencode) Prompt(ctx context.Context, sessionID, text string) (opencode.AssistantResponse, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, promptCall{sessionID: sessionID, text: text})
	f.mu.Unlock()
	if f.promptFunc != nil {
		response, err := f.promptFunc(ctx, sessionID, text)
		if f.afterPrompt != nil {
			f.afterPrompt()
		}
		return response, err
	}
	if f.afterPrompt != nil {
		f.afterPrompt()
	}
	return opencode.AssistantResponse{SessionID: sessionID, Text: "reply:" + text}, nil
}

func (f *fakeOpencode) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

func (f *fakeOpencode) promptSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	sessions := make([]string, 0, len(f.prompts))
	for _, prompt := range f.prompts {
		sessions = append(sessions, prompt.sessionID)
	}
	return sessions
}

func readJSON(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func waitForStatus(t *testing.T, path string, done func(StatusFile) bool) StatusFile {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var status StatusFile
	for time.Now().Before(deadline) {
		readJSON(t, path, &status)
		if done(status) {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	return status
}

type fakeAdmissionChecker struct {
	roomAllowed bool
	err         error
}

func (f fakeAdmissionChecker) Check(_ context.Context, check AdmissionCheck) (AdmissionCheckResult, error) {
	if f.err != nil {
		return AdmissionCheckResult{}, f.err
	}
	return AdmissionCheckResult{
		Version:            admissionSnapshotVersion,
		Allowed:            true,
		DirectAllowed:      true,
		RoomAllowed:        f.roomAllowed,
		Generation:         check.Generation,
		GateGeneration:     check.GateGeneration,
		GateEvidenceDigest: check.GateEvidenceDigest,
		ExpiresAt:          time.Now().UTC().Add(10 * time.Second),
	}, nil
}

func testAdmissionSnapshot(cfg Config, _ bool) AdmissionSnapshot {
	now := time.Now().UTC()
	return AdmissionSnapshot{
		Version:            admissionSnapshotVersion,
		Generation:         "22222222-2222-4222-8222-222222222222",
		GateGeneration:     "33333333-3333-4333-8333-333333333333",
		GateEvidenceDigest: strings.Repeat("b", 64),
		GeneratedAt:        now,
		ExpiresAt:          now.Add(10 * time.Second),
		TenantID:           cfg.TenantID,
		AgentID:            cfg.AgentID,
		RoomJID:            cfg.RoomJID,
		Users:              []string{"bob@example.com"},
		Agents: []AdmissionAgent{{
			AgentID:  cfg.AgentID,
			BareJID:  normalizeBareJID(cfg.XMPP.JID),
			Nickname: cfg.RoomNickname,
		}},
	}
}

func mustWriteAdmissionSnapshot(stateDir string, snapshot AdmissionSnapshot) {
	data, err := json.Marshal(snapshot)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "admission.json"), data, 0o600); err != nil {
		panic(err)
	}
}
