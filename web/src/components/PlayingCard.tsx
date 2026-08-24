import type { HTMLAttributes, ReactNode } from "react";
import { creatorIconSlug } from "../lib/creatorIcon";
import type { Badge } from "../lib/types";
import { BadgeCluster } from "./BadgeCluster";
import { Icon } from "./Icon";
import { Logo } from "./Logo";

// PlayingCard — the shared playing-card chrome for the model catalog
// (playing-card redesign 2026-08-14). Wraps any card body in:
//
//   - a solid card-stock face with a stacked-shadow 3D EDGE (card thickness),
//   - a double inner frame interrupted at top-center by the NAMEPLATE, which
//     carries the model/family name prominently,
//   - the model/family logo TOP-LEFT, capped at ≤30% of the card width
//     (.pcard-logo is 27% wide; badges cluster beneath it),
//   - the COMPANY mark BOTTOM-RIGHT as a grayscale, 50%-alpha watermark —
//     the vendored brand mark when the creator has one, else the
//     letter-badge fallback, both de-colored by the same filter.
//
// Callers own interactivity: pass role/tabIndex/onClick through rootProps
// (the chrome itself is inert). The face keeps the strict 5:7 ratio via the
// `portrait` / `umc-card` className variants — see theme.css.
export function PlayingCard({
  name,
  logo,
  logoDark,
  badges = [],
  watermarkName = "",
  className = "",
  nameExtra,
  children,
  rootProps,
}: {
  name: string;
  logo: string;
  logoDark?: string;
  badges?: Badge[];
  watermarkName?: string;
  className?: string;
  nameExtra?: ReactNode;
  children?: ReactNode;
  rootProps?: HTMLAttributes<HTMLDivElement>;
}) {
  const wmSlug = creatorIconSlug(watermarkName);
  return (
    <div className={`pcard ${className}`.trim()} {...rootProps}>
      <div className="pcard-face">
        <div className="pcard-frame" aria-hidden="true" />
        <div className="pcard-logo" aria-hidden="true">
          <BadgeCluster logo={logo} logoDark={logoDark} name={name} badges={badges} />
        </div>
        <div className="pcard-nameplate">
          <span className="pcard-name" title={name}>{name}</span>
          {nameExtra}
        </div>
        {watermarkName && (
          <div className="pcard-mark" aria-hidden="true" title={watermarkName}>
            {wmSlug ? (
              <Icon slug={wmSlug} name={watermarkName} />
            ) : (
              <Logo slug={watermarkName.toLowerCase()} name={watermarkName} />
            )}
          </div>
        )}
        <div className="pcard-body">{children}</div>
      </div>
    </div>
  );
}
