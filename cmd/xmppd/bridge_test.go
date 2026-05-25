package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chatinfra/xmpp/internal/opencode"
	"github.com/chatinfra/xmpp/internal/xmpp"
)

func TestBridgeCreatesSessionAndRepliesToFullJID(t *testing.T) {
	stateDir := t.TempDir()
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
	stateDir := t.TempDir()
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
	stateDir := t.TempDir()
	firstXMPP := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Body: "hello"}}}
	firstOpenCode := newFakeOpencode("ses-1")
	if err := testBridge(stateDir, firstXMPP, firstOpenCode).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sessions SessionsFile
	readJSON(t, filepath.Join(stateDir, "sessions.json"), &sessions)
	if sessions.Sessions["bob@example.com"] != "ses-1" {
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

func TestBridgeSerializesPromptsForOneSession(t *testing.T) {
	stateDir := t.TempDir()
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
	stateDir := t.TempDir()
	if err := NewStateStore(stateDir).SaveSessions(map[string]string{"bob@example.com": "ses-old"}); err != nil {
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
	if sessions.Sessions["bob@example.com"] != "ses-new" {
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

	stateDir := t.TempDir()
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

func TestBridgeLogsOpencodeErrorsAndUpdatesStatus(t *testing.T) {
	stateDir := t.TempDir()
	x := &fakeXMPP{messages: []*xmpp.Message{{From: "bob@example.com/phone", Body: "hello"}}}
	oc := newFakeOpencode("ses-1")
	oc.promptFunc = func(context.Context, string, string) (opencode.AssistantResponse, error) {
		return opencode.AssistantResponse{}, errors.New("opencode boom")
	}
	var logs bytes.Buffer
	bridge := NewBridgeWithClients(testConfig(stateDir), log.New(&logs, "", 0), x, oc)
	if err := bridge.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(x.sent) != 0 {
		t.Fatalf("sent replies=%+v", x.sent)
	}
	if !bytes.Contains(logs.Bytes(), []byte("opencode boom")) {
		t.Fatalf("logs=%s", logs.String())
	}
	var status StatusFile
	readJSON(t, filepath.Join(stateDir, "status.json"), &status)
	if status.LastError != "opencode boom" || status.LastInboundAt == nil || status.ActiveSessionCount != 1 {
		t.Fatalf("status=%+v", status)
	}
	if status.LastReplyAt != nil {
		t.Fatalf("last reply should be empty: %+v", status.LastReplyAt)
	}
}

func TestBridgeStatusReflectsConnectedAndReplyTimestamps(t *testing.T) {
	stateDir := t.TempDir()
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
	stateDir := t.TempDir()
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
	stateDir := t.TempDir()
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
	return NewBridgeWithClients(testConfig(stateDir), log.New(&bytes.Buffer{}, "", 0), x, oc)
}

func testConfig(stateDir string) Config {
	return Config{
		XMPP:              xmpp.Config{JID: "agent@example.com/xmppd", Password: "secret", Plaintext: true},
		OpencodeBaseURL:   "http://127.0.0.1:2721",
		OpencodeDirectory: "/repo",
		AgentID:           "agent-1",
		StateDir:          stateDir,
		PromptTimeout:     time.Second,
	}
}

type fakeXMPP struct {
	mu                sync.Mutex
	messages          []*xmpp.Message
	blockUntilContext bool
	connected         bool
	presence          bool
	closed            bool
	sent              []sentMessage
	states            []sentChatState
	ops               []string
}

type sentMessage struct{ to, body string }
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

func (f *fakeXMPP) StreamMessages(ctx context.Context, yield func(*xmpp.Message) error) error {
	for _, msg := range f.messages {
		if err := yield(msg); err != nil {
			return err
		}
	}
	if f.blockUntilContext {
		<-ctx.Done()
	}
	return nil
}

func (f *fakeXMPP) SendMessage(to, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMessage{to: to, body: body})
	f.ops = append(f.ops, "message:"+to+":"+body)
	return nil
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
	prompts      []promptCall
	promptFunc   func(context.Context, string, string) (opencode.AssistantResponse, error)
	afterPrompt  func()
}

type promptCall struct{ sessionID, text string }

func newFakeOpencode(sessions ...string) *fakeOpencode {
	return &fakeOpencode{sessions: sessions}
}

func (f *fakeOpencode) CreateSession(context.Context) (opencode.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
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
