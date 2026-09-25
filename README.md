<h1 align="center">
  <img src="docs/asset/TtyServe.svg" alt="TtyServe" width="150">
</h1>

# TtyServe - Advanced and modern web terminal server

TtyServe is a persistent, multi-session, shareable web terminal server in Go — like [ttyd](https://github.com/tsl0922/ttyd),
but sessions survive disconnects and each client can hold multiple terminals in a
renameable tab bar. Users can share terminal with other authenticated users for readonly view or to work jointly with shared input.

<p align="center">
  <img src="docs/asset/demo2.gif" width="800" />
</p>

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
- **Multiple sessions + tabs** (toggleable). Tab bar on **top** or **right**
  (configurable). **Rename** a tab by double-clicking its title. Add/close tabs.
- **Split panels** — drag tabs onto the terminal area to arrange terminals side
  by side or stacked, each panel with its own set of tabs; resize with a
  draggable divider. See [Split panels](#split-panels).
- **Single-session mode** — flip`multi-session: false` for one terminal, no tabs.
- **Persistence off** — set`session-persistence: false` for ttyd-style ephemeral
  terminals that die on disconnect.
- **Tab sharing** (opt-in,`allow-sharing`) — right-click a tab to copy a share
  link; another authenticated user opens it and the terminal joins their tab
  list, view-only or with control. Access persists across their reloads until
  the owner closes the terminal or revokes. Persistent modes only.
- **Inline graphics support** - Sixel and iTerm inline protocol (IIP) graphics
- **Seamless clipboard** - linux like select-to-copy, middle-click to paste. Support for CLI programs (like modern AI coding harnesses) to set clipboard via OSC 52
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

Open http://localhost:7681 (default listen address:port).

Tip: A reverse proxy can serve TtyServe at any arbitrary base-path (http://some-host/to/a/subpath/) without requiring additional configuration at TtyServe's end. It is ensured that all assets and cookies are portable to work from any base-path.

## Configuration

Every option is available both in the YAML config and as a CLI flag of the
same name; flags override the file. `ttyserve --help` lists them all with
defaults. See `config.example.yaml` for the annotated file with detailed information for each options. The available options are:

| Option                                 | Meaning                                                                                                                                              |
| -------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `command`                            | what each terminal runs;`command` is a full shell-style line, e.g. `"/usr/bin/tmux new -A -s main"`.                                             |
| `working-dir`                        | working directory for terminal command (default: server's working directory)                                                                         |
| `env`                                | `env` entries may contain `${header.NAME}`, expanded from the request header at spawn time (e.g. `USER=${header.X-Forwarded-User}`)            |
| `listen`                             | IP address, interface name, or `unix://<path>` socket (default: all interfaces)                                                                    |
| `port`                               | TCP port (default 7681; ignored for unix sockets)                                                                                                    |
| `socket-perm`                        | unix socket permissions,`mode[:user[:group]]` (e.g. `660` or `0660::www-data`); default: umask decides                                         |
| `multi-session`                      | enable tabs / multiple terminals (default: true)                                                                                                     |
| `max-sessions-per-client`            | cap on tabs per client, incl. accepted shares (0 = unlimited)                                                                                        |
| `close-on-exit`                      | remove a session/tab when its command exits (default true); false keeps the tab and offers restart on Enter                                          |
| `auto-respawn`                       | start a new session immediately when the last one ends (default false = will show empty tab bar / 'restart on Enter' when multi-session off)         |
| `session-persistence`                | master on/off for persistence (default: true)                                                                                                        |
| `persistence-mode`                   | `user`, `short_term` or `proxy_header` (default: `short_term`)                                                                               |
| `idle-timeout`                       | short-term session lifetime when disconnected (default: 5m)                                                                                          |
| `users`                              | list of comma-separated `name:password` pairs. This is identity for `user` persistence mode. When set, basic auth is **required in every mode** — only the session identity differs (see [Authentication](#authentication)); empty means no auth at all |
| `auth-realm`                         | HTTP basic-auth realm shown in the browser's login prompt (default `ttyserve`)                                                                     |
| `proxy-header-name`                  | header carrying the identity in `proxy_header` mode (default `X-Forwarded-User`)                                                                 |
| `scrollback-bytes`                   | server-side replay buffer per session (default: 262144)                                                                                              |
| `allow-sharing`                      | let a user share a tab with another authenticated user via a link (default false), works in persistent modes only                                    |
| `public-share-links`                 | ceiling on links that skip sign-in: `none` (default), `readonly`, `all`. Owner still opts in per link; see [Sharing a terminal](#sharing-a-terminal) |
| `max-clients-per-session`            | shared-viewer cap (default: 0 = unlimited)                                                                                                           |
| `cookie-name`                        | short-term session cookie name (default `ttyserve_session`); change to run multiple instances on one host                                          |
| `cookie-secure`                      | mark the session cookie `Secure` — HTTPS only (default false; set true behind TLS)                                                                |
| `allow-origins`                      | extra websocket origins beyond same-host;`["*"]` = any                                                                                             |
| `tls-cert-file` / `tls-key-file`   | path of tls cert/key files, both needed, this enables TLS (applies to TCP and unix-socket listeners alike)                                           |
| `readonly`                           | `true` = read-only terminals, no client input (default: false)                                                                                     |
| `url-arg`                            | URL query params become command args (security-sensitive). e.g.,`/?arg1&arg2=5` -> runs as `command arg1 arg2=5`                                 |
| `url-env`                            | URL query params become command env vars (security-sensitive). e.g.,`/?arg1&arg2=5` -> sets ENV var as `arg1= arg2=5` for the command            |
| `ping-interval`                      | websocket keepalive ping period (default 20s); dead peers are reclaimed after 3× this                                                               |
| `font-size`                          | terminal font size in px (default 14)                                                                                                                |
| `dom-renderer`                       | DOM text rendering instead of canvas — use incase of any GPU blanking issue (default false)                                                         |
| `enable-graphics`                    | inline images via sixel + iTerm2 protocol (default true)                                                                                             |
| `disable-hyperlink`                  | `true` = links in output are not clickable (default false)                                                                                         |
| `middleclick-paste`                  | paste clipboard on middle click (default true)                                                                                                       |
| `clipboard-write`                    | let terminal programs set the system clipboard (reads ignored) via OSC 52 — tmux copy, vim `+clipboard`, ai coding harnesses, etc. (default true) |
| `bell`                               | terminal bell (BEL / `\a`): `none`, `sound`, `visual`, or `both` (default `sound`)                                                                  |
| `title`                              | browser page title (default `TtyServe`)                                                                                                            |
| `favicon`                            | custom icon: file path or base64 encoded `data:` URI (default: built-in)                                                                           |
| `tab-bar-position`                   | `top` or `right`                                                                                                                                 |
| `tab-show-psname` / `tab-show-cwd` | auto tab title parts: foreground process name / terminal's current-directory (default true for both)                                                 |
| `tab-show-ps1`                       | auto title tabs from the shell's window title (default false), overrides `tab-show-psname` / `tab-show-cwd` titling                              |
| `tab-title`                          | fixed tab title, disables all auto-tab-titling                                                                                                       |

Other CLI exclusive flags are:

| flag          | Meaning                                                |
| ------------- | ------------------------------------------------------ |
| -C, --config  | path to YAML config file to load configuration options |
| -V, --version | Print the version and build date of TtyServe binary    |
| --upgrade     | Self-update the binary to latest release               |
| -h, --help    | Print CLI help with all options                        |

The boolean configuration options (true/false) can be set in config YAML file, and/or through CLI flags. Boolean option that are `true` by default can be set to false in CLI as `--option=false`.

### Authentication

Authentication and *identity* are two separate things in TtyServe:

- **`users` decides whether a login is required.** If the list is non-empty,
  HTTP basic auth is required on **every** request, in **every** mode. If it is
  empty there is no authentication at all — anyone who can reach the port gets a
  terminal.
- **`persistence-mode` decides only what a session is tied to** (see below).

So `users` is never silently ignored: configuring it always gates access. The
only thing that varies is the identity a session hangs off —

| Mode | Identity | Role of `users` |
| ---- | -------- | --------------- |
| `user` | the username | **is** the identity (required in this mode) |
| `short_term` | signed cookie **+** username | gates access; the identity binds both, so a second person logging in on the same browser gets their own sessions rather than inheriting the first's |
| `proxy_header` | the header value | an extra door in front; the header stays authoritative for identity |
| persistence off | per-page ephemeral token | gates access; sessions stay ephemeral |

> When `persistence off` or in `short_term` mode, **do not combine an empty `users` with `allow-sharing` on an exposed port.**
> With no authentication a share link is a bearer token to a live shell for
> anyone who obtains the URL.

### Session lifecycle

- **user mode**: identity =`<user>:<password>`, username of HTTP basic-auth. Sessions persist across reconnects and
  across browsers (same credentials) until the shell exits.
- **proxy_header mode**: identity =`header:<value>` of`proxy-header-name`.
  Same lifecycle as user mode. Requests without the header get 403 (fail
  closed — a misconfigured proxy must not hand out sessions).
- **short_term mode**: identity =`cookie:<token>` from a signed HttpOnly session cookie set by TtyServe
  (`cookie:<token>|user:<name>` when`users` are configured, so the identity is
  per browser *and* per user).
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

## Split panels

Available whenever tabs are (`multi-session: true`). Drag a tab from the tab bar
onto the terminal area; a preview shows where it will land:

- **Near an edge** (solid half-panel preview) — the tab opens as a **new
  panel** on that side: left, right, top or bottom. Splits nest, so you can
  build any grid.
- **In the centre** (dashed whole-panel preview) — the tab **joins that
  panel**.

Each panel holds its own set of tabs and shows one of them at a time:

- **Clicking a tab** in the tab bar shows it in *its* panel, so you can cycle a
  panel through its tabs while the others stay put. Tabs currently on screen
  are tinted in the tab bar; the focused one has the blue border, and the
  focused panel has a thin blue outline. Clicking into a panel focuses it.
- **+ New** opens the new terminal in the focused panel; what that panel showed
  before stays in it, one click away.
- **Closing a tab** shows the tab you viewed before it in the same panel; a
  panel closes only when its last tab does.

To **unsplit**, drag a panel's tab into the centre of another panel — a panel
left with no tabs closes — or right-click a tab → **Unsplit panel**, which
moves all of that panel's tabs into its neighbour. The same menu offers
**Split right** / **Split down**, which also split a tab out of its own panel
(e.g. put B beside A when both are in one panel).

**Resizing** — drag the divider between two panels; double-click it to go back
to an even split. Each side keeps a minimum of 120px. Panels reflow live while
you drag, but the terminal programs are told their new size only once, when you
release: resizing on every mouse move would send a `SIGWINCH` per frame and make
full-screen programs like `vim` or `htop` redraw continuously.

A few details worth knowing:

- **Each panel's terminal has its own size** — `tput cols` in a half-width
  panel reports roughly half the columns.
- **A terminal is in at most one panel.** A session has a single PTY size, so it
  can't be shown at two widths at once.
- **The arrangement is per browser**, saved in `localStorage` alongside pinned
  tabs and restored on reload (closed terminals simply drop out of it). Another
  browser, or someone you share a tab with, keeps their own arrangement.
- **Narrow screens** (under 640px wide) show only the focused panel; the split
  comes back when the window is wide enough again.

## Sharing a terminal

Enable with `allow-sharing: true` (persistent modes only — `user`,
`short_term`, `proxy_header`). Off by default.

Once enabled, **right-click any tab you own → Share…** to open the share
dialog. You choose:

- **Access** —*View only* (the other person watches, cannot type) or*Allow
  control* (they can type into the same shell as you).
- **Link expires** — Never, or a time window (1 hour up to 7 days). This limits
  how long the link can be*accepted*; access already granted keeps working.
- **One-time** — the link stops working after the first person accepts it.
- **Anyone with the link — no sign-in required** — only shown when the server
  allows it (see below). Leave it unticked and the link stays private.

Click **Create link** and the shareable link is copied to your clipboard. Send
it to another user; unless you explicitly made the link public, they must sign
in as usual, and the terminal then appears as a tab in their list. Their access
is durable — it survives page reloads and re-logins — until you stop sharing or
the terminal closes.

### Links that skip sign-in

By default every recipient must authenticate. `public-share-links` sets a
server-wide **ceiling** on how far an owner may go beyond that:

| Value | Effect |
| ----- | ------ |
| `none` (default) | Can not skip auth for shared links. |
| `readonly` | An owner may mark a link public, but it is forced **view-only** — an anonymous visitor can watch, never type. |
| `all` | A public link may also grant control. |

The admin sets the ceiling; the owner still opts in **per link**, so enabling
this never silently changes the meaning of existing or future private links.

An anonymous visitor who opens a public link becomes a **guest**: the server
mints them an identity in a signed, HttpOnly cookie (so the token need not stay
in the address bar, and reloads keep working). Guests are deliberately
second-class and enforced server-side:

- A guest **can never create a terminal** — not via the tab bar, not via the
  API, not by opening the page. They only ever see the terminal the link
  granted.
- A guest can only accept a link explicitly marked public; a guest cookie
  cannot be replayed against a private link.
- View-only means view-only: input and resize are rejected on the server, not
  merely hidden in the UI.
- In every mode, guests are reaped when idle (by `idle-timeout`), since (unlike a username) their identity space is unbounded.
- **Stop sharing** disconnects and revokes guests exactly like anyone else.
- A guest sees a **Sign in** button instead of "+ New" to login as authenticated user. Login clears the anonymous identity and returns to the app. (The shared tab is not carried across sign-in; re-open the link if you still want it.)

> **A public link is a bearer credential.** Anyone who obtains the URL — from
> chat, a proxy access log, a browser history — gets in, with no account and no
> audit trail of who they were. Prefer `readonly`, set an expiry, and stop
> sharing when you are done. TtyServe sends `Referrer-Policy: no-referrer` so
> clicking a link in terminal output cannot leak the token to a third-party
> site, and the frontend strips the token from the address bar once accepted.

**What you'll see in the interface:**

- A tab's**share icon appears only once someone has actually joined** — a link
  that exists but hasn't been accepted yet does not badge the tab.
- The right-click menu shows**Sharing… (N users)** once people have joined, so
  you can tell at a glance how many are connected.
- **Stop sharing** revokes everything at once: it invalidates all outstanding
  links (even ones nobody accepted yet) and immediately disconnects everyone
  currently viewing. It appears as soon as a link exists, not only after
  someone joins. Your own terminal keeps running.
- Shared-in tabs (terminals shared*to* you) show an eye icon for view-only or a
  link icon for control, and you can't rename them (the title belongs to the
  owner).

Limits still apply: `max-clients-per-session` counts you plus every shared
viewer, and an accepted share counts against the accepter's
`max-sessions-per-client`. Because a share link is a capability, only hand it to
people you'd trust with that terminal, and prefer *View only* and expiring or
one-time links when you just want someone to watch.

When share is readonly, the terminal display width is determined by only owner's browser window size. However, if share is read+write (allow control) share, the terminal width can change according to both owner and shared users browser window size (whoever last renders the page or resizes the window).

Note: When displaying full screen terminal programs when sharing, like VI or even top, it is recommended to keep the main input user's browser window smaller than all viewers browser windows. This is because terminal display width is determined by input/main user, and if viewer widows is smaller, then texts would overflow in viewer's terminal, and would cause undesireable display.

## Robustness & protocol details

- **Slow-client isolation** — the PTY read loop never blocks on a consumer, so
  one slow or dead viewer can never pause the program or stall a shared session.
  A client that falls too far behind (its 1 MiB output buffer overflows) is
  repainted in place from the current scrollback, not dropped — see
  [Handling output floods](#handling-output-floods).
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
- **One query responder per session** — terminal capability queries (device
  attributes, cursor position, OSC colour) are answered by the *terminal*, and
  with multiple viewers every attached browser is a terminal. If they all
  answered, the program would consume one reply and each surplus reply would
  land at the shell prompt as phantom input: text nobody typed, plus spurious
  bells. The server therefore designates exactly one attached connection as the
  responder (promoting a survivor if it disconnects) and the rest suppress
  their replies at the parser, so real keystrokes are untouched. This applies to
  any multi-viewer situation — a shared terminal, or simply the same user with
  two browsers open.
- `GET /healthz` returns 200 without auth, for load balancers and monitors.

### Handling output floods

When a command dumps output faster than a client can render it (`yes`,
`cat huge.log`, a chatty build), something has to give — the program or the
viewer:

- **ttyd** and **VS Code's terminal** apply *backpressure*: they stop reading
  the PTY until the client catches up, the kernel tty buffer fills, and the
  program's `write()` blocks — so the program runs no faster than the (slowest)
  viewer. That's ideal when a terminal has exactly one viewer.
- **TtyServe deliberately doesn't.** Its sessions are **persistent, detachable,
  and shareable**: a session must keep running at full speed with *zero* clients
  attached (a detached build), and one slow viewer must never throttle the shell
  for the owner or other viewers. So the PTY read loop is decoupled from clients
  and always drains into the scrollback ring.

Instead of pausing the program, TtyServe pauses nothing. When a client's 1 MiB
buffer overflows, its stale backlog is discarded and it is **repainted in place**
from the current scrollback (an in-band full-reset replay frame) — no reconnect,
no dropped tab, and no `write()` ever blocked. During a sustained flood the
viewer just sees periodic repaints of the live screen — all a human can follow
at that rate anyway — and Ctrl-C still reaches the shell throughout. If a link
is so slow that even one frame can't be sent within the write deadline, that
connection falls back to the normal disconnect/reconnect path, without ever
affecting the program or other viewers.

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
