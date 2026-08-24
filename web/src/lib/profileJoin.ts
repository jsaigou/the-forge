import type { ProfileListItem } from "./types";

// findProfileForConfig — Phase 8 (pre-release feedback sprint). Joins the
// profile list to a config by id, the real join since the Phase 6
// surrogate-key migration gave model_profiles a config_id column. Falls
// back to matching by mode name (configs.name is UNIQUE, so this was
// always correct, just fragile to a rename) for the deploy window where a
// newly-built frontend bundle meets an older backend binary that hasn't
// rolled config_id onto the wire yet.
export function findProfileForConfig(
  profiles: ProfileListItem[],
  config: { id: number; name: string },
): ProfileListItem | undefined {
  return (
    profiles.find((p) => p.config_id === config.id) ??
    profiles.find((p) => p.mode === config.name)
  );
}
