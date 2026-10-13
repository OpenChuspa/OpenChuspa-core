#!/usr/bin/env bash
# Prueba de compatibilidad con clientes reales: compila el motor, lo arranca
# en un directorio temporal y ejecuta los criterios de aceptación del MVP
# (MVP-ENGINE-v2.md, sección 11) con el AWS CLI, curl y, si está, boto3.
#
# Uso: test/compat/awscli.sh
# Requiere: go, aws (CLI v2), curl. Opcional: python3 con boto3.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d)
PORT=${CHUSPA_TEST_PORT:-19000}
ENDPOINT="http://127.0.0.1:$PORT"
BUCKET=feria-de-cali
SERVER_PID=""

# Credenciales de prueba generadas en cada ejecución; nunca salen de aquí.
ACCESS_KEY="prueba-$(openssl rand -hex 4)"
SECRET_KEY="$(openssl rand -hex 24)"

# El CLI no debe leer la configuración ni los perfiles de quien ejecuta.
export AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null
export AWS_EC2_METADATA_DISABLED=true AWS_PAGER="" AWS_MAX_ATTEMPTS=1
export AWS_ACCESS_KEY_ID=$ACCESS_KEY AWS_SECRET_ACCESS_KEY=$SECRET_KEY
export AWS_DEFAULT_REGION=cali-san-antonio-1
unset AWS_PROFILE AWS_SESSION_TOKEN

s3() { aws --endpoint-url "$ENDPOINT" "$@"; }
step() { printf '\n== %s\n' "$*"; }
fail() { printf 'FALLÓ: %s\n' "$*" >&2; exit 1; }

stop_server() {
  [ -n "$SERVER_PID" ] || return 0
  kill "-${1:-TERM}" "$SERVER_PID" 2>/dev/null || true
  wait "$SERVER_PID" 2>/dev/null || true
  SERVER_PID=""
}
cleanup() { stop_server KILL; rm -rf "$WORK"; }
trap cleanup EXIT

start_server() {
  CHUSPA_ADDR="127.0.0.1:$PORT" CHUSPA_DATA_DIR="$WORK/data" \
  CHUSPA_ROOT_USER=$ACCESS_KEY CHUSPA_ROOT_PASSWORD=$SECRET_KEY \
    "$WORK/chuspa" serve >>"$WORK/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 50); do
    CHUSPA_ADDR="127.0.0.1:$PORT" "$WORK/chuspa" healthcheck 2>/dev/null && return 0
    sleep 0.1
  done
  cat "$WORK/server.log" >&2
  fail "el servidor no arrancó"
}

# expect_error <código S3> <comando...>: el comando debe fallar con ese código.
expect_error() {
  local code=$1 out; shift
  if out=$("$@" 2>&1); then fail "se esperaba $code y el comando tuvo éxito: $*"; fi
  grep -q "$code" <<<"$out" || fail "se esperaba $code, se obtuvo: $out"
}

# expect_no_start <descripción> <VAR=valor...>: el servidor no debe arrancar.
expect_no_start() {
  local what=$1; shift
  if env -i PATH="$PATH" CHUSPA_DATA_DIR="$WORK/nostart" "$@" "$WORK/chuspa" serve >"$WORK/nostart.log" 2>&1; then
    fail "arrancó $what"
  fi
  [ ! -e "$WORK/nostart" ] || fail "se creó la carpeta de datos $what"
}

cd "$WORK"
step "compilar"
(cd "$ROOT" && go build -o "$WORK/chuspa" ./cmd/chuspa)

step "arranque seguro"
expect_no_start "sin credenciales"
expect_no_start "con un secreto corto" CHUSPA_ROOT_USER=parcero CHUSPA_ROOT_PASSWORD=corto
expect_no_start "anónimo fuera de loopback" CHUSPA_ADDR=0.0.0.0:$PORT CHUSPA_ALLOW_ANONYMOUS=true
expect_no_start "con credenciales de ejemplo fuera de loopback" CHUSPA_ADDR=0.0.0.0:$PORT \
  CHUSPA_ROOT_USER=clave-del-parchao CHUSPA_ROOT_PASSWORD="$SECRET_KEY"

start_server

step "chuspas"
s3 s3 mb "s3://$BUCKET"
s3 s3 ls | grep -q "$BUCKET" || fail "la chuspa no aparece en el listado"
expect_error BucketAlreadyOwnedByYou s3 s3api create-bucket --bucket "$BUCKET"
expect_error InvalidBucketName s3 s3api create-bucket --bucket "Con_Mayusculas"
[ "$(s3 s3api get-bucket-location --bucket "$BUCKET" --query LocationConstraint --output text)" = cali-san-antonio-1 ] ||
  fail "get-bucket-location"

step "objeto pequeño de ida y vuelta"
echo "oíste mira" >salsa.txt
s3 s3 cp salsa.txt "s3://$BUCKET/test/salsa.txt"
s3 s3 ls "s3://$BUCKET/test/" | grep -q salsa.txt || fail "el objeto no aparece en el listado"
s3 s3 cp "s3://$BUCKET/test/salsa.txt" bajado.txt
cmp salsa.txt bajado.txt
: >vacio.bin
s3 s3 cp vacio.bin "s3://$BUCKET/vacio.bin"
s3 s3 cp "s3://$BUCKET/vacio.bin" vacio.bajado
cmp vacio.bin vacio.bajado

step "archivo de 100 MB: multipart en la subida y rangos en la descarga"
head -c 104857600 /dev/urandom >grande.bin
s3 s3 cp grande.bin "s3://$BUCKET/grande.bin" --only-show-errors
s3 s3 cp "s3://$BUCKET/grande.bin" grande.bajado --only-show-errors
cmp grande.bin grande.bajado
s3 s3api head-object --bucket "$BUCKET" --key grande.bin --query ETag --output text | grep -q -- '-[0-9]*"$' ||
  fail "el ETag de la subida grande no es de multipart"
s3 s3api get-object --bucket "$BUCKET" --key grande.bin --range bytes=1000-1999 rango.bin >/dev/null
cmp rango.bin <(dd if=grande.bin bs=1 skip=1000 count=1000 2>/dev/null)

step "listado con más de 1000 objetos"
mkdir muchos
for i in $(seq -w 1 1050); do echo "$i" >"muchos/obj-$i.txt"; done
s3 s3 cp muchos "s3://$BUCKET/muchos/" --recursive --only-show-errors
[ "$(s3 s3 ls "s3://$BUCKET/muchos/" | wc -l | tr -d ' ')" = 1050 ] || fail "el listado paginado no devolvió 1050 objetos"
[ "$(s3 s3api list-objects-v2 --bucket "$BUCKET" --delimiter / --query 'CommonPrefixes[].Prefix' --output text | tr '\t' ' ')" = "muchos/ test/" ] ||
  fail "los prefijos comunes no coinciden"

step "keys hostiles y con caracteres raros"
s3 s3api put-object --bucket "$BUCKET" --key '../../etc/passwd' --body salsa.txt >/dev/null
s3 s3api get-object --bucket "$BUCKET" --key '../../etc/passwd' hostil.txt >/dev/null
cmp salsa.txt hostil.txt
rara='con espacio/ñandú+más&raro=(1)*.txt'
s3 s3 cp salsa.txt "s3://$BUCKET/$rara"
s3 s3 ls "s3://$BUCKET/con espacio/" | grep -qF 'ñandú+más&raro=(1)*.txt' || fail "la key rara no se lista bien"
s3 s3 cp "s3://$BUCKET/$rara" rara.txt
cmp salsa.txt rara.txt
[ "$(ls "$WORK/data" | sort | tr '\n' ' ')" = "blobs meta.db staging " ] || fail "hay archivos inesperados en la carpeta de datos"
[ ! -e "$WORK/etc" ] || fail "una key salió de la carpeta de datos"

step "integridad"
expect_error BadDigest s3 s3api put-object --bucket "$BUCKET" --key malo.txt --body salsa.txt --checksum-crc32 AAAAAA==
expect_error BadDigest s3 s3api put-object --bucket "$BUCKET" --key malo.txt --body salsa.txt --content-md5 1B2M2Y8AsgTpgAmY7PhCfg==
# Cuerpo alterado después de firmar: se declara el SHA-256 de otro contenido.
otro_sha=$(printf 'otro contenido' | openssl dgst -sha256 | awk '{print $NF}')
code=$(curl -s -o alterado.xml -w '%{http_code}' -X PUT --data-binary @salsa.txt \
  --aws-sigv4 "aws:amz:$AWS_DEFAULT_REGION:s3" --user "$ACCESS_KEY:$SECRET_KEY" \
  -H "x-amz-content-sha256: $otro_sha" "$ENDPOINT/$BUCKET/malo.txt")
[ "$code" = 400 ] && grep -q XAmzContentSHA256Mismatch alterado.xml || fail "cuerpo alterado: HTTP $code $(cat alterado.xml)"
expect_error 404 s3 s3api head-object --bucket "$BUCKET" --key malo.txt

step "autenticación"
code=$(curl -s -o sinfirma.xml -w '%{http_code}' "$ENDPOINT/")
[ "$code" = 403 ] && grep -q AccessDenied sinfirma.xml || fail "petición sin firma: HTTP $code"
AWS_SECRET_ACCESS_KEY=$(openssl rand -hex 24) expect_error SignatureDoesNotMatch s3 s3 ls
AWS_ACCESS_KEY_ID=desconocida expect_error SignatureDoesNotMatch s3 s3 ls
AWS_DEFAULT_REGION=us-east-1 expect_error SignatureDoesNotMatch s3 s3 ls

step "límites y operaciones no implementadas"
# El servidor responde 431 y corta la conexión, así que curl sale con error.
code=$(curl -s -o /dev/null -w '%{http_code}' -H "x-relleno: $(head -c 100000 /dev/zero | tr '\0' a)" "$ENDPOINT/" || true)
[ "$code" = 431 ] || fail "headers gigantes: HTTP $code"
expect_error NotImplemented s3 s3api get-bucket-versioning --bucket "$BUCKET"
expect_error NotImplemented s3 s3api copy-object --bucket "$BUCKET" --key copia.txt --copy-source "$BUCKET/test/salsa.txt"
expect_error NoSuchKey s3 s3api get-object --bucket "$BUCKET" --key no-existe.txt /dev/null
expect_error NoSuchBucket s3 s3 ls s3://no-existe-esta-chuspa

if python3 -c 'import boto3' 2>/dev/null; then
  step "boto3 en su configuración por defecto"
  python3 -I "$ROOT/test/compat/boto3_flow.py" "$ENDPOINT" "$BUCKET"
else
  step "boto3 no está instalado: se omite"
fi

step "los datos sobreviven a un reinicio"
stop_server TERM
start_server
s3 s3 cp "s3://$BUCKET/grande.bin" grande.reinicio --only-show-errors
cmp grande.bin grande.reinicio

step "kill -9 en mitad de una subida"
# Subida lenta a propósito, para que el corte caiga con el cuerpo a medio llegar.
curl -s -o /dev/null -T grande.bin --limit-rate 5M \
  --aws-sigv4 "aws:amz:$AWS_DEFAULT_REGION:s3" --user "$ACCESS_KEY:$SECRET_KEY" \
  -H "x-amz-content-sha256: UNSIGNED-PAYLOAD" "$ENDPOINT/$BUCKET/interrumpida.bin" &
CLI_PID=$!
sleep 1
[ -n "$(ls "$WORK/data/staging")" ] || fail "no había ninguna escritura en curso al momento del corte"
stop_server KILL
wait "$CLI_PID" 2>/dev/null && fail "la subida terminó antes del kill -9; no se probó nada"
start_server
expect_error 404 s3 s3api head-object --bucket "$BUCKET" --key interrumpida.bin
[ -z "$(ls "$WORK/data/staging")" ] || fail "quedaron escrituras a medias en staging"
s3 s3 cp "s3://$BUCKET/test/salsa.txt" tras-caida.txt
cmp salsa.txt tras-caida.txt

step "borrado"
expect_error BucketNotEmpty s3 s3 rb "s3://$BUCKET"
s3 s3 rm "s3://$BUCKET" --recursive --only-show-errors
s3 s3 rb "s3://$BUCKET"
[ -z "$(s3 s3 ls)" ] || fail "quedaron chuspas después de borrar"
stop_server TERM
[ -z "$(find "$WORK/data/blobs" -type f)" ] || fail "quedaron blobs después de borrar todo"

step "sin secretos en los logs"
if grep -qe "$SECRET_KEY" -e 'Signature=' -e 'AWS4-HMAC-SHA256' "$WORK/server.log"; then
  fail "el log del servidor contiene material de autenticación"
fi

printf '\nTodo bien, mi llave: el motor pasó la prueba de compatibilidad.\n'
