package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type responseState struct {
	sessionID string
	messages  map[string]*messageState
	nextText  strings.Builder
}

type messageState struct {
	messageID string
	textByID  map[string]string
	partType  map[string]string
	partOrder []string
	text      string
}

func newResponseState(sessionID string) *responseState {
	return &responseState{sessionID: sessionID, messages: map[string]*messageState{}}
}

func (s *responseState) Observe(data []byte) (AssistantResponse, bool, error) {
	event, ok, err := decodeEvent(data)
	if err != nil {
		return AssistantResponse{}, false, err
	}
	if !ok || event.Type == "" {
		return AssistantResponse{}, false, nil
	}
	if !event.matchesSession(s.sessionID) {
		return AssistantResponse{}, false, nil
	}
	switch event.Type {
	case "session.error":
		return AssistantResponse{}, false, fmt.Errorf("opencode session error: %s", describeValue(event.Properties["error"]))
	case "session.next.text.delta":
		if delta, ok := stringValue(event.Properties["delta"]); ok {
			s.nextText.WriteString(delta)
		}
	case "session.next.text.ended":
		text, _ := stringValue(event.Properties["text"])
		if strings.TrimSpace(text) == "" {
			text = s.nextText.String()
		}
		return completedResponse(s.sessionID, "", text)
	case "message.part.delta":
		s.observePartDelta(event.Properties)
	case "message.part.updated":
		s.observePartUpdated(event.Properties)
	case "message.updated":
		return s.observeMessageUpdated(event.Properties)
	}
	return AssistantResponse{}, false, nil
}

func (s *responseState) observePartDelta(properties map[string]any) {
	messageID, ok := stringValue(properties["messageID"])
	if !ok || messageID == "" {
		return
	}
	partID, _ := stringValue(properties["partID"])
	delta, _ := stringValue(properties["delta"])
	if partID == "" || delta == "" {
		return
	}
	msg := s.message(messageID)
	msg.notePart(partID)
	msg.textByID[partID] += delta
	msg.rebuildText()
}

func (s *responseState) observePartUpdated(properties map[string]any) {
	part, ok := objectValue(properties["part"])
	if !ok {
		return
	}
	messageID, ok := stringValue(part["messageID"])
	if !ok || messageID == "" {
		return
	}
	partID, ok := stringValue(part["id"])
	if !ok || partID == "" {
		return
	}
	partType, _ := stringValue(part["type"])
	msg := s.message(messageID)
	msg.partType[partID] = partType
	if partType != "text" {
		msg.rebuildText()
		return
	}
	msg.notePart(partID)
	if text, ok := stringValue(part["text"]); ok {
		msg.textByID[partID] = text
	} else if _, exists := msg.textByID[partID]; !exists {
		msg.textByID[partID] = ""
	}
	msg.rebuildText()
}

func (s *responseState) observeMessageUpdated(properties map[string]any) (AssistantResponse, bool, error) {
	info, ok := objectValue(properties["info"])
	if !ok {
		return AssistantResponse{}, false, nil
	}
	if !isAssistantMessage(info) {
		return AssistantResponse{}, false, nil
	}
	messageID, _ := stringValue(info["id"])
	text := extractAssistantText(info)
	if messageID != "" {
		msg := s.message(messageID)
		if strings.TrimSpace(text) != "" {
			msg.text = text
		}
		if strings.TrimSpace(text) == "" {
			text = msg.text
		}
	}
	if hasNonNil(info, "error") {
		return AssistantResponse{}, false, fmt.Errorf("opencode assistant error: %s", describeValue(info["error"]))
	}
	if !assistantCompleted(info) {
		return AssistantResponse{}, false, nil
	}
	return completedResponse(s.sessionID, messageID, text)
}

func (s *responseState) message(messageID string) *messageState {
	msg := s.messages[messageID]
	if msg == nil {
		msg = &messageState{messageID: messageID, textByID: map[string]string{}, partType: map[string]string{}}
		s.messages[messageID] = msg
	}
	return msg
}

func (m *messageState) rebuildText() {
	if len(m.textByID) == 0 {
		m.text = ""
		return
	}
	var b strings.Builder
	for _, partID := range m.partOrder {
		if m.partType[partID] != "text" {
			continue
		}
		text := m.textByID[partID]
		b.WriteString(text)
	}
	m.text = b.String()
}

func (m *messageState) notePart(partID string) {
	if _, ok := m.textByID[partID]; ok {
		return
	}
	m.partOrder = append(m.partOrder, partID)
}

func completedResponse(sessionID, messageID, text string) (AssistantResponse, bool, error) {
	if strings.TrimSpace(text) == "" {
		return AssistantResponse{}, false, errors.New("opencode assistant response completed without text")
	}
	return AssistantResponse{SessionID: sessionID, MessageID: messageID, Text: strings.TrimSpace(text), FinishedAt: time.Now()}, true, nil
}

type opencodeEvent struct {
	Type       string
	Properties map[string]any
}

func decodeEvent(data []byte) (opencodeEvent, bool, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return opencodeEvent{}, false, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return opencodeEvent{}, false, fmt.Errorf("decode opencode event: %w", err)
	}
	if nested, ok := objectValue(obj["data"]); ok {
		if _, hasType := obj["type"]; !hasType {
			obj = nested
		}
	}
	typeValue, _ := stringValue(obj["type"])
	properties, _ := objectValue(obj["properties"])
	if properties == nil {
		properties = map[string]any{}
	}
	return opencodeEvent{Type: typeValue, Properties: properties}, true, nil
}

func (e opencodeEvent) matchesSession(sessionID string) bool {
	if value, ok := stringValue(e.Properties["sessionID"]); ok {
		return value == sessionID
	}
	if info, ok := objectValue(e.Properties["info"]); ok {
		if value, ok := stringValue(info["sessionID"]); ok {
			return value == sessionID
		}
	}
	if part, ok := objectValue(e.Properties["part"]); ok {
		if value, ok := stringValue(part["sessionID"]); ok {
			return value == sessionID
		}
	}
	return false
}

func isAssistantMessage(info map[string]any) bool {
	if role, ok := stringValue(info["role"]); ok {
		return role == "assistant"
	}
	if typ, ok := stringValue(info["type"]); ok {
		return typ == "assistant"
	}
	return false
}

func assistantCompleted(info map[string]any) bool {
	if finish, ok := stringValue(info["finish"]); ok && finish != "" {
		return !isToolCallFinish(finish)
	}
	if timeObj, ok := objectValue(info["time"]); ok && hasNonNil(timeObj, "completed") {
		return true
	}
	return false
}

func isToolCallFinish(finish string) bool {
	switch strings.TrimSpace(strings.ToLower(finish)) {
	case "tool-calls", "tool_calls":
		return true
	default:
		return false
	}
}

func extractAssistantText(info map[string]any) string {
	if content, ok := sliceValue(info["content"]); ok {
		var b strings.Builder
		for _, item := range content {
			part, ok := objectValue(item)
			if !ok {
				continue
			}
			partType, _ := stringValue(part["type"])
			if partType != "text" {
				continue
			}
			if text, ok := stringValue(part["text"]); ok {
				b.WriteString(text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	if parts, ok := sliceValue(info["parts"]); ok {
		var b strings.Builder
		for _, item := range parts {
			part, ok := objectValue(item)
			if !ok {
				continue
			}
			partType, _ := stringValue(part["type"])
			if partType == "text" {
				if text, ok := stringValue(part["text"]); ok {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	}
	return ""
}

func objectValue(value any) (map[string]any, bool) {
	obj, ok := value.(map[string]any)
	return obj, ok
}

func sliceValue(value any) ([]any, bool) {
	slice, ok := value.([]any)
	return slice, ok
}

func stringValue(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok
}

func hasNonNil(obj map[string]any, key string) bool {
	value, ok := obj[key]
	return ok && value != nil
}

func describeValue(value any) string {
	if value == nil {
		return "<nil>"
	}
	if text, ok := value.(string); ok {
		return text
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}
