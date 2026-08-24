// ScanFrame — Sprint K, amicro.vercel.app's "Face ID" (Text & Interface): a
// rounded-square scan-frame icon with a pulsing inner ring. Replaces
// ProfilingPanel's plain 7×7px dot-busy pulse — a profiling run is a much
// longer-lived, more consequential state (evicts every loaded model) than
// the small dots used elsewhere for ordinary loading/busy indicators, so it
// gets its own more deliberate throbber.
export function ScanFrame({ title }: { title?: string }) {
  return (
    <span className="scan-frame" role="status" aria-label={title ?? "profiling in progress"} title={title}>
      <span className="scan-corner tl" />
      <span className="scan-corner tr" />
      <span className="scan-corner bl" />
      <span className="scan-corner br" />
      <span className="scan-ring" />
    </span>
  );
}
