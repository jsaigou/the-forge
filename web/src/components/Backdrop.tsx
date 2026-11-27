import { useEffect, useRef } from "react";

// Page background: stripe layers, one per band, (theme.css .backdrop) that drift at different speeds as the page
// scrolls (the voice-studio parallax), plus, on the aurora theme only, three slow "distant star" particles.

// Two sets of five bands, one layer per band (theme.css .backdrop, same order). The first five are the top-left set and move DOWN with the
// scroll; the last five are the bottom-right set and move UP against it. Within a set every band has its own speed (no two alike), and one band
// per set (the inner edge of the set, 4th of the top set, 1st of the bottom set) is much faster than the rest, so within a short scroll it pulls
// away from its neighbours and the background shows through the gap.
const SPEEDS = [0.5, 0.3, 0.15, 1.21, 0.08, -0.73, -0.1, -0.22, -0.34, -0.48];

interface Star {
  x: number; y0: number; vx: number; slope: number; arc: number; r: number; t: number;
  f1: number; p1: number; f2: number; p2: number; f3: number; p3: number;
}

function spawn(s: Partial<Star>, w: number, h: number, initial: boolean): Star {
  const sign = Math.random() < 0.5 ? -1 : 1;
  return Object.assign(s, {
    x: initial ? Math.random() * w : -10,
    y0: h * (0.2 + Math.random() * 0.6),
    vx: 3 + Math.random() * 4,
    // Half fly straight (a slight tilt), half bow in one gentle arc.
    slope: (Math.random() - 0.5) * 0.22,
    arc: Math.random() < 0.5 ? 0 : sign * (0.06 + Math.random() * 0.1),
    r: 0.7 + Math.random() * 0.5,
    t: Math.random() * 200,
    f1: 0.12 + Math.random() * 0.25, p1: Math.random() * 6.28,
    f2: 0.45 + Math.random() * 0.7, p2: Math.random() * 6.28,
    f3: 1.3 + Math.random() * 1.5, p3: Math.random() * 6.28,
  }) as Star;
}

export function Backdrop() {
  const layers = useRef<HTMLElement[]>([]);
  const canvas = useRef<HTMLCanvasElement>(null);

  // Parallax: translate each layer by scroll * speed, capped at the layer's overhang (above and below the viewport).
  useEffect(() => {
    const reduce = matchMedia("(prefers-reduced-motion: reduce)");
    const root = document.documentElement;
    const update = () => {
      const y = reduce.matches ? 0 : window.scrollY || 0;
      const cap = parseFloat(getComputedStyle(root).getPropertyValue("--stripe-period") || "1080") * 1.41421356;
      layers.current.forEach((l, i) => {
        if (l) l.style.transform = `translate3d(0, ${Math.max(-cap, Math.min(cap, y * SPEEDS[i]))}px, 0)`;
      });
    };
    update();
    window.addEventListener("scroll", update, { passive: true });
    window.addEventListener("resize", update);
    const mo = new MutationObserver(update);
    mo.observe(root, { attributes: true, attributeFilter: ["data-theme"] });
    return () => {
      window.removeEventListener("scroll", update);
      window.removeEventListener("resize", update);
      mo.disconnect();
    };
  }, []);

  // Stars: aurora only. Brightness is three slow incommensurate sines, so each star spends stretches fully
  // dark and fades back at irregular intervals.
  useEffect(() => {
    const cv = canvas.current;
    const cx = cv?.getContext("2d");
    if (!cv || !cx) return;
    const root = document.documentElement;
    const reduce = matchMedia("(prefers-reduced-motion: reduce)").matches;
    let w = 0, h = 0, raf = 0, on = false;
    let cols = ["#fff", "#fff", "#fff"];
    const stars: Star[] = [];
    const fit = () => {
      const dpr = Math.min(2, window.devicePixelRatio || 1);
      w = window.innerWidth; h = window.innerHeight;
      cv.width = w * dpr; cv.height = h * dpr;
      cx.setTransform(dpr, 0, 0, dpr, 0, 0);
    };
    const sync = () => {
      on = root.dataset.theme === "aurora";
      cv.hidden = !on;
      if (on) {
        const cs = getComputedStyle(root);
        cols = [1, 2, 3].map((n) => cs.getPropertyValue(`--star-${n}`).trim() || "#fff");
        if (!raf) raf = requestAnimationFrame(frame);
      }
    };
    const frame = () => {
      raf = 0;
      if (!on) { cx.clearRect(0, 0, w, h); return; }
      cx.clearRect(0, 0, w, h);
      stars.forEach((s, i) => {
        if (!reduce) { s.t += 0.016; s.x += s.vx * 0.016; }
        if (s.x > w + 10) spawn(s, w, h, false);
        const u = (s.x + 10) / (w + 20);
        const y = s.y0 + (s.slope * u + s.arc * 4 * u * (1 - u)) * h;
        const n = 0.5 * Math.sin(s.t * s.f1 + s.p1) + 0.35 * Math.sin(s.t * s.f2 + s.p2) + 0.15 * Math.sin(s.t * s.f3 + s.p3);
        let b = Math.min(1, Math.max(0, (n + 0.12) / 0.55));
        b = b * b * (3 - 2 * b);
        if (b < 0.01) return;
        const c = cols[i];
        const g = cx.createRadialGradient(s.x, y, 0, s.x, y, 7);
        g.addColorStop(0, c); g.addColorStop(0.15, c); g.addColorStop(1, "rgba(0,0,0,0)");
        cx.globalAlpha = 0.3 * b; cx.fillStyle = g; cx.beginPath(); cx.arc(s.x, y, 7, 0, 6.283); cx.fill();
        cx.globalAlpha = b; cx.fillStyle = c; cx.beginPath(); cx.arc(s.x, y, s.r, 0, 6.283); cx.fill();
      });
      cx.globalAlpha = 1;
      if (!reduce) raf = requestAnimationFrame(frame);
    };
    fit();
    for (let k = 0; k < 3; k++) stars.push(spawn({}, w, h, true));
    const onResize = () => fit();
    window.addEventListener("resize", onResize);
    const mo = new MutationObserver(sync);
    mo.observe(root, { attributes: true, attributeFilter: ["data-theme"] });
    sync();
    return () => {
      window.removeEventListener("resize", onResize);
      mo.disconnect();
      if (raf) cancelAnimationFrame(raf);
    };
  }, []);

  return (
    <div className="backdrop" aria-hidden="true">
      {SPEEDS.map((_, i) => <i key={i} ref={(el) => { if (el) layers.current[i] = el; }} />)}
      <canvas ref={canvas} hidden />
    </div>
  );
}
