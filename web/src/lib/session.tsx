import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { api, setCsrfToken } from "./api";
import type { Role, SessionInfo } from "./types";

interface SessionState extends SessionInfo {
  canOperate: boolean; // operator or admin
  canAdmin: boolean;
}

const SessionContext = createContext<SessionState | null>(null);

function deriveRole(role: Role): { canOperate: boolean; canAdmin: boolean } {
  return { canOperate: role === "operator" || role === "admin", canAdmin: role === "admin" };
}

export function SessionProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<SessionState | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api
      .session()
      .then((s) => {
        if (cancelled) return;
        setCsrfToken(s.csrf_token);
        setSession({ ...s, ...deriveRole(s.role) });
      })
      .catch((e) => {
        if (!cancelled) setError(e instanceof Error ? e.message : "session bootstrap failed");
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (error) {
    return <div className="boot-error">Couldn't reach the Forge API: {error}</div>;
  }
  if (!session) {
    return <div className="boot-loading">Loading…</div>;
  }
  return <SessionContext.Provider value={session}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionState {
  const ctx = useContext(SessionContext);
  if (!ctx) throw new Error("useSession must be used within SessionProvider");
  return ctx;
}
