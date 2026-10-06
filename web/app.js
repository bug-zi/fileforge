const dropzone = document.getElementById('dropzone');
const fileInput = document.getElementById('fileInput');
const concSlider = document.getElementById('concurrency');
const concVal = document.getElementById('concVal');
const qualitySlider = document.getElementById('quality');
const qualityVal = document.getElementById('qualityVal');
const filePanel = document.getElementById('filePanel');
const fileList = document.getElementById('fileList');
const estTotal = document.getElementById('estTotal');
const progressPanel = document.getElementById('progressPanel');
const readyActions = document.getElementById('readyActions');
const startBtn = document.getElementById('startBtn');
const runArea = document.getElementById('runArea');
const barFill = document.getElementById('barFill');
const progText = document.getElementById('progText');
const downloadBtn = document.getElementById('downloadBtn');
const statText = document.getElementById('statText');
const historyList = document.getElementById('historyList');
const historyEmpty = document.getElementById('historyEmpty');

concSlider.addEventListener('input', () => { concVal.textContent = concSlider.value; });
qualitySlider.addEventListener('input', () => {
  qualityVal.textContent = qualitySlider.value;
  scheduleEstimate(); // 质量变化即刷新预估（待确认状态下）
});

dropzone.addEventListener('click', openPicker);

function openPicker() {
  // 部分浏览器对 display:none 的 input 不响应程序化 click()，
  // 标准路径是 showPicker()（需用户手势），旧浏览器回退 click()
  if (typeof fileInput.showPicker === 'function') {
    try {
      fileInput.showPicker();
      return;
    } catch (e) { /* 无手势等场景回退 */ }
  }
  fileInput.click();
}

fileInput.addEventListener('change', () => {
  uploadFiles([...fileInput.files]);
  // 清空以便重复选择同一批文件（change 事件依赖值变化）
  fileInput.value = '';
});

['dragenter', 'dragover'].forEach(ev =>
  dropzone.addEventListener(ev, e => { e.preventDefault(); dropzone.classList.add('over'); })
);
['dragleave', 'drop'].forEach(ev =>
  dropzone.addEventListener(ev, e => { e.preventDefault(); dropzone.classList.remove('over'); })
);
dropzone.addEventListener('drop', e => uploadFiles([...e.dataTransfer.files]));

// 流程状态：idle → ready（已上传待确认）→ running → done
let stage = 'idle';
let currentTaskId = null;
let estTimer = null;
let pollTimer = null;
let polling = false;
const estByFile = new Map(); // orig_name -> 预览数据

async function uploadFiles(files) {
  if (!files.length) return;
  const fd = new FormData();
  for (const f of files) fd.append('files[]', f, f.name);

  try {
    const resp = await fetch('/api/tasks', { method: 'POST', body: fd });
    if (!resp.ok) {
      const err = await resp.json().catch(() => ({}));
      alert(err.error || `上传失败（HTTP ${resp.status}）`);
      return;
    }
    const task = await resp.json();
    stage = 'ready';
    currentTaskId = task.id;
    estByFile.clear();
    renderFiles(task.files);

    // 待确认态：展示开始按钮，先出预览
    progressPanel.hidden = false;
    readyActions.hidden = false;
    startBtn.hidden = false;
    startBtn.disabled = false;
    runArea.hidden = true;
    downloadBtn.hidden = true;
    filePanel.hidden = false;
    estTotal.hidden = false;
    estTotal.textContent = '正在计算压缩预览…';

    scheduleEstimate();
    refreshHistory();
  } catch (err) {
    alert('上传失败：' + err.message);
  }
}

startBtn.addEventListener('click', async () => {
  if (stage !== 'ready' || !currentTaskId) return;
  startBtn.disabled = true;
  const fd = new FormData();
  fd.append('concurrency', concSlider.value);
  fd.append('quality', qualitySlider.value);
  try {
    const resp = await fetch(`/api/tasks/${currentTaskId}/start`, { method: 'POST', body: fd });
    if (!resp.ok) {
      const err = await resp.json().catch(() => ({}));
      alert(err.error || `开始失败（HTTP ${resp.status}）`);
      startBtn.disabled = false;
      return;
    }
    const task = await resp.json();
    stage = 'running';
    readyActions.hidden = true;
    runArea.hidden = false;
    estTotal.hidden = true;
    renderFiles(task.files);
    startSSE(currentTaskId);
  } catch (err) {
    alert('开始失败：' + err.message);
    startBtn.disabled = false;
  }
});

// ---------- 压缩预览（真实编码一次的精确大小） ----------

function scheduleEstimate() {
  clearTimeout(estTimer);
  if (stage !== 'ready') return;
  estTimer = setTimeout(fetchEstimate, 400); // 防抖：滑块拖动停 400ms 才请求
}

async function fetchEstimate() {
  if (stage !== 'ready' || !currentTaskId) return;
  try {
    const resp = await fetch(`/api/tasks/${currentTaskId}/estimate?quality=${qualitySlider.value}`);
    if (!resp.ok) return;
    const data = await resp.json();
    if (stage !== 'ready') return; // 已开始转换，丢弃过期预览
    estByFile.clear();
    for (const f of data.files) estByFile.set(f.orig_name, f);
    estTotal.textContent = `预计输出合计 ${formatSize(data.total)}（质量 ${data.quality}，精确预览）`;
    refreshEstCells();
  } catch { /* 网络抖动忽略 */ }
}

function refreshEstCells() {
  for (const tr of fileList.querySelectorAll('tr')) {
    const cell = tr.querySelector('.est');
    if (cell) cell.textContent = estCellText(tr.dataset.name, null);
  }
}

function estCellText(name, f) {
  if (f && f.status === 'done') return formatSize(f.out_size); // 实际结果
  if (stage === 'ready') {
    const e = estByFile.get(name);
    if (!e) return '…';
    if (e.err) return '无法压缩';
    return formatSize(e.est);
  }
  return '—';
}

// ---------- 文件列表渲染 ----------

function renderFiles(files) {
  filePanel.hidden = false;
  fileList.innerHTML = '';
  for (const f of files) {
    const tr = document.createElement('tr');
    tr.dataset.name = f.orig_name;
    tr.innerHTML =
      `<td>${escapeHtml(f.orig_name)}</td>` +
      `<td class="num">${formatSize(f.size)}</td>` +
      `<td class="num est">${estCellText(f.orig_name, f)}</td>` +
      `<td class="status">${statusDetail(f)}</td>`;
    fileList.appendChild(tr);
  }
}

function updateRow(name, statusText) {
  const tr = fileList.querySelector(`tr[data-name="${CSS.escape(name)}"]`);
  if (tr) tr.querySelector('.status').textContent = statusText;
}

function statusDetail(f) {
  switch (f.status) {
    case 'done':
      return `完成，省 ${percentSaved(f)}%`;
    case 'duplicate':
      return '重复（不进结果包）';
    case 'failed':
      return `失败：${escapeHtml(f.reason || '未知原因')}`;
    case 'processing':
      return '处理中…';
    default:
      return stage === 'ready' ? '待确认' : '待处理';
  }
}

function percentSaved(f) {
  if (!f.size || !f.out_size) return 0;
  return Math.max(0, Math.round((1 - f.out_size / f.size) * 100));
}

// ---------- 进度：SSE 实时推送优先，失败自动降级为 1s 轮询 ----------

function startSSE(id) {
  barFill.style.width = '0';
  progText.textContent = '0/0';
  statText.textContent = '';
  let es = null;
  try {
    es = new EventSource(`/api/tasks/${id}/events`);
  } catch {
    startPolling(id);
    return;
  }
  es.onmessage = e => {
    let d;
    try { d = JSON.parse(e.data); } catch { return; }
    applyFrame(d);
    if (d.final) { es.close(); finish(id); }
  };
  es.addEventListener('complete', e => {
    try { applyFrame(JSON.parse(e.data)); } catch { /* 忽略 */ }
    es.close();
    finish(id);
  });
  es.onerror = () => {
    // SSE 不可用/断开：降级轮询（进度功能永可退化，设计兜底）
    if (es) { es.close(); es = null; }
    if (stage === 'running') startPolling(id);
  };
}

function startPolling(id) {
  if (polling) return;
  polling = true;
  pollTimer = setInterval(async () => {
    try {
      const resp = await fetch(`/api/tasks/${id}`);
      if (!resp.ok) return;
      const task = await resp.json();
      renderFiles(task.files);
      renderProgress(task);
      if (task.status === 'done') { stopPolling(); finish(id); }
    } catch { /* 网络抖动忽略 */ }
  }, 1000);
}

function stopPolling() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  polling = false;
}

function applyFrame(d) {
  const total = d.total || 0;
  const done = d.done || 0;
  barFill.style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  progText.textContent = `${done}/${total}`;
  if (d.last && d.last.name) {
    updateRow(d.last.name, lastText(d.last));
  }
}

function lastText(last) {
  switch (last.status) {
    case 'done': return '完成';
    case 'duplicate': return '重复（不进结果包）';
    case 'failed': return `失败：${escapeHtml(last.reason || '未知原因')}`;
    default: return last.status;
  }
}

async function finish(id) {
  if (stage !== 'running') return;
  stage = 'done';
  stopPolling();
  try {
    const resp = await fetch(`/api/tasks/${id}`);
    if (!resp.ok) return;
    const task = await resp.json();
    renderFiles(task.files);
    renderProgress(task);
    downloadBtn.href = `/api/tasks/${id}/download`;
    downloadBtn.hidden = false;
    statText.textContent = `总耗时 ${((task.elapsed_ms || 0) / 1000).toFixed(1)} s · 并发 ${task.concurrency} · 质量 ${task.quality}`;
    refreshHistory();
  } catch { /* 忽略 */ }
}

function renderProgress(task) {
  const done = task.done || 0;
  const total = task.total || 0;
  barFill.style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  progText.textContent = `${done}/${total}`;
}

// ---------- 历史耗时对比表（答辩杀手锏） ----------

async function refreshHistory() {
  try {
    const resp = await fetch('/api/history');
    if (!resp.ok) return;
    const hist = await resp.json();
    hist.sort((a, b) => (b.created_at || '').localeCompare(a.created_at || ''));
    historyEmpty.hidden = hist.length > 0;
    historyList.innerHTML = '';
    for (const h of hist) {
      const running = h.status === 'processing';
      const stateText = h.status === 'ready' ? '未开始' : (running ? '进行中' : '完成');
      const tr = document.createElement('tr');
      tr.innerHTML =
        `<td>${escapeHtml(h.created_at || '')}</td>` +
        `<td class="num">${h.files}</td>` +
        `<td class="num">${h.status === 'ready' ? '—' : h.concurrency}</td>` +
        `<td class="num">${h.status === 'ready' ? '—' : h.quality}</td>` +
        `<td class="${running ? 'run' : 'ok'}">${stateText}</td>` +
        `<td class="num">${h.status === 'done' ? ((h.elapsed_ms || 0) / 1000).toFixed(1) + ' s' : '—'}</td>`;
      historyList.appendChild(tr);
    }
  } catch { /* 忽略 */ }
}

refreshHistory();

// ---------- 工具 ----------

function formatSize(n) {
  if (n == null) return '';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(2) + ' MB';
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, ch => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  })[ch]);
}
