// 下载网关管理台：原生 ES 模块（零构建）。3 屏 + 信封解包 + 行内干预 + 二次确认。

const state = { view: 'overview' };

// 统一请求：解 {code,data,message} 信封；非 0 抛错。
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
  try { payload = await res.json(); } catch { /* 非 JSON */ }
  if (!res.ok || (payload && payload.code !== 0)) {
    const msg = payload && payload.message ? payload.message : `HTTP ${res.status}`;
    throw new Error(msg);
  }
  return payload ? payload.data : null;
}

function $(id) { return document.getElementById(id); }

function show(view) {
  state.view = view;
  for (const name of ['login', 'overview', 'tasks', 'new']) {
    $(`view-${name}`).hidden = name !== view;
  }
  if (view === 'overview') loadOverview();
  if (view === 'tasks') loadTasks();
}

function fmtTime(ms) {
  if (!ms) return '—';
  return new Date(ms).toLocaleString();
}

function statusBadge(row) {
  // 已受理·未下发（本轮无节点）
  return `<span class="badge">已受理·未下发</span> <span>${row.internal_state}</span>`;
}

async function loadOverview() {
  const grid = $('overview-grid');
  const counts = $('overview-counts');
  try {
    const data = await api('/api/admin/summary');
    grid.innerHTML = [
      ['版本', data.version],
      ['运行时长', `${data.uptime_seconds}s`],
      ['任务总数', data.task_total],
      ['对外口', data.ports.public],
      ['管理口', data.ports.admin],
      ['数据库', data.db_path],
      ['调度器', data.scheduler_enabled ? '启用' : '未启用'],
      ['节点数', (data.nodes || []).length],
    ].map(([k, v]) => `<div class="card"><div class="muted">${k}</div><div>${v}</div></div>`).join('');
    counts.innerHTML = (data.task_counts || [])
      .map((c) => `<span class="badge">${c.state}: ${c.count}</span>`)
      .join('') || '<span class="badge">无任务</span>';
    $('login-error').hidden = true;
  } catch (e) {
    grid.innerHTML = `<div class="error-bar">加载总览失败：${e.message}</div>`;
  }
}

async function loadTasks() {
  const rows = $('task-rows');
  const err = $('task-error');
  err.hidden = true;
  const q = $('task-search').value.trim();
  try {
    const data = await api(`/api/admin/tasks${q ? `?q=${encodeURIComponent(q)}` : ''}`);
    rows.innerHTML = (data.items || []).map((t) => `
      <tr>
        <td>${statusBadge(t)}</td>
        <td>${escapeHtml(t.name)}</td>
        <td>${t.kind}</td>
        <td>${t.permillage}‰</td>
        <td>${fmtTime(t.created_at)}</td>
        <td>
          <button data-act="pause" data-id="${t.task_id}">暂停</button>
          <button data-act="unpause" data-id="${t.task_id}">继续</button>
          <button data-act="remove" data-id="${t.task_id}" class="danger">移除</button>
        </td>
      </tr>`).join('');
    $('task-empty').hidden = (data.items || []).length > 0;
  } catch (e) {
    err.textContent = `加载任务失败：${e.message}`;
    err.hidden = false;
  }
}

function escapeHtml(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}

async function actOnTask(id, action) {
  if (action === 'remove' && !confirm('确认移除该任务记录？（不会删除节点文件）')) return;
  try {
    await api(`/api/admin/tasks/${id}/${action}`, { method: 'POST', body: {} });
    loadTasks();
  } catch (e) {
    const err = $('task-error');
    err.textContent = `操作失败：${e.message}`;
    err.hidden = false;
  }
}

async function doLogin(ev) {
  ev.preventDefault();
  const errBox = $('login-error');
  errBox.hidden = true;
  try {
    await api('/api/admin/login', { method: 'POST', body: { password: $('login-password').value } });
    show('overview');
  } catch (e) {
    errBox.textContent = e.message;
    errBox.hidden = false;
  }
}

async function doCreate(ev) {
  ev.preventDefault();
  const ok = $('create-result');
  const err = $('create-error');
  ok.hidden = true;
  err.hidden = true;
  const body = {
    url: $('create-url').value.trim(),
    kind: $('create-kind').value,
    filename: $('create-filename').value.trim() || null,
  };
  try {
    const data = await api('/api/admin/tasks', { method: 'POST', body });
    ok.textContent = `已受理：task_id=${data.task_id} gid=${data.gid}`;
    ok.hidden = false;
    $('create-url').value = '';
    $('create-filename').value = '';
  } catch (e) {
    err.textContent = `受理失败：${e.message}`;
    err.hidden = false;
  }
}

async function doLogout() {
  try { await api('/api/admin/logout', { method: 'POST', body: {} }); } catch { /* ignore */ }
  show('login');
}

function init() {
  document.querySelectorAll('.nav-btn[data-view]').forEach((b) => {
    b.addEventListener('click', () => show(b.dataset.view));
  });
  $('logout-btn').addEventListener('click', doLogout);
  $('login-form').addEventListener('submit', doLogin);
  $('create-form').addEventListener('submit', doCreate);
  $('task-refresh').addEventListener('click', loadTasks);
  $('task-search').addEventListener('input', () => loadTasks());
  $('task-rows').addEventListener('click', (ev) => {
    const btn = ev.target.closest('button[data-act]');
    if (btn) actOnTask(btn.dataset.id, btn.dataset.act);
  });
  // 启动：先探测会话（summary），成功则进总览，否则登录。
  api('/api/admin/summary').then(() => show('overview')).catch(() => show('login'));
}

init();
