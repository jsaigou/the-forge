# Recovery-code entropy: 114 bits, above the NIST 800-63B fast-hash threshold

## Context

Recovery codes are emergency fallback secrets for users locked out of password+TOTP.
They are hashed at rest with HMAC-SHA256 keyed by a server pepper — a fast, approved
one-way function — NOT a memory-hard password KDF (Argon2id).

The original implementation (`authz/recovery.go`) drew 16 random bytes (128 bits)
and base64-encoded them, but `formatRecoveryCode` truncated to 16 chars for
readability (`XXXX-XXXX-XXXX-XXXX`). 16 base64 chars carry only **96 bits** of
entropy. The type doc justified using a fast hash by citing NIST SP 800-63B §5.1.2's
≥112-bit threshold for look-up secrets — but 96 < 112, so the justification was
factually broken. The code comment on the generation line said "128 bits" while the
delivered entropy was 96.

## Decision

Raise the truncation from 16 to 19 base64 chars (114 bits ≥ 112-bit NIST threshold).
This is a one-line change (`s[:16]` → `s[:19]`) that makes the fast-hash
justification valid. The formatted code becomes `XXXX-XXXX-XXXX-XXXX-XXX` (19 chars
+ 4 dashes = 23 chars).

**Non-breaking:** `VerifyCode` hashes whatever the user types and constant-time-
compares against stored hashes. Old 16-char codes already in the DB still verify
(they're HMAC of 16-char strings). Only newly generated codes get the 19-char
format. No migration, no lockout risk.

The alternative — switching to Argon2id — was rejected because:
1. It breaks all existing stored hashes (HMAC → Argon2 output), invalidating every
   existing recovery code — a lockout risk for users whose last resort is a recovery
   code.
2. It's unnecessary at 114 bits: the NIST guidance prescribes memory-hard KDFs for
   sub-112-bit secrets; raising entropy above the threshold makes the fast-hash
   path legitimately justified.
3. Argon2id in `VerifyCode` is an attacker-triggerable DoS amplifier: each wrong-code
   submission runs N × 64 MiB hashes against all stored codes.

## Consequences

- New recovery codes are 23 chars (19 base64 + 4 dashes), up from 20.
- The fast-hash (HMAC-SHA256) path is retained and now correctly justified.
- Existing codes remain valid until regenerated.
- The type doc, generation comment, and format function comment are corrected to
  state 114 bits accurately.

## Remediation

Implemented in Phase 1 of the QA remediation sprint (2026-08-13). See
`QA-REPORT.md` finding #1 and `docs/v5-qa-remediation-2026-08-13.md` Phase 1.
