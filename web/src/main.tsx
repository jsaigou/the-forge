import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App.tsx";
import "./index.css";
import { initI18n } from "./lib/i18n.ts";
import { SessionProvider } from "./lib/session.tsx";
import "./styles/theme.css";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 5_000,
      retry: 1,
    },
  },
});

// Resolve the active language (and, for 日本語, load its namespace bundles)
// before the first render — avoids a flash of the wrong language on boot.
initI18n().then(() => {
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <SessionProvider>
          <App />
        </SessionProvider>
      </QueryClientProvider>
    </StrictMode>,
  );
});
