/* ttyserve frontend controller */
(function () {
  "use strict";

  const cfg = window.TTYSERVE;
  const tabbar = document.getElementById("tabbar");
  const newtabBtn = document.getElementById("newtab");
  const termsEl = document.getElementById("terminals");
  const statusEl = document.getElementById("status");

  // sessionId -> { term, fit, ws, pane, tabEl, titleEl, connected, reconnectTimer }
  const panes = new Map();
  let activeId = null;
  let draggedId = null; // tab currently being dragged, if any

  const LS_ACTIVE = "ttyserve_active_tab";
  function rememberActive(id) {
    try { localStorage.setItem(LS_ACTIVE, id); } catch (e) {}
  }
  function recallActive() {
    try { return localStorage.getItem(LS_ACTIVE); } catch (e) { return null; }
  }

  // Pinned tabs (per-browser, like the active-tab memory): a pinned tab has
  // no close button, guarding against accidental closes.
  const LS_PINNED = "ttyserve_pinned_tabs";
  function loadPinned() {
    try { return new Set(JSON.parse(localStorage.getItem(LS_PINNED) || "[]")); }
    catch (e) { return new Set(); }
  }
  function savePinned(set) {
    try { localStorage.setItem(LS_PINNED, JSON.stringify([...set])); } catch (e) {}
  }
  const pinnedTabs = loadPinned();

  // showStatus displays a transient toast; pass sticky=true to keep it
  // shown until the next status update (used for ongoing states like
  // "reconnecting").
  function showStatus(msg, sticky) {
    statusEl.textContent = msg;
    statusEl.classList.add("show");
    clearTimeout(showStatus._t);
    if (!sticky) {
      showStatus._t = setTimeout(() => statusEl.classList.remove("show"), 1500);
    }
  }

  // Keep a sticky "reconnecting…" up while any tab is down and retrying.
  function refreshConnStatus() {
    // (Ephemeral tabs count too: their respawn probe loop is a form of
    // reconnecting, and awaitRestart/exited states are excluded below.)
    let down = 0;
    for (const e of panes.values()) {
      if (!e.connected && !e.exited && !e.awaitRestart) down++;
    }
    if (down > 0) {
      showStatus(down > 1 ? "reconnecting… (" + down + " tabs)" : "reconnecting…", true);
    }
  }

  // ---- protocol opcodes (must match server ws.go) ----
  const C_INPUT = "0", C_RESIZE = "1", C_PING = "2";
  const S_OUTPUT = 0x6f; // 'o'
  const S_EXIT = 0x65;   // 'e' — session stream ended
  const S_REPLAY = 0x72; // 'r' — scrollback repaint (suppress query replies)
  const S_PONG = 0x70;   // 'p' — reply to our C_PING (liveness)

  // In persistence-off mode there is no cookie; the page identity travels as
  // ?eid=... on every request so they resolve to the same server client.
  function withEid(path) {
    if (cfg.persistence || !cfg.ephemeralId) return path;
    return path + (path.includes("?") ? "&" : "?") + "eid=" + encodeURIComponent(cfg.ephemeralId);
  }

  // timeoutMs (optional) aborts the request — used by recurring/probing
  // requests so an unresponsive network can't leave them hanging for the
  // browser's multi-minute default and piling up behind each other.
  async function api(method, path, body, timeoutMs) {
    path = withEid(path);
    const opts = { method, headers: {} };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    let timer = null;
    if (timeoutMs && window.AbortController) {
      const ctl = new AbortController();
      opts.signal = ctl.signal;
      timer = setTimeout(() => ctl.abort(), timeoutMs);
    }
    try {
      const res = await fetch(path, opts);
      if (!res.ok) {
        // Surface the server's message (e.g. "session limit reached ...").
        let msg = "";
        try { msg = (await res.text()).trim(); } catch (e) {}
        throw new Error(msg || method + " " + path + " -> " + res.status);
      }
      if (res.status === 204) return null;
      return res.json();
    } finally {
      clearTimeout(timer);
    }
  }

  function openLink(uri) {
    // Only open safe schemes; terminal output is untrusted.
    if (!/^https?:\/\//i.test(uri)) return;
    const w = window.open();
    if (w) { w.opener = null; w.location.href = uri; }
  }

  function makeTerminal(readOnly) {
    const opts = {
      cursorBlink: true,
      fontFamily: "Menlo, Consolas, monospace",
      fontSize: cfg.fontSize || 14,
      theme: { background: "#1e1e1e", foreground: "#cccccc" },
      disableStdin: cfg.readonly || !!readOnly,
      scrollback: 10000,
    };
    if (cfg.hyperlinks) {
      // OSC 8 explicit hyperlinks (e.g. `ls --hyperlink`).
      opts.linkHandler = { activate: (ev, uri) => openLink(uri) };
    }
    const term = new Terminal(opts);
    const fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    // Plain-text URL detection; the addon script is only served when
    // hyperlinks are enabled.
    if (cfg.hyperlinks && window.WebLinksAddon) {
      try {
        term.loadAddon(new WebLinksAddon.WebLinksAddon((ev, uri) => openLink(uri)));
      } catch (e) {}
    }
    // Inline graphics (sixel + iTerm2 image protocol); the addon script is
    // only served when enable_graphics is on.
    if (cfg.graphics && window.ImageAddon) {
      try { term.loadAddon(new ImageAddon.ImageAddon()); } catch (e) {}
    }
    return { term, fit };
  }

  // Upgrade from xterm's DOM renderer (which turns choppy once images are
  // on screen) to the canvas renderer. Canvas is chosen over webgl
  // deliberately: the webgl addon's init races (hidden pages, deferred
  // resize tasks) corrupt its GL state and blank the terminal, while
  // canvas has no such state and initializes safely anywhere.
  function loadRenderer(entry) {
    if (cfg.domRenderer) return; // stay on xterm's DOM renderer
    if (window.CanvasAddon) {
      try {
        const c = new CanvasAddon.CanvasAddon();
        entry.term.loadAddon(c);
        entry.renderer = c;
      } catch (e) {}
    }
  }

  function connect(entry, sessionId) {
    // Resolve relative to the page so the app works under any base path.
    const u = new URL(withEid("ws?session=" + encodeURIComponent(sessionId)), location.href);
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const ws = new WebSocket(proto + "//" + u.host + u.pathname + u.search);
    ws.binaryType = "arraybuffer";
    entry.ws = ws;

    ws.onopen = () => {
      // The server replays its scrollback ring on every (re)connect; start
      // from a clean screen or the replay would duplicate what's already
      // rendered. No-op on a fresh terminal.
      entry.term.reset();
      entry.connected = true;
      entry.lastSeen = Date.now();
      entry._paintedConn = false; // re-fit on this connection's first output
      entry._backoff = 500;
      updateTabState(entry);
      showStatus("connected");
      refreshConnStatus(); // other tabs may still be down
      if (entry.id === activeId) {
        fitActiveSoon(); // fit (or re-fit) now that we can tell the PTY
      } else {
        sendResize(entry);
      }
    };

    ws.onmessage = (ev) => {
      const data = new Uint8Array(ev.data);
      entry.lastSeen = Date.now(); // any frame proves the link is alive
      if (entry.stalled) {
        // The connection survived the outage: seamless resume, nothing lost.
        entry.stalled = false;
        entry.connected = true;
        updateTabState(entry);
        showStatus("connected");
        refreshConnStatus();
      }
      if (data.length === 0) return;
      // First real output of this connection: force a fit on the active tab.
      // A respawned/reloaded terminal can be sized before its pane has real
      // dimensions, leaving the prompt unpainted until some later resize —
      // this paints it as soon as there's something to show.
      if ((data[0] === S_OUTPUT || data[0] === S_REPLAY) && !entry._paintedConn) {
        entry._paintedConn = true;
        if (entry.id === activeId) fitActiveSoon();
      }
      if (data[0] === S_OUTPUT) {
        entry.term.write(data.subarray(1));
      } else if (data[0] === S_REPLAY) {
        // Repaint from scrollback: gate the terminal's automatic query
        // replies (DA/OSC color/DECRQM/…) while these bytes parse, or the
        // replies get sent to the shell as phantom input. Cleared in the
        // write callback, which fires once parsing completes. term.write is
        // ordered, so any live output right after paints on top correctly.
        entry.replaying = true;
        clearTimeout(entry._replayGuard);
        // Guard: never leave input blocked if the write callback is missed.
        // Generous on purpose — a large replay (images) parsing on a
        // background tab can take seconds, and clearing the flag before the
        // parse finishes would let query replies leak to the shell as
        // phantom input. The callback is the real signal; this is only
        // stuck-flag insurance.
        entry._replayGuard = setTimeout(() => { entry.replaying = false; }, 30000);
        entry.term.write(data.subarray(1), () => {
          clearTimeout(entry._replayGuard);
          entry.replaying = false;
        });
      } else if (data[0] === S_EXIT) {
        entry.exited = true;
      }
    };

    ws.onclose = async () => {
      if (!panes.has(sessionId)) return; // tab was closed locally
      entry.connected = false;
      entry.stalled = false;
      updateTabState(entry);
      if (!cfg.persistence) {
        // Ephemeral sessions die with their socket — no reconnect, and the
        // server has already discarded the session. The UX still mirrors
        // persistent mode: a shell exit shows [session ended] with restart
        // on Enter or tab removal; a non-exit disconnect (server restart /
        // network drop) auto-spawns a replacement once the server answers.
        if (entry.exited) {
          // Keep the pane with an Enter-to-restart prompt in the same cases
          // persistent mode would: single-session (without auto-respawn),
          // or close-on-exit: false. The server has already discarded the
          // session either way, so Enter creates a fresh one for this tab.
          if ((!cfg.multiSession && !cfg.autoRespawn) || !cfg.closeOnExit) {
            entry.exited = false;
            entry.awaitRestart = true;
            entry.sessionGone = true; // Enter creates a fresh session
            entry.term.write("\r\n\x1b[33m[session ended]\x1b[0m — press \x1b[1mEnter\x1b[0m to restart\r\n");
            showStatus("session ended — press Enter to restart");
          } else {
            entry.term.write("\r\n\x1b[33m[session ended]\x1b[0m\r\n");
            showStatus("session ended");
            setTimeout(() => removeTabUI(sessionId), 600);
          }
        } else {
          entry.term.write("\r\n\x1b[31m[disconnected]\x1b[0m reconnecting…\r\n");
          ephemeralRespawn(entry, sessionId);
        }
        return;
      }
      // If the server signalled exit, or on any close, verify the session
      // still exists before retrying — otherwise we'd reconnect forever to a
      // session whose shell has exited.
      let alive;
      try {
        // Bounded: a hung check on a dead network must not stall the
        // reconnect backoff for the browser's multi-minute default.
        const list = await api("GET", "sessions", undefined, 8000);
        alive = list.some((s) => s.id === sessionId);
      } catch (e) {
        // Network down: trust the exit signal if we got one, else keep retrying.
        alive = !entry.exited;
      }
      if (!alive) {
        if (!entry.exited) {
          // The session vanished without the server signalling a command
          // exit — a server restart or idle reap made our tab stale. Treat
          // it like a fresh page load: spawn a replacement terminal.
          if (cfg.multiSession) {
            removeTabUI(sessionId, true);
          } else {
            entry.awaitRestart = true; // Enter retries if the spawn fails
            entry.sessionGone = true;
            restartSession(entry);
          }
          return;
        }
        if (!cfg.multiSession && !cfg.autoRespawn) {
          // Single-session mode: keep the dead pane and offer a restart.
          // The session itself is gone server-side, so Enter creates a
          // fresh one in place (see restartSession).
          entry.exited = false;
          entry.awaitRestart = true;
          entry.sessionGone = true;
          updateTabState(entry);
          entry.term.write("\r\n\x1b[33m[session ended]\x1b[0m — press \x1b[1mEnter\x1b[0m to restart\r\n");
          showStatus("session ended — press Enter to restart");
          return;
        }
        entry.term.write("\r\n\x1b[33m[session ended]\x1b[0m\r\n");
        showStatus("session ended");
        setTimeout(() => removeTabUI(sessionId), 600);
        return;
      }
      if (entry.exited) {
        // Command exited but the session is kept (close_on_exit: false).
        // Stop reconnecting and offer a restart instead.
        entry.awaitRestart = true;
        entry.term.write("\r\n\x1b[33m[session ended]\x1b[0m — press \x1b[1mEnter\x1b[0m to restart\r\n");
        showStatus("session ended — press Enter to restart");
        return;
      }
      entry.exited = false;
      refreshConnStatus();
      entry._backoff = Math.min((entry._backoff || 500) * 1.6, 5000);
      entry.reconnectTimer = setTimeout(() => {
        if (panes.has(sessionId)) connect(entry, sessionId);
      }, entry._backoff);
    };

    ws.onerror = () => { try { ws.close(); } catch (e) {} };
  }

  function sendInput(entry, data) {
    if (entry.ws && entry.ws.readyState === WebSocket.OPEN) {
      entry.ws.send(C_INPUT + data);
    }
  }

  function sendResize(entry) {
    if (entry.ws && entry.ws.readyState === WebSocket.OPEN) {
      const dims = { cols: entry.term.cols, rows: entry.term.rows };
      entry.ws.send(C_RESIZE + JSON.stringify(dims));
    }
  }

  function fitActive() {
    const entry = panes.get(activeId);
    if (!entry) return;
    try { entry.fit.fit(); } catch (e) {}
    sendResize(entry);
  }

  // fit() silently no-ops when called before xterm's first render (cell
  // metrics aren't measured yet), which would leave the terminal at 80
  // columns in a full-width window. Re-fit a few times shortly after to
  // catch the renderer once it's ready.
  function fitActiveSoon() {
    fitActive();
    requestAnimationFrame(fitActive);
    setTimeout(fitActive, 150);
  }

  // Copy text to the clipboard; falls back to a hidden textarea +
  // execCommand for non-secure (plain http) contexts where the async
  // Clipboard API is unavailable.
  function copyText(text, entry) {
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).catch(() => {});
      return;
    }
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand("copy"); } catch (e) {}
    ta.remove();
    if (entry) entry.term.focus(); // the textarea stole focus
  }

  // Render a tab title. Auto titles come with their components (process
  // name + cwd) so the directory can be styled dimmer; user titles are
  // plain text. textContent of the result always equals info.title.
  function setTabTitle(titleEl, info) {
    if (info.ps || info.dir) {
      titleEl.textContent = "";
      if (info.ps) titleEl.appendChild(document.createTextNode(info.ps));
      if (info.ps && info.dir) titleEl.appendChild(document.createTextNode(" "));
      if (info.dir) {
        const d = document.createElement("span");
        d.className = "dir";
        d.textContent = info.dir;
        titleEl.appendChild(d);
      }
    } else {
      titleEl.textContent = info.title;
    }
  }

  function updateTabState(entry) {
    if (entry.tabEl) entry.tabEl.classList.toggle("disconnected", !entry.connected);
  }

  // deferConnect skips the immediate websocket dial — used at page load so
  // the active tab can connect first and the rest follow staggered.
  function createTab(info, deferConnect) {
    // A read-only shared tab: input is disabled locally too (the server also
    // drops it, but this stops keystrokes echoing hope-fully into the void).
    const readOnly = !!info.readOnly;
    const { term, fit } = makeTerminal(readOnly);

    const pane = document.createElement("div");
    pane.className = "term-pane";
    // Inner container physically inset from the pane edges provides the
    // padding: the terminal is confined to it, so the fit addon measures
    // the already-padded area and can't spill past any edge (relying on
    // the addon to subtract CSS padding is unreliable across browsers).
    const inner = document.createElement("div");
    inner.className = "term-inner";
    pane.appendChild(inner);
    termsEl.appendChild(pane);
    term.open(inner); // renderer upgrade happens on first activation
    // Background tabs are never fitted (their pane is display:none), so
    // xterm would sit at the default 80x24 — and a scrollback replay
    // produced at the real width would wrap and mis-position, exposing old
    // ring content as garbage when the tab is later activated and reflowed.
    // All panes share one container, so the active tab's grid is the right
    // size for every tab: adopt it up front.
    {
      const act = panes.get(activeId);
      if (act && act.term.cols > 2 && act.term.rows > 1) {
        try { term.resize(act.term.cols, act.term.rows); } catch (e) {}
      }
    }

    if (!cfg.readonly && !readOnly) {
      term.onData((d) => {
        // Drop the terminal's own replies to queries embedded in a
        // scrollback repaint; only real input/live replies reach the PTY.
        if (entry.replaying) return;
        if (entry.awaitRestart) {
          if (d === "\r") restartSession(entry);
          return;
        }
        sendInput(entry, d);
      });
    }
    term.onResize(() => sendResize(entry));

    // Selecting text copies it to the clipboard (like a Linux terminal).
    // Debounced so we don't write on every mouse-move during a drag.
    let copyTimer = null;
    term.onSelectionChange(() => {
      clearTimeout(copyTimer);
      copyTimer = setTimeout(() => {
        const sel = term.getSelection();
        if (sel) copyText(sel, entry);
      }, 120);
    });

    // Middle-click pastes the clipboard. term.paste() respects bracketed
    // paste mode, so pasting into vim/shells behaves correctly.
    pane.addEventListener("mousedown", (e) => {
      if (e.button === 1) e.preventDefault(); // suppress autoscroll
    });
    pane.addEventListener("auxclick", (e) => {
      if (e.button !== 1 || cfg.readonly || readOnly || !cfg.middleclickPaste) return;
      e.preventDefault();
      if (navigator.clipboard && navigator.clipboard.readText) {
        navigator.clipboard.readText()
          .then((text) => { if (text) term.paste(text); })
          .catch(() => showStatus("clipboard read blocked by browser"));
      } else {
        showStatus("paste needs HTTPS (Ctrl+Shift+V still works)");
      }
    });

    // Tab element
    const tabEl = document.createElement("div");
    tabEl.className = "tab";
    tabEl.dataset.sid = info.id;
    // Pin indicator, hidden unless pinned (managed by applyPin).
    const pinEl = document.createElement("span");
    pinEl.className = "pinicon";
    pinEl.title = "Pinned";
    tabEl.appendChild(pinEl);

    const titleEl = document.createElement("span");
    titleEl.className = "title";
    setTabTitle(titleEl, info);
    if (!info.shared) titleEl.title = "Double-click to rename";
    tabEl.appendChild(titleEl);

    // Badge (informational): shared-in tab you're viewing, or an owned tab
    // you've shared out. Kept in sync by applyShareBadge (also from polling).
    const badge = document.createElement("span");
    badge.className = "badge";
    tabEl.appendChild(badge);

    let closeEl = null;
    if (cfg.multiSession) {
      closeEl = document.createElement("span");
      closeEl.className = "close";
      closeEl.textContent = "×";
      closeEl.title = "Close";
      closeEl.addEventListener("click", (e) => {
        e.stopPropagation();
        closeSession(info.id);
      });
      tabEl.appendChild(closeEl);
    }

    // PS1 titling: display whatever the shell announces via OSC 0/2 window
    // title sequences (client-side only; the server's auto-titler is off in
    // this mode and rename persistence still works via the API).
    if (cfg.tabShowPS1) {
      term.onTitleChange((t) => {
        if (t && !entry.renaming) titleEl.textContent = t;
      });
    }

    tabEl.addEventListener("click", () => activate(info.id));
    // Double-click title to rename — owner only (the title is shared state).
    if (!info.shared) {
      titleEl.addEventListener("dblclick", (e) => {
        e.stopPropagation();
        beginRename(info.id, titleEl);
      });
    }

    // Right-click a tab for actions (share, rename, close). Sharing options
    // appear only for tabs you own, when the server allows sharing.
    tabEl.addEventListener("contextmenu", (e) => {
      e.preventDefault();
      showTabMenu(e, info.id);
    });

    // Drag to rearrange tabs. The dragged tab is moved live as the cursor
    // crosses other tabs' midpoints; the final order is saved on drop.
    if (cfg.multiSession) {
      tabEl.draggable = true;
      tabEl.addEventListener("dragstart", (e) => {
        draggedId = info.id;
        e.dataTransfer.effectAllowed = "move";
        e.dataTransfer.setData("text/plain", info.id); // Firefox needs data
        tabEl.classList.add("dragging");
      });
      tabEl.addEventListener("dragend", () => {
        tabEl.classList.remove("dragging");
        draggedId = null;
        saveOrder();
      });
      tabEl.addEventListener("dragover", (e) => {
        if (!draggedId || draggedId === info.id) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = "move";
        const src = panes.get(draggedId);
        if (!src) return;
        const rect = tabEl.getBoundingClientRect();
        const after = cfg.tabBarPosition === "right"
          ? e.clientY > rect.top + rect.height / 2
          : e.clientX > rect.left + rect.width / 2;
        tabbar.insertBefore(src.tabEl, after ? tabEl.nextSibling : tabEl);
      });
    }

    // Insert before the "+ New" button if present.
    if (newtabBtn && newtabBtn.parentElement === tabbar) {
      tabbar.insertBefore(tabEl, newtabBtn);
    } else {
      tabbar.appendChild(tabEl);
    }

    const entry = {
      id: info.id, term, fit, pane, tabEl, titleEl, badge, pinEl, closeEl,
      ws: null, connected: false, exited: false, awaitRestart: false,
      sessionGone: false, replaying: false, stalled: false, lastSeen: 0,
      reconnectTimer: null, _backoff: 500,
      renderer: null, rendererLoaded: false,
      shared: !!info.shared, readOnly: readOnly, sharedOut: !!info.sharedOut,
      sharerCount: info.sharerCount || 0,
      pinned: pinnedTabs.has(info.id),
    };
    applyShareBadge(entry);
    applyPin(entry);
    panes.set(info.id, entry);
    if (!deferConnect) connect(entry, info.id);
    return entry;
  }

  // Persist the DOM tab order server-side so it survives reloads.
  async function saveOrder() {
    const ids = Array.from(tabbar.querySelectorAll(".tab"))
      .map((t) => t.dataset.sid).filter(Boolean);
    try { await api("PUT", "sessions/order", { order: ids }); } catch (e) {}
  }

  function activate(id) {
    if (!panes.has(id)) return;
    activeId = id;
    rememberActive(id);
    for (const [sid, entry] of panes) {
      const on = sid === id;
      entry.pane.classList.toggle("active", on);
      entry.tabEl.classList.toggle("active", on);
      if (on) {
        // Don't steal focus while the tab title is being renamed: a
        // double-click fires two clicks first, and their deferred
        // term.focus() would blur the editor and kick us out of edit mode.
        setTimeout(() => {
          // First time this tab is shown its pane finally has dimensions:
          // safe to swap in the GPU renderer now. Never do it while the
          // page itself is hidden (background browser tab): rAF is
          // suspended there and WebGL init corrupts, leaving invisible
          // text — visibilitychange below picks it up instead.
          if (!entry.rendererLoaded && !document.hidden) {
            entry.rendererLoaded = true;
            loadRenderer(entry);
          }
          fitActiveSoon();
          if (!entry.renaming) entry.term.focus();
        }, 0);
      }
    }
  }

  function beginRename(id, titleEl) {
    const entry = panes.get(id);
    if (!entry) return;
    entry.renaming = true;
    entry.tabEl.draggable = false; // text selection must not start a drag
    const old = titleEl.textContent;
    // Auto titles are styled DOM (bright process + dim .dir span); keep the
    // exact markup so cancelling restores the styling, not flattened text.
    const origHTML = titleEl.innerHTML;
    titleEl.contentEditable = "true";
    titleEl.focus();
    // Select all text.
    const range = document.createRange();
    range.selectNodeContents(titleEl);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);

    function finish(commit) {
      entry.renaming = false;
      entry.tabEl.draggable = !!cfg.multiSession;
      titleEl.contentEditable = "false";
      titleEl.removeEventListener("keydown", onKey);
      titleEl.removeEventListener("blur", onBlur);
      const next = titleEl.textContent.trim();
      if (commit && next && next !== old) {
        // A rename converts the tab to a plain user title (the server pins
        // it and drops the ps/dir components) — render it that way now, or
        // text edited inside the dim .dir span would stay dim until reload.
        setTabTitle(titleEl, { title: next });
        api("PATCH", "sessions/" + encodeURIComponent(id), { title: next })
          .catch(() => { titleEl.innerHTML = origHTML; });
      } else {
        // Cancelled or unchanged: restore the original styled title as-was.
        titleEl.innerHTML = origHTML;
      }
    }
    function onKey(e) {
      if (e.key === "Enter") { e.preventDefault(); finish(true); entry.term.focus(); }
      else if (e.key === "Escape") { e.preventDefault(); finish(false); entry.term.focus(); }
    }
    function onBlur() { finish(true); }
    titleEl.addEventListener("keydown", onKey);
    titleEl.addEventListener("blur", onBlur);
  }

  // Guard against a crash-looping command respawning sessions in a tight loop.
  let lastAutoSpawn = 0;

  // respawnIfLast forces a fresh session when this was the last tab, used
  // when the removal is due to stale state (server restart) rather than a
  // genuine command exit.
  function removeTabUI(id, respawnIfLast) {
    const entry = panes.get(id);
    if (!entry) return;
    clearTimeout(entry.reconnectTimer);
    panes.delete(id);
    if (pinnedTabs.delete(id)) savePinned(pinnedTabs); // forget dead pins
    if (entry.ws) { try { entry.ws.close(); } catch (e) {} }
    // Dispose the renderer addon first and defensively: renderer teardown
    // combined with the image addon's setRenderer hook has thrown before,
    // which would abort term.dispose() halfway.
    if (entry.renderer) { try { entry.renderer.dispose(); } catch (e) {} entry.renderer = null; }
    // Defer the terminal disposal one idle cycle: renderer teardown puts a
    // resize task on xterm's internal idle queue, and disposing
    // synchronously leaves that task pointing at a dead renderer (async
    // "handleResize of undefined" console error in xterm 5.2). Letting the
    // queue drain first avoids it; the UI is removed immediately either way.
    const term = entry.term;
    const disposeTerm = () => { try { term.dispose(); } catch (e) {} };
    if (window.requestIdleCallback) {
      requestIdleCallback(disposeTerm, { timeout: 1000 });
    } else {
      setTimeout(disposeTerm, 100);
    }
    entry.pane.remove();
    entry.tabEl.remove();
    if (activeId === id) {
      const first = panes.keys().next();
      if (!first.done) {
        activate(first.value);
      } else {
        activeId = null;
        // Respawn when stale-state removal asks for it, or always with
        // auto-respawn on — guarded against tight crash loops either way.
        const now = Date.now();
        if ((respawnIfLast || cfg.autoRespawn) && now - lastAutoSpawn > 2000) {
          lastAutoSpawn = now;
          addSession();
        } else {
          showStatus("session ended — press + to start a new one");
        }
      }
    }
  }

  // Restart an ended session. Two cases: the session still exists server-
  // side (close-on-exit: false) and its command is respawned in place; or
  // it was removed (single-session, close-on-exit: true) and a fresh
  // session replaces this pane.
  async function restartSession(entry) {
    if (entry.sessionGone) {
      let info;
      try {
        info = await api("POST", "sessions" + location.search);
      } catch (e) {
        showStatus(e.message || "cannot start session");
        return;
      }
      // Replace the dead pane with a fresh tab bound to the new session.
      clearTimeout(entry.reconnectTimer);
      panes.delete(entry.id);
      if (entry.renderer) { try { entry.renderer.dispose(); } catch (e) {} }
      try { entry.term.dispose(); } catch (e) {}
      entry.pane.remove();
      entry.tabEl.remove();
      createTab(info);
      activate(info.id);
      return;
    }
    try {
      await api("POST", "sessions/" + encodeURIComponent(entry.id) + "/restart");
    } catch (e) {
      showStatus("restart failed");
      return;
    }
    entry.awaitRestart = false;
    entry.exited = false;
    entry._backoff = 500;
    entry.term.reset();
    connect(entry, entry.id);
  }

  // Ephemeral-mode replacement after a non-exit disconnect (server restart
  // or network drop — indistinguishable, and the session is gone either
  // way). Mirrors persistent mode's stale-state recovery: keep probing with
  // backoff until the server answers, then spawn a replacement terminal.
  async function ephemeralRespawn(entry, sessionId) {
    if (!panes.has(sessionId)) return; // tab closed meanwhile
    let up = true;
    try { await api("GET", "sessions", undefined, 8000); } catch (e) { up = false; }
    if (!up) {
      refreshConnStatus(); // sticky "reconnecting…"
      entry._backoff = Math.min((entry._backoff || 500) * 1.6, 5000);
      entry.reconnectTimer = setTimeout(() => ephemeralRespawn(entry, sessionId), entry._backoff);
      return;
    }
    if (cfg.multiSession) {
      removeTabUI(sessionId, true); // respawns a fresh tab if this was the last
      return;
    }
    entry.awaitRestart = true; // Enter works as fallback if the spawn fails
    entry.sessionGone = true;
    await restartSession(entry);
    if (entry.sessionGone && panes.has(sessionId)) {
      // Spawn failed (server flapping): retry on the same backoff schedule.
      entry._backoff = Math.min((entry._backoff || 500) * 1.6, 5000);
      entry.reconnectTimer = setTimeout(() => ephemeralRespawn(entry, sessionId), entry._backoff);
    }
  }

  async function closeSession(id) {
    try { await api("DELETE", "sessions/" + encodeURIComponent(id)); } catch (e) {}
    removeTabUI(id);
  }

  // Build the shareable URL from a token, based on the page's own address so
  // it works behind any proxy prefix exactly as the user sees it.
  function shareURL(token) {
    const u = new URL(location.href);
    u.hash = "";
    u.searchParams.set("share", token);
    return u.href;
  }

  // Create a share link with the chosen access + expiry, returning its URL.
  async function createShareLink(id, readOnly, ttl, singleUse) {
    const res = await api("POST", "sessions/" + encodeURIComponent(id) + "/share",
      { readOnly: readOnly, ttl: ttl || "", singleUse: !!singleUse });
    const entry = panes.get(id);
    if (entry) { entry.sharedOut = true; applyShareBadge(entry); }
    return shareURL(res.token);
  }

  async function stopSharing(id) {
    try { await api("DELETE", "sessions/" + encodeURIComponent(id) + "/share"); }
    catch (e) { showStatus(e.message || "cannot stop sharing"); return; }
    const entry = panes.get(id);
    if (entry) { entry.sharedOut = false; entry.sharerCount = 0; applyShareBadge(entry); }
    showStatus("sharing stopped");
  }

  // --- Share dialog --------------------------------------------------------
  let openDialog = null;
  function closeDialog() {
    if (openDialog) { openDialog.remove(); openDialog = null; }
  }

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  function showShareDialog(id) {
    closeDialog();
    const entry = panes.get(id);
    if (!entry) return;

    const overlay = el("div", "modal-overlay");
    const box = el("div", "modal");
    overlay.appendChild(box);
    overlay.addEventListener("click", (e) => { if (e.target === overlay) closeDialog(); });

    box.appendChild(el("h3", "modal-title", "Share this terminal"));
    box.appendChild(el("p", "modal-sub",
      "Anyone with the link who can sign in gets this tab in their list."));

    // Access level
    const access = el("div", "field");
    access.appendChild(el("label", null, "Access"));
    const accessSel = el("select");
    accessSel.appendChild(new Option("View only", "ro"));
    accessSel.appendChild(new Option("Allow control", "rw"));
    access.appendChild(accessSel);
    box.appendChild(access);

    // Expiry (multi-use window)
    const exp = el("div", "field");
    exp.appendChild(el("label", null, "Link expires"));
    const expSel = el("select");
    [["", "Never"], ["1h", "1 hour"], ["8h", "8 hours"],
     ["24h", "1 day"], ["168h", "7 days"]].forEach(([v, t]) =>
      expSel.appendChild(new Option(t, v)));
    exp.appendChild(expSel);
    box.appendChild(exp);

    // One-time (single-use) — independent of the expiry window.
    const onceField = el("div", "field checkbox");
    const onceBox = el("input"); onceBox.type = "checkbox"; onceBox.id = "share-once";
    const onceLabel = el("label", null, "One-time — link stops working after the first person accepts");
    onceLabel.setAttribute("for", "share-once");
    onceField.appendChild(onceBox);
    onceField.appendChild(onceLabel);
    box.appendChild(onceField);

    box.appendChild(el("p", "modal-hint",
      "Expiry and one-time limit how long the link can be accepted; access already granted persists until you stop sharing."));

    // Result row (hidden until a link is created)
    const result = el("div", "field result");
    result.style.display = "none";
    const linkInput = el("input");
    linkInput.readOnly = true;
    const copyBtn = el("button", "btn", "Copy");
    result.appendChild(linkInput);
    result.appendChild(copyBtn);
    box.appendChild(result);
    copyBtn.addEventListener("click", () => {
      copyText(linkInput.value, entry);
      linkInput.select();
      showStatus("share link copied");
    });

    // Actions. Stop sharing is present whenever a link/access exists (built
    // once, shown/hidden dynamically — a fresh link reveals it immediately).
    const actions = el("div", "modal-actions");
    const stopBtn = el("button", "btn btn-danger", "Stop sharing");
    stopBtn.style.display = entry.sharedOut ? "" : "none";
    stopBtn.addEventListener("click", () => { stopSharing(id); closeDialog(); });
    actions.appendChild(stopBtn);
    const spacer = el("div"); spacer.style.flex = "1";
    actions.appendChild(spacer);
    const cancelBtn = el("button", "btn", "Close");
    cancelBtn.addEventListener("click", closeDialog);
    const createBtn = el("button", "btn btn-primary", "Create link");
    createBtn.addEventListener("click", async () => {
      createBtn.disabled = true;
      try {
        const url = await createShareLink(
          id, accessSel.value === "ro", expSel.value, onceBox.checked);
        linkInput.value = url;
        result.style.display = "";
        stopBtn.style.display = ""; // a link now exists -> allow stopping
        linkInput.select();
        copyText(url, entry);
        showStatus("share link created & copied");
      } catch (e) {
        showStatus(e.message || "cannot create share link");
      } finally {
        createBtn.disabled = false;
      }
    });
    actions.appendChild(cancelBtn);
    actions.appendChild(createBtn);
    box.appendChild(actions);

    document.body.appendChild(overlay);
    openDialog = overlay;
    accessSel.focus();
  }
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeDialog(); });

  // Inline stroke icons (Feather/Lucide style) — no icon font, no CDN, and
  // they tint via currentColor so they sit naturally in the dark UI.
  const ICON_PATHS = {
    share: '<circle cx="18" cy="5" r="3"/><circle cx="6" cy="12" r="3"/><circle cx="18" cy="19" r="3"/><line x1="8.6" y1="13.5" x2="15.4" y2="17.5"/><line x1="15.4" y1="6.5" x2="8.6" y2="10.5"/>',
    eye: '<path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/>',
    pin: '<path d="M12 17v5"/><path d="M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16h14v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V6h1a2 2 0 0 0 0-4H8a2 2 0 0 0 0 4h1z"/>',
    edit: '<path d="M17 3a2.83 2.83 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5L17 3z"/>',
    close: '<line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/>',
    ban: '<circle cx="12" cy="12" r="10"/><line x1="4.93" y1="4.93" x2="19.07" y2="19.07"/>',
  };
  function iconSVG(name) {
    return '<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor"' +
      ' stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
      ICON_PATHS[name] + "</svg>";
  }

  // Reflect an entry's share state in its tab badge + styling. The badge
  // shows only once someone has actually joined (sharerCount) — a link that
  // merely exists doesn't badge the tab (Stop-sharing still appears via the
  // menu/dialog, driven separately by sharedOut).
  function applyShareBadge(entry) {
    const b = entry.badge;
    if (!b) return;
    if (entry.shared) {
      b.innerHTML = iconSVG(entry.readOnly ? "eye" : "share");
      b.title = entry.readOnly ? "Shared with you (view-only)" : "Shared with you";
      entry.tabEl.classList.add("is-shared");
    } else if (entry.sharerCount > 0) {
      b.innerHTML = iconSVG("share");
      const n = entry.sharerCount;
      b.title = n === 1 ? "1 person has joined" : n + " people have joined";
      entry.tabEl.classList.add("is-shared");
    } else {
      b.innerHTML = "";
      b.title = "";
      entry.tabEl.classList.remove("is-shared");
    }
  }

  // Pinned = no close button on the tab (context menu Close is hidden too);
  // unpin to make it closable again.
  function applyPin(entry) {
    if (entry.closeEl) entry.closeEl.style.display = entry.pinned ? "none" : "";
    if (entry.pinEl) {
      entry.pinEl.innerHTML = entry.pinned ? iconSVG("pin") : "";
      entry.pinEl.style.display = entry.pinned ? "" : "none";
    }
    entry.tabEl.classList.toggle("is-pinned", entry.pinned);
  }

  function togglePin(id) {
    const entry = panes.get(id);
    if (!entry) return;
    entry.pinned = !entry.pinned;
    if (entry.pinned) pinnedTabs.add(id); else pinnedTabs.delete(id);
    savePinned(pinnedTabs);
    applyPin(entry);
    showStatus(entry.pinned ? "tab pinned" : "tab unpinned");
  }

  // A tiny right-click menu. Rebuilt per invocation; one at a time.
  let openMenu = null;
  function closeTabMenu() {
    if (openMenu) { openMenu.remove(); openMenu = null; }
  }
  function showTabMenu(ev, id) {
    closeTabMenu();
    const entry = panes.get(id);
    if (!entry) return;
    const items = [];
    if (cfg.allowSharing && !entry.shared) {
      // Owner-only: opens a dialog to pick access + expiry.
      let label = "Share…";
      if (entry.sharerCount > 0) {
        label = "Sharing… (" + entry.sharerCount +
          (entry.sharerCount === 1 ? " user)" : " users)");
      } else if (entry.sharedOut) label = "Sharing…";
      items.push(["share", label, () => showShareDialog(id)]);
      if (entry.sharedOut) items.push(["ban", "Stop sharing", () => stopSharing(id)]);
    }
    // Rename is owner-only (the title is shared state).
    if (!entry.shared) items.push(["edit", "Rename", () => beginRename(id, entry.titleEl)]);
    if (cfg.multiSession) {
      items.push(["pin", entry.pinned ? "Unpin" : "Pin", () => togglePin(id)]);
      if (!entry.pinned) items.push(["close", "Close", () => closeSession(id)]);
    }
    if (items.length === 0) return;

    const menu = document.createElement("div");
    menu.className = "ctxmenu";
    for (const [ic, label, fn] of items) {
      const it = document.createElement("div");
      it.className = "ctxitem";
      it.innerHTML = iconSVG(ic); // static markup; label added as text below
      it.appendChild(document.createTextNode(label));
      it.addEventListener("click", () => { closeTabMenu(); fn(); });
      menu.appendChild(it);
    }
    document.body.appendChild(menu);
    // Keep it on-screen.
    const mw = menu.offsetWidth, mh = menu.offsetHeight;
    menu.style.left = Math.min(ev.clientX, window.innerWidth - mw - 4) + "px";
    menu.style.top = Math.min(ev.clientY, window.innerHeight - mh - 4) + "px";
    openMenu = menu;
  }
  document.addEventListener("click", closeTabMenu);
  document.addEventListener("scroll", closeTabMenu, true);
  window.addEventListener("blur", closeTabMenu);

  async function addSession() {
    try {
      // Forward the page query so url_arg/url_env apply to new tabs too;
      // empty title -> server default.
      const info = await api("POST", "sessions" + location.search);
      createTab(info);
      activate(info.id);
    } catch (e) {
      showStatus(e.message || "cannot add session");
    }
  }

  // Reconcile with the server: update titles (auto cwd titles, renames from
  // another window) and adopt sessions we have no tab for — e.g. when the
  // page was loaded from a stale cache or another window created a tab.
  // In-flight guard + abort timeout: on an unresponsive network the ticks
  // must not pile up behind a hung request or hog the connection pool the
  // websocket reconnects need.
  let syncInFlight = false;
  async function syncSessions() {
    if (document.hidden || syncInFlight) return;
    syncInFlight = true;
    let list;
    try { list = await api("GET", "sessions", undefined, 8000); }
    catch (e) { return; }
    finally { syncInFlight = false; }
    for (const info of list) {
      const entry = panes.get(info.id);
      if (entry) {
        // In PS1 mode the shell's OSC titles own the display; don't let
        // the server's static titles overwrite them.
        if (!cfg.tabShowPS1 && !entry.renaming && entry.titleEl.textContent !== info.title) {
          setTabTitle(entry.titleEl, info);
        }
        // Reconcile owner-side share state (viewers joining/leaving, links
        // created/expired in another window): the badge follows sharerCount,
        // the menu's Stop-sharing follows sharedOut.
        if (!entry.shared) {
          const so = !!info.sharedOut, sc = info.sharerCount || 0;
          if (entry.sharedOut !== so || entry.sharerCount !== sc) {
            entry.sharedOut = so;
            entry.sharerCount = sc;
            applyShareBadge(entry);
          }
        }
      } else if (cfg.multiSession) {
        createTab(info);
        if (!activeId) activate(info.id);
      }
    }
  }

  async function init() {
    if (newtabBtn) newtabBtn.addEventListener("click", addSession);
    // Let drops land anywhere on the bar, not just on other tabs.
    tabbar.addEventListener("dragover", (e) => { if (draggedId) e.preventDefault(); });
    tabbar.addEventListener("drop", (e) => { if (draggedId) e.preventDefault(); });
    window.addEventListener("resize", fitActive);
    if (window.ResizeObserver) {
      new ResizeObserver(() => fitActive()).observe(termsEl);
    }
    // Poll only when it has something to do: titles/adoption need tabs
    // (multiSession) and a stable identity (persistence). The reconnect
    // logic's alive-checks still touch the server's idle timer without it.
    if (cfg.persistence && cfg.multiSession) setInterval(syncSessions, 3000);
    // Cell metrics change once the terminal font finishes loading.
    if (document.fonts && document.fonts.ready) {
      document.fonts.ready.then(() => fitActive());
    }
    // Client-side liveness: browsers cannot see protocol-level ping/pong,
    // so a silently dead network leaves the socket looking "open" for many
    // minutes with no onclose — and no reconnect UI. Send an app-level ping
    // every 10s; after 30s of total silence mark the tab STALLED (sticky
    // "reconnecting…", disconnected dot) — but deliberately do NOT close
    // the socket: TCP often survives an outage, and a surviving connection
    // resumes seamlessly with nothing lost (in ephemeral mode, closing
    // would even kill the session). We keep pinging; either a frame arrives
    // again (instant recovery, handled in onmessage) or the socket dies for
    // real and onclose runs the normal reconnect. Skipped while hidden
    // (throttled timers would false-positive; resumes when visible).
    // Past the server's dead-peer deadline (3× ping-interval) a silent
    // connection cannot resume — the server has already hung up. Waiting
    // longer only defers reconnect until the OS abandons TCP retransmission
    // (often a minute or more), which showed up as "respawn takes a minute"
    // after an idle-reap. Force the reconnect path at that point instead.
    const DEAD_AFTER = Math.max(65000, ((cfg.pingSeconds || 20) * 3 + 5) * 1000);
    function livenessTick() {
      if (document.hidden) return;
      const now = Date.now();
      for (const entry of panes.values()) {
        if (!entry.ws || entry.ws.readyState !== WebSocket.OPEN) continue;
        const silent = now - (entry.lastSeen || now);
        if (silent > DEAD_AFTER) {
          entry.connected = false;
          entry.stalled = false;
          updateTabState(entry);
          refreshConnStatus();
          try { entry.ws.close(); } catch (e) {} // onclose -> alive-check -> reconnect/respawn
          continue;
        }
        if (entry.connected && silent > 30000) {
          entry.connected = false;
          entry.stalled = true;
          updateTabState(entry);
          refreshConnStatus(); // sticky "reconnecting…" immediately
        }
        // Keep pinging even while stalled: the reply (or the TCP reset of a
        // dead connection) is what resolves the stall either way.
        try { entry.ws.send(C_PING); } catch (e) {}
      }
    }
    setInterval(livenessTick, 10000);

    // React to connectivity changes: on restore, skip the pending backoff
    // and redial every disconnected tab immediately; while offline, show a
    // sticky notice (socket death detection can lag the actual outage).
    window.addEventListener("online", () => {
      // Always replace the sticky "network offline" notice — the sockets
      // may have survived a short blip, in which case nothing below runs.
      showStatus("network restored");
      for (const [id, e] of panes) {
        if (e.connected || e.exited || e.awaitRestart) continue;
        if (e.ws && e.ws.readyState === WebSocket.OPEN) {
          // Stalled but the socket may have survived: provoke a resolution
          // (pong resumes it; a dead connection gets reset -> onclose).
          try { e.ws.send(C_PING); } catch (err) {}
          continue;
        }
        if (!cfg.persistence) continue; // ephemeral retry loops handle theirs
        clearTimeout(e.reconnectTimer);
        e._backoff = 500;
        connect(e, id);
      }
      refreshConnStatus(); // re-asserts sticky "reconnecting…" if tabs are down
    });
    window.addEventListener("offline", () => showStatus("network offline", true));

    // Tabs activated while the page was hidden (e.g. auto-respawn after a
    // server restart) postponed their GPU renderer: load it once visible.
    document.addEventListener("visibilitychange", () => {
      if (document.hidden) return;
      const entry = panes.get(activeId);
      if (entry && !entry.rendererLoaded) {
        entry.rendererLoaded = true;
        loadRenderer(entry);
      }
      fitActiveSoon();
      // Don't wait up to 10s for the next tick: judge connections now, so a
      // tab whose socket died (and session expired) while hidden starts its
      // reconnect/respawn the moment the user comes back.
      livenessTick();
    });

    // Accept a share link (?share=<token>) before building tabs, so the
    // shared session appears in this page's list. Strip the token from the
    // URL either way (don't leave a capability sitting in the address bar /
    // history / future reloads). The accepted session id is activated below.
    let sharedTarget = null;
    const shareTok = new URLSearchParams(location.search).get("share");
    if (shareTok) {
      try {
        const info = await api("POST", "sessions/accept", { token: shareTok });
        sharedTarget = info.id;
      } catch (e) {
        showStatus(e.message || "share link invalid or expired");
      }
      const u = new URL(location.href);
      u.searchParams.delete("share");
      history.replaceState(null, "", u.href);
    }

    // The session list is inlined into the page; refetch from the API when a
    // share was just accepted (the inlined list predates it) or the inlined
    // list is empty.
    let list = (!sharedTarget && Array.isArray(cfg.sessions)) ? cfg.sessions : [];
    if (list.length === 0) {
      try { list = await api("GET", "sessions"); } catch (e) {}
    }
    if (!list || list.length === 0) {
      // Server guarantees one on index load, but be defensive.
      await addSession();
      return;
    }
    // Build all tab UIs first (sockets deferred), then connect the
    // previously-active tab immediately so it's usable right away, and
    // stagger the remaining connections behind it in tab order.
    // Prefer the just-accepted shared tab, else the previously-active one.
    const last = recallActive();
    let activeTarget = list[0].id;
    if (sharedTarget && list.some((s) => s.id === sharedTarget)) activeTarget = sharedTarget;
    else if (list.some((s) => s.id === last)) activeTarget = last;
    for (const info of list) createTab(info, true);
    activate(activeTarget);
    connect(panes.get(activeTarget), activeTarget);
    // One 200ms head start for the active tab, then dial the rest in tab
    // order back-to-back (skipping any closed or already-dialed meanwhile).
    // By now the active tab has been fitted; adopt its grid on each
    // background terminal BEFORE connecting, so the incoming replay parses
    // at the width it was produced for (see the note in createTab).
    setTimeout(() => {
      const act = panes.get(activeTarget);
      for (const info of list) {
        if (info.id === activeTarget) continue;
        const e = panes.get(info.id);
        if (!e || !panes.has(info.id) || e.ws) continue;
        if (act && act.term.cols > 2 && act.term.rows > 1) {
          try { e.term.resize(act.term.cols, act.term.rows); } catch (err) {}
        }
        connect(e, info.id);
      }
    }, 200);
  }

  init();
})();
