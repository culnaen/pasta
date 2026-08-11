import {b64ToBytes, decrypt, decryptFileChunk, decodeText, fileURL} from './crypto.js';

const $ = selector => document.querySelector(selector);
const status = $('#status');
const content = $('#content');
const burn = $('#burn');
const publicId = location.pathname.split('/').pop();
const keyText = location.hash.slice(1);
let root;
let paste;

function setStatus(value) {
  status.textContent = value;
}

async function request(url, options = {}) {
  const response = await fetch(url, options);
  if (!response.ok) {
    const error = new Error('Paste is unavailable or has already been opened');
    error.status = response.status;
    throw error;
  }
  return response;
}

async function load(claim = false) {
  try {
    if (!keyText) throw new Error('The decryption key is missing from the link');
    root = b64ToBytes(keyText);
    if (root.length !== 32) throw new Error('Invalid decryption key');

    const response = await request(`/api/v1/pastes/${publicId}${claim ? '/claim' : ''}`, {
      method: claim ? 'POST' : 'GET',
    });
    paste = await response.json();
    if (paste.burnAfterRead && !paste.manifest) {
      burn.hidden = false;
      setStatus('This is a one-time paste. Confirm to open it.');
      return;
    }

    const raw = await decrypt(root, publicId, 'manifest', paste.manifestNonce, paste.manifest);
    const manifest = JSON.parse(decodeText(raw));
    const text = manifest.version >= 2
      ? decodeText(await decrypt(root, publicId, 'text', manifest.textNonce, manifest.textCiphertext))
      : manifest.text || '';
    $('#text').textContent = text || '—';

    const list = $('#files');
    for (const file of manifest.files) {
      const item = document.createElement('li');
      const button = document.createElement('button');
      button.textContent = `Download ${file.name} (${file.size} bytes)`;
      button.addEventListener('click', () => file.chunks ? downloadChunked(file) : download(file));
      item.append(button);
      list.append(item);
    }

    content.hidden = false;
    burn.hidden = true;
    setStatus(
      paste.burnAfterRead && manifest.files.length
        ? 'Paste decrypted locally. Each attached file can be downloaded once during this access window.'
        : 'Paste decrypted locally.',
    );
  } catch (error) {
    setStatus(
      error.message === 'OperationError'
        ? 'Invalid key or damaged data'
        : claim && !error.status
          ? 'Connection was interrupted after confirmation. The paste may be unavailable.'
          : error.message,
    );
  }
}

function claimHeaders() {
  return paste.claimToken ? {'X-Claim-Token': paste.claimToken} : {};
}

async function download(file) {
  try {
    setStatus(`Downloading ${file.name}…`);
    const response = await request(`/api/v1/pastes/${publicId}/objects/${file.id}`, {headers: claimHeaders()});
    const encrypted = await response.text();
    const raw = await decrypt(root, publicId, `object:${file.id}`, file.nonce, encrypted);
    fileURL(raw, file.name);
    setStatus(`${file.name} decrypted and saved.`);
  } catch (error) {
    setStatus(error.message === 'OperationError' ? 'File is damaged or the key is invalid' : error.message);
  }
}

async function downloadChunked(file) {
  try {
    setStatus(`Downloading ${file.name}…`);
    const response = await request(`/api/v1/pastes/${publicId}/objects/${file.id}`, {headers: claimHeaders()});
    if (!response.body) throw new Error('Streaming download is unavailable');

    const reader = response.body.getReader();
    let pending = new Uint8Array(0);
    const readExactly = async size => {
      const output = new Uint8Array(size);
      let offset = 0;
      while (offset < size) {
        if (!pending.length) {
          const next = await reader.read();
          if (next.done) throw new Error('File is damaged');
          pending = next.value;
        }
        const count = Math.min(size - offset, pending.length);
        output.set(pending.subarray(0, count), offset);
        offset += count;
        pending = pending.subarray(count);
      }
      return output;
    };

    const parts = [];
    for (let index = 0; index < file.chunks.length; index++) {
      const chunk = file.chunks[index];
      const encrypted = await readExactly(chunk.size);
      parts.push(await decryptFileChunk(root, publicId, file.id, index, file.chunks.length, chunk.nonce, encrypted));
    }
    const tail = await reader.read();
    if (pending.length || !tail.done) throw new Error('File is damaged');
    fileURL(new Blob(parts), file.name);
    setStatus(`${file.name} decrypted and saved.`);
  } catch (error) {
    setStatus(error.message === 'OperationError' ? 'File is damaged or the key is invalid' : error.message);
  }
}

$('#claim').addEventListener('click', () => load(true));
load();
