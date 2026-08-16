package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type fakeControlBackend struct {
	list       ControlListResult
	send       ControlSendResult
	sendTarget ControlTarget
	sendBody   string
	err        error
}

func (f *fakeControlBackend) ControlList(context.Context) (ControlListResult, error) {
	return f.list, f.err
}

func (f *fakeControlBackend) ControlSend(_ context.Context, target ControlTarget, body string) (ControlSendResult, error) {
	f.sendTarget = target
	f.sendBody = body
	return f.send, f.err
}

func TestControlSocketModeProtocolAndCtlStdin(t *testing.T) {
	stateDir := testStateDir(t)
	backend := &fakeControlBackend{
		list: ControlListResult{
			Room:  ControlRoom{JID: "agents@example.test", Nickname: "agent-self", State: "joined"},
			Peers: []Peer{{AgentID: peerAgentID, JID: "peer@example.test", Nickname: peerNickname}},
		},
		send: ControlSendResult{MessageID: "message-1", Correlation: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Hop: 0},
	}
	ctx, cancel := context.WithCancel(context.Background())
	server, err := StartControlServer(ctx, stateDir, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = server.Close() })
	info, err := os.Stat(filepath.Join(stateDir, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("control socket mode=%v", info.Mode())
	}
	dirInfo, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode=%o", dirInfo.Mode().Perm())
	}

	var listOutput bytes.Buffer
	if err := runCtl([]string{"list", "--state-dir", stateDir}, strings.NewReader(""), &listOutput); err != nil {
		t.Fatal(err)
	}
	if got := listOutput.String(); !strings.Contains(got, "room\tjoined\tagents@example.test\tagent-self") || !strings.Contains(got, "peer\tpeer@example.test") || strings.HasPrefix(strings.TrimSpace(got), "{") {
		t.Fatalf("list output=%q", got)
	}

	var sendOutput bytes.Buffer
	if err := runCtl([]string{"send", "--state-dir", stateDir, "--peer", "peer@example.test"}, strings.NewReader("stdin body"), &sendOutput); err != nil {
		t.Fatal(err)
	}
	if backend.sendBody != "stdin body" || backend.sendTarget != (ControlTarget{Kind: "peer", JID: "peer@example.test"}) {
		t.Fatalf("target=%+v body=%q", backend.sendTarget, backend.sendBody)
	}
	if sendOutput.String() != "sent\tmessage-1\taaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa\t0\n" {
		t.Fatalf("send output=%q", sendOutput.String())
	}
}

func TestControlRequestFrameBoundsIncludeLF(t *testing.T) {
	allowed := strings.Repeat("a", maximumRequestFrame-1) + "\n"
	frame, err := readNDJSONFrame(strings.NewReader(allowed), maximumRequestFrame)
	if err != nil || len(frame) != maximumRequestFrame-1 {
		t.Fatalf("allowed frame len=%d err=%v", len(frame), err)
	}
	tooLarge := strings.Repeat("a", maximumRequestFrame) + "\n"
	if _, err := readNDJSONFrame(strings.NewReader(tooLarge), maximumRequestFrame); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("oversized frame err=%v", err)
	}
	if _, err := readNDJSONFrame(strings.NewReader(`{"version":1}`), maximumRequestFrame); err == nil {
		t.Fatal("unterminated frame was accepted")
	}
}

func TestControlServerReturnsBoundedErrorsWithoutParsingOversizedPrefix(t *testing.T) {
	stateDir := testStateDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	server, err := StartControlServer(ctx, stateDir, &fakeControlBackend{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = server.Close() })
	connection, err := net.Dial("unix", filepath.Join(stateDir, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(connection, strings.Repeat("x", maximumRequestFrame)+"\n"); err != nil {
		t.Fatal(err)
	}
	frame, err := readNDJSONFrame(connection, maximumResponseFrame)
	if err != nil {
		t.Fatal(err)
	}
	var response controlResponse
	if err := decodeStrictJSON(frame, &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Code != "request_too_large" || len([]byte(response.Error.Message)) > maximumControlError {
		t.Fatalf("response=%+v", response)
	}
}

func TestControlStrictShapesAndStableErrors(t *testing.T) {
	cases := []struct {
		name string
		json string
		code string
	}{
		{"unknown list field", `{"version":1,"requestId":"r","op":"list","body":"x"}`, "invalid_request"},
		{"unsupported version", `{"version":2,"requestId":"r","op":"list"}`, "unsupported_version"},
		{"empty body", `{"version":1,"requestId":"r","op":"send","target":{"kind":"room"},"body":""}`, "body_empty"},
		{"unknown peer target field", `{"version":1,"requestId":"r","op":"send","target":{"kind":"peer","jid":"peer@example.test","tenant":"other"},"body":"x"}`, "invalid_target"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request, err := parseControlRequest([]byte(test.json))
			if err == nil && test.code == "invalid_target" {
				_, err = parseControlTarget(request.Target)
			}
			var typed *controlError
			if !errors.As(err, &typed) || typed.Code != test.code {
				t.Fatalf("err=%v typed=%+v", err, typed)
			}
		})
	}

	message := strings.Repeat("界", 300)
	err := newControlError("send_failed", message)
	var typed *controlError
	if !errors.As(err, &typed) || len([]byte(typed.Message)) > maximumControlError || !utf8.ValidString(typed.Message) || strings.HasSuffix(typed.Message, "...") {
		t.Fatalf("bounded error=%+v bytes=%d", typed, len([]byte(typed.Message)))
	}
}

func TestControlRejectsInvalidUTF8BeforeJSONOrStdinConversion(t *testing.T) {
	invalidRequest := append([]byte(`{"version":1,"requestId":"r","op":"send","target":{"kind":"room"},"body":"`), 0xff)
	invalidRequest = append(invalidRequest, []byte(`"}`)...)
	if _, err := parseControlRequest(invalidRequest); err == nil {
		t.Fatal("invalid UTF-8 request was accepted")
	}

	var output bytes.Buffer
	err := runCtl(
		[]string{"send", "--state-dir", testStateDir(t), "--room"},
		bytes.NewReader([]byte{0xff}),
		&output,
	)
	if err == nil || !strings.Contains(err.Error(), "valid UTF-8") || output.Len() != 0 {
		t.Fatalf("invalid stdin err=%v output=%q", err, output.String())
	}
}

func TestControlRejectsDifferentUIDBeforeParsing(t *testing.T) {
	original := controlPeerUID
	controlPeerUID = func(*net.UnixConn) (uint32, error) { return uint32(os.Geteuid() + 1), nil }
	t.Cleanup(func() { controlPeerUID = original })
	stateDir := testStateDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	server, err := StartControlServer(ctx, stateDir, &fakeControlBackend{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = server.Close() })
	connection, err := net.Dial("unix", filepath.Join(stateDir, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	_, _ = io.WriteString(connection, "not-json\n")
	data, err := io.ReadAll(connection)
	if len(data) != 0 {
		t.Fatalf("different UID received parsed response: %q", data)
	}
	if err != nil && !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatal(err)
	}
}

func TestControlResponseFrameBoundIncludesLF(t *testing.T) {
	response := controlResponse{Version: 1, RequestID: "r", OK: true, Result: strings.Repeat("x", maximumResponseFrame)}
	if err := writeControlResponse(io.Discard, response); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("oversized response err=%v", err)
	}
	bounded := errorControlResponse("r", "send_failed", strings.Repeat("x", maximumControlError+20))
	data, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if len([]byte(bounded.Error.Message)) != maximumControlError || len(data)+1 > maximumResponseFrame {
		t.Fatalf("bounded response size=%d message=%d", len(data)+1, len([]byte(bounded.Error.Message)))
	}
}

func TestCtlRejectsJSONAndRequiresStdin(t *testing.T) {
	var output bytes.Buffer
	if err := runCtl([]string{"--json"}, strings.NewReader(""), &output); err == nil || output.Len() != 0 {
		t.Fatalf("--json err=%v output=%q", err, output.String())
	}
	if err := runCtl([]string{"send", "--state-dir", testStateDir(t), "--room"}, strings.NewReader(""), &output); err == nil {
		t.Fatal("empty stdin was accepted")
	}
}

func TestCtlSubcommandHelpSucceedsWithoutSocket(t *testing.T) {
	for _, args := range [][]string{{"list", "--help"}, {"send", "-h"}} {
		var output bytes.Buffer
		if err := runCtl(args, strings.NewReader(""), &output); err != nil {
			t.Fatalf("args=%v err=%v", args, err)
		}
		if !strings.Contains(output.String(), "USAGE") || !strings.Contains(output.String(), "OUTPUT") {
			t.Fatalf("args=%v output=%q", args, output.String())
		}
	}
}

func TestControlSendRejectsLeaseThatExpiredBeforeFinalLockedValidation(t *testing.T) {
	cfg := testConfig(testStateDir(t))
	cfg.RoomEnabled = true
	oldNow := time.Now().UTC().Add(-time.Minute)
	snapshot := roomAdmissionSnapshot(cfg)
	snapshot.GeneratedAt = oldNow
	snapshot.ExpiresAt = oldNow.Add(maximumAdmissionLease)
	mustWriteAdmissionSnapshot(cfg.StateDir, snapshot)
	x := &fakeXMPP{}
	bridge := NewBridgeWithClients(cfg, nil, x, newFakeOpencode())
	bridge.authority = NewAdmissionAuthority(cfg, fakeAdmissionChecker{roomAllowed: true})
	bridge.authority.now = func() time.Time { return oldNow }
	bridge.status.XMPPConnected = true
	bridge.status.RoomState = "joined"
	bridge.selfPresent = true

	_, err := bridge.ControlSend(context.Background(), ControlTarget{Kind: "room"}, "hello")
	var controlErr *controlError
	if !errors.As(err, &controlErr) || controlErr.Code != "room_disabled" {
		t.Fatalf("expired send err=%v", err)
	}
	if len(x.sent) != 0 {
		t.Fatalf("expired lease emitted stanza: %+v", x.sent)
	}
}
