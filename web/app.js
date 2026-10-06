const dropzone = document.getElementById('dropzone');
const fileInput = document.getElementById('fileInput');
const concSlider = document.getElementById('concurrency');
const concVal = document.getElementById('concVal');
const qualitySlider = document.getElementById('quality');
const qualityVal = document.getElementById('qualityVal');
const filePanel = document.getElementById('filePanel');
const fileList = document.getElementById('fileList');
const progressPanel = document.getElementById('progressPanel');
const barFill = document.getElementById('barFill');
const progText = document.getElementById('progText');
const downloadBtn = document.getElementById('downloadBtn');
const statText = document.getElementById('statText');
const historyList = document.getElementById('historyList');
const historyEmpty = document.getElementById('historyEmpty');

concSlider.addEventListener('input', () => { concVal.textContent = concSlider.value; });
qualitySlider.addEventListener('input', () => { qualityVal.textContent = qualitySlider.value; });

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
  // 清空以便重复选择同一批文件（change 事件依赖值变化，演示剧本要同批跑两轮）
  fileInput.value = '';
});

['dragenter', 'dragover'].forEach(ev =>
  dropzone.addEventListener(ev, e => { e.preventDefault(); dropzone.classList.add('over'); })
);
['dragleave', 'drop'].forEach(ev =>
  dropzone.addEventListener(ev, e => { e.preventDefault(); dropzone.classList.remove('over'); })
);
dropzone.addEventListener('drop', e => uploadFiles([...e.dataTransfer.files]));

async function uploadFiles(files) {
  if (!files.length) return;
  const fd = new FormData();
  for (const f of files) fd.append('files[]', f, f.name);
  fd.append('concurrency', concSlider.value);
  fd.append('quality', qualitySlider.value);

  try {
    const resp = await fetch('/api/tasks', { method: 'POST', body: fd });
    if (!resp.ok) {
      const err = await resp.json().catch(() => ({}));
      alert(err.error || `上传失败（HTTP ${resp.status}）`);
      return;
    }
    const task = await resp.json();
    renderFiles(task.files);
    startProgress(task.id);
  } catch (err) {
    alert('上传失败：' + err.message);
  }
}

function renderFiles(files) {
  filePanel.hidden = false;
  fileList.innerHTML = '';
  for (const f of files) {
    const tr = document.createElement('tr');
    tr.dataset.name = f.orig_name;
    tr.innerHTML =
      `<td>${escapeHtml(f.orig_name)}</td>` +
      `<td class="num">${formatSize(f.size)}</td>` +
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
      return `完成，${formatSize(f.size)} → ${formatSize(f.out_size)}`;
    case 'duplicate':
      return '重复（不进结果包）';
    case 'failed':
      return `失败：${escapeHtml(f.reason || '未知原因')}`;
    case 'processing':
      return '处理中…';
    default:
      return '待处理';
  }
}

// 进度：SSE 实时推送优先，失败自动降级为 1s 轮询（阶段 4）
let progressActive = false;

function startProgress(id) {
  progressPanel.hidden = false;
  downloadBtn.hidden = true;
  statText.textContent = '';
  barFill.style.width = '0';
  progText.textContent = '0/0';
  progressActive = true;

  const finish = async () => {
    if (!progressActive) return;
    progressActive = false;
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
  };

  const startPolling = () => {
    if (!progressActive || progressActivePolling) return;
    progressActivePolling = true;
    pollTimer = setInterval(async () => {
      try {
        const resp = await fetch(`/api/tasks/${id}`);
        if (!resp.ok) return;
        const task = await resp.json();
        renderFiles(task.files);
        renderProgress(task);
        if (task.status === 'done') { stopPolling(); finish(); }
      } catch { /* 网络抖动忽略 */ }
    }, 1000);
  };

  let es = null;
  try {
    es = new EventSource(`/api/tasks/${id}/events`);
  } catch {
    startPolling();
    return;
  }
  es.onmessage = e => {
    let d;
    try { d = JSON.parse(e.data); } catch { return; }
    applyFrame(d);
    if (d.final) { es.close(); finish(); }
  };
  es.addEventListener('complete', e => {
    try { applyFrame(JSON.parse(e.data)); } catch { /* 忽略 */ }
    es.close();
    finish();
  });
  es.onerror = () => {
    // SSE 不可用/断开：降级轮询（进度功能永可退化，设计兜底）
    if (es) { es.close(); es = null; }
    if (progressActive) startPolling();
  };
}

let pollTimer = null;
let progressActivePolling = false;

function stopPolling() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  progressActivePolling = false;
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

function renderProgress(task) {
  const done = task.done || 0;
  const total = task.total || 0;
  barFill.style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  progText.textContent = `${done}/${total}`;

  let doneN = 0, dupN = 0, failN = 0;
  for (const f of task.files || []) {
    if (f.status === 'done') doneN++;
    else if (f.status === 'duplicate') dupN++;
    else if (f.status === 'failed') failN++;
  }
  statText.textContent = `完成 ${doneN} · 重复 ${dupN} · 失败 ${failN}`;
}

// 历史耗时对比表（答辩杀手锏：并发 1 vs 高并发两行数字并排）
async function refreshHistory() {
  try {
    const resp = await fetch('/api/history');
    if (!resp.ok) return;
    const hist = await resp.json();
    hist.sort((a, b) => (b.created_at || '').localeCompare(a.created_at || ''));
    historyEmpty.hidden = hist.length > 0;
    historyList.innerHTML = '';
    for (const h of hist) {
      const running = h.files > 0 && h.done < h.files;
      const tr = document.createElement('tr');
      tr.innerHTML =
        `<td>${escapeHtml(h.created_at || '')}</td>` +
        `<td class="num">${h.files}</td>` +
        `<td class="num">${h.concurrency}</td>` +
        `<td class="num">${h.quality}</td>` +
        `<td class="${running ? 'run' : 'ok'}">${running ? '进行中' : '完成'}</td>` +
        `<td class="num">${running ? '—' : ((h.elapsed_ms || 0) / 1000).toFixed(1) + ' s'}</td>`;
      historyList.appendChild(tr);
    }
  } catch { /* 忽略 */ }
}

refreshHistory();

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
