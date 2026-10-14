# OpenChuspa-core

El **motor caleño**: un servidor de almacenamiento de objetos de un solo nodo, compatible con la API de AWS S3, escrito en Go. Abierto por diseño y seguro por defecto.

El diseño completo está en [`MVP-ENGINE-v2.md`](./MVP-ENGINE-v2.md). Las decisiones que se apartan de él quedan en [`docs/adr/`](./docs/adr).

## Estado

Proyecto en construcción, versión `0.x`: todavía no hay releases ni imagen publicada, y no ha pasado una auditoría de seguridad externa. No lo use para datos que no pueda perder.

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
| GetObjectTagging | Siempre responde vacío: aún no se pueden poner tags |
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

Todavía no hay imagen publicada; se construye desde el repositorio:

```bash
mkdir -p secrets && openssl rand -hex 24 > secrets/chuspa_root_password
chmod 600 secrets/chuspa_root_password
echo "CHUSPA_ROOT_USER=mi-llave" > .env
docker compose up -d --build
```

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
| `CHUSPA_REGION` | `cali-san-antonio-1` | Región que reporta el servidor y que exige en la firma. |
| `CHUSPA_ALLOW_ANONYMOUS` | `false` | Sin autenticación. Solo se acepta escuchando en loopback. |
| `CHUSPA_LOG_FORMAT` | `json` | `json` o `text`. |
| `CHUSPA_MULTIPART_EXPIRY` | `168h` | Edad a la que una subida multipart sin completar se aborta sola. `0` la desactiva. |
| `CHUSPA_MIN_FREE_MB` | `1024` | Espacio libre mínimo en disco, en MiB. Por debajo, las escrituras responden `507 InsufficientStorage`. `0` lo desactiva. |
| `CHUSPA_SCRUB_INTERVAL` | `168h` | Cada cuánto se relee todo el contenido y se compara con su SHA-256; cada objeto dañado queda en el log como error. `0` lo desactiva. |

El servidor **no arranca** sin credenciales, con un secreto corto, en modo anónimo fuera de loopback, ni con las credenciales de ejemplo de la documentación fuera de loopback.

No hay TLS propio todavía: fuera de loopback necesita un proxy con TLS delante, y el servidor lo avisa en el log.

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

# Fuzzing de los analizadores de entrada no confiable
go test ./internal/chunked -run '^$' -fuzz '^FuzzReader$' -fuzztime 1m
go test ./internal/auth -run '^$' -fuzz '^FuzzParseAuthorization$' -fuzztime 1m
go test ./internal/s3api -run '^$' -fuzz '^FuzzParseRange$' -fuzztime 1m
```

La suite de compatibilidad genera credenciales aleatorias en cada ejecución y aísla al AWS CLI de la configuración y los perfiles de quien la corre.

## Seguridad, contribuciones y licencia

- Las vulnerabilidades se reportan en privado: [SECURITY.md](./SECURITY.md).
- Para contribuir: [CONTRIBUTING.md](./CONTRIBUTING.md) y [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md). Quién decide y cómo: [GOVERNANCE.md](./GOVERNANCE.md).
- Licencia: [Apache-2.0](./LICENSE).

## Pendiente

- Lo que sigue en el roadmap del diseño: URLs prefirmadas, host virtual, TLS nativo, varios usuarios y políticas, tags, versionado, cifrado en reposo y replicación.
