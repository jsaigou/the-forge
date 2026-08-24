-- SPDX-License-Identifier: Apache-2.0
-- Renames inference-slot internal identifiers "primary"→"a1",
-- "secondary"→"a2" to match the existing "a3"/"a4" convention. Slots have
-- always had a dual naming scheme (internal key vs. conceptual/Tailscale
-- name a1/a2/a3/a4, docs/deployment.md, docs/design.md); a3/a4 never
-- needed a second name because their key already matched. This is a pure
-- data-value rename (service names are data, no schema/API shape change),
-- same pattern as 0003_headroom_rename.sql. Display labels (A1-A4) and
-- ports (8080/8081/8087/8088) are untouched.
--
-- Touch points: slots.name + .unit (the canonical slot registry),
-- slot_state.slot (live/last-known occupancy), sched_queue.target_slot
-- (queued placements), reservations.bay (bay reservations), and
-- usage_events.slot (historical event rows, renamed too for query
-- consistency — "tokens by slot" shouldn't fragment across old/new names).

UPDATE slots SET name = 'a1', unit = 'foundry-a1' WHERE name = 'primary';
UPDATE slots SET name = 'a2', unit = 'foundry-a2' WHERE name = 'secondary';

UPDATE slot_state SET slot = 'a1' WHERE slot = 'primary';
UPDATE slot_state SET slot = 'a2' WHERE slot = 'secondary';

UPDATE sched_queue SET target_slot = 'a1' WHERE target_slot = 'primary';
UPDATE sched_queue SET target_slot = 'a2' WHERE target_slot = 'secondary';

UPDATE reservations SET bay = 'a1' WHERE bay = 'primary';
UPDATE reservations SET bay = 'a2' WHERE bay = 'secondary';

UPDATE usage_events SET slot = 'a1' WHERE slot = 'primary';
UPDATE usage_events SET slot = 'a2' WHERE slot = 'secondary';
