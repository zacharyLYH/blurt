// Reading a Web Push payload the way the browser's own service worker would, so a spec can
// assert the words the user actually sees rather than only that bytes arrived. This is
// RFC 8291 over an RFC 8188 aes128gcm record, which is what the Go sender produces.
import { createDecipheriv, createECDH, hkdfSync, randomBytes } from 'node:crypto'

export interface DeviceKeys {
  privateKey: Buffer
  // The uncompressed P-256 point, as pushManager.subscribe() would hand it over.
  publicKey: Buffer
  auth: Buffer
}

// deviceKeys mints the device half of a subscription exactly as a browser does, and keeps
// the private half so the test can open what the backend encrypted for it.
export function deviceKeys(): DeviceKeys {
  const ecdh = createECDH('prime256v1')
  ecdh.generateKeys()
  return { privateKey: ecdh.getPrivateKey(), publicKey: ecdh.getPublicKey(), auth: randomBytes(16) }
}

// subscription is the body a browser posts to /api/push/subscribe.
export function subscription(keys: DeviceKeys, endpoint: string) {
  return {
    endpoint,
    keys: {
      p256dh: keys.publicKey.toString('base64url'),
      auth: keys.auth.toString('base64url'),
    },
  }
}

// NUL terminates every derivation's info string.
const NUL = Buffer.from([0])

// keyInfo is the info string one derivation expands: the label and its terminator. The
// record counter HKDF needs is the expander's own, so it is not part of this.
function keyInfo(label: string): Buffer {
  return Buffer.concat([Buffer.from(label), NUL])
}

export function decryptPush(body: Buffer, keys: DeviceKeys): string {
  // Header: salt(16) | rs(4) | idlen(1) | keyid, then one record's ciphertext and tag.
  const salt = body.subarray(0, 16)
  const senderKey = body.subarray(21, 21 + body.readUInt8(20))
  const ciphertext = body.subarray(21 + senderKey.length)

  const ecdh = createECDH('prime256v1')
  ecdh.setPrivateKey(keys.privateKey)
  const shared = ecdh.computeSecret(senderKey)

  // The auth secret salts the agreement and both public keys ride the info string, which is
  // what binds the payload to this one subscription. The sender's key comes second, because
  // the user agent's is the one the receiver knows for certain.
  const ikm = Buffer.from(hkdfSync('sha256', shared, keys.auth, Buffer.concat([
    keyInfo('WebPush: info'),
    keys.publicKey,
    senderKey,
  ]), 32))
  const cek = Buffer.from(hkdfSync('sha256', ikm, salt, keyInfo('Content-Encoding: aes128gcm'), 16))
  const nonce = Buffer.from(hkdfSync('sha256', ikm, salt, keyInfo('Content-Encoding: nonce'), 12))

  const decipher = createDecipheriv('aes-128-gcm', cek, nonce)
  decipher.setAuthTag(ciphertext.subarray(ciphertext.length - 16))
  const plaintext = Buffer.concat([
    decipher.update(ciphertext.subarray(0, ciphertext.length - 16)),
    decipher.final(),
  ])

  // The last record's plaintext ends with a 0x02 delimiter and is zero-padded behind it.
  let end = plaintext.length
  while (end > 0 && plaintext[end - 1] === 0) end -= 1
  if (end > 0 && plaintext[end - 1] === 2) end -= 1
  return plaintext.subarray(0, end).toString('utf8')
}