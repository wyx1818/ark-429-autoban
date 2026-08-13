(function () {
  "use strict";

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

  function render(data) {
    var tbody = document.getElementById("ban-tbody");
    document.getElementById("summary").textContent = "Active bans: " + data.count;
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
      cell(row, ban.api_key || ban.auth_id.replace(/^openai-compatibility:/, ""), "auth-id");
      var keyCell = cell(row, "", "auth-id");
      if (ban.masked_key) keyCell.className += " has-tooltip";
      var keySpan = document.createElement("span");
      keySpan.textContent = ban.key_hint || "unknown";
      if (!ban.key_hint) keySpan.className = "muted";
      keyCell.appendChild(keySpan);
      if (ban.masked_key) {
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
      cell(row, ban.reset_at);
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

  function refreshData() {
    var button = document.getElementById("refresh-btn");
    button.disabled = true;
    button.textContent = "Loading...";
    return fetch("bans.json")
      .then(function (response) { return response.json(); })
      .then(render)
      .catch(function () {})
      .finally(function () { button.disabled = false; button.textContent = "Refresh"; });
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest(".unban-btn");
    if (button && confirm("Unban " + button.dataset.authId + "?")) {
      window.location.assign("unban?auth_id=" + encodeURIComponent(button.dataset.authId));
    }
  });
  document.getElementById("refresh-btn").addEventListener("click", refreshData);
  document.getElementById("reload-btn").addEventListener("click", function () {
    if (confirm("Reload key labels from CPA config?")) window.location.assign("reload-config");
  });
  document.getElementById("unban-all-btn").addEventListener("click", function () {
    if (confirm("Unban all?")) window.location.assign("unban-all");
  });

  document.querySelectorAll(".countdown[data-seconds]").forEach(function (node) {
    node.textContent = remaining(Number(node.dataset.seconds));
  });
  syncTheme();
  setInterval(syncTheme, 2000);
  setInterval(refreshData, 15000);
})();
