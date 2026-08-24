import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { CopyButton } from "../components/CopyButton";
import type { ReactNode } from "react";
import { useOnboarding } from "./useOnboarding";

// Guided first-run tour (P4) — a stepped modal card, not a full wizard
// page: the operator can dismiss at any point and the dashboard underneath
// stays exactly as it was. Chrome reuses the app's existing modal pair
// (.modal-backdrop / .modal, same as LoadConfirmModal/DetailModal —
// portaled to document.body for the same transformed-ancestor reasons).
//
// Escape closes = dismisses (sets the done flag), matching DetailModal's
// Esc convention. Backdrop clicks intentionally do NOT dismiss — a stray
// click shouldn't silently skip an operator's only introduction to Smith,
// reservations, and provider keys; there are explicit Skip/Done buttons.
//
// CTAs are plain hash anchors (#models, #help/smith, …) so the existing
// tab router handles navigation with zero new plumbing; clicking one also
// completes the tour.

interface Step {
  title: string;
  body: ReactNode;
  cta?: { label: string; href: string };
}

const A0_BASE_URL = "http://<host>:8085/v1";

const STEPS: Step[] = [
  {
    title: "Welcome to The Forge",
    body: (
      <>
        <p>
          The Forge runs a local inference fleet from a <b>single Go binary</b>: it loads models onto this
          box's GPU on demand, routes to remote providers through the same OpenAI-compatible interface,
          and serves one dashboard for all of it.
        </p>
        <p>This short tour covers the four things worth knowing on day one.</p>
      </>
    ),
    cta: { label: "Explore now →", href: "#models" },
  },
  {
    title: "Models & slots",
    body: (
      <>
        <p>
          Models load <b>on demand</b> across four load bays (a1–a4) — request a model through a0 and the
          scheduler places it, evicting something else if memory is tight. You never load by hand unless
          you want to.
        </p>
        <p>
          The <a href="#models">Models</a> tab is the full catalog. On the host, <code>forge tui</code>{" "}
          gives you a full-screen ops console for the same machinery.
        </p>
      </>
    ),
    cta: { label: "Open Models →", href: "#models" },
  },
  {
    title: "Smith, your maintenance agent",
    body: (
      <>
        <p>
          <b>Smith</b> is the built-in maintenance agent: it runs checks hourly/daily, watches for drift
          and failures, and proposes fixes. You can also just ask it things in plain language.
        </p>
        <p>Find it under Help → “Ask the smith”, and its automation settings under Settings → Smith.</p>
      </>
    ),
    cta: { label: "Ask the smith →", href: "#help/smith" },
  },
  {
    title: "One endpoint: a0",
    body: (
      <>
        <p>
          Agents and apps talk to <b>a0</b> — a single OpenAI-compatible endpoint that reaches every
          configured model, local or remote. Point any OpenAI client at it:
        </p>
        <div className="onboarding-endpoint">
          <code>{A0_BASE_URL}</code>
          <CopyButton text={A0_BASE_URL} title="Copy base URL" />
        </div>
        <p>
          Requests made from inside the tailnet need no API key; external callers use an{" "}
          <code>sk-router-*</code> key from Settings → Security.
        </p>
      </>
    ),
  },
  {
    title: "Scheduling & reservations",
    body: (
      <>
        <p>
          The <a href="#scheduling">Scheduling</a> tab holds two ideas: <b>reservations</b>, which pin a
          bay for a window (yours or another agent's), and <b>scheduled jobs</b>, which force-load models
          off-peak so daytime requests never pay the cold-start cost.
        </p>
      </>
    ),
    cta: { label: "Open Scheduling →", href: "#scheduling" },
  },
  {
    title: "Provider keys & Compressor",
    body: (
      <>
        <p>
          Add remote providers (and their keys) under{" "}
          <a href="#settings/providers">Settings → Providers</a>; they then appear alongside local models
          behind a0 automatically.
        </p>
        <p>
          Before anything leaves the box, <b>Compressor</b> token-compresses prompts against remote
          providers — check its realized savings on the Dashboard's Cost tab.
        </p>
      </>
    ),
  },
];

export function OnboardingTour() {
  const { open, dismiss } = useOnboarding();
  const [stepIdx, setStepIdx] = useState(0);
  const dialogRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    // Basic focus management: move focus into the dialog on open so Escape
    // and Enter land here rather than on whatever tab was last focused.
    dialogRef.current?.focus();
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") {
        e.preventDefault();
        dismiss();
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, dismiss]);

  if (!open) return null;

  const clampedIdx = Math.min(stepIdx, STEPS.length - 1);
  const step = STEPS[clampedIdx];
  const isFinal = clampedIdx === STEPS.length - 1;

  return createPortal(
    <div className="modal-backdrop">
      <div
        ref={dialogRef}
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="onboarding-title"
        tabIndex={-1}
        onClick={(e) => e.stopPropagation()}
        style={{ maxWidth: 460 }}
      >
        <div style={{ fontSize: 11, color: "var(--text-dim)", marginBottom: 6 }}>
          Getting started · {clampedIdx + 1} of {STEPS.length}
        </div>
        <h3 id="onboarding-title">{step.title}</h3>
        <div style={{ fontSize: 13, color: "var(--text-dim)", lineHeight: 1.6 }}>{step.body}</div>
        <div className="form-actions" style={{ marginTop: 18 }}>
          {!isFinal && (
            <button className="btn" disabled={clampedIdx === 0} onClick={() => setStepIdx(clampedIdx - 1)}>
              Back
            </button>
          )}
          {!isFinal && (
            <button className="btn primary" onClick={() => setStepIdx(clampedIdx + 1)}>
              Next
            </button>
          )}
          {isFinal && (
            <button className="btn primary" autoFocus onClick={dismiss}>
              Done
            </button>
          )}
          <span style={{ marginLeft: "auto", display: "flex", gap: 8, alignItems: "center" }}>
            {step.cta && (
              <a className="btn" href={step.cta.href} onClick={dismiss}>
                {step.cta.label}
              </a>
            )}
            {!isFinal && (
              <button className="icon-btn" title="Skip tour" aria-label="Skip tour" onClick={dismiss}>
                ✕
              </button>
            )}
          </span>
        </div>
      </div>
    </div>,
    document.body,
  );
}
