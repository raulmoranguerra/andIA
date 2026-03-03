cat >/bin/kvm_snapshot_fast.sh <<'SH'
#!/bin/sh
set -eu

BASE_URL="${BASE_URL:-http://127.0.0.1}"
OUT="${1:-/tmp/snapshot.jpg}"

TOKEN="$(/bin/kvm_get_token.sh)"

python3 - <<PY
import re, requests
from pathlib import Path

url = "${BASE_URL}/api/stream/mjpeg"
token = "${TOKEN}"
out = Path("${OUT}")

headers = {"Cookie": f"nano-kvm-token={token}"}

# stream=True so we can stop after first frame
r = requests.get(url, headers=headers, stream=True, timeout=3)
r.raise_for_status()

buf = b""
cl = None

# Read until we have Content-Length and end of headers
for chunk in r.iter_content(4096):
    if not chunk:
        continue
    buf += chunk
    m = re.search(br"Content-Length:\s*(\d+)\r?\n", buf)
    if m and (b"\r\n\r\n" in buf or b"\n\n" in buf):
        cl = int(m.group(1))
        break

if cl is None:
    raise RuntimeError("Could not find Content-Length for first frame")

sep = buf.find(b"\r\n\r\n")
sep_len = 4
if sep == -1:
    sep = buf.find(b"\n\n")
    sep_len = 2
if sep == -1:
    raise RuntimeError("Could not find end of headers")

start = sep + sep_len
data = buf[start:]

while len(data) < cl:
    data += next(r.iter_content(4096))

jpeg = data[:cl]
out.write_bytes(jpeg)
print(f"OK: {out}")
PY
SH

chmod +x /bin/kvm_snapshot_fast.sh