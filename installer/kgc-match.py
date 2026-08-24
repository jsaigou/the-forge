#!/usr/bin/env python3
"""kgc-match.py — match the local host against the KGC hardware profile registry.

Reads installer/kgc/kgc.json, detects local CPU (from /proc/cpuinfo or lscpu)
and GPU (KFD topology gfx id, falling back to lspci vendor scan), and prints
the matching profile id on stdout.

Exit codes:
    0  match found — profile id printed on stdout
    1  no matching profile
    2  environment/error — not Linux, unreadable inputs, malformed registry

Used by install.sh (--target=public): on a match the profile id is written to
/etc/sysconfig/forge-kgc and runtime auto-tuning is skipped in favour of the
profile's verified baselines.
"""

import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
REGISTRY = os.path.join(HERE, "kgc.json")


def detect_cpu():
    model = ""
    try:
        with open("/proc/cpuinfo") as f:
            for line in f:
                if line.startswith("model name"):
                    model = line.split(":", 1)[1].strip()
                    break
    except OSError:
        pass
    if not model:
        try:
            import subprocess
            out = subprocess.run(["lscpu"], capture_output=True, text=True,
                                 timeout=5)
            for line in out.stdout.splitlines():
                if line.startswith("Model name:"):
                    model = line.split(":", 1)[1].strip()
                    break
        except Exception:  # noqa: BLE001 — detection is best-effort
            pass
    return model


def detect_gpu():
    """Return a GPU identifier string, e.g. 'gfx1151', or ''."""
    # Preferred: KFD topology exposes the gfx target directly.
    nodes = "/sys/class/kfd/kfd/topology/nodes"
    try:
        for name in sorted(os.listdir(nodes)):
            p = os.path.join(nodes, name, "name")
            with open(p) as f:
                gfx = f.read().strip()
            if gfx.startswith("gfx"):
                return gfx
    except OSError:
        pass
    # Fallback: AMD vendor present on PCI bus (no precise gfx id available).
    try:
        import subprocess
        out = subprocess.run(["lspci", "-n", "-d", "1002:"], capture_output=True,
                             text=True, timeout=5)
        if out.returncode == 0 and out.stdout.strip():
            dev_ids = sorted({ln.split()[2].rstrip(":") + ":" + ln.split()[3]
                              for ln in out.stdout.splitlines() if len(ln.split()) >= 4})
            return "amdgpu-pci:" + ",".join(dev_ids)
    except Exception:  # noqa: BLE001
        pass
    return ""


def main():
    if sys.platform != "linux":
        print("error: kgc-match requires Linux (got {})".format(sys.platform),
              file=sys.stderr)
        return 2
    try:
        with open(REGISTRY) as f:
            registry = json.load(f)
    except (OSError, ValueError, json.JSONDecodeError) as e:
        print("error: cannot load registry {}: {}".format(REGISTRY, e),
              file=sys.stderr)
        return 2

    cpu = detect_cpu()
    gpu = detect_gpu()
    debug = os.environ.get("KGC_DEBUG")
    if debug:
        print("cpu={!r} gpu={!r}".format(cpu, gpu), file=sys.stderr)

    if not cpu and not gpu:
        print("error: could not detect CPU or GPU on this host", file=sys.stderr)
        return 2

    for prof in registry.get("profiles", []):
        m = prof.get("match", {})
        cre = m.get("cpu_regex")
        gre = m.get("gpu_regex")
        cpu_ok = bool(cpu and cre and re.search(cre, cpu))
        gpu_ok = bool(gpu and gre and re.search(gre, gpu))
        # Both signals must agree when both are detectable; either may claim
        # the match alone if the other signal is unavailable.
        if cre and gre:
            matched = (
                (cpu_ok and gpu_ok)
                or (cpu_ok and not gpu)
                or (gpu_ok and not cpu)
            )
        else:
            matched = cpu_ok or gpu_ok
        if matched:
            print(prof["id"])
            return 0
    return 1


if __name__ == "__main__":
    sys.exit(main())
