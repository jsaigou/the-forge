# Recovery-code entropy: 114 bits, above the NIST 800-63B fast-hash threshold

## Context

Recovery codes are emergency fallback secrets for users locked out of password+TOTP.
They are hashed at rest with HMAC-SHA256 keyed by a server pepper - a fast, approved
one-way function - NOT a memory-hard password KDF (Argon2id).

The original implementation drew 16 random bytes (128 bits) and base64-encoded them, but the
formatting step truncated to 16 characters for readability (`XXXX-XXXX-XXXX-XXXX`). 16 base64
characters carry only **96 bits** of entropy. The design had justified using a fast hash by
citing NIST SP 800-63B §5.1.2's ≥112-bit threshold for look-up secrets - but 96 < 112, so the
justification was factually broken.

## Decision

Raise the truncation from 16 to 19 base64 characters (114 bits ≥ the 112-bit NIST threshold).
This is a one-line change that makes the fast-hash justification valid. The formatted code
becomes `XXXX-XXXX-XXXX-XXXX-XXX` (19 characters + 4 dashes = 23 characters).

**Non-breaking:** verification hashes whatever the user types and constant-time-compares
against stored hashes. Old 16-character codes already issued still verify correctly (they're
HMAC of 16-character strings). Only newly generated codes get the 19-character format. No
migration, no lockout risk.

The alternative - switching to Argon2id - was rejected because:
1. It breaks all existing stored hashes (HMAC → Argon2 output), invalidating every existing
   recovery code - a lockout risk for users whose last resort is a recovery code.
2. It's unnecessary at 114 bits: the NIST guidance prescribes memory-hard KDFs for sub-112-bit
   secrets; raising entropy above the threshold makes the fast-hash path legitimately justified.
3. Argon2id in the verification path is an attacker-triggerable DoS amplifier: each wrong-code
   submission would run a memory-hard hash against every stored code.

## Consequences

- New recovery codes are 23 characters (19 base64 + 4 dashes), up from 20.
- The fast-hash (HMAC-SHA256) path is retained and now correctly justified.
- Existing codes remain valid until regenerated.
