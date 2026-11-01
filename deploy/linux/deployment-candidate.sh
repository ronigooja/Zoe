#!/usr/bin/env bash
# Validate a release manifest and deployment inputs without changing a host.
set -euo pipefail

usage() {
  echo "usage: $0 MANIFEST.json [--compose COMPOSE_FILE --env-file ENV_FILE] [--output PLAN.json]" >&2
  exit 2
}
manifest=""
compose_file=""
env_file=""
output=""
while (($#)); do
  case "$1" in
    --compose) [[ $# -ge 2 ]] || usage; compose_file="$2"; shift 2 ;;
    --env-file) [[ $# -ge 2 ]] || usage; env_file="$2"; shift 2 ;;
    --output) [[ $# -ge 2 ]] || usage; output="$2"; shift 2 ;;
    -*) usage ;;
    *) [[ -z "$manifest" ]] || usage; manifest="$1"; shift ;;
  esac
done
[[ -n "$manifest" && -f "$manifest" ]] || usage
if [[ -n "$compose_file" && ! -f "$compose_file" ]]; then echo "compose file not found: $compose_file" >&2; exit 1; fi
if [[ -n "$env_file" && ! -f "$env_file" ]]; then echo "env file not found: $env_file" >&2; exit 1; fi

python3 - "$manifest" "$compose_file" "$env_file" "$output" <<'PY'
import hashlib, json, os, pathlib, re, subprocess, sys
manifest_path, compose, env_file, output = sys.argv[1:]
path = pathlib.Path(manifest_path).resolve()
try:
    doc = json.loads(path.read_text())
except Exception as exc:
    raise SystemExit(f"invalid manifest JSON: {exc}")
if not isinstance(doc, dict): raise SystemExit("manifest must be a JSON object")
source = doc.get("source_revision")
if not isinstance(source, str) or not re.fullmatch(r"[0-9a-f]{40}", source):
    raise SystemExit("source_revision must be a 40-character lowercase git revision")
arch = doc.get("architecture") or doc.get("target_architecture")
if arch not in ("amd64", "arm64"): raise SystemExit("architecture must be amd64 or arm64")
artifacts = doc.get("artifacts")
if not isinstance(artifacts, list) or not artifacts: raise SystemExit("manifest artifacts must be a non-empty list")
seen = set(); checked=[]
for item in artifacts:
    if not isinstance(item, dict): raise SystemExit("each artifact must be an object")
    name, rel, digest, sha = item.get("name"), item.get("path"), item.get("digest"), item.get("sha256")
    if not isinstance(name, str) or not name or name in seen: raise SystemExit(f"invalid or duplicate artifact name: {name!r}")
    seen.add(name)
    if not isinstance(rel, str) or not rel or os.path.isabs(rel) or ".." in pathlib.PurePosixPath(rel).parts:
        raise SystemExit(f"artifact {name} has unsafe path")
    if not isinstance(sha, str) or not re.fullmatch(r"[0-9a-f]{64}", sha): raise SystemExit(f"artifact {name} has invalid sha256")
    if digest is not None and (not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest)): raise SystemExit(f"artifact {name} has invalid digest")
    artifact_path = (path.parent / rel).resolve()
    try: artifact_path.relative_to(path.parent)
    except ValueError: raise SystemExit(f"artifact {name} escapes manifest directory")
    if not artifact_path.is_file(): raise SystemExit(f"artifact {name} missing: {rel}")
    h=hashlib.sha256();
    with artifact_path.open("rb") as f:
        for chunk in iter(lambda:f.read(1024*1024), b""): h.update(chunk)
    actual=h.hexdigest()
    if actual != sha: raise SystemExit(f"artifact {name} sha256 mismatch: expected {sha}, got {actual}")
    checked.append({"name":name,"path":rel,"sha256":actual, **({"digest":digest} if digest else {})})
images = doc.get("images", {})
if not isinstance(images, dict): raise SystemExit("manifest images must be an object")
checked_images = {}
for name, item in images.items():
    if not isinstance(name, str) or not re.fullmatch(r"[a-z0-9][a-z0-9._-]*", name):
        raise SystemExit(f"invalid image name: {name!r}")
    if isinstance(item, str):
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", item):
            raise SystemExit(f"image {name} has an invalid local image ID")
        checked_images[name] = {"local_id": item}
        continue
    if not isinstance(item, dict): raise SystemExit(f"image {name} must be an object")
    ref, digest = item.get("ref"), item.get("digest")
    if not isinstance(ref, str) or not re.fullmatch(r"ghcr\.io/[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*:[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", ref):
        raise SystemExit(f"image {name} has an invalid GHCR reference")
    if not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise SystemExit(f"image {name} has an invalid immutable digest")
    checked_images[name] = {"ref": ref, "digest": digest}
if compose:
    cmd=["docker","compose","-f",compose]
    if env_file: cmd += ["--env-file", env_file]
    cmd += ["config","--quiet"]
    try: subprocess.run(cmd, check=True, stdout=subprocess.DEVNULL)
    except FileNotFoundError: raise SystemExit("docker is required for --compose validation")
    except subprocess.CalledProcessError: raise SystemExit("docker compose config validation failed")
plan={"source_revision":source,"architecture":arch,"artifacts":checked,"images":checked_images,
      "installation_order":["verify manifest and artifact hashes","install OpenCode runtime and immutable Zoe release","switch current selector","start services and verify health/readiness"],
      "rollback":{"selector":"/opt/zoe/releases/current","target":"previous immutable release","images":"previous manifest image digests"},
      "checks":{"manifest_sha256":True,"compose_config":bool(compose),"host_mutation":False}}
text=json.dumps(plan,indent=2,sort_keys=True)+"\n"
if output: pathlib.Path(output).write_text(text); print(f"deployment candidate valid; plan written to {output}")
else: print(text,end="")
PY
