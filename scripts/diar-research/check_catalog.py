"""Check built archives against what internal/registry/catalog.yaml pins.

    python check_catalog.py build/out

Every catalog entry whose source URL names a file in the directory must match
its sha256 and size_bytes. A mismatch means the pins in requirements-export.txt
drifted or the build is not reproducible on this machine; either way the
archive is not the one the catalog trusts, and must not be published under it.
"""

import hashlib
import os
import re
import sys

CATALOG = os.path.join(os.path.dirname(__file__), "..", "..", "internal", "registry", "catalog.yaml")


def entries():
    text = open(CATALOG).read()
    for block in re.split(r"\n  - id: ", text)[1:]:
        url = re.search(r"\n      url: (\S+)", block)
        sha = re.search(r"\n      sha256: (\S+)", block)
        size = re.search(r"\n      size_bytes: (\d+)", block)
        if url and sha and size:
            yield block.split("\n", 1)[0], url.group(1), sha.group(1), int(size.group(1))


def main():
    out = sys.argv[1]
    built = set(os.listdir(out))
    checked, bad = 0, 0
    for model, url, sha, size in entries():
        name = url.rsplit("/", 1)[1]
        if name not in built:
            continue
        path = os.path.join(out, name)
        got = hashlib.sha256(open(path, "rb").read()).hexdigest()
        got_size = os.path.getsize(path)
        ok = got == sha and got_size == size
        checked += 1
        bad += not ok
        print(f"{'ok' if ok else 'MISMATCH':8} {model}: {name}")
        if not ok:
            print(f"         catalog sha256 {sha} size {size}")
            print(f"         built   sha256 {got} size {got_size}")
    if checked == 0:
        sys.exit(f"no archive in {out} is named by the catalog")
    if bad:
        sys.exit(f"{bad} of {checked} archives differ from the catalog")


if __name__ == "__main__":
    main()
