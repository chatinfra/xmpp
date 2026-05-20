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
	sessionRetryInitialDelay = time.Second
	sessionRetryMaxElapsed   = 2 * time.Minute
)

type xmppConn interface {
	Connect(context.Context) error
	SendPresence() error
	StreamMessages(context.Context, func(*xmpp.Message) error) error
	SendMessage(to, body string) error
	Close() error
}

type opencodeClient interface {
	CreateSession(context.Context) (opencode.Session, error)
	Prompt(context.Context, string, string) (opencode.AssistantResponse, error)
}

type Bridge struct {
	cfg      Config
	logger   *log.Logger
	xmpp     xmppConn
	opencode opencodeClient
	store    *StateStore

	mu       sync.Mutex
	sessions map[string]string
	workers  map[string]*sessionWorker
	status   StatusFile
	runCtx   context.Context
	wg       sync.WaitGroup
}

type sessionWorker struct {
	bareJID string
	ch      chan inboundMessage
}

type inboundMessage struct {
	BareJID string
	FullJID string
	Body    string
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
		Agent:         cfg.AgentID,
		PromptTimeout: cfg.PromptTimeout,
	})
	if err != nil {
		return nil, err
	}
	return NewBridgeWithClients(cfg, logger, xmppClient, opencodeClient), nil
}

func NewBridgeWithClients(cfg Config, logger *log.Logger, xmppClient xmppConn, opencodeClient opencodeClient) *Bridge {
	if logger == nil {
		logger = log.Default()
	}
	return &Bridge{
		cfg:      cfg,
		logger:   logger,
		xmpp:     xmppClient,
		opencode: opencodeClient,
		store:    NewStateStore(cfg.StateDir),
		workers:  map[string]*sessionWorker{},
		status: StatusFile{
			StartedAt: time.Now().UTC(),
		},
	}
}

func (b *Bridge) Run(ctx context.Context) error {
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

	if err := b.xmpp.Connect(ctx); err != nil {
		b.recordError(fmt.Errorf("xmpp connect: %w", err))
		return err
	}
	b.setConnected(true)
	defer func() {
		b.setConnected(false)
		_ = b.xmpp.Close()
		b.shutdownWorkers()
		b.flushSessions()
		b.flushStatus()
	}()
	if err := b.xmpp.SendPresence(); err != nil {
		b.recordError(fmt.Errorf("send presence: %w", err))
		return err
	}
	b.logger.Printf("connected jid=%s agent=%s", b.cfg.XMPP.JID, b.cfg.AgentID)
	err = b.xmpp.StreamMessages(ctx, func(msg *xmpp.Message) error {
		return b.enqueue(ctx, msg)
	})
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		b.recordError(fmt.Errorf("xmpp receive: %w", err))
		return err
	}
	return nil
}

func (b *Bridge) enqueue(ctx context.Context, msg *xmpp.Message) error {
	if msg == nil || strings.TrimSpace(msg.Body) == "" {
		return nil
	}
	bare, err := bareJID(msg.From)
	if err != nil {
		b.recordError(err)
		return nil
	}
	now := time.Now().UTC()
	b.mu.Lock()
	b.status.LastInboundAt = &now
	b.mu.Unlock()
	b.flushStatus()
	worker := b.workerFor(bare)
	inbound := inboundMessage{BareJID: bare, FullJID: msg.From, Body: msg.Body}
	select {
	case worker.ch <- inbound:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Bridge) workerFor(bare string) *sessionWorker {
	b.mu.Lock()
	defer b.mu.Unlock()
	if worker := b.workers[bare]; worker != nil {
		return worker
	}
	worker := &sessionWorker{bareJID: bare, ch: make(chan inboundMessage, 64)}
	b.workers[bare] = worker
	b.wg.Add(1)
	runCtx := b.runCtx
	if runCtx == nil {
		runCtx = context.Background()
	}
	go b.runWorker(runCtx, worker)
	return worker
}

func (b *Bridge) runWorker(ctx context.Context, worker *sessionWorker) {
	defer b.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-worker.ch:
			if !ok {
				return
			}
			b.processMessage(ctx, msg)
		}
	}
}

func (b *Bridge) processMessage(ctx context.Context, msg inboundMessage) {
	sessionID, err := b.sessionForWithRetry(ctx, msg.BareJID)
	if err != nil {
		b.logOnlyError("create session", err)
		return
	}
	response, err := b.opencode.Prompt(ctx, sessionID, msg.Body)
	if opencode.IsStaleSession(err) {
		b.logger.Printf("recreating stale opencode session bare_jid=%s", msg.BareJID)
		sessionID, err = b.recreateSession(ctx, msg.BareJID)
		if err == nil {
			response, err = b.opencode.Prompt(ctx, sessionID, msg.Body)
		}
	}
	if err != nil {
		b.logOnlyError("opencode prompt", err)
		return
	}
	if strings.TrimSpace(response.Text) == "" {
		b.logOnlyError("opencode prompt", errors.New("assistant response had no text"))
		return
	}
	if err := b.xmpp.SendMessage(msg.FullJID, response.Text); err != nil {
		b.recordError(fmt.Errorf("send xmpp reply: %w", err))
		return
	}
	now := time.Now().UTC()
	b.mu.Lock()
	b.status.LastReplyAt = &now
	b.mu.Unlock()
	b.flushStatus()
}

func (b *Bridge) sessionFor(ctx context.Context, bare string) (string, error) {
	b.mu.Lock()
	sessionID := b.sessions[bare]
	b.mu.Unlock()
	if sessionID != "" {
		return sessionID, nil
	}
	return b.recreateSession(ctx, bare)
}

func (b *Bridge) sessionForWithRetry(ctx context.Context, bare string) (string, error) {
	startedAt := time.Now()
	delay := sessionRetryInitialDelay
	attempt := 0
	for {
		attempt++
		sessionID, err := b.sessionFor(ctx, bare)
		if err == nil {
			return sessionID, nil
		}
		if !opencode.IsRetryable(err) || time.Since(startedAt) >= sessionRetryMaxElapsed {
			return "", err
		}
		b.logger.Printf("create session transient failure bare_jid=%s attempt=%d retry_in=%s: %v", bare, attempt, delay, err)
		b.recordError(err)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
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

func (b *Bridge) recreateSession(ctx context.Context, bare string) (string, error) {
	session, err := b.opencode.CreateSession(ctx)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	if b.sessions == nil {
		b.sessions = map[string]string{}
	}
	b.sessions[bare] = session.ID
	b.status.ActiveSessionCount = len(b.sessions)
	b.mu.Unlock()
	if err := b.flushSessions(); err != nil {
		return "", err
	}
	b.flushStatus()
	return session.ID, nil
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
	b.mu.Unlock()
	b.flushStatus()
}

func (b *Bridge) recordError(err error) {
	if err == nil {
		return
	}
	b.mu.Lock()
	b.status.LastError = err.Error()
	b.mu.Unlock()
	b.flushStatus()
}

func (b *Bridge) logOnlyError(action string, err error) {
	if err == nil {
		return
	}
	b.logger.Printf("%s failed: %v", action, err)
	b.recordError(err)
}

func (b *Bridge) flushSessions() error {
	b.mu.Lock()
	sessions := make(map[string]string, len(b.sessions))
	for key, value := range b.sessions {
		sessions[key] = value
	}
	b.mu.Unlock()
	return b.store.SaveSessions(sessions)
}

func (b *Bridge) flushStatus() {
	b.mu.Lock()
	status := b.status
	b.mu.Unlock()
	if err := b.store.SaveStatus(status); err != nil {
		b.logger.Printf("write status failed: %v", err)
	}
}

func bareJID(raw string) (string, error) {
	jid, err := xmpp.ParseJID(raw)
	if err == nil {
		return jid.Bare(), nil
	}
	bare, _, _ := strings.Cut(strings.TrimSpace(raw), "/")
	if strings.Contains(bare, "@") && bare != "" {
		return bare, nil
	}
	return "", fmt.Errorf("invalid inbound from JID %q", raw)
}
