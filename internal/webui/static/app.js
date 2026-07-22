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
    retryTimer: null
  };

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
      } else if (env.type === "state_snapshot") {
        state.snap = env.payload || { servers: [], requests: [] };
        render();
      }
    };

    ws.onclose = function () {
      if (state.ws !== ws) return; // superseded by a newer connection
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

  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) requestSnapshot();
  });

  // Re-render every second so Elapsed cells tick between snapshots.
  setInterval(function () {
    if (state.phase === "live") render();
  }, 1000);

  // ---------- boot ----------

  var boot = (!ignoreInjectedToken && injected().token) || localStorage.getItem(STORAGE_KEY);
  if (boot) {
    setPhase("live"); // optimistic: dashboard shell + "connecting" pill
    connect(boot);
  } else {
    setPhase("form");
  }
})();
