package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/chatinfra/xmpp/internal/opencode"
	"github.com/chatinfra/xmpp/internal/xmpp"
)

var (
	sessionRetryInitialDelay       = time.Second
	sessionRetryMaxElapsed         = 2 * time.Minute
	chatStateComposingRefreshEvery = 20 * time.Second
	statusHeartbeatInterval        = 30 * time.Second
	authorityRefreshInterval       = time.Second
	errReplyNotCurrent             = errors.New("reply admission is not current")
)

type xmppConn interface {
	Connect(context.Context) error
	SendPresence() error
	JoinMUC(roomJID, nickname string) error
	StreamEvents(context.Context, func(xmpp.Event) error) error
	SendAgentMessage(to, messageType, body string, metadata xmpp.AgentMessageMetadata) (string, error)
	SendChatState(to string, state xmpp.ChatState) error
	Close() error
}

type opencodeClient interface {
	CreateSession(context.Context) (opencode.Session, error)
	Prompt(context.Context, string, string) (opencode.AssistantResponse, error)
}

type Peer struct {
	AgentID  string `json:"agentId"`
	JID      string `json:"jid"`
	Nickname string `json:"nickname"`
}

type roomOccupant struct {
	JID         string
	Nickname    string
	Affiliation string
}

type Bridge struct {
	cfg       Config
	logger    *log.Logger
	xmpp      xmppConn
	opencode  opencodeClient
	store     *StateStore
	authority *AdmissionAuthority
	initErr   error

	mu               sync.Mutex
	sessions         map[string]SessionEntry
	workers          map[string]*sessionWorker
	status           StatusFile
	lease            *AdmissionLease
	occupants        map[string]roomOccupant
	peers            map[string]Peer
	seen             map[string]struct{}
	joinRequested    bool
	selfPresent      bool
	roomRetryBlocked bool
	runCtx           context.Context
	wg               sync.WaitGroup
	backgroundWG     sync.WaitGroup
	statusWriteMu    sync.Mutex
	sessionWriteMu   sync.Mutex
}

type sessionWorker struct {
	key string
	ch  chan inboundMessage
}

type inboundMessage struct {
	SessionKey       string
	SenderJID        string
	SenderNickname   string
	AgentSender      bool
	Room             bool
	FullJID          string
	ReplyTo          string
	ReplyType        string
	ReplyAddress     string
	Body             string
	ReplyMetadata    xmpp.AgentMessageMetadata
	PublishChatState bool
}

func NewBridge(cfg Config, logger *log.Logger) (*Bridge, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	xmppClient, err := xmpp.New(cfg.XMPP)
	if err != nil {
		return nil, err
	}
	opencodeClient, err := opencode.New(opencode.Config{
		BaseURL:       cfg.OpencodeBaseURL,
		Directory:     cfg.OpencodeDirectory,
		Agent:         cfg.AgentName,
		PromptTimeout: cfg.PromptTimeout,
	})
	if err != nil {
		return nil, err
	}
	bridge := NewBridgeWithClients(cfg, logger, xmppClient, opencodeClient)
	if bridge.initErr != nil {
		return nil, bridge.initErr
	}
	return bridge, nil
}

func NewBridgeWithClients(cfg Config, logger *log.Logger, xmppClient xmppConn, opencodeClient opencodeClient) *Bridge {
	if logger == nil {
		logger = log.Default()
	}
	status, err := newStatusFile(cfg)
	bridge := &Bridge{
		cfg:       cfg,
		logger:    logger,
		xmpp:      xmppClient,
		opencode:  opencodeClient,
		store:     NewStateStore(cfg.StateDir),
		workers:   map[string]*sessionWorker{},
		occupants: map[string]roomOccupant{},
		peers:     map[string]Peer{},
		seen:      map[string]struct{}{},
		status:    status,
		initErr:   err,
	}
	bridge.authority = NewAdmissionAuthority(cfg, NewHTTPAdmissionChecker(cfg))
	return bridge
}

func (b *Bridge) Run(ctx context.Context) error {
	if b.initErr != nil {
		return b.initErr
	}
	if err := b.cfg.Validate(); err != nil {
		return err
	}
	sessions, err := b.store.LoadSessions()
	if err != nil {
		return fmt.Errorf("load sessions: %w", err)
	}
	b.mu.Lock()
	b.runCtx = ctx
	b.sessions = sessions
	b.status.ActiveSessionCount = len(sessions)
	b.mu.Unlock()
	b.flushStatus()

	lease, err := b.refreshAdmission(ctx, false)
	if err != nil {
		b.recordError(admissionErrorCode(err), err.Error())
		return err
	}
	if err := b.xmpp.Connect(ctx); err != nil {
		b.recordError("account_authentication_failed", "XMPP account authentication failed")
		return err
	}
	b.setConnected(true)
	backgroundCtx, cancelBackground := context.WithCancel(ctx)
	b.startBackground(backgroundCtx)
	defer func() {
		cancelBackground()
		b.backgroundWG.Wait()
		b.shutdownWorkers()
		b.setConnected(false)
		_ = b.xmpp.Close()
		_ = b.flushSessions()
		b.flushStatus()
	}()
	if err := b.xmpp.SendPresence(); err != nil {
		b.recordError("xmpp_disconnected", "failed to publish XMPP presence")
		return err
	}
	if b.cfg.RoomEnabled && lease.RoomAllowed {
		if err := b.requestRoomJoin(); err != nil {
			return err
		}
	}
	control, err := StartControlServer(backgroundCtx, b.cfg.StateDir, b)
	if err != nil {
		b.recordError("local_send_failed", "local control socket could not start")
		return err
	}
	defer control.Close()

	b.logger.Printf("connected jid=%s agent_id=%s agent_name=%s", b.cfg.XMPP.JID, b.cfg.AgentID, b.cfg.AgentName)
	err = b.xmpp.StreamEvents(ctx, b.handleEvent)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		b.recordError("xmpp_disconnected", "XMPP connection ended")
		return err
	}
	return nil
}

func (b *Bridge) startBackground(ctx context.Context) {
	b.backgroundWG.Add(2)
	go func() {
		defer b.backgroundWG.Done()
		ticker := time.NewTicker(statusHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.flushStatus()
			}
		}
	}()
	go func() {
		defer b.backgroundWG.Done()
		ticker := time.NewTicker(authorityRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				lease, err := b.refreshAdmission(ctx, false)
				if err != nil {
					b.expireAdmission(admissionErrorCode(err), err.Error())
					continue
				}
				if b.cfg.RoomEnabled && lease.RoomAllowed {
					_ = b.requestRoomJoin()
				}
			}
		}
	}()
}

func (b *Bridge) handleEvent(event xmpp.Event) error {
	if event.Presence != nil {
		b.handlePresence(*event.Presence)
	}
	if event.Message != nil {
		return b.enqueueMessage(event.Message)
	}
	return nil
}

func (b *Bridge) handlePresence(presence xmpp.OccupantPresence) {
	if !b.cfg.RoomEnabled || normalizeBareJID(presence.RoomJID) != b.cfg.RoomJID || presence.Nickname == "" {
		return
	}
	lease, err := b.refreshAdmission(b.context(), true)
	if err != nil {
		b.expireAdmission(admissionErrorCode(err), err.Error())
		return
	}
	realJID := normalizeBareJID(presence.RealJID)
	selfJID := normalizeBareJID(b.cfg.XMPP.JID)
	if presence.NicknameConflict {
		b.failRoom("nickname_conflict", "configured room nickname was rejected")
		return
	}
	if presence.Type == "error" {
		if presence.Nickname == b.cfg.RoomNickname {
			b.failRoom("room_join_failed", "configured room join was rejected")
		} else {
			b.removeOccupant(presence.Nickname)
		}
		return
	}
	if presence.IsUnavailable() {
		if presence.Nickname == b.cfg.RoomNickname {
			b.setRoomPending()
		} else {
			b.removeOccupant(presence.Nickname)
		}
		return
	}
	if presence.Nickname == b.cfg.RoomNickname && realJID != selfJID {
		b.failRoom("nickname_conflict", "configured room nickname is occupied by another account")
		return
	}
	if realJID == "" || presence.Affiliation == "none" {
		b.removeOccupant(presence.Nickname)
		return
	}
	users := lease.Snapshot.userSet()
	agents := lease.Snapshot.agentByJID()
	_, admittedUser := users[realJID]
	agent, admittedAgent := agents[realJID]
	if !admittedUser && !admittedAgent {
		b.removeOccupant(presence.Nickname)
		return
	}
	if admittedAgent && agent.Nickname != presence.Nickname {
		b.removeOccupant(presence.Nickname)
		return
	}
	b.mu.Lock()
	b.occupants[presence.Nickname] = roomOccupant{JID: realJID, Nickname: presence.Nickname, Affiliation: presence.Affiliation}
	b.mu.Unlock()
	if realJID == selfJID && presence.Nickname == b.cfg.RoomNickname && (presence.Self || admittedAgent) {
		b.mu.Lock()
		b.selfPresent = true
		b.status.RoomState = "joined"
		b.clearErrorLocked("room_join_failed", "nickname_conflict", "gate_closed", "gate_mismatch")
		b.mu.Unlock()
		b.flushStatus()
	}
	b.rebuildPeers(lease)
}

func (b *Bridge) enqueueMessage(message *xmpp.Message) error {
	if message == nil || strings.TrimSpace(message.Body) == "" || message.Delay != nil {
		return nil
	}
	var inbound inboundMessage
	var ok bool
	if message.Type == xmpp.GroupchatMessageType {
		inbound, ok = b.admitRoomMessage(message)
	} else if message.Type == xmpp.DirectChatMessageType {
		inbound, ok = b.admitDirectMessage(message)
	}
	if !ok {
		return nil
	}
	now := time.Now().UTC()
	b.mu.Lock()
	b.status.LastInboundAt = &now
	b.mu.Unlock()
	b.flushStatus()
	worker := b.workerFor(inbound.SessionKey)
	select {
	case worker.ch <- inbound:
		return nil
	case <-b.context().Done():
		return b.context().Err()
	}
}

func (b *Bridge) admitDirectMessage(message *xmpp.Message) (inboundMessage, bool) {
	lease, err := b.refreshAdmission(b.context(), false)
	if err != nil {
		return inboundMessage{}, false
	}
	bare := normalizeBareJID(message.From)
	if bare == "" || bare == normalizeBareJID(b.cfg.XMPP.JID) {
		return inboundMessage{}, false
	}
	users := lease.Snapshot.userSet()
	agents := lease.Snapshot.agentByJID()
	_, isUser := users[bare]
	agent, isAgent := agents[bare]
	if !isUser && !isAgent {
		return inboundMessage{}, false
	}
	if isAgent && !b.isCurrentPeer(bare) {
		return inboundMessage{}, false
	}
	metadata, ok := b.replyMetadata(message, isAgent, agent.AgentID)
	if !ok {
		return inboundMessage{}, false
	}
	return inboundMessage{
		SessionKey:       bare,
		SenderJID:        bare,
		AgentSender:      isAgent,
		FullJID:          message.From,
		ReplyTo:          message.From,
		ReplyType:        xmpp.DirectChatMessageType,
		Body:             message.Body,
		ReplyMetadata:    metadata,
		PublishChatState: true,
	}, true
}

func (b *Bridge) admitRoomMessage(message *xmpp.Message) (inboundMessage, bool) {
	lease, err := b.refreshAdmission(b.context(), true)
	if err != nil || normalizeBareJID(message.From) != b.cfg.RoomJID {
		return inboundMessage{}, false
	}
	_, nickname, found := strings.Cut(message.From, "/")
	if !found || nickname == "" || nickname == b.cfg.RoomNickname {
		return inboundMessage{}, false
	}
	b.mu.Lock()
	occupant, present := b.occupants[nickname]
	b.mu.Unlock()
	if !present || occupant.JID == normalizeBareJID(b.cfg.XMPP.JID) {
		return inboundMessage{}, false
	}
	token := b.cfg.RoomNickname + ":"
	if !strings.HasPrefix(message.Body, token) {
		return inboundMessage{}, false
	}
	body := strings.TrimPrefix(message.Body, token)
	body = strings.TrimPrefix(body, " ")
	if strings.TrimSpace(body) == "" {
		return inboundMessage{}, false
	}
	users := lease.Snapshot.userSet()
	agents := lease.Snapshot.agentByJID()
	_, isUser := users[occupant.JID]
	agent, isAgent := agents[occupant.JID]
	if !isUser && !isAgent {
		return inboundMessage{}, false
	}
	if isAgent && !b.isCurrentPeer(occupant.JID) {
		return inboundMessage{}, false
	}
	metadata, ok := b.replyMetadata(message, isAgent, agent.AgentID)
	if !ok {
		return inboundMessage{}, false
	}
	replyAddress := ""
	if isAgent {
		replyAddress = nickname + ": "
	}
	return inboundMessage{
		SessionKey:     "room:" + b.cfg.RoomJID,
		SenderJID:      occupant.JID,
		SenderNickname: nickname,
		AgentSender:    isAgent,
		Room:           true,
		FullJID:        message.From,
		ReplyTo:        b.cfg.RoomJID,
		ReplyType:      xmpp.GroupchatMessageType,
		ReplyAddress:   replyAddress,
		Body:           body,
		ReplyMetadata:  metadata,
	}, true
}

func (b *Bridge) replyMetadata(message *xmpp.Message, agentSender bool, senderAgentID string) (xmpp.AgentMessageMetadata, bool) {
	if agentSender {
		if message.MetadataErr != "" || message.AgentMessage == nil || message.AgentMessage.Hop != 0 ||
			message.AgentMessage.OriginAgentID != senderAgentID || message.AgentMessage.OriginAgentID == b.cfg.AgentID {
			return xmpp.AgentMessageMetadata{}, false
		}
		if err := message.AgentMessage.Validate(); err != nil {
			return xmpp.AgentMessageMetadata{}, false
		}
		key := correlationKey(*message.AgentMessage)
		b.mu.Lock()
		if _, duplicate := b.seen[key]; duplicate {
			b.mu.Unlock()
			return xmpp.AgentMessageMetadata{}, false
		}
		b.seen[key] = struct{}{}
		b.mu.Unlock()
		return xmpp.AgentMessageMetadata{
			Correlation:   message.AgentMessage.Correlation,
			OriginAgentID: message.AgentMessage.OriginAgentID,
			Hop:           1,
		}, true
	}
	correlation, err := newUUID()
	if err != nil {
		b.recordError("local_send_failed", "could not create message correlation")
		return xmpp.AgentMessageMetadata{}, false
	}
	return xmpp.AgentMessageMetadata{Correlation: correlation, OriginAgentID: b.cfg.AgentID, Hop: 0}, true
}

func correlationKey(metadata xmpp.AgentMessageMetadata) string {
	return fmt.Sprintf("%s\x00%s\x00%d", metadata.Correlation, metadata.OriginAgentID, metadata.Hop)
}

func (b *Bridge) workerFor(key string) *sessionWorker {
	b.mu.Lock()
	defer b.mu.Unlock()
	if worker := b.workers[key]; worker != nil {
		return worker
	}
	worker := &sessionWorker{key: key, ch: make(chan inboundMessage, 64)}
	b.workers[key] = worker
	b.wg.Add(1)
	ctx := b.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go b.runWorker(ctx, worker)
	return worker
}

func (b *Bridge) runWorker(ctx context.Context, worker *sessionWorker) {
	defer b.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case message, open := <-worker.ch:
			if !open {
				return
			}
			b.processMessage(ctx, message)
		}
	}
}

func (b *Bridge) processMessage(ctx context.Context, message inboundMessage) {
	lease, admitted := b.inboundAdmission(ctx, message)
	if !admitted {
		return
	}
	leaseExpiresAt := lease.ExpiresAt
	if message.PublishChatState {
		b.sendChatState(message.FullJID, xmpp.ChatStateActive)
		stopComposing := b.startComposingLoop(ctx, message)
		defer func() {
			stopComposing()
			if b.inboundStillAdmitted(ctx, message) {
				b.sendChatState(message.FullJID, xmpp.ChatStateActive)
			}
		}()
	}
	sessionID, err := b.sessionForWithRetry(ctx, message, leaseExpiresAt)
	if err != nil {
		if errors.Is(err, errReplyNotCurrent) {
			return
		}
		b.logOpencodeError(err)
		return
	}
	if !b.inboundStillAdmittedBefore(ctx, message, leaseExpiresAt) {
		return
	}
	response, err := b.opencode.Prompt(ctx, sessionID, message.Body)
	if opencode.IsSessionInvalid(err) {
		b.logger.Printf("recreating invalid opencode session key=%s", message.SessionKey)
		b.discardSession(message.SessionKey, sessionID)
		if !b.inboundStillAdmittedBefore(ctx, message, leaseExpiresAt) {
			return
		}
		sessionID, err = b.recreateSession(ctx, message.SessionKey)
		if err == nil {
			if !b.inboundStillAdmittedBefore(ctx, message, leaseExpiresAt) {
				return
			}
			response, err = b.opencode.Prompt(ctx, sessionID, message.Body)
			if opencode.IsSessionInvalid(err) {
				b.discardSession(message.SessionKey, sessionID)
			}
		}
	}
	if err != nil {
		b.logOpencodeError(err)
		return
	}
	if strings.TrimSpace(response.Text) == "" {
		b.recordError("opencode_no_text", "OpenCode completed without assistant text")
		return
	}
	body := message.ReplyAddress + response.Text
	if !b.inboundStillAdmitted(ctx, message) {
		return
	}
	if err := b.sendReplyIfCurrent(message, body); err != nil {
		if errors.Is(err, errReplyNotCurrent) {
			return
		}
		b.recordError("xmpp_disconnected", "failed to send XMPP reply")
		return
	}
	now := time.Now().UTC()
	b.mu.Lock()
	b.status.LastReplyAt = &now
	b.clearErrorLocked("opencode_failed", "opencode_timeout", "opencode_no_text", "xmpp_disconnected")
	b.mu.Unlock()
	b.flushStatus()
}

func (b *Bridge) sendReplyIfCurrent(message inboundMessage, body string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.status.XMPPConnected || b.lease == nil || !time.Now().UTC().Before(b.lease.ExpiresAt) {
		return errReplyNotCurrent
	}
	if message.AgentSender {
		if _, admitted := b.lease.Snapshot.agentByJID()[message.SenderJID]; !admitted {
			return errReplyNotCurrent
		}
		if _, current := b.peers[message.SenderJID]; !current {
			return errReplyNotCurrent
		}
	} else if !b.lease.Snapshot.admitsUser(message.SenderJID, message.Room) {
		return errReplyNotCurrent
	}
	if message.Room {
		occupant, present := b.occupants[message.SenderNickname]
		if !present || occupant.JID != message.SenderJID || b.status.RoomState != "joined" {
			return errReplyNotCurrent
		}
	}
	_, err := b.xmpp.SendAgentMessage(message.ReplyTo, message.ReplyType, body, message.ReplyMetadata)
	return err
}

func (b *Bridge) inboundAdmission(ctx context.Context, message inboundMessage) (AdmissionLease, bool) {
	lease, err := b.refreshAdmission(ctx, message.Room)
	if err != nil {
		b.expireAdmission(admissionErrorCode(err), err.Error())
		return AdmissionLease{}, false
	}
	if message.AgentSender {
		if _, admitted := lease.Snapshot.agentByJID()[message.SenderJID]; !admitted || !b.isCurrentPeer(message.SenderJID) {
			return AdmissionLease{}, false
		}
	} else if !lease.Snapshot.admitsUser(message.SenderJID, message.Room) {
		return AdmissionLease{}, false
	}
	if !message.Room {
		return lease, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	occupant, present := b.occupants[message.SenderNickname]
	return lease, present && occupant.JID == message.SenderJID && b.selfPresent && b.status.RoomState == "joined"
}

func (b *Bridge) inboundStillAdmitted(ctx context.Context, message inboundMessage) bool {
	_, admitted := b.inboundAdmission(ctx, message)
	return admitted
}

func (b *Bridge) inboundStillAdmittedBefore(ctx context.Context, message inboundMessage, expiresAt time.Time) bool {
	if !time.Now().UTC().Before(expiresAt) {
		return false
	}
	return b.inboundStillAdmitted(ctx, message)
}

func (b *Bridge) logOpencodeError(err error) {
	if err == nil {
		return
	}
	b.logger.Printf("opencode prompt failed")
	code := "opencode_failed"
	if errors.Is(err, context.DeadlineExceeded) {
		code = "opencode_timeout"
	}
	b.recordError(code, "OpenCode prompt failed")
}

func (b *Bridge) startComposingLoop(ctx context.Context, message inboundMessage) func() {
	b.sendChatState(message.FullJID, xmpp.ChatStateComposing)
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(chatStateComposingRefreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				if !b.inboundStillAdmitted(loopCtx, message) {
					return
				}
				b.sendChatState(message.FullJID, xmpp.ChatStateComposing)
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (b *Bridge) sendChatState(to string, state xmpp.ChatState) {
	if err := b.xmpp.SendChatState(to, state); err != nil {
		b.recordError("xmpp_disconnected", "failed to send XMPP chat state")
	}
}

func (b *Bridge) sessionFor(ctx context.Context, message inboundMessage, expiresAt time.Time) (string, error) {
	b.mu.Lock()
	entry := b.sessions[message.SessionKey]
	b.mu.Unlock()
	if entry.ID != "" && entry.Directory == b.cfg.OpencodeDirectory {
		return entry.ID, nil
	}
	if !b.inboundStillAdmittedBefore(ctx, message, expiresAt) {
		return "", errReplyNotCurrent
	}
	return b.recreateSession(ctx, message.SessionKey)
}

func (b *Bridge) sessionForWithRetry(ctx context.Context, message inboundMessage, expiresAt time.Time) (string, error) {
	retryCtx, cancel := context.WithDeadline(ctx, expiresAt)
	defer cancel()
	startedAt := time.Now()
	delay := sessionRetryInitialDelay
	attempt := 0
	for {
		attempt++
		if !b.inboundStillAdmittedBefore(retryCtx, message, expiresAt) {
			return "", errReplyNotCurrent
		}
		sessionID, err := b.sessionFor(retryCtx, message, expiresAt)
		if err == nil {
			return sessionID, nil
		}
		if !opencode.IsRetryable(err) || time.Since(startedAt) >= sessionRetryMaxElapsed {
			return "", err
		}
		b.logger.Printf("create session transient failure key=%s attempt=%d retry_in=%s", message.SessionKey, attempt, delay)
		b.recordError("opencode_failed", "OpenCode session creation failed")
		select {
		case <-retryCtx.Done():
			return "", errReplyNotCurrent
		case <-time.After(delay):
		}
		if delay < 5*time.Second {
			delay *= 2
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
		}
	}
}

func (b *Bridge) recreateSession(ctx context.Context, key string) (string, error) {
	session, err := b.opencode.CreateSession(ctx)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	if b.sessions == nil {
		b.sessions = map[string]SessionEntry{}
	}
	b.sessions[key] = SessionEntry{ID: session.ID, Directory: b.cfg.OpencodeDirectory}
	b.status.ActiveSessionCount = len(b.sessions)
	b.mu.Unlock()
	if err := b.flushSessions(); err != nil {
		return "", err
	}
	b.flushStatus()
	return session.ID, nil
}

func (b *Bridge) discardSession(key, sessionID string) {
	b.mu.Lock()
	removed := false
	if entry, exists := b.sessions[key]; exists && entry.ID == sessionID {
		delete(b.sessions, key)
		b.status.ActiveSessionCount = len(b.sessions)
		removed = true
	}
	b.mu.Unlock()
	if removed {
		_ = b.flushSessions()
		b.flushStatus()
	}
}

func (b *Bridge) requestRoomJoin() error {
	b.mu.Lock()
	if b.joinRequested || b.status.RoomState == "joined" || b.roomRetryBlocked {
		b.mu.Unlock()
		return nil
	}
	b.joinRequested = true
	b.clearRoomPresenceLocked(false)
	b.status.RoomState = "pending"
	b.mu.Unlock()
	b.flushStatus()
	if err := b.xmpp.JoinMUC(b.cfg.RoomJID, b.cfg.RoomNickname); err != nil {
		b.failRoom("room_join_failed", "failed to request room join")
		return err
	}
	return nil
}

func (b *Bridge) refreshAdmission(ctx context.Context, requireRoom bool) (AdmissionLease, error) {
	lease, err := b.authority.Acquire(ctx, requireRoom)
	if err != nil {
		if lease.Snapshot.Generation != "" {
			b.applyLease(lease)
		}
		return lease, err
	}
	b.applyLease(lease)
	return lease, nil
}

func (b *Bridge) applyLease(lease AdmissionLease) {
	b.mu.Lock()
	if b.lease != nil &&
		b.lease.Snapshot.Generation == lease.Snapshot.Generation &&
		b.lease.Snapshot.GateGeneration == lease.Snapshot.GateGeneration &&
		b.lease.Snapshot.GateEvidenceDigest == lease.Snapshot.GateEvidenceDigest &&
		b.lease.ExpiresAt.Equal(lease.ExpiresAt) &&
		b.lease.DirectAllowed == lease.DirectAllowed &&
		b.lease.RoomAllowed == lease.RoomAllowed {
		b.mu.Unlock()
		return
	}
	authorityChanged := b.lease == nil ||
		b.lease.Snapshot.Generation != lease.Snapshot.Generation ||
		b.lease.Snapshot.GateGeneration != lease.Snapshot.GateGeneration ||
		b.lease.Snapshot.GateEvidenceDigest != lease.Snapshot.GateEvidenceDigest
	b.lease = &lease
	b.status.AdmissionGeneration = stringPointer(lease.Snapshot.Generation)
	b.status.GateGeneration = stringPointer(lease.Snapshot.GateGeneration)
	b.status.GateEvidenceDigest = stringPointer(lease.Snapshot.GateEvidenceDigest)
	expires := lease.ExpiresAt.UTC()
	b.status.AdmissionExpiresAt = &expires
	b.clearErrorLocked("admission_missing", "admission_invalid", "admission_expired")
	if !lease.RoomAllowed {
		b.status.RoomState = "disabled"
		b.joinRequested = false
		b.roomRetryBlocked = false
		b.clearRoomPresenceLocked(true)
	} else {
		if authorityChanged {
			b.roomRetryBlocked = false
		}
		b.clearErrorLocked("gate_closed", "gate_mismatch")
	}
	b.mu.Unlock()
	if lease.RoomAllowed {
		b.rebuildPeers(lease)
	}
	b.flushStatus()
}

func (b *Bridge) expireAdmission(code, message string) {
	b.authority.Invalidate()
	b.mu.Lock()
	b.lease = nil
	b.status.AdmissionGeneration = nil
	b.status.GateGeneration = nil
	b.status.GateEvidenceDigest = nil
	b.status.AdmissionExpiresAt = nil
	b.status.RoomState = "disabled"
	b.joinRequested = false
	b.roomRetryBlocked = false
	b.clearRoomPresenceLocked(true)
	b.setErrorLocked(code, message)
	b.mu.Unlock()
	b.flushStatus()
}

func (b *Bridge) rebuildPeers(lease AdmissionLease) {
	if !lease.RoomAllowed {
		b.clearPeers()
		return
	}
	agents := lease.Snapshot.agentByJID()
	self := normalizeBareJID(b.cfg.XMPP.JID)
	b.mu.Lock()
	next := make(map[string]Peer)
	if b.selfPresent && b.status.RoomState == "joined" {
		for nickname, occupant := range b.occupants {
			agent, admitted := agents[occupant.JID]
			if admitted && occupant.JID != self && agent.Nickname == nickname && occupant.Affiliation != "none" {
				next[occupant.JID] = Peer{AgentID: agent.AgentID, JID: occupant.JID, Nickname: nickname}
			}
		}
	}
	changed := !samePeers(b.peers, next)
	b.peers = next
	if changed {
		now := time.Now().UTC()
		b.status.PeerCount = len(next)
		b.status.PeersUpdatedAt = &now
	}
	b.mu.Unlock()
	if changed {
		b.flushStatus()
	}
}

func (b *Bridge) clearPeers() {
	b.mu.Lock()
	changed := len(b.peers) > 0 || b.status.PeerCount != 0
	b.peers = map[string]Peer{}
	b.status.PeerCount = 0
	if changed {
		now := time.Now().UTC()
		b.status.PeersUpdatedAt = &now
	}
	b.mu.Unlock()
}

func (b *Bridge) resetPeers() {
	b.mu.Lock()
	b.peers = map[string]Peer{}
	b.status.PeerCount = 0
	now := time.Now().UTC()
	b.status.PeersUpdatedAt = &now
	b.mu.Unlock()
}

func (b *Bridge) clearRoomPresenceLocked(updateTimestamp bool) {
	b.occupants = map[string]roomOccupant{}
	b.peers = map[string]Peer{}
	b.selfPresent = false
	b.status.PeerCount = 0
	if updateTimestamp {
		now := time.Now().UTC()
		b.status.PeersUpdatedAt = &now
	}
}

func (b *Bridge) removeOccupant(nickname string) {
	b.mu.Lock()
	delete(b.occupants, nickname)
	lease := b.lease
	b.mu.Unlock()
	if lease != nil {
		b.rebuildPeers(*lease)
	}
}

func (b *Bridge) isCurrentPeer(jid string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, exists := b.peers[jid]
	return exists
}

func samePeers(left, right map[string]Peer) bool {
	if len(left) != len(right) {
		return false
	}
	for jid, peer := range left {
		if right[jid] != peer {
			return false
		}
	}
	return true
}

func (b *Bridge) failRoom(code, message string) {
	b.mu.Lock()
	b.status.RoomState = "failed"
	b.joinRequested = false
	b.roomRetryBlocked = true
	b.clearRoomPresenceLocked(true)
	b.setErrorLocked(code, message)
	b.mu.Unlock()
	b.flushStatus()
}

func (b *Bridge) setRoomPending() {
	b.mu.Lock()
	if b.status.RoomState != "disabled" {
		b.status.RoomState = "pending"
	}
	b.joinRequested = false
	b.roomRetryBlocked = false
	b.clearRoomPresenceLocked(true)
	b.mu.Unlock()
	b.flushStatus()
}

func (b *Bridge) shutdownWorkers() {
	b.mu.Lock()
	workers := make([]*sessionWorker, 0, len(b.workers))
	for _, worker := range b.workers {
		workers = append(workers, worker)
	}
	b.workers = map[string]*sessionWorker{}
	b.mu.Unlock()
	for _, worker := range workers {
		close(worker.ch)
	}
	b.wg.Wait()
}

func (b *Bridge) setConnected(connected bool) {
	b.mu.Lock()
	b.status.XMPPConnected = connected
	if !connected {
		if b.cfg.RoomEnabled && b.status.RoomState == "joined" {
			b.status.RoomState = "pending"
		}
		b.joinRequested = false
		b.clearRoomPresenceLocked(true)
	}
	b.mu.Unlock()
	b.flushStatus()
}

func (b *Bridge) recordError(code, message string) {
	b.mu.Lock()
	b.setErrorLocked(code, message)
	b.mu.Unlock()
	b.flushStatus()
}

func (b *Bridge) setErrorLocked(code, message string) {
	if !validStatusErrorCode(code) {
		code = "opencode_failed"
	}
	bounded := truncateUTF8(strings.TrimSpace(message), 512)
	b.status.LastErrorCode = stringPointer(code)
	b.status.LastError = stringPointer(bounded)
}

func (b *Bridge) clearErrorLocked(codes ...string) {
	if b.status.LastErrorCode == nil {
		return
	}
	for _, code := range codes {
		if *b.status.LastErrorCode == code {
			b.status.LastErrorCode = nil
			b.status.LastError = nil
			return
		}
	}
}

func (b *Bridge) flushSessions() error {
	b.sessionWriteMu.Lock()
	defer b.sessionWriteMu.Unlock()
	b.mu.Lock()
	sessions := make(map[string]SessionEntry, len(b.sessions))
	for key, value := range b.sessions {
		sessions[key] = value
	}
	b.mu.Unlock()
	return b.store.SaveSessions(sessions)
}

func (b *Bridge) flushStatus() {
	b.statusWriteMu.Lock()
	defer b.statusWriteMu.Unlock()
	b.mu.Lock()
	b.status.UpdatedAt = time.Now().UTC()
	status := b.status
	b.mu.Unlock()
	if err := status.Validate(time.Time{}); err != nil {
		b.logger.Printf("status validation failed: %v", err)
		return
	}
	if err := b.store.SaveStatus(status); err != nil {
		b.logger.Printf("write status failed: %v", err)
	}
}

func (b *Bridge) context() context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runCtx == nil {
		return context.Background()
	}
	return b.runCtx
}

func stringPointer(value string) *string {
	copy := value
	return &copy
}
