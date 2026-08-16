package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chatinfra/xmpp/internal/xmpp"
)

type Config struct {
	XMPP               xmpp.Config
	OpencodeBaseURL    string
	OpencodeDirectory  string
	AgentID            string
	AgentName          string
	StateDir           string
	PromptTimeout      time.Duration
	AccountStatus      string
	TenantID           string
	MUCHost            string
	RoomJID            string
	RoomNickname       string
	RoomEnabled        bool
	AdmissionPath      string
	InternalAPIBaseURL string
	InternalAPIToken   string
}

func ConfigFromEnv() (Config, error) {
	xmppCfg := xmpp.ConfigFromEnv()
	xmppCfg.Plaintext = envBool("XMPP_PLAINTEXT")
	baseURL := strings.TrimSpace(os.Getenv("OPENCODE_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("OPENCODE_URL"))
	}
	if baseURL == "" {
		port := strings.TrimSpace(os.Getenv("OPENCODE_PORT"))
		if port != "" {
			host := strings.TrimSpace(os.Getenv("OPENCODE_HOST"))
			if host == "" {
				host = "127.0.0.1"
			}
			baseURL = "http://" + host + ":" + port
		}
	}
	directory := firstEnv("OPENCODE_DIRECTORY", "OPENCODE_DIR")
	agentID := firstEnv("OPENCODE_AGENT_ID", "AGENT_ID")
	agentName := firstEnv("OPENCODE_AGENT_NAME", "OPENCODE_AGENT", "AGENT_NAME")
	stateDir := firstEnv("XMPPD_STATE_DIR", "STATE_DIR")
	timeout := 0 * time.Second
	if raw := strings.TrimSpace(os.Getenv("OPENCODE_PROMPT_TIMEOUT")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("OPENCODE_PROMPT_TIMEOUT: %w", err)
		}
		timeout = parsed
	}
	cfg := Config{
		XMPP:               xmppCfg,
		OpencodeBaseURL:    baseURL,
		OpencodeDirectory:  directory,
		AgentID:            agentID,
		AgentName:          agentName,
		StateDir:           stateDir,
		PromptTimeout:      timeout,
		AccountStatus:      strings.ToUpper(firstEnv("XMPP_ACCOUNT_STATUS")),
		TenantID:           firstEnv("XMPP_TENANT_ID", "TENANT_ID"),
		MUCHost:            normalizeDomain(firstEnv("XMPP_MUC_HOST", "MUC_HOST")),
		RoomJID:            firstEnv("XMPP_ROOM_JID"),
		RoomNickname:       firstEnv("XMPP_ROOM_NICKNAME"),
		RoomEnabled:        envBool("XMPPD_ROOM_ENABLED"),
		AdmissionPath:      firstEnv("XMPPD_ADMISSION_PATH"),
		InternalAPIBaseURL: firstEnv("CHATINFRA_INTERNAL_API_BASE_URL", "CHATINFRA_API_BASE_URL"),
		InternalAPIToken:   firstEnv("CHATINFRA_API_TOKEN"),
	}
	if cfg.AdmissionPath == "" && cfg.StateDir != "" {
		cfg.AdmissionPath = cfg.StateDir + "/admission.json"
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var missing []string
	if strings.TrimSpace(c.XMPP.JID) == "" {
		missing = append(missing, "XMPP_JID")
	}
	if strings.TrimSpace(c.XMPP.Password) == "" {
		missing = append(missing, "XMPP_PASS")
	}
	if strings.TrimSpace(c.OpencodeBaseURL) == "" {
		missing = append(missing, "OPENCODE_BASE_URL or OPENCODE_PORT")
	} else if parsed, err := url.Parse(c.OpencodeBaseURL); err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("invalid opencode base URL %q", c.OpencodeBaseURL)
	}
	if strings.TrimSpace(c.OpencodeDirectory) == "" {
		missing = append(missing, "OPENCODE_DIRECTORY")
	}
	if strings.TrimSpace(c.AgentID) == "" {
		missing = append(missing, "OPENCODE_AGENT_ID or AGENT_ID")
	} else if !isCanonicalUUID(c.AgentID) {
		return fmt.Errorf("OPENCODE_AGENT_ID must be a canonical lowercase UUID")
	}
	if strings.TrimSpace(c.AgentName) == "" {
		missing = append(missing, "OPENCODE_AGENT_NAME or OPENCODE_AGENT")
	}
	if strings.TrimSpace(c.StateDir) == "" {
		missing = append(missing, "XMPPD_STATE_DIR")
	}
	if c.AccountStatus == "" {
		missing = append(missing, "XMPP_ACCOUNT_STATUS")
	} else if c.AccountStatus != "ACTIVE" {
		return fmt.Errorf("XMPP_ACCOUNT_STATUS must be ACTIVE before xmppd starts")
	}
	if c.TenantID == "" {
		missing = append(missing, "XMPP_TENANT_ID")
	} else if !isCanonicalUUID(c.TenantID) {
		return fmt.Errorf("XMPP_TENANT_ID must be a canonical lowercase UUID")
	}
	if c.MUCHost == "" {
		missing = append(missing, "XMPP_MUC_HOST")
	}
	if c.RoomJID == "" {
		missing = append(missing, "XMPP_ROOM_JID")
	} else if normalizeBareJID(c.RoomJID) != c.RoomJID || (c.TenantID != "" && c.MUCHost != "" && c.RoomJID != expectedRoomJID(c.TenantID, c.MUCHost)) {
		return fmt.Errorf("XMPP_ROOM_JID does not match the tenant UUID and bound mucHost")
	}
	if c.RoomNickname == "" {
		missing = append(missing, "XMPP_ROOM_NICKNAME")
	} else if c.AgentID != "" && c.RoomNickname != expectedRoomNickname(c.AgentID) {
		return fmt.Errorf("XMPP_ROOM_NICKNAME does not match the immutable agent UUID")
	}
	if c.AdmissionPath == "" {
		missing = append(missing, "XMPPD_ADMISSION_PATH")
	}
	if strings.TrimSpace(c.InternalAPIBaseURL) == "" {
		missing = append(missing, "CHATINFRA_INTERNAL_API_BASE_URL")
	} else if parsed, err := url.Parse(c.InternalAPIBaseURL); err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("invalid internal API base URL")
	}
	if c.InternalAPIToken == "" {
		missing = append(missing, "CHATINFRA_API_TOKEN")
	}
	if len(missing) > 0 {
		return errors.New("missing required environment: " + strings.Join(missing, ", "))
	}
	return nil
}

func expectedRoomJID(tenantID, mucHost string) string {
	return "agents-" + strings.ReplaceAll(tenantID, "-", "") + "@" + normalizeDomain(mucHost)
}

func expectedRoomNickname(agentID string) string {
	return "agent-" + strings.ReplaceAll(agentID, "-", "")
}

func normalizeDomain(value string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}

func normalizeBareJID(value string) string {
	bare, _, _ := strings.Cut(strings.TrimSpace(value), "/")
	local, domain, ok := strings.Cut(bare, "@")
	if !ok || local == "" || domain == "" {
		return ""
	}
	return strings.ToLower(local) + "@" + normalizeDomain(domain)
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

func envBool(key string) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return false
	}
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}
