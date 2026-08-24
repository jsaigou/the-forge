import type { Badge } from "../lib/types";
import { Icon } from "./Icon";

const MAX_CLUSTER_BADGES = 4;

// Sprint J2: badge glyphs move off the inline `.mbadges` row next to the
// name and onto the icon itself — a small, capped row of circular chips
// overlapping the icon's bottom edge, so icon + badges read as one composed
// identity instead of competing with the name for horizontal space. Text
// -pill badges (unrecognized key_features, Icon.icon === "") are NOT
// rendered here — those stay in the plain-list contexts (detail views,
// Bay.tsx) via BadgeIcon. The backend already caps/orders glyph badges by
// priority (registry.go's deriveBadges), so slicing to MAX_CLUSTER_BADGES
// here is a display-only safety net, not where the real cap lives.
export function BadgeCluster({
  logo,
  logoDark,
  name,
  badges,
  xl,
}: {
  logo: string;
  logoDark?: string;
  name: string;
  badges: Badge[];
  xl?: boolean;
}) {
  const glyphs = badges.filter((b) => b.icon).slice(0, MAX_CLUSTER_BADGES);
  return (
    <div className={`bcluster ${xl ? "xl" : ""}`.trim()}>
      <Icon slug={logo} slugDark={logoDark} name={name} xl={xl} />
      {glyphs.length > 0 && (
        <div className="bchips">
          {glyphs.map((b) => (
            <span key={b.id} className="bchip" title={b.label}>
              <Icon slug={b.icon} name={b.label} />
            </span>
          ))}
        </div>
      )}
    </div>
  );
}
