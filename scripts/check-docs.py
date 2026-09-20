#!/usr/bin/env python3
"""Every ```yaml block in README.md must load through `epmon validate`.

Same check CI runs (see .github/workflows/ci.yml). Required env:
EPMON_API_KEY, TOKEN, DEV_TOKEN (any dummy values).
"""
import os
import re
import subprocess
import sys
import tempfile


def main() -> int:
    with open("README.md") as f:
        blocks = re.findall(r"```yaml\n(.*?)```", f.read(), re.S)
    if not blocks:
        print("no yaml blocks found in README")
        return 1
    for i, b in enumerate(blocks):
        with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as f:
            f.write(b)
            path = f.name
        r = subprocess.run(
            ["go", "run", "./cmd/epmon", "validate", "--config", path],
            capture_output=True,
            text=True,
        )
        print(f"README yaml block {i}: exit={r.returncode} {r.stderr.strip()}")
        os.unlink(path)
        if r.returncode != 0:
            print(f"README yaml block {i} failed to validate")
            return 1
    for example in ("config.example.yaml", "config.example.json"):
        r = subprocess.run(
            ["go", "run", "./cmd/epmon", "validate", "--config", example],
            capture_output=True,
            text=True,
        )
        print(f"{example}: exit={r.returncode} {r.stderr.strip()}")
        if r.returncode != 0:
            return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
