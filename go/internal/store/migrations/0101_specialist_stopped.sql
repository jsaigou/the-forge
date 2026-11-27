-- SPDX-License-Identifier: Apache-2.0
-- ADR-0022: an operator stop of an always-on specialist must survive a daemon restart (it used to live in memory only, so a restart silently
-- started the member again). stopped=1 means "the operator stopped it; leave it stopped until it is started again". Additive.
ALTER TABLE specialist_occupants ADD COLUMN stopped INTEGER NOT NULL DEFAULT 0;
