-- SPDX-License-Identifier: Apache-2.0
-- The quantization name of the Radiance 2.x container of Qwen3.6 (docs/adr/0033-radiance-2x-yaml-launch.md). Additive only.
--
-- IQ8R is upstream Radiance's rotated-int8 format; the container stores its trunk that way and its routed experts as a 4-bit codebook, so neither
-- FP8 (the 1.17 containers) nor any GGUF quantization describes it. Quantization names arrive by migration with the model that needs them (the
-- table is GET-only through the API). INSERT OR IGNORE: a row an operator already added under this name keeps its id.
INSERT OR IGNORE INTO quantizations (name) VALUES ('IQ8R_W4');
