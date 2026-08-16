package xmpp

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	mucNamespace          = "http://jabber.org/protocol/muc"
	mucUserNamespace      = "http://jabber.org/protocol/muc#user"
	stanzaErrorNamespace  = "urn:ietf:params:xml:ns:xmpp-stanzas"
	xep203DelayNamespace  = "urn:xmpp:delay"
	legacyDelayNamespace  = "jabber:x:delay"
	agentMessageNamespace = "urn:chatinfra:agent-message:1"
	AgentMessageNamespace = agentMessageNamespace
	GroupchatMessageType  = "groupchat"
	DirectChatMessageType = "chat"
)

type Delay struct {
	Namespace string    `json:"namespace"`
	Stamp     time.Time `json:"stamp"`
}

type AgentMessageMetadata struct {
	Correlation   string `json:"correlation"`
	OriginAgentID string `json:"originAgentId"`
	Hop           int    `json:"hop"`
}

type OccupantPresence struct {
	From             string
	RoomJID          string
	Nickname         string
	Type             string
	ErrorCondition   string
	Available        bool
	RealJID          string
	Affiliation      string
	Role             string
	Self             bool
	NicknameConflict bool
	StatusCodes      []int
}

func (c *Client) JoinMUC(roomJID, nickname string) error {
	if strings.TrimSpace(roomJID) == "" || strings.TrimSpace(nickname) == "" {
		return errors.New("room JID and nickname are required")
	}
	stanza := fmt.Sprintf(
		"<presence to='%s/%s'><x xmlns='%s'><history maxchars='0' maxstanzas='0' seconds='0'/></x></presence>",
		xmlEscape(roomJID), xmlEscape(nickname), mucNamespace,
	)
	return c.writeStanza(stanza)
}

func (c *Client) SendGroupchat(roomJID, body string) error {
	_, err := c.sendMessage(roomJID, GroupchatMessageType, body, nil)
	return err
}

func (c *Client) SendAgentMessage(to, messageType, body string, metadata AgentMessageMetadata) (string, error) {
	if err := metadata.Validate(); err != nil {
		return "", err
	}
	if messageType != DirectChatMessageType && messageType != GroupchatMessageType {
		return "", fmt.Errorf("unsupported message type %q", messageType)
	}
	return c.sendMessage(to, messageType, body, &metadata)
}

func (c *Client) sendMessage(to, messageType, body string, metadata *AgentMessageMetadata) (string, error) {
	id := c.nextID("message")
	var extension string
	if metadata != nil {
		extension = fmt.Sprintf(
			"<agent-message xmlns='%s' correlation='%s' origin-agent-id='%s' hop='%d'/>",
			agentMessageNamespace,
			xmlEscape(metadata.Correlation),
			xmlEscape(metadata.OriginAgentID),
			metadata.Hop,
		)
	}
	stanza := fmt.Sprintf(
		"<message id='%s' to='%s' type='%s'><body>%s</body>%s</message>",
		xmlEscape(id), xmlEscape(to), xmlEscape(messageType), xmlEscape(body), extension,
	)
	if err := c.writeStanza(stanza); err != nil {
		return "", err
	}
	return id, nil
}

func (m AgentMessageMetadata) Validate() error {
	if !isCanonicalUUID(m.Correlation) {
		return errors.New("agent-message correlation must be a canonical lowercase UUID")
	}
	if !isCanonicalUUID(m.OriginAgentID) {
		return errors.New("agent-message origin-agent-id must be a canonical lowercase UUID")
	}
	if m.Hop != 0 && m.Hop != 1 {
		return errors.New("agent-message hop must be 0 or 1")
	}
	return nil
}

func (p OccupantPresence) IsUnavailable() bool { return !p.Available }

func decodeDelay(decoder *xml.Decoder, start xml.StartElement) (Delay, error) {
	stamp := attr(start, "stamp")
	if err := decoder.Skip(); err != nil {
		return Delay{}, err
	}
	parsed, _ := parseDelayStamp(stamp)
	return Delay{Namespace: start.Name.Space, Stamp: parsed}, nil
}

func parseDelayStamp(raw string) (time.Time, error) {
	formats := []string{
		time.RFC3339Nano,
		"20060102T15:04:05",
		"2006-01-02T15:04:05",
	}
	for _, format := range formats {
		if parsed, err := time.Parse(format, raw); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid delayed-delivery stamp %q", raw)
}

func isDelayElement(start xml.StartElement) bool {
	return (start.Name.Local == "delay" && start.Name.Space == xep203DelayNamespace) ||
		(start.Name.Local == "x" && start.Name.Space == legacyDelayNamespace)
}

func decodeAgentMessage(decoder *xml.Decoder, start xml.StartElement) (AgentMessageMetadata, error) {
	hop, err := strconv.Atoi(attr(start, "hop"))
	if err != nil {
		_ = decoder.Skip()
		return AgentMessageMetadata{}, errors.New("agent-message hop is not decimal")
	}
	metadata := AgentMessageMetadata{
		Correlation:   attr(start, "correlation"),
		OriginAgentID: attr(start, "origin-agent-id"),
		Hop:           hop,
	}
	if err := decoder.Skip(); err != nil {
		return AgentMessageMetadata{}, err
	}
	if err := metadata.Validate(); err != nil {
		return AgentMessageMetadata{}, err
	}
	return metadata, nil
}

func decodeOccupantPresence(decoder *xml.Decoder, start xml.StartElement) (OccupantPresence, error) {
	from := attr(start, "from")
	room, nickname, _ := strings.Cut(from, "/")
	presence := OccupantPresence{
		From:      from,
		RoomJID:   room,
		Nickname:  nickname,
		Type:      attr(start, "type"),
		Available: attr(start, "type") != "unavailable" && attr(start, "type") != "error",
	}
	for {
		token, err := decoder.Token()
		if err != nil {
			return presence, err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			switch {
			case typed.Name.Local == "x" && typed.Name.Space == mucUserNamespace:
				if err := decodeMUCUser(decoder, typed, &presence); err != nil {
					return presence, err
				}
			case typed.Name.Local == "error":
				if err := decodePresenceError(decoder, typed, &presence); err != nil {
					return presence, err
				}
			case typed.Name.Local == "conflict" && typed.Name.Space == stanzaErrorNamespace:
				presence.NicknameConflict = true
				if err := decoder.Skip(); err != nil {
					return presence, err
				}
			default:
				if err := decoder.Skip(); err != nil {
					return presence, err
				}
			}
		case xml.EndElement:
			if typed.Name == start.Name {
				return presence, nil
			}
		}
	}
}

func decodePresenceError(decoder *xml.Decoder, start xml.StartElement, presence *OccupantPresence) error {
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			if typed.Name.Space == stanzaErrorNamespace && presence.ErrorCondition == "" {
				presence.ErrorCondition = typed.Name.Local
				if typed.Name.Local == "conflict" {
					presence.NicknameConflict = true
				}
			}
			if err := decoder.Skip(); err != nil {
				return err
			}
		case xml.EndElement:
			if typed.Name == start.Name {
				return nil
			}
		}
	}
}

func decodeMUCUser(decoder *xml.Decoder, start xml.StartElement, presence *OccupantPresence) error {
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			switch typed.Name.Local {
			case "item":
				presence.RealJID = attr(typed, "jid")
				presence.Affiliation = attr(typed, "affiliation")
				presence.Role = attr(typed, "role")
				if err := decoder.Skip(); err != nil {
					return err
				}
			case "status":
				code, err := strconv.Atoi(attr(typed, "code"))
				if err == nil {
					presence.StatusCodes = append(presence.StatusCodes, code)
					if code == 110 {
						presence.Self = true
					}
					if code == 409 {
						presence.NicknameConflict = true
					}
				}
				if err := decoder.Skip(); err != nil {
					return err
				}
			default:
				if err := decoder.Skip(); err != nil {
					return err
				}
			}
		case xml.EndElement:
			if typed.Name == start.Name {
				return nil
			}
		}
	}
}

func isCanonicalUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, ch := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}
