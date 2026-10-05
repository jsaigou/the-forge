#!/usr/bin/env python3
"""KV-cache regime probe for one loaded llama-server slot (WS-I, docs/compression-redesign/briefs/WS-I.md).

For the model currently loaded on --port it sends four chat requests directly to the slot (loopback, bypassing a0/compressor)
and reads llama.cpp's own per-request `timings` (cache_n = prompt tokens served from KV cache, prompt_n = tokens newly
prefilled) to classify how prefix reuse behaves:

  T1  system + user(doc A ~7K tokens + question)                     cold prefill; measures prefill tok/s
  T2  T1 + assistant reply + new user turn (append-only)             expect cache_n ~= len(T1) if prefix reuse works
  T3  T2 with one word edited ~10% into doc A (history rewrite)      pure-attention: reuse up to the edit; hybrid/recurrent
                                                                     without usable checkpoint: full re-prefill
  T4  T2 repeated verbatim                                           expect near-total reuse

Class per step = cache_n / (cache_n + prompt_n).  Requests are tiny on the decode side (max_tokens=8), temperature 0.
Only inference requests are sent; nothing is loaded/unloaded here (the caller does that). Stdlib only.

  python3 cache-regime-probe.py --port 8080 --corpus DIR --label qwen38-flash-next --out result.json
"""
import argparse, json, os, re, time, urllib.request


def post(port, messages, timeout=900):
    body = json.dumps({"messages": messages, "max_tokens": 8, "temperature": 0, "cache_prompt": True, "stream": False}).encode()
    req = urllib.request.Request(f"http://127.0.0.1:{port}/v1/chat/completions", data=body, headers={"content-type": "application/json"})
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=timeout) as r:
        d = json.loads(r.read())
    return d, time.perf_counter() - t0


def props(port):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/props", timeout=20) as r:
        return json.loads(r.read())


def step(d, wall):
    t = d.get("timings", {})
    c, n = t.get("cache_n"), t.get("prompt_n")
    tot = (c or 0) + (n or 0)
    return {"cache_n": c, "prompt_n": n, "total_prompt_tokens": tot, "reuse_frac": (c / tot) if (c is not None and tot) else None,
            "prefill_tps": t.get("prompt_per_second"), "wall_s": round(wall, 2), "usage_prompt_tokens": d.get("usage", {}).get("prompt_tokens")}


def classify(t2, t3):
    r2, r3 = t2["reuse_frac"], t3["reuse_frac"]
    if r2 is None or r3 is None:
        return "unknown (server did not report timings.cache_n)"
    if r2 < 0.5:
        return "full-reprefill (no cross-turn reuse even for append-only)"
    if r3 >= 0.80:
        return "shift-or-checkpoint-reuse (mid-history edit still mostly reused)"
    if r3 >= 0.30:
        return "prefix-reuse (append-only reused; mid-history edit re-prefills only from the edit onward)"
    if r3 >= 0.05:
        return "partial-reuse (some reuse before a mid-history edit, e.g. coarse checkpoints)"
    return "append-only-reuse; mid-history edit forces FULL re-prefill (SWA/hybrid/recurrent pattern)"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, required=True)
    ap.add_argument("--corpus", required=True)
    ap.add_argument("--label", required=True)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    docA = open(os.path.join(a.corpus, "pdf_text.txt"), encoding="utf-8").read() + "\n\n" + open(os.path.join(a.corpus, "page_scrape.md"), encoding="utf-8").read()
    start = len(docA) // 2  # edit ~50% into the history so "reuse up to the edit" (~0.5) is distinguishable from "full re-prefill" (~0)
    m = re.search(r" the ", docA[start:])
    cut = start + m.start()
    docA_edit = docA[:cut] + " thee " + docA[cut + 5:]
    nonce = os.urandom(6).hex()  # unique prefix per run: T1 must be a genuinely cold prefill, not a hit on a previous run's cached prompt
    sysm = {"role": "system", "content": f"[probe {nonce}] You are a careful assistant. Answer very briefly."}
    q1 = {"role": "user", "content": f"Document:\n{docA}\n\nQuestion: how many sections does the document appear to have? Answer with a number."}
    out = {"label": a.label, "ts": time.strftime("%Y-%m-%dT%H:%M:%S"), "edit_depth_chars": cut, "doc_chars": len(docA), "nonce": nonce, "edit_depth_frac_of_doc": round(cut / len(docA), 2)}
    p = props(a.port)
    out["n_ctx"] = p.get("default_generation_settings", {}).get("n_ctx")
    out["model"] = os.path.basename(str(p.get("model_path", ""))) or None
    d1, w1 = post(a.port, [sysm, q1]); out["T1"] = step(d1, w1)
    r1 = d1["choices"][0]["message"].get("content") or "ok"
    hist = [sysm, q1, {"role": "assistant", "content": r1}, {"role": "user", "content": "Now name one author or organisation mentioned. Answer briefly."}]
    d2, w2 = post(a.port, hist); out["T2"] = step(d2, w2)
    q1e = {"role": "user", "content": q1["content"].replace(docA, docA_edit)}
    hist_e = [sysm, q1e] + hist[2:]
    d3, w3 = post(a.port, hist_e); out["T3"] = step(d3, w3)
    d4, w4 = post(a.port, hist_e); out["T4"] = step(d4, w4)
    out["class"] = classify(out["T2"], out["T3"])
    json.dump(out, open(a.out, "w"), indent=1)
    print(json.dumps({k: out[k] for k in ("label", "n_ctx", "class")}), {k: (out[k]["reuse_frac"], out[k]["prefill_tps"], out[k]["wall_s"]) for k in ("T1", "T2", "T3", "T4")}, flush=True)


if __name__ == "__main__":
    main()
