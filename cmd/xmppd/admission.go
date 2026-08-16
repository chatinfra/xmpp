package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	admissionSnapshotVersion = 1
	maximumAdmissionLease    = 15 * time.Second
	maximumAdmissionFileSize = 1 << 20
)

type AdmissionSnapshot struct {
	Version            int              `json:"version"`
	Generation         string           `json:"generation"`
	GateGeneration     string           `json:"gateGeneration"`
	GateEvidenceDigest string           `json:"gateEvidenceDigest"`
	GeneratedAt        time.Time        `json:"generatedAt"`
	ExpiresAt          time.Time        `json:"expiresAt"`
	TenantID           string           `json:"tenantId"`
	AgentID            string           `json:"agentId"`
	RoomJID            string           `json:"roomJid"`
	Users              []string         `json:"users"`
	Agents             []AdmissionAgent `json:"agents"`
}

type AdmissionAgent struct {
	AgentID  string `json:"agentId"`
	BareJID  string `json:"bareJid"`
	Nickname string `json:"nickname"`
}

type AdmissionCheck struct {
	Generation         string `json:"generation"`
	GateGeneration     string `json:"gateGeneration"`
	GateEvidenceDigest string `json:"gateEvidenceDigest"`
	TenantID           string `json:"tenantId"`
	AgentID            string `json:"agentId"`
	RoomJID            string `json:"roomJid"`
}

type AdmissionCheckResult struct {
	Version            int       `json:"version"`
	Allowed            bool      `json:"allowed"`
	DirectAllowed      bool      `json:"directAllowed"`
	RoomAllowed        bool      `json:"roomAllowed"`
	Generation         string    `json:"generation"`
	GateGeneration     string    `json:"gateGeneration"`
	GateEvidenceDigest string    `json:"gateEvidenceDigest"`
	ExpiresAt          time.Time `json:"expiresAt"`
}

type admissionChecker interface {
	Check(context.Context, AdmissionCheck) (AdmissionCheckResult, error)
}

type admissionSnapshotFetcher interface {
	Snapshot(context.Context) (AdmissionSnapshot, error)
}

type HTTPAdmissionChecker struct {
	endpoint string
	token    string
	client   *http.Client
}

func NewHTTPAdmissionChecker(cfg Config) *HTTPAdmissionChecker {
	base := strings.TrimRight(cfg.InternalAPIBaseURL, "/")
	endpoint := base + "/internal/v1/agent-runtime-communication/agents/" + url.PathEscape(cfg.AgentID) + "/admission"
	return &HTTPAdmissionChecker{
		endpoint: endpoint,
		token:    cfg.InternalAPIToken,
		client:   &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *HTTPAdmissionChecker) Check(ctx context.Context, check AdmissionCheck) (AdmissionCheckResult, error) {
	body, err := json.Marshal(check)
	if err != nil {
		return AdmissionCheckResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return AdmissionCheckResult{}, errors.New("create admission check request")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return AdmissionCheckResult{}, errors.New("admission authority unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return AdmissionCheckResult{}, fmt.Errorf("admission authority rejected request with status %d", response.StatusCode)
	}
	limited := io.LimitReader(response.Body, 64*1024+1)
	data, err := io.ReadAll(limited)
	if err != nil || len(data) > 64*1024 {
		return AdmissionCheckResult{}, errors.New("invalid admission authority response")
	}
	var result AdmissionCheckResult
	if err := decodeStrictJSON(data, &result); err != nil {
		return AdmissionCheckResult{}, errors.New("invalid admission authority response")
	}
	return result, nil
}

func (c *HTTPAdmissionChecker) Snapshot(ctx context.Context) (AdmissionSnapshot, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return AdmissionSnapshot{}, errors.New("create admission snapshot request")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return AdmissionSnapshot{}, errors.New("admission authority unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return AdmissionSnapshot{}, fmt.Errorf("admission authority rejected snapshot request with status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumAdmissionFileSize+1))
	if err != nil || len(data) > maximumAdmissionFileSize {
		return AdmissionSnapshot{}, errors.New("invalid admission authority snapshot")
	}
	var snapshot AdmissionSnapshot
	if err := decodeStrictJSON(data, &snapshot); err != nil {
		return AdmissionSnapshot{}, errors.New("invalid admission authority snapshot")
	}
	return snapshot, nil
}

type AdmissionLease struct {
	Snapshot      AdmissionSnapshot
	DirectAllowed bool
	RoomAllowed   bool
	ExpiresAt     time.Time
}

type AdmissionAuthority struct {
	cfg     Config
	checker admissionChecker
	now     func() time.Time

	acquireMu sync.Mutex
	mu        sync.Mutex
	lease     *AdmissionLease
}

func NewAdmissionAuthority(cfg Config, checker admissionChecker) *AdmissionAuthority {
	return &AdmissionAuthority{cfg: cfg, checker: checker, now: func() time.Time { return time.Now().UTC() }}
}

func (a *AdmissionAuthority) Acquire(ctx context.Context, requireRoom bool) (AdmissionLease, error) {
	a.acquireMu.Lock()
	defer a.acquireMu.Unlock()
	now := a.now().UTC()
	a.mu.Lock()
	if a.lease != nil && now.Before(a.lease.ExpiresAt) {
		lease := *a.lease
		a.mu.Unlock()
		if !lease.DirectAllowed {
			return AdmissionLease{}, newAdmissionError("admission_invalid", "direct admission is not current")
		}
		if requireRoom && !lease.RoomAllowed {
			return lease, newAdmissionError("gate_closed", "room-production gate is closed")
		}
		return lease, nil
	}
	a.lease = nil
	a.mu.Unlock()

	var snapshot AdmissionSnapshot
	var err error
	fetchedSnapshot := false
	if fetcher, ok := a.checker.(admissionSnapshotFetcher); ok {
		fetchedSnapshot = true
		snapshot, err = fetcher.Snapshot(ctx)
		if err == nil {
			err = validateAdmissionSnapshot(snapshot, a.cfg, now)
		}
		if err == nil {
			err = writeAdmissionSnapshotAtomic(a.cfg.AdmissionPath, snapshot)
		}
	} else {
		snapshot, err = loadAdmissionSnapshot(a.cfg.AdmissionPath)
	}
	if err != nil {
		if fetchedSnapshot {
			return AdmissionLease{}, newAdmissionError("admission_invalid", err.Error())
		}
		return AdmissionLease{}, err
	}
	if err := validateAdmissionSnapshot(snapshot, a.cfg, now); err != nil {
		return AdmissionLease{}, err
	}
	result, err := a.checker.Check(ctx, AdmissionCheck{
		Generation:         snapshot.Generation,
		GateGeneration:     snapshot.GateGeneration,
		GateEvidenceDigest: snapshot.GateEvidenceDigest,
		TenantID:           snapshot.TenantID,
		AgentID:            snapshot.AgentID,
		RoomJID:            snapshot.RoomJID,
	})
	if err != nil {
		return AdmissionLease{}, newAdmissionError("admission_invalid", err.Error())
	}
	if result.Version != admissionSnapshotVersion ||
		result.Generation != snapshot.Generation ||
		result.GateGeneration != snapshot.GateGeneration ||
		result.GateEvidenceDigest != snapshot.GateEvidenceDigest {
		return AdmissionLease{}, newAdmissionError("gate_mismatch", "admission authority generation or digest mismatch")
	}
	if !result.Allowed || !result.DirectAllowed {
		return AdmissionLease{}, newAdmissionError("admission_invalid", "admission authority denied direct operation")
	}
	leaseNow := a.now().UTC()
	expiresAt := earliestTime(snapshot.ExpiresAt, result.ExpiresAt, leaseNow.Add(maximumAdmissionLease))
	if !expiresAt.After(leaseNow) {
		return AdmissionLease{}, newAdmissionError("admission_expired", "admission authority lease is expired")
	}
	lease := AdmissionLease{
		Snapshot:      snapshot,
		DirectAllowed: result.DirectAllowed,
		RoomAllowed:   result.RoomAllowed,
		ExpiresAt:     expiresAt,
	}
	a.mu.Lock()
	a.lease = &lease
	a.mu.Unlock()
	if requireRoom && !lease.RoomAllowed {
		return lease, newAdmissionError("gate_closed", "room-production gate is closed")
	}
	return lease, nil
}

func writeAdmissionSnapshotAtomic(path string, snapshot AdmissionSnapshot) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("admission state directory cannot be created")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return errors.New("admission state directory cannot be protected")
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return errors.New("admission snapshot cannot be encoded")
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".admission.json.*.tmp")
	if err != nil {
		return errors.New("admission snapshot temporary file cannot be created")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return errors.New("admission snapshot temporary file cannot be protected")
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return errors.New("admission snapshot temporary file cannot be written")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return errors.New("admission snapshot temporary file cannot be synchronized")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("admission snapshot temporary file cannot be closed")
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return errors.New("admission snapshot cannot be published")
	}
	return nil
}

func (a *AdmissionAuthority) Invalidate() {
	a.acquireMu.Lock()
	defer a.acquireMu.Unlock()
	a.mu.Lock()
	a.lease = nil
	a.mu.Unlock()
}

type admissionError struct {
	Code    string
	Message string
}

func (e *admissionError) Error() string { return e.Message }

func newAdmissionError(code, message string) error {
	return &admissionError{Code: code, Message: truncateUTF8(strings.TrimSpace(message), 512)}
}

func admissionErrorCode(err error) string {
	var typed *admissionError
	if errors.As(err, &typed) {
		return typed.Code
	}
	return "admission_invalid"
}

func loadAdmissionSnapshot(path string) (AdmissionSnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AdmissionSnapshot{}, newAdmissionError("admission_missing", "admission snapshot is missing")
		}
		return AdmissionSnapshot{}, newAdmissionError("admission_invalid", "admission snapshot cannot be read")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumAdmissionFileSize+1))
	if err != nil || len(data) > maximumAdmissionFileSize {
		return AdmissionSnapshot{}, newAdmissionError("admission_invalid", "admission snapshot is invalid")
	}
	var snapshot AdmissionSnapshot
	if err := decodeStrictJSON(data, &snapshot); err != nil {
		return AdmissionSnapshot{}, newAdmissionError("admission_invalid", "admission snapshot is malformed")
	}
	return snapshot, nil
}

func validateAdmissionSnapshot(snapshot AdmissionSnapshot, cfg Config, now time.Time) error {
	if snapshot.Version != admissionSnapshotVersion ||
		!isCanonicalUUID(snapshot.Generation) ||
		!isCanonicalUUID(snapshot.GateGeneration) ||
		!isSHA256(snapshot.GateEvidenceDigest) ||
		!isCanonicalUUID(snapshot.TenantID) ||
		!isCanonicalUUID(snapshot.AgentID) {
		return newAdmissionError("admission_invalid", "admission snapshot identity fields are invalid")
	}
	if snapshot.TenantID != cfg.TenantID || snapshot.AgentID != cfg.AgentID || snapshot.RoomJID != normalizeBareJID(snapshot.RoomJID) || snapshot.RoomJID != cfg.RoomJID {
		return newAdmissionError("gate_mismatch", "admission snapshot does not match the configured agent room")
	}
	if snapshot.GeneratedAt.IsZero() || snapshot.ExpiresAt.IsZero() ||
		!snapshot.ExpiresAt.After(snapshot.GeneratedAt) ||
		snapshot.ExpiresAt.Sub(snapshot.GeneratedAt) > maximumAdmissionLease {
		return newAdmissionError("admission_invalid", "admission snapshot lease bound is invalid")
	}
	if !snapshot.ExpiresAt.After(now) {
		return newAdmissionError("admission_expired", "admission snapshot is expired")
	}
	if err := validateSortedUsers(snapshot.Users); err != nil {
		return err
	}
	if err := validateSortedAgents(snapshot.Agents); err != nil {
		return err
	}
	return nil
}

func validateSortedUsers(users []string) error {
	previous := ""
	for _, user := range users {
		normalized := normalizeBareJID(user)
		if normalized == "" || normalized != user || (previous != "" && normalized <= previous) {
			return newAdmissionError("admission_invalid", "admission users must be sorted unique normalized bare JIDs")
		}
		previous = normalized
	}
	return nil
}

func validateSortedAgents(agents []AdmissionAgent) error {
	previous := ""
	seenIDs := make(map[string]struct{}, len(agents))
	for _, agent := range agents {
		if !isCanonicalUUID(agent.AgentID) || normalizeBareJID(agent.BareJID) != agent.BareJID ||
			agent.Nickname != expectedRoomNickname(agent.AgentID) ||
			(previous != "" && agent.BareJID <= previous) {
			return newAdmissionError("admission_invalid", "admission agents must be sorted unique canonical identities")
		}
		if _, duplicate := seenIDs[agent.AgentID]; duplicate {
			return newAdmissionError("admission_invalid", "admission agents contain a duplicate agent ID")
		}
		seenIDs[agent.AgentID] = struct{}{}
		previous = agent.BareJID
	}
	return nil
}

func (s AdmissionSnapshot) userSet() map[string]struct{} {
	result := make(map[string]struct{}, len(s.Users))
	for _, user := range s.Users {
		result[user] = struct{}{}
	}
	return result
}

func (s AdmissionSnapshot) admitsUser(jid string, _ bool) bool {
	users := s.userSet()
	_, admitted := users[jid]
	return admitted
}

func (s AdmissionSnapshot) agentByJID() map[string]AdmissionAgent {
	result := make(map[string]AdmissionAgent, len(s.Agents))
	for _, agent := range s.Agents {
		result[agent.BareJID] = agent
	}
	return result
}

func decodeStrictJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

func isSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func earliestTime(values ...time.Time) time.Time {
	sort.Slice(values, func(i, j int) bool { return values[i].Before(values[j]) })
	return values[0]
}
