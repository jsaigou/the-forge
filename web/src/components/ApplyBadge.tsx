import type { ApplyMode } from "../settings/fields";

// ApplyBadge — Sprint 12 (was H) Phase 6. The honest per-field apply-mode
// signal (see the sprint plan's "Apply modes — four, not three" section):
// verified in code, not assumed, per field — immediate (settings-KV, read
// per request/tick) / live (ReloadConfig picks it up) / restart (stored now,
// running process never re-reads it) / seed (consulted only on a fresh boot
// where the live key is unset). Renders as a `.chip` (theme.css:448) tinted
// per mode — no new color system, reuses --ok/--cool/--warn/--text-mute.
const LABELS: Record<ApplyMode, string> = {
  immediate: "Immediate",
  live: "Live",
  restart: "Restart required",
  seed: "Boot seed",
};

const TITLES: Record<ApplyMode, string> = {
  immediate: "Takes effect on the next request — no reload or restart needed.",
  live: "Takes effect within one cycle of the running daemon picking up the change — no restart needed.",
  restart: "Stored now, but the running daemon won't use it until the next restart.",
  seed: "Only consulted on a fresh boot where the live setting has never been set — writing this changes nothing on a running system.",
};

export function ApplyBadge({ mode }: { mode: ApplyMode }) {
  if (mode === "immediate") return null;
  return (
    <span className={`chip apply-${mode}`} title={TITLES[mode]}>
      {LABELS[mode]}
    </span>
  );
}
