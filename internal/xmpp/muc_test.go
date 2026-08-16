package xmpp

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestJoinMUCRequestsNoHistory(t *testing.T) {
	stanza := captureClientWriteUntil(t, "</presence>", func(client *Client) error {
		return client.JoinMUC("agents-abc@conference.example.test", "agent-123")
	})
	for _, fragment := range []string{
		"<presence to='agents-abc@conference.example.test/agent-123'>",
		"<x xmlns='http://jabber.org/protocol/muc'>",
		"<history maxchars='0' maxstanzas='0' seconds='0'/>",
	} {
		if !strings.Contains(stanza, fragment) {
			t.Fatalf("join stanza missing %q: %s", fragment, stanza)
		}
	}
}

func TestDecodeMessageParsesBothDelayFormats(t *testing.T) {
	cases := []struct {
		name      string
		extension string
		namespace string
		want      time.Time
	}{
		{
			name:      "XEP-0203",
			extension: `<delay xmlns='urn:xmpp:delay' stamp='2026-07-21T12:34:56Z'/>`,
			namespace: xep203DelayNamespace,
			want:      time.Date(2026, 7, 21, 12, 34, 56, 0, time.UTC),
		},
		{
			name:      "legacy",
			extension: `<x xmlns='jabber:x:delay' stamp='20260721T12:34:56'/>`,
			namespace: legacyDelayNamespace,
			want:      time.Date(2026, 7, 21, 12, 34, 56, 0, time.UTC),
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			message := decodeMessageXML(t, `<message from='room@example.test/nick' type='groupchat'><body>old</body>`+test.extension+`</message>`)
			if message.Delay == nil || message.Delay.Namespace != test.namespace || !message.Delay.Stamp.Equal(test.want) {
				t.Fatalf("delay=%+v", message.Delay)
			}
		})
	}
}

func TestAgentMessageMetadataRoundTripAndValidation(t *testing.T) {
	metadata := AgentMessageMetadata{
		Correlation:   "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		OriginAgentID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		Hop:           1,
	}
	stanza := captureClientWrite(t, func(client *Client) error {
		_, err := client.SendAgentMessage("peer@example.test", DirectChatMessageType, "hello", metadata)
		return err
	})
	message := decodeMessageXML(t, stanza)
	if message.AgentMessage == nil || *message.AgentMessage != metadata || message.MetadataErr != "" {
		t.Fatalf("agent metadata=%+v err=%q", message.AgentMessage, message.MetadataErr)
	}

	malformed := decodeMessageXML(t, `<message from='peer@example.test' type='chat'><body>x</body><agent-message xmlns='urn:chatinfra:agent-message:1' correlation='not-a-uuid' origin-agent-id='bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb' hop='0'/></message>`)
	if malformed.AgentMessage != nil || malformed.MetadataErr == "" {
		t.Fatalf("malformed metadata unexpectedly admitted: %+v", malformed)
	}
}

func TestDecodeOccupantPresenceRealJIDUnavailableAndConflict(t *testing.T) {
	available := decodePresenceXML(t, `<presence from='agents@example.test/agent-peer'><x xmlns='http://jabber.org/protocol/muc#user'><item affiliation='member' role='participant' jid='peer@example.test/phone'/><status code='110'/></x></presence>`)
	if !available.Available || available.RealJID != "peer@example.test/phone" || available.Affiliation != "member" || !available.Self {
		t.Fatalf("available presence=%+v", available)
	}
	unavailable := decodePresenceXML(t, `<presence from='agents@example.test/agent-peer' type='unavailable'><x xmlns='http://jabber.org/protocol/muc#user'><item affiliation='none' role='none' jid='peer@example.test'/></x></presence>`)
	if !unavailable.IsUnavailable() || unavailable.Nickname != "agent-peer" {
		t.Fatalf("unavailable presence=%+v", unavailable)
	}
	conflict := decodePresenceXML(t, `<presence from='agents@example.test/agent-self' type='error'><error type='cancel'><conflict xmlns='urn:ietf:params:xml:ns:xmpp-stanzas'/></error></presence>`)
	if !conflict.NicknameConflict || conflict.Available {
		t.Fatalf("conflict presence=%+v", conflict)
	}
	forbidden := decodePresenceXML(t, `<presence from='agents@example.test/agent-self' type='error'><error type='auth'><forbidden xmlns='urn:ietf:params:xml:ns:xmpp-stanzas'/></error></presence>`)
	if forbidden.Available || forbidden.NicknameConflict || forbidden.ErrorCondition != "forbidden" {
		t.Fatalf("forbidden presence=%+v", forbidden)
	}
}

func TestSendGroupchatUsesGroupchatType(t *testing.T) {
	stanza := captureClientWrite(t, func(client *Client) error {
		return client.SendGroupchat("agents@example.test", "hello")
	})
	if !strings.Contains(stanza, "type='groupchat'") || !strings.Contains(stanza, "<body>hello</body>") {
		t.Fatalf("groupchat stanza=%s", stanza)
	}
}

func decodeMessageXML(t *testing.T, raw string) Message {
	t.Helper()
	decoder := xml.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		t.Fatal(err)
	}
	message, err := decodeMessage(decoder, token.(xml.StartElement))
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func decodePresenceXML(t *testing.T, raw string) OccupantPresence {
	t.Helper()
	decoder := xml.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		t.Fatal(err)
	}
	presence, err := decodeOccupantPresence(decoder, token.(xml.StartElement))
	if err != nil {
		t.Fatal(err)
	}
	return presence
}
