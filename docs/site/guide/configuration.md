# Configuration

Operational settings live in the daemon's database and are edited with `fletcher
settings`, with no editing of systemd unit files. Settings take effect on the
next daemon start.

## List and set

```sh
fletcher settings list     # every setting, its current value (or default), and help
```

Set one and apply it with a restart:

```sh
fletcher settings set log_level debug
fletcher daemon restart
fletcher settings unset log_level     # revert to the flag/env default
```

## Settable keys

| Key | What it does |
|---|---|
| `runtime` | Job runtime: `firecracker` (default on KVM), `runc`, or mock |
| `snapshot` | Snapshot backend (`reflink` for Firecracker, `btrfs` for runc) |
| `btrfs_root` | Root directory for btrfs snapshots |
| `default_image` | Base image `job` / `session create` use when `--image` is omitted (`fletcher-base` out of the box, set empty to make `--image` required) |
| `default_agent` | Agent the app's create form suggests by default: `pi` / `claude` / `codex` (a hint for clients; the agent itself is baked into the image) |
| `public_endpoint` | Your public `host:port` for WireGuard (overrides UPnP discovery) |
| `wireguard_port` | UDP port for the built-in tunnel (default 51820) |
| `pairing_port` | Public TCP port the iOS app dials to complete pairing over TLS (default 51821; see [Pair a device](/guide/pairing)) |
| `no_upnp` | Disable the automatic UPnP port-forward attempt |
| `gateway_listen` | Listen address for the model gateway (default `127.0.0.1:11500`) |
| `mcp_listen` | Listen address for the MCP server (default `127.0.0.1:11600`) |
| `remote_api_listen` | Mode B: extra `host:port` to expose the token-gated API on, beyond the WireGuard tunnel (e.g. your Tailscale IP, so the app reaches the box over a VPN you already run; see [Networking](/advanced/networking#mode-b-bring-your-own-vpn)) |
| `public_web` | Opt in to public HTTPS on ports 80/443 (see [Public web](/advanced/public-web)) |
| `acme_staging` | Use Let's Encrypt staging certs while testing |
| `log_level` | Daemon log level |
| `credentials_dir` | Where the daemon stores credentials |
| `session_idle_timeout` | Auto-stop idle sessions after this duration (default 30m, `0` disables) |
| `session_max_count` | Cap on how many sessions can exist (default 10) |
| `session_max_disk_gb` | Cap on total session disk (default 50) |

Run `fletcher settings list` for the authoritative, up-to-date list with each
key's current value and help text.

## What stays outside settings

Only **bootstrap** config (where the database, socket, and age key live) stays in
the flag/env layer, because it's needed to open the database these settings live
in. Everything else is a setting.

## Secrets

Model keys and other secrets are set separately and stored encrypted by the
daemon. They're never part of `settings`.

```sh
fletcher secret set anthropic_api_key sk-ant-...
```

The gateway uses them to reach models on an agent's behalf. The values never
enter a VM. See [Your first agent](/guide/first-agent).
