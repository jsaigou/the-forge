// Shared pill-button range/scope toggle — pre-release feedback round, Phase 5
// (2026-08-12). Before this, Dashboard.tsx and Console's activity heatmap
// each hand-rolled the identical button-row JSX for three unrelated option
// lists (HEATMAP_SCOPES, COST_RANGES, RANGES) — one implementation now
// backs all of them, so a future range list only needs its own options
// array, not another copy of the markup.
export interface RangeOption<K extends string> {
  key: K;
  label: string;
}

export function RangeToggle<K extends string>({
  options,
  value,
  onChange,
}: {
  options: readonly RangeOption<K>[];
  value: K;
  onChange: (key: K) => void;
}) {
  return (
    <div className="sort-toggle" style={{ marginLeft: "auto", display: "flex", gap: 4 }}>
      {options.map((o) => (
        <button
          key={o.key}
          className={`tab ${value === o.key ? "active" : ""}`}
          style={{ fontSize: 11, padding: "3px 10px" }}
          onClick={() => onChange(o.key)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
