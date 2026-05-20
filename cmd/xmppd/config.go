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
	XMPP              xmpp.Config
	OpencodeBaseURL   string
	OpencodeDirectory string
	AgentID           string
	StateDir          string
	PromptTimeout     time.Duration
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
	agentID := firstEnv("OPENCODE_AGENT", "AGENT_ID")
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
		XMPP:              xmppCfg,
		OpencodeBaseURL:   baseURL,
		OpencodeDirectory: directory,
		AgentID:           agentID,
		StateDir:          stateDir,
		PromptTimeout:     timeout,
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
		missing = append(missing, "OPENCODE_AGENT or AGENT_ID")
	}
	if strings.TrimSpace(c.StateDir) == "" {
		missing = append(missing, "XMPPD_STATE_DIR")
	}
	if len(missing) > 0 {
		return errors.New("missing required environment: " + strings.Join(missing, ", "))
	}
	return nil
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
