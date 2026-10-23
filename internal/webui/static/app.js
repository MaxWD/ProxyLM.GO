// ProxyLM Web UI — vanilla JS client for the /admin/stream WebSocket.
// The daemon pushes a full state snapshot every second; rendering is a pure
// function of the last snapshot (same model as the TUI). Read-only by design.
(function () {
  "use strict";

  var STORAGE_KEY = "proxylm_admin_key";
  var SUBPROTO = "proxylm-admin";
  var TOKEN_PREFIX = "proxylm-token.";
  var T_LOAD_CONFIDENT_MIN = 3; // mirrors TUI: below this, t_load gets a "*"

  // 12-color server palette, applied by index in the priority-sorted list
  // (mirrors the TUI's ServerColorByIndex approach).
  var SERVER_COLORS = [
    "#58a6ff", "#d2a8ff", "#7ee787", "#ffa657", "#f778ba", "#79c0ff",
    "#e3b341", "#56d4dd", "#ff7b72", "#a5d6ff", "#d29922", "#95de64"
  ];

  // ---------- state ----------

  var state = {
    ws: null,
    phase: "form",          // form | connecting | live | reconnecting
    gotHello: false,
    everConnected: false,   // ever got hello this page session
    hello: null,            // {version, num_servers, show_completed_minutes}
    snap: null,             // last state_snapshot payload
    selected: null,         // {type:"server",name} | {type:"request",id}
    retryMs: 1000,
    retryTimer: null,
    view: "live",           // live | usage
    uWindow: "24h",         // 1h | 24h | 7d | all
    uTab: "models",         // models | clients | matrix
    uData: null,            // last usage_stats payload for uWindow
    uRecvAt: 0,             // Date.now() when uData arrived
    uSort: {                // per-tab sort; default Requests desc
      models: { key: "requests", dir: -1 },
      clients: { key: "requests", dir: -1 }
    },
    uTimer: null
  };

  var PREFS_KEY = "proxylm_usage_prefs";
  var WINDOWS = ["1h", "24h", "7d", "all"];
  var TABS = ["models", "clients", "matrix"];
  var STATS_POLL_MS = 10000;
  var FAIL_WARN_PCT = 5;

  var $ = function (id) { return document.getElementById(id); };

  // ---------- helpers ----------

  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function b64url(s) {
    var bytes = new TextEncoder().encode(s);
    var bin = "";
    for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
    return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  function parseT(s) {
    if (!s) return null;
    var t = Date.parse(s);
    return isNaN(t) ? null : t;
  }

  function fmtClock(ts) {
    if (!ts) return "—";
    var d = new Date(ts);
    return String(d.getHours()).padStart(2, "0") + ":" +
           String(d.getMinutes()).padStart(2, "0") + ":" +
           String(d.getSeconds()).padStart(2, "0");
  }

  function fmtMs(ms) {
    if (!(ms > 0)) return "—";
    if (ms < 1000) return Math.round(ms) + "ms";
    if (ms < 10000) return (ms / 1000).toFixed(1) + "s";
    if (ms < 60000) return Math.round(ms / 1000) + "s";
    var m = Math.floor(ms / 60000);
    return m + "m" + String(Math.round((ms % 60000) / 1000)).padStart(2, "0") + "s";
  }

  function fmtTok(v) { return v > 0 ? v.toFixed(1) : "—"; }

  function fmtTLoad(ms, loaded) {
    if (!(loaded > 0)) return "—";
    return fmtMs(ms) + (loaded < T_LOAD_CONFIDENT_MIN ? "*" : "");
  }

  // Port of the TUI's tokPerSecCIHalf: convert a CI half-width on the
  // ms-per-token coefficient into a half-width on the tok/s value.
  function tokPerSecCIHalf(tokPerSec, kCI) {
    if (!(tokPerSec > 0) || !(kCI > 0)) return 0;
    var k = 1000.0 / tokPerSec;
    var lower = k + kCI, upper = k - kCI;
    if (upper <= 0) return tokPerSec;
    return (1000.0 / upper - 1000.0 / lower) / 2.0;
  }

  function tokCell(v, kCI) {
    if (!(v > 0)) return '<span class="dim">—</span>';
    var half = tokPerSecCIHalf(v, kCI);
    var s = esc(v.toFixed(1));
    if (half > 0) s += '<span class="ci">±' + esc(half.toFixed(1)) + "</span>";
    return s;
  }

  function serverColor(idx) { return SERVER_COLORS[idx % SERVER_COLORS.length]; }

  function sortedServers() {
    var list = (state.snap && state.snap.servers ? state.snap.servers.slice() : []);
    list.sort(function (a, b) {
      if (a.priority !== b.priority) return a.priority - b.priority;
      return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
    });
    return list;
  }

  function serverIndexMap() {
    var m = {};
    sortedServers().forEach(function (s, i) { m[s.name] = i; });
    return m;
  }

  function serverByName(name) {
    var list = (state.snap && state.snap.servers) || [];
    for (var i = 0; i < list.length; i++) if (list[i].name === name) return list[i];
    return null;
  }

  function ttlMs() {
    var min = state.hello && state.hello.show_completed_minutes;
    return min > 0 ? min * 60000 : 0;
  }

  // Visible requests: completed/failed rows hide after the TTL (like the TUI).
  function visibleRequests() {
    var reqs = (state.snap && state.snap.requests ? state.snap.requests.slice() : []);
    var ttl = ttlMs(), now = Date.now();
    if (ttl > 0) {
      reqs = reqs.filter(function (r) {
        if (r.status !== "completed" && r.status !== "failed") return true;
        var done = parseT(r.completed_at);
        return !done || now - done <= ttl;
      });
    }
    reqs.sort(function (a, b) {
      var rank = { running: 0, queued: 1 };
      var ra = rank[a.status] !== undefined ? rank[a.status] : 2;
      var rb = rank[b.status] !== undefined ? rank[b.status] : 2;
      if (ra !== rb) return ra - rb;
      if (ra === 0) return (parseT(a.started_at) || 0) - (parseT(b.started_at) || 0);
      if (ra === 1) return (parseT(a.created_at) || 0) - (parseT(b.created_at) || 0);
      return (parseT(b.completed_at) || 0) - (parseT(a.completed_at) || 0);
    });
    return reqs;
  }

  // ---------- websocket ----------

  // The `proxylm web` command serves config.js with window.PROXYLM =
  // {ws: "<daemon /admin/stream URL>", token: "<optional admin key>"}.
  // Same-origin fallback covers serving the static dir next to the daemon
  // behind a reverse proxy.
  function injected() { return window.PROXYLM || {}; }

  function wsURL() {
    if (injected().ws) return injected().ws;
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    return proto + "//" + location.host + "/admin/stream";
  }

  function connect(key) {
    if (state.ws) { try { state.ws.close(); } catch (e) { /* noop */ } }
    state.gotHello = false;
    setConn(state.everConnected ? "reconnecting" : "connecting");

    var ws;
    try {
      ws = new WebSocket(wsURL(), [SUBPROTO, TOKEN_PREFIX + b64url(key)]);
    } catch (e) {
      showAuthError("Could not open a WebSocket connection.");
      return;
    }
    state.ws = ws;

    ws.onmessage = function (ev) {
      var env;
      try { env = JSON.parse(ev.data); } catch (e) { return; }
      if (env.type === "hello") {
        state.gotHello = true;
        state.everConnected = true;
        state.hello = env.payload || {};
        state.retryMs = 1000;
        localStorage.setItem(STORAGE_KEY, key);
        setPhase("live");
        setConn("live");
        renderHello();
        if (state.view === "usage") startStats(); // resend after (re)connect
      } else if (env.type === "state_snapshot") {
        state.snap = env.payload || { servers: [], requests: [] };
        render();
      } else if (env.type === "usage_stats") {
        var p = env.payload || {};
        if (p.window !== state.uWindow) return; // stale response for another window
        state.uData = p;
        state.uRecvAt = Date.now();
        renderUsage();
      }
    };

    ws.onclose = function () {
      if (state.ws !== ws) return; // superseded by a newer connection
      stopStats();
      if (state.gotHello || state.everConnected) {
        // Daemon restarted or network blip: keep the key, retry with backoff.
        setPhase("live"); // keep dashboard visible with last data
        setConn("reconnecting");
        scheduleRetry(key);
      } else {
        // First handshake with this key failed: unreachable daemon or bad key.
        // The browser hides the HTTP status of a failed WS upgrade, so we
        // cannot distinguish the two — say so and keep the stored key intact.
        setPhase("form");
        showAuthError("Connection failed — daemon unreachable or admin key rejected.");
      }
    };
  }

  function scheduleRetry(key) {
    if (state.retryTimer) clearTimeout(state.retryTimer);
    var delay = state.retryMs + Math.floor(Math.random() * 300);
    state.retryMs = Math.min(state.retryMs * 1.7, 10000);
    state.retryTimer = setTimeout(function () { connect(key); }, delay);
  }

  function requestSnapshot() {
    if (state.ws && state.ws.readyState === WebSocket.OPEN && state.gotHello) {
      state.ws.send(JSON.stringify({ type: "request_snapshot", time: new Date().toISOString() }));
    }
  }

  // ---------- usage stats: protocol ----------

  function sendStats() {
    if (state.ws && state.ws.readyState === WebSocket.OPEN && state.gotHello) {
      state.ws.send(JSON.stringify({
        type: "stats_request",
        time: new Date().toISOString(),
        payload: { window: state.uWindow }
      }));
    }
  }

  function stopStats() {
    if (state.uTimer) { clearInterval(state.uTimer); state.uTimer = null; }
  }

  function startStats() {
    stopStats();
    sendStats();
    state.uTimer = setInterval(function () {
      if (!document.hidden) sendStats();
    }, STATS_POLL_MS);
  }

  function loadPrefs() {
    try {
      var p = JSON.parse(localStorage.getItem(PREFS_KEY) || "{}");
      if (p.view === "live" || p.view === "usage") state.view = p.view;
      if (WINDOWS.indexOf(p.window) >= 0) state.uWindow = p.window;
      if (TABS.indexOf(p.tab) >= 0) state.uTab = p.tab;
    } catch (e) { /* noop */ }
  }

  function savePrefs() {
    try {
      localStorage.setItem(PREFS_KEY, JSON.stringify({
        view: state.view, window: state.uWindow, tab: state.uTab
      }));
    } catch (e) { /* noop */ }
  }

  // ---------- usage stats: pure pivots ----------

  function clientLabel(c) { return c ? c : "(anonymous)"; }

  function maxTime(a, b) {
    var ta = parseT(a), tb = parseT(b);
    return (tb || 0) > (ta || 0) ? b : a;
  }

  // Group flat rows by keyFn; returns a map of aggregates with sets of
  // servers / clients / models plus per-secondary counts.
  function aggregate(rows, keyFn) {
    var out = {};
    rows.forEach(function (r) {
      var k = keyFn(r);
      var a = out[k];
      if (!a) {
        a = out[k] = { name: k, requests: 0, failed: 0, busy: 0, queueW: 0,
          prompt: 0, output: 0, servers: {}, clients: {}, models: {}, last: "" };
      }
      var n = r.requests || 0;
      a.requests += n;
      a.failed += r.failed || 0;
      a.busy += r.busy_ms || 0;
      a.queueW += (r.queue_ms_avg || 0) * n;
      a.prompt += r.prompt_tokens || 0;
      a.output += r.output_tokens || 0;
      if (r.server) a.servers[r.server] = (a.servers[r.server] || 0) + n;
      a.clients[r.client || ""] = (a.clients[r.client || ""] || 0) + n;
      a.models[r.model] = (a.models[r.model] || 0) + n;
      a.last = maxTime(a.last, r.last_used);
    });
    return Object.keys(out).map(function (k) {
      var a = out[k];
      a.failPct = a.requests > 0 ? a.failed * 100 / a.requests : 0;
      a.avgRun = a.requests > 0 ? a.busy / a.requests : 0;
      a.avgQueue = a.requests > 0 ? a.queueW / a.requests : 0;
      a.tokens = a.prompt + a.output;
      a.nServers = Object.keys(a.servers).length;
      a.nClients = Object.keys(a.clients).length;
      a.lastTs = parseT(a.last) || 0;
      return a;
    });
  }

  function pivotModels(rows) { return aggregate(rows, function (r) { return r.model; }); }
  function pivotClients(rows) { return aggregate(rows, function (r) { return r.client || ""; }); }

  // rows = models, cols = clients (both ordered by total desc).
  function pivotMatrix(rows) {
    var cell = {}, rowTot = {}, colTot = {}, max = 0;
    rows.forEach(function (r) {
      var c = r.client || "", n = r.requests || 0;
      cell[r.model] = cell[r.model] || {};
      cell[r.model][c] = (cell[r.model][c] || 0) + n;
      rowTot[r.model] = (rowTot[r.model] || 0) + n;
      colTot[c] = (colTot[c] || 0) + n;
    });
    Object.keys(cell).forEach(function (m) {
      Object.keys(cell[m]).forEach(function (c) { if (cell[m][c] > max) max = cell[m][c]; });
    });
    var byTot = function (t) {
      return function (a, b) { return t[b] - t[a] || (a < b ? -1 : a > b ? 1 : 0); };
    };
    return {
      models: Object.keys(rowTot).sort(byTot(rowTot)),
      clients: Object.keys(colTot).sort(byTot(colTot)),
      cell: cell, rowTot: rowTot, colTot: colTot, max: max
    };
  }

  function sortItems(items, key, dir, str) {
    return items.slice().sort(function (a, b) {
      var x = a[key], y = b[key];
      var c = str ? (x < y ? -1 : x > y ? 1 : 0) : x - y;
      return c * dir || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
    });
  }

  function topModels(a) {
    var names = Object.keys(a.models).sort(function (x, y) {
      return a.models[y] - a.models[x] || (x < y ? -1 : 1);
    });
    var s = names.slice(0, 3).map(function (m) {
      return esc(m) + ' <span class="dim">' + a.models[m] + "</span>";
    }).join(", ");
    if (names.length > 3) s += ' <span class="dim">+' + (names.length - 3) + "</span>";
    return s;
  }

  // ---------- usage stats: formatting ----------

  function fmtCount(n) {
    if (!(n > 0)) return "0";
    if (n < 1000) return String(Math.round(n));
    if (n < 1e6) return (n / 1000).toFixed(n < 1e4 ? 1 : 0) + "k";
    return (n / 1e6).toFixed(n < 1e7 ? 1 : 0) + "M";
  }

  function fmtDur(ms) {
    if (!(ms > 0)) return "—";
    if (ms < 60000) return fmtMs(ms);
    var m = Math.floor(ms / 60000);
    if (m < 60) return m + "m " + Math.round((ms % 60000) / 1000) + "s";
    var h = Math.floor(m / 60);
    if (h < 24) return h + "h " + (m % 60) + "m";
    return Math.floor(h / 24) + "d " + (h % 24) + "h";
  }

  function fmtAgo(ts) {
    if (!ts) return "—";
    var s = Math.max(0, Math.round((Date.now() - ts) / 1000));
    if (s < 60) return s + "s ago";
    if (s < 3600) return Math.floor(s / 60) + "m ago";
    if (s < 86400) return Math.floor(s / 3600) + "h ago";
    return Math.floor(s / 86400) + "d ago";
  }

  function failCell(a) {
    if (!(a.requests > 0)) return '<span class="dim">—</span>';
    var txt = esc(a.failPct.toFixed(a.failPct < 10 ? 1 : 0)) + "%";
    return a.failPct > FAIL_WARN_PCT ? '<span class="warn">' + txt + "</span>" : txt;
  }

  function tokCountCell(a) {
    return a.tokens > 0 ? esc(fmtCount(a.prompt)) + " / " + esc(fmtCount(a.output)) : '<span class="dim">—</span>';
  }

  function serverChips(a, idx) {
    var names = Object.keys(a.servers).sort();
    if (!names.length) return '<span class="dim">—</span>';
    return names.map(function (n) {
      var color = idx[n] !== undefined ? serverColor(idx[n]) : "inherit";
      return '<span class="chip" style="color:' + color + '">' + esc(n) + "</span>";
    }).join("");
  }

  // ---------- usage stats: rendering ----------

  function sortHeader(cols, sort) {
    return "<tr>" + cols.map(function (c) {
      var cls = c.str ? ' class="l"' : "";
      var on = sort.key === c.k;
      return "<th" + cls + ' data-key="' + c.k + '" data-str="' + (c.str ? 1 : 0) + '"' +
        (on ? ' aria-sort="' + (sort.dir > 0 ? "ascending" : "descending") + '"' : "") + ">" +
        esc(c.l) + (on ? (sort.dir > 0 ? " ▲" : " ▼") : "") + "</th>";
    }).join("") + "</tr>";
  }

  var MODEL_COLS = [
    { k: "name", l: "Model", str: 1 }, { k: "requests", l: "Requests" },
    { k: "failPct", l: "Fail %" }, { k: "busy", l: "Busy time" },
    { k: "avgRun", l: "Avg run" }, { k: "avgQueue", l: "Avg queue" },
    { k: "tokens", l: "Tokens in/out" }, { k: "nServers", l: "Servers" },
    { k: "nClients", l: "Clients" }, { k: "lastTs", l: "Last used" }
  ];
  var CLIENT_COLS = [
    { k: "name", l: "Client", str: 1 }, { k: "requests", l: "Requests" },
    { k: "failPct", l: "Fail %" }, { k: "busy", l: "Busy time" },
    { k: "tokens", l: "Tokens in/out" }, { k: "nModels", l: "Top models" },
    { k: "lastTs", l: "Last used" }
  ];

  function modelsHTML(rows) {
    var sort = state.uSort.models, idx = serverIndexMap();
    var items = sortItems(pivotModels(rows), sort.key, sort.dir, sort.key === "name");
    return '<table class="u-table" data-tab="models"><thead>' + sortHeader(MODEL_COLS, sort) +
      "</thead><tbody>" + items.map(function (a) {
        return '<tr><td class="l">' + esc(a.name) + "</td>" +
          "<td>" + a.requests + "</td><td>" + failCell(a) + "</td>" +
          "<td>" + esc(fmtDur(a.busy)) + "</td><td>" + esc(fmtMs(a.avgRun)) + "</td>" +
          "<td>" + esc(fmtMs(a.avgQueue)) + "</td><td>" + tokCountCell(a) + "</td>" +
          "<td>" + serverChips(a, idx) + "</td><td>" + a.nClients + "</td>" +
          "<td>" + esc(fmtAgo(a.lastTs)) + "</td></tr>";
      }).join("") + "</tbody></table>";
  }

  function clientsHTML(rows) {
    var sort = state.uSort.clients;
    var items = pivotClients(rows).map(function (a) {
      a.nModels = Object.keys(a.models).length;
      a.label = clientLabel(a.name);
      return a;
    });
    items = sortItems(items, sort.key === "name" ? "label" : sort.key, sort.dir, sort.key === "name");
    return '<table class="u-table" data-tab="clients"><thead>' + sortHeader(CLIENT_COLS, sort) +
      "</thead><tbody>" + items.map(function (a) {
        return '<tr><td class="l">' + esc(a.label) + "</td>" +
          "<td>" + a.requests + "</td><td>" + failCell(a) + "</td>" +
          "<td>" + esc(fmtDur(a.busy)) + "</td><td>" + tokCountCell(a) + "</td>" +
          "<td>" + topModels(a) + "</td><td>" + esc(fmtAgo(a.lastTs)) + "</td></tr>";
      }).join("") + "</tbody></table>";
  }

  function matrixHTML(rows) {
    var m = pivotMatrix(rows);
    var head = '<tr><th class="l">Model</th>' + m.clients.map(function (c) {
      return "<th>" + esc(clientLabel(c)) + "</th>";
    }).join("") + '<th>Total</th></tr>';
    var body = m.models.map(function (mo) {
      return '<tr><td class="l">' + esc(mo) + "</td>" + m.clients.map(function (c) {
        var n = (m.cell[mo] && m.cell[mo][c]) || 0;
        if (!n) return '<td class="cell"><span class="dim">·</span></td>';
        var pct = Math.round(8 + 42 * n / m.max); // numeric only: safe in style
        return '<td class="cell" style="background:color-mix(in srgb, var(--teal) ' + pct +
          '%, transparent)">' + n + "</td>";
      }).join("") + '<td class="tot">' + m.rowTot[mo] + "</td></tr>";
    }).join("");
    var foot = '<tr><td class="l tot">Total</td>' + m.clients.map(function (c) {
      return '<td class="tot">' + m.colTot[c] + "</td>";
    }).join("") + '<td class="tot">' +
      m.models.reduce(function (s, mo) { return s + m.rowTot[mo]; }, 0) + "</td></tr>";
    return '<table class="u-table u-matrix"><thead>' + head + "</thead><tbody>" + body +
      "</tbody><tfoot>" + foot + "</tfoot></table>";
  }

  function renderUsageChrome() {
    Array.prototype.forEach.call(document.querySelectorAll("#view-switch button"), function (b) {
      b.setAttribute("aria-pressed", b.dataset.view === state.view ? "true" : "false");
    });
    Array.prototype.forEach.call(document.querySelectorAll("#u-window button"), function (b) {
      b.setAttribute("aria-pressed", b.dataset.window === state.uWindow ? "true" : "false");
    });
    Array.prototype.forEach.call(document.querySelectorAll("#u-tabs button"), function (b) {
      b.setAttribute("aria-pressed", b.dataset.tab === state.uTab ? "true" : "false");
    });
    $("live-view").hidden = state.view !== "live";
    $("usage-view").hidden = state.view !== "usage";
  }

  function renderUsage() {
    renderUsageChrome();
    if (state.view !== "usage") return;
    var d = state.uData;
    var upd = $("u-updated"), ret = $("u-retention"), err = $("u-error");
    var body = $("u-body"), empty = $("u-empty");
    upd.textContent = d ? "updated " + Math.max(0, Math.round((Date.now() - state.uRecvAt) / 1000)) + "s ago" : "loading…";
    ret.hidden = !(d && state.uWindow === "all");
    if (!ret.hidden) {
      ret.textContent = d.retention_days > 0 ? "history kept " + d.retention_days + " days" : "history kept forever";
    }
    err.hidden = !(d && d.error);
    err.textContent = d && d.error ? d.error : "";
    if (!d) { body.innerHTML = ""; empty.hidden = true; return; }
    var rows = d.rows || [];
    empty.hidden = rows.length > 0;
    empty.textContent = state.uWindow === "all" ? "No requests recorded" : "No requests in the last " + state.uWindow;
    if (!rows.length) { body.innerHTML = ""; return; }
    body.innerHTML = state.uTab === "models" ? modelsHTML(rows)
      : state.uTab === "clients" ? clientsHTML(rows) : matrixHTML(rows);
  }

  function setView(v) {
    if (state.view === v) return;
    state.view = v;
    savePrefs();
    if (v === "usage") { renderUsage(); if (state.gotHello) startStats(); }
    else { stopStats(); renderUsageChrome(); render(); }
  }

  // ---------- phases / chrome ----------

  function setPhase(phase) {
    state.phase = phase;
    $("auth").hidden = phase !== "form";
    $("dash").hidden = phase === "form";
  }

  function setConn(s) {
    var el = $("conn");
    el.dataset.state = s;
    el.textContent = s === "live" ? "connected" : s;
  }

  function showAuthError(msg) {
    var el = $("auth-error");
    el.textContent = msg;
    el.hidden = false;
  }

  function renderHello() {
    $("daemon-version").textContent = state.hello.version ? "v" + state.hello.version.replace(/^v/, "") : "";
  }

  // ---------- rendering ----------

  function render() {
    if (state.phase !== "live" || !state.snap) return;
    renderCounters();
    renderRack();
    renderRequests();
    renderDetail();
  }

  function renderCounters() {
    var reqs = visibleRequests();
    var q = 0, run = 0, done = 0, fail = 0;
    reqs.forEach(function (r) {
      if (r.status === "queued") q++;
      else if (r.status === "running") run++;
      else if (r.status === "completed") done++;
      else if (r.status === "failed") fail++;
    });
    var servers = (state.snap.servers || []);
    var healthy = servers.filter(function (s) { return s.healthy; }).length;
    $("counters").innerHTML =
      '<span class="c-amber"><b>' + q + "</b> queued</span>" +
      '<span class="c-green"><b>' + run + "</b> running</span>" +
      "<span><b>" + done + "</b> done</span>" +
      '<span class="c-red"><b>' + fail + "</b> failed</span>" +
      "<span><b>" + healthy + "/" + servers.length + "</b> healthy</span>";
  }

  function renderRack() {
    var servers = sortedServers();
    var rack = $("rack");
    if (!servers.length) {
      rack.innerHTML = '<p class="empty">No backends configured.</p>';
      return;
    }
    rack.innerHTML = servers.map(function (s, i) {
      var mode = !s.healthy ? "down" : (s.in_flight ? "busy" : "idle");
      var selected = state.selected && state.selected.type === "server" && state.selected.name === s.name;
      var model = s.current_model
        ? '<span class="unit-model">' + esc(s.current_model) + "</span>"
        : '<span class="unit-model is-idle">idle</span>';
      var eject = "";
      if (s.current_model && s.loaded_models_probed &&
          (s.loaded_models || []).indexOf(s.current_model) < 0) {
        eject = '<span class="unit-eject" title="current model is no longer in backend memory">⏏</span>';
      }
      var q = '<span class="unit-q' + (s.queue_depth > 0 ? " has-q" : "") + '">[Q:' + (s.queue_depth || 0) + "]</span>";
      var slow = s.slow ? '<span class="badge-slow">SLOW</span>' : "";
      var metric = "";
      if (s.perf_ok && s.current_model) {
        metric = '<div class="unit-metric">' + esc(fmtTLoad(s.t_load_ms, s.t_load_loaded)) +
          " · ↓" + esc(fmtTok(s.tok_in_per_sec)) + " · ↑" + esc(fmtTok(s.tok_out_per_sec)) + " tok/s</div>";
      }
      return '<div class="unit" role="listitem" tabindex="0" data-name="' + esc(s.name) + '"' +
        ' data-health="' + (s.healthy ? "up" : "down") + '"' +
        (selected ? ' data-selected="1"' : "") + ">" +
        '<div class="unit-row1">' +
          '<span class="lamp" data-mode="' + mode + '">●</span>' +
          '<span class="unit-name" style="color:' + serverColor(i) + '">' + esc(s.name) + "</span>" +
          '<span class="unit-prio">p' + s.priority + "</span>" +
        "</div>" +
        '<div class="unit-row2">' + model + eject + q + slow + "</div>" +
        metric +
      "</div>";
    }).join("");
  }

  function renderRequests() {
    var reqs = visibleRequests();
    var idx = serverIndexMap();
    $("req-empty").hidden = reqs.length > 0;
    $("req-body").innerHTML = reqs.map(function (r) {
      var selected = state.selected && state.selected.type === "request" && state.selected.id === r.id;

      var srv = "—";
      if (r.server_name) {
        var color = idx[r.server_name] !== undefined ? serverColor(idx[r.server_name]) : "inherit";
        srv = '<span style="color:' + color + '">' + esc(r.server_name) + "</span>";
      }
      if (r.last_failed_server) {
        srv = '<span class="srv-failed-x" title="previous attempt failed on ' +
          esc(r.last_failed_server) + '">✗</span> ' + srv;
      }

      var model = esc(r.model);
      var assigned = r.server_name ? serverByName(r.server_name) : null;
      if (r.status === "queued" && assigned && assigned.current_model &&
          assigned.current_model !== r.model) {
        model += '<span class="needs-swap" title="needs a model swap on ' + esc(r.server_name) + '">~</span>';
      }

      var st = '<span class="st st-' + esc(r.status) + '">' + esc(r.status) + "</span>";
      if (r.attempt > 1 || (r.status === "failed" && r.attempt > 0)) {
        st += ' <span class="attempts">(' + r.attempt + "/" + r.max_attempts + ")</span>";
      }

      var started = parseT(r.started_at), completed = parseT(r.completed_at);
      var elapsed = "—";
      if (started) {
        elapsed = fmtMs((completed || Date.now()) - started);
      }

      var tok = (r.prompt_tokens || r.output_tokens)
        ? (r.prompt_tokens || 0) + "→" + (r.output_tokens || 0) : "—";

      return '<tr data-id="' + esc(r.id) + '"' + (selected ? ' data-selected="1"' : "") + ">" +
        "<td>" + esc(r.id.slice(-4)) + "</td>" +
        "<td>" + esc(r.client_name) + "</td>" +
        "<td>" + model + "</td>" +
        "<td>" + srv + "</td>" +
        "<td>" + st + "</td>" +
        "<td>" + (r.model_reloaded ? '<span class="rm-yes">✓</span>' : '<span class="rm-no">—</span>') + "</td>" +
        "<td>" + fmtClock(parseT(r.created_at)) + "</td>" +
        "<td>" + fmtClock(started) + "</td>" +
        "<td>" + elapsed + "</td>" +
        "<td>" + tok + "</td>" +
        '<td class="col-endpoint">' + esc(r.endpoint) + "</td>" +
      "</tr>";
    }).join("");
  }

  function renderDetail() {
    var el = $("detail"), title = $("detail-title");
    if (!state.selected) {
      title.textContent = "Detail";
      el.innerHTML = '<p class="empty">Select a server or a request to inspect it.</p>';
      return;
    }
    if (state.selected.type === "server") {
      var s = serverByName(state.selected.name);
      if (!s) { state.selected = null; renderDetail(); return; }
      title.textContent = "Server · " + s.name;
      el.innerHTML = serverDetailHTML(s);
    } else {
      var reqs = (state.snap.requests || []);
      var r = null;
      for (var i = 0; i < reqs.length; i++) if (reqs[i].id === state.selected.id) { r = reqs[i]; break; }
      if (!r) { state.selected = null; renderDetail(); return; }
      title.textContent = "Request · …" + r.id.slice(-4);
      el.innerHTML = requestDetailHTML(r);
    }
  }

  function kv(rows) {
    return '<dl class="kv">' + rows.map(function (p) {
      return "<dt>" + p[0] + "</dt><dd>" + p[1] + "</dd>";
    }).join("") + "</dl>";
  }

  function serverDetailHTML(s) {
    var models = (s.models || []);
    var modelsHTML = models.length
      ? '<span class="models-list">' + models.map(function (m) {
          return m === s.current_model ? '<span class="cur">▶' + esc(m) + "</span>" : esc(m);
        }).join(", ") + "</span>"
      : '<span class="dim">none discovered</span>';

    var inMem;
    if (!s.loaded_models_probed) inMem = '<span class="dim">n/a</span>';
    else if (!(s.loaded_models || []).length) inMem = '<span class="dim">— (none)</span>';
    else inMem = esc(s.loaded_models.join(", "));

    var html = kv([
      ["URL", esc(s.url)],
      ["Health", s.healthy ? '<span class="ok">healthy</span>' : '<span class="bad">unhealthy</span>'],
      ["Current model", s.current_model ? esc(s.current_model) : '<span class="dim">idle</span>'],
      ["Queue depth", String(s.queue_depth || 0)],
      ["Failures", String(s.failure_count || 0)],
      ["In memory", inMem],
      ["Models (" + models.length + ")", modelsHTML]
    ]);

    var stats = s.per_model_stats || [];
    if (stats.length) {
      html += '<table class="perf-table"><thead><tr>' +
        "<th>model</th><th>ep</th><th>n</th><th>t_load</th><th>↓tok/s</th><th>↑tok/s</th><th>R²</th>" +
        "</tr></thead><tbody>";
      stats.forEach(function (m) {
        var tload = '<span class="dim">—</span>';
        if (m.loaded > 0) {
          tload = esc(fmtMs(m.t_load_ms));
          if (m.t_load_ci > 0) tload += '<span class="ci">±' + esc(fmtMs(m.t_load_ci)) + "</span>";
        }
        var r2 = m.r_squared > 0 ? m.r_squared.toFixed(2) : "—";
        if (m.fit_quality === "degraded") r2 = '<span class="fit-degraded">' + r2 + "</span>";
        html += "<tr><td>" + esc(m.model) + "</td>" +
          '<td class="dim">' + esc((m.endpoint || "").replace("/v1/", "")) + "</td>" +
          "<td>" + m.samples + "</td>" +
          "<td>" + tload + "</td>" +
          "<td>" + tokCell(m.tok_in_per_sec, m.k_in_ci) + "</td>" +
          "<td>" + tokCell(m.tok_out_per_sec, m.k_out_ci) + "</td>" +
          "<td>" + r2 + "</td></tr>";
      });
      html += "</tbody></table>";
    }
    return html;
  }

  function requestDetailHTML(r) {
    var started = parseT(r.started_at), completed = parseT(r.completed_at);
    var html = kv([
      ["ID", esc(r.id)],
      ["Client", esc(r.client_name)],
      ["Model", esc(r.model)],
      ["Endpoint", esc(r.endpoint)],
      ["Stream", r.stream ? "yes" : "no"],
      ["Server", r.server_name ? esc(r.server_name) : "—"],
      ["Status", '<span class="st st-' + esc(r.status) + '">' + esc(r.status) + "</span>" +
        (r.attempt ? ' <span class="attempts">(' + r.attempt + "/" + r.max_attempts + ")</span>" : "")],
      ["HTTP", r.http_status ? String(r.http_status) : "—"],
      ["Created", fmtClock(parseT(r.created_at))],
      ["Started", fmtClock(started)],
      ["Completed", fmtClock(completed)],
      ["Queue wait", fmtMs(r.queue_wait_ms)],
      ["Tokens I→O", (r.prompt_tokens || 0) + " → " + (r.output_tokens || 0)],
      ["Model reloaded", r.model_reloaded ? "yes" : "no"]
    ]);
    if (r.error_message) {
      html += '<p class="err">' + esc(r.error_message) + "</p>";
    }
    if (r.last_tokens) {
      html += '<div class="tail">' + esc(r.last_tokens) +
        (r.status === "running" ? '<span class="cursor">▊</span>' : "") + "</div>";
    }
    return html;
  }

  // ---------- events ----------

  $("auth-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var key = $("auth-key").value || localStorage.getItem(STORAGE_KEY) || "";
    if (!key) { showAuthError("Enter the admin key."); return; }
    $("auth-error").hidden = true;
    connect(key);
  });

  var ignoreInjectedToken = false; // set by "forget key" so --token doesn't reconnect us

  $("forget").addEventListener("click", function () {
    ignoreInjectedToken = true;
    stopStats();
    localStorage.removeItem(STORAGE_KEY);
    if (state.retryTimer) clearTimeout(state.retryTimer);
    if (state.ws) { try { state.ws.close(); } catch (e) { /* noop */ } }
    state.everConnected = false;
    state.snap = null;
    state.selected = null;
    $("auth-key").value = "";
    $("auth-error").hidden = true;
    setPhase("form");
  });

  $("rack").addEventListener("click", function (ev) {
    var unit = ev.target.closest(".unit");
    if (!unit) return;
    state.selected = { type: "server", name: unit.dataset.name };
    render();
  });
  $("rack").addEventListener("keydown", function (ev) {
    if (ev.key !== "Enter" && ev.key !== " ") return;
    var unit = ev.target.closest(".unit");
    if (!unit) return;
    ev.preventDefault();
    state.selected = { type: "server", name: unit.dataset.name };
    render();
  });

  $("req-body").addEventListener("click", function (ev) {
    var tr = ev.target.closest("tr[data-id]");
    if (!tr) return;
    state.selected = { type: "request", id: tr.dataset.id };
    render();
  });

  $("view-switch").addEventListener("click", function (ev) {
    var b = ev.target.closest("button[data-view]");
    if (b) setView(b.dataset.view);
  });

  $("u-window").addEventListener("click", function (ev) {
    var b = ev.target.closest("button[data-window]");
    if (!b || b.dataset.window === state.uWindow) return;
    state.uWindow = b.dataset.window;
    state.uData = null; // old window's data must not be shown under the new label
    savePrefs();
    renderUsage();
    if (state.view === "usage") startStats();
  });

  $("u-tabs").addEventListener("click", function (ev) {
    var b = ev.target.closest("button[data-tab]");
    if (!b || b.dataset.tab === state.uTab) return;
    state.uTab = b.dataset.tab;
    savePrefs();
    renderUsage();
  });

  $("u-body").addEventListener("click", function (ev) {
    var th = ev.target.closest("th[data-key]");
    var table = ev.target.closest("table[data-tab]");
    if (!th || !table) return;
    var s = state.uSort[table.dataset.tab];
    if (!s) return;
    if (s.key === th.dataset.key) s.dir = -s.dir;
    else { s.key = th.dataset.key; s.dir = th.dataset.str === "1" ? 1 : -1; }
    renderUsage();
  });

  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) {
      requestSnapshot();
      if (state.view === "usage") sendStats();
    }
  });

  // Re-render every second so Elapsed cells tick between snapshots.
  setInterval(function () {
    if (state.phase !== "live") return;
    if (state.view === "usage") {
      // Only the label ticks; re-rendering tables would reset their scroll.
      if (state.uData) {
        $("u-updated").textContent = "updated " +
          Math.max(0, Math.round((Date.now() - state.uRecvAt) / 1000)) + "s ago";
      }
      // "Last used" is relative; refresh it on arrival of data only.
    } else {
      render();
    }
  }, 1000);

  // ---------- boot ----------

  loadPrefs();
  renderUsageChrome();

  var boot = (!ignoreInjectedToken && injected().token) || localStorage.getItem(STORAGE_KEY);
  if (boot) {
    setPhase("live"); // optimistic: dashboard shell + "connecting" pill
    connect(boot);
  } else {
    setPhase("form");
  }
})();
