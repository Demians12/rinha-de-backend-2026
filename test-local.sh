#!/bin/bash
# test-local.sh
# Testa SEM Docker — Go nativo ARM64 no M1
# Executar na RAIZ do repo: ./test-local.sh

set -e
ROOT="$(cd "$(dirname "$0")" && pwd)"
RESOURCES="${ROOT}/resources"
TMP_DIR="/tmp/rinha-test-$$"
mkdir -p "${TMP_DIR}"

cleanup() {
    [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
    rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

echo "═══════════════════════════════════════════════════"
echo "  Rinha 2026 — Teste Local (sem Docker, M1 nativo)"
echo "═══════════════════════════════════════════════════"

# ── 1. Go ────────────────────────────────────────────────────────────────────
echo ""
echo "▶ Verificando Go..."
if ! command -v go &> /dev/null; then
    echo "✗ Go não encontrado. Instala: brew install go"
    exit 1
fi
echo "  ✓ $(go version)"

# ── 2. Compilar (ARM64 nativo — muito mais rápido que emulado) ───────────────
echo ""
echo "▶ Compilando (src/)..."
cd "${ROOT}/src"
go mod tidy 2>/dev/null
go build -o "${TMP_DIR}/buildindex"  ./cmd/buildindex && echo "  ✓ buildindex"
go build -o "${TMP_DIR}/server"      ./cmd/server     && echo "  ✓ server"
go build -o "${TMP_DIR}/healthcheck" ./cmd/healthcheck && echo "  ✓ healthcheck"
cd "${ROOT}"

# ── 3. Buildindex com dataset de exemplo (100 refs, instantâneo) ─────────────
echo ""
echo "▶ Buildando índice (100 refs de exemplo)..."

# Cria gz do exemplo se não existir
EXAMPLE_GZ="${TMP_DIR}/example-refs.json.gz"
python3 -c "
import json, gzip
refs = json.load(open('${RESOURCES}/example-references.json'))
with gzip.open('${EXAMPLE_GZ}', 'wt') as f:
    json.dump(refs, f)
"

REFS_PATH="${EXAMPLE_GZ}" \
INDEX_PATH="${TMP_DIR}/index.bin" \
    "${TMP_DIR}/buildindex"

echo "  ✓ index.bin: $(du -sh ${TMP_DIR}/index.bin | cut -f1)"

# ── 4. Servidor ──────────────────────────────────────────────────────────────
echo ""
echo "▶ Subindo servidor..."
UDS="${TMP_DIR}/api.sock"

INDEX_PATH="${TMP_DIR}/index.bin" \
MCC_RISK_PATH="${RESOURCES}/mcc_risk.json" \
UDS_PATH="${UDS}" \
    "${TMP_DIR}/server" > "${TMP_DIR}/server.log" 2>&1 &
SERVER_PID=$!

for i in $(seq 1 30); do
    [ -S "${UDS}" ] && break
    sleep 0.3
    [ $i -eq 30 ] && { echo "✗ Timeout — vê logs: ${TMP_DIR}/server.log"; exit 1; }
done
echo "  ✓ Servidor pronto (PID ${SERVER_PID})"

# ── 5. Sanidade ──────────────────────────────────────────────────────────────
echo ""
echo "▶ GET /ready..."
STATUS=$(curl -sf --unix-socket "${UDS}" http://localhost/ready -o /dev/null -w "%{http_code}")
[ "${STATUS}" = "200" ] && echo "  ✓ 200 OK" || { echo "✗ ${STATUS}"; exit 1; }

echo ""
echo "▶ POST /fraud-score (corretude)..."

LEGIT=$(curl -sf --unix-socket "${UDS}" http://localhost/fraud-score \
    -X POST -H "Content-Type: application/json" -d '{
        "id":"tx-1",
        "transaction":{"amount":41.12,"installments":2,"requested_at":"2026-03-11T18:45:53Z"},
        "customer":{"avg_amount":82.24,"tx_count_24h":3,"known_merchants":["MERC-003","MERC-016"]},
        "merchant":{"id":"MERC-016","mcc":"5411","avg_amount":60.25},
        "terminal":{"is_online":false,"card_present":true,"km_from_home":29.23},
        "last_transaction":null}')

FRAUD=$(curl -sf --unix-socket "${UDS}" http://localhost/fraud-score \
    -X POST -H "Content-Type: application/json" -d '{
        "id":"tx-2",
        "transaction":{"amount":9505.97,"installments":10,"requested_at":"2026-03-14T05:15:12Z"},
        "customer":{"avg_amount":81.28,"tx_count_24h":20,"known_merchants":["MERC-008","MERC-007","MERC-005"]},
        "merchant":{"id":"MERC-068","mcc":"7802","avg_amount":54.86},
        "terminal":{"is_online":false,"card_present":true,"km_from_home":952.27},
        "last_transaction":null}')

echo "  legit → ${LEGIT}"
echo "  fraud → ${FRAUD}"

python3 -c "
import json, sys
l = json.loads('${LEGIT}')
f = json.loads('${FRAUD}')
ok = True
if not l.get('approved'):
    print('  ✗ legit deveria ser approved=true'); ok=False
else:
    print(f'  ✓ legit: approved=true  fraud_score={l[\"fraud_score\"]}')
if f.get('approved'):
    print('  ✗ fraud deveria ser approved=false'); ok=False
else:
    print(f'  ✓ fraud: approved=false fraud_score={f[\"fraud_score\"]}')
sys.exit(0 if ok else 1)
"

# ── 6. Benchmark latência (HTTP keep-alive sobre UDS) ────────────────────────
echo ""
echo "▶ Benchmark latência (1000 req, keep-alive, UDS)..."

python3 - <<PYEOF
import socket, time, json, os

SOCK = "${UDS}"
N    = 1000

payload = json.dumps({
    "id": "tx-bench",
    "transaction": {"amount":384.88,"installments":3,"requested_at":"2026-03-11T20:23:35Z"},
    "customer":    {"avg_amount":769.76,"tx_count_24h":3,"known_merchants":["MERC-009","MERC-001"]},
    "merchant":    {"id":"MERC-001","mcc":"5912","avg_amount":298.95},
    "terminal":    {"is_online":False,"card_present":True,"km_from_home":13.7},
    "last_transaction": {"timestamp":"2026-03-11T14:58:35Z","km_from_current":18.8}
}).encode()

req = (
    b"POST /fraud-score HTTP/1.1\r\n"
    b"Host: localhost\r\n"
    b"Content-Type: application/json\r\n"
    b"Connection: keep-alive\r\n"
    b"Content-Length: " + str(len(payload)).encode() + b"\r\n\r\n"
) + payload

def read_response(s):
    buf = b""
    while b"\r\n\r\n" not in buf:
        buf += s.recv(4096)
    header, body = buf.split(b"\r\n\r\n", 1)
    for line in header.split(b"\r\n"):
        if b"content-length" in line.lower():
            cl = int(line.split(b":")[1])
            while len(body) < cl:
                body += s.recv(4096)
            break
    return body

s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(SOCK)

# warmup
for _ in range(100):
    s.sendall(req); read_response(s)

lats = []
for _ in range(N):
    t0 = time.perf_counter()
    s.sendall(req)
    read_response(s)
    lats.append((time.perf_counter()-t0)*1000)
s.close()

lats.sort()
n = len(lats)
print(f"""
  ┌───────────────────────────────────────────┐
  │  HTTP over UDS — keep-alive — {N} reqs     │
  │  Índice: refs do exemplo em IVF int8      │
  ├───────────────────────────────────────────┤
  │  mean    {sum(lats)/n:>7.3f} ms                    │
  │  p50     {lats[n//2]:>7.3f} ms                    │
  │  p95     {lats[int(n*.95)]:>7.3f} ms                    │
  │  p99     {lats[int(n*.99)]:>7.3f} ms                    │
  └───────────────────────────────────────────┘

  ⚠  Este p99 é com o dataset de exemplo.
  Com 3M refs + índice rules/KNN o p99 real será diferente.
  O número definitivo vem do teste na Rinha (Mac Mini 2014).
""")
PYEOF

echo ""
echo "═══════════════════════════════════════════════════"
echo "  ✓ Teste local concluído."
echo "  Se tudo OK → ./build-and-push.sh"
echo "═══════════════════════════════════════════════════"
