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

    modules = ROOT / "node_modules"
    if not (modules / "billion-context/dist/index.js").is_file():
        parser.error("billion-context is missing dist/index.js")

    bundle = ROOT / "bundle"
    with tempfile.TemporaryDirectory(prefix=".runtime-", dir=bundle) as directory:
        stage = Path(directory)
        node, license_file = stage_node(stage, manifest["engines"]["node"], NODE_DISTRIBUTIONS[host])
        runtime = stage / "runtime.zip"
        with zipfile.ZipFile(runtime, "w", compression=zipfile.ZIP_DEFLATED) as archive:
            archive.write(node, node.name)
            archive.write(license_file, license_file.name)
            # parse-bmfont-ascii 1.0.6 omits LICENSE.md from its npm tarball.
            license_path = Path("licenses/parse-bmfont-ascii-1.0.6.txt")
            archive.write(ROOT / "scripts" / license_path, license_path.as_posix())
            for path in sorted(modules.rglob("*")):
                relative = path.relative_to(modules)
                if ".bin" in relative.parts or relative.as_posix() == ".package-lock.json":
                    continue
                if path.is_symlink():
                    raise RuntimeError(f"unexpected symlink in node_modules: {relative}")
                if path.is_file():
                    archive.write(path, path.relative_to(ROOT).as_posix())
        runtime.replace(bundle / runtime.name)


if __name__ == "__main__":
    main()
