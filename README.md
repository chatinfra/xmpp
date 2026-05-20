# XMPP Go tools

This module contains the `xmpp` CLI and the `xmppd` bridge daemon.

## `xmppd` bridge daemon

`cmd/xmppd` runs one long-lived bridge for one XMPP-attached agent. It connects the configured XMPP account, receives inbound one-to-one chat messages, submits each non-empty body to the agent's local OpenCode v2 API, waits for the completed assistant response, and sends the response back to the sender's full JID.

Required environment:

| Variable | Purpose |
| --- | --- |
| `XMPP_JID` | XMPP account JID used by this bridge |
| `XMPP_PASS` | XMPP account password |
| `XMPP_HOST` | XMPP server host |
| `OPENCODE_BASE_URL` or `OPENCODE_PORT` | Local OpenCode API endpoint |
| `OPENCODE_DIRECTORY` | OpenCode `directory` scope / workdir |
| `OPENCODE_AGENT` or `AGENT_ID` | Agent target passed to OpenCode prompt calls |
| `XMPPD_STATE_DIR` | Directory for daemon state files |

Optional environment:

| Variable | Purpose |
| --- | --- |
| `OPENCODE_PROMPT_TIMEOUT` | Go duration bounding each prompt/response wait |
| `XMPP_PLAINTEXT` | Test/local-only plaintext XMPP connection toggle |

## State files

`xmppd` writes state under `XMPPD_STATE_DIR`:

- `sessions.json` maps each remote bare JID to its OpenCode session ID. The daemon reloads this file on startup so conversations survive restarts; if OpenCode rejects a stale session, the mapping is recreated.
- `status.json` reports bridge health: XMPP connection state, last inbound timestamp, last reply timestamp, latest error, active session count, and daemon start time.

Messages for the same remote bare JID are serialized through that JID's OpenCode session. Messages for different JIDs may run concurrently. OpenCode errors and prompt timeouts are log-only: the daemon records `lastError` in `status.json`, sends no XMPP reply for that prompt, and continues serving later messages.
