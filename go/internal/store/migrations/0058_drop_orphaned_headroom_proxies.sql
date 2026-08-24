-- Sprint 6 (v1.1 post-launch hardening, docs/v5-headroom-replacement.md): the
-- aiand/qwencloud headroom_proxies rows were orphaned (orphaned_at set) at
-- the exact moment the shared "external" foundry-compress proxy was created
-- during Sprint 3 cutover -- both providers' router_providers.headroom_proxy
-- link is empty, and their legacy headroom@aiand/headroom@qwencloud units no
-- longer exist. Their historical sample/savings rows are real production
-- data (aiand alone: 1448 headroom_samples, 3997 headroom_label_samples,
-- 1448 headroom_savings), so children are deleted explicitly before the
-- parent rows to satisfy the ON DELETE RESTRICT FKs -- this is a deliberate
-- decision to discard that history, not a side effect of a cascade.
DELETE FROM headroom_label_samples WHERE proxy_id IN (
    SELECT id FROM headroom_proxies WHERE service IN ('aiand', 'qwencloud')
);
DELETE FROM headroom_samples WHERE proxy_id IN (
    SELECT id FROM headroom_proxies WHERE service IN ('aiand', 'qwencloud')
);
DELETE FROM headroom_savings WHERE proxy_id IN (
    SELECT id FROM headroom_proxies WHERE service IN ('aiand', 'qwencloud')
);
DELETE FROM headroom_proxies WHERE service IN ('aiand', 'qwencloud');
