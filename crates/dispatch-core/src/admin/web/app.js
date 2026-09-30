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
//   · 导航里只有 总览 / 节点 / 任务 / 设置 —— 没有「添加」。
// ─────────────────────────────────────────────────────────────────────────────

const VIEWS = {
  '/': 'overview',
  '/login': 'login',
  '/nodes': 'nodes',
  '/tasks': 'tasks',
  '/settings': 'settings',
};
const PATHS = {
  login: '/login',
  overview: '/',
  nodes: '/nodes',
  tasks: '/tasks',
  settings: '/settings',
};

const state = {
  view: 'overview',
  nodes: [],
  policies: [],
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

function showErr(id, msg) {
  const el = $(id);
  if (!el) return;
  if (msg) { el.textContent = msg; el.hidden = false; } else { el.hidden = true; }
}

// ── 视图切换 ────────────────────────────────────────────────────────────────
function show(view) {
  state.view = view;
  for (const name of ['login', 'overview', 'nodes', 'tasks', 'settings']) {
    $(`view-${name}`).hidden = name !== view;
  }
  $('banner').hidden = view === 'login';
  document.querySelectorAll('.nav-btn[data-view]').forEach((b) => {
    b.classList.toggle('active', b.dataset.view === view);
  });
  if (PATHS[view] && location.pathname !== PATHS[view]) {
    history.pushState({ view }, '', PATHS[view]);
  }
  if (view === 'overview') loadOverview();
  if (view === 'nodes') loadNodes();
  if (view === 'tasks') loadTasks();
  if (view === 'settings') loadPolicies();
}

// ═════════════════════════════ 总览 ═════════════════════════════
async function loadOverview() {
  const grid = $('overview-grid');
  showErr('overview-error', null);
  try {
    const data = await api('/api/admin/summary');
    const nodeCount = Array.isArray(data.nodes) ? data.nodes.length : '—';
    grid.innerHTML = [
      ['版本', data.version],
      ['运行时长', `${data.uptime_seconds}s`],
      ['任务总数', data.task_total],
      ['节点数', nodeCount],
      ['对外口', data.ports.public],
      ['管理口', data.ports.admin],
      ['数据库', data.db_path],
      ['调度内核', data.scheduler_enabled ? '已启用' : '未启用（任务只排队）'],
    ].map(([k, v]) => `<div class="card"><div class="muted">${escapeHtml(k)}</div><div>${escapeHtml(v)}</div></div>`).join('');
    const counts = data.task_counts || [];
    $('overview-counts').innerHTML = counts.length
      ? counts.map((c) => `<span class="badge">${escapeHtml(c.state)}: ${escapeHtml(c.count)}</span>`).join('')
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

function onlineCell(n) {
  if (n.online) {
    const code = n.http_code ? ` · HTTP ${n.http_code}` : '';
    const ms = n.latency_ms != null ? ` · ${n.latency_ms}ms` : '';
    return `<span class="dot dot-on"></span>在线${escapeHtml(code + ms)}`;
  }
  const why = n.probe_error ? `（${escapeHtml(n.probe_error)}）` : '';
  return `<span class="dot dot-off"></span>离线${why}`;
}

function renderNodes() {
  const rows = $('node-rows');
  rows.innerHTML = state.nodes.map((n) => {
    const revealed = state.revealed.has(n.node_id);
    const pwCell = n.password_set
      ? (revealed
        ? `<span class="pw-shown">${escapeHtml(n.password)}</span>`
        : '<span class="muted">••••••</span>')
      : '<span class="muted">未设置</span>';
    const pwBtn = n.password_set
      ? `<button class="icon-btn" data-act="eye" data-id="${n.node_id}" title="${revealed ? '隐藏密码' : '显示密码'}">
           <svg viewBox="0 0 24 24" width="15" height="15" aria-hidden="true">
             <path d="M1.5 12s4-7 10.5-7 10.5 7 10.5 7-4 7-10.5 7S1.5 12 1.5 12Z"
                   fill="none" stroke="currentColor" stroke-width="1.6" />
             <circle cx="12" cy="12" r="3.2" fill="none" stroke="currentColor" stroke-width="1.6" />
           </svg>
         </button>`
      : '';
    const rate = n.max_rate_kbps > 0 ? `${n.max_rate_kbps} KB/s` : '不限';
    return `<tr>
      <td>${escapeHtml(n.alias)}</td>
      <td class="mono">${escapeHtml(n.base_url)}</td>
      <td class="nowrap">${onlineCell(n)}</td>
      <td>${escapeHtml(n.user)}</td>
      <td class="nowrap">${pwCell} ${pwBtn}</td>
      <td>${escapeHtml(n.role)}</td>
      <td>${escapeHtml(n.weight)}</td>
      <td>${escapeHtml(n.max_concurrent)}</td>
      <td class="nowrap">${escapeHtml(rate)}</td>
      <td class="nowrap">
        <label class="switch" title="调度开关：只影响是否派发新任务，不影响节点上正在运行的程序">
          <input type="checkbox" data-act="toggle" data-id="${n.node_id}" ${n.enabled ? 'checked' : ''} />
          <span>${n.enabled ? '启用' : '已关闭'}</span>
        </label>
      </td>
      <td class="nowrap">
        <button data-act="edit" data-id="${n.node_id}">编辑</button>
        <button data-act="del" data-id="${n.node_id}" class="danger">删除</button>
      </td>
    </tr>`;
  }).join('');
  $('node-empty').hidden = state.nodes.length > 0;
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

  try {
    if (isEdit) {
      await api(`/api/admin/nodes/${state.editingNodeId}`, { method: 'PUT', body });
    } else {
      await api('/api/admin/nodes', { method: 'POST', body });
    }
    $('node-dialog').close();
    loadNodes();
  } catch (e) {
    showErr('node-form-error', `保存失败：${e.message}`);
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
        <td class="nowrap"><span class="badge">${escapeHtml(t.internal_state)}</span></td>
        <td>${escapeHtml(t.name)}</td>
        <td>${escapeHtml(t.kind)}</td>
        <td>${escapeHtml(t.permillage)}‰</td>
        <td class="nowrap">${escapeHtml(fmtTime(t.created_at))}</td>
        <td class="nowrap">
          <button data-act="pause" data-id="${escapeHtml(t.task_id)}">暂停</button>
          <button data-act="unpause" data-id="${escapeHtml(t.task_id)}">恢复</button>
          <button data-act="retry" data-id="${escapeHtml(t.task_id)}">重试</button>
          <button data-act="del" data-id="${escapeHtml(t.task_id)}" class="danger">删除</button>
        </td>
      </tr>`).join('');
    $('task-empty').hidden = items.length > 0;
  } catch (e) {
    showErr('task-error', `加载任务失败：${e.message}`);
  }
}

async function actOnTask(id, action) {
  showErr('task-error', null);
  try {
    await api(`/api/admin/tasks/${encodeURIComponent(id)}/${action}`, { method: 'POST', body: {} });
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
    loadTasks();
  } catch (e) {
    showErr('taskdel-error', `删除失败：${e.message}`);
  }
}

// ═════════════════════════════ 设置（派发策略）═════════════════════════════
async function loadPolicies() {
  showErr('policy-error', null);
  hideOk('policy-ok');
  try {
    const data = await api('/api/admin/config');
    state.policies = data.policies || [];
    if (data.priority && data.priority.step) state.prioStep = data.priority.step;
    $('policy-note').textContent = data.effective_note || '';
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
      <td class="nowrap muted">未生效</td>
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
  try {
    const data = await api('/api/admin/config', { method: 'PUT', body: readPolicyForm() });
    state.policies = data.policies || [];
    renderPolicyTable();
    ok('policy-ok', '已保存。注意：策略要等调度内核落地后才会真正影响派发。');
  } catch (e) {
    showErr('policy-error', `保存失败：${e.message}`);
  }
}

function ok(id, msg) { const el = $(id); el.textContent = msg; el.hidden = false; }
function hideOk(id) { $(id).hidden = true; }

// ═════════════════════════════ 登录 / 初始化 ═════════════════════════════
async function doLogin(ev) {
  ev.preventDefault();
  showErr('login-error', null);
  try {
    await api('/api/admin/login', { method: 'POST', body: { password: $('login-password').value } });
    $('login-password').value = '';
    show('overview');
  } catch (e) {
    showErr('login-error', e.message);
  }
}

async function doLogout() {
  try { await api('/api/admin/logout', { method: 'POST', body: {} }); } catch { /* ignore */ }
  state.revealed.clear();
  show('login');
}

function init() {
  document.querySelectorAll('.nav-btn[data-view]').forEach((b) => {
    b.addEventListener('click', () => show(b.dataset.view));
  });
  window.addEventListener('popstate', () => show(VIEWS[location.pathname] || 'overview'));

  $('logout-btn').addEventListener('click', doLogout);
  $('login-form').addEventListener('submit', doLogin);

  // 节点
  $('node-refresh').addEventListener('click', loadNodes);
  $('node-new').addEventListener('click', () => openNodeDialog(null));
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
  $('task-rows').addEventListener('click', (ev) => {
    const btn = ev.target.closest('button[data-act]');
    if (!btn) return;
    if (btn.dataset.act === 'del') openTaskDeleteDialog(btn.dataset.id);
    else actOnTask(btn.dataset.id, btn.dataset.act);
  });
  $('taskdel-form').addEventListener('submit', submitTaskDelete);
  $('taskdel-cancel').addEventListener('click', () => $('taskdel-dialog').close());

  // 设置
  $('policy-refresh').addEventListener('click', loadPolicies);
  $('policy-save').addEventListener('click', savePolicies);

  // 启动：先探测会话（summary），成功则进入目标页，否则回登录。
  const start = VIEWS[location.pathname] || 'overview';
  api('/api/admin/summary')
    .then(() => show(start === 'login' ? 'overview' : start))
    .catch(() => show('login'));
}

init();
