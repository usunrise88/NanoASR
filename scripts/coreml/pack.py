"""Archive packaged models for the release the catalog downloads them from,
and print, or write, the sha256 and size each catalog entry pins.

usage: pack.py OUT_DIR ID [ID ...] [--update-catalog]
  ID is a package directory under $NANOASR_COREML_WORK/pkg. --update-catalog
  writes the new sha256 and size_bytes into internal/registry/catalog.yaml.
"""
import argparse
import re

import common

CATALOG = common.ROOT / "internal" / "registry" / "catalog.yaml"


def update_catalog(model_id, sha, size):
    s = CATALOG.read_text()
    start = s.index(f"  - id: {model_id}\n")
    end = s.find("\n  - id: ", start + 1)
    end = len(s) if end < 0 else end
    entry = s[start:end]
    new, n = re.subn(r"(\n    source:\n      url: \S+\n)      sha256: \w+\n      size_bytes: \d+\n",
                     rf"\g<1>      sha256: {sha}\n      size_bytes: {size}\n", entry)
    if n != 1:
        raise SystemExit(f"{model_id}: no source sha256 and size_bytes to update in {CATALOG}")
    CATALOG.write_text(s[:start] + new + s[end:])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("out")
    ap.add_argument("ids", nargs="+")
    ap.add_argument("--update-catalog", action="store_true")
    args = ap.parse_args()
    for model_id in args.ids:
        archive, sha, size = common.pack(common.PKG / model_id, args.out)
        print(f"{archive.name}\n      sha256: {sha}\n      size_bytes: {size}")
        if args.update_catalog:
            update_catalog(model_id, sha, size)
    if args.update_catalog:
        print("updated", CATALOG.relative_to(common.ROOT))


if __name__ == "__main__":
    main()
