import type { Badge } from "../lib/types";
import { Icon } from "./Icon";

// §0.7: render a badge as an inline icon (vendored SVG via <Icon>) with a
// hover tooltip. Badges with an empty icon slug (unknown features) fall
// back to a generic text pill. Extracted (product/QA sprint, 2026-07-29)
// from ConfigCardView/ModelCardView's identical private copies so Bay.tsx
// can render real badge icons too, instead of just a count.
export function BadgeIcon({ badge }: { badge: Badge }) {
  if (badge.icon) {
    return <Icon slug={badge.icon} name={badge.label} sm />;
  }
  return (
    <span className="kf" title={badge.label}>
      {badge.label}
    </span>
  );
}
