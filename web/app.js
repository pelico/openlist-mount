// 简易封装
async function req(path, opts) {
  opts = opts || {};
  const res = await fetch(path, {
    method: opts.method || 'GET',
    headers: Object.assign({'Content-Type': 'application/json'}, opts.headers || {}),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const text = await res.text();
  let data;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) throw new Error(typeof data === 'string' ? data : (data && data.error) || res.statusText);
  return data;
}

function fmtTime(t) {
  if (!t) return '-';
  const d = new Date(t);
  if (isNaN(d)) return '-';
  return d.toLocaleString();
}

async function checkRclone() {
  const el = document.getElementById('rclone-status');
  try {
    const r = await req('/api/rclone/check');
    if (r.installed) {
      el.textContent = 'rclone ' + (r.version || '已安装');
      el.className = 'rclone-badge good';
    } else {
      el.textContent = 'rclone 未安装';
      el.className = 'rclone-badge bad';
    }
  } catch (e) {
    el.textContent = 'rclone 状态未知';
    el.className = 'rclone-badge bad';
  }
}

async function loadList() {
  const listEl = document.getElementById('list');
  listEl.innerHTML = '<p class="hint">加载中…</p>';
  try {
    const items = await req('/api/mounts');
    if (!items.length) {
      listEl.innerHTML = '<p class="hint">还没有挂载项,使用上方表单添加。</p>';
      return;
    }
    listEl.innerHTML = items.map(renderItem).join('');
    bindItemActions();
  } catch (e) {
    listEl.innerHTML = '<p class="hint">加载失败: ' + e.message + '</p>';
  }
}

function renderItem(it) {
  const c = it.config;
  const s = it.status;
  const cls = s.running ? 'run' : (s.last_error ? 'err' : 'stop');
  const label = s.running ? '运行中' : (s.last_error ? '错误' : '已停止');
  const toggleBtn = s.running
    ? `<button data-act="stop" data-id="${c.id}" class="ghost">停止</button>`
    : `<button data-act="start" data-id="${c.id}" class="primary">启动</button>`;
  return `
  <div class="item">
    <div class="item-head">
      <div>
        <span class="item-name">${escapeHtml(c.name)}</span>
        <span class="badge ${cls}">${label}</span>
      </div>
      <div class="item-actions">
        ${toggleBtn}
        <button data-act="log" data-id="${c.id}" class="ghost">日志</button>
        <button data-act="del" data-id="${c.id}" class="danger">删除</button>
      </div>
    </div>
    <div class="item-meta">
      URL: ${escapeHtml(c.url)}<br>
      挂载点: ${escapeHtml(c.mountpoint)}
      ${s.running ? ` · PID: ${s.pid} · 启动于 ${fmtTime(s.started_at)}` : ''}
      ${s.last_error ? ` · 错误: ${escapeHtml(s.last_error)}` : ''}
      <br>目录缓存 ${escapeHtml(c.dir_cache || '24h')} · 属性缓存 ${escapeHtml(c.attr_time || '1h')} · 缓存模式 ${escapeHtml(c.vfs_cache_mode || 'off')}
      ${c.allow_other ? ' · 允许其他用户' : ''}
      ${c.auto_start ? ' · 开机自启' : ''}
    </div>
  </div>`;
}

function bindItemActions() {
  document.querySelectorAll('#list button[data-act]').forEach(btn => {
    btn.onclick = async () => {
      const id = btn.dataset.id;
      const act = btn.dataset.act;
      try {
        if (act === 'start') await req(`/api/mounts/${id}/start`, {method: 'POST'});
        else if (act === 'stop') await req(`/api/mounts/${id}/stop`, {method: 'POST'});
        else if (act === 'log') {
          const r = await fetch(`/api/mounts/${id}/log`);
          const text = await r.text();
          openModal('日志 - ' + id, text);
          return;
        }
        else if (act === 'del') {
          if (!confirm('确认删除该挂载配置?')) return;
          await req(`/api/mounts/${id}`, {method: 'DELETE'});
        }
        await loadList();
      } catch (e) {
        alert(e.message);
      }
    };
  });
}

function escapeHtml(s) {
  return String(s || '').replace(/[&<>"']/g, m => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[m]));
}

// 表单提交
document.getElementById('add-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const form = e.target;
  const data = {
    name: form.name.value.trim(),
    url: form.url.value.trim(),
    username: form.username.value.trim(),
    password: form.password.value,
    mountpoint: form.mountpoint.value.trim(),
    dir_cache: form.dir_cache.value.trim() || '24h',
    attr_time: form.attr_time.value.trim() || '1h',
    vfs_cache_mode: form.vfs_cache_mode.value || 'off',
    allow_other: form.allow_other.checked,
    auto_start: form.auto_start.checked,
  };
  try {
    await req('/api/mounts', {method: 'POST', body: data});
    form.reset();
    await loadList();
  } catch (err) {
    alert(err.message);
  }
});

// 刷新 + systemd
document.getElementById('btn-refresh').onclick = loadList;
document.getElementById('btn-systemd').onclick = async () => {
  const r = await fetch('/api/systemd', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: '{}'});
  const text = await r.text();
  openModal('openlist-mount.service', text + '\n\n# 安装:\n# sudo cp openlist-mount.service /etc/systemd/system/\n# sudo systemctl daemon-reload\n# sudo systemctl enable --now openlist-mount');
};

// 模态框
function openModal(title, body) {
  document.getElementById('modal-title').textContent = title;
  document.getElementById('modal-body').textContent = body;
  document.getElementById('modal').classList.remove('hidden');
}
document.getElementById('modal-close').onclick = () => document.getElementById('modal').classList.add('hidden');

// 启动
checkRclone();
loadList();
// 每 10s 刷新列表状态
setInterval(loadList, 10000);
