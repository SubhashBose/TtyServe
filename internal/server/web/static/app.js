/* goterm frontend controller */
(function () {
  "use strict";

  const cfg = window.GOTERM;
  const tabbar = document.getElementById("tabbar");
  const newtabBtn = document.getElementById("newtab");
  const termsEl = document.getElementById("terminals");
  const statusEl = document.getElementById("status");

  // sessionId -> { term, fit, ws, pane, tabEl, titleEl, connected, reconnectTimer }
  const panes = new Map();
  let activeId = null;

  function showStatus(msg) {
    statusEl.textContent = msg;
    statusEl.classList.add("show");
    clearTimeout(showStatus._t);
    showStatus._t = setTimeout(() => statusEl.classList.remove("show"), 1500);
  }

  // ---- protocol opcodes (must match server ws.go) ----
  const C_INPUT = "0", C_RESIZE = "1", C_PING = "2";
  const S_OUTPUT = 0x6f; // 'o'
  const S_EXIT = 0x65;   // 'e' — session stream ended

  async function api(method, path, body) {
    const opts = { method, headers: {} };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(path, opts);
    if (!res.ok) throw new Error(method + " " + path + " -> " + res.status);
    if (res.status === 204) return null;
    return res.json();
  }

  function makeTerminal() {
    const term = new Terminal({
      cursorBlink: true,
      fontFamily: "Menlo, Consolas, monospace",
      fontSize: 14,
      theme: { background: "#1e1e1e", foreground: "#cccccc" },
      disableStdin: !cfg.writeEnabled,
      scrollback: 10000,
    });
    const fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    return { term, fit };
  }

  function connect(entry, sessionId) {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = proto + "//" + location.host + "/ws?session=" + encodeURIComponent(sessionId);
    const ws = new WebSocket(url);
    ws.binaryType = "arraybuffer";
    entry.ws = ws;

    ws.onopen = () => {
      entry.connected = true;
      entry._backoff = 500;
      updateTabState(entry);
      showStatus("connected");
      sendResize(entry);
    };

    ws.onmessage = (ev) => {
      const data = new Uint8Array(ev.data);
      if (data.length === 0) return;
      if (data[0] === S_OUTPUT) {
        entry.term.write(data.subarray(1));
      } else if (data[0] === S_EXIT) {
        entry.exited = true;
      }
    };

    ws.onclose = async () => {
      if (!panes.has(sessionId)) return; // tab was closed locally
      entry.connected = false;
      updateTabState(entry);
      if (!cfg.persistence) {
        entry.term.write("\r\n\x1b[31m[disconnected]\x1b[0m\r\n");
        return;
      }
      // If the server signalled exit, or on any close, verify the session
      // still exists before retrying — otherwise we'd reconnect forever to a
      // session whose shell has exited.
      let alive;
      try {
        const list = await api("GET", "/api/sessions");
        alive = list.some((s) => s.id === sessionId);
      } catch (e) {
        // Network down: trust the exit signal if we got one, else keep retrying.
        alive = !entry.exited;
      }
      if (!alive) {
        entry.term.write("\r\n\x1b[33m[session ended]\x1b[0m\r\n");
        showStatus("session ended");
        setTimeout(() => removeTabUI(sessionId), 600);
        return;
      }
      entry.exited = false;
      showStatus("reconnecting…");
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

  function updateTabState(entry) {
    if (entry.tabEl) entry.tabEl.classList.toggle("disconnected", !entry.connected);
  }

  function createTab(info) {
    const { term, fit } = makeTerminal();

    const pane = document.createElement("div");
    pane.className = "term-pane";
    termsEl.appendChild(pane);
    term.open(pane);

    if (cfg.writeEnabled) {
      term.onData((d) => sendInput(entry, d));
    }
    term.onResize(() => sendResize(entry));

    // Tab element
    const tabEl = document.createElement("div");
    tabEl.className = "tab";
    const titleEl = document.createElement("span");
    titleEl.className = "title";
    titleEl.textContent = info.title;
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

    tabEl.addEventListener("click", () => activate(info.id));
    // Double-click title to rename.
    titleEl.addEventListener("dblclick", (e) => {
      e.stopPropagation();
      beginRename(info.id, titleEl);
    });

    // Insert before the "+ New" button if present.
    if (newtabBtn && newtabBtn.parentElement === tabbar) {
      tabbar.insertBefore(tabEl, newtabBtn);
    } else {
      tabbar.appendChild(tabEl);
    }

    const entry = {
      id: info.id, term, fit, pane, tabEl, titleEl,
      ws: null, connected: false, exited: false, reconnectTimer: null, _backoff: 500,
    };
    panes.set(info.id, entry);
    connect(entry, info.id);
    return entry;
  }

  function activate(id) {
    if (!panes.has(id)) return;
    activeId = id;
    for (const [sid, entry] of panes) {
      const on = sid === id;
      entry.pane.classList.toggle("active", on);
      entry.tabEl.classList.toggle("active", on);
      if (on) {
        setTimeout(() => { fitActive(); entry.term.focus(); }, 0);
      }
    }
  }

  function beginRename(id, titleEl) {
    const entry = panes.get(id);
    if (!entry) return;
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
      titleEl.contentEditable = "false";
      titleEl.removeEventListener("keydown", onKey);
      titleEl.removeEventListener("blur", onBlur);
      const next = titleEl.textContent.trim();
      if (commit && next && next !== old) {
        api("PATCH", "/api/sessions/" + encodeURIComponent(id), { title: next })
          .catch(() => { titleEl.textContent = old; });
      } else {
        titleEl.textContent = old;
      }
    }
    function onKey(e) {
      if (e.key === "Enter") { e.preventDefault(); finish(true); }
      else if (e.key === "Escape") { e.preventDefault(); finish(false); }
    }
    function onBlur() { finish(true); }
    titleEl.addEventListener("keydown", onKey);
    titleEl.addEventListener("blur", onBlur);
  }

  // Guard against a crash-looping command respawning sessions in a tight loop.
  let lastAutoSpawn = 0;

  function removeTabUI(id) {
    const entry = panes.get(id);
    if (!entry) return;
    clearTimeout(entry.reconnectTimer);
    panes.delete(id);
    if (entry.ws) { try { entry.ws.close(); } catch (e) {} }
    entry.term.dispose();
    entry.pane.remove();
    entry.tabEl.remove();
    if (activeId === id) {
      const first = panes.keys().next();
      if (!first.done) {
        activate(first.value);
      } else {
        const now = Date.now();
        if (now - lastAutoSpawn > 2000) {
          lastAutoSpawn = now;
          addSession();
        } else {
          showStatus("session ended — press + to start a new one");
        }
      }
    }
  }

  async function closeSession(id) {
    try { await api("DELETE", "/api/sessions/" + encodeURIComponent(id)); } catch (e) {}
    removeTabUI(id);
  }

  async function addSession() {
    try {
      const info = await api("POST", "/api/sessions", { title: "terminal" });
      createTab(info);
      activate(info.id);
    } catch (e) {
      showStatus("cannot add session");
    }
  }

  async function init() {
    if (newtabBtn) newtabBtn.addEventListener("click", addSession);
    window.addEventListener("resize", fitActive);
    if (window.ResizeObserver) {
      new ResizeObserver(() => fitActive()).observe(termsEl);
    }

    let list = [];
    try { list = await api("GET", "/api/sessions"); } catch (e) {}
    if (!list || list.length === 0) {
      // Server guarantees one on index load, but be defensive.
      await addSession();
      return;
    }
    for (const info of list) createTab(info);
    activate(list[0].id);
  }

  init();
})();
