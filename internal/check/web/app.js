// Model Guard console. Plain fetch + DOM; no framework required.
(function () {
  "use strict";

  var token = "";
  var state = { targets: [], prompts: [], checking: [], config: {}, models: [], health: "" };
  var configDirty = false;
  var selectedModelIds = new Set();
  var modelSearchQuery = "";
  var isModelDropdownOpen = false;
  var inFlightTests = {};
  var lastFocusedElement = null;
  var hasCheckingResults = false;
  var activePollTimer = null;
  var activePollInFlight = false;
  var supplierData = null;
  var supplierLoadPromise = null;
  var supplierReloadQueued = false;
  var supplierLoadSeq = 0;
  var supplierPending = Object.create(null);

  var sectionName = { targets: "监测目标", prompts: "检测题库", results: "检测记录", settings: "连接设置" };
  var stateName = {
    unknown: "未检测", checking: "检测中", passed: "通过",
    failed: "已禁用(降智)", unavailable: "检测不可用", review: "人工暂停",
  };
  var verdictName = { checking: "检测中", passed: "通过", failed: "未通过", unavailable: "不可用", manual: "人工" };
  var graderName = {
    none: "人工评价",
    number: "独立数字",
    contains: "包含关键字",
    exact: "精确匹配",
    regex: "正则匹配"
  };

  var defaultTimingPolicy = {
    normal_min: 30,
    error_retry_min: 2,
    isolation_first_min: 360,
    isolation_later_min: 1440,
    recovery_confirm_min: 2,
    unavailable_min: 30
  };

  function parseTimingPolicy(getVal) {
    var fields = [
      { key: "normal_min", id: "normal", label: "正常间隔" },
      { key: "error_retry_min", id: "retry", label: "错误重试" },
      { key: "isolation_first_min", id: "iso-first", label: "首次隔离" },
      { key: "isolation_later_min", id: "iso-later", label: "后续隔离" },
      { key: "recovery_confirm_min", id: "rec-confirm", label: "恢复确认" },
      { key: "unavailable_min", id: "unavail", label: "不可用冷却" }
    ];
    var res = {};
    for (var i = 0; i < fields.length; i++) {
      var f = fields[i];
      var raw = getVal(f);
      var val = Number(raw);
      if (!Number.isInteger(val) || val < 1 || val > 43200) {
        throw new Error(f.label + "必须为 1 到 43200 之间的整数分钟");
      }
      res[f.key] = val;
    }
    return res;
  }

  (function readToken() {
    var m = location.hash.match(/token=([A-Za-z0-9]+)/);
    if (m) token = m[1];
    try {
      if (!token) token = sessionStorage.getItem("mg-token") || "";
      else sessionStorage.setItem("mg-token", token);
    } catch (e) { /* private mode */ }
  })();

  function setConnectionStatus(status, text) {
    ["top-status-dot", "sidebar-status-dot"].forEach(function (id) {
      var dot = document.getElementById(id);
      if (dot) dot.className = status === "online" ? "is-online" : status === "warning" ? "is-warning" : "is-danger";
    });
    var topEl = document.getElementById("top-status");
    if (topEl) topEl.textContent = text;
    var sideEl = document.getElementById("sidebar-status");
    if (sideEl) sideEl.textContent = text;
  }

  function api(method, path, body) {
    var headers = { "content-type": "application/json" };
    if (token) headers.authorization = "Bearer " + token;
    return fetch(path, {
      method: method,
      headers: headers,
      body: body ? JSON.stringify(body) : undefined,
    }).then(function (r) {
      if (r.status === 401) {
        setConnectionStatus("danger", "未授权");
        toast("未授权：请从 Magpie 插件入口重新打开控制台");
        throw new Error("unauthorized");
      }
      return r.json().then(function (data) {
        if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
        return data;
      });
    }).catch(function (err) {
      if (err.message !== "unauthorized") {
        setConnectionStatus("danger", "连接异常");
      }
      throw err;
    });
  }

  function toast(msg) {
    var el = document.getElementById("toast");
    if (!el) return;
    el.textContent = msg;
    el.classList.remove("hidden");
    clearTimeout(toast.timer);
    toast.timer = setTimeout(function () { el.classList.add("hidden"); }, 4000);
  }

  // Escape HTML string and single quotes to prevent attribute breakout and XSS
  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>'"]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", "'": "&#39;", '"': "&quot;" }[c];
    });
  }

  // ---- response-model rule editor ----
  var responseMatchLabels = { exact: "精确", contains: "包含", glob: "通配 *", regex: "正则" };
  var responseActionLabels = { disable: "禁用目标", reroute: "后续改道", record: "仅记录" };

  function optionList(labels, selected) {
    return Object.keys(labels).map(function (k) {
      return "<option value='" + esc(k) + "'" + (k === selected ? " selected" : "") + ">" + esc(labels[k]) + "</option>";
    }).join("");
  }

  function responseRuleRowHtml(rule) {
    rule = rule || {};
    return "<div class='response-rule-row' data-response-rule>" +
      "<input type='hidden' class='rule-id' value='" + esc(rule.id || "") + "'>" +
      "<select class='rule-match' title='匹配方式'>" + optionList(responseMatchLabels, rule.match || "contains") + "</select>" +
      "<input class='rule-pattern' placeholder='真实模型匹配字符串' value='" + esc(rule.pattern || "") + "'>" +
      "<select class='rule-action' title='命中动作'>" + optionList(responseActionLabels, rule.action || "disable") + "</select>" +
      "<button type='button' class='rule-remove' title='删除规则'>×</button>" +
      "</div>";
  }

  function renderResponseRules(container, rules) {
    if (!container) return;
    rules = rules || [];
    container.innerHTML = rules.length
      ? rules.map(responseRuleRowHtml).join("")
      : "<div class='response-rule-empty'>暂无规则；命中后按动作禁用、改道或记录。</div>";
  }

  function appendResponseRule(container) {
    if (!container) return;
    var empty = container.querySelector(".response-rule-empty");
    if (empty) empty.remove();
    container.insertAdjacentHTML("beforeend", responseRuleRowHtml({}));
    var rows = container.querySelectorAll("[data-response-rule]");
    var row = rows[rows.length - 1];
    var input = row && row.querySelector(".rule-pattern");
    if (input) input.focus();
  }

  function readResponseRules(container) {
    if (!container) return [];
    var out = [];
    container.querySelectorAll("[data-response-rule]").forEach(function (row) {
      var pattern = (row.querySelector(".rule-pattern").value || "").trim();
      if (!pattern) throw new Error("业务回复模型规则的匹配字符串不能为空");
      var match = row.querySelector(".rule-match").value;
      if (match === "regex") {
        try { new RegExp(pattern); } catch (e) { throw new Error("响应模型正则无效：" + pattern); }
      }
      var action = row.querySelector(".rule-action").value;
      out.push({
        id: row.querySelector(".rule-id").value || "",
        match: match,
        pattern: pattern,
        action: action
      });
    });
    return out;
  }

  function bindResponseRuleEditor(container, markDirty) {
    if (!container || container.dataset.ruleBound === "1") return;
    container.dataset.ruleBound = "1";
    container.addEventListener("click", function (ev) {
      var btn = ev.target.closest && ev.target.closest(".rule-remove");
      if (!btn) return;
      var row = btn.closest("[data-response-rule]");
      if (row) row.remove();
      if (!container.querySelector("[data-response-rule]")) {
        container.innerHTML = "<div class='response-rule-empty'>暂无规则；命中后按动作禁用、改道或记录。</div>";
      }
      if (markDirty !== false && typeof markConfigDirty === "function") markConfigDirty();
    });
    container.addEventListener("change", function () {
      if (markDirty !== false && typeof markConfigDirty === "function") markConfigDirty();
    });
    container.addEventListener("input", function () {
      if (markDirty !== false && typeof markConfigDirty === "function") markConfigDirty();
    });
  }

  function fmtTime(s) {
    if (!s || s === "0001-01-01T00:00:00Z") return "—";
    var d = new Date(s);
    if (isNaN(d.getTime()) || d.getFullYear() < 100) return "—";
    return d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
  }

  function isBlocked(t) {
    if (typeof t.blocked === "boolean") return t.blocked;
    return t.state === "failed" || t.state === "review";
  }

  function updateOverview() {
    var targets = state.targets || [];
    var totalEnabled = targets.filter(function (t) { return t.enabled; }).length;
    var passedCount = targets.filter(function (t) { return t.enabled && !isBlocked(t) && t.state === "passed" && t.schedule_stage !== "retry"; }).length;
    var blockedCount = targets.filter(function (t) { return isBlocked(t); }).length;
    var unavailCount = targets.filter(function (t) { return t.enabled && t.state === "unavailable"; }).length;

    var statTotal = document.getElementById("stat-total");
    if (statTotal) statTotal.textContent = totalEnabled;
    var statTotalDesc = document.getElementById("stat-total-desc");
    if (statTotalDesc) statTotalDesc.textContent = "开启监测 / 共 " + targets.length + " 个";

    var statPassed = document.getElementById("stat-passed");
    if (statPassed) statPassed.textContent = passedCount;

    var statBlocked = document.getElementById("stat-blocked");
    if (statBlocked) statBlocked.textContent = blockedCount;

    var statUnavail = document.getElementById("stat-unavailable");
    if (statUnavail) statUnavail.textContent = unavailCount;

    var navCount = document.getElementById("nav-target-count");
    if (navCount) navCount.textContent = targets.length;

    // Scheduler badge: honest dynamic status (never fake enabled when not configured)
    var badge = document.getElementById("scheduler-badge");
    var badgeText = document.getElementById("scheduler-badge-text");
    var pulse = document.getElementById("scheduler-pulse");
    if (badge && badgeText) {
      if (state.health) {
        badge.className = "intro-badge is-warning";
        if (pulse) pulse.className = "pulse pulse-warn";
        badgeText.textContent = "调度异常: " + state.health;
      } else if (!state.config || !state.config.gateway_url) {
        badge.className = "intro-badge is-muted";
        if (pulse) pulse.className = "pulse pulse-muted";
        badgeText.textContent = "网关未配置";
      } else if (targets.length === 0) {
        badge.className = "intro-badge is-muted";
        if (pulse) pulse.className = "pulse pulse-muted";
        badgeText.textContent = "暂无监测目标";
      } else if (totalEnabled === 0) {
        badge.className = "intro-badge is-muted";
        if (pulse) pulse.className = "pulse pulse-muted";
        badgeText.textContent = "检测已全部暂停";
      } else {
        badge.className = "intro-badge is-good";
        if (pulse) pulse.className = "pulse";
        badgeText.textContent = "自动调度运行中 (" + totalEnabled + " 个监测中)";
      }
    }

    var routingEl = document.getElementById("routing-status");
    if (routingEl) {
      var routing = state.routing || {};
      routingEl.classList.toggle("routing-error", !!routing.error);
      routingEl.textContent = routing.error ? routing.error + "；直连门禁仍生效，后台将重试路由同步。请根据上述原因检查。" :
        routing.enabled ? "路由组同步正常 · 已管理 " + (routing.managed_groups || 0) + " 个组 · 检测探针保持可用。" :
        "路由组同步未启用 · 仅拦截完整模型 ID 的直接请求。";
    }

    var healthy = !state.health;
    setConnectionStatus(healthy ? "online" : "warning", healthy ? "后台连接正常" : "后台需要检查");
  }

  function renderModelFilter() {
    var sel = document.getElementById("result-model");
    if (!sel) return;
    var old = sel.value;
    sel.innerHTML = "<option value=''>全部模型</option>" + state.targets.map(function (t) {
      return "<option value='" + esc(t.model) + "'>" + esc(t.model) + "</option>";
    }).join("");
    sel.value = old;
  }

  // ---- navigation ----
  function isPageActive(tab) {
    var el = document.getElementById("page-" + tab);
    return !!(el && el.classList.contains("active"));
  }

  function hasActiveChecks() {
    return (state.checking && state.checking.length > 0) ||
      hasCheckingResults ||
      Object.keys(inFlightTests).length > 0;
  }

  function scheduleActivePoll() {
    clearTimeout(activePollTimer);
    if (!hasActiveChecks()) return;
    activePollTimer = setTimeout(function () {
      if (activePollInFlight) {
        scheduleActivePoll();
        return;
      }
      activePollInFlight = true;
      loadState().then(function () {
        activePollInFlight = false;
        scheduleActivePoll();
      });
    }, 2000);
  }

  document.querySelectorAll("#tabs button").forEach(function (b) {
    b.addEventListener("click", function () {
      document.querySelectorAll("#tabs button").forEach(function (x) { x.classList.remove("active"); });
      b.classList.add("active");
      document.querySelectorAll(".page").forEach(function (p) { p.classList.remove("active"); });
      var targetPage = document.getElementById("page-" + b.dataset.tab);
      if (targetPage) targetPage.classList.add("active");
      var mainShell = document.querySelector(".main-shell");
      if (mainShell) mainShell.scrollTop = 0;
      var currentSection = document.getElementById("current-section");
      if (currentSection) currentSection.textContent = sectionName[b.dataset.tab] || b.dataset.tab;
      if (b.dataset.tab === "results") loadResults();
      if (b.dataset.tab === "targets") loadState();
      if (b.dataset.tab === "prompts") renderPrompts();
      if (b.dataset.tab === "settings") loadSuppliers();
    });
  });

  // ---- state and targets ----
  function loadState() {
    loadSuppliers();
    return api("GET", "/api/state").then(function (snap) {
      state.config = snap.config || {};
      state.targets = snap.targets || [];
      state.prompts = snap.prompts || [];
      state.checking = snap.checking || [];
      state.health = snap.health || "";
      state.routing = snap.routing || {};
      renderTargets();
      renderPrompts();
      renderSettings();
      renderModelFilter();
      updateOverview();
      if (isPageActive("results")) {
        loadResults();
      }
      scheduleActivePoll();
      if (state.health) toast("后台：" + state.health);
    }).catch(function (e) {
      toast("加载状态失败：" + e.message);
    });
  }

  function getPolicyHtml(t) {
    var globalMode = (state.config && state.config.schedule_mode) || "fixed";
    var mode = t.schedule_mode;
    var isInherit = !mode;
    var effMode = isInherit ? globalMode : mode;
    var policyBadgeClass = effMode === "staged" ? "policy-staged" : "policy-fixed";
    var policyLabel = effMode === "staged" ? "分阶段" : "固定间隔";
    var inheritHtml = isInherit ? "<span class='policy-sub'>继承全局</span>" : "";
    var customHtml = t.timing_policy ? "<span class='badge-custom' title='此模型使用独立的分阶段间隔'>单独配置</span>" : "";
    var responseHtml = t.response_rule_mode === "custom" ? "<span class='badge-custom' title='此模型使用自定义业务回复模型规则'>响应规则</span>" : "";
    return "<div class='policy-cell'><span class='policy-badge " + policyBadgeClass + "'>" + esc(policyLabel) + "</span>" + inheritHtml + customHtml + responseHtml + "</div>";
  }

  function getStageBadge(t, effMode) {
    var stage = t.schedule_stage;
    var streak = t.fail_streak || 1;
    var rechecks = t.isolation_rechecks || 0;

    if (stage === "recovery") {
      // The first recovery pass must keep production traffic blocked.
      return "<span class='stage-badge stage-recovery' title='首测通过保持阻断，按设定的恢复确认间隔复测，第 2 次连续通过才恢复'><i></i>恢复确认 1/2</span>";
    } else if (stage === "retry") {
      return "<span class='stage-badge stage-retry' title='快速复测原失败题中，满 3 轮才阻断隔离（当前第 " + esc(streak) + " 次）'><i></i>重试中(" + esc(streak) + "/3)</span>";
    } else if (stage === "isolated") {
      var isoTitle = rechecks > 0 ? ("再次失败延长隔离期（" + rechecks + "次续期）") : "首次隔离冷冻期，等待期满复测原失败题";
      var isoLabel = rechecks > 0 ? "已隔离(续期)" : "已隔离";
      return "<span class='stage-badge stage-isolated' title='" + esc(isoTitle) + "'><i></i>" + esc(isoLabel) + "</span>";
    } else if (stage === "unavailable") {
      return "<span class='stage-badge stage-unavailable' title='接口不可用，冷却中'><i></i>不可用冷冻</span>";
    } else if (stage === "normal") {
      return "<span class='stage-badge stage-normal' title='按正常周期等待检测'><i></i>常规周期</span>";
    }

    if (effMode === "staged") {
      return "<span class='stage-badge stage-normal' title='分阶段常规探测'><i></i>常规周期</span>";
    }
    return "<span class='stage-badge stage-fixed' title='固定间隔模式'>固定</span>";
  }

  function renderTargets() {
    var tbody = document.querySelector("#target-table tbody");
    if (!tbody) return;

    // Preserve open disclosures and keyboard position across background refreshes.
    var active = document.activeElement;
    var focusedRow = tbody.contains(active) ? active.closest("tr[data-model]") : null;
    var focusedModel = focusedRow ? focusedRow.dataset.model : "";
    var focusedAction = focusedRow ? active.dataset.action : "";
    var focusedDetails = focusedRow ? active.closest("details[data-kind]") : null;
    var focusedKind = focusedDetails ? focusedDetails.dataset.kind : "actions";
    var openDetails = new Set();
    tbody.querySelectorAll("tr[data-model] details[open]").forEach(function (detail) {
      openDetails.add(detail.closest("tr").dataset.model + "\n" + detail.dataset.kind);
    });
    var search = document.getElementById("target-search");
    var filter = document.getElementById("target-filter");
    var query = search ? search.value.trim().toLowerCase() : "";
    var filterValue = filter ? filter.value : "all";
    var visible = state.targets.filter(function (t) {
      if (query && (t.model + " " + (t.reason || "")).toLowerCase().indexOf(query) === -1) return false;
      if (filterValue === "blocked") return isBlocked(t);
      if (filterValue === "passed") return t.enabled && !isBlocked(t) && t.state === "passed" && t.schedule_stage !== "retry";
      if (filterValue === "attention") return t.state === "failed" || t.state === "unavailable" || t.schedule_stage === "retry" || t.schedule_stage === "recovery";
      if (filterValue === "paused") return !t.enabled;
      return true;
    });
    var count = document.getElementById("target-visible-count");
    var countText = "显示 " + visible.length + " / 共 " + state.targets.length + " 个";
    if (count && count.textContent !== countText) count.textContent = countText;

    tbody.innerHTML = visible.map(function (t) {
      var blocked = isBlocked(t);
      var isTempAllow = t.allow_until && new Date(t.allow_until) > new Date();

      var gateBadge = "";
      if (isTempAllow) {
        gateBadge = "<span class='gate-badge is-warn' title='临时放行至 " + esc(fmtTime(t.allow_until)) + "'><i></i>临时放行</span>";
      } else if (blocked) {
        var reason = t.manual_hold ? "手动禁用" : (t.reason || "探针未通过，已阻断");
        gateBadge = "<span class='gate-badge is-danger' title='" + esc(reason) + "'><i></i>已阻断</span>";
      } else {
        gateBadge = "<span class='gate-badge is-ok'><i></i>正常放行</span>";
      }

      var isChecking = (state.checking || []).indexOf(t.model) !== -1;
      var stateLabel = stateName[t.state] || t.state;
      var stateBadgeClass = "status-" + esc(t.state);
      if (t.schedule_stage === "recovery") {
        // A recovery confirmation has not released the model yet.
        stateLabel = "首次复测通过";
        stateBadgeClass = "status-recovery-pending";
      } else if (t.schedule_stage === "retry") {
        stateLabel = "答错，等待复测";
        stateBadgeClass = "status-recovery-pending";
      }
      var stateHtml = "";
      if (isChecking && t.state === "unknown") {
        stateHtml = "<span class='status-badge status-checking'><i></i>检测中</span>";
      } else {
        stateHtml = "<span class='status-badge " + stateBadgeClass + "'><i></i>" + esc(stateLabel) + "</span>";
        if (isChecking) {
          stateHtml += "<span class='status-badge status-checking'><i></i>检测中</span>";
        }
      }
      if (!t.enabled) {
        // Contract: "暂停检测不显示解封"
        stateHtml += "<span class='badge-paused' title='自动探针检测已暂停'>已暂停检测</span>";
      }

      var effMode = t.schedule_mode || (state.config && state.config.schedule_mode) || "fixed";
      var policyHtml = getPolicyHtml(t);
      var stageHtml = getStageBadge(t, effMode);

      // Action buttons: clear distinction between disabling model and pausing detection
      var ops = [];
      // 1. Gateway route control
      if (blocked || t.manual_hold) {
        ops.push("<button class='btn-action btn-enable' data-model='" + esc(t.model) + "' data-action='enable' title='解除手动禁用，恢复模型路由'>解除禁用</button>");
      } else {
        ops.push("<button class='btn-action btn-disable' data-model='" + esc(t.model) + "' data-action='disable' title='手动禁用该模型，阻断其网关路由'>禁用模型</button>");
      }
      if (blocked) {
        ops.push("<button class='btn-action btn-allow' data-model='" + esc(t.model) + "' data-action='allow' title='临时放行业务请求 15 分钟'>放行 15 分</button>");
      }


      // 3. Monitoring schedule control (monitor-off / monitor-on)
      if (t.enabled) {
        ops.push("<button class='btn-action btn-monitor-off' data-model='" + esc(t.model) + "' data-action='monitor-off' title='暂停该模型的自动定时检测（不会解封）'>暂停检测</button>");
      } else {
        ops.push("<button class='btn-action btn-monitor-on' data-model='" + esc(t.model) + "' data-action='monitor-on' title='恢复该模型的自动定时检测'>恢复检测</button>");
      }


      // 5. Delete target
      ops.push("<button class='btn-action danger-button' data-model='" + esc(t.model) + "' data-action='delete' title='删除该监测目标'>删除</button>");

      var slash = t.model.indexOf("/");
      var provider = slash < 0 ? "自定义模型" : t.model.slice(0, slash);
      var modelName = slash < 0 ? t.model : t.model.slice(slash + 1);
      var reason = t.reason || "";
      var reasonHtml = reason ? "<span class='reason-text'>" + esc(reason) + "</span>" : "<span class='cell-muted'>—</span>";
      if (reason.length > 50 || reason.indexOf("\n") !== -1) {
        reasonHtml = "<details class='reason-details' data-kind='reason'><summary aria-label='查看完整原因'><span class='reason-preview'>" + esc(reason) + "</span><span class='reason-toggle' aria-hidden='true'></span></summary><p>" + esc(reason) + "</p></details>";
      }
      var primaryActions = "<button class='btn-action primary-mini' data-model='" + esc(t.model) + "' data-action='recheck'>重测</button>" +
        "<button class='btn-action btn-policy' data-model='" + esc(t.model) + "' data-action='policy'>策略</button>";

      return "<tr data-model='" + esc(t.model) + "'>" +
        "<td class='target-model-cell' data-label='模型'><div class='model-identity'><strong title='" + esc(t.model) + "'>" + esc(modelName) + "</strong><span class='model-provider'>" + esc(provider) + "</span></div></td>" +
        "<td data-label='路由与检测'><div class='cell-stack'>" + gateBadge + stateHtml + "</div></td>" +
        "<td data-label='检测策略'><div class='cell-stack'>" + policyHtml + stageHtml + "</div></td>" +
        "<td class='target-time-cell' data-label='检测时间'><div class='time-next'><span>" + (t.enabled ? "下次" : "暂停 · 原计划") + "</span><time datetime='" + esc(t.next_check_at || "") + "'>" + esc(fmtTime(t.next_check_at)) + "</time></div><div class='time-previous'><span>上次</span><time datetime='" + esc(t.checked_at || "") + "'>" + esc(fmtTime(t.checked_at)) + "</time></div></td>" +
        "<td class='target-reason-cell' data-label='最近原因'>" + reasonHtml + "</td>" +
        "<td class='target-actions-cell' data-label='操作'><div class='row-primary-actions'>" + primaryActions + "</div><details class='row-menu' data-kind='actions'><summary>更多操作</summary><div class='row-menu-actions'>" + ops.join("") + "</div></details></td>" +
        "</tr>";
    }).join("");

    var emptyEl = document.getElementById("target-empty");
    if (emptyEl) {
      emptyEl.classList.toggle("hidden", visible.length > 0);
      emptyEl.querySelector("strong").textContent = state.targets.length ? "没有匹配的监测目标" : "还没有监测目标";
      emptyEl.querySelector(":scope > span").textContent = state.targets.length ? "调整搜索内容或状态筛选，再试一次。" : "展开上方「添加监测目标」，开始建立检测计划。";
    }
    var tableEl = document.getElementById("target-table");
    if (tableEl) tableEl.classList.toggle("hidden", visible.length === 0);

    tbody.querySelectorAll("tr[data-model] details").forEach(function (detail) {
      detail.open = openDetails.has(detail.closest("tr").dataset.model + "\n" + detail.dataset.kind);
    });

    if (focusedModel) {
      tbody.querySelectorAll("tr[data-model]").forEach(function (row) {
        if (row.dataset.model !== focusedModel) return;
        var focusTarget = Array.from(row.querySelectorAll("button[data-action]")).find(function (button) {
          return focusedAction && button.dataset.action === focusedAction;
        });
        if (!focusTarget) focusTarget = row.querySelector("details[data-kind='" + focusedKind + "'] > summary");
        if (focusTarget) focusTarget.focus({ preventScroll: true });
      });
    }

    tbody.querySelectorAll("button[data-action]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var model = btn.dataset.model, action = btn.dataset.action;
        if (action === "policy") {
          openTargetPolicyDialog(model);
          return;
        }
        if (action === "delete") {
          if (!confirm("删除对 " + model + " 的监测？门禁将不再拦截该模型。")) return;
          api("POST", "/api/targets/delete", { model: model }).then(function () {
            toast("已删除对 " + model + " 的监测");
            loadState();
          }).catch(function (e) { toast("操作失败：" + e.message); });
          return;
        }

        btn.disabled = true;
        api("POST", "/api/targets/action", { model: model, action: action }).then(function () {
          var actionMsgs = {
            enable: "已解除对 " + model + " 的手动禁用",
            disable: "已禁用模型 " + model + "，阻断路由",
            allow: "已临时放行 " + model + " 15 分钟",
            "monitor-off": "已暂停 " + model + " 的自动检测",
            "monitor-on": "已恢复 " + model + " 的自动检测",
            recheck: "已触发 " + model + " 重新检测",
            hold: "已暂停 " + model
          };
          toast(actionMsgs[action] || ("操作成功：" + action));
          loadState();
        }).catch(function (e) {
          toast("操作失败：" + e.message);
        }).then(function () {
          btn.disabled = false;
        });
      });
    });
  }

  var targetSearch = document.getElementById("target-search");
  if (targetSearch) targetSearch.addEventListener("input", renderTargets);
  var targetFilter = document.getElementById("target-filter");
  if (targetFilter) targetFilter.addEventListener("change", renderTargets);

  // ---- searchable model picker ----
  function renderModelPicker() {
    var optionsContainer = document.getElementById("model-options-list");
    var matchInfo = document.getElementById("model-match-info");
    var statusEl = document.getElementById("model-picker-status");
    if (!optionsContainer) return;

    var models = state.models || [];
    var q = modelSearchQuery.trim().toLowerCase();
    var filtered = models.filter(function (m) {
      return !q || (m.id && m.id.toLowerCase().indexOf(q) !== -1);
    });

    if (matchInfo) {
      matchInfo.textContent = q ? (filtered.length + " / " + models.length + " 个匹配") : (models.length + " 个可用模型");
    }
    if (statusEl) {
      statusEl.textContent = models.length ? ("已选 " + selectedModelIds.size + " / " + models.length + " 个") : "未加载模型";
    }

    if (!models.length) {
      optionsContainer.innerHTML = "<div class='dropdown-empty'>暂无模型，请点击「刷新模型列表」读取本机 Magpie 内部目录</div>";
    } else if (!filtered.length) {
      optionsContainer.innerHTML = "<div class='dropdown-empty'>未找到匹配「" + esc(q) + "」的模型</div>";
    } else {
      optionsContainer.innerHTML = filtered.map(function (m) {
        var isSelected = selectedModelIds.has(m.id);
        return "<div class='model-option-item " + (isSelected ? "is-selected" : "") + "' data-id='" + esc(m.id) + "'>" +
          "<input type='checkbox' class='model-checkbox' aria-label='选择 " + esc(m.id) + "' " + (isSelected ? "checked" : "") + ">" +
          "<span class='model-id-label' title='" + esc(m.id) + "'>" + esc(m.id) + "</span>" +
          "</div>";
      }).join("");

      optionsContainer.querySelectorAll(".model-option-item").forEach(function (item) {
        item.addEventListener("click", function (ev) {
          ev.stopPropagation();
          var keepFocus = item.contains(document.activeElement);
          var id = item.dataset.id;
          if (selectedModelIds.has(id)) {
            selectedModelIds.delete(id);
          } else {
            selectedModelIds.add(id);
          }
          renderModelPicker();
          if (keepFocus) {
            optionsContainer.querySelectorAll(".model-option-item").forEach(function (option) {
              if (option.dataset.id === id) option.querySelector("input").focus({ preventScroll: true });
            });
          }
          updateSelectedPreview();
        });
      });
    }

    updateSelectedPreview();
  }

  function updateSelectedPreview() {
    var preview = document.getElementById("selected-models-preview");
    var tagsList = document.getElementById("selected-tags-list");
    var countBadge = document.getElementById("selected-count-badge");
    if (!preview || !tagsList || !countBadge) return;

    var count = selectedModelIds.size;
    countBadge.textContent = "已选择 " + count + " 个模型";

    if (count === 0) {
      preview.classList.add("hidden");
      tagsList.innerHTML = "";
    } else {
      preview.classList.remove("hidden");
      var ids = Array.from(selectedModelIds);
      tagsList.innerHTML = ids.map(function (id) {
        return "<span class='model-tag' title='" + esc(id) + "'>" +
          "<span>" + esc(id) + "</span>" +
          "<button type='button' class='model-tag-del' data-id='" + esc(id) + "' aria-label='移除'>×</button>" +
          "</span>";
      }).join("");

      tagsList.querySelectorAll(".model-tag-del").forEach(function (delBtn) {
        delBtn.addEventListener("click", function (ev) {
          ev.stopPropagation();
          selectedModelIds.delete(delBtn.dataset.id);
          renderModelPicker();
        });
      });
    }
  }

  var searchInput = document.getElementById("model-search-input");
  var searchClear = document.getElementById("model-search-clear");
  var dropdownMenu = document.getElementById("model-dropdown");
  var dropdownToggle = document.getElementById("model-dropdown-btn");

  function openModelDropdown() {
    if (!dropdownMenu) return;
    isModelDropdownOpen = true;
    dropdownMenu.classList.remove("hidden");
    if (dropdownToggle) dropdownToggle.textContent = "▴";
  }

  function closeModelDropdown() {
    if (!dropdownMenu) return;
    isModelDropdownOpen = false;
    dropdownMenu.classList.add("hidden");
    if (dropdownToggle) dropdownToggle.textContent = "▾";
  }

  if (searchInput) {
    searchInput.addEventListener("focus", openModelDropdown);
    searchInput.addEventListener("input", function () {
      modelSearchQuery = searchInput.value;
      if (searchClear) searchClear.classList.toggle("hidden", !modelSearchQuery);
      openModelDropdown();
      renderModelPicker();
    });
    searchInput.addEventListener("keydown", function (e) {
      if (e.key === "Escape") closeModelDropdown();
    });
  }

  if (searchClear) {
    searchClear.addEventListener("click", function () {
      searchInput.value = "";
      modelSearchQuery = "";
      searchClear.classList.add("hidden");
      renderModelPicker();
      searchInput.focus();
    });
  }

  if (dropdownToggle) {
    dropdownToggle.addEventListener("click", function (ev) {
      ev.stopPropagation();
      if (isModelDropdownOpen) closeModelDropdown();
      else { openModelDropdown(); if (searchInput) searchInput.focus(); }
    });
  }

  var selectAllBtn = document.getElementById("btn-select-all");
  if (selectAllBtn) {
    selectAllBtn.addEventListener("click", function () {
      var q = modelSearchQuery.trim().toLowerCase();
      (state.models || []).forEach(function (m) {
        if (!q || (m.id && m.id.toLowerCase().indexOf(q) !== -1)) {
          selectedModelIds.add(m.id);
        }
      });
      renderModelPicker();
    });
  }

  var selectNoneBtn = document.getElementById("btn-select-none");
  if (selectNoneBtn) {
    selectNoneBtn.addEventListener("click", function () {
      selectedModelIds.clear();
      renderModelPicker();
    });
  }

  var clearTagsBtn = document.getElementById("btn-clear-selected-tags");
  if (clearTagsBtn) {
    clearTagsBtn.addEventListener("click", function () {
      selectedModelIds.clear();
      renderModelPicker();
    });
  }

  document.addEventListener("click", function (ev) {
    var combobox = document.getElementById("model-combobox");
    if (combobox && !combobox.contains(ev.target)) {
      closeModelDropdown();
    }
  });

  // Refresh the local Magpie catalog independently of probe gateway settings.
  var refreshModelsBtn = document.getElementById("refresh-models");
  if (refreshModelsBtn) {
    refreshModelsBtn.addEventListener("click", function () {
      var button = this;
      button.disabled = true;
      button.classList.add("is-loading");
      api("GET", "/api/models").then(function (models) {
        state.models = models || [];
        renderModelPicker();
        openModelDropdown();
        toast(state.models.length ? "已读取 " + state.models.length + " 个 Magpie 内部模型" : "Magpie 内部模型列表为空，请先在 Magpie 中配置模型");
      }).catch(function (e) {
        toast("获取模型失败：" + e.message);
      }).then(function () {
        button.disabled = false;
        button.classList.remove("is-loading");
      });
    });
  }

  var newCustomTpToggle = document.getElementById("new-enable-custom-tp");
  if (newCustomTpToggle) {
    newCustomTpToggle.addEventListener("change", function () {
      var wrap = document.getElementById("new-tp-fields");
      if (wrap) wrap.classList.toggle("hidden", !newCustomTpToggle.checked);
    });
  }

  // Add selected models to monitoring plan
  var addTargetBtn = document.getElementById("add-target");
  if (addTargetBtn) {
    addTargetBtn.addEventListener("click", function () {
      var selected = Array.from(selectedModelIds);
      if (!selected.length) {
        toast("请先选择要监测的模型");
        openModelDropdown();
        if (searchInput) searchInput.focus();
        return;
      }
      var interval = parseInt(document.getElementById("new-interval").value, 10) || 30;
      var failwait = parseInt(document.getElementById("new-failwait").value, 10) || 0;
      var scheduleMode = document.getElementById("new-schedule-mode").value;
      var isCustomTp = document.getElementById("new-enable-custom-tp").checked;
      var timingPolicy = null;
      if (isCustomTp) {
        try {
          timingPolicy = parseTimingPolicy(function (f) {
            return document.getElementById("new-tp-" + f.id).value;
          });
        } catch (err) {
          toast(err.message);
          return;
        }
      }

      var done = 0;
      var errors = [];
      addTargetBtn.disabled = true;

      Promise.all(selected.map(function (model) {
        return api("POST", "/api/targets", {
          model: model,
          enabled: true,
          interval_min: interval,
          fail_recheck_min: failwait,
          schedule_mode: scheduleMode,
          timing_policy: timingPolicy,
          prompt_ids: [],
          state: "unknown"
        }).then(function () {
          done++;
        }).catch(function (e) {
          errors.push(model + ": " + e.message);
        });
      })).then(function () {
        selectedModelIds.clear();
        renderModelPicker();
        closeModelDropdown();
        if (errors.length) {
          toast("已添加 " + done + " 个目标，失败 " + errors.length + " 个: " + errors[0]);
        } else {
          toast("已成功添加 " + done + " 个监测目标");
          var addPanel = document.getElementById("target-add-panel");
          if (addPanel) addPanel.open = false;
        }
        loadState();
      }).then(function () {
        addTargetBtn.disabled = false;
      });
    });
  }

  // ---- Target Policy Dialog ----
  var targetPolicyDialog = document.getElementById("target-policy-dialog");
  var targetPolicyForm = document.getElementById("target-policy-form");

  function openTargetPolicyDialog(model) {
    if (!targetPolicyDialog) return;
    lastFocusedElement = document.activeElement;

    var t = (state.targets || []).find(function (item) { return item.model === model; });
    if (!t) {
      toast("未找到目标：" + model);
      return;
    }

    document.getElementById("tpf-model").value = t.model;
    document.getElementById("tpf-model-name").textContent = t.model;

    var effMode = t.schedule_mode || (state.config && state.config.schedule_mode) || "fixed";
    var stageEl = document.getElementById("tpf-current-stage");
    if (stageEl) {
      stageEl.innerHTML = getStageBadge(t, effMode);
    }

    document.getElementById("tpf-schedule-mode").value = t.schedule_mode || "";
    document.getElementById("tpf-interval").value = t.interval_min || 30;
    document.getElementById("tpf-failwait").value = t.fail_recheck_min != null ? t.fail_recheck_min : 0;
    document.getElementById("tpf-recalculate-schedule").checked = false;

    var overrideToggle = document.getElementById("tpf-override-toggle");
    var fieldsWrap = document.getElementById("tpf-tp-fields");
    var hasCustom = !!t.timing_policy;
    if (overrideToggle) overrideToggle.checked = hasCustom;
    if (fieldsWrap) fieldsWrap.classList.toggle("hidden", !hasCustom);

    var tp = t.timing_policy || (state.config && state.config.timing_policy) || defaultTimingPolicy;
    document.getElementById("tpf-tp-normal").value = tp.normal_min != null ? tp.normal_min : 30;
    document.getElementById("tpf-tp-retry").value = tp.error_retry_min != null ? tp.error_retry_min : 2;
    document.getElementById("tpf-tp-iso-first").value = tp.isolation_first_min != null ? tp.isolation_first_min : 360;
    document.getElementById("tpf-tp-iso-later").value = tp.isolation_later_min != null ? tp.isolation_later_min : 1440;
    document.getElementById("tpf-tp-rec-confirm").value = tp.recovery_confirm_min != null ? tp.recovery_confirm_min : 2;
    document.getElementById("tpf-tp-unavail").value = tp.unavailable_min != null ? tp.unavailable_min : 30;

    document.getElementById("tpf-response-mode").value = t.response_rule_mode === "custom" ? "custom" : "inherit";
    renderResponseRules(document.getElementById("tpf-response-rules"), t.response_model_rules || []);
    if (typeof syncTargetResponseEditor === "function") syncTargetResponseEditor();

    if (typeof targetPolicyDialog.showModal === "function") {
      targetPolicyDialog.showModal();
    } else {
      targetPolicyDialog.classList.remove("hidden");
    }
  }

  function closeTargetPolicyDialog() {
    if (!targetPolicyDialog) return;
    if (typeof targetPolicyDialog.close === "function" && targetPolicyDialog.open) {
      targetPolicyDialog.close();
    } else {
      targetPolicyDialog.classList.add("hidden");
    }
    if (lastFocusedElement && typeof lastFocusedElement.focus === "function") {
      lastFocusedElement.focus();
    }
  }

  var tpfToggle = document.getElementById("tpf-override-toggle");
  if (tpfToggle) {
    tpfToggle.addEventListener("change", function () {
      var wrap = document.getElementById("tpf-tp-fields");
      if (wrap) wrap.classList.toggle("hidden", !tpfToggle.checked);
    });
  }

  var tpfPresetBtn = document.getElementById("tpf-btn-preset-codex");
  if (tpfPresetBtn) {
    tpfPresetBtn.addEventListener("click", function () {
      var tpfToggle = document.getElementById("tpf-override-toggle");
      if (tpfToggle) {
        tpfToggle.checked = true;
        var wrap = document.getElementById("tpf-tp-fields");
        if (wrap) wrap.classList.remove("hidden");
      }
      document.getElementById("tpf-tp-normal").value = 30;
      document.getElementById("tpf-tp-retry").value = 2;
      document.getElementById("tpf-tp-iso-first").value = 360;
      document.getElementById("tpf-tp-iso-later").value = 1440;
      document.getElementById("tpf-tp-rec-confirm").value = 2;
      document.getElementById("tpf-tp-unavail").value = 30;
      toast("已填入 codex-guard 推荐预设");
    });
  }

  var tpfResponseRules = document.getElementById("tpf-response-rules");
  bindResponseRuleEditor(tpfResponseRules, false);
  var tpfAddResponseRule = document.getElementById("tpf-add-response-rule");
  var tpfResponseMode = document.getElementById("tpf-response-mode");
  function syncTargetResponseEditor() {
    if (!tpfResponseMode) return;
    var custom = tpfResponseMode.value === "custom";
    if (tpfResponseRules) tpfResponseRules.style.display = custom ? "" : "none";
    if (tpfAddResponseRule) tpfAddResponseRule.style.display = custom ? "" : "none";
  }
  if (tpfAddResponseRule) {
    tpfAddResponseRule.addEventListener("click", function () {
      appendResponseRule(tpfResponseRules);
    });
  }
  if (tpfResponseMode) tpfResponseMode.addEventListener("change", syncTargetResponseEditor);
  syncTargetResponseEditor();

  var tpfCloseBtn = document.getElementById("tpf-close");
  if (tpfCloseBtn) tpfCloseBtn.addEventListener("click", closeTargetPolicyDialog);
  var tpfCancelBtn = document.getElementById("tpf-cancel");
  if (tpfCancelBtn) tpfCancelBtn.addEventListener("click", closeTargetPolicyDialog);

  if (targetPolicyDialog) {
    targetPolicyDialog.addEventListener("click", function (ev) {
      if (ev.target === targetPolicyDialog) closeTargetPolicyDialog();
    });
    targetPolicyDialog.addEventListener("cancel", function (ev) {
      ev.preventDefault();
      closeTargetPolicyDialog();
    });
  }

  if (targetPolicyForm) {
    targetPolicyForm.addEventListener("submit", function (ev) {
      ev.preventDefault();
      var model = document.getElementById("tpf-model").value;
      var t = (state.targets || []).find(function (item) { return item.model === model; });
      if (!t) { toast("目标不存在"); return; }

      var intervalMin = Number(document.getElementById("tpf-interval").value);
      if (!Number.isInteger(intervalMin) || intervalMin < 1 || intervalMin > 43200) {
        toast("常规间隔必须为 1 到 43200 之间的整数分钟");
        return;
      }
      var failRecheckMin = Number(document.getElementById("tpf-failwait").value);
      if (!Number.isInteger(failRecheckMin) || failRecheckMin < 0 || failRecheckMin > 43200) {
        toast("失败复检必须为 0 到 43200 之间的整数分钟");
        return;
      }

      var scheduleMode = document.getElementById("tpf-schedule-mode").value;
      var recalc = document.getElementById("tpf-recalculate-schedule").checked;

      var timingPolicy = null;
      var isOverride = document.getElementById("tpf-override-toggle").checked;
      if (isOverride) {
        try {
          timingPolicy = parseTimingPolicy(function (f) {
            return document.getElementById("tpf-tp-" + f.id).value;
          });
        } catch (err) {
          toast(err.message);
          return;
        }
      }

      var responseRuleMode = document.getElementById("tpf-response-mode").value;
      var responseRules = [];
      if (responseRuleMode === "custom") {
        try {
          responseRules = readResponseRules(document.getElementById("tpf-response-rules"));
        } catch (err) {
          toast(err.message);
          return;
        }
      }

      var payload = {
        model: t.model,
        enabled: t.enabled,
        prompt_ids: t.prompt_ids || [],
        interval_min: intervalMin,
        fail_recheck_min: failRecheckMin,
        schedule_mode: scheduleMode,
        timing_policy: timingPolicy,
        response_rule_mode: responseRuleMode,
        response_model_rules: responseRules,
        recalculate_schedule: recalc
      };

      var submitBtn = document.getElementById("tpf-submit");
      if (submitBtn) submitBtn.disabled = true;

      api("POST", "/api/targets", payload).then(function () {
        closeTargetPolicyDialog();
        toast("已更新 " + model + " 的调度策略");
        loadState();
      }).catch(function (e) {
        toast("保存策略失败：" + e.message);
      }).then(function () {
        if (submitBtn) submitBtn.disabled = false;
      });
    });
  }

  // ---- prompts & dialog ----
  var promptDialog = document.getElementById("prompt-dialog");
  var promptForm = document.getElementById("prompt-form");

  function updateGraderHelper() {
    var grader = document.getElementById("pf-grader").value;
    var expectInput = document.getElementById("pf-expect");
    var expectHint = document.getElementById("pf-expect-hint");
    if (!expectInput || !expectHint) return;

    if (grader === "none") {
      expectInput.placeholder = "人工评价无须期望规则（留空）";
      expectHint.textContent = "人工评价题目仅记录回答，不参与自动门禁阻断";
    } else if (grader === "number") {
      expectInput.placeholder = "例如：21";
      expectHint.textContent = "独立数字判分：回答中必须出现该数字（且两侧非紧邻数字）";
    } else if (grader === "contains") {
      expectInput.placeholder = "例如：关键字或子串";
      expectHint.textContent = "包含判分：回答中包含该关键字即视为通过";
    } else if (grader === "exact") {
      expectInput.placeholder = "例如：预期完整答案";
      expectHint.textContent = "精确匹配：去除首尾空白后与回答完全一致即通过";
    } else if (grader === "regex") {
      expectInput.placeholder = "例如：(?i)^(yes|true)$";
      expectHint.textContent = "正则匹配：回答匹配该正则表达式即视为通过";
    }
  }

  var pfGraderSelect = document.getElementById("pf-grader");
  if (pfGraderSelect) {
    pfGraderSelect.addEventListener("change", updateGraderHelper);
  }

  function openPromptDialog(title, eyebrow, promptData) {
    if (!promptDialog) return;
    lastFocusedElement = document.activeElement;

    document.getElementById("prompt-form-title").textContent = title;
    var eyebrowEl = document.getElementById("pf-eyebrow");
    if (eyebrowEl) eyebrowEl.textContent = eyebrow || "PROBE DEFINITION";

    promptForm.reset();
    document.getElementById("pf-id").value = promptData ? promptData.id : "";
    document.getElementById("pf-builtin").value = (promptData && promptData.builtin) ? "true" : "false";

    if (promptData) {
      document.getElementById("pf-name").value = promptData.name || "";
      document.getElementById("pf-text").value = promptData.text || "";
      document.getElementById("pf-grader").value = promptData.grader || "none";
      document.getElementById("pf-expect").value = promptData.expect || "";
      document.getElementById("pf-automatic").checked = !!promptData.automatic;
    } else {
      document.getElementById("pf-grader").value = "none";
      document.getElementById("pf-automatic").checked = false;
    }

    updateGraderHelper();

    if (typeof promptDialog.showModal === "function") {
      promptDialog.showModal();
    } else {
      promptDialog.classList.remove("hidden");
    }

    var nameInput = document.getElementById("pf-name");
    if (nameInput) nameInput.focus();
  }

  function closePromptDialog() {
    if (!promptDialog) return;
    if (typeof promptDialog.close === "function" && promptDialog.open) {
      promptDialog.close();
    } else {
      promptDialog.classList.add("hidden");
    }
    if (lastFocusedElement && typeof lastFocusedElement.focus === "function") {
      lastFocusedElement.focus();
    }
  }

  var newPromptBtn = document.getElementById("new-prompt");
  if (newPromptBtn) {
    newPromptBtn.addEventListener("click", function () {
      openPromptDialog("新建题目", "CUSTOM PROBE", null);
    });
  }

  var pfCloseBtn = document.getElementById("pf-close");
  if (pfCloseBtn) pfCloseBtn.addEventListener("click", closePromptDialog);
  var pfCancelBtn = document.getElementById("pf-cancel");
  if (pfCancelBtn) pfCancelBtn.addEventListener("click", closePromptDialog);

  if (promptDialog) {
    promptDialog.addEventListener("click", function (ev) {
      if (ev.target === promptDialog) closePromptDialog();
    });
    promptDialog.addEventListener("cancel", function (ev) {
      ev.preventDefault();
      closePromptDialog();
    });
  }

  if (promptForm) {
    promptForm.addEventListener("submit", function (ev) {
      ev.preventDefault();
      var id = document.getElementById("pf-id").value;
      var isBuiltin = document.getElementById("pf-builtin").value === "true";
      var name = document.getElementById("pf-name").value.trim();
      var text = document.getElementById("pf-text").value;
      var grader = document.getElementById("pf-grader").value;
      var expect = document.getElementById("pf-expect").value.trim();
      var automatic = document.getElementById("pf-automatic").checked;

      if (!name) { toast("题目名称不能为空"); return; }
      if (!text || !text.trim()) { toast("题目文本不能为空"); return; }
      if (grader !== "none" && !expect) {
        toast("自动判分题目需要填写判分期望 / 规则");
        document.getElementById("pf-expect").focus();
        return;
      }

      var payload = {
        name: name,
        text: text,
        grader: grader,
        expect: expect,
        automatic: automatic
      };
      if (id) {
        payload.id = id;
        if (isBuiltin) payload.builtin = true;
      }

      var submitBtn = document.getElementById("pf-submit");
      if (submitBtn) submitBtn.disabled = true;

      api("POST", "/api/prompts", payload).then(function () {
        closePromptDialog();
        toast(id ? "题目已更新" : "新题目已创建");
        loadState();
      }).catch(function (e) {
        toast("保存题目失败：" + e.message);
      }).then(function () {
        if (submitBtn) submitBtn.disabled = false;
      });
    });
  }

  function renderPrompts() {
    var tbody = document.querySelector("#prompt-table tbody");
    if (!tbody) return;

    // Preserve selected target models across re-renders
    var savedRunSelections = {};
    tbody.querySelectorAll("select.run-prompt").forEach(function (sel) {
      if (sel.dataset.prompt) savedRunSelections[sel.dataset.prompt] = sel.value;
    });

    var openPromptDetails = new Set();
    tbody.querySelectorAll("tr[data-prompt] details[open]").forEach(function (detail) {
      openPromptDetails.add(detail.closest("tr").dataset.prompt);
    });

    tbody.innerHTML = state.prompts.map(function (p) {
      var gLabel = graderName[p.grader] || p.grader;
      var gClass = "type-" + (p.grader || "none");

      var runSel = "";
      if (state.targets.length) {
        var savedVal = savedRunSelections[p.id] || "";
        var options = state.targets.map(function (t) {
          var selAttr = (t.model === savedVal) ? " selected" : "";
          return "<option value='" + esc(t.model) + "'" + selAttr + ">" + esc(t.model) + "</option>";
        }).join("");
        var isRunning = inFlightTests[p.id];
        runSel = "<div class='prompt-test-controls'><select class='run-prompt' aria-label='为 " + esc(p.name) + " 选择测试模型' data-prompt='" + esc(p.id) + "'>" + options + "</select>" +
          "<button class='run-btn primary-mini' data-prompt='" + esc(p.id) + "' " + (isRunning ? "disabled" : "") + ">" +
          (isRunning ? "测试中…" : "测试") + "</button></div>";
      }

      // Contract: all prompts can be edited with full fields; builtin only prohibits deletion
      var editBtn = "<button class='edit-prompt secondary-button-sm' data-id='" + esc(p.id) + "' title='编辑该题目文本、规则和门禁设置'>编辑</button>";
      var delBtn = p.builtin ? "" : "<button class='del-prompt danger-button-sm' data-id='" + esc(p.id) + "' title='删除该自定义题目'>删除</button>";

      var builtinBadge = p.builtin ? "<span class='builtin-badge'>内置</span>" : "";

      var expectHtml = p.expect ? "<code class='expected-answer'>" + esc(p.expect) + "</code>" : "<span class='cell-muted'>人工查看回答</span>";
      if ((p.expect || "").length > 70) {
        expectHtml = "<details class='reason-details'><summary aria-label='查看完整期望答案'><span class='reason-preview'><code>" + esc(p.expect) + "</code></span><span class='reason-toggle' aria-hidden='true'></span></summary><p><code>" + esc(p.expect) + "</code></p></details>";
      }
      return "<tr data-prompt='" + esc(p.id) + "'>" +
        "<td class='prompt-title-cell' data-label='题目'><div class='prompt-identity'><strong>" + esc(p.name) + "</strong><span><code>" + esc(p.id) + "</code>" + builtinBadge + "</span></div></td>" +
        "<td data-label='判分与期望答案'><div class='cell-stack'><span class='type-pill " + esc(gClass) + "'>" + esc(gLabel) + "</span>" + expectHtml + "</div></td>" +
        "<td data-label='检测方式'><span class='boolean-pill " + (p.automatic ? "yes" : "") + "'>" + (p.automatic ? "参与自动检测" : "仅手动测试") + "</span></td>" +
        "<td class='prompt-test-cell' data-label='测试模型'>" + (runSel || "<span class='cell-muted'>先添加监测目标</span>") + "</td>" +
        "<td class='prompt-manage-cell' data-label='管理'><div class='row-primary-actions'>" + editBtn + delBtn + "</div></td>" +
        "</tr>";
    }).join("");

    tbody.querySelectorAll("tr[data-prompt] details").forEach(function (detail) {
      detail.open = openPromptDetails.has(detail.closest("tr").dataset.prompt);
    });

    var emptyEl = document.getElementById("prompt-empty");
    if (emptyEl) emptyEl.classList.toggle("hidden", state.prompts.length > 0);
    var tableEl = document.getElementById("prompt-table");
    if (tableEl) tableEl.classList.toggle("hidden", state.prompts.length === 0);

    // Bind edit buttons
    tbody.querySelectorAll(".edit-prompt").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var id = btn.dataset.id;
        var p = (state.prompts || []).find(function (item) { return item.id === id; });
        if (!p) { toast("未找到题目：" + id); return; }
        openPromptDialog("编辑题目 (" + p.id + ")", p.builtin ? "BUILTIN PROBE" : "CUSTOM PROBE", p);
      });
    });

    // Bind delete buttons (builtin prompts have no delete button)
    tbody.querySelectorAll(".del-prompt").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var id = btn.dataset.id;
        if (!confirm("确定删除题目 " + id + "？")) return;
        api("POST", "/api/prompts/delete", { id: id }).then(function () {
          toast("题目已删除");
          loadState();
        }).catch(function (e) { toast("删除失败：" + e.message); });
      });
    });

    // Bind probe run buttons
    tbody.querySelectorAll(".run-btn").forEach(function (b) {
      b.addEventListener("click", function () {
        var promptId = b.dataset.prompt;
        var sel = tbody.querySelector("select.run-prompt[data-prompt='" + promptId + "']");
        if (!sel || !sel.value) { toast("请选择测试模型"); return; }
        var targetModel = sel.value;

        b.disabled = true;
        b.textContent = "测试中…";
        inFlightTests[promptId] = targetModel;
        toast("正在发送探针测试 " + targetModel + " …");
        scheduleActivePoll();

        api("POST", "/api/probe", { model: targetModel, prompt_id: promptId }).then(function (res) {
          var verdictStr = verdictName[res.verdict] || res.verdict || "未知";
          toast("测试结果：" + verdictStr + (res.reason ? (" (" + res.reason + ")") : ""));
        }).catch(function (e) {
          toast("测试请求失败：" + e.message);
        }).then(function () {
          delete inFlightTests[promptId];
          b.disabled = false;
          b.textContent = "测试";
          loadState();
        });
      });
    });
  }

  // ---- results ----
  function loadResults() {
    var modelEl = document.getElementById("result-model");
    var model = modelEl ? modelEl.value : "";
    var refreshBtn = document.getElementById("refresh-results");
    if (refreshBtn) refreshBtn.disabled = true;

    return api("GET", "/api/results?limit=100" + (model ? "&model=" + encodeURIComponent(model) : "")).then(function (rows) {
      var box = document.getElementById("result-list");
      if (!box) return;
      var openIds = new Set();
      box.querySelectorAll("details[open][data-result-id]").forEach(function (d) {
        openIds.add(d.dataset.resultId);
      });
      hasCheckingResults = (rows || []).some(function (r) { return r.verdict === "checking"; });
      box.innerHTML = (rows || []).map(function (r) {
        var isChecking = r.verdict === "checking";
        var openAttr = r.id && openIds.has(r.id) ? " open" : "";
        var detailAttr = " data-result-id='" + esc(r.id || "") + "'" + (isChecking ? " class='is-checking'" : "");
        var content = r.answer || ("(" + (r.detail || (isChecking ? "探针检测进行中，等待模型返回…" : "无回答内容")) + ")");
        return "<details" + detailAttr + openAttr + "><summary>" +
          "<span class='status-badge status-" + esc(r.verdict) + "'><i></i>" + esc(r.prompt_id === "response-model" ? (r.verdict === "failed" ? "规则禁用" : (String(r.detail || "").includes("改道") ? "规则改道" : "规则记录")) : (verdictName[r.verdict] || r.verdict)) + "</span>" +
          "<strong>" + esc(r.model) + "</strong>" +
          "<span class='result-meta'>" + esc(r.prompt_id === "response-model" ? "响应模型" : r.prompt_id) + (r.actual_model ? " · 实际模型 " + esc(r.actual_model) : "") + " · " + fmtTime(r.created_at) + " · " + esc(r.source) + "</span>" +
          "</summary><pre>" + esc(content) + "</pre></details>";
      }).join("");

      var emptyEl = document.getElementById("result-empty");
      if (emptyEl) emptyEl.classList.toggle("hidden", rows && rows.length > 0);
      box.classList.toggle("hidden", !rows || rows.length === 0);
      scheduleActivePoll();
    }).catch(function (e) {
      toast("获取检测记录失败：" + e.message);
    }).then(function () {
      if (refreshBtn) refreshBtn.disabled = false;
    });
  }

  var refreshResultsBtn = document.getElementById("refresh-results");
  if (refreshResultsBtn) refreshResultsBtn.addEventListener("click", loadResults);
  var resultModelSel = document.getElementById("result-model");
  if (resultModelSel) resultModelSel.addEventListener("change", loadResults);

  // ---- supplier protection ----
  var supplierProtocolNames = {
    chat: "Chat",
    responses: "Responses",
    anthropic: "Anthropic",
    gemini: "Gemini",
    decide: "Decide"
  };

  function supplierEl(tag, className, text) {
    var el = document.createElement(tag);
    if (className) el.className = className;
    el.textContent = text == null ? "" : String(text);
    return el;
  }

  function supplierProtocolLabel(protocol) {
    return Object.prototype.hasOwnProperty.call(supplierProtocolNames, protocol)
      ? supplierProtocolNames[protocol]
      : String(protocol);
  }

  function supplierNameFor(id) {
    var providers = (supplierData && supplierData.providers) || [];
    for (var i = 0; i < providers.length; i++) {
      if (providers[i] && String(providers[i].id) === String(id)) {
        return providers[i].name || providers[i].id;
      }
    }
    return id;
  }

  function setSupplierMessage(text, kind) {
    var el = document.getElementById("supplier-msg");
    if (!el) return;
    el.textContent = text || "";
    el.className = "form-message" + (kind === "error" ? " is-error" : kind === "success" ? " is-success" : kind === "warning" ? " is-warning" : "");
  }

  function applySupplierData(data) {
    data = data || {};
    var providers = [];
    if (Array.isArray(data.providers)) {
      providers = data.providers.filter(function (p) { return p && typeof p === "object"; }).map(function (p) {
        return {
          id: p.id == null ? "" : String(p.id),
          name: p.name == null ? "" : String(p.name),
          protected: !!p.protected,
          protocols: Array.isArray(p.protocols) ? p.protocols.map(String) : [],
          conflict: p.conflict == null ? "" : String(p.conflict),
          unsupported: p.unsupported == null ? "" : String(p.unsupported)
        };
      });
    }
    supplierData = {
      configured: data.configured !== false,
      providers: providers,
      error: data.error == null ? "" : String(data.error)
    };
    var msgEl = document.getElementById("supplier-msg");
    if (msgEl && msgEl.textContent === "正在刷新供应商状态…") setSupplierMessage("", "");
    renderSupplierPanel();
  }

  function loadSuppliers(force) {
    if (supplierLoadPromise) {
      if (force) supplierReloadQueued = true;
      return supplierLoadPromise;
    }
    var seq = ++supplierLoadSeq;
    if (!supplierData) renderSupplierPanel();
    supplierLoadPromise = api("GET", "/api/suppliers").then(function (data) {
      if (seq !== supplierLoadSeq) return;
      applySupplierData(data);
    }).catch(function (err) {
      if (seq !== supplierLoadSeq) return;
      supplierData = supplierData || { configured: true, providers: [], error: "" };
      supplierData.error = err.message || "无法读取供应商状态";
      renderSupplierPanel();
      setSupplierMessage("读取供应商状态失败：" + supplierData.error, "error");
    }).then(function () {
      supplierLoadPromise = null;
      if (supplierReloadQueued) {
        supplierReloadQueued = false;
        loadSuppliers();
      }
    });
    return supplierLoadPromise;
  }

  function renderSupplierPanel() {
    var listEl = document.getElementById("supplier-list");
    if (!listEl) return;
    var summaryEl = document.getElementById("supplier-summary");
    var focusedSupplier = null;
    var activeEl = document.activeElement;
    if (activeEl && activeEl.classList && activeEl.classList.contains("supplier-action") && listEl.contains(activeEl) && activeEl.closest) {
      var focusedRow = activeEl.closest(".supplier-row");
      if (focusedRow) focusedSupplier = focusedRow.getAttribute("data-supplier-id");
    }
    listEl.textContent = "";

    if (!supplierData) {
      if (summaryEl) summaryEl.textContent = "读取中…";
      listEl.appendChild(supplierEl("p", "supplier-empty", "正在读取供应商保护状态…"));
      return;
    }

    var providers = supplierData.providers || [];
    var supported = providers.filter(function (p) { return !p.unsupported; });
    var unsupported = providers.filter(function (p) { return !!p.unsupported; });
    var protectedCount = supported.filter(function (p) { return p.protected; }).length;
    if (summaryEl) {
      if (supplierData.error && !providers.length) summaryEl.textContent = "状态不可用";
      else if (!supplierData.configured) summaryEl.textContent = "未找到供应商配置";
      else if (!providers.length) summaryEl.textContent = "未发现自定义供应商";
      else {
        var summary = supported.length ? protectedCount + " / " + supported.length + " 已保护" : "没有可接管的自定义供应商";
        if (unsupported.length) summary += " · " + unsupported.length + " 个不支持接管";
        summaryEl.textContent = summary;
      }
    }

    if (supplierData.error) {
      listEl.appendChild(supplierEl("p", "supplier-empty is-error", "供应商状态：" + supplierData.error));
    }

    if (supported.length) {
      supported.forEach(function (p) {
        listEl.appendChild(renderSupplierRow(p));
      });
      if (focusedSupplier != null) {
        Array.prototype.forEach.call(listEl.querySelectorAll(".supplier-row"), function (row) {
          if (row.getAttribute("data-supplier-id") === focusedSupplier) {
            var action = row.querySelector(".supplier-action");
            if (action && !action.disabled && document.activeElement !== action) action.focus();
          }
        });
      }
    }
    if (unsupported.length) {
      listEl.appendChild(supplierEl("p", "supplier-group-title", "不支持接管的供应商"));
      unsupported.forEach(function (p) {
        listEl.appendChild(renderSupplierRow(p));
      });
    }
    if (providers.length) return;

    if (!supplierData.configured) {
      if (!supplierData.error) {
        listEl.appendChild(supplierEl("p", "supplier-empty", "未找到可保护的自定义供应商配置（providers.json）。请先在 Magpie 中创建自定义供应商，再点击下方刷新。"));
      }
      return;
    }

    if (!supplierData.error) {
      listEl.appendChild(supplierEl("p", "supplier-empty", "暂无显式配置的自定义供应商。请在 Magpie 原生供应商编辑器中添加后点击下方刷新。"));
    }
  }

  function renderSupplierRow(p) {
    var row = document.createElement("div");
    row.className = "supplier-row";
    row.setAttribute("data-supplier-id", p.id);

    var main = document.createElement("div");
    main.className = "supplier-main";

    var title = document.createElement("div");
    title.className = "supplier-title";
    title.appendChild(supplierEl("strong", "", p.name || p.id || "未命名供应商"));
    if (p.unsupported) {
      title.appendChild(supplierEl("span", "supplier-badge is-muted", "不支持接管"));
    } else {
      title.appendChild(supplierEl("span", "supplier-badge " + (p.protected ? "is-on" : "is-off"), p.protected ? "已保护" : "未保护"));
      if (p.conflict) title.appendChild(supplierEl("span", "supplier-badge is-warn", "冲突"));
    }
    main.appendChild(title);

    if (p.id) main.appendChild(supplierEl("div", "supplier-id", p.id));

    var protoWrap = document.createElement("div");
    protoWrap.className = "supplier-protocols";
    if (p.protocols.length) {
      p.protocols.forEach(function (protocol) {
        protoWrap.appendChild(supplierEl("span", "supplier-proto", supplierProtocolLabel(protocol)));
      });
    } else {
      protoWrap.appendChild(supplierEl("span", "supplier-proto is-empty", "未配置协议地址"));
    }
    main.appendChild(protoWrap);

    if (p.unsupported) {
      main.appendChild(supplierEl("p", "supplier-unsupported", p.unsupported));
    } else if (p.conflict) {
      main.appendChild(supplierEl("p", "supplier-conflict", "⚠ 冲突：" + p.conflict));
    }
    row.appendChild(main);

    var pending = Object.prototype.hasOwnProperty.call(supplierPending, p.id);
    var anyPending = Object.keys(supplierPending).length > 0;
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "secondary-button-sm supplier-action";
    if (p.unsupported) {
      btn.textContent = "不支持接管";
      btn.setAttribute("aria-label", "不支持接管：" + (p.name || p.id));
    } else if (pending) {
      btn.textContent = supplierPending[p.id] ? "正在启用…" : "正在恢复…";
      btn.setAttribute("aria-busy", "true");
    } else {
      btn.textContent = p.protected ? "恢复原始地址" : "启用保护";
      btn.setAttribute("aria-label", (p.protected ? "恢复原始地址：" : "启用保护：") + (p.name || p.id));
      btn.addEventListener("click", function () {
        setSupplierProtection(p.id, !p.protected);
      });
    }
    btn.disabled = !!p.unsupported || anyPending || !p.id;
    row.appendChild(btn);
    return row;
  }

  function focusSupplierAction(id) {
    var active = document.activeElement;
    if (active && active !== document.body && active !== document.documentElement) return;
    var listEl = document.getElementById("supplier-list");
    if (!listEl) return;
    var rows = listEl.querySelectorAll(".supplier-row");
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].getAttribute("data-supplier-id") === String(id)) {
        var btn = rows[i].querySelector(".supplier-action");
        if (btn && !btn.disabled) btn.focus();
        return;
      }
    }
  }

  function setSupplierProtection(id, enabled) {
    if (!id || Object.prototype.hasOwnProperty.call(supplierPending, id)) return;
    var actionText = enabled ? "启用保护" : "恢复原始地址";
    supplierPending[id] = !!enabled;
    renderSupplierPanel();
    setSupplierMessage("正在" + actionText + "…", "");
    var refreshBtn = document.getElementById("supplier-refresh");
    if (refreshBtn) refreshBtn.disabled = true;

    api("POST", "/api/suppliers/protection", { provider_id: id, enabled: !!enabled }).then(function (data) {
      delete supplierPending[id];
      supplierLoadSeq++;
      if (data && Array.isArray(data.providers)) {
        applySupplierData(data);
      } else {
        renderSupplierPanel();
        loadSuppliers(true);
      }
      var name = supplierNameFor(id);
      if (data && data.error) {
        setSupplierMessage("操作已提交，但供应商状态报告：" + data.error, "warning");
      } else {
        setSupplierMessage("✓ 已" + actionText + "：" + name, "success");
        toast("已" + actionText + "：" + name);
      }
      focusSupplierAction(id);
      loadState();
    }).catch(function (err) {
      delete supplierPending[id];
      renderSupplierPanel();
      setSupplierMessage(actionText + "失败：" + err.message, "error");
      toast(actionText + "失败：" + err.message);
      focusSupplierAction(id);
    }).then(function () {
      var btn = document.getElementById("supplier-refresh");
      if (btn && !Object.keys(supplierPending).length) btn.disabled = false;
    });
  }

  var supplierRefreshBtn = document.getElementById("supplier-refresh");
  if (supplierRefreshBtn) {
    supplierRefreshBtn.addEventListener("click", function () {
      if (supplierLoadPromise) return;
      supplierRefreshBtn.disabled = true;
      setSupplierMessage("正在刷新供应商状态…", "");
      loadSuppliers(true).then(function () {
        if (!Object.keys(supplierPending).length) supplierRefreshBtn.disabled = false;
      });
    });
  }

  // ---- settings & real verification ----
  function markConfigDirty() {
    configDirty = true;
    var msgEl = document.getElementById("config-msg");
    if (msgEl && !msgEl.textContent) {
      msgEl.textContent = "有未保存的配置更改";
      msgEl.className = "form-message";
    }
  }

  // Protect uncommitted drafts from being overwritten by 15s polling
  [
    "cfg-url", "cfg-key", "cfg-limit", "cfg-timeout", "cfg-cool",
    "cfg-schedule-mode", "cfg-recalculate-schedule",
    "cfg-tp-normal", "cfg-tp-retry", "cfg-tp-iso-first", "cfg-tp-iso-later", "cfg-tp-rec-confirm", "cfg-tp-unavail",
    "cfg-response-mode", "cfg-response-reroute",
    "cfg-provider-attempts"
  ].forEach(function (id) {
    var el = document.getElementById(id);
    if (el) {
      var evt = (el.type === "checkbox" || el.tagName === "SELECT") ? "change" : "input";
      el.addEventListener(evt, markConfigDirty);
    }
  });

  var cfgResponseRules = document.getElementById("cfg-response-rules");
  bindResponseRuleEditor(cfgResponseRules);
  var cfgAddResponseRule = document.getElementById("cfg-add-response-rule");
  if (cfgAddResponseRule) {
    cfgAddResponseRule.addEventListener("click", function () {
      appendResponseRule(cfgResponseRules);
      markConfigDirty();
    });
  }

  var cfgPresetBtn = document.getElementById("cfg-btn-preset-codex");
  if (cfgPresetBtn) {
    cfgPresetBtn.addEventListener("click", function () {
      var modeEl = document.getElementById("cfg-schedule-mode");
      if (modeEl) modeEl.value = "staged";
      document.getElementById("cfg-tp-normal").value = 30;
      document.getElementById("cfg-tp-retry").value = 2;
      document.getElementById("cfg-tp-iso-first").value = 360;
      document.getElementById("cfg-tp-iso-later").value = 1440;
      document.getElementById("cfg-tp-rec-confirm").value = 2;
      document.getElementById("cfg-tp-unavail").value = 30;
      configDirty = true;
      var msgEl = document.getElementById("config-msg");
      if (msgEl) {
        msgEl.textContent = "已填入 codex-guard 阶梯推荐预设（3 错 2 过与 6 项间隔），请点击下方保存";
        msgEl.className = "form-message is-warning";
      }
      toast("已填入 codex-guard 推荐预设参数");
    });
  }

  function renderSettings() {
    var c = state.config || {};
    // If user has modified the form or is currently focused on an input, do not overwrite drafts
    if (configDirty) return;
    if (document.activeElement && document.activeElement.closest && document.activeElement.closest("#page-settings")) return;

    var urlEl = document.getElementById("cfg-url");
    if (urlEl) urlEl.value = c.gateway_url || "";
    var keyEl = document.getElementById("cfg-key");
    if (keyEl) {
      keyEl.value = c.gateway_key || "";
      keyEl.placeholder = c.gateway_url ? "已保存（留空保持不变）" : "magpie";
    }
    var limitEl = document.getElementById("cfg-limit");
    if (limitEl) limitEl.value = c.daily_limit || 120;
    var timeoutEl = document.getElementById("cfg-timeout");
    if (timeoutEl) timeoutEl.value = c.timeout_sec || 180;
    var coolEl = document.getElementById("cfg-cool");
    if (coolEl) coolEl.value = c.cool_min || 15;

    var modeEl = document.getElementById("cfg-schedule-mode");
    if (modeEl) modeEl.value = c.schedule_mode || "fixed";

    var responseModeEl = document.getElementById("cfg-response-mode");
    if (responseModeEl) responseModeEl.value = c.response_model_mode || "off";
    var responseRerouteEl = document.getElementById("cfg-response-reroute");
    if (responseRerouteEl) responseRerouteEl.value = c.response_reroute_min || 30;
    renderResponseRules(document.getElementById("cfg-response-rules"), c.response_rules || []);

    var providerAttemptsEl = document.getElementById("cfg-provider-attempts");
    if (providerAttemptsEl) providerAttemptsEl.value = c.provider_attempts || 2;

    var tp = c.timing_policy || defaultTimingPolicy;
    var tpNormal = document.getElementById("cfg-tp-normal");
    if (tpNormal) tpNormal.value = tp.normal_min != null ? tp.normal_min : 30;
    var tpRetry = document.getElementById("cfg-tp-retry");
    if (tpRetry) tpRetry.value = tp.error_retry_min != null ? tp.error_retry_min : 2;
    var tpIsoFirst = document.getElementById("cfg-tp-iso-first");
    if (tpIsoFirst) tpIsoFirst.value = tp.isolation_first_min != null ? tp.isolation_first_min : 360;
    var tpIsoLater = document.getElementById("cfg-tp-iso-later");
    if (tpIsoLater) tpIsoLater.value = tp.isolation_later_min != null ? tp.isolation_later_min : 1440;
    var tpRecConfirm = document.getElementById("cfg-tp-rec-confirm");
    if (tpRecConfirm) tpRecConfirm.value = tp.recovery_confirm_min != null ? tp.recovery_confirm_min : 2;
    var tpUnavail = document.getElementById("cfg-tp-unavail");
    if (tpUnavail) tpUnavail.value = tp.unavailable_min != null ? tp.unavailable_min : 30;

    var recalcEl = document.getElementById("cfg-recalculate-schedule");
    if (recalcEl) recalcEl.checked = false;
  }

  var saveConfigBtn = document.getElementById("save-config");
  if (saveConfigBtn) {
    saveConfigBtn.addEventListener("click", function () {
      var msgEl = document.getElementById("config-msg");

      var timingPolicy;
      try {
        timingPolicy = parseTimingPolicy(function (f) {
          return document.getElementById("cfg-tp-" + f.id).value;
        });
      } catch (err) {
        if (msgEl) {
          msgEl.textContent = err.message;
          msgEl.className = "form-message is-error";
        }
        toast(err.message);
        return;
      }

      var recalcEl = document.getElementById("cfg-recalculate-schedule");
      var recalc = recalcEl ? recalcEl.checked : false;

      var responseRules;
      try {
        responseRules = readResponseRules(document.getElementById("cfg-response-rules"));
      } catch (err) {
        if (msgEl) {
          msgEl.textContent = err.message;
          msgEl.className = "form-message is-error";
        }
        toast(err.message);
        return;
      }

      // Preserve all other config fields from state.config
      var existingCfg = state.config || {};
      var c = Object.assign({}, existingCfg, {
        gateway_url: document.getElementById("cfg-url").value.trim(),
        gateway_key: document.getElementById("cfg-key").value,
        daily_limit: parseInt(document.getElementById("cfg-limit").value, 10),
        timeout_sec: parseInt(document.getElementById("cfg-timeout").value, 10),
        cool_min: parseInt(document.getElementById("cfg-cool").value, 10),
        schedule_mode: document.getElementById("cfg-schedule-mode").value,
        timing_policy: timingPolicy,
        response_model_mode: document.getElementById("cfg-response-mode").value,
        response_reroute_min: parseInt(document.getElementById("cfg-response-reroute").value, 10),
        response_rules: responseRules,
        provider_attempts: parseInt(document.getElementById("cfg-provider-attempts").value, 10),
        recalculate_schedule: recalc
      });

      saveConfigBtn.disabled = true;
      if (msgEl) {
        msgEl.textContent = "正在保存配置…";
        msgEl.className = "form-message";
      }

      api("POST", "/api/config", c).then(function () {
        configDirty = false;
        if (recalcEl) recalcEl.checked = false;
        if (msgEl) {
          msgEl.textContent = "✓ 配置已保存。请在监测目标中点击「重测」验证检测连接。";
          msgEl.className = "form-message is-success";
        }
        toast("配置已保存");
        loadState();
      }).catch(function (saveErr) {
        if (msgEl) {
          msgEl.textContent = "保存配置失败：" + saveErr.message;
          msgEl.className = "form-message is-error";
        }
        toast("保存配置失败：" + saveErr.message);
      }).then(function () {
        saveConfigBtn.disabled = false;
      });
    });
  }

  var topRefreshBtn = document.getElementById("top-refresh");
  if (topRefreshBtn) {
    topRefreshBtn.addEventListener("click", function () {
      toast("正在刷新数据…");
      loadState();
    });
  }

  function denyAccess() {
    document.body.innerHTML = "<div class='no-token'><h2>未授权访问</h2><p>请从 Magpie 插件入口重新打开控制台以获得访问令牌。</p></div>";
  }

  if (!token) {
    // Cookie session from /open/<ticket>: probe once, deny only on 401.
    api("GET", "/api/state").then(function () {
      loadState().then(function () {
        api("GET", "/api/models").then(function (models) {
          state.models = models || [];
          renderModelPicker();
        }).catch(function () {});
      });
      setInterval(loadState, 15000);
    }).catch(function (err) {
      if (err.message === "unauthorized") denyAccess();
    });
    return;
  }

  // Initial load
  loadState().then(function () {
    // Local model discovery does not require a configured probe gateway.
    api("GET", "/api/models").then(function (models) {
      state.models = models || [];
      renderModelPicker();
    }).catch(function () {
      var statusEl = document.getElementById("model-picker-status");
      if (statusEl) statusEl.textContent = "内部列表未加载，请点击刷新";
    });
  });

  // Safe polling: does not wipe uncommitted settings, does not interrupt open dialog or running tests
  setInterval(loadState, 15000);
})();
