# Contributing to xmpp

Thanks for improving `xmpp` and `xmppd`.

## Repository model

- Public mirror: <https://github.com/chatinfra/xmpp.git>
- Canonical source: `go/xmpp/` inside the ChatInfra monorepo

The public mirror exists for inspection, forks, local host edits, and pull requests. It is downstream of the monorepo. Maintainers import accepted public PRs into canonical `go/xmpp` first, then synchronize the mirror.

Canonical `go/xmpp` keeps the monorepo module path used by every module under `go/` (the `super/go/<tool>` form). The mirror sync rewrites published `*.go`, `go.mod`, and `*.md` module-path references so the public repository declares `module github.com/chatinfra/xmpp` and public-facing examples use the mirror module path, for example:

```sh
go install github.com/chatinfra/xmpp/cmd/xmpp@latest
```

Maintainers apply the inverse transform when importing an accepted public PR, so canonical stays on the monorepo module path. Files outside `*.go`, `go.mod`, and `*.md` are published byte-for-byte without transform.

Because Markdown is transformed too, a published `*.md` file never mentions the canonical monorepo module path. Write module documentation so every sentence carrying a module path stays true after the rewrite — the mechanical half is enforced by the mirror tooling tests, but a sentence that transforms into a false claim is caught only in review.

## Fork-and-PR flow

```sh
git clone git@github.com:<you>/xmpp.git
cd xmpp
git checkout -b my-xmpp-change
go test ./...
git push -u origin my-xmpp-change
```

Then open a pull request against `chatinfra/xmpp`. Include any admission, room-gating, output-contract, or bridge-daemon implications in the PR description.

`xmpp` command results are YAML and `--json` is unsupported, so a change that adds or alters structured output must also update the matching schema under `spec/outputs/` and keep `xmpp schemas` consistent with the files that actually exist. `xmppd` state files are a contract too: `status.json` is a versioned, credential-free, bounded observation, and `control.sock` validates every room or peer target server-side. Do not widen either surface without saying so in the PR.

## Host-local edits

This module publishes two commands, and OpenCode hosts keep **one** editable mirror checkout at `/data/opencode/src/xmpp` shared by the `/data/opencode/bin/xmpp` CLI launcher and the `/data/opencode/bin/xmppd` bridge-daemon launcher. Each launcher builds its own command package (`./cmd/xmpp` or `./cmd/xmppd`) from that shared checkout into its own cache. Use this for diagnostics or emergency local patches:

```sh
sudo -u opencode git -C /data/opencode/src/xmpp status
sudo -u opencode /data/opencode/bin/xmpp --help
sudo -u opencode /data/opencode/bin/xmppd --help
```

Because the checkout is shared, an edit intended for one command still moves the source both launchers build from — run `go test ./...` in the checkout before leaving a host patched.

Reconfigure preserves dirty host checkouts and logs a warning instead of resetting local work. To return to mirror updates, commit/stash/revert local edits, then re-run reconfigure so the clean checkout can fast-forward. Because `xmppd` runs as a supervised user-systemd service, restart the affected bridge unit after editing daemon source so the launcher rebuilds and the new binary is picked up.

## Maintainer import and mirror sync

Maintainers import accepted public changes into canonical `go/xmpp`, preserving the monorepo as source of truth. For reviewed public mirror commits, generate an `mbox` patch and apply it with the monorepo helper so patch hunks are reverse-transformed back to the canonical module path:

```sh
git -C /path/to/chatinfra-xmpp-mirror format-patch -1 --stdout <accepted-commit> > /path/to/pr.patch
bin/import_sched_public_pr --tool xmpp /path/to/pr.patch /path/to/monorepo/go/xmpp
```

`bin/import_sched_public_pr` lives in the monorepo next to the mirror sync tooling. It rewrites patch hunk content for `*.go`, `go.mod`, and `*.md`, refuses binary or non-allowlisted path-bearing patches, and then runs `git am` in the target canonical worktree. The same helper serves the companion CLI mirrors with `--tool sched`, `--tool jmap`, `--tool specd`, or `--tool voice`.

For a one-off text-only patch that touches only `*.go`, `go.mod`, and `*.md`, the equivalent `git format-patch | sed | git am` flow is:

```sh
canonical_module='super/go'/'xmpp'
public_module_regex='github[.]com/chatinfra'/'xmpp'
git -C /path/to/chatinfra-xmpp-mirror format-patch -1 --stdout <accepted-commit> \
  | sed -E "/^[ +-]/ s#${public_module_regex}#${canonical_module}#g" \
  | git -C /path/to/monorepo/go/xmpp am -
```

Prefer the helper for normal imports; it validates patch shape before applying. After the monorepo change lands, run the mirror sync tooling from the monorepo root to update the public repository:

```sh
bin/sync_go_github --tool xmpp
```

The sync treats `./go/xmpp` as canonical source when run from the monorepo root, clones or reuses the public mirror checkout under `$SUPER_TMP_DIR/xmpp-public-mirror-checkout` or `./tmp/xmpp-public-mirror-checkout` via the SSH remote `git@github.com:chatinfra/xmpp.git`, refuses dirty canonical or mirror state, requires mirror `HEAD` to match its fetched upstream exactly, copies only this module's subtree into the public mirror checkout, commits generated changes, and pushes the mirror branch. Use `--dry-run` to run the same preflight checks and prepare the transformed staging tree without touching the mirror checkout.

Because this module publishes two commands, verify a published result by cloning the public mirror over `https://` and building both standalone:

```sh
go build -trimpath ./cmd/xmpp
go build -trimpath ./cmd/xmppd
```

Both must build with no monorepo context, since OpenCode host launchers clone and build the mirror alone.
