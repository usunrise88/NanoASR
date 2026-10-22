"""What every converter here shares: where the sources are, how a model is
converted and compiled, and how a package is written and archived."""
import gzip
import hashlib
import json
import os
import shutil
import sys
import tarfile
import uuid
from pathlib import Path

import coremltools as ct
import numpy as np
from coremltools.proto import Model_pb2

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
# fetch.sh puts sources and weights here; override with NANOASR_COREML_WORK.
WORK = Path(os.environ.get("NANOASR_COREML_WORK", ROOT / ".cache" / "coreml")).resolve()
SRC = WORK / "src"
WEIGHTS = WORK / "weights"
BUILD = WORK / "build"  # .mlpackage intermediates
PKG = WORK / "pkg"      # one directory per catalog entry, as it is archived
REVISION = "2026-10-22"  # the catalog revision of every CoreML build
# 2026-10-22 00:00Z: the time every archived file carries.
MTIME = 1792627200


def add_path(*paths):
    for p in paths:
        p = str(p)
        if p not in sys.path:
            sys.path.insert(0, p)


def convert(traced, inputs, outputs, precision="fp16"):
    """A traced module to an ML program for macOS 15. inputs are (name,
    example tensor) pairs, outputs names; every one is in the given precision,
    which is also the compute precision. The Go bridge converts to and from
    float32 either way."""
    dt = np.float16 if precision == "fp16" else np.float32
    return ct.convert(
        traced,
        inputs=[ct.TensorType(n, shape=tuple(t.shape), dtype=dt) for n, t in inputs],
        outputs=[ct.TensorType(n, dtype=dt) for n in outputs],
        compute_precision=ct.precision.FLOAT16 if precision == "fp16" else ct.precision.FLOAT32,
        minimum_deployment_target=ct.target.macOS15,
        convert_to="mlprogram",
        skip_model_load=True,
    )


def merge(functions, out, default):
    """Single-function packages, (function name, path) pairs, into one
    package whose functions share their weights."""
    desc = ct.utils.MultiFunctionDescriptor()
    for name, path in functions:
        desc.add_function(str(path), src_function_name="main", target_function_name=name)
    desc.default_function_name = default
    shutil.rmtree(out, ignore_errors=True)
    ct.utils.save_multifunction(desc, str(out))


def save(ml, path):
    """Save a converted model without the metadata coremltools stamps on it:
    the conversion date and a map whose order changes from run to run, which
    would make every build's bytes differ."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    shutil.rmtree(path, ignore_errors=True)
    ml.user_defined_metadata.clear()
    ml.save(str(path))
    print("saved", path.relative_to(WORK))
    return path


def stabilize(package):
    """Make a package's bytes a function of the model alone, which coremltools
    does not: it names the package's items by random UUIDs, and serialises
    the program's maps in whatever order they come. The compiled model
    carries a hash of both."""
    package = Path(package)
    path = package / "Manifest.json"
    m = json.loads(path.read_text())
    ids = {k: str(uuid.uuid5(uuid.NAMESPACE_URL, v["path"])).upper() for k, v in m["itemInfoEntries"].items()}
    spec = package / "Data" / m["itemInfoEntries"][m["rootModelIdentifier"]]["path"]
    m["itemInfoEntries"] = {ids[k]: v for k, v in m["itemInfoEntries"].items()}
    m["rootModelIdentifier"] = ids[m["rootModelIdentifier"]]
    path.write_text(json.dumps(m, indent=4, sort_keys=True) + "\n")
    model = Model_pb2.Model()
    model.ParseFromString(spec.read_bytes())
    spec.write_bytes(model.SerializeToString(deterministic=True))


def compile_into(package, dst):
    """Compile an .mlpackage into the .mlmodelc the server loads."""
    stabilize(package)
    compiled = ct.utils.compile_model(str(package))
    shutil.rmtree(dst, ignore_errors=True)
    shutil.move(compiled, dst)


def package_dir(model_id):
    """The empty package directory for a catalog entry."""
    d = PKG / model_id
    shutil.rmtree(d, ignore_errors=True)
    d.mkdir(parents=True)
    return d


def notice(pkg, text):
    """A NOTICE saying where the package came from: what the licences of the
    models redistributed here ask for."""
    (Path(pkg) / "NOTICE").write_text(text.strip() + "\n\nPackaged for NanoASR (https://github.com/usunrise88/NanoASR) by\n"
                                      f"scripts/coreml, revision {REVISION}.\n")


def pack(model_dir, out_dir):
    """model_dir as <name>-<REVISION>.tar.gz in out_dir: names sorted, no
    owners, one time and mode per kind, gzip without a name or time, so the
    same files give the same archive. Returns its path, sha256 and size."""
    model_dir, out_dir = Path(model_dir), Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    name = model_dir.name
    archive = out_dir / f"{name}-{REVISION}.tar.gz"
    with open(archive, "wb") as f, \
            gzip.GzipFile(filename="", mode="wb", fileobj=f, compresslevel=9, mtime=0) as gz, \
            tarfile.open(fileobj=gz, mode="w|", format=tarfile.PAX_FORMAT) as tar:
        for p in [model_dir] + sorted(model_dir.rglob("*")):
            info = tarfile.TarInfo(str(Path(name) / p.relative_to(model_dir)) if p != model_dir else name)
            info.mtime = MTIME
            if p.is_dir():
                info.type, info.mode = tarfile.DIRTYPE, 0o755
                tar.addfile(info)
            else:
                info.mode, info.size = 0o644, p.stat().st_size
                with open(p, "rb") as src:
                    tar.addfile(info, src)
    h = hashlib.sha256()
    with open(archive, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return archive, h.hexdigest(), archive.stat().st_size
