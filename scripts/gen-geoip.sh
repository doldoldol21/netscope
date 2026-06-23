#!/usr/bin/env bash
# Regenerate the embedded GeoIP country database under internal/geoip/data/.
#
# Source: DB-IP IP-to-Country Lite (https://db-ip.com/db/download/ip-to-country-lite),
# licensed CC BY 4.0 — attribution is in README.md. Run monthly to refresh.
#
# Produces compact, sorted binaries (gzipped) the geoip package embeds:
#   v4.bin.gz       5-byte entries: uint32 range start + uint8 country index
#   v6.bin.gz       9-byte entries: uint64 /64 prefix + uint8 country index
#   countries.txt   concatenated 2-letter codes, indexed by the bytes above
set -euo pipefail
cd "$(dirname "$0")/.."
OUT="internal/geoip/data"
MONTH="$(date +%Y-%m)"
URL="https://download.db-ip.com/free/dbip-country-lite-${MONTH}.csv.gz"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

echo "==> downloading $URL"
curl -fsSL "$URL" -o "$TMP/dbip.csv.gz"
gzip -dc "$TMP/dbip.csv.gz" > "$TMP/dbip.csv"

echo "==> building compact database in $OUT"
mkdir -p "$OUT"
OUT="$OUT" python3 - "$TMP/dbip.csv" <<'PY'
import csv, struct, gzip, ipaddress, os, sys
out = os.environ["OUT"]; src = sys.argv[1]
codes = {}; order = []
def idx(c):
    if c not in codes:
        codes[c] = len(order); order.append(c)
    return codes[c]
v4 = []; v6 = {}
with open(src) as f:
    for row in csv.reader(f):
        if len(row) != 3:
            continue
        s, _, c = row
        try:
            ip = ipaddress.ip_address(s)
        except ValueError:
            continue
        if ip.version == 4:
            v4.append((int(ip), idx(c)))
        else:
            p = int.from_bytes(ip.packed[:8], "big")
            if p not in v6:
                v6[p] = idx(c)
v4.sort()
v6s = sorted(v6.items())
gzip.open(out + "/v4.bin.gz", "wb", 9).write(b"".join(struct.pack(">IB", s, i) for s, i in v4))
gzip.open(out + "/v6.bin.gz", "wb", 9).write(b"".join(struct.pack(">QB", p, i) for p, i in v6s))
open(out + "/countries.txt", "w").write("".join(order))
print(f"    v4={len(v4)} v6={len(v6s)} countries={len(order)}")
PY
echo "done."
