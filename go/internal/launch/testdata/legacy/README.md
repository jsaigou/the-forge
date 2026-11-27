# Frozen historical oracles: NOT launchers

`start-a1.sh` .. `start-a4.sh` here are byte-for-byte copies of the per-slot launcher scripts that production ran before the generic shim
(ADR-0026, Radiance phase 1b step e). They were moved out of `go/internal/smith/launchers/` (which now embeds the 3-line shim) so the golden
tests in `golden_test.go` keep a real, independent oracle for what the builder must reproduce.

- Nothing installs, embeds or executes these as a launcher. Do not copy them to a host.
- Do not edit them. `TestLegacyOraclesAreFrozen` pins their sha256; changing one means changing what "correct" means for the builder, which needs an ADR.
- On ForgeHost the same originals are kept as `/usr/local/lib/forge/start-aN.sh.pre-shim` (rollback).
