package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const DefaultPromptTimeout = 2 * time.Minute

type Config struct {
	BaseURL       string
	Directory     string
	Agent         string
	PromptTimeout time.Duration
	HTTPClient    *http.Client
}

type Client struct {
	baseURL       *url.URL
	directory     string
	directoryMu   sync.RWMutex
	useDirectory  bool
	agent         string
	promptTimeout time.Duration
	httpClient    *http.Client
}

type Session struct {
	ID string
}

type AssistantResponse struct {
	SessionID  string
	MessageID  string
	Text       string
	FinishedAt time.Time
}

type HTTPError struct {
	StatusCode int
	Status     string
	Method     string
	Path       string
	Body       string
}

func (e *HTTPError) Error() string {
	message := fmt.Sprintf("opencode request failed: %s %s: %s", e.Method, e.Path, e.Status)
	if e.Body != "" {
		message += ": " + e.Body
	}
	return message
}

type StaleSessionError struct {
	SessionID string
	Err       error
}

func (e *StaleSessionError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("opencode rejected stale session %q", e.SessionID)
	}
	return fmt.Sprintf("opencode rejected stale session %q: %v", e.SessionID, e.Err)
}

func (e *StaleSessionError) Unwrap() error { return e.Err }

func IsStaleSession(err error) bool {
	var stale *StaleSessionError
	return errors.As(err, &stale)
}

func IsSessionInvalid(err error) bool {
	if IsStaleSession(err) {
		return true
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.Method != http.MethodPost || !isSessionMessagePath(httpErr.Path) {
		return false
	}
	if isStaleSessionHTTPError(err) {
		return true
	}
	if httpErr.StatusCode != http.StatusInternalServerError {
		return false
	}
	return strings.Contains(strings.ToLower(httpErr.Body), "unknownerror")
}

func New(cfg Config) (*Client, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, errors.New("opencode base URL is required")
	}
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid opencode base URL %q", base)
	}
	if strings.TrimSpace(cfg.Directory) == "" {
		return nil, errors.New("opencode directory is required")
	}
	timeout := cfg.PromptTimeout
	if timeout == 0 {
		timeout = DefaultPromptTimeout
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL:       parsed,
		directory:     cfg.Directory,
		useDirectory:  true,
		agent:         strings.TrimSpace(cfg.Agent),
		promptTimeout: timeout,
		httpClient:    httpClient,
	}, nil
}

func (c *Client) CreateSession(ctx context.Context) (Session, error) {
	body := map[string]any{}
	if c.agent != "" {
		body["agent"] = c.agent
	}
	data, err := c.doJSON(ctx, http.MethodPost, "/session", body)
	if err != nil {
		if !shouldRetrySessionWithoutDirectory(err) {
			return Session{}, err
		}
		fallback, fallbackErr := c.doJSONWithDirectory(ctx, http.MethodPost, "/session", body, false)
		if fallbackErr != nil {
			return Session{}, fmt.Errorf("%w (retry without directory failed: %v)", err, fallbackErr)
		}
		c.disableDirectoryQuery()
		data = fallback
	}
	id, ok := firstString(data, "id", "sessionID")
	if !ok {
		if nested, ok := firstObject(data, "data", "info", "session"); ok {
			id, ok = firstString(nested, "id", "sessionID")
		}
	}
	if !ok || strings.TrimSpace(id) == "" {
		return Session{}, fmt.Errorf("opencode create session response did not include session id")
	}
	return Session{ID: id}, nil
}

func (c *Client) SubmitPrompt(ctx context.Context, sessionID, text string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("opencode session id is required")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("opencode prompt text is required")
	}
	message := map[string]any{
		"parts": []map[string]string{{
			"type": "text",
			"text": text,
		}},
		"noReply": false,
	}
	if c.agent != "" {
		message["agent"] = c.agent
	}
	_, err := c.doJSON(ctx, http.MethodPost, "/session/"+url.PathEscape(sessionID)+"/message", message)
	if err != nil {
		if isStaleSessionHTTPError(err) {
			return &StaleSessionError{SessionID: sessionID, Err: err}
		}
		return err
	}
	return nil
}

func (c *Client) Prompt(ctx context.Context, sessionID, text string) (AssistantResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.promptTimeout)
	defer cancel()
	events, errs, stop, err := c.Subscribe(ctx)
	if err != nil {
		return AssistantResponse{}, err
	}
	defer stop()
	if err := c.SubmitPrompt(ctx, sessionID, text); err != nil {
		return AssistantResponse{}, err
	}
	return AwaitAssistantResponse(ctx, sessionID, events, errs)
}

type SSEEvent struct {
	ID    string
	Event string
	Data  []byte
}

func (c *Client) Subscribe(ctx context.Context) (<-chan SSEEvent, <-chan error, func(), error) {
	streamCtx, cancel := context.WithCancel(ctx)
	target := c.buildURL("/event")
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, nil, func() {}, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, nil, func() {}, fmt.Errorf("connect to opencode event stream: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		return nil, nil, func() {}, c.httpError(req, resp, body)
	}
	events := make(chan SSEEvent, 16)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		defer resp.Body.Close()
		err := scanSSE(resp.Body, func(event SSEEvent) error {
			select {
			case <-streamCtx.Done():
				return streamCtx.Err()
			case events <- event:
				return nil
			}
		})
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			errs <- err
			return
		}
		if ctxErr := streamCtx.Err(); ctxErr != nil && !errors.Is(ctxErr, context.Canceled) {
			errs <- ctxErr
		}
	}()
	return events, errs, cancel, nil
}

func AwaitAssistantResponse(ctx context.Context, sessionID string, events <-chan SSEEvent, errs <-chan error) (AssistantResponse, error) {
	state := newResponseState(sessionID)
	for {
		select {
		case <-ctx.Done():
			return AssistantResponse{}, ctx.Err()
		case err, ok := <-errs:
			if ok && err != nil {
				return AssistantResponse{}, err
			}
			errs = nil
		case event, ok := <-events:
			if !ok {
				return AssistantResponse{}, errors.New("opencode event stream closed before assistant response completed")
			}
			response, done, err := state.Observe(event.Data)
			if err != nil {
				return AssistantResponse{}, err
			}
			if done {
				return response, nil
			}
		}
	}
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any) (map[string]any, error) {
	return c.doJSONWithDirectory(ctx, method, path, body, c.directoryQueryEnabled())
}

func (c *Client) doJSONWithDirectory(ctx context.Context, method, path string, body any, includeDirectory bool) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	target := c.buildURLWithDirectory(path, includeDirectory)
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to opencode: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.httpError(req, resp, respBody)
	}
	if len(strings.TrimSpace(string(respBody))) == 0 {
		return map[string]any{}, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, fmt.Errorf("decode opencode response: %w", err)
	}
	return decoded, nil
}

func (c *Client) buildURL(path string) string {
	return c.buildURLWithDirectory(path, c.directoryQueryEnabled())
}

func (c *Client) buildURLWithDirectory(path string, includeDirectory bool) string {
	u := *c.baseURL
	basePath := strings.TrimRight(u.Path, "/")
	requestPath := strings.TrimLeft(path, "/")
	if requestPath == "" {
		u.Path = basePath
	} else if basePath == "" {
		u.Path = "/" + requestPath
	} else {
		u.Path = basePath + "/" + requestPath
	}
	q := u.Query()
	if includeDirectory && c.directory != "" {
		q.Set("directory", c.directory)
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String()
}

func (c *Client) directoryQueryEnabled() bool {
	c.directoryMu.RLock()
	defer c.directoryMu.RUnlock()
	return c.useDirectory
}

func (c *Client) disableDirectoryQuery() {
	c.directoryMu.Lock()
	defer c.directoryMu.Unlock()
	c.useDirectory = false
}

func (c *Client) httpError(req *http.Request, resp *http.Response, body []byte) error {
	return &HTTPError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Method:     req.Method,
		Path:       requestPath(req.URL),
		Body:       compactBody(body),
	}
}

func requestPath(u *url.URL) string {
	if u == nil {
		return ""
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return path
}

func compactBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 240 {
		text = text[:240] + "…"
	}
	return strings.ReplaceAll(text, "\n", " ")
}

func shouldRetrySessionWithoutDirectory(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusInternalServerError
}

func IsRetryable(err error) bool {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= http.StatusInternalServerError
	}
	return strings.Contains(err.Error(), "connect to opencode")
}

func isStaleSessionHTTPError(err error) bool {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode == http.StatusGone {
		return true
	}
	body := strings.ToLower(httpErr.Body)
	return (httpErr.StatusCode == http.StatusBadRequest || httpErr.StatusCode == http.StatusConflict) &&
		strings.Contains(body, "session") &&
		(strings.Contains(body, "stale") || strings.Contains(body, "not found") || strings.Contains(body, "missing") || strings.Contains(body, "deleted") || strings.Contains(body, "invalid"))
}

func isSessionMessagePath(path string) bool {
	path, _, _ = strings.Cut(path, "?")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 3 && parts[0] == "session" && parts[1] != "" && parts[2] == "message"
}

func scanSSE(r io.Reader, fn func(SSEEvent) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	var event SSEEvent
	var dataLines []string
	dispatch := func() error {
		if len(dataLines) == 0 {
			event = SSEEvent{}
			return nil
		}
		event.Data = []byte(strings.Join(dataLines, "\n"))
		if fn != nil {
			if err := fn(event); err != nil {
				return err
			}
		}
		event = SSEEvent{}
		dataLines = nil
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if ok && strings.HasPrefix(value, " ") {
			value = strings.TrimPrefix(value, " ")
		}
		if !ok {
			field, value = line, ""
		}
		switch field {
		case "event":
			event.Event = value
		case "data":
			dataLines = append(dataLines, value)
		case "id":
			event.ID = value
		}
	}
	if err := dispatch(); err != nil {
		return err
	}
	return scanner.Err()
}

func firstString(obj map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := obj[key].(string); ok {
			return value, true
		}
	}
	return "", false
}

func firstObject(obj map[string]any, keys ...string) (map[string]any, bool) {
	for _, key := range keys {
		if value, ok := obj[key].(map[string]any); ok {
			return value, true
		}
	}
	return nil, false
}
