# OpenChuspa Engine — MVP del Motor S3 Caleño

> Motor de almacenamiento de objetos **compatible con AWS S3**, construido desde cero (sin MinIO como base). Documento de alcance, arquitectura y plan de implementación del MVP.

---

## 1. Objetivo

Construir un servidor de almacenamiento de objetos que hable el **protocolo S3** lo suficiente como para que clientes estándar funcionen de verdad contra él:

- AWS CLI (`aws s3 ...`)
- `boto3` (Python)
- `@aws-sdk/client-s3` (Node.js)
- `aws-sdk-go-v2` (Go)
- `curl` directo a la API REST

El binario corre en un contenedor Docker propio (`openchuspa/chuspa`), **sin depender de MinIO ni de ningún otro servidor de objetos**.

---

## 2. Lenguaje y stack

| Decisión | Elección | Motivo |
|---|---|---|
| Lenguaje | **Go** | Binario estático único → imagen Docker mínima; concurrencia nativa (goroutines) ideal para I/O; stdlib de crypto facilita AWS SigV4. |
| Router HTTP | `net/http` (stdlib) | Sin dependencias extra; suficiente para enrutar por método + path. |
| Serialización | `encoding/xml` (stdlib) | S3 responde en XML; la stdlib basta. |
| Almacenamiento | **Filesystem** (disco local) | Simple, inspeccionable, montable como volumen Docker. |
| Auth | **AWS Signature V4** (opcional vía env) | Es lo que hace a un servidor "S3 compatible" de verdad. |

Sin frameworks pesados: todo con la librería estándar de Go + (opcional) un router ligero si hace falta.

---

## 3. Alcance del MVP

### ✅ Dentro del MVP (Fase 1)

Operaciones del protocolo S3 a implementar:

| Operación S3 | Método + Ruta | Jerga caleña | Descripción |
|---|---|---|---|
| ListBuckets | `GET /` | `ve mira chuspa://` | Lista todas las chuspas |
| CreateBucket | `PUT /{bucket}` | `ve crea` | Crea una chuspa |
| DeleteBucket | `DELETE /{bucket}` | `ve borra chuspa://nombre` | Borra una chuspa vacía |
| HeadBucket | `HEAD /{bucket}` | — | ¿Existe la chuspa? |
| ListObjectsV2 | `GET /{bucket}?list-type=2` | `ve mira chuspa://nombre/` | Lista chucherías |
| PutObject | `PUT /{bucket}/{key}` | `ve subi` | Sube una chuchería |
| GetObject | `GET /{bucket}/{key}` | `ve baja` | Descarga una chuchería |
| HeadObject | `HEAD /{bucket}/{key}` | — | Metadata del objeto |
| DeleteObject | `DELETE /{bucket}/{key}` | `ve borra` | Elimina una chuchería |

Requisitos transversales:
- Respuestas en el **XML exacto** que espera el protocolo S3 (incluyendo errores: `NoSuchBucket`, `NoSuchKey`, `BucketAlreadyExists`, etc.).
- Headers correctos: `ETag` (MD5 del contenido), `Content-Type`, `Content-Length`, `Last-Modified`, `x-amz-request-id`.
- **Autenticación AWS Signature V4** activable por configuración.
- Persistencia en disco, resistente a reinicios del contenedor (volumen).

### ❌ Fuera del MVP (fases futuras)

- Multipart upload (archivos grandes en partes)
- Versionado de objetos
- Políticas IAM / ACLs granulares
- Replicación / clustering distribuido
- Cifrado en reposo ("Sello Caleño")
- Lifecycle, tags, CORS config, presigned URLs

---

## 4. Arquitectura

```
server/
├── main.go                  # arranque, config por env, router, graceful shutdown
├── go.mod
├── go.sum
└── internal/
    ├── s3api/
    │   ├── router.go        # mapea método+path -> handler
    │   ├── bucket.go        # ListBuckets, CreateBucket, DeleteBucket, HeadBucket
    │   ├── object.go        # Put/Get/Head/Delete Object, ListObjectsV2
    │   ├── xml.go           # structs de respuesta/errores XML del protocolo S3
    │   └── errors.go        # catálogo de errores S3 (código, status HTTP, mensaje)
    ├── storage/
    │   ├── storage.go       # interface Backend (abstracción)
    │   └── fs.go            # implementación en filesystem
    └── auth/
        └── sigv4.go         # verificación de AWS Signature V4
```

### Flujo de una petición

```
Cliente (aws cli / boto3)
   │  HTTP + firma SigV4
   ▼
main.go  ──►  auth.VerifySigV4()  ──►  s3api.Router  ──►  handler  ──►  storage.Backend (disco)
   │                                                                         │
   └────────────────────  respuesta XML + headers S3  ◄──────────────────────┘
```

### Layout en disco (backend filesystem)

```
/data/                          # volumen montado (CHUSPA_DATA_DIR)
├── mi-chuspa/                  # un directorio por bucket
│   ├── .chuspa-meta.json       # metadata del bucket (fecha de creación, región)
│   ├── test/
│   │   └── file.txt            # el objeto "test/file.txt"
│   └── music/
│       └── cancion.mp3
└── otra-chuspa/
```

Las "claves" (keys) con `/` se mapean a subdirectorios. La metadata por objeto (ETag, content-type) se guarda junto al archivo o en un sidecar `.meta`.

---

## 5. Configuración (variables de entorno)

| Variable | Default | Descripción |
|---|---|---|
| `CHUSPA_ADDR` | `:9000` | Dirección y puerto de escucha |
| `CHUSPA_DATA_DIR` | `/data` | Carpeta raíz de almacenamiento |
| `CHUSPA_ROOT_USER` | *(vacío)* | Access Key. Si está vacío → **sin auth** (modo pruebas local) |
| `CHUSPA_ROOT_PASSWORD` | *(vacío)* | Secret Key. Requerido si hay `ROOT_USER` |
| `CHUSPA_REGION` | `cali-san-antonio-1` | Región reportada por el servidor |

**Auth opcional:** si `CHUSPA_ROOT_USER` y `CHUSPA_ROOT_PASSWORD` están definidos, el servidor **exige firma SigV4 válida**; si no, corre abierto (útil para desarrollo).

---

## 6. Dockerfile (multi-stage)

```dockerfile
# ---- Stage 1: build ----
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Binario estático, sin CGO, para que corra en una imagen mínima
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /chuspa ./

# ---- Stage 2: runtime ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates && \
    adduser -D -u 10001 chuspa
COPY --from=build /chuspa /usr/local/bin/chuspa
USER chuspa
EXPOSE 9000
VOLUME ["/data"]
ENV CHUSPA_ADDR=":9000" \
    CHUSPA_DATA_DIR="/data" \
    CHUSPA_REGION="cali-san-antonio-1"
ENTRYPOINT ["chuspa"]
```

Resultado: imagen de ~10–15 MB. (Se puede bajar a `scratch` si se añade manualmente el bundle de CA certs.)

### docker-compose.yml

```yaml
services:
  openchuspa-engine:
    image: openchuspa/chuspa:latest
    build: ./server
    container_name: motor_caleno_s3
    ports:
      - "9000:9000"
    environment:
      CHUSPA_ROOT_USER: "clave-del-parchao"
      CHUSPA_ROOT_PASSWORD: "la-secreta-de-la-cuadra"
      CHUSPA_REGION: "cali-san-antonio-1"
    volumes:
      - ./chuspa_data:/data
    restart: unless-stopped
```

### Comando de una línea (el de la landing)

```bash
docker run -p 9000:9000 -v $(pwd)/chuspa_data:/data openchuspa/chuspa
```

---

## 7. Cómo se probará (criterios de aceptación)

El MVP se considera listo cuando **todos** estos comandos funcionan contra el contenedor:

```bash
export AWS_ACCESS_KEY_ID=clave-del-parchao
export AWS_SECRET_ACCESS_KEY=la-secreta-de-la-cuadra
export ENDPOINT=http://localhost:9000

# Crear una chuspa
aws s3 mb s3://feria-de-cali --endpoint-url $ENDPOINT

# Listar chuspas
aws s3 ls --endpoint-url $ENDPOINT

# Subir una chuchería
echo "oíste mira" > salsa.txt
aws s3 cp salsa.txt s3://feria-de-cali/test/salsa.txt --endpoint-url $ENDPOINT

# Listar contenido
aws s3 ls s3://feria-de-cali/test/ --endpoint-url $ENDPOINT

# Descargar
aws s3 cp s3://feria-de-cali/test/salsa.txt bajado.txt --endpoint-url $ENDPOINT

# Borrar
aws s3 rm s3://feria-de-cali/test/salsa.txt --endpoint-url $ENDPOINT
```

Pruebas automatizadas en Go:
- **Unitarias**: backend de storage (crear/leer/listar/borrar), cálculo de ETag, parser de rutas.
- **Integración**: `httptest.Server` + `aws-sdk-go-v2` ejecutando el flujo completo contra el servidor en memoria.
- **SigV4**: firma válida → 200; firma inválida/ausente (con auth activa) → 403 `SignatureDoesNotMatch` / `AccessDenied`.

---

## 8. Plan de implementación por pasos

1. **Esqueleto**: `go mod init`, `main.go` que levanta un `net/http` y responde a `GET /` con el XML de ListBuckets vacío.
2. **Storage filesystem**: interface `Backend` + implementación en disco (buckets como carpetas).
3. **Buckets**: Create / Delete / Head / List buckets con sus respuestas y errores XML.
4. **Objetos**: Put / Get / Head / Delete + ListObjectsV2, con ETag y content-type.
5. **Errores S3**: catálogo XML completo y mapeo a status HTTP.
6. **Auth SigV4**: middleware de verificación activable por env.
7. **Dockerfile + compose**: build multi-stage y prueba del `docker run`.
8. **Tests**: unitarios + integración con `aws-sdk-go-v2`.
9. **Alinear la landing**: que los snippets y el comando Docker apunten a este motor real.

---

## 9. Riesgos y notas

- **Compatibilidad S3 es amplia**: el MVP cubre el núcleo; algunos clientes usan features avanzadas (multipart para archivos >5GB, por ejemplo) que fallarán hasta implementarlas. Documentar qué está soportado.
- **SigV4 es detallista**: el cálculo de la firma canónica debe ser exacto (orden de headers, encoding de la URI, payload hash). Es la parte más propensa a bugs; de ahí los tests dedicados.
- **Seguridad**: el MVP no es para exponer a internet tal cual. Sin HTTPS (va detrás de un reverse proxy / el adaptador que lo termine) y con un solo par de credenciales root.
- **Persistencia**: depende del volumen montado; sin volumen, los datos se pierden al recrear el contenedor.

---

## 10. Siguientes pasos (post-MVP)

- Multipart upload
- Presigned URLs (`ve subi` sin exponer credenciales)
- Versionado y lifecycle
- Múltiples usuarios y políticas (el "Reglamento del Parche")
- Cifrado en reposo ("Sello Caleño")
- Clustering / erasure coding para alta disponibilidad
```
