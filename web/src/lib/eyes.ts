// Easter egg: switching from a dark theme to the light one flashes "MY EYES !!!" (Japanese: 目ガ！！！) for a moment.
// A decorative overlay that ignores the pointer; the flash fades out on its own (theme.css .me-eyes).
import i18next from "i18next";
import { currentLang } from "./i18n";

export function flashMeEyes(): void {
  if (document.querySelector(".me-eyes")) return;
  const el = document.createElement("div");
  el.className = "me-eyes";
  el.setAttribute("aria-hidden", "true");
  const text = document.createElement("span");
  text.textContent = i18next.t("common:eyes_flash");
  text.lang = currentLang();
  el.appendChild(text);
  document.body.appendChild(el);
  // 100 ms at full white, a 100 ms fade, gone at 200 ms. Timers, not a CSS animation: an animation can sit
  // "pending" in a slow or unfocused tab and would leave the overlay up past the 200 ms limit.
  window.setTimeout(() => el.classList.add("out"), 100);
  window.setTimeout(() => el.remove(), 200);
}
