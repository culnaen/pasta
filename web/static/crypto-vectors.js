import {b64ToBytes, decodeText, decryptFileChunk} from './crypto.js';

const vector = {
  root: 'AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8',
  publicID: 'vector-public-id',
  objectID: 'vector-object',
  nonce: 'ICEiIyQlJicoKSor',
  ciphertext: 'XcFw-MgGvCQEP_3V3Hy7aq6EcJ5dSR3qCOgZGU9SKrc',
  plaintext: 'vector plaintext',
};

export async function verifyCryptoVectors() {
  const root = b64ToBytes(vector.root);
  const plaintext = await decryptFileChunk(root, vector.publicID, vector.objectID, 0, 2, vector.nonce, b64ToBytes(vector.ciphertext));
  if (decodeText(plaintext) !== vector.plaintext) throw new Error('golden vector decryption mismatch');

  const corrupted = b64ToBytes(vector.ciphertext);
  corrupted[0] ^= 1;
  await expectIntegrityFailure(() => decryptFileChunk(root, vector.publicID, vector.objectID, 0, 2, vector.nonce, corrupted));
  await expectIntegrityFailure(() => decryptFileChunk(root, vector.publicID, vector.objectID, 1, 2, vector.nonce, b64ToBytes(vector.ciphertext)));
  await expectIntegrityFailure(() => decryptFileChunk(root, 'other-public-id', vector.objectID, 0, 2, vector.nonce, b64ToBytes(vector.ciphertext)));
}

async function expectIntegrityFailure(operation) {
  try {
    await operation();
  } catch (error) {
    if (error.name === 'OperationError') return;
    throw error;
  }
  throw new Error('tampered cryptographic input was accepted');
}
