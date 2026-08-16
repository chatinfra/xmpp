package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/chatinfra/xmpp/internal/xmpp"
)

const (
	controlProtocolVersion = 1
	maximumRequestFrame    = 65536
	maximumResponseFrame   = 131072
	maximumControlError    = 512
	maximumControlBody     = 16384
	maximumPeerJID         = 3071
	maximumControlPeers    = 1024
)

type ControlTarget struct {
	Kind string `json:"kind"`
	JID  string `json:"jid,omitempty"`
}

type ControlRoom struct {
	JID      string `json:"jid"`
	Nickname string `json:"nickname"`
	State    string `json:"state"`
}

type ControlListResult struct {
	Room  ControlRoom `json:"room"`
	Peers []Peer      `json:"peers"`
}

type ControlSendResult struct {
	MessageID   string `json:"messageId"`
	Correlation string `json:"correlation"`
	Hop         int    `json:"hop"`
}

type controlRequest struct {
	Version   int             `json:"version"`
	RequestID string          `json:"requestId"`
	Op        string          `json:"op"`
	Target    json.RawMessage `json:"target,omitempty"`
	Body      *string         `json:"body,omitempty"`
}

type controlResponse struct {
	Version   int             `json:"version"`
	RequestID string          `json:"requestId"`
	OK        bool            `json:"ok"`
	Result    any             `json:"result,omitempty"`
	Error     *controlFailure `json:"error,omitempty"`
}

type controlFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type controlError struct {
	Code    string
	Message string
}

func (e *controlError) Error() string { return e.Message }

func newControlError(code, message string) error {
	return &controlError{Code: code, Message: truncateUTF8(strings.TrimSpace(message), maximumControlError)}
}

type controlBackend interface {
	ControlList(context.Context) (ControlListResult, error)
	ControlSend(context.Context, ControlTarget, string) (ControlSendResult, error)
}

type ControlServer struct {
	listener *net.UnixListener
	done     chan struct{}
	clients  sync.WaitGroup
}

var controlPeerUID = unixPeerUID

func StartControlServer(ctx context.Context, stateDir string, backend controlBackend) (*ControlServer, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, "control.sock")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	server := &ControlServer{listener: listener, done: make(chan struct{})}
	go server.serve(ctx, backend, path)
	return server, nil
}

func (s *ControlServer) serve(ctx context.Context, backend controlBackend, path string) {
	defer close(s.done)
	defer os.Remove(path)
	go func() {
		<-ctx.Done()
		_ = s.listener.Close()
	}()
	for {
		connection, err := s.listener.AcceptUnix()
		if err != nil {
			return
		}
		s.clients.Add(1)
		go func() {
			defer s.clients.Done()
			serveControlConnection(ctx, connection, backend)
		}()
	}
}

func (s *ControlServer) Close() error {
	err := s.listener.Close()
	<-s.done
	s.clients.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func serveControlConnection(ctx context.Context, connection *net.UnixConn, backend controlBackend) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	uid, err := controlPeerUID(connection)
	if err != nil || uid != uint32(os.Geteuid()) {
		return
	}
	frame, err := readNDJSONFrame(connection, maximumRequestFrame)
	if err != nil {
		code := "invalid_request"
		if errors.Is(err, errFrameTooLarge) {
			code = "request_too_large"
		}
		_ = writeControlResponse(connection, errorControlResponse("", code, err.Error()))
		return
	}
	request, err := parseControlRequest(frame)
	if err != nil {
		_ = writeControlResponse(connection, responseForError(request.RequestID, err))
		return
	}
	var result any
	switch request.Op {
	case "list":
		result, err = backend.ControlList(ctx)
	case "send":
		target, decodeErr := parseControlTarget(request.Target)
		if decodeErr != nil {
			err = newControlError("invalid_target", "target must use the exact room or peer shape")
			break
		}
		result, err = backend.ControlSend(ctx, target, *request.Body)
	}
	if err != nil {
		_ = writeControlResponse(connection, responseForError(request.RequestID, err))
		return
	}
	response := controlResponse{Version: controlProtocolVersion, RequestID: request.RequestID, OK: true, Result: result}
	if err := writeControlResponse(connection, response); errors.Is(err, errFrameTooLarge) {
		_ = writeControlResponse(connection, errorControlResponse(request.RequestID, "response_too_large", "complete response exceeds 131072 bytes"))
	}
}

func parseControlTarget(data []byte) (ControlTarget, error) {
	var raw map[string]json.RawMessage
	if err := decodeStrictJSON(data, &raw); err != nil {
		return ControlTarget{}, newControlError("invalid_target", "target must be one JSON object")
	}
	var target ControlTarget
	if err := decodeStrictJSON(data, &target); err != nil {
		return ControlTarget{}, newControlError("invalid_target", "target contains unknown or invalid fields")
	}
	switch target.Kind {
	case "room":
		if !exactKeys(raw, "kind") {
			return ControlTarget{}, newControlError("invalid_target", "room target must contain exactly kind")
		}
	case "peer":
		if !exactKeys(raw, "kind", "jid") {
			return ControlTarget{}, newControlError("invalid_target", "peer target must contain exactly kind and jid")
		}
	default:
		return ControlTarget{}, newControlError("invalid_target", "unknown target kind")
	}
	return target, nil
}

func parseControlRequest(frame []byte) (controlRequest, error) {
	if !utf8.Valid(frame) {
		return controlRequest{}, newControlError("invalid_request", "request must be valid UTF-8")
	}
	var raw map[string]json.RawMessage
	if err := decodeStrictJSON(frame, &raw); err != nil {
		return controlRequest{}, newControlError("invalid_request", "request must be one valid JSON object")
	}
	var request controlRequest
	if err := json.Unmarshal(frame, &request); err != nil {
		return request, newControlError("invalid_request", "request fields have invalid types")
	}
	if !validRequestID(request.RequestID) {
		return request, newControlError("invalid_request", "requestId must be 1-64 printable ASCII characters")
	}
	if request.Version != controlProtocolVersion {
		return request, newControlError("unsupported_version", "control protocol version must be 1")
	}
	switch request.Op {
	case "list":
		if !exactKeys(raw, "version", "requestId", "op") {
			return request, newControlError("invalid_request", "list request contains unknown or operation-inapplicable fields")
		}
	case "send":
		if !exactKeys(raw, "version", "requestId", "op", "target", "body") || len(request.Target) == 0 || request.Body == nil {
			return request, newControlError("invalid_request", "send request requires exactly target and body operation fields")
		}
		if !utf8.ValidString(*request.Body) {
			return request, newControlError("invalid_request", "body must be valid UTF-8")
		}
		if len([]byte(*request.Body)) == 0 {
			return request, newControlError("body_empty", "body must not be empty")
		}
		if len([]byte(*request.Body)) > maximumControlBody {
			return request, newControlError("body_too_large", "body exceeds 16384 UTF-8 bytes")
		}
	default:
		return request, newControlError("invalid_request", "op must be list or send")
	}
	return request, nil
}

func exactKeys(raw map[string]json.RawMessage, keys ...string) bool {
	if len(raw) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, exists := raw[key]; !exists {
			return false
		}
	}
	return true
}

func validRequestID(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for _, ch := range []byte(value) {
		if ch < 0x20 || ch > 0x7e {
			return false
		}
	}
	return true
}

var errFrameTooLarge = errors.New("frame exceeds byte bound including terminating LF")

func readNDJSONFrame(reader io.Reader, maximum int) ([]byte, error) {
	limited := io.LimitReader(reader, int64(maximum+1))
	frame, err := bufio.NewReaderSize(limited, maximum+1).ReadBytes('\n')
	if len(frame) > maximum {
		return nil, errFrameTooLarge
	}
	if err != nil {
		return nil, errors.New("frame must end with LF")
	}
	if len(frame) == 0 || frame[len(frame)-1] != '\n' {
		return nil, errors.New("frame must end with LF")
	}
	return frame[:len(frame)-1], nil
}

func writeControlResponse(writer io.Writer, response controlResponse) error {
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maximumResponseFrame {
		return errFrameTooLarge
	}
	_, err = writer.Write(data)
	return err
}

func errorControlResponse(requestID, code, message string) controlResponse {
	return controlResponse{
		Version:   controlProtocolVersion,
		RequestID: requestID,
		OK:        false,
		Error: &controlFailure{
			Code:    code,
			Message: truncateUTF8(strings.TrimSpace(message), maximumControlError),
		},
	}
}

func responseForError(requestID string, err error) controlResponse {
	var typed *controlError
	if errors.As(err, &typed) {
		return errorControlResponse(requestID, typed.Code, typed.Message)
	}
	return errorControlResponse(requestID, "send_failed", "control operation failed")
}

func unixPeerUID(connection *net.UnixConn) (uint32, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credential *syscall.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if socketErr != nil {
		return 0, socketErr
	}
	return credential.Uid, nil
}

func (b *Bridge) ControlList(ctx context.Context) (ControlListResult, error) {
	if _, err := b.refreshAdmission(ctx, false); err != nil {
		b.expireAdmission(admissionErrorCode(err), err.Error())
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	peers := make([]Peer, 0, len(b.peers))
	for _, peer := range b.peers {
		peers = append(peers, peer)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].JID < peers[j].JID })
	if len(peers) > maximumControlPeers {
		return ControlListResult{}, newControlError("response_too_large", "peer set exceeds 1024 entries")
	}
	return ControlListResult{
		Room:  ControlRoom{JID: b.cfg.RoomJID, Nickname: b.cfg.RoomNickname, State: b.status.RoomState},
		Peers: peers,
	}, nil
}

func (b *Bridge) ControlSend(ctx context.Context, target ControlTarget, body string) (ControlSendResult, error) {
	if !utf8.ValidString(body) {
		return ControlSendResult{}, newControlError("invalid_request", "body must be valid UTF-8")
	}
	if len([]byte(body)) == 0 {
		return ControlSendResult{}, newControlError("body_empty", "body must not be empty")
	}
	if len([]byte(body)) > maximumControlBody {
		return ControlSendResult{}, newControlError("body_too_large", "body exceeds 16384 UTF-8 bytes")
	}
	b.mu.Lock()
	connected := b.status.XMPPConnected
	b.mu.Unlock()
	if !connected {
		return ControlSendResult{}, newControlError("not_connected", "XMPP connection is not active")
	}
	messageType := ""
	destination := ""
	var validatedLease AdmissionLease
	switch target.Kind {
	case "room":
		if target.JID != "" {
			return ControlSendResult{}, newControlError("invalid_target", "room target must contain exactly kind")
		}
		if !b.cfg.RoomEnabled {
			return ControlSendResult{}, newControlError("room_disabled", "room mode is disabled")
		}
		lease, err := b.refreshAdmission(ctx, true)
		if err != nil {
			return ControlSendResult{}, newControlError("room_disabled", "room-production gate is not current")
		}
		validatedLease = lease
		b.mu.Lock()
		joined := b.status.RoomState == "joined"
		b.mu.Unlock()
		if !joined {
			return ControlSendResult{}, newControlError("room_not_joined", "configured room is not joined")
		}
		messageType = xmpp.GroupchatMessageType
		destination = b.cfg.RoomJID
	case "peer":
		if target.JID == "" || strings.Contains(target.JID, "/") || len([]byte(target.JID)) > maximumPeerJID || normalizeBareJID(target.JID) != target.JID {
			return ControlSendResult{}, newControlError("invalid_target", "peer target must be a normalized bounded bare JID")
		}
		lease, err := b.refreshAdmission(ctx, false)
		if err != nil {
			return ControlSendResult{}, newControlError("target_not_admitted", "peer admission authority is not current")
		}
		if _, admitted := lease.Snapshot.agentByJID()[target.JID]; !admitted || !b.isCurrentPeer(target.JID) {
			return ControlSendResult{}, newControlError("target_not_admitted", "peer is not currently admitted and discovered")
		}
		validatedLease = lease
		messageType = xmpp.DirectChatMessageType
		destination = target.JID
	default:
		return ControlSendResult{}, newControlError("invalid_target", "target kind must be room or peer")
	}
	correlation, err := newUUID()
	if err != nil {
		return ControlSendResult{}, newControlError("send_failed", "could not create message correlation")
	}
	metadata := xmpp.AgentMessageMetadata{Correlation: correlation, OriginAgentID: b.cfg.AgentID, Hop: 0}
	b.mu.Lock()
	if !b.status.XMPPConnected {
		b.mu.Unlock()
		return ControlSendResult{}, newControlError("not_connected", "XMPP connection is not active")
	}
	if !b.leaseMatchesLocked(validatedLease, time.Now().UTC()) {
		b.mu.Unlock()
		if target.Kind == "room" {
			return ControlSendResult{}, newControlError("room_disabled", "room-production gate is not current")
		}
		return ControlSendResult{}, newControlError("target_not_admitted", "peer admission authority is not current")
	}
	if target.Kind == "room" {
		if !b.lease.RoomAllowed || !b.selfPresent || b.status.RoomState != "joined" {
			b.mu.Unlock()
			return ControlSendResult{}, newControlError("room_not_joined", "configured room is not joined")
		}
	}
	if target.Kind == "peer" {
		_, admitted := b.lease.Snapshot.agentByJID()[target.JID]
		_, current := b.peers[target.JID]
		if !admitted || !current || !b.selfPresent || b.status.RoomState != "joined" {
			b.mu.Unlock()
			return ControlSendResult{}, newControlError("target_not_admitted", "peer is not currently admitted and discovered")
		}
	}
	messageID, err := b.xmpp.SendAgentMessage(destination, messageType, body, metadata)
	b.mu.Unlock()
	if err != nil {
		b.recordError("local_send_failed", "local XMPP send failed")
		return ControlSendResult{}, newControlError("send_failed", "XMPP send failed")
	}
	b.mu.Lock()
	b.clearErrorLocked("local_send_failed")
	b.mu.Unlock()
	b.flushStatus()
	return ControlSendResult{MessageID: messageID, Correlation: correlation, Hop: 0}, nil
}

func (b *Bridge) leaseMatchesLocked(expected AdmissionLease, now time.Time) bool {
	if b.lease == nil || !now.Before(b.lease.ExpiresAt) || !now.Before(expected.ExpiresAt) {
		return false
	}
	return b.lease.Snapshot.Generation == expected.Snapshot.Generation &&
		b.lease.Snapshot.GateGeneration == expected.Snapshot.GateGeneration &&
		b.lease.Snapshot.GateEvidenceDigest == expected.Snapshot.GateEvidenceDigest &&
		b.lease.ExpiresAt.Equal(expected.ExpiresAt) &&
		b.lease.DirectAllowed == expected.DirectAllowed &&
		b.lease.RoomAllowed == expected.RoomAllowed
}

func runCtl(args []string, stdin io.Reader, stdout io.Writer) error {
	for _, arg := range args {
		if arg == "--json" || strings.HasPrefix(arg, "--json=") {
			return errors.New("--json is not supported")
		}
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		_, err := io.WriteString(stdout, xmppdCtlHelp)
		return err
	}
	op := args[0]
	for _, arg := range args[1:] {
		if arg == "--help" || arg == "-h" {
			_, err := io.WriteString(stdout, xmppdCtlHelp)
			return err
		}
	}
	flags := flag.NewFlagSet("xmppd ctl "+op, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	stateDir := ""
	peer := ""
	room := false
	flags.StringVar(&stateDir, "state-dir", "", "xmppd state directory")
	if op == "send" {
		flags.StringVar(&peer, "peer", "", "normalized peer bare JID")
		flags.BoolVar(&room, "room", false, "send to configured room")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || stateDir == "" {
		return errors.New("--state-dir is required and positional arguments are not accepted")
	}
	requestID, err := newUUID()
	if err != nil {
		return err
	}
	request := map[string]any{"version": controlProtocolVersion, "requestId": requestID, "op": op}
	if op == "send" {
		if room == (peer != "") {
			return errors.New("send requires exactly one of --room or --peer")
		}
		body, err := io.ReadAll(io.LimitReader(stdin, maximumControlBody+1))
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return errors.New("send body must be provided on stdin")
		}
		if len(body) > maximumControlBody {
			return errors.New("send body exceeds 16384 UTF-8 bytes")
		}
		if !utf8.Valid(body) {
			return errors.New("send body must be valid UTF-8")
		}
		request["body"] = string(body)
		if room {
			request["target"] = map[string]any{"kind": "room"}
		} else {
			request["target"] = map[string]any{"kind": "peer", "jid": peer}
		}
	} else if op != "list" {
		return fmt.Errorf("unknown ctl operation %q", op)
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maximumRequestFrame {
		return errors.New("control request exceeds 65536 bytes")
	}
	connection, err := net.DialTimeout("unix", filepath.Join(stateDir, "control.sock"), 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect control socket: %w", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := connection.Write(data); err != nil {
		return err
	}
	frame, err := readNDJSONFrame(connection, maximumResponseFrame)
	if err != nil {
		return err
	}
	if !utf8.Valid(frame) {
		return errors.New("control socket returned invalid UTF-8")
	}
	var response controlResponse
	if err := decodeStrictJSON(frame, &response); err != nil {
		return errors.New("control socket returned an invalid response")
	}
	if response.Version != controlProtocolVersion || response.RequestID != requestID {
		return errors.New("control socket returned a mismatched response")
	}
	if !response.OK {
		if response.Error == nil {
			return errors.New("control socket returned an invalid error")
		}
		return fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}
	switch op {
	case "list":
		encoded, _ := json.Marshal(response.Result)
		var result ControlListResult
		if err := decodeStrictJSON(encoded, &result); err != nil {
			return errors.New("control socket returned an invalid list result")
		}
		if _, err := fmt.Fprintf(stdout, "room\t%s\t%s\t%s\n", result.Room.State, result.Room.JID, result.Room.Nickname); err != nil {
			return err
		}
		for _, item := range result.Peers {
			if _, err := fmt.Fprintf(stdout, "peer\t%s\t%s\t%s\n", item.JID, item.AgentID, item.Nickname); err != nil {
				return err
			}
		}
	case "send":
		encoded, _ := json.Marshal(response.Result)
		var result ControlSendResult
		if err := decodeStrictJSON(encoded, &result); err != nil {
			return errors.New("control socket returned an invalid send result")
		}
		_, err = fmt.Fprintf(stdout, "sent\t%s\t%s\t%d\n", result.MessageID, result.Correlation, result.Hop)
		return err
	}
	return nil
}

const xmppdCtlHelp = `USAGE
  xmppd ctl list --state-dir DIR
  xmppd ctl send --state-dir DIR (--room | --peer JID) < BODY

FLAGS
  --state-dir DIR  per-agent xmppd state directory (required)
  --room           send groupchat to the configured joined room
  --peer JID       send chat to a current admitted discovered peer

OUTPUT
  stdout: compact tab-separated room/peer rows or one sent-result row.
  stderr: failures only; message bodies and credentials are never printed.

EXAMPLES
  xmppd ctl list --state-dir "$XMPPD_STATE_DIR"
  printf '%s' 'agent-0123...: hello' | xmppd ctl send --state-dir "$XMPPD_STATE_DIR" --room
`
