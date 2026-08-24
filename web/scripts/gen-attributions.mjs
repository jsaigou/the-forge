#!/usr/bin/env node
// Sprint F — software attribution generator.
//
// Emits web/src/lib/attributions.generated.ts: the Go module deps (from
// go/go.mod), the npm deps (from web/package.json + their own installed
// package.json), and a curated table for the runtime stack (llama.cpp,
// Headroom, ComfyUI, vLLM — external processes this app launches/proxies,
// not code dependencies, so nothing in the repo declares them).
//
// Run:    npm run attributions  (also runs automatically before `npm run build`)
// Output: committed web/src/lib/attributions.generated.ts — a diff appears
//         only when a dependency, its license, or its version actually
//         changes.
//
// Go's license text isn't in go.mod, so it's sniffed from the module cache
// (`go env GOMODCACHE`) against a small SPDX matcher below. If the Go
// toolchain or a given module isn't available (e.g. a frontend-only CI
// runner with no `go` on PATH), that dependency's record is carried over
// unchanged from whatever this file already contains, with a warning —
// never fabricated, never silently blanked.

import { execFileSync } from "node:child_process";
import { existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));
const WEB_ROOT = resolve(__dirname, "..");
const REPO_ROOT = resolve(WEB_ROOT, "..");
const OUT_PATH = resolve(WEB_ROOT, "src/lib/attributions.generated.ts");

// ── Curated: the external runtime stack ─────────────────────────────────────
// Nothing in this repo declares these — they're separately-installed
// services this app launches (llama.cpp, ComfyUI, vLLM) or proxies to
// (headroom-ai, the Qwen3 TTS/embedding/STT always-on services). Kept here,
// in the one generated artifact, rather than hand-authored on the page
// itself.
const RUNTIME_STACK = [
  {
    name: "llama.cpp",
    role: "Local inference engine — every A1–A4 load bay runs llama-server",
    license: "MIT",
    projectUrl: "https://github.com/ggml-org/llama.cpp",
  },
  // headroom-ai (the third-party context-compression proxy this used to
  // front local slots + remote providers with) was fully replaced by our
  // own forge-compress binary (Sprint 3, docs/v5-headroom-replacement.md)
  // and its last install artifacts deleted from ForgeHost (Sprint 6) — removed
  // here since nothing in the live system proxies to it any more. The
  // onnxruntime/HF-tokenizer bindings forge-compress itself uses aren't
  // yet listed — a follow-up, not invented here without checking their
  // real license/project URLs first.
  {
    name: "Qwen3-TTS",
    role: "Speech synthesis (forge-tts.service, port 8082) — Qwen3-TTS-12Hz-1.7B CustomVoice/VoiceDesign/Base",
    license: "Apache-2.0",
    projectUrl: "https://huggingface.co/Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice",
  },
  {
    name: "Qwen3-Embedding",
    role: "Always-on CPU embedding service (forge-embedding.service, port 8083) — Qwen3-Embedding-0.6B",
    license: "Apache-2.0",
    projectUrl: "https://huggingface.co/Qwen/Qwen3-Embedding-0.6B",
  },
  {
    name: "parakeet.cpp (Nemotron ASR)",
    role: "Always-on CPU speech-to-text (forge-stt.service, port 8084) — parakeet.cpp server running nemotron-3.5-asr-streaming-0.6b",
    license: "MIT (server); OpenMDW-1.1 (model)",
    projectUrl: "https://github.com/mudler/parakeet.cpp",
  },
  {
    name: "ComfyUI",
    role: "Image/video generation backend, managed as a reservation-aware service",
    license: "GPL-3.0",
    projectUrl: "https://github.com/comfyanonymous/ComfyUI",
  },
  {
    name: "vLLM",
    role: "Inference engine backing the vLLM-style configs (Carbon 8B, Hy-MT2)",
    license: "Apache-2.0",
    projectUrl: "https://github.com/vllm-project/vllm",
  },
];

// ── npm deps ─────────────────────────────────────────────────────────────

function firstString(...vals) {
  for (const v of vals) if (typeof v === "string" && v) return v;
  return "";
}

function npmDeps() {
  const pkg = JSON.parse(readFileSync(resolve(WEB_ROOT, "package.json"), "utf8"));
  const names = [
    ...Object.keys(pkg.dependencies ?? {}),
    ...Object.keys(pkg.devDependencies ?? {}),
  ];
  const direct = new Set(Object.keys(pkg.dependencies ?? {}));

  return names
    .map((name) => {
      const pkgJsonPath = resolve(WEB_ROOT, "node_modules", name, "package.json");
      if (!existsSync(pkgJsonPath)) {
        console.error(`[gen-attributions] npm ${name}: not installed (run npm install first) — skipped`);
        return null;
      }
      const meta = JSON.parse(readFileSync(pkgJsonPath, "utf8"));
      const repoUrl = typeof meta.repository === "string" ? meta.repository : meta.repository?.url ?? "";
      return {
        name,
        version: meta.version ?? "",
        license: firstString(meta.license, meta.licenses?.[0]?.type, "unknown"),
        projectUrl: firstString(meta.homepage, repoUrl.replace(/^git\+/, "").replace(/\.git$/, "")),
        direct: direct.has(name),
      };
    })
    .filter((d) => d !== null)
    .sort((a, b) => a.name.localeCompare(b.name));
}

// ── Go deps ──────────────────────────────────────────────────────────────

function parseGoMod(text) {
  // require ( ... ) blocks, each line "path version [// indirect]"; also
  // handles a bare single-line "require path version" (unused today, kept
  // for robustness).
  const mods = [];
  const blockRe = /require\s*\(([\s\S]*?)\)/g;
  let m;
  while ((m = blockRe.exec(text)) !== null) {
    for (const line of m[1].split("\n")) {
      const t = line.trim();
      if (!t) continue;
      const lm = t.match(/^(\S+)\s+(v\S+)(\s*\/\/\s*indirect)?/);
      if (lm) mods.push({ path: lm[1], version: lm[2], indirect: !!lm[3] });
    }
  }
  const singleRe = /^require\s+(\S+)\s+(v\S+)/gm;
  while ((m = singleRe.exec(text)) !== null) {
    mods.push({ path: m[1], version: m[2], indirect: false });
  }
  return mods;
}

// Go module cache directories escape uppercase letters as "!" + lowercase
// (golang.org/ref/mod#module-cache).
function escapeModulePath(p) {
  return p.replace(/[A-Z]/g, (c) => `!${c.toLowerCase()}`);
}

const LICENSE_FILENAMES = ["LICENSE", "LICENSE.md", "LICENSE.txt", "COPYING"];

// Small SPDX sniffer — sufficient for this dependency set (checked live
// against all 23 real modules while writing this script). Order matters:
// check the more specific/identifying phrases first.
function sniffLicense(text) {
  if (/Apache License[\s\S]{0,40}Version 2\.0/i.test(text)) return "Apache-2.0";
  if (/Mozilla Public License[\s\S]{0,20}2\.0/i.test(text)) return "MPL-2.0";
  if (/Redistribution and use in source and binary forms/i.test(text)) {
    return /(may not be used|used to endorse or promote)/i.test(text) ? "BSD-3-Clause" : "BSD-2-Clause";
  }
  if (/Permission to use, copy, modify, and\/or distribute this software/i.test(text)) return "ISC";
  if (/MIT License|Permission is hereby granted, free of charge/i.test(text)) return "MIT";
  return "";
}

function goDeps(previous) {
  const goModPath = resolve(REPO_ROOT, "go/go.mod");
  const mods = parseGoMod(readFileSync(goModPath, "utf8"));

  let modCache = "";
  try {
    modCache = execFileSync("go", ["env", "GOMODCACHE"], { encoding: "utf8" }).trim();
  } catch (e) {
    console.error(`[gen-attributions] \`go env GOMODCACHE\` failed (${e.message}) — Go toolchain unavailable.`);
    console.error(`[gen-attributions]   Every Go dependency's license will be carried over from the existing file.`);
  }

  const prevByPath = new Map((previous?.goDeps ?? []).map((d) => [d.path, d]));

  return mods
    .map(({ path, version, indirect }) => {
      const prev = prevByPath.get(path);
      let license = "";
      if (modCache) {
        const dir = resolve(modCache, `${escapeModulePath(path)}@${version}`);
        if (existsSync(dir)) {
          const licenseFile = readdirSync(dir).find((f) => LICENSE_FILENAMES.includes(f));
          if (licenseFile) {
            license = sniffLicense(readFileSync(resolve(dir, licenseFile), "utf8"));
          }
        }
      }
      if (!license) {
        if (prev && prev.version === version && prev.license) {
          console.error(`[gen-attributions] go ${path}: license not resolved this run — reusing previous value "${prev.license}"`);
          license = prev.license;
        } else {
          console.error(`[gen-attributions] go ${path}: license could not be determined and no prior value exists — marking unknown`);
          license = "unknown — needs manual verification";
        }
      }
      return { path, version, indirect, license, projectUrl: `https://${path.replace(/\/v\d+$/, "")}` };
    })
    .sort((a, b) => (a.indirect === b.indirect ? a.path.localeCompare(b.path) : a.indirect ? 1 : -1));
}

// ── Main ─────────────────────────────────────────────────────────────────

function loadPrevious() {
  if (!existsSync(OUT_PATH)) return null;
  const text = readFileSync(OUT_PATH, "utf8");
  try {
    const goMatch = text.match(/export const GO_DEPS[^=]*=\s*(\[[\s\S]*?\n\];)/);
    const goDeps = goMatch ? JSON.parse(goMatch[1].replace(/;$/, "")) : [];
    return { goDeps };
  } catch {
    return null;
  }
}

function main() {
  const previous = loadPrevious();
  const npm = npmDeps();
  const go = goDeps(previous);

  const out = `// Sprint F — software attribution manifest (GENERATED by web/scripts/gen-attributions.mjs; do not edit by hand).
//
// Regenerated automatically before every build ("npm run build" runs this
// first). npm licenses come straight from each installed package's own
// package.json. Go licenses are sniffed from the module cache's vendored
// LICENSE file against a small SPDX matcher — see gen-attributions.mjs for
// the toolchain-unavailable fallback (reuses the prior value, never
// fabricates or blanks one).

export interface NpmDep {
  name: string;
  version: string;
  license: string;
  projectUrl: string;
  direct: boolean;
}

export interface GoDep {
  path: string;
  version: string;
  indirect: boolean;
  license: string;
  projectUrl: string;
}

export interface RuntimeComponent {
  name: string;
  role: string;
  license: string;
  projectUrl: string;
}

export const NPM_DEPS: NpmDep[] = ${JSON.stringify(npm, null, 2)};

export const GO_DEPS: GoDep[] = ${JSON.stringify(go, null, 2)};

// Curated — see gen-attributions.mjs's RUNTIME_STACK. Nothing in this repo
// declares these; they're external processes this app launches or proxies.
export const RUNTIME_STACK: RuntimeComponent[] = ${JSON.stringify(RUNTIME_STACK, null, 2)};
`;

  writeFileSync(OUT_PATH, out, "utf8");
  console.log(`[gen-attributions] wrote ${npm.length} npm deps, ${go.length} Go deps, ${RUNTIME_STACK.length} runtime components → ${OUT_PATH}`);
}

main();
