# OpenChuspa-core

El **motor caleño**: un servidor de almacenamiento de objetos de un solo nodo, compatible con la API de AWS S3, escrito en Go. Abierto por diseño y seguro por defecto.

El diseño completo está en [`MVP-ENGINE-v2.md`](./MVP-ENGINE-v2.md). Las decisiones que se apartan de él quedan en [`docs/adr/`](./docs/adr).

## Estado

Proyecto en construcción, versión `0.x`: todavía no hay releases ni imagen publicada, y no es apto para producción.

Lo que hay funciona de verdad contra clientes reales. Esta tabla solo lista lo que pasa en [`test/compat/awscli.sh`](./test/compat/awscli.sh) o en las pruebas unitarias.

| Operación | Estado |
|---|---|
| ListBuckets, CreateBucket, HeadBucket, GetBucketLocation, DeleteBucket | Funciona |
| PutObject, GetObject (con `Range`), HeadObject, DeleteObject | Funciona |
| ListObjectsV2 (prefijo, delimitador, paginación, `encoding-type=url`) | Funciona |
| CreateMultipartUpload, UploadPart, CompleteMultipartUpload, AbortMultipartUpload, ListParts | Funciona |
| Autenticación SigV4 por header, con ventana de 15 minutos | Funciona |
| Cuerpos `aws-chunked` firmados y sin firmar, con trailers | Funciona (pruebas propias y vectores de AWS) |
| Checksums CRC32, CRC32C, CRC64NVME, SHA-1 y SHA-256 | Funciona |
| DeleteObjects, CopyObject, ListObjects v1, ListMultipartUploads | `NotImplemented` (fase 1.1) |
| URLs prefirmadas, versionado, ACL, políticas, tags, CORS, lifecycle | `NotImplemented` |

Clientes probados de punta a punta: **AWS CLI v2**, **boto3** y **curl**. Faltan `@aws-sdk/client-s3`, `aws-sdk-go-v2` como cliente y `ceph/s3-tests`.

Lo que no está implementado responde `501 NotImplemented`; nunca un éxito falso.

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

El direccionamiento es por ruta (`/chuspa/key`), probado con el endpoint por IP (`http://127.0.0.1:9000`). El estilo de host virtual (`chuspa.host`) no está soportado: si un SDK lo elige, hay que activarle *path style*.

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

El servidor **no arranca** sin credenciales, con un secreto corto, en modo anónimo fuera de loopback, ni con las credenciales de ejemplo de la documentación fuera de loopback.

No hay TLS propio todavía: fuera de loopback necesita un proxy con TLS delante, y el servidor lo avisa en el log.

## Cómo guarda los datos

```
<CHUSPA_DATA_DIR>/
  meta.db          índice bbolt: chuspas, objetos y subidas multipart
  blobs/ab/cd/…    contenido, con un nombre aleatorio generado por el servidor
  staging/         escrituras en curso
```

La key de un objeto vive solo en el índice y nunca forma parte de una ruta de archivo, así que `../../etc/passwd` es un nombre como cualquier otro. Cada escritura va a `staging/`, se sincroniza a disco, se mueve a `blobs/` y solo entonces se confirma en el índice: una caída deja el objeto anterior o el nuevo, nunca uno a medias. Al arrancar se borran los restos de escrituras interrumpidas.

## Código

```
cmd/chuspa/              binario: serve, healthcheck, version
internal/config/         variables de entorno y validación de arranque
internal/s3api/          enrutado, handlers, XML y catálogo de errores
internal/auth/           verificación SigV4
internal/chunked/        decodificador aws-chunked
internal/checksum/       algoritmos de checksum
internal/storage/        interfaz Backend
internal/storage/fsbolt/ blobs en disco + índice bbolt
internal/httpx/          request-id, log de acceso, pánicos, límite de conexiones
test/compat/             pruebas con clientes reales
```

Dependencias en tiempo de ejecución: la biblioteca estándar y `go.etcd.io/bbolt` (MIT). `aws-sdk-go-v2` (Apache-2.0) se usa solo en pruebas.

## Pruebas

```bash
go vet ./...
go test -race ./...

# Compatibilidad con clientes reales: compila el motor, lo arranca en una
# carpeta temporal y ejecuta los criterios de aceptación del MVP.
# Requiere aws (CLI v2) y curl; usa boto3 si está instalado.
test/compat/awscli.sh

# Fuzzing de los analizadores de entrada no confiable
go test ./internal/chunked -run '^$' -fuzz '^FuzzReader$' -fuzztime 1m
go test ./internal/auth -run '^$' -fuzz '^FuzzParseAuthorization$' -fuzztime 1m
go test ./internal/s3api -run '^$' -fuzz '^FuzzParseRange$' -fuzztime 1m
```

La suite de compatibilidad genera credenciales aleatorias en cada ejecución y aísla al AWS CLI de la configuración y los perfiles de quien la corre.

## Pendiente para cerrar el MVP

- Licencia y archivos del proyecto (`LICENSE`, `CONTRIBUTING.md`, `SECURITY.md`, `GOVERNANCE.md`): dependen de las decisiones de la sección 14 del diseño.
- Imagen distroless firmada, `docker-compose.yml` y primer release.
- `ceph/s3-tests` y los SDK de JavaScript y Go en CI; `govulncheck`, `staticcheck`, CodeQL y verificación de licencias.
- Recolección periódica de huérfanos y expiración de subidas multipart abandonadas (hoy solo se limpia al arrancar).
- Subcomando `chuspa inspect`, copia periódica y reconstrucción del índice.
- Tiempo máximo de inactividad durante la lectura del cuerpo de una subida.
