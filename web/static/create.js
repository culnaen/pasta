import {randomKey, bytesToB64, encrypt, encryptFileChunk, fileChunkBytes, textBytes} from './crypto.js';

const $ = selector => document.querySelector(selector);
const form = $('#create-form');
const status = $('#status');
const result = $('#result');
const fileInput = $('#files');
const fileList = $('#file-list');
const fileSummary = $('#file-summary');
const uploadZone = $('.upload-zone');
const statistics = $('#statistics');
let files = [];
let limits = {maxFiles: 10, maxFileBytes: 50 * 1024 * 1024, maxPasteBytes: 100 * 1024 * 1024, maxTextBytes: 1024 * 1024, ttlHours: [1, 24, 168, 720]};

function setStatus(text) { status.textContent = text; }
function fail(message) { setStatus(message); form.querySelector('button[type=submit]').disabled = false; }
function formatBytes(bytes) { return bytes < 1024 * 1024 ? `${Math.ceil(bytes / 1024)} KB` : `${(bytes / 1024 / 1024).toFixed(1)} MB`; }

function renderFiles() {
  fileList.replaceChildren();
  const total = files.reduce((sum, file) => sum + file.size, 0);
  fileSummary.textContent = files.length ? `${files.length} file${files.length === 1 ? '' : 's'} · ${formatBytes(total)}` : '';
  files.forEach((file, index) => {
    const item = document.createElement('li');
    item.className = 'file-item';
    const name = document.createElement('span');
    name.textContent = file.name;
    const size = document.createElement('small');
    size.textContent = formatBytes(file.size);
    const remove = document.createElement('button');
    remove.className = 'remove-file';
    remove.type = 'button';
    remove.ariaLabel = `Remove ${file.name}`;
    remove.textContent = '×';
    remove.addEventListener('click', () => { files.splice(index, 1); renderFiles(); });
    item.append(name, size, remove);
    fileList.append(item);
  });
}

function addFiles(incoming) {
  const next = [...files, ...incoming].slice(0, limits.maxFiles);
  files = next;
  renderFiles();
}

async function request(url, options = {}) {
  const response = await fetch(url, options);
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new Error(body.error || 'Server error');
  }
  return response;
}

function uploadObject(url, data, label, extraHeaders = {}) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('PUT', url);
    xhr.setRequestHeader('Content-Type', 'application/octet-stream');
    for (const [name, value] of Object.entries(extraHeaders)) xhr.setRequestHeader(name, value);
    xhr.upload.onprogress = event => { if (event.lengthComputable) setStatus(`Uploading ${label}: ${Math.round(event.loaded / event.total * 100)}%`); };
    xhr.onerror = () => reject(new Error('Network error during upload'));
    xhr.onload = () => xhr.status >= 200 && xhr.status < 300 ? resolve() : reject(new Error('Upload failed'));
    xhr.send(data);
  });
}

async function loadLimits() {
  try {
    limits = await request('/api/v1/config').then(response => response.json());
    $('#text').maxLength = limits.maxTextBytes;
    const ttl = $('#ttl');
    ttl.replaceChildren();
    for (const hours of limits.ttlHours) {
      const option = document.createElement('option');
      option.value = hours;
      option.textContent = hours < 24 ? `${hours} hour${hours === 1 ? '' : 's'}` : `${hours / 24} day${hours === 24 ? '' : 's'}`;
      ttl.append(option);
    }
    ttl.value = limits.ttlHours.includes(168) ? 168 : limits.ttlHours[0];
    renderFiles();
  } catch {
    fail('Could not load server limits.');
  }
}

async function loadStats() {
  try {
    const stats = await request('/api/v1/stats').then(response => response.json());
    statistics.textContent = `Currently sharing ${stats.links} link${stats.links === 1 ? '' : 's'} · ${stats.files} file${stats.files === 1 ? '' : 's'}`;
    statistics.hidden = false;
  } catch {}
}

fileInput.addEventListener('change', () => { addFiles([...fileInput.files]); fileInput.value = ''; });
for (const eventName of ['dragenter', 'dragover']) uploadZone.addEventListener(eventName, event => { event.preventDefault(); uploadZone.classList.add('dragging'); });
for (const eventName of ['dragleave', 'drop']) uploadZone.addEventListener(eventName, event => { event.preventDefault(); uploadZone.classList.remove('dragging'); });
uploadZone.addEventListener('drop', event => addFiles([...event.dataTransfer.files]));

loadStats();

if (!globalThis.crypto?.subtle || !globalThis.crypto.getRandomValues || !globalThis.File?.prototype.arrayBuffer) {
  form.querySelector('button[type=submit]').disabled = true;
  setStatus('This browser does not support Web Crypto and file handling.');
} else {
  loadLimits();
}

form.addEventListener('submit', async event => {
  event.preventDefault();
  const button = form.querySelector('button[type=submit]');
  button.disabled = true;
  result.hidden = true;
  try {
    const text = $('#text').value;
    if (!text && !files.length) throw new Error('Add text or a file');
    if (files.length > limits.maxFiles) throw new Error(`Maximum ${limits.maxFiles} files`);
    for (const file of files) if (file.size > limits.maxFileBytes) throw new Error(`${file.name}: file is too large`);
    const plainBytes = files.reduce((sum, file) => sum + file.size, new TextEncoder().encode(text).length);
    if (plainBytes > limits.maxPasteBytes) throw new Error('Paste is too large');
    setStatus('Creating paste…');
    const init = await request('/api/v1/pastes', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({objectCount: files.length, ttlHours: Number($('#ttl').value), burnAfterRead: $('#burn').checked})}).then(response => response.json());
    const root = randomKey();
    const entries = [];
    for (let i = 0; i < files.length; i++) {
      const file = files[i];
      const objectID = init.objectIds[i];
      const total = Math.max(1, Math.ceil(file.size / fileChunkBytes));
      const chunks = [];
      for (let index = 0; index < total; index++) {
        setStatus(`Encrypting file ${i + 1}/${files.length}, chunk ${index + 1}/${total}: ${file.name}`);
        const encrypted = await encryptFileChunk(root, init.publicId, objectID, index, total, await file.slice(index * fileChunkBytes, Math.min(file.size, (index + 1) * fileChunkBytes)).arrayBuffer());
        chunks.push({nonce: encrypted.nonce, size: encrypted.data.byteLength});
        await uploadObject(`/api/v1/pastes/${init.publicId}/objects/${objectID}/chunks/${index}`, encrypted.data, `${i + 1}/${files.length}, chunk ${index + 1}/${total}: ${file.name}`, index === total - 1 ? {'X-Chunk-Final': '1'} : {});
      }
      entries.push({id: objectID, name: file.name, type: file.type, size: file.size, chunks});
    }
    setStatus('Encrypting text…');
    const encryptedText = await encrypt(root, init.publicId, 'text', textBytes(text));
    setStatus('Encrypting manifest…');
    const manifest = await encrypt(root, init.publicId, 'manifest', textBytes(JSON.stringify({version: 3, textCiphertext: encryptedText.data, textNonce: encryptedText.nonce, files: entries})));
    setStatus('Publishing…');
    await request(`/api/v1/pastes/${init.publicId}/complete`, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({manifest: manifest.data, manifestNonce: manifest.nonce})});
    $('#public-link').value = `${location.origin}/p/${init.publicId}#${bytesToB64(root)}`;
    $('#delete-link').value = `${location.origin}/d/${init.publicId}#${init.deleteToken}`;
    result.hidden = false;
    result.scrollIntoView({behavior: 'smooth', block: 'nearest'});
    setStatus('Ready. Send the first link to the recipient.');
  } catch (error) {
    fail(error.message);
  }
});

document.addEventListener('click', async event => {
  const id = event.target.dataset.copy;
  if (!id) return;
  const button = event.target;
  const input = document.getElementById(id);
  try {
    await navigator.clipboard.writeText(input.value);
    button.textContent = 'Copied';
    setTimeout(() => { button.textContent = 'Copy'; }, 1800);
  } catch {
    input.select();
    setStatus('Copy the link manually.');
  }
});

$('#show-qr').addEventListener('click', () => {
  const node = $('#qr');
  const qr = globalThis.qrcode(0, 'M');
  qr.addData($('#public-link').value);
  qr.make();
  node.hidden = false;
  node.innerHTML = qr.createSvgTag({cellSize: 4, margin: 2});
});
