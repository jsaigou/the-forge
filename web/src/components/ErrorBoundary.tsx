import { Component, type ErrorInfo, type ReactNode } from "react";

// There was no ErrorBoundary anywhere in the app before this (verified: zero
// hits for ErrorBoundary/componentDidCatch across web/src). Any render throw
// unmounted the whole React tree, leaving the dark page background — the
// reported "ComfyUI play/stop blanks the screen, needs a refresh" symptom.
// This boundary wraps the active tab (App.tsx) so a throw in one page can't
// take down the whole shell (topbar/tabs stay usable), and gives the user a
// visible error + a way back in without a hard reload.
//
// Dashboard cost/savings sprint, Phase 5: `resetKeys` lets a caller mount one
// boundary per sub-tab (e.g. Dashboard's four tabs) and have it auto-clear
// when the key changes (tab switch), instead of the error state persisting
// until "Try again" is clicked — see joyful-splashing-moonbeam.md Phase 5.
interface Props {
  children: ReactNode;
  resetKeys?: unknown[];
}

interface State {
  error: Error | null;
}

export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    // Logged to the console (not swallowed) so a live repro session can read
    // the component stack via read_console_messages / devtools.
    // eslint-disable-next-line no-console
    console.error("[ErrorBoundary] caught render error:", error, info.componentStack);
  }

  componentDidUpdate(prevProps: Props) {
    if (!this.state.error) return;
    const prev = prevProps.resetKeys;
    const next = this.props.resetKeys;
    if (!next) return;
    const changed = !prev || prev.length !== next.length || next.some((k, i) => k !== prev[i]);
    if (changed) this.reset();
  }

  private reset = () => this.setState({ error: null });

  render() {
    if (this.state.error) {
      return (
        <section className="page">
          <div className="card" style={{ borderLeft: "3px solid var(--crit)", padding: 16 }}>
            <div style={{ fontSize: 13, fontWeight: 600, color: "var(--crit)", marginBottom: 8 }}>
              Something went wrong rendering this page
            </div>
            <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 4, fontFamily: "var(--mono)" }}>
              {this.state.error.message}
            </div>
            <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 14 }}>
              The rest of the app is unaffected — you can try again or switch tabs.
            </div>
            <button className="btn primary" onClick={this.reset}>Try again</button>
          </div>
        </section>
      );
    }
    return this.props.children;
  }
}
