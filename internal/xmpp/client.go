package xmpp

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultPort = 5222

const chatStateNamespace = "http://jabber.org/protocol/chatstates"

type ChatState string

const (
	ChatStateActive    ChatState = "active"
	ChatStateComposing ChatState = "composing"
)

type Config struct {
	Host      string
	Port      int
	JID       string
	Password  string
	Resource  string
	StartTLS  bool
	Plaintext bool
	Timeout   time.Duration
	Dialer    func(context.Context, string, string) (net.Conn, error)
}

type Client struct {
	cfg      Config
	jid      JID
	conn     net.Conn
	reader   *bufio.Reader
	decoder  *xml.Decoder
	features Features
	seq      int
}

type JID struct {
	Local    string `json:"local"`
	Domain   string `json:"domain"`
	Resource string `json:"resource,omitempty"`
}

func (j JID) Bare() string { return j.Local + "@" + j.Domain }

func (j JID) Full() string {
	if j.Resource == "" {
		return j.Bare()
	}
	return j.Bare() + "/" + j.Resource
}

type Features struct {
	StartTLS   bool
	Plain      bool
	Bind       bool
	Session    bool
	RawElement string
}

type Message struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	Type string `json:"type,omitempty"`
	Body string `json:"body"`
}

type PingResult struct {
	ID string `json:"id"`
	OK bool   `json:"ok"`
}

func ParseJID(raw string) (JID, error) {
	bare, resource, _ := strings.Cut(strings.TrimSpace(raw), "/")
	local, domain, ok := strings.Cut(bare, "@")
	if !ok || local == "" || domain == "" {
		return JID{}, fmt.Errorf("invalid jid %q; expected local@domain[/resource]", raw)
	}
	return JID{Local: local, Domain: domain, Resource: resource}, nil
}

func New(cfg Config) (*Client, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if !cfg.Plaintext {
		cfg.StartTLS = true
	}
	jid, err := ParseJID(cfg.JID)
	if err != nil {
		return nil, err
	}
	if cfg.Resource == "" {
		cfg.Resource = jid.Resource
	}
	if cfg.Resource == "" {
		cfg.Resource = "xmpp-go"
	}
	if cfg.Host == "" {
		cfg.Host = jid.Domain
	}
	if cfg.Password == "" {
		return nil, errors.New("missing password")
	}
	return &Client{cfg: cfg, jid: jid}, nil
}

func ConfigFromEnv() Config {
	port := DefaultPort
	if raw := os.Getenv("XMPP_PORT"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			port = parsed
		}
	}
	return Config{
		Host:     os.Getenv("XMPP_HOST"),
		Port:     port,
		JID:      os.Getenv("XMPP_JID"),
		Password: os.Getenv("XMPP_PASS"),
		Resource: os.Getenv("XMPP_RESOURCE"),
	}
}

func (c *Client) Connect(ctx context.Context) error {
	dialer := c.cfg.Dialer
	if dialer == nil {
		d := &net.Dialer{Timeout: c.cfg.Timeout}
		dialer = d.DialContext
	}
	conn, err := dialer(ctx, "tcp", net.JoinHostPort(c.cfg.Host, strconv.Itoa(c.cfg.Port)))
	if err != nil {
		return fmt.Errorf("connect %s:%d: %w", c.cfg.Host, c.cfg.Port, err)
	}
	c.conn = conn
	c.resetDecoder()

	if err := c.openStream(); err != nil {
		return err
	}
	features, err := c.readFeatures()
	if err != nil {
		return err
	}
	c.features = features
	if c.cfg.StartTLS && features.StartTLS {
		if err := c.startTLS(ctx); err != nil {
			return err
		}
	}
	if err := c.authPlain(); err != nil {
		return err
	}
	if err := c.openStream(); err != nil {
		return err
	}
	features, err = c.readFeatures()
	if err != nil {
		return err
	}
	c.features = features
	if features.Bind {
		if err := c.bindResource(); err != nil {
			return err
		}
	}
	if features.Session {
		if err := c.session(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	_, _ = io.WriteString(c.conn, "</stream:stream>")
	return c.conn.Close()
}

func (c *Client) SendPresence() error {
	_, err := io.WriteString(c.conn, "<presence/>")
	return err
}

func (c *Client) SendMessage(to, body string) error {
	stanza := fmt.Sprintf("<message to='%s' type='chat'><body>%s</body></message>", xmlEscape(to), xmlEscape(body))
	_, err := io.WriteString(c.conn, stanza)
	return err
}

func (c *Client) SendChatState(to string, state ChatState) error {
	if !state.valid() {
		return fmt.Errorf("unsupported xmpp chat state %q", state)
	}
	stanza := fmt.Sprintf("<message to='%s' type='chat'><%s xmlns='%s'/></message>", xmlEscape(to), state, chatStateNamespace)
	_, err := io.WriteString(c.conn, stanza)
	return err
}

func (s ChatState) valid() bool {
	switch s {
	case ChatStateActive, ChatStateComposing:
		return true
	default:
		return false
	}
}

func (c *Client) Ping(ctx context.Context, to string) (*PingResult, error) {
	id := c.nextID("ping")
	if to == "" {
		to = c.jid.Domain
	}
	stanza := fmt.Sprintf("<iq type='get' to='%s' id='%s'><ping xmlns='urn:xmpp:ping'/></iq>", xmlEscape(to), id)
	if _, err := io.WriteString(c.conn, stanza); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		resp, err := c.readIQ()
		if err != nil {
			return nil, err
		}
		if resp.ID == id {
			return &PingResult{ID: id, OK: resp.Type == "result"}, nil
		}
	}
}

func (c *Client) ReceiveMessage(ctx context.Context) (*Message, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		tok, err := c.receiveToken(ctx)
		if err != nil {
			return nil, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "message" {
			continue
		}
		msg, err := decodeMessage(c.decoder, start)
		if err != nil {
			return nil, err
		}
		if msg.Body != "" {
			return &msg, nil
		}
	}
}

func (c *Client) StreamMessages(ctx context.Context, yield func(*Message) error) error {
	for {
		msg, err := c.ReceiveMessage(ctx)
		if err != nil {
			if receiveEndedByContext(ctx, err) {
				return nil
			}
			return err
		}
		if err := yield(msg); err != nil {
			return err
		}
	}
}

func (c *Client) receiveToken(ctx context.Context) (xml.Token, error) {
	if c.conn == nil {
		return c.decoder.Token()
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetReadDeadline(deadline)
	} else {
		_ = c.conn.SetReadDeadline(time.Time{})
	}
	done := make(chan struct{})
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = c.conn.SetReadDeadline(time.Now())
			case <-done:
			}
		}()
		defer close(done)
	}
	tok, err := c.decoder.Token()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return nil, context.DeadlineExceeded
		}
	}
	return tok, err
}

func receiveEndedByContext(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	deadline, ok := ctx.Deadline()
	return ok && !time.Now().Before(deadline)
}

func (c *Client) openStream() error {
	stream := fmt.Sprintf("<stream:stream to='%s' version='1.0' xmlns='jabber:client' xmlns:stream='http://etherx.jabber.org/streams'>", xmlEscape(c.jid.Domain))
	_, err := io.WriteString(c.conn, stream)
	return err
}

func (c *Client) startTLS(ctx context.Context) error {
	if _, err := io.WriteString(c.conn, "<starttls xmlns='urn:ietf:params:xml:ns:xmpp-tls'/>"); err != nil {
		return err
	}
	if err := c.expectStart("proceed"); err != nil {
		return err
	}
	tlsConn := tls.Client(c.conn, &tls.Config{ServerName: c.cfg.Host, MinVersion: tls.VersionTLS12})
	if deadline, ok := ctx.Deadline(); ok {
		_ = tlsConn.SetDeadline(deadline)
	}
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("starttls handshake: %w", err)
	}
	_ = tlsConn.SetDeadline(time.Time{})
	c.conn = tlsConn
	c.resetDecoder()
	if err := c.openStream(); err != nil {
		return err
	}
	features, err := c.readFeatures()
	if err != nil {
		return err
	}
	c.features = features
	return nil
}

func (c *Client) authPlain() error {
	if !c.features.Plain {
		return errors.New("server did not advertise SASL PLAIN")
	}
	payload := base64.StdEncoding.EncodeToString([]byte("\x00" + c.jid.Local + "\x00" + c.cfg.Password))
	if _, err := io.WriteString(c.conn, "<auth xmlns='urn:ietf:params:xml:ns:xmpp-sasl' mechanism='PLAIN'>"+payload+"</auth>"); err != nil {
		return err
	}
	return c.expectStart("success")
}

func (c *Client) bindResource() error {
	id := c.nextID("bind")
	stanza := fmt.Sprintf("<iq type='set' id='%s'><bind xmlns='urn:ietf:params:xml:ns:xmpp-bind'><resource>%s</resource></bind></iq>", id, xmlEscape(c.cfg.Resource))
	if _, err := io.WriteString(c.conn, stanza); err != nil {
		return err
	}
	iq, err := c.readIQ()
	if err != nil {
		return err
	}
	if iq.ID != id || iq.Type != "result" {
		return fmt.Errorf("resource bind failed: id=%s type=%s", iq.ID, iq.Type)
	}
	return nil
}

func (c *Client) session() error {
	id := c.nextID("session")
	if _, err := io.WriteString(c.conn, "<iq type='set' id='"+id+"'><session xmlns='urn:ietf:params:xml:ns:xmpp-session'/></iq>"); err != nil {
		return err
	}
	iq, err := c.readIQ()
	if err != nil {
		return err
	}
	if iq.ID != id || iq.Type != "result" {
		return fmt.Errorf("session setup failed: id=%s type=%s", iq.ID, iq.Type)
	}
	return nil
}

func (c *Client) resetDecoder() {
	c.reader = bufio.NewReader(c.conn)
	c.decoder = xml.NewDecoder(c.reader)
}

func (c *Client) nextID(prefix string) string {
	c.seq++
	return fmt.Sprintf("%s_%d", prefix, c.seq)
}

func (c *Client) readFeatures() (Features, error) {
	for {
		tok, err := c.decoder.Token()
		if err != nil {
			return Features{}, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "features" {
			continue
		}
		return decodeFeatures(c.decoder, start)
	}
}

func (c *Client) expectStart(local string) error {
	for {
		tok, err := c.decoder.Token()
		if err != nil {
			return err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local == local {
			if err := c.decoder.Skip(); err != nil {
				return err
			}
			return nil
		}
		if start.Name.Local == "failure" {
			return errors.New("xmpp failure from server")
		}
	}
}

type iqResult struct{ ID, Type string }

func (c *Client) readIQ() (iqResult, error) {
	for {
		tok, err := c.decoder.Token()
		if err != nil {
			return iqResult{}, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "iq" {
			continue
		}
		res := iqResult{ID: attr(start, "id"), Type: attr(start, "type")}
		if err := c.decoder.Skip(); err != nil {
			return iqResult{}, err
		}
		return res, nil
	}
}

func decodeFeatures(decoder *xml.Decoder, start xml.StartElement) (Features, error) {
	features := Features{}
	for {
		tok, err := decoder.Token()
		if err != nil {
			return features, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "starttls":
				features.StartTLS = true
			case "mechanism":
				var mechanism string
				if err := decoder.DecodeElement(&mechanism, &t); err != nil {
					return features, err
				}
				if strings.EqualFold(strings.TrimSpace(mechanism), "PLAIN") {
					features.Plain = true
				}
			case "bind":
				features.Bind = true
			case "session":
				features.Session = true
			}
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return features, nil
			}
		}
	}
}

func decodeMessage(decoder *xml.Decoder, start xml.StartElement) (Message, error) {
	msg := Message{From: attr(start, "from"), To: attr(start, "to"), Type: attr(start, "type")}
	for {
		tok, err := decoder.Token()
		if err != nil {
			return msg, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "body" {
				if err := decoder.DecodeElement(&msg.Body, &t); err != nil {
					return msg, err
				}
			}
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return msg, nil
			}
		}
	}
}

func attr(start xml.StartElement, name string) string {
	for _, a := range start.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func xmlEscape(v string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(v))
	return b.String()
}
