# goterm

A persistent, multi-session web terminal in Go — like [ttyd](https://github.com/tsl0922/ttyd),
but sessions survive disconnects and each client can hold multiple terminals in a
renameable tab bar.

## Features

- **Persistent sessions** — disconnect and reconnect; your shells keep running.
  Server-side scrollback is replayed so the screen repaints on reconnect.
- **Two persistence modes** (configurable):
  - `user` — sessions tied to an HTTP basic-auth user. Users/passwords are in the
    config. Sessions live until the shell exits or the server stops.
  - `short_term` — no login required. A session is bound to a secure, signed,
    HttpOnly cookie and reaped after `idle_timeout` of no active connection — so a
    brief network blip won't lose your work, but abandoned sessions get cleaned up.
- **Multiple sessions + tabs** (toggleable). Tab bar on **top** or **right**
  (configurable). **Rename** a tab by double-clicking its title. Add/close tabs.
- **Single-session mode** — flip `multi_session: false` for one terminal, no tabs.
- **Persistence off** — set `session_persistence: false` for ttyd-style ephemeral
  terminals that die on disconnect.
- ttyd-inspired options: read-only mode, shared viewers per session, custom
  command/args/env/working-dir, TLS, configurable title, ping interval.

## Build

Requires Go 1.22+.

```sh
cd goterm
go build -o goterm ./cmd/goterm
```

That's it — this builds offline. All dependencies are already resolved:
`go.sum` is committed, and the two `gopkg.in/*` modules (which need a network
redirect that some environments block) are vendored under `third_party/` and
wired up with `replace` directives in `go.mod`. The other deps
(`creack/pty`, `gorilla/websocket`, `google/uuid`) live in the module cache /
are fetched from GitHub directly.

If you'd rather pull everything fresh from the network instead of using the
vendored copies, delete the two `replace` lines at the bottom of `go.mod` and the
`third_party/` directory, then run `go mod tidy`.

Dependencies:
- `github.com/creack/pty` — PTY allocation
- `github.com/gorilla/websocket` — websockets
- `github.com/google/uuid` — session IDs
- `gopkg.in/yaml.v3` — config parsing (vendored)

The xterm.js front-end bundles are vendored under
`internal/server/web/static/` and embedded into the binary, so the build is fully
self-contained — no Node, no CDN at runtime.

### Verified

This was compiled and exercised end-to-end: `go build` + `go vet` clean, and a
live run confirmed cookie-based short-term sessions, the tab REST API
(list/create/rename/delete), websocket↔PTY I/O, and scrollback replay on
reconnect.

## Run

```sh
./goterm -config config.example.yaml
# or override the address:
./goterm -config config.example.yaml -listen :8080
```

Open http://localhost:7681.

## Configuration

See `config.example.yaml` for every option with defaults. Key ones:

| Option | Meaning |
|---|---|
| `session_persistence` | master on/off for persistence |
| `persistence_mode` | `user` or `short_term` |
| `multi_session` | enable tabs / multiple terminals |
| `tab_bar_position` | `top` or `right` |
| `users` | list of `{name, password}` for `user` mode |
| `idle_timeout` | short-term session lifetime when disconnected |
| `command` / `args` / `env` / `working_dir` | what each terminal runs |
| `write_enabled` | `false` = read-only terminals |
| `max_clients_per_session` | shared-viewer cap (0 = unlimited) |
| `scrollback_bytes` | server-side replay buffer per session |
| `tls_cert_file` / `tls_key_file` | enable HTTPS |
| `allow_origins` | extra websocket origins beyond same-host; `["*"]` = any |

## How it works

```
cmd/goterm            entrypoint: flags, config, graceful shutdown
internal/config       YAML config + defaults + validation
internal/auth         identity resolution: basic-auth user OR signed cookie
internal/terminal     PTY wrapper, output fan-out, scrollback ring buffer
internal/session      Client (an identity) owns N Sessions (tabs); Manager
                      creates them, enforces limits, reaps idle short-term clients
internal/server       HTTP routing, session REST API, websocket bridge, embedded UI
```

A **Client** is one identity (a username, or a cookie holder). Each client owns a
set of **Sessions**; each session is one PTY-backed shell shown in one tab. The
websocket at `/ws?session=<id>` bridges that PTY to xterm.js in the browser.

### Session lifecycle

- **user mode**: identity = `user:<name>`. Sessions persist across reconnects and
  across browsers (same credentials) until the shell exits.
- **short_term mode**: identity = `cookie:<token>` from a signed HttpOnly cookie.
  When all websockets for a client detach, an idle timer starts; after
  `idle_timeout` the client and all its sessions are killed by the reaper.
- **persistence off**: each page load is a fresh ephemeral identity; closing the
  socket discards the session.

## Robustness & protocol details

- **Slow-client isolation** — the PTY read loop never blocks on a consumer.
  Each websocket has a coalescing output buffer (capped at 1 MiB); a client
  that can't keep up is dropped and simply repaints from scrollback on
  reconnect. One dead viewer can never stall a shared session.
- **Sliding cookie expiration** — in short-term mode the cookie is refreshed on
  every request, so an actively-used browser never loses its identity; only
  idle clients age out (enforced server-side by the reaper regardless).
- **Dead-peer detection** — websocket read deadlines + pong handler reclaim
  connections to vanished clients within ~2× `ping_interval`.
- **Exit signalling** — when a shell exits, the server sends an `e` opcode;
  the browser marks the tab ended, verifies against the session API, and
  removes it (with a guard against respawn loops if the command crashes
  instantly). No infinite reconnect attempts to dead sessions.
- `GET /healthz` returns 200 without auth, for load balancers and monitors.

## Security notes

- Set `cookie_secure: true` and configure TLS when exposing over a network.
- Basic-auth passwords are compared in constant time but stored in plaintext in
  the config; protect the config file, or front the service with a reverse proxy
  that handles auth.
- The cookie signing secret is random per process, so short-term cookies are
  invalidated on restart (acceptable for short-lived sessions).
- Websocket origin policy is same-host by default. If the UI is served from a
  different host (e.g. behind a proxy), add that origin to `allow_origins`;
  `["*"]` disables the check entirely (not recommended with cookie auth).
