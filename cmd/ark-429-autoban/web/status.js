(function () {
  "use strict";

  var PLUGIN = "ark-429-autoban";
  var API_BASE = "/v0/management/plugins/" + PLUGIN;
  var KEY_STORAGE = "ark429AutobanManagementKey";
  var ENC_PREFIX = "enc::v1::";
  var SECRET_SALT = "cli-proxy-api-webui::secure-storage";

  // --- management key resolution -------------------------------

  function hostWindow() {
    try {
      if (window.parent && window.parent !== window) return window.parent;
    } catch (_) {}
    return window;
  }

  function storageOf(win) {
    try {
      return win.localStorage;
    } catch (_) {
      return null;
    }
  }

  function obfuscationKey(win) {
    var host = "", ua = "";
    try {
      host = win.location.host;
      ua = win.navigator.userAgent;
    } catch (_) {}
    return SECRET_SALT + "|" + host + "|" + ua;
  }

  function xorDeobfuscate(payload, win) {
    var binary = atob(payload.slice(ENC_PREFIX.length));
    var key = obfuscationKey(win);
    var out = "";
    for (var i = 0; i < binary.length; i++) {
      out += String.fromCharCode(binary.charCodeAt(i) ^ key.charCodeAt(i % key.length));
    }
    return decodeURIComponent(escape(out));
  }

  // Read the management key from a management-panel localStorage entry.
  // Supports both the CPA built-in panel (key: "managementKey", value is the
  // obfuscated key itself) and CPA-Manager-Plus (key: "cli-proxy-auth", value
  // is an obfuscated JSON object containing managementKey).
  function keyFromPanel(store, key, win) {
    if (!store) return "";
    var raw;
    try { raw = store.getItem(key); } catch (_) { return ""; }
    if (!raw) return "";
    var text = raw;
    if (raw.indexOf(ENC_PREFIX) === 0) {
      try { text = xorDeobfuscate(raw, win); } catch (_) { return ""; }
    }
    try {
      var parsed = JSON.parse(text);
      if (parsed && typeof parsed.managementKey === "string") return parsed.managementKey;
    } catch (_) {
      // Not JSON: treat as the raw key (built-in panel stores the bare key).
      if (text && text.indexOf("{") !== 0) return text;
    }
    return "";
  }

  var keyInput = document.getElementById("mgmt-key");
  var mgmtKey = "";

  function initKey() {
    // 1. Previously saved by this page (highest priority).
    try { mgmtKey = localStorage.getItem(KEY_STORAGE) || ""; } catch (_) { mgmtKey = ""; }
    // 2. Auto-detect from the embedding management panel.
    if (!mgmtKey) {
      var host = hostWindow();
      mgmtKey = keyFromPanel(storageOf(host), "managementKey", host) ||
                keyFromPanel(storageOf(host), "cli-proxy-auth", host);
      // Also probe our own window's storage (standalone tab on same origin).
      if (!mgmtKey && host !== window) {
        mgmtKey = keyFromPanel(storageOf(window), "managementKey", window) ||
                  keyFromPanel(storageOf(window), "cli-proxy-auth", window);
      }
    }
    keyInput.value = mgmtKey;
  }

  function saveKey() {
    mgmtKey = keyInput.value.trim();
    try { localStorage.setItem(KEY_STORAGE, mgmtKey); } catch (_) {}
    if (mgmtKey) {
      setNotice("Key saved.", false);
      startPolling();
      refreshData();
    } else {
      try { localStorage.removeItem(KEY_STORAGE); } catch (_) {}
      stopPolling();
      setNotice("Key cleared.", false);
    }
  }

  document.getElementById("save-key-btn").addEventListener("click", saveKey);
  keyInput.addEventListener("keydown", function (e) {
    if (e.key === "Enter") saveKey();
  });

  // --- API helpers ---------------------------------------------

  function apiFetch(path, options) {
    options = options || {};
    var headers = options.headers || {};
    // Authorization: Bearer works both when the page is embedded in the
    // CPA-Manager-Plus panel (its proxy verifies this header, then swaps in
    // the saved CPA management key upstream) and when talking to CPA directly
    // (accepted alongside X-Management-Key by the CPA management middleware).
    if (mgmtKey) headers["Authorization"] = "Bearer " + mgmtKey;
    if (options.body && !headers["Content-Type"]) headers["Content-Type"] = "application/json";
    return fetch(API_BASE + path, {
      method: options.method || "GET",
      headers: headers,
      body: options.body || undefined,
      credentials: "same-origin"
    });
  }

  function setNotice(text, isError) {
    var el = document.getElementById("auth-notice");
    if (!el) return;
    el.textContent = text || "";
    el.hidden = !text;
    el.className = "notice" + (isError ? " error" : "");
  }

  // Extract the server's own error message so the user can tell an invalid
  // key apart from an IP ban ("IP banned due to too many failed attempts").
  function serverError(response) {
    return response.json().catch(function () { return null; }).then(function (data) {
      if (data && (data.error || data.message)) {
        return "HTTP " + response.status + ": " + (data.error || data.message);
      }
      return "HTTP " + response.status;
    });
  }

  // --- rendering -----------------------------------------------

  function syncTheme() {
    try {
      var theme = window.parent && window.parent.document
        ? window.parent.document.documentElement.getAttribute("data-theme") : null;
      if (theme === "dark" || theme === "white") {
        document.documentElement.setAttribute("data-theme", theme);
        return;
      }
    } catch (_) {}
    document.documentElement.removeAttribute("data-theme");
  }

  function remaining(seconds) {
    var d = Math.floor(seconds / 86400);
    var h = Math.floor((seconds % 86400) / 3600);
    var m = Math.floor((seconds % 3600) / 60);
    var s = seconds % 60;
    if (d > 0) return d + "d " + h + "h " + m + "m";
    if (h > 0) return h + "h " + m + "m " + s + "s";
    if (m > 0) return m + "m " + s + "s";
    return s > 0 ? s + "s" : "";
  }

  function cell(row, value, className) {
    var td = document.createElement("td");
    if (className) td.className = className;
    td.textContent = value;
    row.appendChild(td);
    return td;
  }

  var refreshTimer = null;
  var refreshRemaining = 0;

  function render(data) {
    var tbody = document.getElementById("ban-tbody");
    document.getElementById("summary").textContent = "Active bans: " + data.count + " · Scanned keys: " + (data.scanned_keys || 0) + (refreshTimer ? " · Refresh in " + Math.ceil(refreshRemaining) + "s" : "");
    tbody.replaceChildren();
    if (!data.bans.length) {
      var emptyRow = document.createElement("tr");
      var empty = cell(emptyRow, "No active bans. All credentials are available.", "empty");
      empty.colSpan = 6;
      tbody.appendChild(emptyRow);
      return;
    }
    data.bans.forEach(function (ban) {
      var row = document.createElement("tr");
      // API Key column ("ark-plan #4"): hover shows the full CPA auth ID.
      var apiCell = cell(row, "", "auth-id has-tooltip");
      var apiSpan = document.createElement("span");
      apiSpan.textContent = ban.api_key || ban.auth_id.replace(/^openai-compatibility:/, "");
      var apiTip = document.createElement("span");
      apiTip.className = "tooltip tooltip-left";
      apiTip.textContent = ban.auth_id;
      apiSpan.appendChild(apiTip);
      apiCell.appendChild(apiSpan);
      var keyCell = cell(row, "", "auth-id");
      if (ban.masked_key && ban.masked_key !== ban.key_hint) keyCell.className += " has-tooltip";
      var keySpan = document.createElement("span");
      keySpan.textContent = ban.key_hint || "unknown";
      if (!ban.key_hint) keySpan.className = "muted";
      keyCell.appendChild(keySpan);
      if (ban.masked_key && ban.masked_key !== ban.key_hint) {
        var tip = document.createElement("span");
        tip.className = "tooltip";
        tip.textContent = ban.masked_key;
        keySpan.appendChild(tip);
      }
      var winCell = cell(row, "", "window-cell");
      if (ban.error_code) winCell.className += " has-tooltip";
      var winSpan = document.createElement("span");
      winSpan.textContent = ban.window;
      winCell.appendChild(winSpan);
      if (ban.error_code) {
        var winTip = document.createElement("span");
        winTip.className = "tooltip";
        winTip.textContent = ban.error_code;
        winSpan.appendChild(winTip);
      }
      cell(row, ban.reset_at || "", "has-tooltip");
      var resetSpan = row.lastChild;
      var resetInner = document.createElement("span");
      resetInner.textContent = ban.reset_at || "";
      resetSpan.textContent = "";
      var resetTip = document.createElement("span");
      resetTip.className = "tooltip";
      if (ban.reset_at_unix) {
        resetTip.textContent = new Date(ban.reset_at_unix * 1000).toISOString();
      }
      resetInner.appendChild(resetTip);
      resetSpan.appendChild(resetInner);
      cell(row, remaining(ban.remaining_seconds), "countdown");
      var action = cell(row, "");
      var button = document.createElement("button");
      button.type = "button";
      button.className = "unban-btn";
      button.dataset.authId = ban.auth_id;
      button.textContent = "Unban";
      action.appendChild(button);
      tbody.appendChild(row);
    });
  }

  var REFRESH_INTERVAL = 30000;

  // Start the auto-refresh polling loop only when a key is available.
  function startPolling() {
    if (refreshTimer) return;
    refreshRemaining = REFRESH_INTERVAL / 1000;
    refreshTimer = setInterval(function () {
      refreshRemaining -= 1;
      if (refreshRemaining <= 0) {
        refreshRemaining = REFRESH_INTERVAL / 1000;
        refreshData();
      }
      updateSummaryCountdown();
    }, 1000);
  }

  function stopPolling() {
    if (refreshTimer) { clearInterval(refreshTimer); refreshTimer = null; }
    updateSummaryCountdown();
  }

  function updateSummaryCountdown() {
    var el = document.getElementById("summary");
    var text = el.textContent.replace(/ · Refresh in \d+s$/, "");
    if (refreshTimer) text += " · Refresh in " + Math.ceil(refreshRemaining) + "s";
    el.textContent = text;
  }

  var refreshInFlight = false;

  function refreshData() {
    if (refreshInFlight) return Promise.resolve();
    refreshInFlight = true;
    var button = document.getElementById("refresh-btn");
    button.disabled = true;
    button.textContent = "Loading...";
    refreshRemaining = REFRESH_INTERVAL / 1000;
    return apiFetch("/bans")
      .then(function (response) {
        if (!response.ok) {
          // Stop polling on auth failures so a bad key does not hammer the
          // API (and trip the 5-failure IP ban).
          if (response.status === 401 || response.status === 403) stopPolling();
          return serverError(response).then(function (msg) {
            setNotice(msg, true);
            return null;
          });
        }
        setNotice("");
        return response.json();
      })
      .then(function (data) { if (data) render(data); })
      .catch(function () { setNotice("Request failed. Check network/panel connection.", true); })
      .finally(function () {
        refreshInFlight = false;
        button.disabled = false;
        button.textContent = "Refresh";
      });
  }

  function postAction(path, body, message) {
    return apiFetch(path, { method: "POST", body: body ? JSON.stringify(body) : undefined })
      .then(function (response) {
        if (!response.ok) {
          return serverError(response).then(function (msg) { setNotice(msg, true); });
        }
        return response.json().then(function (data) {
          if (message && data && typeof data[message] !== "undefined") {
            setNotice(message === "reloaded"
              ? "Reloaded " + data[message] + " key labels."
              : "Removed " + data[message] + " ban(s).");
          } else {
            setNotice("");
          }
        });
      })
      .then(refreshData)
      .catch(function () { setNotice("Request failed. Check network/panel connection.", true); });
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest(".unban-btn");
    if (button && confirm("Unban " + button.dataset.authId + "?")) {
      postAction("/unban", { auth_id: button.dataset.authId }, "removed");
    }
  });
  document.getElementById("refresh-btn").addEventListener("click", refreshData);
  document.getElementById("reload-btn").addEventListener("click", function () {
    if (confirm("Reload key labels from CPA config?")) postAction("/reload-config", null, "reloaded");
  });
  document.getElementById("unban-all-btn").addEventListener("click", function () {
    if (confirm("Unban all?")) postAction("/unban-all", null, "removed");
  });

  document.querySelectorAll(".countdown[data-seconds]").forEach(function (node) {
    node.textContent = remaining(Number(node.dataset.seconds));
  });
  // Fill reset-at tooltips from data-unix.
  document.querySelectorAll(".tooltip[data-unix]").forEach(function (node) {
    var unix = Number(node.dataset.unix);
    if (unix) node.textContent = new Date(unix * 1000).toISOString();
  });
  initKey();
  syncTheme();
  setInterval(syncTheme, 2000);
  if (mgmtKey) {
    startPolling();
    refreshData();
  } else {
    setNotice("No management key. Enter a key to enable live refresh.", false);
  }
})();
