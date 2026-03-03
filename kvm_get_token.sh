cat >/bin/kvm_get_token.sh <<'SH'
#!/bin/sh
set -eu

BASE_URL="${BASE_URL:-http://127.0.0.1}"
CONF="${CONF:-/etc/nanokvm.snapshot.conf}"
TOKEN_FILE="${TOKEN_FILE:-/tmp/nanokvm.token}"
SKEW="${SKEW:-60}"  # refresh if token expires in < 60s

. "$CONF"

now="$(date +%s)"

# Reuse cached token if valid
if [ -f "$TOKEN_FILE" ]; then
  tok="$(cat "$TOKEN_FILE" 2>/dev/null || true)"
  if [ -n "$tok" ]; then
    payload="$(echo "$tok" | cut -d. -f2 | tr '_-' '/+' )"
    case $((${#payload} % 4)) in
      2) payload="${payload}==";;
      3) payload="${payload}=";;
    esac
    exp="$(printf '%s' "$payload" | base64 -d 2>/dev/null | sed -n 's/.*"exp":\([0-9]\+\).*/\1/p' | head -n 1 || true)"
    if [ -n "$exp" ] && [ "$((exp - now))" -gt "$SKEW" ]; then
      echo "$tok"
      exit 0
    fi
  fi
fi

# Refresh token
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

tok="$(echo "$LOGIN_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p' | head -n 1)"

[ -n "$tok" ] || { echo "ERROR: could not obtain token"; echo "$LOGIN_JSON"; exit 2; }

printf '%s' "$tok" >"$TOKEN_FILE"
chmod 600 "$TOKEN_FILE" 2>/dev/null || true
echo "$tok"
SH

chmod +x /bin/kvm_get_token.sh