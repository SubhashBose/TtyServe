# TtyServe

A persistent, multi-session web terminal server in Go — like [ttyd](https://github.com/tsl0922/ttyd),
but sessions survive disconnects and each client can hold multiple terminals in a
renameable tab bar.

## Features

- **Persistent sessions** — disconnect and reconnect; your shells keep running.
  Server-side scrollback is replayed so the screen repaints on reconnect.
- **Three persistence modes** (configurable):
  - `short_term` — no login required. A session is bound to a secure, signed,
    HttpOnly cookie and reaped after`idle-timeout` of no active connection — so a
    brief network blip won't lose your work, but abandoned sessions get cleaned up.
  - `user` — sessions tied to an HTTP basic-auth user. Users/passwords are in the
    config. Sessions live until the shell exits or the server stops.
  - `proxy_header` — sessions tied to the value of a header set by an
    authenticating reverse proxy (`proxy-header-name`, default`X-Forwarded-User`); like`user` mode but the proxy does the auth. Bind to`unix://` or`127.0.0.1` so the header can't be spoofed directly.`user` and`proxy_header` persistence modes survives different browser logins as well.
    Users with same login credentials will find terminal sessions persistent across browsers.
- **Multiple sessions + tabs** (toggleable). Tab bar on**top** or**right**
  (configurable).**Rename** a tab by double-clicking its title. Add/close tabs.
- **Single-session mode** — flip`multi-session: false` for one terminal, no tabs.
- **Persistence off** — set`session-persistence: false` for ttyd-style ephemeral
  terminals that die on disconnect.
- **Tab sharing** (opt-in, `allow-sharing`) — right-click a tab to copy a share
  link; another authenticated user opens it and the terminal joins their tab
  list, view-only or with control. Access persists across their reloads until
  the owner closes the terminal or revokes. Persistent modes only.
- Inline graphics support - Sixel and iTerm inline protocol (IIP) graphics
- Other options: read-only mode, shared viewers per session, custom
  command/args/env/working-dir, TLS, configurable title, ping interval, unix socket listen, socket permission, header value to env variable.

## System-wide installation

Pre-compiled standalone binary can be downloaded and run without any installation or third-party dependency.

If you prefer system wide installation, run this CMD for automated download and setup

```bash
sudo wget $(curl -sL https://install-scripts.bose.dev/detect-platform.sh | sh -s -- SubhashBose/TtyServe ttyserve) -O /usr/local/bin/ttyserve && sudo chmod +x /usr/local/bin/ttyserve
```

## Upgrade

When new release is available it can be upgraded as

```bash
ttyserve --upgrade
```

## Run

```sh
./ttyserve --config config.example.yaml
# or override options on the command line:
./ttyserve --config config.example.yaml --listen 127.0.0.1 --port 8080
# config file is optional; flags alone work too:
./ttyserve --port 8080 --close-on-exit=false
# common options have short forms (-c command, -p port, -l listen, ...):
./ttyserve -p 8080 -c "/usr/bin/tmux new -A -s main"

./ttyserve --version   # print version (and build date) and exit
./ttyserve --upgrade   # download and install the latest release, then exit
```

Open http://localhost:7681.

## Configuration

Every option is available both in the YAML config and as a CLI flag of the
same name; flags override the file. `ttyserve --help` lists them all with
defaults. See `config.example.yaml` for the annotated file. Key options:

| Option                                  | Meaning                                                                                                                                                                                                                                            |
| --------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `listen`                              | IP address, interface name, or `unix://<path>` socket (default: all interfaces)                                                                                                                                                                  |
| `port`                                | TCP port (default 7681; ignored for unix sockets)                                                                                                                                                                                                  |
| `socket-perm`                         | unix socket permissions,`mode[:user[:group]]` (e.g. `660` or `0660::www-data`); default: umask decides                                                                                                                                       |
| `command` / `env` / `working-dir` | what each terminal runs;`command` is a full shell-style line, e.g. `"/usr/bin/tmux new -A -s main"`. `env` entries may contain `${header.NAME}`, expanded from the request header at spawn time (e.g. `USER=${header.X-Forwarded-User}`) |
| `session-persistence`                 | master on/off for persistence (default: true)                                                                                                                                                                                                      |
| `persistence-mode`                    | `user`, `short_term` or `proxy_header` (default: `short_term`)                                                                                                                                                                             |
| `proxy-header-name`                   | header carrying the identity in `proxy_header` mode (default `X-Forwarded-User`)                                                                                                                                                               |
| `users`                               | list of comma-separated `name:password` pairs for `user` mode; with persistence off they act as a plain access gate                                                                                                                            |
| `idle-timeout`                        | short-term session lifetime when disconnected (default: 5m)                                                                                                                                                                                        |
| `multi-session`                       | enable tabs / multiple terminals (default: true)                                                                                                                                                                                                   |
| `tab-bar-position`                    | `top` or `right`                                                                                                                                                                                                                               |
| `readonly`                            | `true` = read-only terminals, no client input                                                                                                                                                                                                    |
| `url-arg` / `url-env`               | URL query params become command args / env vars (mutually exclusive; security-sensitive)                                                                                                                                                           |
| `max-clients-per-session`             | shared-viewer cap (0 = unlimited)                                                                                                                                                                                                                  |
| `scrollback-bytes`                    | server-side replay buffer per session (default: 262144)                                                                                                                                                                                            |
| `font-size`                           | terminal font size in px (default 14)                                                                                                                                                                                                              |
| `enable-graphics`                     | inline images via sixel + iTerm2 protocol (default true)                                                                                                                                                                                           |
| `dom-renderer`                        | DOM text rendering instead of canvas — use incase of any GPU blanking issue (default false)                                                                                                                                                       |
| `disable-hyperlink`                   | `true` = links in output are not clickable (default false)                                                                                                                                                                                       |
| `middleclick-paste`                   | paste clipboard on middle click (default true)                                                                                                                                                                                                     |
| `allow-sharing`                       | let a user share a tab with another authenticated user via a link (default false; persistent modes only)                                                                                                                                            |
| `tab-show-psname` / `tab-show-cwd`  | auto tab title parts: process name / dir (default true)                                                                                                                                                                                            |
| `tab-show-ps1`                        | title tabs from the shell's window title (default false)                                                                                                                                                                                           |
| `tab-title`                           | fixed tab title, disables auto-titling                                                                                                                                                                                                             |
| `favicon`                             | custom icon: file path or base64 encoded `data:` URI (default: built-in)                                                                                                                                                                         |
| `tls-cert-file` / `tls-key-file`    | enable TLS (applies to TCP and unix-socket listeners alike)                                                                                                                                                                                        |
| `allow-origins`                       | extra websocket origins beyond same-host;`["*"]` = any                                                                                                                                                                                           |

### Session lifecycle

- **user mode**: identity =`user:<name>`. Sessions persist across reconnects and
  across browsers (same credentials) until the shell exits.
- **proxy_header mode**: identity =`header:<value>` of`proxy-header-name`.
  Same lifecycle as user mode. Requests without the header get 403 (fail
  closed — a misconfigured proxy must not hand out sessions).
- **short_term mode**: identity =`cookie:<token>` from a signed HttpOnly cookie.
  When all websockets for a client detach, an idle timer starts; after`idle-timeout` the client and all its sessions are killed by the reaper.
- **persistence off**: each page load is a fresh ephemeral identity; closing the
  socket discards the session. If`users` are configured, they gate access
  (HTTP basic-auth) without changing these semantics.

### Tab titles

Unless renamed (double-click) or created with an explicit title, tabs are
auto-titled `<process> <dir>` — the foreground process (via `TIOCGPGRP`;
Linux and macOS) and the shell's working directory basename. Options, by
precedence:

- `tab-title` — fixed title for every tab, no auto-titling.
- `tab-show-ps1` — title tabs with whatever the shell announces via OSC 0/2
  window-title sequences (most PS1 setups emit`user@host:dir`; works over
  ssh when the remote shell emits titles). Client-side, off by default.
- `tab-show-psname` /`tab-show-cwd` — the`<process> <dir>` parts, each
  individually toggleable (default on).

The directory comes from two sources, best first:

- **OSC 7** — if the shell announces its directory (`ESC ]7;file://host/path`),
  that wins: it is exact and correct even over`ssh` or inside containers.
  Zsh/VTE/starship setups often emit it already; for plain bash add:
  ```sh
  PROMPT_COMMAND='printf "\e]7;file://%s%s\e\\" "$HOSTNAME" "$PWD"'
  ```
- **`/proc/<pid>/cwd`** of the direct child, as fallback (Linux only; checked
  every few seconds, and only for sessions that produced output and have a
  viewer attached).

## Robustness & protocol details

- **Slow-client isolation** — the PTY read loop never blocks on a consumer.
  Each websocket has a coalescing output buffer (capped at 1 MiB); a client
  that can't keep up is dropped and simply repaints from scrollback on
  reconnect. One dead viewer can never stall a shared session.
- **Sliding cookie expiration** — in short-term mode the cookie is refreshed on
  every request, so an actively-used browser never loses its identity; only
  idle clients age out (enforced server-side by the reaper regardless).
- **Dead-peer detection** — websocket read deadlines + pong handler reclaim
  connections to vanished clients within ~3×`ping-interval` (=60s by
  default) — which is also the window within which a connection that
  survives a network outage resumes seamlessly.
- **Exit signalling** — when a shell exits, the server sends an`e` opcode;
  the browser marks the tab ended, verifies against the session API, and
  removes it (with a guard against respawn loops if the command crashes
  instantly). No infinite reconnect attempts to dead sessions. With`close-on-exit: false` the session is kept: the tab shows`[session ended]` and pressing Enter respawns the command in place
  (`POST /sessions/{id}/restart`). When the*last* session ends, nothing
  respawns by default — multi-session leaves the tab bar empty,
  single-session offers restart on Enter; set`auto-respawn: true` for the
  old immediately-start-a-new-one behavior.
- `GET /healthz` returns 200 without auth, for load balancers and monitors.

## Security notes

- Set`cookie-secure: true` and configure TLS when exposing over a network.
- Basic-auth passwords are compared in constant time but stored in plaintext in
  the config; protect the config file, or front the service with a reverse proxy
  that handles auth.
- The cookie signing secret is random per process, so short-term cookies are
  invalidated on restart (acceptable for short-lived sessions).
- `url-arg` /`url-env` let any client who can load the page append arguments
  or environment variables to the spawned command. Treat them as remote
  command-line access: enable only behind authentication you trust, and never
  with`allow-origins: ["*"]`.
- Behind a path-mounting proxy that hosts other apps on the same origin, set`cookie-path` to the mount prefix so sibling apps never receive the session
  cookie.
- `proxy_header` mode trusts the header blindly — it is only safe when clients
  cannot reach ttyserve directly. Bind to a`unix://` socket or`127.0.0.1`,
  and configure the proxy to strip/overwrite the header on incoming requests.
- Websocket origin policy is same-host by default. If the UI is served from a
  different host (e.g. behind a proxy), add that origin to`allow-origins`;`["*"]` disables the check entirely (not recommended with cookie auth).
