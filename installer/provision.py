#!/usr/bin/env python3
"""provision.py — The Forge model asset provisioner.

Downloads GGUF/model assets listed in installer/assets.manifest.json from
Hugging Face, verifies them (sha256 when known, GGUF magic for gguf entries),
and installs them atomically under <root>/<dest_rel_path>/.

stdlib only. Idempotent: complete+verified files are skipped unless --force.

Usage:
    installer/provision.py                       # mandatory entries
    installer/provision.py --mandatory           # same, explicit
    installer/provision.py --all                 # mandatory + optional
    installer/provision.py --only gemma4-e4b-qat
    installer/provision.py --root /var/lib/forge --dry-run

Exit codes: 0 = all selected assets OK/skipped, 1 = one or more failures,
2 = usage/manifest error.
"""

import argparse
import hashlib
import json
import os
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

CHUNK = 1024 * 1024
RETRIES = 3
BACKOFF = 2  # seconds, doubled per retry

# Trusted directory for the manifest: the installer directory this script
# itself lives in. Asset paths are confined to the --root prefix at runtime.
HERE = os.path.dirname(os.path.abspath(__file__))


def load_manifest(path):
    resolved = Path(path).resolve()
    if not resolved.is_relative_to(Path(HERE).resolve()):
        raise ValueError(
            "manifest path escapes the installer directory: {}".format(path)
        )
    with open(resolved) as f:
        m = json.load(f)
    if "models" not in m:
        raise ValueError("manifest missing 'models' array")
    return m


def select_models(manifest, args):
    out = []
    for model in manifest["models"]:
        if model.get("type") == "service":
            if args.all or (args.only and model["id"] in args.only):
                out.append(model)
            continue
        if args.only:
            if model["id"] in args.only:
                out.append(model)
        elif args.all:
            out.append(model)
        elif args.mandatory or not (args.all or args.only):
            if model.get("role") == "mandatory":
                out.append(model)
    return out


def url_for(base_url, repo, filename):
    return "{}/{}/resolve/main/{}".format(
        base_url.rstrip("/"), repo, urllib.parse.quote(filename)
    )


def verify_existing(path, entry, root):
    """Return (ok, reason) for a file already on disk."""
    resolved = Path(path).resolve()
    if not resolved.is_relative_to(Path(root).resolve()):
        raise ValueError("asset path escapes the install root: {}".format(path))
    size = os.path.getsize(resolved)
    want_size = entry.get("size_bytes")
    if want_size is not None and size != want_size:
        return False, "size mismatch"
    if entry.get("gguf"):
        with open(resolved, "rb") as f:
            if f.read(4) != b"GGUF":
                return False, "bad GGUF magic"
    want_sha = entry.get("sha256")
    if want_sha:
        h = hashlib.sha256()
        with open(resolved, "rb") as f:
            while True:
                chunk = f.read(CHUNK)
                if not chunk:
                    break
                h.update(chunk)
        if h.hexdigest() != want_sha.lower():
            return False, "sha256 mismatch"
    elif want_size is None and not entry.get("gguf"):
        return False, "nothing to verify against"  # cannot confirm completeness
    return True, "verified"


def download(entry, dest_path, base_url, root):
    """Download entry -> dest_path atomically with resume + retry.
    Returns (status, bytes_written) or raises on final failure."""
    url = url_for(base_url, entry["_hf_repo"], entry["filename"])
    dest = Path(dest_path).resolve()
    if not dest.is_relative_to(Path(root).resolve()):
        raise ValueError(
            "asset path escapes the install root: {}".format(dest_path)
        )
    part = dest.parent / (dest.name + ".part")
    tmp_hash = hashlib.sha256()
    offset = 0
    resumed = False

    if os.path.exists(part) and not entry.get("_no_resume"):
        offset = os.path.getsize(part)
        if offset:
            resumed = True

    last_err = None
    for attempt in range(RETRIES):
        try:
            req = urllib.request.Request(url)
            if offset and attempt == 0:
                req.add_header("Range", "bytes={}-".format(offset))
                # Hash the preserved prefix so the final digest is correct.
                with open(part, "rb") as pf:
                    while True:
                        chunk = pf.read(CHUNK)
                        if not chunk:
                            break
                        tmp_hash.update(chunk)
                mode = "ab"
            else:
                if attempt > 0 or offset == 0:
                    offset = 0
                    tmp_hash = hashlib.sha256()
                    mode = "wb"
                else:
                    mode = "ab"

            with urllib.request.urlopen(req, timeout=60) as resp:
                if offset and attempt == 0 and resp.status != 206:
                    # Server ignored Range — start over.
                    offset = 0
                    tmp_hash = hashlib.sha256()
                    mode = "wb"
                written = offset
                mode = "wb" if written == 0 else mode
                with open(part, mode) as out:
                    while True:
                        chunk = resp.read(CHUNK)
                        if not chunk:
                            break
                        out.write(chunk)
                        tmp_hash.update(chunk)
                        written += len(chunk)

            want_sha = entry.get("sha256")
            if want_sha and tmp_hash.hexdigest() != want_sha.lower():
                raise IOError("sha256 mismatch after download")
            if entry.get("gguf"):
                with open(part, "rb") as f:
                    if f.read(4) != b"GGUF":
                        raise IOError("bad GGUF magic after download")
            want_size = entry.get("size_bytes")
            if want_size is not None and written != want_size:
                raise IOError(
                    "size mismatch: got {} want {}".format(written, want_size)
                )
            os.replace(part, dest)
            return ("RESUMED" if resumed else "DOWNLOADED"), written
        except Exception as err:  # noqa: BLE001 — report and retry anything
            last_err = err
            time.sleep(BACKOFF * (2 ** attempt))
    raise IOError(str(last_err))


def human(n):
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if n < 1024 or unit == "TB":
            return "{:.1f}{}".format(n, unit) if unit != "B" else "{} B".format(int(n))
        n /= 1024.0


def main(argv=None):
    ap = argparse.ArgumentParser(description="Forge asset provisioner")
    ap.add_argument("--manifest", default=os.path.join(HERE, "assets.manifest.json"))
    ap.add_argument("--root", default="/opt/forge",
                    help="base directory dest_rel_path is resolved against")
    sel = ap.add_mutually_exclusive_group()
    sel.add_argument("--only", action="append", metavar="ID")
    sel.add_argument("--mandatory", action="store_true")
    sel.add_argument("--all", action="store_true")
    ap.add_argument("--force", action="store_true",
                    help="re-download even if the file is present and verified")
    ap.add_argument("--base-url", default="https://huggingface.co",
                    help="override download host (for testing/mirrors)")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args(argv)

    try:
        manifest = load_manifest(args.manifest)
    except (OSError, ValueError, json.JSONDecodeError) as e:
        print("error: cannot load manifest {}: {}".format(args.manifest, e),
              file=sys.stderr)
        return 2

    if args.only:
        known = {m["id"] for m in manifest["models"]}
        unknown = set(args.only) - known
        if unknown:
            print("error: unknown model id(s): {}".format(", ".join(sorted(unknown))),
                  file=sys.stderr)
            return 2

    root = Path(args.root).resolve()
    rows = []
    failed = False
    for model in select_models(manifest, args):
        mid = model["id"]
        if model.get("type") == "service":
            rows.append((mid, "-", "SERVICE (external)", "-"))
            continue
        if not model.get("hf_repo"):
            rows.append((mid, "-", "PENDING (hf_repo TBD in manifest)", "-"))
            continue
        dest_dir = (root / model.get("dest_rel_path", "")).resolve()
        if not dest_dir.is_relative_to(root):
            print("error: dest_rel_path escapes install root: {}".format(
                model.get("dest_rel_path")), file=sys.stderr)
            return 2
        for entry in model.get("files", []):
            fname = entry.get("filename")
            if not fname:
                rows.append((mid, "-", "PENDING (filename TBD in manifest)", "-"))
                continue
            disp = fname if len(fname) <= 40 else fname[:37] + "..."
            if args.dry_run:
                rows.append((mid, disp, "DRY-RUN", str(entry.get("size_bytes") or "-")))
                continue
            dest_path = (dest_dir / fname).resolve()
            if not dest_path.is_relative_to(root):
                print("error: filename escapes install root: {}".format(fname),
                      file=sys.stderr)
                return 2
            if not args.force and os.path.exists(dest_path):
                ok, why = verify_existing(dest_path,
                                          {**entry, "gguf": model.get("gguf", False)},
                                          root)
                if ok:
                    rows.append((mid, disp, "SKIP ({})".format(why),
                                 human(os.path.getsize(dest_path))))
                    continue
            try:
                os.makedirs(dest_dir, exist_ok=True)
            except OSError as e:
                rows.append((mid, disp, "FAIL (mkdir: {})".format(e), "-"))
                failed = True
                continue
            dl_entry = dict(entry)
            dl_entry["_hf_repo"] = model["hf_repo"]
            dl_entry.setdefault("gguf", model.get("gguf", False))
            try:
                status, nbytes = download(dl_entry, dest_path, args.base_url, root)
                rows.append((mid, disp, status, human(nbytes)))
            except (OSError, urllib.error.URLError) as e:
                rows.append((mid, disp, "FAIL ({})".format(e), "-"))
                failed = True

    width_id = max([len(r[0]) for r in rows] + [10])
    width_f = max([len(r[1]) for r in rows] + [8])
    print("\n{:{w1}}  {:{w2}}  {:<20}  {}".format(
        "MODEL", "FILE", "STATUS", "BYTES", w1=width_id, w2=width_f))
    print("-" * (width_id + width_f + 30))
    for r in rows:
        print("{:{w1}}  {:{w2}}  {:<20}  {}".format(*r, w1=width_id, w2=width_f))
    print("\n{} item(s)".format(len(rows)))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
