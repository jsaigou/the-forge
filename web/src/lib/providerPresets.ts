// Curated provider preset registry (Sprint E, docs/v5-prerelease-readiness.md).
// Prefills the Settings → Providers "+ Add provider" form so an operator
// doesn't have to type five URLs from memory. Modeled on llamaFlags.ts's
// house pattern: a small curated table + a documented fallback (here,
// "Custom…") for anything not in it, rather than a dynamic-discovery
// mechanism. Every field below was probed live on 2026-08-04 — see
// docs/v5-prerelease-readiness.md's Sprint E section for the raw probe
// output — not guessed.
//
// IMPORTANT: `credits` must stay in sync with the dispatch switch in
// go/internal/providers/credits.go (`func (c *creditsClients) fetch`). Only
// list a preset's `credits` as anything other than "none" once a real
// parser for that provider's balance/spend shape exists there — otherwise
// a filled-in creditsUrl silently renders "balance unavailable" forever via
// the generic DeepSeek-shape fallback (genericCreditsClient), which is
// worse than admitting the provider has no balance API. As of 2026-08-04
// the real parsers are: deepseek, aiand (spend, not balance), openrouter.
export type CreditsSupport = "deepseek" | "aiand" | "openrouter" | "none";

export interface ProviderPreset {
  id: string; // prefilled provider name AND the icon slug it must match
  label: string;
  targetUrl: string;
  // Azure OpenAI has no fixed base URL — it's per-resource
  // (https://<resource>.openai.azure.com/...). targetUrl is a template the
  // operator must edit, not a working default.
  targetUrlIsTemplate?: boolean;
  statusUrl: string; // "" unless a real Statuspage /api/v2/summary.json exists
  billingConsoleUrl: string;
  billCurrency: string;
  orgIdRequired?: boolean; // aiand's Analytics API needs X-Org-ID alongside the key
  credits: CreditsSupport;
  creditsUrl: string; // "" whenever credits === "none"
  country: string;
  dataResidencyGroup: string;
  note?: string;
}

export const PROVIDER_PRESETS: ProviderPreset[] = [
  {
    id: "deepseek",
    label: "DeepSeek",
    targetUrl: "https://api.deepseek.com/v1",
    statusUrl: "https://deepseek.statuspage.io/api/v2/summary.json",
    billingConsoleUrl: "https://platform.deepseek.com/usage",
    billCurrency: "USD",
    credits: "deepseek",
    creditsUrl: "https://api.deepseek.com/user/balance",
    country: "CN",
    dataResidencyGroup: "China",
  },
  {
    id: "aiand",
    label: "AI&",
    targetUrl: "https://api.aiand.com/v1",
    statusUrl: "https://status.aiand.com/api/v2/summary.json",
    billingConsoleUrl: "https://console.aiand.com/settings/billing",
    billCurrency: "JPY",
    orgIdRequired: true,
    credits: "aiand",
    creditsUrl: "https://api.aiand.com/v1/analytics/metrics?range=24h",
    country: "JP",
    dataResidencyGroup: "Japan",
    note: "No balance-query API exists (confirmed against AI&'s own docs) — credits are purchased via a Stripe web checkout only. The Analytics API surfaces real period spend instead. Japan-based provider; this account bills in JPY.",
  },
  {
    id: "openrouter",
    label: "OpenRouter",
    targetUrl: "https://openrouter.ai/api/v1",
    statusUrl: "",
    billingConsoleUrl: "https://openrouter.ai/credits",
    billCurrency: "USD",
    credits: "openrouter",
    creditsUrl: "https://openrouter.ai/api/v1/key",
    country: "US",
    dataResidencyGroup: "United States",
    note: "No Statuspage feed found (status.openrouter.ai has no /api/v2/summary.json) — health falls back to a live probe of the target URL.",
  },
  {
    id: "openai",
    label: "OpenAI",
    targetUrl: "https://api.openai.com/v1",
    statusUrl: "https://status.openai.com/api/v2/summary.json",
    billingConsoleUrl: "https://platform.openai.com/usage",
    billCurrency: "USD",
    credits: "none",
    creditsUrl: "",
    country: "US",
    dataResidencyGroup: "United States",
    note: "No usable balance API: the legacy credit_grants endpoint is deprecated and the org-costs endpoint needs an Admin API key, not this stored inference key.",
  },
  {
    id: "anthropic",
    label: "Anthropic",
    targetUrl: "https://api.anthropic.com/v1",
    statusUrl: "https://status.anthropic.com/api/v2/summary.json",
    billingConsoleUrl: "https://console.anthropic.com/settings/billing",
    billCurrency: "USD",
    credits: "none",
    creditsUrl: "",
    country: "US",
    dataResidencyGroup: "United States",
    note: "No usable balance API: the cost/usage report lives behind the beta Admin API, not this stored inference key.",
  },
  {
    id: "google",
    label: "Google (Gemini)",
    targetUrl: "https://generativelanguage.googleapis.com/v1beta/openai",
    statusUrl: "",
    billingConsoleUrl: "https://aistudio.google.com/usage",
    billCurrency: "USD",
    credits: "none",
    creditsUrl: "",
    country: "US",
    dataResidencyGroup: "United States",
    note: "Google Cloud's status feed isn't a Statuspage summary.json — health falls back to a live probe. No per-key balance API.",
  },
  {
    id: "microsoft",
    label: "Microsoft (Azure OpenAI)",
    targetUrl: "https://<resource>.openai.azure.com/openai/v1",
    targetUrlIsTemplate: true,
    statusUrl: "",
    billingConsoleUrl: "https://portal.azure.com/#view/Microsoft_Azure_CostManagement",
    billCurrency: "USD",
    credits: "none",
    creditsUrl: "",
    country: "US",
    dataResidencyGroup: "Azure region (operator-selected)",
    note: "Azure OpenAI has no single base URL — replace <resource> with your own resource name before saving. No per-key balance API; cost tracking lives in Azure Cost Management.",
  },
  {
    id: "moonshot",
    label: "Moonshot AI (Kimi)",
    targetUrl: "https://api.moonshot.ai/v1",
    statusUrl: "https://status.moonshot.cn/api/v2/summary.json",
    billingConsoleUrl: "https://platform.moonshot.ai/console/pay",
    billCurrency: "USD",
    credits: "none",
    creditsUrl: "",
    country: "CN",
    dataResidencyGroup: "China",
    note: "No documented per-key balance API. The .ai endpoint bills in USD; swap to api.moonshot.cn (CNY billing) if routing through the China-region product instead.",
  },
  {
    id: "fireworks",
    label: "Fireworks AI",
    targetUrl: "https://api.fireworks.ai/inference/v1",
    statusUrl: "https://status.fireworks.ai/api/v2/summary.json",
    billingConsoleUrl: "https://fireworks.ai/account/billing",
    billCurrency: "USD",
    credits: "none",
    creditsUrl: "",
    country: "US",
    dataResidencyGroup: "United States",
    note: "No documented per-key balance API.",
  },
  {
    id: "mistral",
    label: "Mistral AI",
    targetUrl: "https://api.mistral.ai/v1",
    statusUrl: "https://status.mistral.ai/api/v2/summary.json",
    billingConsoleUrl: "https://console.mistral.ai/billing",
    billCurrency: "EUR",
    credits: "none",
    creditsUrl: "",
    country: "FR",
    dataResidencyGroup: "European Union",
    note: "No documented per-key balance API.",
  },
  {
    id: "qwen",
    label: "Qwen Cloud (Alibaba Cloud MaaS)",
    targetUrl: "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1",
    statusUrl: "",
    billingConsoleUrl: "https://maas.console.aliyun.com/",
    billCurrency: "USD",
    credits: "none",
    creditsUrl: "",
    country: "SG",
    dataResidencyGroup: "Southeast Asia",
    note: "Alibaba Cloud Model-as-a-Service (compatible-mode OpenAI API), Singapore region. Models: qwen3.8-max, qwen3.7-plus, qwen3.6-flash, glm-5.2. No documented per-key balance API.",
  },
];

export function presetFor(providerName: string): ProviderPreset | undefined {
  const id = providerName.toLowerCase();
  return PROVIDER_PRESETS.find((p) => p.id === id);
}

// providerIconSlug normalizes a provider's display name to the manifest slug
// its brand icon lives under. The raw `name.toLowerCase()` the whole app used
// before (2026-08-14) breaks the moment a provider is renamed off the exact
// slug — "AI&" → "ai&" (manifest has "aiand"), "Qwen Cloud" → "qwen cloud"
// (manifest has "qwen"), and the production "qwen-code"/"qwen/code" dupes all
// missed and letter-badged. This centralizes the alias table every surface
// (RemoteOfferings, Routing, ProviderKeys, Console, ModelCardView,
// CatalogPanel) shares, so a rename can't silently drop a vendor mark — and
// so credits.go's case-sensitive dispatch hazard (it switches on row.Name
// verbatim) has a FE-side counterpart while the backend is hardened too.
export function providerIconSlug(name: string): string {
  if (!name) return "";
  const s = name.trim().toLowerCase();
  const aliases: Record<string, string> = {
    "ai&": "aiand", "ai and": "aiand", "ai-and": "aiand", "ai_and": "aiand", aiand: "aiand",
    // Qwen Cloud is Alibaba Cloud's MaaS — operator feedback 2026-08-14
    // ("Qwen is missing an icon for Alibaba") wants the corporate mark on
    // its chips, not a second Qwen product star.
    "qwen cloud": "qwen", qwencloud: "qwen", "qwen-cloud": "qwen", qwen: "qwen",
    "alibaba cloud": "alibaba", alibaba: "alibaba",
    "google gemini": "google", google: "google",
    "azure openai": "microsoft", azure: "microsoft", microsoft: "microsoft",
    anthropic: "anthropic", claude: "anthropic",
    "moonshot ai": "moonshot", moonshot: "moonshot", kimi: "moonshot",
  };
  if (aliases[s]) return aliases[s];
  const stripped = s.replace(/[\s\-_/]+/g, "");
  if (aliases[stripped]) return aliases[stripped];
  return s;
}
