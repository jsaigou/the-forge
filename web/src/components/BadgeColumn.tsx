import type { Badge } from "../lib/types";
import { Icon } from "./Icon";

const MAX_COLUMN_BADGES = 4;

// BadgeColumn — Sprint K operator addition: on config cards, badges move
// off BadgeCluster's icon-anchored cluster (Sprint J2's "icon, then a chip
// row below it") into a vertical column between the icon and the name/
// creator text. Deliberately a separate component from BadgeCluster rather
// than a layout prop on it — portrait model cards (ModelCardView) keep
// J2's stacked cluster untouched, and the two don't share a DOM shape
// (this one doesn't render the icon itself; the caller places it).
// Same glyph-only filtering + cap as BadgeCluster: text-pill badges
// (unrecognized key_features, Icon.icon === "") stay out of card faces —
// those live in the plain-list contexts (detail views, Bay.tsx).
export function BadgeColumn({ badges }: { badges: Badge[] }) {
  const glyphs = badges.filter((b) => b.icon).slice(0, MAX_COLUMN_BADGES);
  if (glyphs.length === 0) return null;
  return (
    <div className="bcolumn">
      {glyphs.map((b) => (
        <span key={b.id} className="bchip" title={b.label}>
          <Icon slug={b.icon} name={b.label} />
        </span>
      ))}
    </div>
  );
}
