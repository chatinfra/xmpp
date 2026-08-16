package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	statusSchemaVersion = 2
	statusStaleAfter    = 90 * time.Second
)

var (
	// The managed builder sets this with -ldflags "-X main.buildSourceDigest=<sha256>".
	buildSourceDigest = ""
	binaryDigest      = computeBinaryDigest
)

type SessionsFile struct {
	Version  int                     `json:"version,omitempty"`
	Sessions map[string]SessionEntry `json:"sessions"`
}

type SessionEntry struct {
	ID        string `json:"id"`
	Directory string `json:"directory,omitempty"`
}

func (e *SessionEntry) UnmarshalJSON(data []byte) error {
	var id string
	if err := json.Unmarshal(data, &id); err == nil {
		e.ID = id
		e.Directory = ""
		return nil
	}
	var entry struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return err
	}
	if entry.ID == "" {
		entry.ID = entry.SessionID
	}
	e.ID = entry.ID
	e.Directory = entry.Directory
	return nil
}

type StatusFile struct {
	SchemaVersion       int        `json:"schemaVersion"`
	ProcessInvocationID string     `json:"processInvocationId"`
	BuildSourceDigest   string     `json:"buildSourceDigest"`
	BuildBinaryDigest   string     `json:"buildBinaryDigest"`
	XMPPConnected       bool       `json:"xmppConnected"`
	RoomState           string     `json:"roomState"`
	RoomJID             string     `json:"roomJid"`
	RoomNickname        string     `json:"roomNickname"`
	PeerCount           int        `json:"peerCount"`
	PeersUpdatedAt      *time.Time `json:"peersUpdatedAt"`
	AdmissionGeneration *string    `json:"admissionGeneration"`
	GateGeneration      *string    `json:"gateGeneration"`
	GateEvidenceDigest  *string    `json:"gateEvidenceDigest"`
	AdmissionExpiresAt  *time.Time `json:"admissionExpiresAt"`
	LastInboundAt       *time.Time `json:"lastInboundAt"`
	LastReplyAt         *time.Time `json:"lastReplyAt"`
	LastErrorCode       *string    `json:"lastErrorCode"`
	LastError           *string    `json:"lastError"`
	ActiveSessionCount  int        `json:"activeSessionCount"`
	StartedAt           time.Time  `json:"startedAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
}

type StateStore struct {
	dir string
}

func NewStateStore(dir string) *StateStore { return &StateStore{dir: dir} }

func (s *StateStore) LoadSessions() (map[string]SessionEntry, error) {
	path := filepath.Join(s.dir, "sessions.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]SessionEntry{}, nil
		}
		return nil, err
	}
	var file SessionsFile
	if err := json.Unmarshal(data, &file); err == nil && file.Sessions != nil {
		return file.Sessions, nil
	}
	var legacy map[string]SessionEntry
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, err
	}
	if legacy == nil {
		legacy = map[string]SessionEntry{}
	}
	return legacy, nil
}

func (s *StateStore) SaveSessions(sessions map[string]SessionEntry) error {
	copyMap := make(map[string]SessionEntry, len(sessions))
	for key, value := range sessions {
		copyMap[key] = value
	}
	return s.writeJSON("sessions.json", SessionsFile{Version: 2, Sessions: copyMap}, 0o600)
}

func (s *StateStore) SaveStatus(status StatusFile) error {
	return s.writeJSON("status.json", status, 0o600)
}

func (s *StateStore) writeJSON(name string, value any, perm os.FileMode) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", s.dir, err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(s.dir, name)
	tmp, err := os.CreateTemp(s.dir, "."+name+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if err := os.Chmod(path, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

func newStatusFile(cfg Config) (StatusFile, error) {
	digest, err := binaryDigest()
	if err != nil {
		return StatusFile{}, fmt.Errorf("compute running binary digest: %w", err)
	}
	if !isSHA256(buildSourceDigest) {
		return StatusFile{}, errors.New("build source digest is not a lowercase SHA-256 value")
	}
	invocationID := strings.TrimSpace(os.Getenv("INVOCATION_ID"))
	if invocationID == "" {
		invocationID, err = newUUID()
		if err != nil {
			return StatusFile{}, fmt.Errorf("create process invocation id: %w", err)
		}
	}
	now := time.Now().UTC()
	return StatusFile{
		SchemaVersion:       statusSchemaVersion,
		ProcessInvocationID: invocationID,
		BuildSourceDigest:   buildSourceDigest,
		BuildBinaryDigest:   digest,
		RoomState:           "disabled",
		RoomJID:             cfg.RoomJID,
		RoomNickname:        cfg.RoomNickname,
		StartedAt:           now,
		UpdatedAt:           now,
	}, nil
}

func (s StatusFile) Validate(now time.Time) error {
	if s.SchemaVersion != statusSchemaVersion || strings.TrimSpace(s.ProcessInvocationID) == "" {
		return errors.New("invalid status schema or process invocation")
	}
	if !isSHA256(s.BuildSourceDigest) || !isSHA256(s.BuildBinaryDigest) {
		return errors.New("invalid status build digest")
	}
	switch s.RoomState {
	case "disabled", "pending", "joined", "failed":
	default:
		return fmt.Errorf("invalid room state %q", s.RoomState)
	}
	if s.RoomJID == "" || s.RoomNickname == "" || s.PeerCount < 0 || s.ActiveSessionCount < 0 || s.StartedAt.IsZero() || s.UpdatedAt.IsZero() {
		return errors.New("invalid required status field")
	}
	if (s.LastErrorCode == nil) != (s.LastError == nil) {
		return errors.New("status error code and message must both be null or non-null")
	}
	if s.LastErrorCode != nil {
		if !validStatusErrorCode(*s.LastErrorCode) || len([]byte(*s.LastError)) > 512 {
			return errors.New("invalid bounded status error")
		}
	}
	if s.GateEvidenceDigest != nil && !isSHA256(*s.GateEvidenceDigest) {
		return errors.New("invalid gate evidence digest")
	}
	for _, generation := range []*string{s.AdmissionGeneration, s.GateGeneration} {
		if generation != nil && !isCanonicalUUID(*generation) {
			return errors.New("invalid status generation")
		}
	}
	if !now.IsZero() && now.Sub(s.UpdatedAt) > statusStaleAfter {
		return errors.New("status is stale")
	}
	return nil
}

func validStatusErrorCode(code string) bool {
	switch code {
	case "account_authentication_failed", "xmpp_disconnected", "admission_missing", "admission_invalid", "admission_expired", "gate_closed", "gate_mismatch", "room_join_failed", "nickname_conflict", "opencode_failed", "opencode_timeout", "opencode_no_text", "local_send_failed":
		return true
	default:
		return false
	}
}

func truncateUTF8(value string, maximum int) string {
	data := []byte(value)
	if len(data) <= maximum {
		return value
	}
	data = data[:maximum]
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return string(data)
}

func computeBinaryDigest() (string, error) {
	file, err := os.Open("/proc/self/exe")
	if err != nil {
		path, pathErr := os.Executable()
		if pathErr != nil {
			return "", err
		}
		file, err = os.Open(path)
		if err != nil {
			return "", err
		}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("running executable is not a regular file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func newUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
