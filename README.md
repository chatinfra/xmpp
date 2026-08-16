# XMPP Go tools

This module contains the `xmpp` CLI and the `xmppd` bridge daemon.

## Public mirror

The public repository is <https://github.com/chatinfra/xmpp.git>. Its root is a mirror of this monorepo's canonical `go/xmpp/` subtree, so public checkouts contain `go.mod`, `go.sum`, `cmd/xmpp`, `cmd/xmppd`, `internal`, tests, the `spec/outputs/` schemas, and these docs directly at repository root.

`go/xmpp` in the ChatInfra monorepo remains canonical. Maintainers import accepted public changes back into the monorepo first, then update the public mirror from the canonical subtree. The public mirror declares `module github.com/chatinfra/xmpp` while canonical `go/xmpp` keeps the monorepo module path used by every module under `go/` (the `super/go/<tool>` form). The mirror sync applies a forward path transform to published `*.go`, `go.mod`, and `*.md` files, and maintainers apply the inverse reverse transform when importing an accepted public pull request, so mirror checkouts use the public module path in examples such as `github.com/chatinfra/xmpp/cmd/xmpp`.

## Build and test

```sh
go test ./...
go build ./cmd/xmpp
go build ./cmd/xmppd
```

The module declares Go 1.24 in `go.mod` and pins its dependencies in `go.sum`, so a fresh mirror clone builds both commands with no monorepo context. From a published mirror checkout, module-path installation uses the same public path shown after sync:

```sh
go install github.com/chatinfra/xmpp/cmd/xmpp@latest
```

## `xmpp` CLI output

`xmpp` help is deterministic terminal text. Successful command results are YAML documents, and `xmpp listen` emits a YAML stream with `---` separators between messages. Run `xmpp schemas` to list supported structured YAML schema IDs and paths under `spec/outputs/`. `--json` is unsupported.

## `xmppd` bridge daemon

`cmd/xmppd` runs one long-lived bridge for one lifecycle-managed agent. It starts only for an active managed account, obtains a short authenticated admission lease, and supports admitted direct chat plus gated XEP-0045 room traffic. Room joins request no history; delayed, duplicate, self-echoed, unaddressed, unresolved, ownerless, removed, and cross-tenant traffic is rejected. Agent correlation metadata permits only one automated response hop.

Required environment:

| Variable | Purpose |
| --- | --- |
| `XMPP_JID` | XMPP account JID used by this bridge |
| `XMPP_PASS` | XMPP account password |
| `XMPP_HOST` | XMPP server host |
| `OPENCODE_BASE_URL` or `OPENCODE_PORT` | Local OpenCode API endpoint |
| `OPENCODE_DIRECTORY` | OpenCode `directory` scope / workdir |
| `OPENCODE_AGENT_ID` or `AGENT_ID` | Immutable canonical agent lifecycle UUID |
| `OPENCODE_AGENT_NAME` or `OPENCODE_AGENT` | Mutable OpenCode v2 agent-name selector |
| `XMPP_ACCOUNT_STATUS` | Must be `ACTIVE` before startup |
| `XMPP_TENANT_ID` | Immutable canonical tenant UUID |
| `XMPP_MUC_HOST` | `mucHost` from the explicit bound Ejabberd authority |
| `XMPP_ROOM_JID` | Exact UUID-derived tenant room under the bound `mucHost` |
| `XMPP_ROOM_NICKNAME` | Exact UUID-derived immutable agent nickname |
| `CHATINFRA_INTERNAL_API_BASE_URL` | Internal authenticated admission-check API |
| `CHATINFRA_API_TOKEN` | Protected instance-scoped API token |
| `XMPPD_STATE_DIR` | Mode-`0700` directory for daemon state and `control.sock` |

Optional environment:

| Variable | Purpose |
| --- | --- |
| `OPENCODE_PROMPT_TIMEOUT` | Go duration bounding each prompt/response wait |
| `XMPP_PLAINTEXT` | Test/local-only plaintext XMPP connection toggle |
| `XMPPD_ROOM_ENABLED` | Requests room mode; current gate/room/affiliation admission is still required |
| `XMPPD_ADMISSION_PATH` | Admission snapshot path; defaults to `<XMPPD_STATE_DIR>/admission.json` |

## State files

`xmppd` writes state under `XMPPD_STATE_DIR`:

- `admission.json` is the credential-free, generation-bound, maximum-15-second authority projection checked against the authenticated internal API.
- `sessions.json` keeps direct sessions by remote bare JID and a separate room-scoped session. Matching-directory sessions survive restarts; rejected traffic never creates one.
- `status.json` is the exact schema-version-2 bounded runtime observation, including process/build identity, room and peer state, admission generations/expiry, activity, heartbeat, and paired bounded error fields. It is stale after 90 seconds and contains no credential.
- `control.sock` is a mode-`0600` runtime-user-local NDJSON socket. It admits only the same UID and validates every room or current-peer target server-side. This is runtime-user authority, not per-agent isolation between co-resident agents sharing that UID.

Messages for the same direct bare JID are serialized through that JID's OpenCode session; admitted groupchat uses the room session. OpenCode failures produce stable bounded status errors, no body reply, and do not stop later admitted traffic.

## `xmppd ctl`

`xmppd ctl list --state-dir "$XMPPD_STATE_DIR"` prints compact room and sorted peer rows. `xmppd ctl send --state-dir "$XMPPD_STATE_DIR" --room` and `--peer <bare-jid>` read the complete message body from standard input. The CLI never accepts a body argument, credential, or `--json` flag.

## OpenCode host layout

Source-backed OpenCode hosts use these stable paths. `xmpp` and `xmppd` share one source checkout:

| Path | Purpose |
| ---- | ------- |
| `/data/opencode/src/xmpp` | Editable source checkout cloned from the public mirror, shared by both tools |
| `/data/opencode/bin/xmpp` | Stable CLI launcher used by ChatInfra operations |
| `/data/opencode/bin/xmppd` | Stable bridge-daemon launcher referenced by rendered systemd units |
| `/data/opencode/.cache/xmpp` and `/data/opencode/.cache/xmppd` | Cached build output and source hash per tool |
| `/data/opencode/.cache/go-build` and `/data/opencode/.cache/go-mod` | OpenCode-owned Go build and module caches |

Each launcher rebuilds its command package (`./cmd/xmpp` or `./cmd/xmppd`) when the source hash changes or the cached binary is missing, then execs the cached binary with the original arguments. ChatInfra-controlled operations run the launchers as the `opencode` user so editable source is not executed as root.

For local host edits:

```sh
sudo -u opencode git -C /data/opencode/src/xmpp status
sudo -u opencode editor /data/opencode/src/xmpp/internal/...
sudo -u opencode /data/opencode/bin/xmpp --help
```

Installer and reconfigure flows preserve dirty `/data/opencode/src/xmpp` checkouts and log a warning instead of resetting local work. Clean checkouts are fetched and fast-forwarded from the configured mirror.

## Contribution workflow

1. Fork <https://github.com/chatinfra/xmpp.git>.
2. Clone your fork and create a topic branch.
3. Make changes, run `go test ./...`, and push the branch.
4. Open a pull request against the public mirror.

Accepted public changes are reviewed and imported into canonical `go/xmpp` in the ChatInfra monorepo before the public mirror is synchronized again. See [CONTRIBUTING.md](./CONTRIBUTING.md) for details.
