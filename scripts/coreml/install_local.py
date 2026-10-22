"""Install a package built here into a NanoASR models directory, as the
downloader would install the archive: the files under <id>@<revision>/ and
model.yaml from the catalog entry. For trying a build before it is released.

usage: install_local.py MODELS_DIR ID [ID ...]
"""
import shutil
import sys
from pathlib import Path

import yaml

import common

CATALOG = common.ROOT / "internal" / "registry" / "catalog.yaml"


def main():
    root, ids = Path(sys.argv[1]), sys.argv[2:]
    entries = {m["id"]: m for m in yaml.safe_load(CATALOG.read_text())["models"]}
    for model_id in ids:
        m = entries[model_id]
        dst = root / f"{model_id}@{m['revision']}"
        shutil.rmtree(dst, ignore_errors=True)
        shutil.copytree(common.PKG / model_id, dst)
        with open(dst / "model.yaml", "w") as f:
            f.write("# Written by nanoasr when this model was downloaded.\n"
                    "# Edits are lost if the model is re-downloaded.\n")
            yaml.safe_dump(m, f, allow_unicode=True, sort_keys=False)
        missing = [v for v in m["files"].values() if not (dst / v).exists()]
        if missing:
            raise SystemExit(f"{model_id}: the package has no {', '.join(missing)}")
        print("installed", dst)


if __name__ == "__main__":
    main()
