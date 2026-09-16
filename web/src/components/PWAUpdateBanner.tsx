/// <reference types="vite-plugin-pwa/react" />
import { useRegisterSW } from "virtual:pwa-register/react";

// PWAUpdateBanner — closes a real gap found live 2026-09-14: this app's
// service worker was registered with the vite-plugin-pwa default bare
// <script> (navigator.serviceWorker.register() and nothing else), which has
// no update-detection logic at all. A new build lands on the server and the
// SW picks it up in the background, but an already-open tab is never told
// — it just keeps running the stale JS it already loaded, indefinitely.
// Three real UI changes in one session were deployed and curl-verified
// correct on the server while looking unchanged in an open tab, purely
// because of this. See vite.config.ts's injectRegister:false comment.
//
// Fix: register manually via virtual:pwa-register/react's useRegisterSW,
// poll for a new service worker every 15s (this is a low-traffic internal
// ops console behind Tailscale — a short interval costs nothing and this
// tab is often left open for a whole working session), and surface a small
// non-blocking "reload now" prompt rather than forcing a reload — an
// operator mid-edit in a form shouldn't lose it without warning.
export function PWAUpdateBanner() {
  const {
    needRefresh: [needRefresh, setNeedRefresh],
    updateServiceWorker,
  } = useRegisterSW({
    onRegisteredSW(_url, registration) {
      if (!registration) return;
      const poll = () => registration.update().catch(() => {});
      setInterval(poll, 15_000);
    },
  });

  if (!needRefresh) return null;

  return (
    <div
      className="card"
      style={{
        position: "fixed", bottom: 16, right: 16, zIndex: 200, width: 280,
        boxShadow: "0 8px 24px rgba(0,0,0,0.35)",
      }}
    >
      <div style={{ fontSize: 12.5, marginBottom: 10 }}>
        A new version of this app is ready — reload to pick it up.
      </div>
      <div style={{ display: "flex", gap: 8 }}>
        <button className="btn" onClick={() => setNeedRefresh(false)}>Later</button>
        <button className="go" style={{ flex: 1 }} onClick={() => updateServiceWorker(true)}>
          Reload now
        </button>
      </div>
    </div>
  );
}
