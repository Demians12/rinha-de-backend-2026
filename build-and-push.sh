#!/bin/bash
# build-and-push.sh
# Executar na RAIZ do repo: ./build-and-push.sh

set -e

IMAGE="demians/rinha-2026"
TAG="latest"
FULL="${IMAGE}:${TAG}"
PLATFORM="linux/amd64"

echo "═══════════════════════════════════════════════════"
echo "  Rinha 2026 — Build & Push"
echo "  Image:    ${FULL}"
echo "  Platform: ${PLATFORM}"
echo "═══════════════════════════════════════════════════"

# ── 1. Pré-requisitos ────────────────────────────────────────────────────────
echo ""
echo "▶ Verificando pré-requisitos..."

if ! docker info > /dev/null 2>&1; then
    echo "✗ Docker não está rodando. Abre o Docker Desktop."
    exit 1
fi
echo "  ✓ Docker rodando"

if [ ! -f "resources/references.json.gz" ]; then
    echo "✗ resources/references.json.gz não encontrado."
    exit 1
fi
echo "  ✓ references.json.gz ($(du -sh resources/references.json.gz | cut -f1))"

if [ ! -f "Dockerfile" ]; then
    echo "✗ Dockerfile não encontrado na raiz."
    exit 1
fi
echo "  ✓ Dockerfile presente"

if [ ! -f "src/go.mod" ]; then
    echo "✗ src/go.mod não encontrado."
    exit 1
fi
echo "  ✓ src/go.mod presente"

# ── 2. Buildx ────────────────────────────────────────────────────────────────
echo ""
echo "▶ Configurando buildx..."

if ! docker buildx ls | grep -q "rinha-builder"; then
    docker buildx create --name rinha-builder --driver docker-container --bootstrap
    echo "  ✓ Builder 'rinha-builder' criado"
else
    echo "  ✓ Builder 'rinha-builder' já existe"
fi
docker buildx use rinha-builder

# ── 3. Build e push ──────────────────────────────────────────────────────────
echo ""
echo "▶ Building ${FULL} para ${PLATFORM}..."
echo "  (Stage de buildindex processa 3M vetores — ~15-20min no M1 emulado)"
echo ""

docker buildx build \
    --platform "${PLATFORM}" \
    --tag "${FULL}" \
    --push \
    .

echo ""
echo "  ✓ Push concluído: ${FULL}"

# ── 4. Branch submission ─────────────────────────────────────────────────────
echo ""
echo "▶ Criando branch submission..."

CURRENT_BRANCH=$(git branch --show-current)

git checkout -B submission

# Remove tudo que não é necessário para rodar o teste
git rm -rf src/ 2>/dev/null || true
git rm -f test-local.sh build-and-push.sh SETUP.md 2>/dev/null || true

# Adiciona só o necessário
git add docker-compose.yml
git add haproxy.cfg
git add info.json
git add participants/

git commit -m "submission: go rules knn uds $(date +%Y-%m-%d-%H%M)"

git push origin submission --force
echo "  ✓ Branch submission publicada"

# Volta para main
git checkout "${CURRENT_BRANCH}"

echo ""
echo "═══════════════════════════════════════════════════"
echo "  ✓ Tudo pronto!"
echo ""
echo "  Próximo passo — abrir issue no fork:"
echo "  https://github.com/Demians12/rinha-de-backend-2026/issues/new"
echo ""
echo "  Corpo da issue:"
echo "    rinha/test demians12-go-vptree"
echo "═══════════════════════════════════════════════════"
