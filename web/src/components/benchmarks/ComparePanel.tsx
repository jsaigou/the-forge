// Side-by-side comparison of up to four rows; the best value in each line is highlighted.
import { useTranslation } from "react-i18next";
import { formatCell, formatContext, SCORE_METRICS, type BenchRow } from "../../lib/benchmarkRows";
import { appLocale } from "../../lib/format";

export function ComparePanel({ rows, onClear }: { rows: BenchRow[]; onClear: () => void }) {
  const { t } = useTranslation("settings");
  const locale = appLocale();
  // The deepest point of the sweep: speed with the context nearly full.
  const full = (r: BenchRow) => r.profile?.depth_benchmarks?.length ? r.profile.depth_benchmarks[r.profile.depth_benchmarks.length - 1] : undefined;
  const one = (v: number) => v.toLocaleString(locale, { minimumFractionDigits: 1, maximumFractionDigits: 1 });

  const lines: { label: string; lower?: boolean; get: (r: BenchRow) => { value: number | null; text: string } }[] = [
    { label: t("benchmarks.table.col.ctx"), get: (r) => ({ value: r.ctx, text: formatContext(r.ctx) }) },
    { label: t("benchmarks.table.cmp.mem"), lower: true, get: (r) => ({ value: r.cells.mem.value, text: formatCell("mem", r.cells.mem, locale) }) },
    { label: t("benchmarks.table.cmp.pf_empty"), get: (r) => ({ value: r.cells.pf.value, text: formatCell("pf", r.cells.pf, locale) }) },
    { label: t("benchmarks.table.cmp.pf_full"), get: (r) => ({ value: full(r)?.pp2048_tps ?? null, text: full(r) ? Math.round(full(r)!.pp2048_tps).toLocaleString(locale) : "–" }) },
    { label: t("benchmarks.table.cmp.dc_empty"), get: (r) => ({ value: r.cells.dc.value, text: formatCell("dc", r.cells.dc, locale) }) },
    { label: t("benchmarks.table.cmp.dc_full"), get: (r) => ({ value: full(r)?.tg128_tps ?? null, text: full(r) ? one(full(r)!.tg128_tps) : "–" }) },
    ...SCORE_METRICS.map((m) => ({ label: t(`benchmarks.table.col.${m}`), get: (r: BenchRow) => ({ value: r.cells[m].value, text: formatCell(m, r.cells[m], locale) }) })),
  ];
  const columns = { gridTemplateColumns: `minmax(150px, 220px) repeat(${rows.length}, minmax(130px, 1fr))` };

  return (
    <div className="card bench-compare">
      <div style={{ display: "flex", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
        <strong>{t("benchmarks.table.comparing", { count: rows.length })}</strong>
        <span className="bench-dim">{t("benchmarks.table.best_hint")}</span>
        <button type="button" className="btn cat-chip-btn" style={{ marginLeft: "auto" }} onClick={onClear}>{t("benchmarks.table.clear")}</button>
      </div>
      <div style={{ overflowX: "auto" }}>
        <div className="bench-cmp-row" style={columns}>
          <span />
          {rows.map((r) => <span key={r.key} className={`bench-cmp-head${r.remote ? " offer" : ""}`}>{r.name}</span>)}
        </div>
        {lines.map((line) => {
          const cells = rows.map(line.get);
          const have = cells.map((c) => c.value).filter((v): v is number => v !== null);
          const top = Math.max(0, ...have);
          const best = have.length > 1 ? (line.lower ? Math.min(...have) : top) : null;
          return (
            <div key={line.label} className="bench-cmp-row" style={columns}>
              <span className="bench-dim">{line.label}</span>
              {cells.map((c, i) => (
                <span key={rows[i].key} className={`bench-cmp-cell${c.value !== null && c.value === best ? " best" : ""}`}>
                  <span className="mono">{c.text}</span>
                  <span className="track"><span style={{ width: c.value !== null && top > 0 ? `${Math.round((c.value / top) * 100)}%` : 0 }} /></span>
                </span>
              ))}
            </div>
          );
        })}
      </div>
    </div>
  );
}
