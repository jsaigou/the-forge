-- Drop the infra-service catalog rows seeded by the since-deleted migration
-- 0050 (STT/Embedding/Aligner/TTS). merged_config.go promotes every catalog
-- services row into a Type="service" mode, and handleInfraServices then
-- emitted each of those twice on the Console — once as the fixed [ports]
-- systemd row and once as a service_mode row. The fixed always-on services
-- are ports-owned; the catalog services table should only hold true service
-- modes (e.g. comfyui). No-op on databases where 0050 never ran.
DELETE FROM services WHERE name IN ('STT', 'Embedding', 'Aligner', 'TTS');
