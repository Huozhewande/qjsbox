#!/bin/sh
# Builds the sandboxed worker. Usage: worker/build.sh [output-path]
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
out=${1:-"$root/bin/qjsbox-worker"}
mkdir -p "$(dirname -- "$out")"
build=$(mktemp -d)
trap 'rm -rf "$build"' EXIT HUP INT TERM
python3 - "$root" "$build" <<'PY'
import hashlib,json,pathlib,sys
root,build=map(pathlib.Path,sys.argv[1:]);vendor=root/'worker/vendor'
manifest=json.loads((vendor/'manifest.json').read_text())
for name,digest in manifest['files'].items():
    if hashlib.sha256((vendor/name).read_bytes()).hexdigest()!=digest:raise SystemExit('Vendored source hash mismatch: '+name)
with (build/'embedded.h').open('w') as out:
    for name,path in [('acorn_js',vendor/'acorn/acorn.js'),('bootstrap_js',root/'worker/bootstrap.js')]:
        data=path.read_bytes()
        out.write('static const unsigned char '+name+'[] = {\n')
        for n in range(0,len(data),32):out.write(','.join(map(str,data[n:n+32]))+',\n')
        out.write('0};\nstatic const unsigned long '+name+'_len = '+str(len(data))+';\n')
PY
q="$root/worker/vendor/quickjs"
"${CC:-cc}" -O2 -fwrapv -Wno-deprecated-declarations -D_GNU_SOURCE '-DCONFIG_VERSION="2025-09-13"' -I"$q" -I"$build" \
  "$root/worker/worker.c" "$q/quickjs.c" "$q/dtoa.c" "$q/libregexp.c" "$q/libunicode.c" "$q/cutils.c" \
  -lm -o "$build/qjsbox-worker"
# Replace via rename: overwriting an executed binary in place makes macOS on
# Apple Silicon kill the new one for an invalid code signature.
cp "$build/qjsbox-worker" "$out.tmp.$$"
mv -f "$out.tmp.$$" "$out"
printf '%s\n' "$out"
