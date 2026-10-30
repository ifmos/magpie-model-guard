// Model Guard console. No frameworks; plain fetch + DOM.
(function () {
  "use strict";

  var token = "";
  var state = { targets: [], prompts: [], config: {}, models: [] };

  // token comes from the console URL hash fragment (?#token=...)
  (function readToken() {
    var m = location.hash.match(/token=([A-Za-z0-9]+)/);
    if (m) token = m[1];
    try {
      if (!token) token = sessionStorage.getItem("mg-token") || "";
      else sessionStorage.setItem("mg-token", token);
    } catch (e) { /* private mode */ }
  })();

  function api(method, path, body) {
    return fetch(path, {
      method: method,
      headers: {
        "authorization": "Bearer " + token,
        "content-type": "application/json",
      },
      body: body ? JSON.stringify(body) : undefined,
    }).then(function (r) {
      if (r.status === 401) {
        toast("未授权：请从 Magpie 插件入口重新打开控制台");
        throw new Error("unauthorized");
      }
      return r.json().then(function (data) {
        if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
        return data;
      });
    });
  }

  function toast(msg) {
    var el = document.getElementById("toast");
    el.textContent = msg;
    el.classList.remove("hidden");
    setTimeout(function () { el.classList.add("hidden"); }, 4000);
  }

  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
    });
  }

  function fmtTime(s) {
    if (!s || s === "0001-01-01T00:00:00Z") return "—";
    var d = new Date(s);
    if (isNaN(d.getTime()) || d.getYear() < 100) return "—";
    return d.toLocaleString();
  }

  var stateName = {
    unknown: "未检测", checking: "检测中", passed: "通过",
    failed: "已禁用(降智)", unavailable: "检测不可用", review: "人工暂停",
  };

  // ---- tabs ----
  var tabs = document.querySelectorAll("#tabs button");
  tabs.forEach(function (b) {
    b.addEventListener("click", function () {
      tabs.forEach(function (x) { x.classList.remove("active"); });
      b.classList.add("active");
      document.querySelectorAll(".page").forEach(function (p) { p.classList.remove("active"); });
      document.getElementById("page-" + b.dataset.tab).classList.add("active");
      if (b.dataset.tab === "results") loadResults();
      if (b.dataset.tab === "targets") loadState();
    });
  });

  // ---- targets ----
  function loadState() {
    return api("GET", "/api/state").then(function (snap) {
      state.config = snap.config || {};
      state.targets = snap.targets || [];
      state.prompts = snap.prompts || [];
      renderTargets();
      renderSettings();
      if (snap.health) toast("后台：" + snap.health);
    }).catch(function (e) { toast(e.message); });
  }

  function renderTargets() {
    var tbody = document.querySelector("#target-table tbody");
    tbody.innerHTML = state.targets.map(function (t) {
      var ops = ["recheck", "allow", "hold", "enable", "disable", "delete"]
        .filter(function (a) {
          if (a === "enable" && t.enabled) return false;
          if ((a === "recheck" || a === "allow" || a === "hold" || a === "disable") && !t.enabled) return false;
          return true;
        })
        .map(function (a) {
          var label = { recheck: "重测", allow: "放行15分", hold: "暂停", enable: "启用", disable: "停用", delete: "删除" }[a];
          return '<button data-model="' + esc(t.model) + '" data-action="' + a + '">' + label + "</button>";
        }).join(" ");
      return "<tr>" +
        "<td>" + esc(t.model) + "</td>" +
        "<td class='state-" + esc(t.state) + "'>" + (stateName[t.state] || t.state) + "</td>" +
        "<td>" + fmtTime(t.checked_at) + "</td>" +
        "<td>" + fmtTime(t.next_check_at) + "</td>" +
        "<td class='reason'>" + esc(t.reason || "") + "</td>" +
        "<td>" + ops + "</td></tr>";
    }).join("");
    tbody.querySelectorAll("button").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var model = btn.dataset.model, action = btn.dataset.action;
        if (action === "delete") {
          if (!confirm("删除对 " + model + " 的监测？门禁将不再拦截该模型。")) return;
          api("POST", "/api/targets/delete", { model: model }).then(loadState).catch(function (e) { toast(e.message); });
          return;
        }
        api("POST", "/api/targets/action", { model: model, action: action })
          .then(loadState).catch(function (e) { toast(e.message); });
      });
    });
  }

  document.getElementById("refresh-models").addEventListener("click", function () {
    api("GET", "/api/models").then(function (models) {
      state.models = models || [];
      var pick = document.getElementById("model-pick");
      pick.innerHTML = state.models.map(function (m) {
        return "<option value='" + esc(m.id) + "'>" + esc(m.id) + "</option>";
      }).join("");
      if (!state.models.length) toast("模型列表为空：请检查网关配置");
    }).catch(function (e) { toast(e.message); });
  });

  document.getElementById("add-target").addEventListener("click", function () {
    var pick = document.getElementById("model-pick");
    var selected = Array.prototype.slice.call(pick.selectedOptions).map(function (o) { return o.value; });
    if (!selected.length) { toast("请先刷新并选择模型"); return; }
    var interval = parseInt(document.getElementById("new-interval").value, 10) || 30;
    var failwait = parseInt(document.getElementById("new-failwait").value, 10) || 0;
    var done = 0;
    selected.forEach(function (model) {
      api("POST", "/api/targets", {
        model: model, enabled: true, interval_min: interval, fail_recheck_min: failwait,
        prompt_ids: [], state: "unknown",
      }).then(function () {
        done++;
        if (done === selected.length) { toast("已添加 " + done + " 个监测目标"); loadState(); }
      }).catch(function (e) { toast(e.message); });
    });
  });

  // ---- prompts ----
  function renderPrompts() {
    var tbody = document.querySelector("#prompt-table tbody");
    tbody.innerHTML = state.prompts.map(function (p) {
      var grader = { none: "人工", contains: "包含", exact: "精确", regex: "正则" }[p.grader] || p.grader;
      var canRun = state.targets.length > 0;
      var runSel = canRun
        ? "<select class='run-prompt' data-prompt='" + esc(p.id) + "'>" +
          state.targets.map(function (t) { return "<option value='" + esc(t.model) + "'>" + esc(t.model) + "</option>"; }).join("") +
          "</select><button class='run-btn' data-prompt='" + esc(p.id) + "'>测试</button>"
        : "";
      var del = p.builtin ? "" : "<button class='del-prompt' data-id='" + esc(p.id) + "'>删除</button>";
      return "<tr>" +
        "<td>" + esc(p.id) + "</td>" +
        "<td>" + esc(p.name) + "</td>" +
        "<td>" + grader + "</td>" +
        "<td>" + esc(p.expect || "—") + "</td>" +
        "<td>" + (p.automatic ? "是" : "否") + "</td>" +
        "<td>" + runSel + del + "</td></tr>";
    }).join("");
    tbody.querySelectorAll(".del-prompt").forEach(function (b) {
      b.addEventListener("click", function () {
        if (!confirm("删除该题目？")) return;
        api("POST", "/api/prompts/delete", { id: b.dataset.id })
          .then(loadState).catch(function (e) { toast(e.message); });
      });
    });
    tbody.querySelectorAll(".run-btn").forEach(function (b) {
      b.addEventListener("click", function () {
        var sel = tbody.querySelector("select.run-prompt[data-prompt='" + b.dataset.prompt + "']");
        toast("测试进行中…");
        api("POST", "/api/probe", { model: sel.value, prompt_id: b.dataset.prompt })
          .then(function (res) {
            toast("结果：" + (res.verdict === "passed" ? "通过" : res.verdict === "failed" ? "未通过" : res.verdict));
          }).catch(function (e) { toast(e.message); });
      });
    });
  }

  var promptForm = document.getElementById("prompt-form");
  document.getElementById("new-prompt").addEventListener("click", function () {
    promptForm.classList.remove("hidden");
    document.getElementById("prompt-form-title").textContent = "新建题目";
  });
  document.getElementById("pf-cancel").addEventListener("click", function () {
    promptForm.classList.add("hidden");
  });
  promptForm.addEventListener("submit", function (ev) {
    ev.preventDefault();
    var p = {
      name: document.getElementById("pf-name").value,
      text: document.getElementById("pf-text").value,
      grader: document.getElementById("pf-grader").value,
      expect: document.getElementById("pf-expect").value,
      automatic: document.getElementById("pf-automatic").checked,
    };
    api("POST", "/api/prompts", p).then(function () {
      promptForm.classList.add("hidden");
      toast("题目已保存");
      loadState();
    }).catch(function (e) { toast(e.message); });
  });

  // ---- results ----
  function loadResults() {
    var sel = document.getElementById("result-model");
    if (!sel.options.length) {
      sel.innerHTML = "<option value=''>全部模型</option>" +
        state.targets.map(function (t) { return "<option>" + esc(t.model) + "</option>"; }).join("");
    }
    var model = sel.value;
    return api("GET", "/api/results?limit=100" + (model ? "&model=" + encodeURIComponent(model) : ""))
      .then(function (rows) {
        var box = document.getElementById("result-list");
        box.innerHTML = (rows || []).map(function (r) {
          return "<details><summary>" +
            "<span class='state-" + esc(r.verdict) + "'>" + esc(r.verdict) + "</span> " +
            esc(r.model) + " · " + esc(r.prompt_id) + " · " + fmtTime(r.created_at) + " · " + esc(r.source) +
            "</summary><pre>" + esc(r.answer || ("(" + (r.detail || "无回答") + ")")) + "</pre></details>";
        }).join("") || "<p>暂无记录</p>";
      }).catch(function (e) { toast(e.message); });
  }
  document.getElementById("refresh-results").addEventListener("click", loadResults);
  document.getElementById("result-model").addEventListener("change", loadResults);

  // ---- settings ----
  function renderSettings() {
    var c = state.config || {};
    if (document.activeElement && document.activeElement.id && document.activeElement.id.startsWith("cfg-")) return;
    document.getElementById("cfg-url").value = c.gateway_url || "";
    document.getElementById("cfg-key").value = c.gateway_key || "";
    document.getElementById("cfg-key").placeholder = c.gateway_url ? "已保存（留空不变）" : "magpie";
    document.getElementById("cfg-limit").value = c.daily_limit || 120;
    document.getElementById("cfg-timeout").value = c.timeout_sec || 180;
    document.getElementById("cfg-cool").value = c.cool_min || 15;
  }

  document.getElementById("save-config").addEventListener("click", function () {
    var c = {
      gateway_url: document.getElementById("cfg-url").value.trim(),
      gateway_key: document.getElementById("cfg-key").value,
      daily_limit: parseInt(document.getElementById("cfg-limit").value, 10),
      timeout_sec: parseInt(document.getElementById("cfg-timeout").value, 10),
      cool_min: parseInt(document.getElementById("cfg-cool").value, 10),
    };
    api("POST", "/api/config", c).then(function () {
      document.getElementById("config-msg").textContent = "已保存";
      toast("配置已保存");
      loadState();
    }).catch(function (e) {
      document.getElementById("config-msg").textContent = e.message;
    });
  });

  // hook prompts rendering into loadState
  var origLoadState = loadState;
  loadState = function () {
    return origLoadState().then(renderPrompts);
  };

  if (!token) {
    document.body.innerHTML = "<p class='no-token'>请从 Magpie 插件入口打开控制台。</p>";
    return;
  }
  loadState();
  setInterval(loadState, 15000);
})();
