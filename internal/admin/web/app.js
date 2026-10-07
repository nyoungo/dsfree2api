/* dsfree2api web console */
(function () {
"use strict";

const $ = (s, r) => (r || document).querySelector(s);
const $$ = (s, r) => Array.from((r || document).querySelectorAll(s));
const view = $("#view");
const TITLES = {
  dashboard: "仪表盘", models: "模型与站点", keys: "API Key", proxy: "代理线路",
  turnstile: "Turnstile", playground: "对话调试", logs: "实时日志", settings: "设置"
};

let overviewTimer = null;
let logStop = null;
let lastOverview = null;

/* ── helpers ─────────────────────────────────────────────── */
function toast(msg, kind) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = "toast " + (kind || "");
  clearTimeout(t._h);
  t._h = setTimeout(() => t.classList.add("hidden"), 3200);
}

async function api(path, opts) {
  opts = opts || {};
  const init = { method: opts.method || (opts.body !== undefined ? "POST" : "GET"), headers: {} };
  if (opts.signal) init.signal = opts.signal;
  if (opts.body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = typeof opts.body === "string" ? opts.body : JSON.stringify(opts.body);
  }
  const res = await fetch(path, init);
  let data = null;
  try { data = await res.json(); } catch (e) { data = null; }
  if (res.status === 401 && !path.endsWith("/login")) {
    showLogin();
    throw new Error("unauthorized");
  }
  if (!res.ok) {
    const msg = (data && (data.error || data.detail)) || res.statusText;
    throw new Error(typeof msg === "string" ? msg : JSON.stringify(msg));
  }
  return data;
}

function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, c =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
const fmtInt = n => (n || 0).toLocaleString("en-US");
const fmtPct = (a, b) => b ? (a * 100 / b).toFixed(1) + "%" : "0%";
const fmtMs = ms => ms >= 1000 ? (ms / 1000).toFixed(2) + "s" : Math.round(ms) + "ms";
const fmtBytes = n => n > 1048576 ? (n / 1048576).toFixed(1) + "MB" : n > 1024 ? (n / 1024).toFixed(1) + "KB" : n + "B";
const timeStr = t => new Date(t).toLocaleTimeString("zh-CN", { hour12: false });

function switchHTML(id, checked) {
  return `<label class="switch"><input type="checkbox" id="${id}" ${checked ? "checked" : ""}><span class="slider"></span></label>`;
}

/* ── auth ────────────────────────────────────────────────── */
async function ensureSession() {
  try {
    const s = await api("/api/session");
    if (s.authenticated) { $("#login").classList.add("hidden"); return true; }
  } catch (e) { /* ignore */ }
  showLogin();
  return false;
}
function showLogin() {
  $("#login").classList.remove("hidden");
  setTimeout(() => $("#login-password").focus(), 50);
}
async function doLogin() {
  const pw = $("#login-password").value;
  if (!pw) return;
  try {
    await api("/api/login", { method: "POST", body: { password: pw } });
    $("#login-error").textContent = "";
    $("#login").classList.add("hidden");
    $("#login-password").value = "";
    route();
  } catch (e) {
    $("#login-error").textContent = e.message === "invalid password" ? "密码错误" : e.message;
  }
}

/* ── router ──────────────────────────────────────────────── */
function route() {
  if (overviewTimer) { clearInterval(overviewTimer); overviewTimer = null; }
  if (logStop) { logStop(); logStop = null; }
  const hash = location.hash.replace(/^#\/?/, "") || "dashboard";
  const page = (hash.split("?")[0] || "dashboard");
  $$("#nav a").forEach(a => a.classList.toggle("active", a.dataset.page === page));
  $("#page-title").textContent = TITLES[page] || page;
  view.innerHTML = '<div class="empty">加载中…</div>';
  const fn = PAGES[page] || PAGES.dashboard;
  fn().catch(e => { view.innerHTML = `<div class="empty">加载失败：${esc(e.message)}</div>`; });
}

/* ── overview poll ───────────────────────────────────────── */
async function fetchOverview() {
  const ov = await api("/api/overview");
  lastOverview = ov;
  renderBadges(ov);
  return ov;
}
function renderBadges(ov) {
  const st = (ov.turnstile.status || []).filter(s => s.valid).length;
  const total = (ov.sites || []).filter(s => s.enabled).length;
  $("#sys-version").textContent = "v" + (ov.system.version || "?");
  $("#sys-badges").innerHTML = `
    <span class="badge ok">运行 ${Math.floor(ov.system.uptime_s / 3600)}h${Math.floor(ov.system.uptime_s % 3600 / 60)}m</span>
    <span class="badge">${esc(ov.system.go)} · ${esc(ov.system.os)}/${esc(ov.system.arch)}</span>
    <span class="badge ${st ? "ok" : "warn"}">Turnstile ${st}/${total}</span>
    <span class="badge">在线 ${fmtInt(ov.metrics.in_flight)}</span>`;
}

/* ── pages ───────────────────────────────────────────────── */
const PAGES = {};

PAGES.dashboard = async () => {
  view.innerHTML = `
    <div class="grid g4">
      <div class="card"><div class="stat-label">总请求</div><div class="stat-value" id="s-req">–</div><div class="stat-sub" id="s-rps"></div></div>
      <div class="card"><div class="stat-label">错误 / 空响应</div><div class="stat-value" id="s-err">–</div><div class="stat-sub" id="s-errpct"></div></div>
      <div class="card"><div class="stat-label">Token（估算）</div><div class="stat-value" id="s-tok">–</div><div class="stat-sub" id="s-toksub"></div></div>
      <div class="card"><div class="stat-label">延迟 P50 / P95</div><div class="stat-value" id="s-lat">–</div><div class="stat-sub" id="s-latsub"></div></div>
    </div>
    <div class="grid g2">
      <div class="card"><h3>模型用量</h3><div class="table-wrap"><table>
        <thead><tr><th>模型</th><th class="num">请求</th><th class="num">错误</th><th class="num">Token</th><th class="num">平均耗时</th></tr></thead>
        <tbody id="tb-models"><tr><td colspan="5" class="empty">暂无数据</td></tr></tbody>
      </table></div></div>
      <div class="card"><h3>站点状态</h3><div id="site-cards" class="grid" style="gap:10px"></div></div>
    </div>
    <div class="card"><div class="toolbar" style="margin-bottom:10px"><h3 style="margin:0">最近请求</h3>
      <div><button class="btn small" id="btn-invalidate">刷新会话缓存</button>
      <button class="btn small danger" id="btn-reset">清空统计</button></div></div>
      <div class="table-wrap"><table>
        <thead><tr><th>时间</th><th>模型</th><th>站点</th><th>线路</th><th>类型</th><th class="num">耗时</th><th class="num">Token</th><th>状态</th></tr></thead>
        <tbody id="tb-recent"><tr><td colspan="8" class="empty">暂无请求</td></tr></tbody>
      </table></div>
    </div>`;

  $("#btn-invalidate").onclick = async () => { await api("/api/actions", { body: { action: "invalidate" } }); toast("已清除 nonce 与 cookie 缓存", "ok"); };
  $("#btn-reset").onclick = async () => { await api("/api/actions", { body: { action: "reset_metrics" } }); toast("统计已清空", "ok"); refresh(); };

  async function refresh() {
    let ov;
    try { ov = await fetchOverview(); } catch (e) { return; }
    const m = ov.metrics;
    $("#s-req").textContent = fmtInt(m.requests);
    $("#s-rps").textContent = `${m.rps.toFixed(2)} req/s · 流式 ${fmtInt(m.streams)} · 运行中 ${fmtInt(m.in_flight)}`;
    $("#s-err").textContent = fmtInt(m.errors);
    $("#s-err").className = "stat-value " + (m.errors ? "err" : "ok");
    $("#s-errpct").textContent = `错误率 ${fmtPct(m.errors, m.requests)} · 空响应 ${fmtInt(m.empty)} · 拒绝 ${fmtInt(m.unauthorized)} · 限流 ${fmtInt(m.rate_limited)}`;
    $("#s-tok").textContent = fmtInt(m.prompt_tokens + m.completion_tokens);
    $("#s-toksub").textContent = `输入 ${fmtInt(m.prompt_tokens)} · 输出 ${fmtInt(m.completion_tokens)}`;
    $("#s-lat").textContent = m.latency.count ? `${fmtMs(m.latency.p50)} / ${fmtMs(m.latency.p95)}` : "–";
    $("#s-latsub").textContent = m.latency.count ? `样本 ${m.latency.count} · 最大 ${fmtMs(m.latency.max)}` : "尚无样本";

    const rows = Object.entries(m.models || {});
    $("#tb-models").innerHTML = rows.length ? rows.map(([id, v]) => `
      <tr><td><span class="tag">${esc(id)}</span></td>
      <td class="num">${fmtInt(v.requests)}</td>
      <td class="num" style="color:${v.errors ? "var(--err)" : "inherit"}">${fmtInt(v.errors)}</td>
      <td class="num">${fmtInt(v.prompt_tokens + v.completion_tokens)}</td>
      <td class="num">${v.requests ? fmtMs(v.sum_ms / v.requests) : "–"}</td></tr>`).join("")
      : '<tr><td colspan="5" class="empty">暂无数据</td></tr>';

    $("#site-cards").innerHTML = (ov.sites || []).map(s => {
      const ts = (ov.turnstile.status || []).find(x => x.site === s.code) || {};
      const ms = m.sites && m.sites[s.code];
      const pct = ts.remaining_s > 0 ? Math.max(0, Math.min(100, ts.remaining_s / (ov.turnstile.config.cookie_ttl_seconds || 10800) * 100)) : 0;
      return `<div class="card" style="padding:12px">
        <div class="toolbar">
          <div><b>${esc(s.code.toUpperCase())}</b> <span class="pill ${s.enabled ? "on" : "off"}">${s.enabled ? "启用" : "停用"}</span>
            <div class="dim">${esc(s.base_url)}</div></div>
          <div class="right"><button class="btn small" data-probe="${esc(s.code)}">探测</button>
            <button class="btn small" data-quota="${esc(s.code)}">额度</button>
            <button class="btn small" data-rotate="${esc(s.code)}">重置</button></div>
        </div>
        <div class="dim" style="margin-top:8px">请求 ${fmtInt(ms ? ms.requests : 0)} · 错误 ${fmtInt(ms ? ms.errors : 0)} · Cookie ${ts.valid ? "有效 " + Math.round(ts.remaining_s / 60) + " 分钟" : "未建立"}</div>
        <div class="bar"><i style="width:${pct}%"></i></div></div>`;
    }).join("") || '<div class="empty">无站点</div>';

    $$("[data-probe]").forEach(b => b.onclick = async () => {
      b.disabled = true; b.textContent = "…";
      try {
        const r = await api("/api/actions", { body: { action: "probe_site", site: b.dataset.probe } });
        toast(r.ok ? `${b.dataset.probe}: HTTP ${r.status} · ${r.ms}ms · 对话容器 ${r.chat_container ? "有" : "无"}` : `${b.dataset.probe}: ${r.error || "失败"}`, r.ok ? "ok" : "err");
      } catch (e) { toast(e.message, "err"); }
      b.disabled = false; b.textContent = "探测";
    });

    $$("[data-quota]").forEach(b => b.onclick = async () => {
      b.disabled = true; b.textContent = "…";
      try {
        const r = await api("/api/actions", { body: { action: "site_balance", site: b.dataset.quota } });
        const period = r.reset_period === "daily" ? "每日" : (r.reset_period || "");
        toast(r.ok ? `${b.dataset.quota}: 剩余 ${fmtInt(r.remaining)} / ${fmtInt(r.limit)} · 已用 ${fmtInt(r.used)} · ${period}重置` : `${b.dataset.quota}: ${r.error || "失败"}`, r.ok ? "ok" : "err");
      } catch (e) { toast(e.message, "err"); }
      b.disabled = false; b.textContent = "额度";
    });

    $$("[data-rotate]").forEach(b => b.onclick = async () => {
      b.disabled = true; b.textContent = "…";
      try {
        const r = await api("/api/actions", { body: { action: "rotate_guest", site: b.dataset.rotate } });
        toast(r.ok ? `${b.dataset.rotate}: 已换新访客身份，剩余 ${fmtInt(r.remaining)} / ${fmtInt(r.limit)}` : `${b.dataset.rotate}: ${r.error || "失败"}`, r.ok ? "ok" : "err");
      } catch (e) { toast(e.message, "err"); }
      b.disabled = false; b.textContent = "重置";
    });

    const rec = (m.recent || []).slice(0, 30);
    $("#tb-recent").innerHTML = rec.length ? rec.map(r => `
      <tr><td class="dim">${timeStr(r.at)}</td>
      <td><span class="tag">${esc(r.served_by || r.model)}</span></td>
      <td>${esc(r.site)}</td><td class="dim">${esc(r.route || "–")}</td>
      <td>${r.stream ? "流式" : "同步"}</td>
      <td class="num">${fmtMs(r.duration_ms)}</td>
      <td class="num">${fmtInt((r.prompt_tokens || 0) + (r.completion_tokens || 0))}</td>
      <td><span class="pill ${r.status === "ok" ? "on" : "off"}">${esc(r.status)}</span>${r.error ? ` <span class="dim">${esc(r.error.slice(0, 60))}</span>` : ""}</td></tr>`).join("")
      : '<tr><td colspan="8" class="empty">暂无请求</td></tr>';
  }
  await refresh();
  overviewTimer = setInterval(refresh, 5000);
};

PAGES.models = async () => {
  const ov = await fetchOverview();
  view.innerHTML = `
    <div class="card"><div class="toolbar"><h3 style="margin:0">模型</h3>
      <div class="dim">bot_id / post_id 修改后会自动清除该模型的 nonce 缓存</div></div>
      <div class="table-wrap mt"><table>
      <thead><tr><th>ID</th><th>标签</th><th>站点</th><th>上游模型</th><th>路径</th><th class="num">bot</th><th class="num">post</th><th>启用</th><th></th></tr></thead>
      <tbody id="tb"></tbody></table></div></div>
    <div class="card"><h3>站点</h3><div class="table-wrap"><table>
      <thead><tr><th>代码</th><th>Base URL</th><th>AJAX URL</th><th>语言</th><th>启用</th></tr></thead>
      <tbody id="tb-sites"></tbody></table></div></div>`;

  $("#tb").innerHTML = ov.models.map(m => `
    <tr data-id="${esc(m.id)}">
      <td><span class="tag">${esc(m.id)}</span></td>
      <td><input data-f="label" value="${esc(m.label)}" style="min-width:130px"></td>
      <td>${esc(m.site)}</td><td class="dim">${esc(m.upstream_id)}</td>
      <td><input data-f="page_path" value="${esc(m.page_path)}" style="width:80px"></td>
      <td class="num"><input data-f="bot_id" type="number" value="${m.bot_id}" style="width:90px;text-align:right"></td>
      <td class="num"><input data-f="post_id" type="number" value="${m.post_id}" style="width:90px;text-align:right"></td>
      <td>${switchHTML("sw-" + m.id, m.enabled)}</td>
      <td><button class="btn small primary" data-save="${esc(m.id)}">保存</button></td>
    </tr>`).join("");

  $("#tb-sites").innerHTML = ov.sites.map(s => `
    <tr data-code="${esc(s.code)}">
      <td><b>${esc(s.code.toUpperCase())}</b></td>
      <td><input data-f="base_url" value="${esc(s.base_url)}" style="min-width:190px"></td>
      <td><input data-f="ajax_url" value="${esc(s.ajax_url)}" style="min-width:230px"></td>
      <td><input data-f="language" value="${esc(s.language)}" style="min-width:200px"></td>
      <td>${switchHTML("sws-" + s.code, s.enabled)}
          <div class="mt"><button class="btn small primary" data-savesite="${esc(s.code)}">保存</button></div></td>
    </tr>`).join("");

  $$("[data-save]").forEach(b => b.onclick = async () => {
    const tr = b.closest("tr"), id = b.dataset.save;
    const g = f => $(`[data-f="${f}"]`, tr).value;
    try {
      await api("/api/actions", { body: { action: "update_model", id, label: g("label"), page_path: g("page_path"), bot_id: +g("bot_id"), post_id: +g("post_id") } });
      toast("模型已保存", "ok");
    } catch (e) { toast(e.message, "err"); }
  });
  $$("[data-savesite]").forEach(b => b.onclick = async () => {
    const tr = b.closest("tr"), code = b.dataset.savesite;
    const g = f => $(`[data-f="${f}"]`, tr).value;
    try {
      await api("/api/actions", { body: { action: "update_site", code, base_url: g("base_url"), ajax_url: g("ajax_url"), language: g("language") } });
      toast("站点已保存", "ok");
    } catch (e) { toast(e.message, "err"); }
  });
  ov.models.forEach(m => {
    $("#sw-" + CSS.escape(m.id)).onchange = async ev => {
      try { await api("/api/actions", { body: { action: "toggle_model", id: m.id, enabled: ev.target.checked } }); toast("已更新", "ok"); }
      catch (e) { toast(e.message, "err"); ev.target.checked = !ev.target.checked; }
    };
  });
  ov.sites.forEach(s => {
    $("#sws-" + CSS.escape(s.code)).onchange = async ev => {
      try { await api("/api/actions", { body: { action: "toggle_site", id: s.code, enabled: ev.target.checked } }); toast("已更新", "ok"); }
      catch (e) { toast(e.message, "err"); ev.target.checked = !ev.target.checked; }
    };
  });
};

PAGES.keys = async () => {
  const ov = await fetchOverview();
  view.innerHTML = `
    <div class="card"><div class="toolbar">
      <div><h3 style="margin:0">下游 API Key</h3><div class="dim mt">留空表示不鉴权；请求时使用 <code>Authorization: Bearer &lt;key&gt;</code> 或 <code>X-Api-Key</code></div></div>
      <button class="btn primary" id="btn-gen">生成新 Key</button></div>
      <div class="table-wrap mt"><table>
        <thead><tr><th>Key</th><th>状态</th><th></th></tr></thead><tbody id="tb"></tbody></table></div>
      <div class="dim mt">修改会立即生效并写入 config.toml（自动备份为 .bak）</div></div>`;
  const render = keys => {
    $("#tb").innerHTML = keys.length ? keys.map(k => `
      <tr><td><code>${esc(mask(k))}</code></td><td><span class="pill on">启用</span></td>
      <td class="right"><button class="btn small" data-copy="${esc(k)}">复制</button>
      <button class="btn small danger" data-del="${esc(k)}">删除</button></td></tr>`).join("")
      : '<tr><td colspan="3" class="empty">未配置任何 Key —— 当前不鉴权</td></tr>';
    $$("[data-copy]").forEach(b => b.onclick = () => { navigator.clipboard && navigator.clipboard.writeText(b.dataset.copy); toast("已复制", "ok"); });
    $$("[data-del]").forEach(b => b.onclick = async () => {
      if (!confirm("删除该 Key？")) return;
      try { await api("/api/actions", { body: { action: "delete_key", key: b.dataset.del } }); toast("已删除", "ok"); PAGES.keys(); }
      catch (e) { toast(e.message, "err"); }
    });
  };
  render(ov.keys || []);
  $("#btn-gen").onclick = async () => {
    try {
      const r = await api("/api/actions", { body: { action: "create_key" } });
      if (r.ok) { toast("已生成", "ok"); PAGES.keys(); }
    } catch (e) { toast(e.message, "err"); }
  };
};
const mask = k => k.length > 16 ? k.slice(0, 7) + "…" + k.slice(-6) : k;

PAGES.proxy = async () => {
  const ov = await fetchOverview();
  const p = ov.proxy;
  view.innerHTML = `
    <div class="grid g2">
      <div class="card"><h3>主线路</h3>
        <div class="field"><label>PROXY_URL（留空 = 直连）</label><input id="px-primary" value="${esc(p.url)}" placeholder="socks5://user:pass@host:port"></div>
        <div class="row">
          <button class="btn primary" id="px-save">保存</button>
          <button class="btn" id="px-test">连通性测试</button>
        </div>
        <div class="dim mt">支持 http:// · https:// · socks5://</div>
      </div>
      <div class="card"><h3>降级策略</h3>
        <div class="row">
          <div class="field"><label>slow-start（秒）</label><input id="px-slow" type="number" step="0.5" value="${p.slow_start}"></div>
          <div class="field"><label>每站点并发</label><input id="px-conc" type="number" value="${p.max_concurrent}"></div>
          <div class="field"><label>每 Key 请求/分钟（0=不限）</label><input id="px-rate" type="number" value="${p.rate_per_minute}"></div>
        </div>
        <div class="row mt">
          <div class="field"><label>跨站配额切换</label>${switchHTML("px-cross", p.cross_site_failover)}</div>
          <button class="btn primary right" id="px-savelim">保存</button>
        </div>
        <div class="dim mt">主线路在 slow-start 内无首事件，或输出内容前失败时，自动切换到备用线路。</div>
      </div>
    </div>
    <div class="card"><div class="toolbar"><h3 style="margin:0">备用线路</h3>
      <div class="row"><input id="px-fb" placeholder="socks5://..." style="width:280px"><button class="btn" id="px-add">添加</button></div></div>
      <div class="table-wrap mt"><table><thead><tr><th>#</th><th>地址</th><th></th></tr></thead><tbody id="tb-fb"></tbody></table></div>
      <div class="dim mt">当前生效顺序：${ov.cache.routes.map(r => `<span class="tag">${esc(r.name)} → ${esc(r.proxy)}</span>`).join(" ")}</div>
    </div>`;

  const renderFb = list => {
    $("#tb-fb").innerHTML = list.length ? list.map((u, i) => `
      <tr><td class="num">${i + 1}</td><td><code>${esc(u)}</code></td>
      <td class="right"><button class="btn small danger" data-rm="${esc(u)}">移除</button></td></tr>`).join("")
      : '<tr><td colspan="3" class="empty">无备用线路</td></tr>';
    $$("[data-rm]").forEach(b => b.onclick = async () => {
      try { await api("/api/actions", { body: { action: "remove_fallback", url: b.dataset.rm } }); PAGES.proxy(); }
      catch (e) { toast(e.message, "err"); }
    });
  };
  renderFb(p.fallbacks || []);

  $("#px-save").onclick = async () => {
    try {
      await api("/api/config", { method: "PUT", body: await merged(c => { c.proxy.url = $("#px-primary").value.trim(); }) });
      toast("主线路已保存", "ok"); PAGES.proxy();
    } catch (e) { toast(e.message, "err"); }
  };
  $("#px-test").onclick = async () => {
    const b = $("#px-test"); b.disabled = true; b.textContent = "测试中…";
    try {
      const r = await api("/api/actions", { body: { action: "test_proxy", url: $("#px-primary").value.trim() } });
      toast(r.ok ? `连通 · HTTP ${r.status} · ${r.ms}ms` : (r.error || "失败"), r.ok ? "ok" : "err");
    } catch (e) { toast(e.message, "err"); }
    b.disabled = false; b.textContent = "连通性测试";
  };
  $("#px-savelim").onclick = async () => {
    try {
      await api("/api/actions", { body: {
        action: "set_limits", slow_start: +$("#px-slow").value,
        max_concurrent: +$("#px-conc").value, rate_per_minute: +$("#px-rate").value,
        cross_site_failover: $("#px-cross").checked } });
      toast("已保存", "ok");
    } catch (e) { toast(e.message, "err"); }
  };
  $("#px-add").onclick = async () => {
    const v = $("#px-fb").value.trim(); if (!v) return;
    try { await api("/api/actions", { body: { action: "add_fallback", url: v } }); $("#px-fb").value = ""; PAGES.proxy(); }
    catch (e) { toast(e.message, "err"); }
  };
};

async function merged(mutate) {
  const cfg = await api("/api/config");
  mutate(cfg);
  return cfg;
}

PAGES.turnstile = async () => {
  const ov = await fetchOverview();
  const t = ov.turnstile.config;
  view.innerHTML = `
    <div class="grid g2">
      <div class="card"><h3>求解器配置</h3>
        <div class="row">
          <div class="field"><label>Token 获取方式</label><select id="ts-provider">
            <option value="api">方式 1：求解服务 API</option>
            <option value="browser">方式 2：本地浏览器自动获取</option>
            <option value="manual">方式 3：手动导入 Cookie</option>
          </select></div>
          <div class="field"><label>启用</label>${switchHTML("ts-en", t.enabled)}</div>
        </div>
        <div id="ts-api" class="ts-panel">
          <div class="ts-panel-title">方式 1 · 求解服务 API</div>
          <div class="field"><label>求解服务 API URL</label><input id="ts-url" value="${esc(t.api_url)}"></div>
          <div class="field"><label>API Key（留空保持不变）</label><input id="ts-key" value="" placeholder="已配置，输入以覆盖"></div>
          <div class="row">
            <div class="field"><label>Sitekey</label><input id="ts-sitekey" value="${esc(t.sitekey)}"></div>
            <div class="field"><label>Action</label><input id="ts-action" value="${esc(t.action)}"></div>
          </div>
        </div>
        <div id="ts-browser" class="ts-panel" style="display:none">
          <div class="ts-panel-title">方式 2 · 本地浏览器自动获取</div>
          <div class="field"><label>浏览器路径（Chrome / Edge 可执行文件）</label>
            <input id="ts-bpath" value="${esc(t.browser_path || "")}"
              placeholder="C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe"></div>
          <div class="row">
            <div class="field"><label>无头模式</label>${switchHTML("ts-bhead", !!t.browser_headless)}</div>
            <div class="field"><label>时区（可选，按代理出口对齐）</label>
              <input id="ts-btz" value="${esc(t.browser_timezone || "")}" placeholder="Asia/Tokyo"></div>
          </div>
          <div class="row">
            <div class="field"><label>Locale（可选）</label>
              <input id="ts-bloc" value="${esc(t.browser_locale || "")}" placeholder="ja-JP"></div>
            <div class="field"><label>用户数据目录（可选）</label>
              <input id="ts-bprof" value="${esc(t.browser_user_data_dir || "")}" placeholder="留空 = 临时 profile"></div>
          </div>
          <div class="dim">CDP 直连浏览器，无需 Playwright / Docker；求解期间会短暂打开一个窗口（无头模式除外）。</div>
        </div>
        <div id="ts-manual" class="ts-panel" style="display:none">
          <div class="ts-panel-title">方式 3 · 手动导入 Cookie</div>
          <div class="row">
            <div class="field"><label>Cookie TTL（秒）</label><input id="ts-ttl" type="number" value="${t.cookie_ttl_seconds}"></div>
            <div class="field"><label>站点</label><select id="im-site">${
              (ov.sites || []).map(s => `<option value="${esc(s.code)}">${esc(s.code.toUpperCase())} · ${esc(s.base_url)}</option>`).join("")
            }</select></div>
          </div>
          <div class="field"><label>Cookie（name=value; name2=value2）</label><textarea id="im-cookies" style="min-height:96px"
            placeholder="在已通过验证的浏览器里执行 document.cookie，或从开发者工具复制请求头 Cookie"></textarea></div>
          <div class="dim">Cookie 与出口 IP / UA 绑定：请在同一代理线路的浏览器中获取。导入后在 TTL 内免求解，状态见右侧「已验证 Cookie」。</div>
          <button class="btn primary mt" id="im-save">导入</button>
        </div>
        <div class="dim" id="ts-hint" style="margin-top:10px"></div>
        <button class="btn primary mt" id="ts-save">保存配置</button>
      </div>
      <div class="card"><h3>已验证 Cookie</h3>
        <div id="ts-status" class="grid" style="gap:10px"></div>
        <div class="row mt">
          <button class="btn" id="ts-refresh">强制刷新全部</button>
          <button class="btn danger" id="ts-clear">清空 Cookie</button>
        </div>
        <div class="dim mt">缓存 TTL ${t.cookie_ttl_seconds}s · 求解失败重试 ${t.retries} 次 · 退避 ${t.retry_backoff_seconds}s</div>
      </div>
    </div>
    <div class="card mt"><h3>求解历史</h3><div class="table-wrap"><table>
      <thead><tr><th>时间</th><th>站点</th><th>线路</th><th class="num">耗时</th><th>结果</th></tr></thead>
      <tbody id="ts-hist"></tbody></table></div></div>`;

  $("#ts-status").innerHTML = (ov.turnstile.status || []).map(s => `
    <div class="card" style="padding:12px"><div class="toolbar">
      <div><b>${esc(s.site.toUpperCase())}</b> <span class="pill ${s.valid ? "on" : "off"}">${s.valid ? "有效" : "未建立"}</span></div>
      <button class="btn small" data-rf="${esc(s.site)}">刷新</button></div>
      <div class="dim mt">${s.valid ? "剩余 " + Math.round(s.remaining_s / 60) + " 分钟 · " + s.cookie_count + " 个 Cookie" : "下次请求时自动求解"}</div>
    </div>`).join("");

  const hist = ov.turnstile.history || [];
  $("#ts-hist").innerHTML = hist.length ? hist.slice().reverse().slice(0, 40).map(h => `
    <tr><td class="dim">${timeStr(h.at)}</td><td>${esc(h.site)}</td><td class="dim">${esc(h.proxy)}</td>
    <td class="num">${fmtMs(h.elapsed_ms)}</td>
    <td><span class="pill ${h.ok ? "on" : "off"}">${h.ok ? "成功" : "失败"}</span>${h.error ? ` <span class="dim">${esc(h.error.slice(0, 70))}</span>` : ""}</td></tr>`).join("")
    : '<tr><td colspan="5" class="empty">尚无记录</td></tr>';

  $$("[data-rf]").forEach(b => b.onclick = async () => {
    b.disabled = true;
    try { await api("/api/actions", { body: { action: "refresh_cookies", site: b.dataset.rf } }); toast("已作废，将在下次请求时重新求解", "ok"); PAGES.turnstile(); }
    catch (e) { toast(e.message, "err"); }
    b.disabled = false;
  });
  $("#ts-provider").value = t.provider || "api";
  const TS_HINTS = {
    api: ["已开启：调用求解服务 API 获取 Turnstile Token", "已关闭：跳过 Turnstile 求解，直接请求上游站点"],
    browser: ["已开启：用下方配置的本地浏览器自动求解 Token", "已关闭：跳过 Turnstile 求解，直接请求上游站点"],
    manual: ["手动方式：不自动求解，导入已通过验证的 Cookie 即可（TTL 内免求解）", "已关闭：跳过 Turnstile 求解，直接请求上游站点"],
  };
  const syncProvider = () => {
    const p = $("#ts-provider").value;
    $("#ts-api").style.display = p === "api" ? "" : "none";
    $("#ts-browser").style.display = p === "browser" ? "" : "none";
    $("#ts-manual").style.display = p === "manual" ? "" : "none";
    $("#ts-refresh").style.display = p === "manual" ? "none" : "";
    const hints = TS_HINTS[p] || TS_HINTS.api;
    $("#ts-hint").textContent = $("#ts-en").checked ? hints[0] : hints[1];
  };
  $("#ts-provider").onchange = syncProvider;
  $("#ts-en").onchange = syncProvider;
  syncProvider();
  $("#ts-save").onclick = async () => {
    try {
      await api("/api/actions", { body: {
        action: "set_turnstile", enabled: $("#ts-en").checked,
        provider: $("#ts-provider").value,
        api_url: $("#ts-url").value.trim(),
        api_key: $("#ts-key").value.trim(), sitekey: $("#ts-sitekey").value.trim(),
        challenge_action: $("#ts-action").value.trim(), cookie_ttl_seconds: +$("#ts-ttl").value,
        browser_path: $("#ts-bpath").value.trim(),
        browser_headless: $("#ts-bhead").checked,
        browser_user_data_dir: $("#ts-bprof").value.trim(),
        browser_timezone: $("#ts-btz").value.trim(),
        browser_locale: $("#ts-bloc").value.trim() } });
      toast("已保存", "ok"); PAGES.turnstile();
    } catch (e) { toast(e.message, "err"); }
  };
  $("#ts-refresh").onclick = async () => {
    try { await api("/api/actions", { body: { action: "invalidate" } }); toast("已作废全部 cookie", "ok"); PAGES.turnstile(); }
    catch (e) { toast(e.message, "err"); }
  };
  $("#ts-clear").onclick = async () => {
    try { await api("/api/actions", { body: { action: "clear_cookies" } }); toast("已清空 Cookie", "ok"); PAGES.turnstile(); }
    catch (e) { toast(e.message, "err"); }
  };
  $("#im-save").onclick = async () => {
    try {
      const r = await api("/api/actions", { body: {
        action: "import_cookies", site: $("#im-site").value, cookies: $("#im-cookies").value } });
      toast(`已导入 ${r.cookie_count} 个 Cookie，${timeStr(r.expires_at * 1000)} 到期`, "ok");
      PAGES.turnstile();
    } catch (e) { toast(e.message, "err"); }
  };
};

PAGES.playground = async () => {
  const ov = await fetchOverview();
  const enabled = ov.models.filter(m => m.enabled);
  view.innerHTML = `
    <div class="grid g2" style="align-items:start">
      <div class="card"><h3>请求</h3>
        <div class="row">
          <div class="field"><label>模型</label><select id="pg-model">${enabled.map(m => `<option value="${esc(m.id)}">${esc(m.id)}</option>`).join("")}</select></div>
          <div class="field" style="flex:0 0 auto"><label>流式</label>${switchHTML("pg-stream", true)}</div>
        </div>
        <div class="field"><label>System 提示</label><textarea id="pg-system" style="min-height:60px">你是一个乐于助人的助手。</textarea></div>
        <div class="field"><label>Tools 定义（JSON，可留空）</label><textarea id="pg-tools" style="min-height:80px" placeholder='[{"type":"function","function":{"name":"get_weather","description":"获取天气","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]'></textarea></div>
        <div class="field"><label>输入</label><textarea id="pg-input" style="min-height:70px" placeholder="输入消息…"></textarea></div>
        <div class="row"><button class="btn primary" id="pg-send">发送</button>
        <button class="btn danger" id="pg-cancel" disabled>取消</button>
        <button class="btn" id="pg-clear">清空输出</button>
        <span class="dim" id="pg-meta"></span></div>
      </div>
      <div class="card"><h3>响应</h3><div class="out-box" id="pg-out"><span class="dim">尚未发送请求</span></div></div>
    </div>`;

  let ac = null, timer = null;
  const stopTimer = () => { if (timer) { clearInterval(timer); timer = null; } };

  $("#pg-clear").onclick = () => { stopTimer(); ac = null; $("#pg-out").innerHTML = '<span class="dim">尚未发送请求</span>'; $("#pg-meta").textContent = ""; };
  $("#pg-cancel").onclick = () => { if (ac) ac.abort(); };
  $("#pg-send").onclick = async () => {
    const input = $("#pg-input").value.trim();
    if (!input) { toast("请输入内容", "err"); return; }
    let tools = null;
    const raw = $("#pg-tools").value.trim();
    if (raw) { try { tools = JSON.parse(raw); } catch (e) { toast("Tools JSON 解析失败", "err"); return; } }

    const messages = [{ role: "system", content: $("#pg-system").value }, { role: "user", content: input }];
    const model = $("#pg-model").value;
    const stream = $("#pg-stream").checked;
    const out = $("#pg-out");
    out.innerHTML = "";

    const status = document.createElement("span");
    status.className = "dim";
    status.textContent = "连接中… 0.0s";
    const caret = document.createElement("span"); caret.className = "caret";
    out.appendChild(status); out.appendChild(caret);

    ac = new AbortController();
    const btn = $("#pg-send"), cancel = $("#pg-cancel");
    btn.disabled = true; btn.textContent = "请求中…";
    cancel.disabled = false;
    $("#pg-meta").textContent = "";
    const t0 = Date.now();
    const tick = () => { status.textContent = `连接中… ${((Date.now() - t0) / 1000).toFixed(1)}s`; };
    stopTimer(); timer = setInterval(tick, 100);

    const finish = () => {
      stopTimer(); ac = null;
      btn.disabled = false; btn.textContent = "发送";
      cancel.disabled = true;
      if (!$("#pg-meta").textContent) $("#pg-meta").textContent = `${fmtMs(Date.now() - t0)}`;
    };
    const firstText = () => { if (status.parentNode) status.remove(); };

    try {
      if (stream) {
        const res = await fetch("/api/playground", { method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ model, messages, stream: true, tools }), signal: ac.signal });
        if (!res.ok) { const j = await res.json().catch(() => ({})); throw new Error(j.error || res.statusText); }
        out.appendChild(caret);
        const reader = res.body.getReader(), dec = new TextDecoder();
        let buf = "", first = null;
        for (;;) {
          const { done, value } = await reader.read(); if (done) break;
          buf += dec.decode(value, { stream: true });
          let idx;
          while ((idx = buf.indexOf("\n\n")) >= 0) {
            const frame = buf.slice(0, idx); buf = buf.slice(idx + 2);
            if (!frame.startsWith("data: ")) continue;
            const payload = frame.slice(6);
            if (payload === "[DONE]") continue;
            let j; try { j = JSON.parse(payload); } catch (e) { continue; }
            if (j.error) { firstText(); out.insertBefore(document.createTextNode("\n[错误] " + j.error), caret); continue; }
            if (j.delta) { if (!first) first = Date.now(); firstText(); out.insertBefore(document.createTextNode(j.delta), caret); }
            if (j.done) { $("#pg-meta").textContent = `${j.ms}ms · ${j.site} · ${j.route}`; }
          }
        }
        caret.remove();
      } else {
        const j = await api("/api/playground", { body: { model, messages, stream: false, tools }, signal: ac.signal });
        out.textContent = j.text;
        $("#pg-meta").textContent = `${j.ms}ms · ${j.site} · ${j.route} · prompt ${j.prompt_chars} 字符`;
      }
    } catch (e) {
      caret.remove();
      if (e && e.name === "AbortError") {
        out.innerHTML = '<span class="dim">已取消</span>';
      } else {
        out.innerHTML = `<span style="color:var(--err)">请求失败：${esc(e.message)}</span>`;
      }
    }
    finish();
  };
};

PAGES.logs = async () => {
  view.innerHTML = `
    <div class="card"><div class="toolbar">
      <div class="row">
        <div class="field" style="margin:0"><label>最低级别</label>
          <select id="lg-level" style="width:120px"><option>DEBUG</option><option selected>INFO</option><option>WARN</option><option>ERROR</option></select></div>
        <button class="btn" id="lg-toggle">暂停</button>
        <button class="btn danger" id="lg-clear">清屏</button>
      </div>
      <div class="dim" id="lg-state">实时连接中…</div>
    </div>
    <div class="log-box mt" id="lg-box"></div></div>`;

  const box = $("#lg-box");
  let paused = false, es = null, gen = 0;
  const minVal = { DEBUG: -4, INFO: 0, WARN: 4, ERROR: 8 };

  function append(e) {
    if (paused) return;
    const sel = $("#lg-level");
    if (sel && minVal[e.level] < minVal[sel.value]) return;
    const line = document.createElement("div");
    line.className = "log-line";
    const attrs = e.attrs ? Object.entries(e.attrs).map(([k, v]) => `${k}=${v}`).join(" ") : "";
    line.innerHTML = `<span class="t">${timeStr(e.ts)}</span> <span class="lv lv-${e.level}">${e.level}</span> ${esc(e.msg)}${attrs ? ' <span class="t">' + esc(attrs) + "</span>" : ""}`;
    box.appendChild(line);
    while (box.children.length > 600) box.removeChild(box.firstChild);
    box.scrollTop = box.scrollHeight;
  }

  function stopES() { if (es) { es.close(); es = null; } }

  // (Re)load history and reopen the SSE stream with the currently selected
  // minimum level. Never re-render the page here: rebuilding the select would
  // reset it to INFO before the value is read, and would leak the old stream.
  // History comes solely from the server replay on connect (and on every
  // automatic reconnect), so the box is cleared in onopen instead of fetching
  // the JSON endpoint separately — otherwise entries show up twice.
  async function load() {
    const my = ++gen;
    const level = $("#lg-level").value;
    stopES();
    box.innerHTML = "";
    es = new EventSource("/api/logs?stream=1&level=" + level);
    es.onmessage = ev => { if (my !== gen) return; try { append(JSON.parse(ev.data)); } catch (e) {} };
    es.onerror = () => { if (my !== gen) return; const st = $("#lg-state"); if (st) st.textContent = "连接中断，自动重连中…"; };
    es.onopen = () => {
      if (my !== gen) return;
      box.innerHTML = "";
      const st = $("#lg-state"); if (st) st.textContent = "实时连接中…";
    };
    logStop = () => { gen++; stopES(); };
  }

  $("#lg-toggle").onclick = e => { paused = !paused; e.target.textContent = paused ? "继续" : "暂停"; };
  $("#lg-clear").onclick = () => { box.innerHTML = ""; };
  $("#lg-level").onchange = load;
  await load();
};

PAGES.settings = async () => {
  const cfg = await api("/api/config");
  view.innerHTML = `
    <div class="grid g2">
      <div class="card"><h3>服务</h3>
        <div class="row">
          <div class="field"><label>监听地址</label><input id="st-host" value="${esc(cfg.server.host)}"></div>
          <div class="field"><label>端口</label><input id="st-port" type="number" value="${cfg.server.port}"></div>
        </div>
        <div class="field"><label>日志级别</label><select id="st-log">
          ${["DEBUG", "INFO", "WARN", "ERROR"].map(l => `<option ${cfg.server.log_level === l ? "selected" : ""}>${l}</option>`).join("")}
        </select></div>
        <div class="row">
          <div class="field"><label>管理台地址</label><input id="st-ah" value="${esc(cfg.admin.host)}"></div>
          <div class="field"><label>管理台端口</label><input id="st-ap" type="number" value="${cfg.admin.port}"></div>
        </div>
        <div class="field"><label>管理台密码（留空保持不变）</label><input id="st-pw" type="password" placeholder="••••••••"></div>
        <button class="btn primary" id="st-save">保存服务设置</button>
      </div>
      <div class="card"><h3>上游</h3>
        <div class="row">
          <div class="field"><label>请求超时（秒）</label><input id="st-to" type="number" value="${cfg.upstream.timeout}"></div>
          <div class="field"><label>流式超时（秒）</label><input id="st-sto" type="number" value="${cfg.upstream.stream_timeout}"></div>
        </div>
        <div class="row">
          <div class="field"><label>配置缓存 TTL（秒）</label><input id="st-ttl" type="number" value="${cfg.upstream.config_ttl_seconds}"></div>
          <div class="field"><label>会话刷新重试</label><input id="st-retry" type="number" value="${cfg.upstream.refresh_retries}"></div>
        </div>
        <div class="row">
          <div class="field"><label>失败退避（秒）</label><input id="st-back" type="number" step="0.1" value="${cfg.upstream.retry_backoff_seconds}"></div>
          <div class="field"><label>自动刷新会话</label>${switchHTML("st-ar", cfg.upstream.auto_refresh)}</div>
        </div>
        <div class="field"><label>User-Agent</label><input id="st-ua" value="${esc(cfg.upstream.user_agent)}"></div>
        <div class="field"><label>数据目录</label><input id="st-dir" value="${esc(cfg.runtime.data_dir)}"></div>
        <button class="btn primary" id="st-save2">保存上游设置</button>
      </div>
    </div>
    <div class="card"><h3>危险操作</h3>
      <div class="row">
        <button class="btn danger" id="st-reset">清空统计</button>
        <button class="btn danger" id="st-inv">作废全部会话缓存</button>
      </div>
      <div class="dim mt">配置保存会自动备份原文件为 <code>config.toml.bak</code>；修改端口/密码需重启服务生效。</div>
    </div>`;

  const put = async mutate => {
    try {
      const c = await api("/api/config");
      mutate(c);
      await api("/api/config", { method: "PUT", body: c });
      toast("已保存并写入 config.toml", "ok");
      return true;
    } catch (e) { toast(e.message, "err"); return false; }
  };
  $("#st-save").onclick = () => put(c => {
    c.server.host = $("#st-host").value;
    c.server.port = +$("#st-port").value;
    c.server.log_level = $("#st-log").value;
    c.admin.host = $("#st-ah").value;
    c.admin.port = +$("#st-ap").value;
    if ($("#st-pw").value) c.admin.password = $("#st-pw").value;
  });
  $("#st-save2").onclick = () => put(c => {
    c.upstream.timeout = +$("#st-to").value;
    c.upstream.stream_timeout = +$("#st-sto").value;
    c.upstream.config_ttl_seconds = +$("#st-ttl").value;
    c.upstream.refresh_retries = +$("#st-retry").value;
    c.upstream.retry_backoff_seconds = +$("#st-back").value;
    c.upstream.auto_refresh = $("#st-ar").checked;
    c.upstream.user_agent = $("#st-ua").value;
    c.runtime.data_dir = $("#st-dir").value;
  });
  $("#st-reset").onclick = async () => { try { await api("/api/actions", { body: { action: "reset_metrics" } }); toast("已清空", "ok"); } catch (e) { toast(e.message, "err"); } };
  $("#st-inv").onclick = async () => { try { await api("/api/actions", { body: { action: "invalidate" } }); toast("已作废", "ok"); } catch (e) { toast(e.message, "err"); } };
};

/* ── boot ────────────────────────────────────────────────── */
window.addEventListener("hashchange", route);
$("#login-btn").onclick = doLogin;
$("#login-password").addEventListener("keydown", e => { if (e.key === "Enter") doLogin(); });
$("#logout-btn").onclick = async () => { try { await api("/api/logout", { method: "POST" }); } catch (e) {} location.hash = "#/dashboard"; showLogin(); };

(async function init() {
  if (await ensureSession()) route();
})();
})();
