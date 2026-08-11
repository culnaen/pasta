const enc = new TextEncoder();
const dec = new TextDecoder();

export function bytesToB64(bytes) {
  let value = '';
  for (const byte of bytes) value += String.fromCharCode(byte);
  return btoa(value).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '');
}

export function b64ToBytes(value) {
  const padded = value.replaceAll('-', '+').replaceAll('_', '/') + '==='.slice((value.length + 3) % 4);
  const decoded = atob(padded);
  return Uint8Array.from(decoded, byte => byte.charCodeAt(0));
}

export function randomKey() {
  return crypto.getRandomValues(new Uint8Array(32));
}

export async function deriveKey(root, publicID, purpose) {
  const material = await crypto.subtle.importKey('raw', root, 'HKDF', false, ['deriveKey']);
  return crypto.subtle.deriveKey(
    {
      name: 'HKDF',
      hash: 'SHA-256',
      salt: enc.encode(`pasta:v1:${publicID}`),
      info: enc.encode(purpose),
    },
    material,
    {name: 'AES-GCM', length: 256},
    false,
    ['encrypt', 'decrypt'],
  );
}

export async function encrypt(root, publicID, purpose, plain) {
  const key = await deriveKey(root, publicID, purpose);
  const nonce = crypto.getRandomValues(new Uint8Array(12));
  const ciphertext = await crypto.subtle.encrypt(
    {name: 'AES-GCM', iv: nonce, additionalData: enc.encode(`pasta:v1:${publicID}:${purpose}`)},
    key,
    plain,
  );
  return {nonce: bytesToB64(nonce), data: bytesToB64(new Uint8Array(ciphertext))};
}

export async function decrypt(root, publicID, purpose, nonce, data) {
  const key = await deriveKey(root, publicID, purpose);
  return crypto.subtle.decrypt(
    {name: 'AES-GCM', iv: b64ToBytes(nonce), additionalData: enc.encode(`pasta:v1:${publicID}:${purpose}`)},
    key,
    b64ToBytes(data),
  );
}

export const fileChunkBytes = 4 * 1024 * 1024;

export async function encryptFileChunk(root, publicID, objectID, index, total, plain) {
  const purpose = `object:${objectID}`;
  const key = await deriveKey(root, publicID, purpose);
  const nonce = crypto.getRandomValues(new Uint8Array(12));
  const aad = enc.encode(`pasta:v1:${publicID}:${purpose}:chunk:${index}:${total}`);
  const data = await crypto.subtle.encrypt({name: 'AES-GCM', iv: nonce, additionalData: aad}, key, plain);
  return {nonce: bytesToB64(nonce), data: new Uint8Array(data)};
}

export async function decryptFileChunk(root, publicID, objectID, index, total, nonce, data) {
  const purpose = `object:${objectID}`;
  const key = await deriveKey(root, publicID, purpose);
  const aad = enc.encode(`pasta:v1:${publicID}:${purpose}:chunk:${index}:${total}`);
  return crypto.subtle.decrypt({name: 'AES-GCM', iv: b64ToBytes(nonce), additionalData: aad}, key, data);
}

export function textBytes(value) {
  return enc.encode(value);
}

export function decodeText(value) {
  return dec.decode(value);
}

export function fileURL(bytes, name) {
  const url = URL.createObjectURL(new Blob([bytes], {type: 'application/octet-stream'}));
  const link = document.createElement('a');
  link.href = url;
  link.download = name;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
