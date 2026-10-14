# OpenChuspa-core

El **motor caleño**: un servidor de almacenamiento de objetos de un solo nodo, compatible con la API de AWS S3, escrito en Go. Abierto por diseño y seguro por defecto.

Las decisiones de diseño vigentes quedan en [`docs/adr/`](./docs/adr); los documentos con los que se diseñó el motor están en [`docs/historial/`](./docs/historial).

## Estado

Versión estable `1.0.0`: un nodo, con datos versionados y reconstruibles, verificación de contenido, TLS nativo y probado en cada cambio contra los SDK de AWS, `ceph/s3-tests` y una prueba de resistencia con `kill -9`. La imagen está en Docker Hub (`openchuspa/chuspa`) y en `ghcr.io/openchuspa/chuspa`, firmada con cosign. Como con cualquier almacenamiento de un solo nodo, mantenga copias de lo que no pueda perder (ver «Cómo guarda los datos»).

Lo que hay funciona de verdad contra clientes reales. Esta tabla solo lista lo que pasa en CI, en [`test/compat/run.sh`](./test/compat/run.sh) o en las pruebas unitarias.

| Operación | Estado |
|---|---|
| ListBuckets, CreateBucket, HeadBucket, GetBucketLocation, DeleteBucket | Funciona |
| PutObject, GetObject (con `Range` y condiciones), HeadObject, DeleteObject, DeleteObjects | Funciona |
| Escrituras condicionales (`If-None-Match: *`, `If-Match`) en PutObject y CompleteMultipartUpload | Funciona |
| CopyObject y UploadPartCopy (copias grandes) | Funciona |
| ListObjectsV2 y ListObjects v1 (prefijo, delimitador, paginación, `encoding-type=url`) | Funciona |
| CreateMultipartUpload, UploadPart, CompleteMultipartUpload, AbortMultipartUpload, ListParts, ListMultipartUploads | Funciona |
| Autenticación SigV4 por header, con ventana de 15 minutos | Funciona |
| Cuerpos `aws-chunked` firmados y sin firmar, con trailers | Funciona |
| Checksums CRC32, CRC32C, CRC64NVME, SHA-1 y SHA-256 | Funciona |
| ListObjectVersions y `versionId=null` | Solo la vista de una chuspa sin versionado |
| GetObjectTagging | Responde siempre una lista vacía |
| URLs prefirmadas, versionado, ACL, políticas, tags, CORS, lifecycle, object lock, cifrado | `NotImplemented` |

Clientes probados de punta a punta en CI: **AWS CLI v2**, **boto3**, **`@aws-sdk/client-s3`**, **`aws-sdk-go-v2`** y **curl**. De `ceph/s3-tests` pasan 222 pruebas de 744 ejecutadas; el detalle está en [`docs/compatibilidad.md`](./docs/compatibilidad.md).

Lo que no está implementado responde `501 NotImplemented`, y un header que pida algo que el motor no hace (ACL pública, cifrado, tags, object lock) se rechaza en vez de ignorarse: nunca un éxito falso.

## Inicio rápido

Requiere Go 1.27 o superior.

```bash
go build -o chuspa ./cmd/chuspa

export CHUSPA_ROOT_USER=mi-llave
export CHUSPA_ROOT_PASSWORD="$(openssl rand -hex 24)"   # mínimo 32 caracteres
export CHUSPA_DATA_DIR=./data
./chuspa serve
```

En otra terminal, con las mismas credenciales:

```bash
export AWS_ACCESS_KEY_ID=$CHUSPA_ROOT_USER
export AWS_SECRET_ACCESS_KEY=$CHUSPA_ROOT_PASSWORD
export AWS_DEFAULT_REGION=cali-san-antonio-1
export AWS_ENDPOINT_URL=http://127.0.0.1:9000

aws s3 mb s3://feria-de-cali
aws s3 cp salsa.txt s3://feria-de-cali/test/salsa.txt
aws s3 ls s3://feria-de-cali/test/
```

El direccionamiento es por ruta (`/chuspa/key`). El estilo de host virtual (`chuspa.host`) no está soportado: en los SDK de JavaScript y Go hay que activar *path style* (`forcePathStyle` / `UsePathStyle`).

### Con Docker

```bash
mkdir -p secrets && openssl rand -hex 24 > secrets/chuspa_root_password
chmod 600 secrets/chuspa_root_password
echo "CHUSPA_ROOT_USER=mi-llave" > .env
docker compose up -d
```

El `docker-compose.yml` usa la imagen publicada con su versión exacta. Para construirla desde el código: `docker build -t chuspa:local .`.

La imagen es distroless: sin shell, sin root, con el sistema de archivos de solo lectura y sin capabilities. El puerto se publica solo en loopback.

## CLI `ve`

`ve` traduce los comandos caleños a S3. Sirve con el motor y con cualquier servicio compatible con S3.

```bash
go build -o ve ./cmd/ve
export AWS_ACCESS_KEY_ID=mi-llave AWS_SECRET_ACCESS_KEY="$SECRETO"
# VE_ENDPOINT es http://localhost:9000 por defecto; VE_REGION, cali-san-antonio-1.

./ve crea chuspa://feria-de-cali
./ve subi ./cancion.mp3 a chuspa://feria-de-cali/music/
./ve mira chuspa://feria-de-cali/music/
./ve baja chuspa://feria-de-cali/music/cancion.mp3 .
./ve sincroniza ./mis-fotos chuspa://feria-de-cali/fotos/
./ve borra chuspa://feria-de-cali/music/cancion.mp3
./ve borra chuspa://feria-de-cali -r      # la vacía y la borra
```

| `ve` | AWS CLI |
|---|---|
| `ve mira chuspa://[chuspa/prefijo] [-r]` | `aws s3 ls [--recursive]` |
| `ve crea chuspa://chuspa` | `aws s3 mb` |
| `ve subi <archivo>... [a] chuspa://chuspa/key` | `aws s3 cp` (multipart desde 64 MiB) |
| `ve baja chuspa://chuspa/key [destino]` | `aws s3 cp` |
| `ve borra chuspa://chuspa/key`, `-r` para un prefijo o la chuspa entera | `aws s3 rm`, `aws s3 rb --force` |
| `ve sincroniza <carpeta> chuspa://...` y al revés | `aws s3 sync`, sin borrar en el destino |

`s3://` también se acepta como dirección. Al bajar o sincronizar, las keys que se saldrían de la carpeta de destino (como `../../etc/passwd`) se saltan con un aviso. `ve ayuda` muestra todas las opciones.

## Configuración

| Variable | Por defecto | Descripción |
|---|---|---|
| `CHUSPA_ADDR` | `127.0.0.1:9000` | Dirección de escucha. Loopback por defecto. |
| `CHUSPA_DATA_DIR` | `/data` | Carpeta raíz de datos. |
| `CHUSPA_ROOT_USER` | *(obligatoria)* | Access key: de 3 a 128 letras, dígitos, `-` o `_`. |
| `CHUSPA_ROOT_PASSWORD` | *(obligatoria)* | Secret key, mínimo 32 caracteres. |
| `CHUSPA_ROOT_PASSWORD_FILE` | *(vacío)* | Ruta a un archivo con el secreto; alternativa a la anterior. |
| `CHUSPA_ROOT_PASSWORD_PREVIOUS` (o `_FILE`) | *(vacío)* | Secreto anterior, aceptado además del actual mientras se rota. |
| `CHUSPA_TLS_CERT_FILE` / `CHUSPA_TLS_KEY_FILE` | *(vacío)* | Certificado y llave PEM para TLS nativo (1.2 o superior). Se releen al cambiar en disco. |
| `CHUSPA_METRICS_ADDR` | *(vacío)* | Dirección para las métricas de Prometheus en `/metrics`, aparte de la API (por ejemplo `127.0.0.1:9100`). Vacía, no se publican. |
| `CHUSPA_AUTH_MAX_FAILURES` | `20` | Autenticaciones fallidas por minuto que bloquean una IP durante 5 minutos (`503 SlowDown`). `0` lo desactiva. |
| `CHUSPA_REGION` | `cali-san-antonio-1` | Región que reporta el servidor y que exige en la firma. |
| `CHUSPA_ALLOW_ANONYMOUS` | `false` | Sin autenticación. Solo se acepta escuchando en loopback. |
| `CHUSPA_LOG_FORMAT` | `json` | `json` o `text`. |
| `CHUSPA_MULTIPART_EXPIRY` | `168h` | Edad a la que una subida multipart sin completar se aborta sola. `0` la desactiva. |
| `CHUSPA_MIN_FREE_MB` | `1024` | Espacio libre mínimo en disco, en MiB. Por debajo, las escrituras responden `507 InsufficientStorage`. `0` lo desactiva. |
| `CHUSPA_SCRUB_INTERVAL` | `168h` | Cada cuánto se relee todo el contenido y se compara con su SHA-256; cada objeto dañado queda en el log como error. `0` lo desactiva. |

El servidor **no arranca** sin credenciales, con un secreto corto, en modo anónimo fuera de loopback, ni con las credenciales de ejemplo de la documentación fuera de loopback.

Fuera de loopback, el servidor necesita TLS: el propio (`CHUSPA_TLS_CERT_FILE` y `CHUSPA_TLS_KEY_FILE`) o un proxy con TLS delante. Sin ninguno de los dos lo avisa en el log. Detrás de un proxy, el bloqueo por fuerza bruta ve la IP del proxy: un ataque bloquea a todos durante 5 minutos, lo que se prefiere a dejar probar llaves sin límite.

**Rotar el secreto sin cortar el servicio:** se pone el nuevo en `CHUSPA_ROOT_PASSWORD`, el actual en `CHUSPA_ROOT_PASSWORD_PREVIOUS` y se reinicia; se cambian los clientes; se quita `CHUSPA_ROOT_PASSWORD_PREVIOUS` y se reinicia otra vez.

## Monitoreo

Con `CHUSPA_METRICS_ADDR` el motor publica en `/metrics`, en formato de Prometheus:

| Métrica | Para qué |
|---|---|
| `chuspa_requests_total{method,code}` | Tráfico y errores; alertar sobre la tasa de `5xx`. |
| `chuspa_request_duration_seconds` | Latencia por método (histograma). |
| `chuspa_received_bytes_total`, `chuspa_sent_bytes_total` | Volumen. |
| `chuspa_disk_free_bytes` | Alertar antes de llegar a `CHUSPA_MIN_FREE_MB`. |
| `chuspa_scrub_damaged_objects` | Debe ser 0; cualquier otro valor es un disco dañando datos. |
| `chuspa_last_scrub_timestamp_seconds`, `chuspa_last_backup_timestamp_seconds` | Que la verificación y la copia del índice sigan corriendo. |
| `chuspa_maintenance_errors_total` | Fallos del mantenimiento. |
| `chuspa_auth_blocks_total` | IPs bloqueadas por fuerza bruta. |
| `chuspa_build_info{version}` | Versión desplegada. |

Las métricas no llevan keys, nombres de chuspa ni secretos.

## Cómo guarda los datos

```
<CHUSPA_DATA_DIR>/
  meta.db               índice bbolt: chuspas, objetos y subidas multipart
  blobs/ab/cd/<id>      contenido, con un nombre aleatorio generado por el servidor
  blobs/ab/cd/<id>.meta chuspa, key y metadatos del objeto, para reconstruir el índice
  chuspas/<nombre>      una marca por chuspa
  staging/              escrituras en curso
```

La key de un objeto vive solo en el índice y nunca forma parte de una ruta de archivo, así que `../../etc/passwd` es un nombre como cualquier otro. Cada escritura va a `staging/`, se sincroniza a disco, se mueve a `blobs/` y solo entonces se confirma en el índice: una caída deja el objeto anterior o el nuevo, nunca uno a medias. Al arrancar se borran los restos de escrituras interrumpidas.

Con el servidor en marcha, cada hora se borran los blobs sin referencia y se abortan las subidas multipart abandonadas, cada día queda una copia consistente del índice en `meta.db.bak` y, cada semana, se relee todo el contenido contra su SHA-256 para detectar un disco que se está dañando.

**Si `meta.db` se daña o se pierde**, con el servidor detenido:

```bash
CHUSPA_DATA_DIR=./data ./chuspa rebuild-index   # índice nuevo desde blobs/ y chuspas/
CHUSPA_DATA_DIR=./data ./chuspa verify          # relee todo; sale con error si algo no cuadra
```

`rebuild-index` no borra nada: renombra el índice anterior a `meta.db.reemplazado-<fecha>` y aparta en `recuperados/` lo que no puede asignar a un objeto (partes de subidas sin completar, versiones viejas que quedaron por una caída). Las subidas multipart en curso no se recuperan; los objetos y las chuspas, sí, también las vacías.

**Formato y actualizaciones.** El índice guarda la versión del formato de los datos. Al arrancar, una versión nueva migra sola los datos de las anteriores (de 0.1.x y 0.2.x al formato 2, escribiendo los metadatos junto a cada blob), y una versión vieja se niega a abrir datos de una más nueva para no dañarlos. El CI prueba la migración con datos reales escritos por v0.2.0 (`internal/storage/fsbolt/testdata/formato1`). Antes de actualizar, conviene igual una copia de la carpeta de datos con el servidor detenido.

Como las keys viven en el índice, los datos no se inspeccionan con `ls`. Con el servidor detenido:

```bash
export CHUSPA_DATA_DIR=./data
./chuspa inspect                          # chuspas, con cantidad de objetos y bytes
./chuspa inspect feria-de-cali test/      # objetos de una chuspa, por prefijo
./chuspa export feria-de-cali test/salsa.txt ./salsa.txt
```

## Código

```
cmd/chuspa/              binario: serve, healthcheck, inspect, export, verify, rebuild-index, version
cmd/ve/                  CLI caleña: mira, crea, subi, baja, borra, sincroniza
internal/config/         variables de entorno y validación de arranque
internal/s3api/          enrutado, handlers, XML y catálogo de errores
internal/auth/           verificación SigV4
internal/chunked/        decodificador aws-chunked
internal/checksum/       algoritmos de checksum
internal/storage/        interfaz Backend
internal/storage/fsbolt/ blobs en disco + índice bbolt
internal/httpx/          request-id, log de acceso, pánicos, límite de conexiones
test/compat/             pruebas con clientes reales
test/s3tests/            configuración y lista de pruebas de ceph/s3-tests
```

Dependencias en tiempo de ejecución del motor: la biblioteca estándar y `go.etcd.io/bbolt` (MIT). La CLI `ve` usa además `aws-sdk-go-v2` (Apache-2.0), que el motor solo usa en pruebas.

## Pruebas

```bash
go vet ./...
go test -race ./...

# Compatibilidad con clientes reales: compila el motor, lo arranca en una
# carpeta temporal y ejecuta los criterios de aceptación del MVP.
# Requiere aws (CLI v2) y curl; usa boto3 y Node.js si están instalados.
test/compat/run.sh

# Conformidad con ceph/s3-tests: las pruebas que ya pasan no pueden dejar de pasar
test/s3tests/run.sh

# Análisis estático, vulnerabilidades y licencias, con las versiones del CI
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# Resistencia: clientes concurrentes mientras el motor se mata con kill -9;
# todo lo confirmado debe leerse idéntico y chuspa verify no debe ver daños
go run ./test/soak -duration 2m

# Fuzzing de los analizadores de entrada no confiable
go test ./internal/chunked -run '^$' -fuzz '^FuzzReader$' -fuzztime 1m
go test ./internal/auth -run '^$' -fuzz '^FuzzParseAuthorization$' -fuzztime 1m
go test ./internal/s3api -run '^$' -fuzz '^FuzzParseRange$' -fuzztime 1m
```

Cada noche, el workflow `nightly` corre 15 minutos de fuzzing por analizador y 15 minutos de la prueba de resistencia con 16 clientes.

La suite de compatibilidad genera credenciales aleatorias en cada ejecución y aísla al AWS CLI de la configuración y los perfiles de quien la corre.

## Seguridad, contribuciones y licencia

- Las vulnerabilidades se reportan en privado: [SECURITY.md](./SECURITY.md).
- Para contribuir: [CONTRIBUTING.md](./CONTRIBUTING.md) y [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md). Quién decide y cómo: [GOVERNANCE.md](./GOVERNANCE.md).
- Licencia: [Apache-2.0](./LICENSE).
