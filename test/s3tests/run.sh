#!/usr/bin/env bash
# Conformidad con ceph/s3-tests (https://github.com/ceph/s3-tests, MIT).
#
# Compila el motor, lo arranca en una carpeta temporal y ejecuta las pruebas
# de test/s3tests/passing.txt: las que el motor ya pasa. Si alguna falla, es
# una regresión y el script termina con error.
#
#   test/s3tests/run.sh          ejecuta la lista (lo que hace el CI)
#   test/s3tests/run.sh --all    ejecuta test_s3.py completo y reescribe la
#                                lista con lo que pase; revise el diff antes
#                                de subirlo
#
# Requiere: go, git, python3 con venv.
set -euo pipefail

# Versión fija de la suite; se actualiza a mano junto con passing.txt.
S3TESTS_COMMIT=5b3987116eca0b4b8fa57d6b10f3db57bab196b6

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
LIST="$ROOT/test/s3tests/passing.txt"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/chuspa-s3tests.XXXXXX")
PORT=${CHUSPA_TEST_PORT:-19100}
SERVER_PID=""
SECRET_KEY="$(openssl rand -hex 24)"

cleanup() {
  [ -z "$SERVER_PID" ] || { kill "$SERVER_PID" 2>/dev/null || true; wait "$SERVER_PID" 2>/dev/null || true; }
  rm -rf "$WORK"
}
trap cleanup EXIT

(cd "$ROOT" && go build -o "$WORK/chuspa" ./cmd/chuspa)

git init -q "$WORK/src"
git -C "$WORK/src" fetch -q --depth 1 https://github.com/ceph/s3-tests.git "$S3TESTS_COMMIT"
git -C "$WORK/src" checkout -q FETCH_HEAD
python3 -m venv "$WORK/venv"
"$WORK/venv/bin/pip" install --quiet --disable-pip-version-check -r "$WORK/src/requirements.txt" pytest-timeout

# La suite exige varias identidades; el motor tiene una sola credencial raíz,
# así que todas apuntan a ella. Las pruebas que dependen de un segundo usuario
# no están en la lista.
identity() {
  printf '[%s]\ndisplay_name = openchuspa\nuser_id = openchuspa\nemail = %s@example.com\naccess_key = s3tests\nsecret_key = %s\n%s\n\n' \
    "$1" "${1// /-}" "$SECRET_KEY" "${2:-}"
}
{
  printf '[DEFAULT]\nhost = 127.0.0.1\nport = %s\nis_secure = False\nssl_verify = False\n\n' "$PORT"
  printf '[fixtures]\nbucket prefix = s3t-{random}-\niam name prefix = s3-tests-\niam path prefix = /s3-tests/\n\n'
  identity "s3 main" "api_name = us-east-1"
  identity "s3 alt"
  identity "s3 tenant" "tenant = testx"
  identity "iam"
  identity "iam root"
  identity "iam alt root"
} >"$WORK/s3tests.conf"

# La suite firma con us-east-1.
CHUSPA_ADDR="127.0.0.1:$PORT" CHUSPA_DATA_DIR="$WORK/data" CHUSPA_REGION=us-east-1 \
CHUSPA_ROOT_USER=s3tests CHUSPA_ROOT_PASSWORD=$SECRET_KEY CHUSPA_LOG_FORMAT=text \
  "$WORK/chuspa" serve >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do
  CHUSPA_ADDR="127.0.0.1:$PORT" "$WORK/chuspa" healthcheck 2>/dev/null && break
  sleep 0.1
done
CHUSPA_ADDR="127.0.0.1:$PORT" "$WORK/chuspa" healthcheck || { cat "$WORK/server.log" >&2; exit 1; }

export S3TEST_CONF="$WORK/s3tests.conf" AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null
unset AWS_PROFILE AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_DEFAULT_REGION AWS_REGION
cd "$WORK/src"
pytest() { "$WORK/venv/bin/python" -m pytest -p no:cacheprovider --timeout=120 -q "$@"; }

if [ "${1:-}" = "--all" ]; then
  pytest s3tests/functional/test_s3.py -rN --junitxml="$WORK/junit.xml" || true
  "$WORK/venv/bin/python" - "$WORK/junit.xml" "$LIST" <<'PY'
import sys
import xml.etree.ElementTree as ET

cases = list(ET.parse(sys.argv[1]).iter("testcase"))
skipped = [c for c in cases if c.find("skipped") is not None]
passed = sorted(
    "s3tests/functional/test_s3.py::" + c.get("name")
    for c in cases
    if all(c.find(tag) is None for tag in ("skipped", "failure", "error"))
)
with open(sys.argv[2], "w") as f:
    f.write("\n".join(passed) + "\n")
print(f"test_s3.py: {len(passed)} pasan de {len(cases)} ({len(skipped)} omitidas por la propia suite)")
PY
else
  # shellcheck disable=SC2046
  pytest $(tr '\n' ' ' <"$LIST")
  printf '\ns3-tests: pasan las %s pruebas de la lista.\n' "$(wc -l <"$LIST" | tr -d ' ')"
fi
