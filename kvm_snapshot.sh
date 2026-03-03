cat >/bin/kvm_snapshot.sh <<'SH'
#!/bin/sh
set -eu

BASE_URL="${BASE_URL:-http://127.0.0.1}"

CONF="${CONF:-/etc/nanokvm.snapshot.conf}"
[ -f "$CONF" ] || { echo "ERROR: missing config $CONF" >&2; exit 2; }

# shellcheck disable=SC1090
. "$CONF"

: "${KVM_USER:?Missing KVM_USER in $CONF}"
: "${KVM_PASS:?Missing KVM_PASS in $CONF}"

OUT="${1:-/tmp/snapshot.png}"

RANGE_BYTES="${RANGE_BYTES:-4194304}"
RANGE_END=$((RANGE_BYTES - 1))

ENC_PASS="$(printf "%s" "$KVM_PASS" \
  | openssl enc -aes-256-cbc -md md5 -salt \
      -pass pass:nanokvm-sipeed-2024 \
      -base64 -A 2>/dev/null \
  | sed 's/+/%2B/g; s/\//%2F/g; s/=/%3D/g')"

LOGIN_JSON="$(curl -sS \
  -X POST "$BASE_URL/api/auth/login" \
  -H "Content-Type: application/json" \ 
  -d "{\"username\":\"$KVM_USER\",\"password\":\"$ENC_PASS\"}" \
  --max-time 5)"

CODE="$(echo "$LOGIN_JSON" | sed -n 's/.*"code":\([-0-9]\+\).*/\1/p' | head -n 1)"
TOKEN="$(echo "$LOGIN_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p' | head -n 1)"

if [ "${CODE:-}" != "0" ] || [ -z "${TOKEN:-}" ]; then
  echo "ERROR: login failed"
  echo "$LOGIN_JSON"
  exit 3
fi

curl -sS -r "0-${RANGE_END}" \
  -H "Cookie: nano-kvm-token=$TOKEN" \
  "$BASE_URL/api/stream/mjpeg" \
| ffmpeg -hide_banner -loglevel fatal \
    -f mjpeg -i pipe:0 \
    -frames:v 1 \
    "$OUT" || true
[ -f "$OUT" ] || { echo "ERROR: snapshot not generated"; exit 4; }

echo "OK: $OUT"
SH

chmod +x /bin/kvm_snapshot.sh