package xmpp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseJID(t *testing.T) {
	jid, err := ParseJID("alice@example.com/mobile")
	if err != nil {
		t.Fatal(err)
	}
	if jid.Local != "alice" || jid.Domain != "example.com" || jid.Resource != "mobile" || jid.Full() != "alice@example.com/mobile" {
		t.Fatalf("jid=%+v", jid)
	}
	if _, err := ParseJID("bad"); err == nil {
		t.Fatal("expected invalid jid error")
	}
}

func TestPlaintextConnectSendAndReceive(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	done := make(chan error, 1)
	go fakeServer(serverConn, done, t)

	client, err := New(Config{
		JID:       "alice@example.com/mobile",
		Password:  "secret",
		Plaintext: true,
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			return clientConn, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.SendPresence(); err != nil {
		t.Fatal(err)
	}
	if err := client.SendMessage("bob@example.com", "hello & <there>"); err != nil {
		t.Fatal(err)
	}
	msg, err := client.ReceiveMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if msg.From != "bob@example.com" || msg.Body != "reply" {
		t.Fatalf("msg=%+v", msg)
	}
	_ = client.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSendChatStateActive(t *testing.T) {
	stanza := captureClientWrite(t, func(client *Client) error {
		return client.SendChatState("bob@example.com/mobile", ChatStateActive)
	})

	if !strings.Contains(stanza, "<message to='bob@example.com/mobile' type='chat'>") {
		t.Fatalf("stanza missing message wrapper: %s", stanza)
	}
	if !strings.Contains(stanza, "<active xmlns='http://jabber.org/protocol/chatstates'/>") {
		t.Fatalf("stanza missing active chat state: %s", stanza)
	}
	if strings.Contains(stanza, "<body>") {
		t.Fatalf("chat-state stanza should not include body: %s", stanza)
	}
}

func TestSendChatStateComposingEscapesDestination(t *testing.T) {
	stanza := captureClientWrite(t, func(client *Client) error {
		return client.SendChatState("bob@example.com/mobile&<unsafe>", ChatStateComposing)
	})

	if !strings.Contains(stanza, "to='bob@example.com/mobile&amp;&lt;unsafe&gt;'") {
		t.Fatalf("stanza did not escape destination JID: %s", stanza)
	}
	if !strings.Contains(stanza, "<composing xmlns='http://jabber.org/protocol/chatstates'/>") {
		t.Fatalf("stanza missing composing chat state: %s", stanza)
	}
}

func captureClientWrite(t *testing.T, write func(*Client) error) string {
	return captureClientWriteUntil(t, "</message>", write)
}

func captureClientWriteUntil(t *testing.T, marker string, write func(*Client) error) string {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	readDone := make(chan struct {
		stanza string
		err    error
	}, 1)
	go func() {
		stanza, err := readUntilString(bufio.NewReader(serverConn), marker)
		readDone <- struct {
			stanza string
			err    error
		}{stanza: stanza, err: err}
	}()
	client := &Client{conn: clientConn}
	if err := write(client); err != nil {
		t.Fatalf("write stanza: %v", err)
	}
	select {
	case result := <-readDone:
		if result.err != nil {
			t.Fatalf("read stanza: %v", result.err)
		}
		return result.stanza
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stanza")
	}
	return ""
}

func fakeServer(conn net.Conn, done chan<- error, t *testing.T) {
	t.Helper()
	defer conn.Close()
	reader := bufio.NewReader(conn)
	if err := readUntil(reader, "<stream:stream"); err != nil {
		done <- err
		return
	}
	if _, err := io.WriteString(conn, `<stream:stream xmlns:stream='http://etherx.jabber.org/streams'><stream:features><mechanisms xmlns='urn:ietf:params:xml:ns:xmpp-sasl'><mechanism>PLAIN</mechanism></mechanisms></stream:features>`); err != nil {
		done <- err
		return
	}
	if err := readUntil(reader, "</auth>"); err != nil {
		done <- err
		return
	}
	authPayload := base64.StdEncoding.EncodeToString([]byte("\x00alice\x00secret"))
	if !strings.Contains(lastRead, authPayload) {
		done <- fmt.Errorf("auth payload missing: %s", lastRead)
		return
	}
	if _, err := io.WriteString(conn, `<success xmlns='urn:ietf:params:xml:ns:xmpp-sasl'/>`); err != nil {
		done <- err
		return
	}
	if err := readUntil(reader, "<stream:stream"); err != nil {
		done <- err
		return
	}
	if _, err := io.WriteString(conn, `<stream:stream xmlns:stream='http://etherx.jabber.org/streams'><stream:features><bind xmlns='urn:ietf:params:xml:ns:xmpp-bind'/></stream:features>`); err != nil {
		done <- err
		return
	}
	if err := readUntil(reader, "</iq>"); err != nil {
		done <- err
		return
	}
	if !strings.Contains(lastRead, "<resource>mobile</resource>") {
		done <- fmt.Errorf("bind resource missing: %s", lastRead)
		return
	}
	if _, err := io.WriteString(conn, `<iq type='result' id='bind_1'><bind xmlns='urn:ietf:params:xml:ns:xmpp-bind'><jid>alice@example.com/mobile</jid></bind></iq>`); err != nil {
		done <- err
		return
	}
	if err := readUntil(reader, "<presence/>"); err != nil {
		done <- err
		return
	}
	if err := readUntil(reader, "</message>"); err != nil {
		done <- err
		return
	}
	if !strings.Contains(lastRead, "hello &amp; &lt;there&gt;") {
		done <- fmt.Errorf("message not escaped: %s", lastRead)
		return
	}
	_, _ = io.WriteString(conn, `<message from='bob@example.com' to='alice@example.com/mobile' type='chat'><body>reply</body></message>`)
	_ = readUntil(reader, "</stream:stream>")
	done <- nil
}

var lastRead string

func readUntil(reader *bufio.Reader, marker string) error {
	value, err := readUntilString(reader, marker)
	if err != nil {
		return err
	}
	lastRead = value
	return nil
}

func readUntilString(reader *bufio.Reader, marker string) (string, error) {
	var b strings.Builder
	for {
		r, _, err := reader.ReadRune()
		if err != nil {
			return "", err
		}
		b.WriteRune(r)
		if strings.Contains(b.String(), marker) {
			return b.String(), nil
		}
	}
}

func TestDecodeFeatures(t *testing.T) {
	decoder := xmlDecoder(`<stream:features><starttls/><mechanisms><mechanism>PLAIN</mechanism></mechanisms><bind/><session/></stream:features>`)
	tok, err := decoder.Token()
	if err != nil {
		t.Fatal(err)
	}
	features, err := decodeFeatures(decoder, tok.(xml.StartElement))
	if err != nil {
		t.Fatal(err)
	}
	if !features.StartTLS || !features.Plain || !features.Bind || !features.Session {
		t.Fatalf("features=%+v", features)
	}
}

func xmlDecoder(raw string) *xml.Decoder {
	return xml.NewDecoder(strings.NewReader(raw))
}
