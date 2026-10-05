#!/usr/bin/env python3
"""mcp-schema-budget: measure and gate the token cost of an MCP server's tools/list.

Context-creation redesign, WS-H2 / contract C5: every MCP server a client attaches adds its
`tools/list` schema to *every* request, so each server has a budget (default 4,000 tokens).

Three ways to supply the tool list (stdlib only; `tokenizers` optional for real token counts):

  --url URL            streamable-http MCP endpoint: initialize -> notifications/initialized
                       -> tools/list (follows nextCursor). Only run this against servers you
                       own/trust; it never executes anything but HTTP calls. URLs and headers
                       are never printed (use --header-env NAME=ENVVAR for auth headers).
  --input FILE|-       a saved tools/list result: {"tools":[...]}, {"result":{"tools":[...]}},
                       or a bare list of tool objects.
  --dump               (with --url) print the raw tools array as JSON and exit - lets the fetch
                       half run next to the server (e.g. `docker exec -i <ctr> python3 - --url
                       http://127.0.0.1:8101/mcp --dump < scripts/mcp-schema-budget.py`) while
                       the tokenizer half runs where the tokenizer file is.

Token counting: pass --tokenizer path/to/tokenizer.json (Qwen3 tokenizer.json from
Qwen/Qwen3-0.6B) and the `tokenizers` package. Without them, falls back to an estimate of
chars/3.5 and says so (exit code and budget still apply, but treat as approximate).

What is counted per tool: the OpenAI-style function object a chat template renders into the
prompt, `{"type":"function","function":{"name","description","parameters"}}`, serialized with
json.dumps (Qwen3's template emits the same JSON per tool, one per line). Chat-template
wrapper text and LibreChat's `_mcp_<server>` name suffix add a few tokens per tool that are
not included; see docs/compression-redesign/mcp-token-budget.md.

Exit status: 0 within budget, 1 over budget, 2 usage/fetch error.
"""
import argparse
import json
import os
import sys
import urllib.error
import urllib.request

DEFAULT_BUDGET = 4000
PROTOCOL = "2025-06-18"
EST_CHARS_PER_TOKEN = 3.5


def _post(url, payload, headers, session):
    h = {
        "Content-Type": "application/json",
        "Accept": "application/json, text/event-stream",
        "MCP-Protocol-Version": PROTOCOL,
    }
    h.update(headers)
    if session:
        h["Mcp-Session-Id"] = session
    req = urllib.request.Request(url, data=json.dumps(payload).encode(), headers=h, method="POST")
    with urllib.request.urlopen(req, timeout=30) as resp:
        body = resp.read().decode("utf-8", "replace")
        sid = resp.headers.get("Mcp-Session-Id") or session
        ctype = resp.headers.get("Content-Type", "")
    return body, sid, ctype


def _parse(body, ctype, want_id):
    """Return the JSON-RPC message with id == want_id from a JSON or SSE body."""
    if not body.strip():
        return None
    if "text/event-stream" in ctype:
        for block in body.replace("\r\n", "\n").split("\n\n"):
            data = "\n".join(l[5:].lstrip() for l in block.split("\n") if l.startswith("data:"))
            if not data:
                continue
            try:
                msg = json.loads(data)
            except ValueError:
                continue
            if isinstance(msg, dict) and msg.get("id") == want_id:
                return msg
        return None
    msg = json.loads(body)
    if isinstance(msg, list):
        msg = next((m for m in msg if m.get("id") == want_id), None)
    return msg


def fetch_tools(url, headers):
    session = None
    body, session, ctype = _post(url, {
        "jsonrpc": "2.0", "id": 1, "method": "initialize",
        "params": {"protocolVersion": PROTOCOL, "capabilities": {},
                   "clientInfo": {"name": "mcp-schema-budget", "version": "1"}}}, headers, session)
    if _parse(body, ctype, 1) is None:
        raise RuntimeError("no initialize response")
    _post(url, {"jsonrpc": "2.0", "method": "notifications/initialized"}, headers, session)
    tools, cursor, rid = [], None, 2
    while True:
        params = {"cursor": cursor} if cursor else {}
        body, session, ctype = _post(url, {"jsonrpc": "2.0", "id": rid, "method": "tools/list",
                                           "params": params}, headers, session)
        msg = _parse(body, ctype, rid)
        if not msg or "result" not in msg:
            raise RuntimeError("tools/list failed: %s" % (json.dumps(msg.get("error")) if msg else "empty"))
        tools.extend(msg["result"].get("tools", []))
        cursor = msg["result"].get("nextCursor")
        rid += 1
        if not cursor:
            return tools


def load_input(path):
    raw = sys.stdin.read() if path == "-" else open(path, encoding="utf-8").read()
    data = json.loads(raw)
    if isinstance(data, dict):
        data = data.get("result", data).get("tools", data.get("tools"))
    if not isinstance(data, list):
        raise ValueError("input is not a tools/list result or a list of tools")
    return data


def as_function(tool):
    return {"type": "function", "function": {
        "name": tool.get("name", ""),
        "description": tool.get("description", "") or "",
        "parameters": tool.get("inputSchema", {"type": "object", "properties": {}}),
    }}


def make_counter(tok_path):
    if tok_path:
        try:
            from tokenizers import Tokenizer
        except ImportError:
            sys.exit("--tokenizer needs the `tokenizers` package (pip install tokenizers)")
        tk = Tokenizer.from_file(tok_path)
        return (lambda s: len(tk.encode(s).ids)), "tokenizer:" + os.path.basename(tok_path)
    return (lambda s: round(len(s) / EST_CHARS_PER_TOKEN)), "ESTIMATE chars/%.1f (no --tokenizer)" % EST_CHARS_PER_TOKEN


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--url", help="streamable-http MCP endpoint (never printed)")
    ap.add_argument("--input", help="saved tools/list JSON file, or - for stdin")
    ap.add_argument("--header-env", action="append", default=[], metavar="NAME=ENVVAR",
                    help="send header NAME with the value of $ENVVAR (value never printed)")
    ap.add_argument("--dump", action="store_true", help="with --url: print raw tools JSON and exit")
    ap.add_argument("--tokenizer", help="path to a Qwen3 tokenizer.json")
    ap.add_argument("--budget", type=int, default=DEFAULT_BUDGET, help="max tokens (default %d)" % DEFAULT_BUDGET)
    ap.add_argument("--label", default="", help="name to show in the report")
    ap.add_argument("--json", action="store_true", help="machine-readable output")
    a = ap.parse_args()

    if bool(a.url) == bool(a.input):
        ap.error("give exactly one of --url / --input")
    try:
        if a.url:
            hdrs = {}
            for spec in a.header_env:
                name, _, var = spec.partition("=")
                if var not in os.environ:
                    ap.error("env var %s not set" % var)
                hdrs[name] = os.environ[var]
            tools = fetch_tools(a.url, hdrs)
        else:
            tools = load_input(a.input)
    except (urllib.error.URLError, OSError, ValueError, RuntimeError) as e:
        # do not echo the URL: it may embed a token. type + reason only.
        print("error: %s: %s" % (type(e).__name__, getattr(e, "reason", e) if a.url else e), file=sys.stderr)
        return 2

    if a.dump:
        json.dump(tools, sys.stdout)
        return 0

    count, how = make_counter(a.tokenizer)
    rows = []
    for t in tools:
        s = json.dumps(as_function(t), ensure_ascii=False)
        rows.append((t.get("name", "?"), len(s), count(s)))
    total_chars = sum(r[1] for r in rows)
    total_tokens = sum(r[2] for r in rows)
    over = total_tokens > a.budget

    if a.json:
        print(json.dumps({"label": a.label, "counter": how, "tools": [
            {"name": n, "chars": c, "tokens": k} for n, c, k in rows],
            "total_chars": total_chars, "total_tokens": total_tokens,
            "budget": a.budget, "over_budget": over}))
    else:
        print("# %s  (%d tools, counter: %s)" % (a.label or "mcp server", len(rows), how))
        w = max([len(r[0]) for r in rows] + [4])
        print("%-*s %8s %8s" % (w, "tool", "chars", "tokens"))
        for n, c, k in sorted(rows, key=lambda r: -r[2]):
            print("%-*s %8d %8d" % (w, n, c, k))
        print("%-*s %8d %8d" % (w, "TOTAL", total_chars, total_tokens))
        print("budget %d tokens: %s" % (a.budget, "OVER by %d" % (total_tokens - a.budget) if over else "ok (%d spare)" % (a.budget - total_tokens)))
    return 1 if over else 0


if __name__ == "__main__":
    sys.exit(main())
