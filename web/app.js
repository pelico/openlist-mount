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

function fmtBytes(b) {
  if (!b && b !== 0) return '-';
  const units = ['B','K','M','G','T','P'];
  let i = 0, v = b;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(1) + units[i];
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

async function checkHealth() {
  const el = document.getElementById('health-hint');
  try {
    const h = await req('/api/health');
    const msgs = [];
    if (h.fuse && !h.fuse.available) msgs.push('❌ ' + h.fuse.hint);
    if (h.fuse_conf_hint) msgs.push('⚠ ' + h.fuse_conf_hint);
    if (h.disk && h.disk.free_bytes !== undefined) {
      msgs.push('💾 数据目录剩余 ' + fmtBytes(h.disk.free_bytes) + ' / ' + fmtBytes(h.disk.total_bytes));
    }
    if (msgs.length) {
      el.innerHTML = msgs.join(' · ');
      el.className = 'health-hint' + (msgs.some(m => m.startsWith('❌')) ? ' bad' : (msgs.some(m => m.startsWith('⚠')) ? ' warn' : ''));
    }
  } catch (e) {
    // health 接口挂了就算了
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
  const fuse = s.fuse_state;
  const toggleBtn = s.running
    ? `<button data-act="stop" data-id="${c.id}" class="ghost">停止</button>`
    : `<button data-act="start" data-id="${c.id}" class="primary">启动</button>`;

  // 拼装缓存参数摘要
  const cacheParts = [];
  cacheParts.push('模式 ' + (c.vfs_cache_mode || 'off'));
  if (c.vfs_cache_mode && c.vfs_cache_mode !== 'off') {
    if (c.vfs_cache_max_size) cacheParts.push('上限 ' + c.vfs_cache_max_size);
    if (c.vfs_cache_max_age) cacheParts.push('最久 ' + c.vfs_cache_max_age);
    cacheParts.push('扫描 ' + (c.vfs_cache_poll_interval || '1m'));
    if (c.cache_dir) cacheParts.push('缓存目录 ' + c.cache_dir);
  }
  // 内存/并发摘要
  const memParts = [];
  memParts.push('并发 ' + (c.transfers || 4));
  memParts.push('buffer ' + (c.buffer_size || '32M'));
  memParts.push('预读 ' + (c.max_read_ahead || '128K'));

  return `
  <div class="item">
    <div class="item-head">
      <div>
        <span class="item-name">${escapeHtml(c.name)}</span>
        <span class="badge ${cls}">${label}</span>
        ${fuse === 'stale' ? '<span class="badge err">FUSE僵尸</span>' : ''}
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
      ${s.running ? ` · PID ${s.pid} · 启动 ${fmtTime(s.started_at)}` : ''}
      ${s.last_error ? ` · <span class="err-text">错误: ${escapeHtml(s.last_error)}</span>` : ''}
      <br>目录缓存 ${escapeHtml(c.dir_cache || '24h')} · 属性缓存 ${escapeHtml(c.attr_time || '1h')}
      <br>${cacheParts.join(' · ')}
      <br>🧠 ${memParts.join(' · ')}
      ${c.allow_other ? ' · allow_other' : ''}
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
    // 新增缓存控制字段
    cache_dir: form.cache_dir.value.trim(),
    vfs_cache_max_size: form.vfs_cache_max_size.value.trim(),
    vfs_cache_max_age: form.vfs_cache_max_age.value.trim(),
    vfs_cache_poll_interval: form.vfs_cache_poll_interval.value.trim(),
    // 新增内存/并发控制字段
    buffer_size: form.buffer_size.value.trim(),
    transfers: parseInt(form.transfers.value, 10) || 0,
    max_read_ahead: form.max_read_ahead.value.trim(),
  };
  try {
    await req('/api/mounts', {method: 'POST', body: data});
    form.reset();
    // reset 后恢复极限资源默认值(而不是全部空)
    form.vfs_cache_mode.value = 'writes';
    form.vfs_cache_max_size.value = '500M';
    form.vfs_cache_max_age.value = '5m';
    form.vfs_cache_poll_interval.value = '1m';
    form.cache_dir.value = '/tmp/rclone-cache';
    form.transfers.value = 1;
    form.buffer_size.value = '8M';
    form.max_read_ahead.value = '0';
    form.allow_other.checked = true;
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
checkHealth();
loadList();
// 每 15s 刷新列表 + 健康状态
setInterval(() => { loadList(); checkHealth(); }, 15000);
