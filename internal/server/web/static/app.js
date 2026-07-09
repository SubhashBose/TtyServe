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

  // In persistence-off mode there is no cookie; the page identity travels as
  // ?eid=... on every request so they resolve to the same server client.
  function withEid(path) {
    if (cfg.persistence || !cfg.ephemeralId) return path;
    return path + (path.includes("?") ? "&" : "?") + "eid=" + encodeURIComponent(cfg.ephemeralId);
  }

  async function api(method, path, body) {
    path = withEid(path);
    const opts = { method, headers: {} };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(path, opts);
    if (!res.ok) {
      // Surface the server's message (e.g. "session limit reached ...").
      let msg = "";
      try { msg = (await res.text()).trim(); } catch (e) {}
      throw new Error(msg || method + " " + path + " -> " + res.status);
    }
    if (res.status === 204) return null;
    return res.json();
  }

  function openLink(uri) {
    // Only open safe schemes; terminal output is untrusted.
    if (!/^https?:\/\//i.test(uri)) return;
    const w = window.open();
    if (w) { w.opener = null; w.location.href = uri; }
  }

  function makeTerminal() {
    const opts = {
      cursorBlink: true,
      fontFamily: "Menlo, Consolas, monospace",
      fontSize: cfg.fontSize || 14,
      theme: { background: "#1e1e1e", foreground: "#cccccc" },
      disableStdin: cfg.readonly,
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
      if (data.length === 0) return;
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
        entry._replayGuard = setTimeout(() => { entry.replaying = false; }, 3000);
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
        const list = await api("GET", "api/sessions");
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

  function createTab(info) {
    const { term, fit } = makeTerminal();

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

    if (!cfg.readonly) {
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
      if (e.button !== 1 || cfg.readonly || !cfg.middleclickPaste) return;
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
    const titleEl = document.createElement("span");
    titleEl.className = "title";
    setTabTitle(titleEl, info);
    titleEl.title = "Double-click to rename";
    tabEl.appendChild(titleEl);

    if (cfg.multiSession) {
      const closeEl = document.createElement("span");
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
    // Double-click title to rename.
    titleEl.addEventListener("dblclick", (e) => {
      e.stopPropagation();
      beginRename(info.id, titleEl);
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
      id: info.id, term, fit, pane, tabEl, titleEl,
      ws: null, connected: false, exited: false, awaitRestart: false,
      sessionGone: false, replaying: false, reconnectTimer: null, _backoff: 500,
      renderer: null, rendererLoaded: false,
    };
    panes.set(info.id, entry);
    connect(entry, info.id);
    return entry;
  }

  // Persist the DOM tab order server-side so it survives reloads.
  async function saveOrder() {
    const ids = Array.from(tabbar.querySelectorAll(".tab"))
      .map((t) => t.dataset.sid).filter(Boolean);
    try { await api("PUT", "api/sessions/order", { order: ids }); } catch (e) {}
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
        api("PATCH", "api/sessions/" + encodeURIComponent(id), { title: next })
          .catch(() => { titleEl.textContent = old; });
      } else {
        titleEl.textContent = old;
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
        info = await api("POST", "api/sessions" + location.search);
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
      await api("POST", "api/sessions/" + encodeURIComponent(entry.id) + "/restart");
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
    try { await api("GET", "api/sessions"); } catch (e) { up = false; }
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
    try { await api("DELETE", "api/sessions/" + encodeURIComponent(id)); } catch (e) {}
    removeTabUI(id);
  }

  async function addSession() {
    try {
      // Forward the page query so url_arg/url_env apply to new tabs too;
      // empty title -> server default.
      const info = await api("POST", "api/sessions" + location.search);
      createTab(info);
      activate(info.id);
    } catch (e) {
      showStatus(e.message || "cannot add session");
    }
  }

  // Reconcile with the server: update titles (auto cwd titles, renames from
  // another window) and adopt sessions we have no tab for — e.g. when the
  // page was loaded from a stale cache or another window created a tab.
  async function syncSessions() {
    if (document.hidden) return;
    let list;
    try { list = await api("GET", "api/sessions"); } catch (e) { return; }
    for (const info of list) {
      const entry = panes.get(info.id);
      if (entry) {
        // In PS1 mode the shell's OSC titles own the display; don't let
        // the server's static titles overwrite them.
        if (!cfg.tabShowPS1 && !entry.renaming && entry.titleEl.textContent !== info.title) {
          setTabTitle(entry.titleEl, info);
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
    // React to connectivity changes: on restore, skip the pending backoff
    // and redial every disconnected tab immediately; while offline, show a
    // sticky notice (socket death detection can lag the actual outage).
    window.addEventListener("online", () => {
      // Always replace the sticky "network offline" notice — the sockets
      // may have survived a short blip, in which case nothing below runs.
      showStatus("network restored");
      if (!cfg.persistence) return;
      for (const [id, e] of panes) {
        if (!e.connected && !e.exited && !e.awaitRestart) {
          clearTimeout(e.reconnectTimer);
          e._backoff = 500;
          connect(e, id);
        }
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
    });

    // The session list is inlined into the page; falling back to the API
    // covers older cached pages and defensive corner cases.
    let list = Array.isArray(cfg.sessions) ? cfg.sessions : [];
    if (list.length === 0) {
      try { list = await api("GET", "api/sessions"); } catch (e) {}
    }
    if (!list || list.length === 0) {
      // Server guarantees one on index load, but be defensive.
      await addSession();
      return;
    }
    for (const info of list) createTab(info);
    // Reactivate the tab that was active before the reload, if it survived.
    const last = recallActive();
    activate(list.some((s) => s.id === last) ? last : list[0].id);
  }

  init();
})();
