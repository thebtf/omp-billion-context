import argparse
import hashlib
import json
import platform
import shutil
import tarfile
import tempfile
import urllib.request
import zipfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
NODE_DISTRIBUTIONS = {
    "windows-amd64": (
        "win-x64", ".zip", "node.exe",
        "e612af99f626ab415ea225af6bce5c971feb448cb5cae250bb1c3fa50c5f7d9f",
    ),
    "linux-amd64": (
        "linux-x64", ".tar.gz", "bin/node",
        "061ec3e3062115705b56b51fa9cc330ab50d34b6969387d4e4b1a50c6809acc2",
    ),
    "darwin-arm64": (
        "darwin-arm64", ".tar.gz", "bin/node",
        "ff6e17d4874b76d272212b908cc020d6e815fba5a6c7eaaa06add725840307fd",
    ),
}


def stage_node(stage, version, distribution):
    platform_slug, suffix, executable, expected_digest = distribution
    release = f"node-v{version}-{platform_slug}"
    download = stage / f"{release}{suffix}"
    url = f"https://nodejs.org/dist/v{version}/{download.name}"
    digest = hashlib.sha256()
    with urllib.request.urlopen(url, timeout=60) as response, download.open("wb") as file:
        while chunk := response.read(1024 * 1024):
            digest.update(chunk)
            file.write(chunk)
    if digest.hexdigest() != expected_digest:
        raise RuntimeError(f"SHA-256 mismatch for {download.name}")

    if suffix == ".zip":
        archive = zipfile.ZipFile(download)
        open_member = archive.open
    else:
        archive = tarfile.open(download, "r:gz")
        open_member = archive.extractfile

    node = stage / executable.rsplit("/", 1)[-1]
    license_file = stage / "NODE-LICENSE"
    with archive:
        for member, destination in ((executable, node), ("LICENSE", license_file)):
            source = open_member(f"{release}/{member}")
            if source is None:
                raise RuntimeError(f"expected a file for {member} in {download.name}")
            with source, destination.open("wb") as file:
                shutil.copyfileobj(source, file)
    node.chmod(0o755)
    return node, license_file


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--target", choices=NODE_DISTRIBUTIONS)
    args = parser.parse_args()
    machine = platform.machine().lower()
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(machine, machine)
    host = f"{platform.system().lower()}-{arch}"
    if host not in NODE_DISTRIBUTIONS:
        parser.error(f"unsupported runtime build host: {host}")
    if args.target is not None and args.target != host:
        parser.error(f"target {args.target} must be built on a matching host, got {host}")

    manifest = json.loads((ROOT / "package.json").read_text(encoding="utf-8"))
    lock = json.loads((ROOT / "package-lock.json").read_text(encoding="utf-8"))
    for path, package in lock["packages"].items():
        if not path:
            continue
        installed = ROOT / path / "package.json"
        if (
            not installed.is_file()
            or json.loads(installed.read_text(encoding="utf-8"))["version"] != package["version"]
        ):
            parser.error(f"missing or mismatched {path}; run npm ci --omit=dev --ignore-scripts")

    kernel = ROOT / "node_modules/acp-kernel"
    worker = ROOT / "kernel"
    if set(lock["packages"]) != {"", "node_modules/acp-kernel"}:
        parser.error("the runtime must contain only the standalone acp-kernel package")
    if not (kernel / "dist/index.js").is_file() or not (kernel / "LICENSE").is_file():
        parser.error("acp-kernel is incomplete; run npm ci --omit=dev --ignore-scripts")

    bundle = ROOT / "bundle"
    with tempfile.TemporaryDirectory(prefix=".runtime-", dir=bundle) as directory:
        stage = Path(directory)
        node, license_file = stage_node(stage, manifest["engines"]["node"], NODE_DISTRIBUTIONS[host])
        runtime = stage / "runtime.zip"
        files = {node.name: node, "NODE-LICENSE": license_file}
        for name in ("package.json", "LICENSE"):
            files[f"node_modules/acp-kernel/{name}"] = kernel / name
        for path in sorted((kernel / "dist").rglob("*.js")):
            if path.is_symlink() or not path.is_file():
                raise RuntimeError(f"invalid kernel runtime entry: {path.name}")
            files[path.relative_to(ROOT).as_posix()] = path
        for path in sorted(worker.glob("*.mjs")):
            if path.is_symlink() or not path.is_file():
                raise RuntimeError(f"invalid worker module: {path.name}")
            files[path.relative_to(ROOT).as_posix()] = path
        required = [node.name, "NODE-LICENSE", "node_modules/acp-kernel/package.json", "node_modules/acp-kernel/dist/index.js", "kernel/worker.mjs", "kernel/host.mjs", "kernel/store.mjs"]
        with zipfile.ZipFile(runtime, "w", compression=zipfile.ZIP_DEFLATED) as archive:
            for name, path in sorted(files.items()):
                entry = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
                entry.create_system = 3
                entry.external_attr = (0o100755 if name == node.name else 0o100644) << 16
                entry.compress_type = zipfile.ZIP_DEFLATED
                with path.open("rb") as source, archive.open(entry, "w") as destination:
                    shutil.copyfileobj(source, destination)
            missing = set(required) - set(files)
            if missing:
                raise ValueError(f"missing kernel entry points: {sorted(missing)}")
            sizes = {name: archive.getinfo(name).file_size for name in files}
        digest = hashlib.sha256()
        with runtime.open("rb") as source:
            while chunk := source.read(1024 * 1024):
                digest.update(chunk)
        metadata = stage / "runtime.json"
        metadata.write_text(json.dumps({
            "schema": 2,
            "kernel_version": manifest["dependencies"]["acp-kernel"],
            "node_version": manifest["engines"]["node"],
            "platform": host,
            "archive_sha256": digest.hexdigest(),
            "required": sizes,
        }, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
        runtime.replace(bundle / runtime.name)
        metadata.replace(bundle / metadata.name)


if __name__ == "__main__":
    main()
