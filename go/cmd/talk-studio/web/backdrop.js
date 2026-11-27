// Page background motion: the three .stripes layers drift at different speeds as the page scrolls (the voice-studio
// parallax), and on a dark theme three slow "distant star" particles cross the screen. Same module as voice-studio's.
// Colors come from the theme: --star-1..3 and --stars-on (1 on a dark theme, 0 on a light one).

const SPEEDS = [0.12, 0.28, 0.5];
const reduceMotion = matchMedia('(prefers-reduced-motion: reduce)');
const root = document.documentElement;

function parallax() {
  const layers = Array.from(document.querySelectorAll('.stripes i'));
  if (!layers.length) return;
  const update = () => {
    const y = reduceMotion.matches ? 0 : window.scrollY || 0;
    layers.forEach((l, i) => { l.style.transform = `translate3d(0, ${y * (SPEEDS[i] || 0.3)}px, 0)`; });
  };
  window.addEventListener('scroll', update, { passive: true });
  update();
}

function spawn(s, w, h, initial) {
  const sign = Math.random() < 0.5 ? -1 : 1;
  s.x = initial ? Math.random() * w : -10;
  s.y0 = h * (0.2 + Math.random() * 0.6);
  s.vx = 3 + Math.random() * 4;
  // Half fly straight (a slight tilt), half bow in one gentle arc.
  s.slope = (Math.random() - 0.5) * 0.22;
  s.arc = Math.random() < 0.5 ? 0 : sign * (0.06 + Math.random() * 0.1);
  s.r = 0.7 + Math.random() * 0.5;
  s.t = Math.random() * 200;
  s.f1 = 0.12 + Math.random() * 0.25; s.p1 = Math.random() * 6.28;
  s.f2 = 0.45 + Math.random() * 0.7; s.p2 = Math.random() * 6.28;
  s.f3 = 1.3 + Math.random() * 1.5; s.p3 = Math.random() * 6.28;
  return s;
}

function stars() {
  const host = document.querySelector('.stripes');
  if (!host) return;
  const cv = document.createElement('canvas');
  cv.setAttribute('aria-hidden', 'true');
  cv.style.cssText = 'position:absolute;inset:0;width:100%;height:100%';
  host.appendChild(cv);
  const cx = cv.getContext('2d');
  if (!cx) return;
  let w = 0, h = 0, raf = 0, on = false;
  let cols = ['#fff', '#fff', '#fff'];
  const list = [];
  const fit = () => {
    const dpr = Math.min(2, window.devicePixelRatio || 1);
    w = window.innerWidth; h = window.innerHeight;
    cv.width = w * dpr; cv.height = h * dpr;
    cx.setTransform(dpr, 0, 0, dpr, 0, 0);
  };
  const sync = () => {
    const cs = getComputedStyle(root);
    on = cs.getPropertyValue('--stars-on').trim() === '1';
    cv.hidden = !on;
    cols = [1, 2, 3].map((n) => cs.getPropertyValue('--star-' + n).trim() || '#fff');
    if (on && !raf) raf = requestAnimationFrame(frame);
  };
  // Brightness is three slow incommensurate sines, so each star spends stretches fully dark and fades back at irregular times.
  const frame = () => {
    raf = 0;
    cx.clearRect(0, 0, w, h);
    if (!on) return;
    list.forEach((s, i) => {
      if (!reduceMotion.matches) { s.t += 0.016; s.x += s.vx * 0.016; }
      if (s.x > w + 10) spawn(s, w, h, false);
      const u = (s.x + 10) / (w + 20);
      const y = s.y0 + (s.slope * u + s.arc * 4 * u * (1 - u)) * h;
      const n = 0.5 * Math.sin(s.t * s.f1 + s.p1) + 0.35 * Math.sin(s.t * s.f2 + s.p2) + 0.15 * Math.sin(s.t * s.f3 + s.p3);
      let b = Math.min(1, Math.max(0, (n + 0.12) / 0.55));
      b = b * b * (3 - 2 * b);
      if (b < 0.01) return;
      const c = cols[i];
      const g = cx.createRadialGradient(s.x, y, 0, s.x, y, 7);
      g.addColorStop(0, c); g.addColorStop(0.15, c); g.addColorStop(1, 'rgba(0,0,0,0)');
      cx.globalAlpha = 0.3 * b; cx.fillStyle = g; cx.beginPath(); cx.arc(s.x, y, 7, 0, 6.283); cx.fill();
      cx.globalAlpha = b; cx.fillStyle = c; cx.beginPath(); cx.arc(s.x, y, s.r, 0, 6.283); cx.fill();
    });
    cx.globalAlpha = 1;
    if (!reduceMotion.matches) raf = requestAnimationFrame(frame);
  };
  fit();
  for (let k = 0; k < 3; k++) list.push(spawn({}, w, h, true));
  window.addEventListener('resize', fit);
  new MutationObserver(sync).observe(root, { attributes: true, attributeFilter: ['data-theme'] });
  matchMedia('(prefers-color-scheme: light)').addEventListener('change', sync);
  sync();
}

parallax();
stars();
