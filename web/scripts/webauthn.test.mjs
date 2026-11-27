// Tests for src/lib/webauthn.ts: the browser<->server wire format for passkeys. Run with `npm test`.
import { test } from "node:test";
import assert from "node:assert/strict";
import { b64urlToBytes, bytesToB64url, toCreationOptions, toRequestOptions, attestationToJSON, assertionToJSON } from "../src/lib/webauthn.ts";

const RAW = Uint8Array.from([0xff, 0xfe, 0x80, 0x00, 0x01, 0xc3, 0x28, 0x7f, 0xa5]);
const buf = (u8) => u8.buffer.slice(u8.byteOffset, u8.byteOffset + u8.byteLength);

test("base64url round-trips arbitrary bytes, unpadded, URL-safe alphabet", () => {
  const enc = bytesToB64url(RAW);
  assert.match(enc, /^[A-Za-z0-9_-]+$/);
  assert.deepEqual([...b64urlToBytes(enc)], [...RAW]);
  for (let n = 0; n < 12; n++) {
    const b = Uint8Array.from({ length: n }, (_, i) => (i * 37 + 11) & 0xff);
    assert.deepEqual([...b64urlToBytes(bytesToB64url(b))], [...b]);
  }
});

test("a standard-alphabet / padded value is not what the server gets", () => {
  // btoa(String.fromCharCode(...RAW)) uses + / and = ; the encoder must not.
  assert.doesNotMatch(bytesToB64url(RAW), /[+/=]/);
});

test("creation and request options turn every binary string into bytes", () => {
  const id = bytesToB64url(RAW);
  const c = toCreationOptions({
    rp: { id: "ops.example", name: "Forge" }, user: { id, name: "testuser", displayName: "testuser" }, challenge: id,
    pubKeyCredParams: [{ type: "public-key", alg: -7 }], excludeCredentials: [{ id, type: "public-key", transports: ["usb"] }],
  });
  assert.ok(c.challenge instanceof Uint8Array && c.user.id instanceof Uint8Array && c.excludeCredentials[0].id instanceof Uint8Array);
  assert.deepEqual([...c.challenge], [...RAW]);
  assert.deepEqual([...c.user.id], [...RAW]);
  const r = toRequestOptions({ challenge: id, rpId: "ops.example", allowCredentials: [{ id, type: "public-key" }] });
  assert.deepEqual([...r.challenge], [...RAW]);
  assert.deepEqual([...r.allowCredentials[0].id], [...RAW]);
  assert.equal(toRequestOptions({ challenge: id }).allowCredentials, undefined);
});

test("responses keep cred.id as is and encode every buffer as base64url", () => {
  const cred = {
    id: "AbC-_dEf", rawId: buf(RAW),
    response: { clientDataJSON: buf(RAW), attestationObject: buf(RAW), authenticatorData: buf(RAW), signature: buf(RAW), userHandle: buf(RAW), getTransports: () => ["internal"] },
  };
  const a = attestationToJSON(cred);
  assert.equal(a.id, "AbC-_dEf"); // not btoa'd again
  assert.equal(a.rawId, bytesToB64url(RAW));
  assert.equal(a.response.clientDataJSON, bytesToB64url(RAW));
  assert.deepEqual(a.response.transports, ["internal"]);
  const s = assertionToJSON(cred);
  assert.equal(s.id, "AbC-_dEf");
  assert.equal(s.response.signature, bytesToB64url(RAW));
  assert.equal(s.response.userHandle, bytesToB64url(RAW));
  assert.equal(assertionToJSON({ ...cred, response: { ...cred.response, userHandle: null } }).response.userHandle, undefined);
  for (const v of [a.rawId, a.response.clientDataJSON, a.response.attestationObject, s.response.authenticatorData]) assert.doesNotMatch(v, /[+/=]/);
});
