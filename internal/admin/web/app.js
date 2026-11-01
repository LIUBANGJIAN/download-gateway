// 下载网关管理台：原生 ES 模块（零构建）。
//
// ─────────────────────────────────────────────────────────────────────────────
// 职责边界（本文件最重要的一条约束）
//
// **管理台只「看」和「管」，永不产生任务。**
//
// 因此这里没有、也不该有「添加任务」的代码路径。任务提交的唯一入口是
// 面向外部的协议面（`:6800` 上的 aria2 / BitComet 原生协议）。
//
// 工程上怎么保证这条边界不会被悄悄破坏：
//   · 后端 `POST /api/admin/tasks` 已删除，前端就算想调也会拿到 405；
//   · 本文件不定义任何 `createTask` / `submitTask` 之类的函数；
//   · 导航里只有 总览 / 节点 / 任务 / 告警 / 设置 —— 没有「添加」。
// ─────────────────────────────────────────────────────────────────────────────

const VIEWS = {
  '/': 'overview',
  '/login': 'login',
  '/nodes': 'nodes',
  '/tasks': 'tasks',
  '/events': 'events',
  '/settings': 'settings',
};
const PATHS = {
  login: '/login',
  overview: '/',
  nodes: '/nodes',
  tasks: '/tasks',
  events: '/events',
  settings: '/settings',
};

const state = {
  view: 'overview',
  nodes: [],
  nodeFilter: '',
  policies: [],
  events: [],          // 已加载的告警行（追加上去，不覆盖）
  eventsCursor: 0,     // 下一页游标；0 = 没有更多
  prioStep: 10,
  revealed: new Set(), // 已点开「眼睛」的节点 id（明文只在内存里，不落 localStorage）
  pendingTask: null,   // 待删除的任务
  editingNodeId: null, // 正在编辑的节点 id；null = 新增
};

// ── 请求封装：解 {code,data,message} 信封；非 0 抛错 ─────────────────────────
async function api(path, { method = 'GET', body } = {}) {
  const res = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) {
    show('login');
    throw new Error('未认证，请先登录');
  }
  let payload = null;
  try { payload = await res.json(); } catch { /* 非 JSON（如 404 空白体） */ }
  if (!res.ok || (payload && payload.code !== 0)) {
    const msg = payload && payload.message ? payload.message : `HTTP ${res.status}`;
    throw new Error(msg);
  }
  return payload ? payload.data : null;
}

const $ = (id) => document.getElementById(id);

function escapeHtml(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}

const fmtTime = (secs) => (secs ? new Date(secs * 1000).toLocaleString() : '—');

// 运行时长按人读的单位显示。原来直接拼 `${n}s`，跑三天会看到「259200s」。
function fmtUptime(secs) {
  const n = Number(secs);
  if (!Number.isFinite(n) || n < 0) return '—';
  const d = Math.floor(n / 86400);
  const h = Math.floor((n % 86400) / 3600);
  const m = Math.floor((n % 3600) / 60);
  const s = Math.floor(n % 60);
  const parts = [];
  if (d) parts.push(`${d} 天`);
  if (h) parts.push(`${h} 小时`);
  if (m) parts.push(`${m} 分`);
  if (!d && !h) parts.push(`${s} 秒`);
  return parts.join(' ');
}

function showErr(id, msg) {
  const el = $(id);
  if (!el) return;
  if (msg) { el.textContent = msg; el.hidden = false; } else { el.hidden = true; }
}

// ── 全局轻提示 ──────────────────────────────────────────────────────────────
// 用于「操作成功但不需要人读长文」的场合（切开关、暂停任务）。
// 失败仍然走页面内的 error-bar —— 错误要留在原地，不能被 3 秒后消失的 toast 吞掉。
let toastTimer = null;

function toast(msg, tone = 'ok') {
  const el = $('toast');
  el.textContent = msg;
  el.dataset.tone = tone;
  el.hidden = false;
  // 先移除再加：连续两次同向提示时，浏览器会认为样式没变而不重播过渡。
  el.classList.remove('show');
  void el.offsetWidth;
  el.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => {
    el.classList.remove('show');
    toastTimer = setTimeout(() => { el.hidden = true; }, 200);
  }, 2600);
}

// ── 视图切换 ────────────────────────────────────────────────────────────────
function show(view) {
  state.view = view;
  // 给 CSS 一个「当前在哪」的钩子：登录页要藏掉导航（见 app.css 的 body[data-view="login"]）
  document.body.dataset.view = view;
  for (const name of ['login', 'overview', 'nodes', 'tasks', 'events', 'settings']) {
    $(`view-${name}`).hidden = name !== view;
  }
  $('banner').hidden = view === 'login';
  document.querySelectorAll('.nav-btn[data-view]').forEach((b) => {
    const on = b.dataset.view === view;
    b.classList.toggle('active', on);
    // aria-current 让读屏软件知道「你在哪一页」，纯视觉的 .active 传达不了
    if (on) b.setAttribute('aria-current', 'page'); else b.removeAttribute('aria-current');
  });
  if (PATHS[view] && location.pathname !== PATHS[view]) {
    history.pushState({ view }, '', PATHS[view]);
  }
  if (view === 'overview') loadOverview();
  if (view === 'nodes') loadNodes();
  if (view === 'tasks') loadTasks();
  if (view === 'events') loadEvents(true);
  if (view === 'settings') { loadPolicies(); loadPasswordStatus(); }
  syncAutoRefresh();
}

// ═════════════════════════════ 总览 ═════════════════════════════

// 横幅文案必须跟着真实状态走。
// 写死「已启用」在 DISPATCH_SCHEDULER=off 时就是撒谎：用户以为任务在跑，
// 实际全在排队。反过来写死「未启用」则是自贬。所以两句话都要有，由数据选。
function renderBanner(schedulerOn) {
  const t = $('banner-text');
  const b = $('banner');
  if (schedulerOn) {
    b.dataset.tone = 'info';
    t.textContent = '调度与下发已启用：任务会自动选点、投递到 BitComet 节点，进度每 15 秒回读。'
      + '节点需为「在线」才会被选中（新节点请在「节点」页点一次探测）。';
  } else {
    b.dataset.tone = 'warn';
    t.textContent = '调度已关闭：新任务只会排队，不会自动投递到任何节点。'
      + '打开方式：去掉 DISPATCH_SCHEDULER=off 重启，或到「设置」把 app_config.scheduler.enabled 置为 on。';
  }
}

async function loadOverview() {
  const grid = $('overview-grid');
  showErr('overview-error', null);
  try {
    const data = await api('/api/admin/summary');
    renderBanner(!!data.scheduler_enabled);
    const nodeCount = Array.isArray(data.nodes) ? data.nodes.length : '—';
    const cells = [
      ['版本', data.version],
      ['运行时长', fmtUptime(data.uptime_seconds)],
      ['任务总数', data.task_total],
      ['节点数', nodeCount],
      ['对外口', data.ports && data.ports.public],
      ['管理口', data.ports && data.ports.admin],
      ['数据库', data.db_path],
      ['调度内核', data.scheduler_enabled ? '已启用' : '未启用（任务只排队）'],
    ];
    grid.innerHTML = cells.map(([k, v]) => (
      `<div class="card"><div class="k">${escapeHtml(k)}</div>`
      + `<div class="v">${escapeHtml(v == null || v === '' ? '—' : v)}</div></div>`
    )).join('');
    const counts = data.task_counts || [];
    $('overview-counts').innerHTML = counts.length
      ? counts.map((c) => `<span class="badge" data-state="${escapeHtml(c.state)}">`
        + `${escapeHtml(c.state)}: ${escapeHtml(c.count)}</span>`).join('')
      : '<span class="badge">无任务</span>';
  } catch (e) {
    showErr('overview-error', `加载总览失败：${e.message}`);
  }
}

// ═════════════════════════════ 节点 ═════════════════════════════
async function loadNodes() {
  showErr('node-error', null);
  try {
    const data = await api('/api/admin/nodes');
    state.nodes = data.items || [];
    // 刷新后旧的「已点开」状态作废：密码明文不该在页面里长期停留。
    state.revealed.clear();
    renderNodes();
  } catch (e) {
    showErr('node-error', `加载节点失败：${e.message}`);
  }
}

// onlineCell 显示节点的**调度可用性**，而不是"端口通不通"。
//
// 三态的原因：`state` 由调度内核给出（走完整登录 + about 调用），
// `reachable` 只是内联探活看到的端口响应。两者不一致时最该说清楚的是
// "能连上但用不了"—— 那种节点以前显示绿色"在线"，用户会一直等一个
// 永远不会被派发的任务。
function onlineCell(n) {
  const lat = n.latency_ms != null ? ` · ${n.latency_ms}ms` : '';
  const why = n.probe_error ? `（${escapeHtml(n.probe_error)}）` : '';
  if (n.state === 'online') {
    return `<span class="dot dot-on"></span>在线${escapeHtml(lat)}`;
  }
  if (n.state === 'degraded') {
    return `<span class="dot dot-warn"></span>可用性异常${why}`;
  }
  if (n.reachable) {
    return `<span class="dot dot-warn"></span>可达但不可用${why}`;
  }
  return `<span class="dot dot-off"></span>离线${why}`;
}

const NODE_ROLE_LABEL = { generic: '通用', series: '剧集', netdisk: '网盘' };

function renderNodes() {
  const rows = $('node-rows');
  const kw = state.nodeFilter.trim().toLowerCase();
  const items = kw
    ? state.nodes.filter((n) => `${n.alias} ${n.base_url}`.toLowerCase().includes(kw))
    : state.nodes;

  rows.innerHTML = items.map((n) => {
    const revealed = state.revealed.has(n.node_id);
    const pwCell = n.password_set
      ? (revealed
        ? `<span class="pw-shown">${escapeHtml(n.password)}</span>`
        : '<span class="muted">••••••</span>')
      : '<span class="muted">未设置</span>';
    const pwBtn = n.password_set
      ? `<button class="icon-btn" data-act="eye" data-id="${n.node_id}"
                 title="${revealed ? '隐藏密码' : '显示密码'}"
                 aria-label="${revealed ? '隐藏密码' : '显示密码'}">
           <svg viewBox="0 0 24 24" width="15" height="15" aria-hidden="true">
             <path d="M1.5 12s4-7 10.5-7 10.5 7 10.5 7-4 7-10.5 7S1.5 12 1.5 12Z"
                   fill="none" stroke="currentColor" stroke-width="1.6" />
             <circle cx="12" cy="12" r="3.2" fill="none" stroke="currentColor" stroke-width="1.6" />
           </svg>
         </button>`
      : '';
    const rate = n.max_rate_kbps > 0 ? `${n.max_rate_kbps} KB/s` : '不限';
    const role = NODE_ROLE_LABEL[n.role] || n.role;
    return `<tr>
      <td>
        ${escapeHtml(n.alias)}
        <span class="role-tag" title="角色：${escapeHtml(n.role)}">${escapeHtml(role)}</span>
      </td>
      <td class="mono">${escapeHtml(n.base_url)}</td>
      <td class="nowrap">${onlineCell(n)}</td>
      <td>${escapeHtml(n.user)}</td>
      <td class="nowrap">${pwCell} ${pwBtn}</td>
      <td>
        <div class="stack">
          <span>权重 ${escapeHtml(n.weight)}</span>
          <span class="sub">并发 ${escapeHtml(n.max_concurrent)} · 限速 ${escapeHtml(rate)}</span>
        </div>
      </td>
      <td class="nowrap">
        <label class="switch" title="调度开关：只影响是否派发新任务，不影响节点上正在运行的程序">
          <input type="checkbox" data-act="toggle" data-id="${n.node_id}" ${n.enabled ? 'checked' : ''} />
          <span>${n.enabled ? '启用' : '已关闭'}</span>
        </label>
      </td>
      <td class="nowrap">
        <button class="sm" data-act="edit" data-id="${n.node_id}">编辑</button>
        <button class="sm danger" data-act="del" data-id="${n.node_id}">删除</button>
      </td>
    </tr>`;
  }).join('');

  // 空状态要区分「一台都没有」和「筛选没命中」—— 两者该说的话不一样
  const empty = $('node-empty');
  empty.hidden = items.length > 0;
  if (!items.length && kw) empty.textContent = `没有匹配「${state.nodeFilter.trim()}」的节点。`;
  else empty.textContent = '暂无节点。点右上角「新增节点」把第一台下载机接进来。';
}

async function revealPassword(id) {
  if (state.revealed.has(id)) { state.revealed.delete(id); renderNodes(); return; }
  try {
    const data = await api(`/api/admin/nodes/${id}/secret`, { method: 'POST', body: {} });
    const node = state.nodes.find((n) => n.node_id === id);
    if (node) node.password = data.password;
    state.revealed.add(id);
    renderNodes();
  } catch (e) {
    showErr('node-error', `读取密码失败：${e.message}`);
  }
}

async function toggleNode(id, enabled) {
  showErr('node-error', null);
  try {
    await api(`/api/admin/nodes/${id}/enabled`, { method: 'POST', body: { enabled } });
    toast(enabled ? '已纳入调度' : '已停止向该节点派发新任务');
    loadNodes();
  } catch (e) {
    showErr('node-error', `切换调度开关失败：${e.message}`);
    loadNodes();
  }
}

// ── 节点表单弹窗 ─────────────────────────────────────────────────────────
function openNodeDialog(id) {
  state.editingNodeId = id ?? null;
  const isEdit = state.editingNodeId !== null;
  $('node-dialog-title').textContent = isEdit ? '编辑节点' : '新增节点';
  showErr('node-form-error', null);
  const n = isEdit ? state.nodes.find((x) => x.node_id === id) : null;
  $('nf-alias').value = n ? n.alias : '';
  $('nf-base-url').value = n ? n.base_url : '';
  $('nf-user').value = n ? n.user : 'admin';
  $('nf-password').value = '';
  $('nf-password').type = 'password';
  $('nf-password').placeholder = isEdit ? '留空 = 不修改；点右侧眼睛可查看当前密码' : '必填';
  $('nf-role').value = n ? n.role : 'generic';
  $('nf-weight').value = n ? n.weight : 1;
  $('nf-max-concurrent').value = n ? n.max_concurrent : 3;
  $('nf-max-rate').value = n ? n.max_rate_kbps : 0;
  $('nf-tags').value = n ? (n.tags || '') : '';
  $('nf-enabled').checked = n ? !!n.enabled : true;
  $('nf-eye').disabled = !isEdit;
  $('node-dialog').showModal();
}

async function submitNodeForm(ev) {
  ev.preventDefault();
  showErr('node-form-error', null);
  const isEdit = state.editingNodeId !== null;
  const pw = $('nf-password').value;
  const body = {
    alias: $('nf-alias').value.trim(),
    base_url: $('nf-base-url').value.trim(),
    user: $('nf-user').value.trim() || null,
    weight: Number($('nf-weight').value),
    max_concurrent: Number($('nf-max-concurrent').value),
    max_rate_kbps: Number($('nf-max-rate').value),
    role: $('nf-role').value,
    tags: $('nf-tags').value.trim(),
  };
  // 编辑时密码留空 = 不改动，因此**不把空串发上去**（后端虽也做了兜底，但两层都做更稳）。
  if (!isEdit || pw !== '') body.password = pw;

  const btn = $('node-form').querySelector('button[type="submit"]');
  btn.disabled = true;
  try {
    if (isEdit) {
      await api(`/api/admin/nodes/${state.editingNodeId}`, { method: 'PUT', body });
    } else {
      await api('/api/admin/nodes', { method: 'POST', body });
    }
    $('node-dialog').close();
    toast(isEdit ? '节点已保存' : '节点已添加');
    loadNodes();
  } catch (e) {
    showErr('node-form-error', `保存失败：${e.message}`);
  } finally {
    btn.disabled = false;
  }
}

// ── 删除节点弹窗 ─────────────────────────────────────────────────────────
let pendingDeleteNode = null;

function openNodeDeleteDialog(id) {
  pendingDeleteNode = id;
  const n = state.nodes.find((x) => x.node_id === id);
  $('nodedel-alias').textContent = n ? n.alias : `#${id}`;
  $('nodedel-confirm').value = '';
  showErr('nodedel-error', null);
  $('nodedel-dialog').showModal();
}

async function submitNodeDelete(ev) {
  ev.preventDefault();
  const n = state.nodes.find((x) => x.node_id === pendingDeleteNode);
  const typed = $('nodedel-confirm').value.trim();
  if (!n || typed !== n.alias) {
    showErr('nodedel-error', '输入的别名与节点别名不一致，已阻止删除。');
    return;
  }
  try {
    await api(`/api/admin/nodes/${pendingDeleteNode}`, { method: 'DELETE' });
    $('nodedel-dialog').close();
    pendingDeleteNode = null;
    toast('节点已删除');
    loadNodes();
  } catch (e) {
    showErr('nodedel-error', `删除失败：${e.message}`);
  }
}

// ═════════════════════════════ 任务 ═════════════════════════════
async function loadTasks() {
  showErr('task-error', null);
  const q = $('task-search').value.trim();
  try {
    const data = await api(`/api/admin/tasks${q ? `?q=${encodeURIComponent(q)}` : ''}`);
    const items = data.items || [];
    $('task-rows').innerHTML = items.map((t) => `
      <tr>
        <td class="nowrap"><span class="badge" data-state="${escapeHtml(t.internal_state)}">${escapeHtml(t.internal_state)}</span></td>
        <td>${escapeHtml(t.name)}</td>
        <td>${escapeHtml(t.kind)}</td>
        ${progressCell(t)}
        <td class="nowrap muted">${escapeHtml(fmtTime(t.created_at))}</td>
        <td class="nowrap">
          <button class="sm" data-act="pause" data-id="${escapeHtml(t.task_id)}">暂停</button>
          <button class="sm" data-act="unpause" data-id="${escapeHtml(t.task_id)}">恢复</button>
          <button class="sm" data-act="retry" data-id="${escapeHtml(t.task_id)}">重试</button>
          <button class="sm danger" data-act="del" data-id="${escapeHtml(t.task_id)}">删除</button>
        </td>
      </tr>`).join('');
    $('task-empty').hidden = items.length > 0;
  } catch (e) {
    showErr('task-error', `加载任务失败：${e.message}`);
  }
}

// 进度条：网关只给 `permillage`（0–1000），原来直接显示「400‰」——
// 千分号对人不友好，且看不出还剩多少。这里换成百分比 + 条。
// 宽度来自 Number() 计算后的数值，不含任何用户输入，直接进 style 安全。
function progressCell(t) {
  const pm = Number(t.permillage);
  const permille = Number.isFinite(pm) ? Math.min(1000, Math.max(0, pm)) : 0;
  const pct = permille / 10;
  const st = String(t.internal_state || '');
  const cls = st === 'completed' ? ' done' : (st === 'failed' || st === 'error' ? ' failed' : '');
  return `<td>
    <div class="prog-cell" title="${permille}‰">
      <div class="prog" role="progressbar" aria-valuenow="${pct.toFixed(1)}"
           aria-valuemin="0" aria-valuemax="100" aria-label="下载进度">
        <div class="prog-bar${cls}" style="width:${pct.toFixed(1)}%"></div>
      </div>
      <span class="prog-num">${pct.toFixed(1)}%</span>
    </div>
  </td>`;
}

async function actOnTask(id, action) {
  showErr('task-error', null);
  try {
    await api(`/api/admin/tasks/${encodeURIComponent(id)}/${action}`, { method: 'POST', body: {} });
    const label = { pause: '已暂停', unpause: '已恢复', retry: '已重新入队' }[action] || '已完成';
    toast(label);
    loadTasks();
  } catch (e) {
    showErr('task-error', `操作失败：${e.message}`);
  }
}

function openTaskDeleteDialog(id) {
  const tr = document.querySelector(`#task-rows button[data-id="${CSS.escape(id)}"]`);
  const name = tr ? tr.closest('tr').children[1].textContent : id;
  state.pendingTask = id;
  $('taskdel-name').textContent = name;
  $('taskdel-files').checked = false; // 默认不勾：安全默认
  showErr('taskdel-error', null);
  $('taskdel-dialog').showModal();
}

async function submitTaskDelete(ev) {
  ev.preventDefault();
  try {
    await api(`/api/admin/tasks/${encodeURIComponent(state.pendingTask)}/remove`, {
      method: 'POST',
      body: { delete_files: $('taskdel-files').checked },
    });
    $('taskdel-dialog').close();
    state.pendingTask = null;
    toast('任务已删除');
    loadTasks();
  } catch (e) {
    showErr('taskdel-error', `删除失败：${e.message}`);
  }
}

// 自动刷新：只盯进度时才有意义，因此只在「任务」页且勾选时跑。
// 离开页面必须停表 —— 否则后台每 5 秒打一次接口，白烧 CPU 还刷日志。
let autoTimer = null;

function syncAutoRefresh() {
  const on = $('task-auto').checked && state.view === 'tasks';
  if (autoTimer) { clearInterval(autoTimer); autoTimer = null; }
  if (on) {
    autoTimer = setInterval(() => { if (state.view === 'tasks') loadTasks(); }, 5000);
  }
}

// ═════════════════════════════ 告警（event_log 的读入口）════════════════════
//
// 只读。翻页用 id 游标（`before`）而不是页码：`event_log` 一直在追加，
// offset 分页在翻页时会重复或漏行（见 events.go 顶部说明）。
const EVENT_LEVEL_LABEL = { info: '信息', warn: '警告', error: '错误' };

async function loadEvents(reset) {
  showErr('event-error', null);
  if (reset) {
    state.events = [];
    state.eventsCursor = 0;
  }
  const q = new URLSearchParams();
  const level = $('event-level').value;
  const category = $('event-category').value;
  if (level) q.set('level', level);
  if (category) q.set('category', category);
  if (!reset && state.eventsCursor) q.set('before', String(state.eventsCursor));
  q.set('limit', '100');
  try {
    const data = await api(`/api/admin/events?${q}`);
    state.events = state.events.concat(data.items || []);
    state.eventsCursor = data.next_before || 0;
    renderEvents();
  } catch (e) {
    showErr('event-error', `加载告警失败：${e.message}`);
  }
}

function renderEvents() {
  const idOrDash = (v) => (v == null ? '—' : escapeHtml(v));
  $('event-rows').innerHTML = state.events.map((e) => {
    const detail = e.detail || '';
    return `<tr>
      <td class="nowrap muted">${escapeHtml(fmtTime(e.created_at))}</td>
      <td class="nowrap"><span class="lvl lvl-${escapeHtml(e.level)}">${escapeHtml(EVENT_LEVEL_LABEL[e.level] || e.level)}</span></td>
      <td class="nowrap">${escapeHtml(e.category)}</td>
      <td class="nowrap">${idOrDash(e.node_id)}</td>
      <td class="nowrap mono">${idOrDash(e.task_id)}</td>
      <td>${escapeHtml(e.message)}</td>
      <td class="mono detail-cell" title="${escapeHtml(detail)}">${escapeHtml(detail)}</td>
    </tr>`;
  }).join('');
  $('event-empty').hidden = state.events.length > 0;
  // 游标为 0 = 服务端已无更早的行。这时隐藏按钮，而不是留着让人点出空结果。
  $('event-more-box').hidden = !state.eventsCursor;
}

// ═════════════════════════════ 设置（派发策略）═════════════════════════════
async function loadPolicies() {
  showErr('policy-error', null);
  hideOk('policy-ok');
  try {
    const data = await api('/api/admin/config');
    state.policies = data.policies || [];
    if (data.priority && data.priority.step) state.prioStep = data.priority.step;
    // 逐条统计而不是写死数字：策略增删后这里不会变成假话。
    const total = state.policies.length;
    const eff = state.policies.filter((p) => p.effective).length;
    // 以前这里写「全部已接入调度内核 —— 全部接入调度内核 ——」（前缀和后缀重复了一遍），
    // 读起来像结巴。前后两段各说一件事：先给比例，再说「保存后什么时候生效」。
    const tail = total === eff
      ? '保存后下一个调度周期（约 1 秒）即按新值选点，不需要重启。'
      : `其余 ${total - eff} 条暂未生效，鼠标悬停行末「未生效」可看具体原因。`;
    $('policy-note').textContent = total ? `${eff} / ${total} 条已接入调度内核。${tail}` : '';
    renderPolicyTable();
    renderEnv(data.env || []);
  } catch (e) {
    showErr('policy-error', `加载设置失败：${e.message}`);
  }
}

function renderPolicyTable() {
  const rows = $('policy-rows');
  rows.innerHTML = state.policies.map((p, idx) => `
    <tr draggable="true" data-idx="${idx}" data-key="${escapeHtml(p.key)}">
      <td class="drag-col"><span class="handle" title="拖动调整优先级">⋮⋮</span></td>
      <td>
        <div>${escapeHtml(p.name)}</div>
        <div class="muted mono">${escapeHtml(p.key)}</div>
      </td>
      <td class="desc">${escapeHtml(p.description)}</td>
      <td class="nowrap">${escapeHtml(p.category)}</td>
      <td class="prio-col"><span class="prio">${escapeHtml(p.priority)}</span></td>
      <td>
        <label class="switch">
          <input type="checkbox" data-role="enable" data-idx="${idx}"
                 ${p.enabled ? 'checked' : ''} ${p.can_disable ? '' : 'disabled'} />
          <span>${p.enabled ? '开' : '关'}</span>
        </label>
        ${p.can_disable ? '' : '<span class="lock-tag">不可关闭</span>'}
      </td>
      <td class="nowrap${p.effective ? ' eff-on' : ' muted'}"
          title="${escapeHtml(p.effective_note || '')}">${p.effective ? '已生效' : '未生效'}</td>
    </tr>`).join('');
  bindPolicyDrag(rows);
}

function renderEnv(items) {
  $('env-rows').innerHTML = items.map((it) => `
    <tr>
      <td class="mono">${escapeHtml(it.key)}</td>
      <td class="mono">${escapeHtml(it.value)}</td>
      <td><span class="risk risk-${escapeHtml(it.risk)}">${escapeHtml(it.risk)}</span></td>
      <td class="desc">${escapeHtml(it.description)}</td>
    </tr>`).join('');
}

let dragIdx = null;

function bindPolicyDrag(tbody) {
  tbody.querySelectorAll('tr[data-idx]').forEach((tr) => {
    tr.addEventListener('dragstart', (e) => {
      dragIdx = Number(tr.dataset.idx);
      tr.classList.add('dragging');
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData('text/plain', tr.dataset.idx); // Firefox 必须设 data 才触发 drag
    });
    tr.addEventListener('dragend', () => tr.classList.remove('dragging'));
    tr.addEventListener('dragover', (e) => {
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
    });
    tr.addEventListener('drop', (e) => {
      e.preventDefault();
      const to = Number(tr.dataset.idx);
      if (dragIdx === null || dragIdx === to) return;
      const moved = state.policies.splice(dragIdx, 1)[0];
      state.policies.splice(to, 0, moved);
      dragIdx = null;
      reassignPriorities();
      renderPolicyTable();
    });
  });
}

/// 行序即优先级：从上到下依次 10 / 20 / 30…（越小越优先）。
function reassignPriorities() {
  state.policies.forEach((p, i) => { p.priority = (i + 1) * state.prioStep; });
}

function readPolicyForm() {
  const out = state.policies.map((p, idx) => {
    const cb = document.querySelector(`#policy-rows input[data-role="enable"][data-idx="${idx}"]`);
    return {
      key: p.key,
      enabled: cb ? cb.checked : p.enabled,
      priority: p.priority,
    };
  });
  return { policies: out };
}

async function savePolicies() {
  showErr('policy-error', null);
  hideOk('policy-ok');
  const btn = $('policy-save');
  btn.disabled = true;
  try {
    const data = await api('/api/admin/config', { method: 'PUT', body: readPolicyForm() });
    state.policies = data.policies || [];
    renderPolicyTable();
    // 之前这里写的是「策略要等调度内核落地后才会真正影响派发」——
    // 调度内核早在 B1 就落地了，那句话会让人以为保存无效。
    ok('policy-ok', '已保存并立即生效：下一个调度周期（约 1 秒）就会按新优先级选点，不需要重启。');
    loadPolicies();
  } catch (e) {
    showErr('policy-error', `保存失败：${e.message}`);
  } finally {
    btn.disabled = false;
  }
}

// ── 管理口令（用户可自行设置）──────────────────────────────────────────────
//
// 只走两个接口：GET 读「来源 + 上次修改时刻」，POST 提交 {current, new}。
// 口令从不出现在 URL / localStorage 里，输入框在提交成功后才清空。
async function loadPasswordStatus() {
  showErr('pw-error', null);
  hideOk('pw-ok');
  try {
    const d = await api('/api/admin/password');
    // 来源说明必须写准：改过一次后，环境变量里的初始口令就**不再有效**。
    $('pw-note').textContent = d.customized
      ? `当前口令由你在管理台设置（${d.min_length}–${d.max_length} 个字符），已落库；`
        + `上次修改：${fmtTime(d.updated_at)}。部署时环境变量里的初始口令已不再生效。`
      : `当前口令来自部署环境（环境变量或启动时随机生成），尚未在管理台改过。`
        + `改过之后，环境变量里的初始口令将不再生效。`;
  } catch (e) {
    showErr('pw-error', `读取口令状态失败：${e.message}`);
  }
}

async function savePassword() {
  showErr('pw-error', null);
  hideOk('pw-ok');
  const current = $('pw-current').value;
  const next = $('pw-new').value;
  const confirm = $('pw-confirm').value;
  if (!current) { showErr('pw-error', '请输入当前口令'); return; }
  if (next !== confirm) { showErr('pw-error', '两次输入的新口令不一致'); return; }
  try {
    const d = await api('/api/admin/password', {
      method: 'POST',
      body: { current_password: current, new_password: next },
    });
    // 成功后才清空，失败保留输入（用户不必重打一遍）。
    $('pw-current').value = '';
    $('pw-new').value = '';
    $('pw-confirm').value = '';
    ok('pw-ok', d.sessions_revoked
      ? '口令已修改，其它已登录会话已失效。'
      : '口令已修改并立即生效。其它已登录的浏览器仍保持登录（按当前策略不强制下线）。');
    loadPasswordStatus();
  } catch (e) {
    showErr('pw-error', `修改失败：${e.message}`);
  }
}

function ok(id, msg) { const el = $(id); el.textContent = msg; el.hidden = false; }
function hideOk(id) { $(id).hidden = true; }

// ═════════════════════════════ 登录 / 初始化 ═════════════════════════════
async function doLogin(ev) {
  ev.preventDefault();
  showErr('login-error', null);
  const btn = $('login-form').querySelector('button[type="submit"]');
  btn.disabled = true;
  try {
    await api('/api/admin/login', {
      method: 'POST',
      body: {
        username: $('login-username').value.trim(),
        password: $('login-password').value,
      },
    });
    $('login-password').value = '';
    show('overview');
  } catch (e) {
    showErr('login-error', e.message);
    $('login-password').select();
  } finally {
    btn.disabled = false;
  }
}

async function doLogout() {
  try { await api('/api/admin/logout', { method: 'POST', body: {} }); } catch { /* ignore */ }
  state.revealed.clear();
  show('login');
}

// 忘记口令：向服务端要一串一次性临时口令。
//
// ⚠️ 口令**不会**出现在这个页面的响应里 —— 服务端只把它写进日志，
// 用户去宿主机 `docker logs <容器> | grep temp_password` 取。
// 页面能做的只有「确认生成成功 + 告诉用户去哪看」，绝不能假装拿到了口令
// （把口令回传就等于这个匿名接口可以给任何人发管理口令）。
async function requestTempPassword() {
  const btn = $('reset-request-btn');
  const out = $('reset-request-result');
  btn.disabled = true;
  out.hidden = true;
  try {
    const d = await api('/api/admin/password/reset-request', { method: 'POST', body: {} });
    const minutes = Math.max(1, Math.round((d.expires_in || 600) / 60));
    out.hidden = false;
    out.textContent = `已生成，${minutes} 分钟内有效。请在宿主机执行：${d.log_hint}`
      + '（原口令仍然可用；改完口令后临时口令立即失效）';
  } catch (e) {
    out.hidden = false;
    out.textContent = `生成失败：${e.message}`;
  } finally {
    btn.disabled = false;
  }
}

function init() {
  document.querySelectorAll('.nav-btn[data-view]').forEach((b) => {
    b.addEventListener('click', () => show(b.dataset.view));
  });
  window.addEventListener('popstate', () => show(VIEWS[location.pathname] || 'overview'));

  $('banner-close').addEventListener('click', () => { $('banner').hidden = true; });

  $('logout-btn').addEventListener('click', doLogout);
  $('login-form').addEventListener('submit', doLogin);
  $('reset-request-btn').addEventListener('click', requestTempPassword);

  // 总览
  $('overview-refresh').addEventListener('click', loadOverview);

  // 节点
  $('node-refresh').addEventListener('click', loadNodes);
  $('node-new').addEventListener('click', () => openNodeDialog(null));
  $('node-search').addEventListener('input', () => {
    state.nodeFilter = $('node-search').value;
    renderNodes();
  });
  $('node-form').addEventListener('submit', submitNodeForm);
  $('nf-cancel').addEventListener('click', () => $('node-dialog').close());
  $('nodedel-form').addEventListener('submit', submitNodeDelete);
  $('nodedel-cancel').addEventListener('click', () => $('nodedel-dialog').close());
  $('nf-eye').addEventListener('click', async () => {
    const input = $('nf-password');
    if (input.type === 'text') { input.type = 'password'; return; }
    if (state.editingNodeId === null) return;
    try {
      const data = await api(`/api/admin/nodes/${state.editingNodeId}/secret`, { method: 'POST', body: {} });
      input.value = data.password;
      input.type = 'text';
    } catch (e) {
      showErr('node-form-error', `读取密码失败：${e.message}`);
    }
  });
  $('node-rows').addEventListener('click', (ev) => {
    const btn = ev.target.closest('button[data-act]');
    if (!btn) return;
    const id = Number(btn.dataset.id);
    if (btn.dataset.act === 'eye') revealPassword(id);
    if (btn.dataset.act === 'edit') openNodeDialog(id);
    if (btn.dataset.act === 'del') openNodeDeleteDialog(id);
  });
  $('node-rows').addEventListener('change', (ev) => {
    const cb = ev.target.closest('input[data-act="toggle"]');
    if (cb) toggleNode(Number(cb.dataset.id), cb.checked);
  });

  // 任务
  $('task-refresh').addEventListener('click', loadTasks);
  $('task-search').addEventListener('input', loadTasks);
  $('task-auto').addEventListener('change', syncAutoRefresh);
  $('task-rows').addEventListener('click', (ev) => {
    const btn = ev.target.closest('button[data-act]');
    if (!btn) return;
    if (btn.dataset.act === 'del') openTaskDeleteDialog(btn.dataset.id);
    else actOnTask(btn.dataset.id, btn.dataset.act);
  });
  $('taskdel-form').addEventListener('submit', submitTaskDelete);
  $('taskdel-cancel').addEventListener('click', () => $('taskdel-dialog').close());

  // 告警
  $('event-refresh').addEventListener('click', () => loadEvents(true));
  $('event-level').addEventListener('change', () => loadEvents(true));
  $('event-category').addEventListener('change', () => loadEvents(true));
  $('event-more').addEventListener('click', () => loadEvents(false));

  // 设置
  $('policy-refresh').addEventListener('click', loadPolicies);
  $('policy-save').addEventListener('click', savePolicies);
  $('pw-save').addEventListener('click', savePassword);

  // 启动：先探测会话（summary），成功则进入目标页，否则回登录。
  // 顺手用这次的 summary 把横幅写实 —— 否则直接打开 /nodes 时横幅会一直停在
  // 「正在读取调度状态…」（loadOverview 根本不会跑）。
  //
  // 目标页本身就是 /login 时不做探测：那一页的含义就是"我要登录"，
  // 探测只会换来一个 401 和一条控制台红字。
  const start = VIEWS[location.pathname] || 'overview';
  if (start === 'login') {
    show('login');
    return;
  }
  api('/api/admin/summary')
    .then((d) => {
      if (d) renderBanner(!!d.scheduler_enabled);
      show(start);
    })
    .catch(() => show('login'));
}

init();
