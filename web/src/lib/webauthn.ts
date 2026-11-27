// SPDX-License-Identifier: Apache-2.0
//
// WebAuthn wire helpers. The server speaks base64url (unpadded) for every binary field (challenge, user handle, credential ids,
// attestation and assertion payloads). navigator.credentials wants ArrayBuffers in and gives ArrayBuffers out, so both directions go
// through here. Casting the JSON straight to PublicKeyCredential*Options (strings where buffers belong) and base64-encoding with btoa
// (standard alphabet, padded; and btoa(cred.id) double-encodes an id that is already base64url) makes the ceremony fail in the browser or on
// the server.

export function b64urlToBytes(s: string): Uint8Array<ArrayBuffer> {
  const pad = s.length % 4 === 0 ? "" : "=".repeat(4 - (s.length % 4));
  const bin = atob(s.replace(/-/g, "+").replace(/_/g, "/") + pad);
  const out = new Uint8Array(new ArrayBuffer(bin.length));
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

export function bytesToB64url(buf: ArrayBuffer | Uint8Array): string {
  const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  let bin = "";
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

interface DescriptorJSON {
  id: string;
  type: string;
  transports?: string[];
}

function descriptors(list: DescriptorJSON[] | undefined): PublicKeyCredentialDescriptor[] | undefined {
  return list?.map((d) => ({ id: b64urlToBytes(d.id), type: d.type as PublicKeyCredentialType, transports: d.transports as AuthenticatorTransport[] | undefined }));
}

export interface CreationOptionsJSON {
  rp: PublicKeyCredentialRpEntity;
  user: { id: string; name: string; displayName: string };
  challenge: string;
  pubKeyCredParams: PublicKeyCredentialParameters[];
  timeout?: number;
  excludeCredentials?: DescriptorJSON[];
  authenticatorSelection?: AuthenticatorSelectionCriteria;
  attestation?: AttestationConveyancePreference;
}

export interface RequestOptionsJSON {
  challenge: string;
  timeout?: number;
  rpId?: string;
  allowCredentials?: DescriptorJSON[];
  userVerification?: UserVerificationRequirement;
}

export function toCreationOptions(o: CreationOptionsJSON): PublicKeyCredentialCreationOptions {
  return {
    ...o,
    challenge: b64urlToBytes(o.challenge),
    user: { ...o.user, id: b64urlToBytes(o.user.id) },
    excludeCredentials: descriptors(o.excludeCredentials),
  };
}

export function toRequestOptions(o: RequestOptionsJSON): PublicKeyCredentialRequestOptions {
  return { ...o, challenge: b64urlToBytes(o.challenge), allowCredentials: descriptors(o.allowCredentials) };
}

export function attestationToJSON(cred: PublicKeyCredential) {
  const r = cred.response as AuthenticatorAttestationResponse;
  return {
    id: cred.id, // already base64url
    rawId: bytesToB64url(cred.rawId),
    type: "public-key" as const,
    response: {
      clientDataJSON: bytesToB64url(r.clientDataJSON),
      attestationObject: bytesToB64url(r.attestationObject),
      transports: typeof r.getTransports === "function" ? r.getTransports() : undefined,
    },
  };
}

export function assertionToJSON(cred: PublicKeyCredential) {
  const r = cred.response as AuthenticatorAssertionResponse;
  return {
    id: cred.id,
    rawId: bytesToB64url(cred.rawId),
    type: "public-key" as const,
    response: {
      clientDataJSON: bytesToB64url(r.clientDataJSON),
      authenticatorData: bytesToB64url(r.authenticatorData),
      signature: bytesToB64url(r.signature),
      userHandle: r.userHandle ? bytesToB64url(r.userHandle) : undefined,
    },
  };
}
