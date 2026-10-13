# OpenChuspa Engine — Diseño v2 del Motor S3 Caleño

> Motor de almacenamiento de objetos **compatible con AWS S3**, escrito desde cero, **abierto por diseño y seguro por defecto**.
> Este documento reemplaza a [`MVP-ENGINE.md`](./MVP-ENGINE.md) (v1), que se conserva como referencia histórica.

| | |
|---|---|
| **Versión** | 2.0 |
| **Fecha** | 2026-10-13 |
| **Estado** | Propuesta, pendiente de revisión por los mantenedores |
| **Licencia propuesta** | Apache-2.0 |

---

## 0. Qué cambia respecto a la v1

| # | Tema | v1 | v2 | Por qué |
|---|---|---|---|---|
| 1 | Subidas `aws-chunked` | No se menciona | Obligatorio desde el primer PutObject | Los SDK y el AWS CLI actuales calculan un checksum CRC por defecto y lo envían como trailer. Sin decodificar ese formato, el objeto se guarda corrupto o la subida falla. |
| 2 | Multipart | Fuera del MVP | Dentro del MVP | `aws s3 cp` cambia a multipart a partir de **8 MB** (`multipart_threshold`), no de 5 GB. Sin multipart, el criterio de aceptación solo se cumple con archivos pequeños. |
| 3 | Descargas por rango | No se menciona | `Range` obligatorio en GetObject | Las descargas grandes del CLI se hacen en partes paralelas con `Range`. |
| 4 | Layout en disco | La key es la ruta del archivo | Blobs opacos + índice embebido | La key como ruta permite path traversal, hace chocar `a` con `a/b`, no admite keys de 1024 bytes y vuelve lento el listado ordenado. |
| 5 | Sin credenciales | El servidor arranca abierto | El servidor **no arranca** | Un default abierto termina expuesto en internet. El modo anónimo existe, pero hay que pedirlo y solo escucha en loopback. |
| 6 | SigV4 | Implementación propia | Se recalcula con el firmador oficial de `aws-sdk-go-v2` | Es la pieza más propensa a errores; se reutiliza código probado en vez de reescribirlo. |
| 7 | Pruebas | Suite propia | AWS CLI real + SDK reales + `ceph/s3-tests` en CI | La compatibilidad se mide contra los clientes de verdad, no contra nuestra lectura de la especificación. |
| 8 | Imagen | `alpine`, tag `latest` | Distroless sin root, tags inmutables, firmada | Menos superficie de ataque y trazabilidad de cada imagen hasta su commit. |
| 9 | Proyecto | Solo código | Licencia, gobierno, política de seguridad y cadena de suministro | Sin eso es código publicado, no un proyecto open source. |

---

## 1. Principios no negociables

**Abierto**

1. **Todo en público.** Código, issues, roadmap, decisiones de diseño y resultados de compatibilidad viven en el repositorio. Lo que no está escrito ahí, no existe.
2. **Licencia clara desde el primer commit.** Sin `LICENSE` nadie puede usar ni contribuir legalmente.
3. **Sala limpia.** No se copia ni se traduce código de proyectos con licencia incompatible (ver §3.2). Se implementa a partir de la documentación pública del protocolo y de proyectos con licencia permisiva.
4. **Honestidad sobre lo que funciona.** La landing y el README solo prometen lo que pasa en CI. Lo demás va al roadmap, marcado como pendiente.
5. **Cualquiera puede reproducirlo.** Un `git clone` y un comando bastan para compilar, probar y obtener el mismo binario que se publica.

**Seguro**

6. **Seguro por defecto.** La configuración que sale de la caja es la segura; lo inseguro requiere una decisión explícita y deja rastro en el log.
7. **Falla cerrado.** Ante una firma dudosa, una key rara o una configuración incompleta, se rechaza.
8. **Mínima superficie.** Pocas dependencias, todas auditadas y fijadas; binario estático; contenedor sin shell, sin root y con sistema de archivos de solo lectura.
9. **Nada entra sin validar.** Toda entrada del cliente (ruta, headers, XML, cuerpo) tiene límites de tamaño y formato.
10. **Cadena de suministro verificable.** Cada release se puede rastrear hasta su commit y verificar criptográficamente.

---

## 2. Objetivo y no-objetivos

**Objetivo.** Un servidor de un solo nodo que hable el protocolo S3 lo suficiente para que estos clientes funcionen sin parches ni flags especiales:

- AWS CLI v2 (`aws s3 ...` y `aws s3api ...`)
- `boto3` (Python)
- `@aws-sdk/client-s3` (Node.js)
- `aws-sdk-go-v2` (Go)
- `curl` contra la API REST

**No-objetivos del MVP.**

- Implementar "todo S3". Son más de cien operaciones; la meta es un subconjunto declarado y medido (§10).
- Alta disponibilidad o clúster. El nodo único es el producto; la replicación es una fase posterior (§12).
- Servir directamente a internet sin TLS delante.

---

## 3. Stack y dependencias

### 3.1 Decisiones

| Decisión | Elección | Motivo |
|---|---|---|
| Lenguaje | **Go**, última versión estable (hoy 1.27.x) | Binario estático, concurrencia nativa, fuzzing y detector de carreras incluidos. |
| HTTP | `net/http` (stdlib) | El enrutado por método y patrón de la stdlib es suficiente. |
| XML | `encoding/xml` (stdlib) | Es el formato del protocolo. |
| Verificación SigV4 | `github.com/aws/aws-sdk-go-v2/aws/signer/v4` | Firmador oficial; se usa para recalcular la firma esperada. |
| Índice de metadata | `go.etcd.io/bbolt` | Clave-valor embebido, Go puro, transaccional y ordenado por clave: el listado paginado sale gratis. |
| Blobs | Filesystem local | Simple y montable como volumen. |
| Checksums | `hash/crc32`, `hash/crc64`, `crypto/*` (stdlib) | CRC32, CRC32C, CRC64NVME, SHA-1 y SHA-256 sin dependencias. |
| Logs y métricas | `log/slog` (stdlib); Prometheus en la fase 2 | Logs estructurados sin dependencias. |

Regla: una dependencia nueva solo entra con un issue que explique por qué la stdlib no basta, cuál es su licencia y quién la mantiene.

### 3.2 Política de licencias

Licencias verificadas el 2026-10-13 con la API de GitHub.

| Proyecto | Licencia | Uso permitido |
|---|---|---|
| `aws/aws-sdk-go-v2` | Apache-2.0 | Dependencia |
| `etcd-io/bbolt` | MIT | Dependencia |
| `ceph/s3-tests` | MIT | Suite de conformidad en CI |
| `seaweedfs/seaweedfs` | Apache-2.0 | Referencia de lectura |
| `versity/versitygw` | Apache-2.0 | Referencia de lectura |
| `johannesboyne/gofakes3` | MIT | Referencia de lectura |
| `minio/minio` | **AGPL-3.0** | **Prohibido copiar o adaptar código.** Solo se puede usar como caja negra para comparar comportamiento. |

- Permitidas en dependencias: Apache-2.0, MIT, BSD-2/3-Clause, ISC.
- Prohibidas en dependencias y como fuente de código: AGPL, GPL, SSPL, BUSL y cualquier licencia "source available".
- El CI falla si aparece una dependencia fuera de la lista permitida.

---

## 4. Alcance

### 4.1 Fase 1 — MVP

| Operación S3 | Método y ruta | Jerga caleña |
|---|---|---|
| ListBuckets | `GET /` | `ve mira chuspa://` |
| CreateBucket | `PUT /{bucket}` | `ve crea` |
| HeadBucket | `HEAD /{bucket}` | — |
| GetBucketLocation | `GET /{bucket}?location` | — |
| DeleteBucket | `DELETE /{bucket}` | `ve borra chuspa://nombre` |
| ListObjectsV2 | `GET /{bucket}?list-type=2` | `ve mira chuspa://nombre/` |
| PutObject | `PUT /{bucket}/{key}` | `ve subi` |
| GetObject (con `Range`) | `GET /{bucket}/{key}` | `ve baja` |
| HeadObject | `HEAD /{bucket}/{key}` | — |
| DeleteObject | `DELETE /{bucket}/{key}` | `ve borra` |
| CreateMultipartUpload | `POST /{bucket}/{key}?uploads` | `ve subi` (archivo grande) |
| UploadPart | `PUT /{bucket}/{key}?partNumber=N&uploadId=X` | — |
| CompleteMultipartUpload | `POST /{bucket}/{key}?uploadId=X` | — |
| AbortMultipartUpload | `DELETE /{bucket}/{key}?uploadId=X` | — |
| ListParts | `GET /{bucket}/{key}?uploadId=X` | — |

### 4.2 Fase 1.1 — inmediatamente después

| Operación | Para qué |
|---|---|
| DeleteObjects | Borrado masivo desde los SDK |
| CopyObject | `aws s3 cp` y `mv` entre chuspas |
| ListObjects (v1) | Herramientas antiguas |
| ListMultipartUploads | Limpieza de subidas incompletas |

### 4.3 Fuera del MVP

Versionado, políticas y varios usuarios, URLs prefirmadas, CORS, tags, lifecycle, object lock, cifrado en reposo, replicación. Todo está ubicado en el roadmap (§12).

---

## 5. Requisitos de protocolo

> Los detalles de esta sección se deben contrastar con la referencia de la API de S3 al implementar cada operación. En caso de duda, el árbitro es el comportamiento del cliente real en CI.

### 5.1 Cuerpo de las subidas

El servidor debe aceptar las tres formas en que un cliente envía un cuerpo, según el header `x-amz-content-sha256`:

| Valor | Significado | Acción del servidor |
|---|---|---|
| Hash SHA-256 en hexadecimal | Cuerpo normal firmado | Calcular el SHA-256 mientras se escribe y rechazar si no coincide |
| `UNSIGNED-PAYLOAD` | Cuerpo normal sin firmar | Aceptar; la integridad la da el checksum si viene |
| `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` | Cuerpo en chunks, cada uno firmado | Decodificar y verificar la firma encadenada de cada chunk |
| `STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER` | Igual, con trailers firmados | Decodificar, verificar chunks y trailer |
| `STREAMING-UNSIGNED-PAYLOAD-TRAILER` | Chunks sin firma, con trailers | Decodificar y validar el checksum del trailer |

Con `Content-Encoding: aws-chunked` el tamaño real del objeto viene en `x-amz-decoded-content-length`, y `x-amz-trailer` anuncia qué headers llegan al final. El decodificador de chunks es código propio: es pequeño, está bien especificado y lleva fuzzing obligatorio (§9.2).

### 5.2 Checksums

Los clientes actuales calculan un checksum en cada subida y validan el de cada descarga (`request_checksum_calculation` y `response_checksum_validation` valen `WHEN_SUPPORTED` por defecto). Algoritmo por defecto de cada cliente, según la documentación de AWS consultada el 2026-10-13:

| Cliente | Algoritmo por defecto |
|---|---|
| AWS CLI v2 | CRC64NVME |
| boto3 | CRC32 |
| SDK for JavaScript 3.x | CRC32 |
| SDK for Go V2 | CRC32 |

El servidor debe:

- Aceptar `x-amz-checksum-{crc32,crc32c,crc64nvme,sha1,sha256}` como header o como trailer.
- Recalcular el checksum mientras escribe y rechazar con `BadDigest` si no coincide.
- Guardarlo en la metadata y devolverlo en GetObject y HeadObject cuando el cliente lo pide.

### 5.3 Respuestas

- XML con el formato exacto del protocolo, incluidos los errores (`NoSuchBucket`, `NoSuchKey`, `BucketAlreadyOwnedByYou`, `BucketNotEmpty`, `InvalidBucketName`, `SignatureDoesNotMatch`, `AccessDenied`, `RequestTimeTooSkewed`, `EntityTooLarge`, `BadDigest`, `InvalidPart`, `NoSuchUpload`).
- Headers: `ETag`, `Content-Type`, `Content-Length`, `Last-Modified`, `Accept-Ranges`, `x-amz-request-id`.
- `ETag`: MD5 del contenido en subidas simples; en multipart, MD5 de los MD5 de las partes seguido de `-N`.
- Listado en orden lexicográfico por key, con `prefix`, `delimiter`, `max-keys` y token de continuación.
- Direccionamiento por ruta (`/{bucket}/{key}`) en el MVP; el estilo de host virtual queda para la fase 2.

### 5.4 Validación de nombres

- **Bucket:** 3 a 63 caracteres, minúsculas, dígitos y guiones; sin puntos en el MVP.
- **Key:** hasta 1024 bytes en UTF-8 válido. Se aceptan `/`, `..` y cualquier otro carácter porque la key **nunca** se usa como ruta de archivo (§6.2).

---

## 6. Arquitectura

### 6.1 Paquetes

```
cmd/chuspa/              # arranque, configuración, subcomandos (serve, healthcheck, version)
internal/
  config/                # lectura y validación de variables de entorno
  s3api/                 # router, handlers, XML de respuestas y catálogo de errores
  auth/                  # verificación SigV4 (header) y firmas de chunks
  chunked/               # decodificador aws-chunked
  checksum/              # algoritmos de checksum y validación
  storage/               # interfaz Backend
  storage/fsbolt/        # implementación: blobs en disco + índice bbolt
  httpx/                 # límites, timeouts, request-id, logging y recuperación de pánicos
test/
  compat/                # pruebas con AWS CLI y SDK reales
  s3tests/               # configuración y lista de pruebas de ceph/s3-tests
docs/
  adr/                   # registros de decisiones de arquitectura
  compatibilidad.md      # matriz generada por el CI
```

### 6.2 Almacenamiento

```
/data/
  meta.db                # índice bbolt: buckets, objetos, subidas multipart
  blobs/
    ab/cd/abcd…ef        # contenido del objeto, nombre opaco generado por el servidor
  staging/               # escrituras en curso y partes de multipart
```

- **La key vive solo en el índice.** El archivo en disco tiene un identificador aleatorio generado por el servidor. Así desaparecen el path traversal, los choques entre `a` y `a/b`, el límite de 255 bytes por nombre y las diferencias entre sistemas de archivos.
- **Escritura atómica.** Se escribe en `staging/`, se hace `fsync`, se mueve con `rename` a `blobs/` y solo entonces se confirma la transacción en el índice. Un corte de energía deja el objeto anterior o el nuevo, nunca uno a medias.
- **Recolección de huérfanos.** Al arrancar, y luego de forma periódica, se borran los blobs sin entrada en el índice y las subidas multipart abandonadas.
- **Interfaz `Backend`.** Los handlers no conocen el disco, así que otro backend (otro motor, otro disco, replicación) se añade sin tocar la API.

Costo asumido: los datos ya no se inspeccionan con `ls`. Se compensa con un subcomando `chuspa inspect` que lista y exporta desde el índice.

### 6.3 Flujo de una petición

```
Cliente
  │  HTTP + firma SigV4
  ▼
httpx (límites, timeouts, request-id)
  ▼
auth.Verify ──► falla: 403 en XML, sin tocar el almacenamiento
  ▼
s3api.Router ──► handler
  ▼
chunked.Decode ──► checksum.Verify ──► storage.Backend
  ▼
respuesta XML + headers S3 + línea de log con request-id
```

---

## 7. Seguridad

### 7.1 Modelo de amenazas

| Amenaza | Mitigación |
|---|---|
| Servidor expuesto sin autenticación | No arranca sin credenciales (§7.2) |
| Credenciales de ejemplo en producción | Se rechazan las credenciales de la documentación fuera de loopback |
| Path traversal mediante la key | La key nunca es una ruta (§6.2) |
| Firma falsificada o reutilizada | SigV4 completo, comparación en tiempo constante, ventana de reloj de 15 minutos |
| Cuerpo manipulado en tránsito | SHA-256 del cuerpo, firmas de chunk y checksums |
| Agotamiento de recursos | Límites de tamaño, de headers, de conexiones y timeouts (§7.4) |
| Corrupción por caída a mitad de escritura | Escritura atómica con `fsync` y `rename` |
| Fuga de secretos en logs | Nunca se registran `Authorization`, firmas ni llaves; hay una prueba que lo verifica |
| Escape del contenedor | Sin root, sin shell, solo lectura, sin capabilities (§8) |
| Dependencia o imagen comprometida | Fijado por hash, `govulncheck`, firma, SBOM y provenance (§9.3) |

Fuera del modelo en el MVP: un atacante con acceso de escritura al volumen de datos, y el cifrado en reposo (fase 3).

### 7.2 Arranque seguro

| Situación | Comportamiento |
|---|---|
| Sin `CHUSPA_ROOT_USER` o sin `CHUSPA_ROOT_PASSWORD` | No arranca; el error explica cómo configurarlas |
| Secreto de menos de 32 caracteres | No arranca |
| Credenciales iguales a las de la documentación y dirección distinta de loopback | No arranca |
| `CHUSPA_ALLOW_ANONYMOUS=true` | Arranca sin auth **solo** si escucha en loopback; avisa en el log en cada inicio |
| Sin TLS propio y dirección distinta de loopback | Arranca y avisa en el log de que necesita un proxy con TLS delante |

### 7.3 Verificación de SigV4

1. Analizar `Authorization` con un parser estricto: algoritmo, credencial, headers firmados y firma.
2. Rechazar si la fecha de `x-amz-date` se aleja más de 15 minutos del reloj del servidor (`RequestTimeTooSkewed`).
3. Exigir que `host`, `x-amz-date` y `x-amz-content-sha256` estén entre los headers firmados.
4. Reconstruir la petición solo con los headers firmados y recalcular la firma con el firmador oficial.
5. Comparar con `crypto/subtle.ConstantTimeCompare`.
6. Verificar el cuerpo según §5.1.

Cualquier fallo responde 403 con el mismo tiempo y el mismo detalle, sin revelar si la llave existe.

### 7.4 Endurecimiento HTTP

Valores iniciales propuestos, todos configurables:

| Límite | Valor |
|---|---|
| Lectura de headers | 10 s |
| Inactividad de la conexión | 120 s |
| Tamaño máximo de headers | 64 KiB |
| Tamaño máximo de un PutObject o de una parte | 5 GiB |
| Tamaño máximo de un cuerpo XML | 1 MiB |
| Partes por subida multipart | 10 000 |
| Conexiones simultáneas | Configurable; por defecto 1 000 |

Además: recuperación de pánicos que devuelve 500 en XML sin tumbar el proceso, apagado ordenado que espera las peticiones en curso, y el decodificador XML de la stdlib, que no resuelve entidades externas.

### 7.5 Configuración

| Variable | Default | Descripción |
|---|---|---|
| `CHUSPA_ADDR` | `127.0.0.1:9000` | Dirección de escucha. Loopback por defecto. |
| `CHUSPA_DATA_DIR` | `/data` | Carpeta raíz de datos |
| `CHUSPA_ROOT_USER` | *(obligatoria)* | Access key |
| `CHUSPA_ROOT_PASSWORD` | *(obligatoria)* | Secret key, mínimo 32 caracteres |
| `CHUSPA_ROOT_PASSWORD_FILE` | *(vacío)* | Ruta a un archivo con el secreto; pensado para Docker y Kubernetes secrets |
| `CHUSPA_REGION` | `cali-san-antonio-1` | Región que reporta el servidor y que valida en la firma |
| `CHUSPA_ALLOW_ANONYMOUS` | `false` | Modo sin auth, solo en loopback |
| `CHUSPA_TLS_CERT_FILE`, `CHUSPA_TLS_KEY_FILE` | *(vacío)* | TLS nativo (fase 2) |
| `CHUSPA_LOG_FORMAT` | `json` | `json` o `text` |

La imagen de Docker define `CHUSPA_ADDR=:9000`, porque dentro del contenedor el aislamiento lo da la publicación del puerto.

---

## 8. Contenedor

### 8.1 Dockerfile

Los `<digest>` se resuelven y se fijan en el repositorio; un bot los actualiza por PR.

```dockerfile
# ---- build ----
FROM golang:1.27-alpine@sha256:<digest> AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -buildid= -X main.version=${VERSION}" \
      -o /out/chuspa ./cmd/chuspa
RUN mkdir -p /out/data

# ---- runtime ----
FROM gcr.io/distroless/static-debian12:nonroot@sha256:<digest>
COPY --from=build /out/chuspa /usr/local/bin/chuspa
COPY --from=build --chown=nonroot:nonroot /out/data /data
USER nonroot:nonroot
EXPOSE 9000
VOLUME ["/data"]
ENV CHUSPA_ADDR=":9000" CHUSPA_DATA_DIR="/data"
HEALTHCHECK --interval=30s --timeout=3s CMD ["/usr/local/bin/chuspa", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/chuspa"]
CMD ["serve"]
```

- La imagen final no tiene shell, gestor de paquetes ni usuario root.
- `-trimpath` y `-buildid=` hacen el binario reproducible.
- El healthcheck es un subcomando del propio binario porque la imagen no trae `curl`.
- El tag exacto de la imagen base se confirma al fijar el digest.

### 8.2 docker-compose.yml

```yaml
services:
  chuspa:
    image: ghcr.io/openchuspa/chuspa:0.1.0
    ports:
      - "127.0.0.1:9000:9000"
    environment:
      CHUSPA_ROOT_USER: "${CHUSPA_ROOT_USER:?definila en .env}"
      CHUSPA_ROOT_PASSWORD_FILE: /run/secrets/chuspa_root_password
      CHUSPA_REGION: "cali-san-antonio-1"
    secrets:
      - chuspa_root_password
    volumes:
      - chuspa_data:/data
    read_only: true
    cap_drop: ["ALL"]
    security_opt: ["no-new-privileges:true"]
    restart: unless-stopped

secrets:
  chuspa_root_password:
    file: ./secrets/chuspa_root_password

volumes:
  chuspa_data:
```

- Nunca `latest`: cada despliegue referencia una versión exacta.
- El secreto no va en el archivo ni en una variable visible con `docker inspect`.
- El puerto se publica solo en loopback; exponerlo es una decisión de quien despliega.

---

## 9. Calidad y cadena de suministro

### 9.1 CI en cada pull request

| Trabajo | Qué garantiza |
|---|---|
| `go vet`, `staticcheck`, `gofmt` | Código idiomático y sin errores obvios |
| `go test -race ./...` | Sin carreras de datos |
| `govulncheck` | Sin vulnerabilidades conocidas alcanzables |
| Verificación de licencias | Ninguna dependencia fuera de la lista permitida |
| Compatibilidad | AWS CLI, boto3, SDK de JavaScript y SDK de Go reales contra el binario |
| `ceph/s3-tests` | Conformidad; el porcentaje aprobado no puede bajar |
| CodeQL | Análisis estático de seguridad |
| Build de la imagen | El Dockerfile siempre compila |

### 9.2 Fuzzing

Con el fuzzing nativo de Go, sobre todo lo que analiza entrada no confiable: el header `Authorization`, el decodificador `aws-chunked`, el header `Range`, los parámetros de listado y el XML de CompleteMultipartUpload. Corre unos minutos en cada PR y de forma prolongada cada noche; todo fallo encontrado queda como caso de prueba.

### 9.3 Releases

- Versionado semántico y `CHANGELOG.md`. Hasta la 1.0, las versiones `0.x` pueden romper compatibilidad y lo dicen.
- Imagen multi-arquitectura (`amd64`, `arm64`) en `ghcr.io/openchuspa/chuspa`.
- Firma con cosign sin llaves (identidad OIDC de GitHub Actions), SBOM y provenance adjuntos a la imagen.
- Binarios con sus sumas SHA-256 firmadas.
- El README documenta el comando para verificar la firma antes de ejecutar la imagen.

### 9.4 Higiene del repositorio

- `main` protegida: PR obligatorio, una revisión, CI en verde, sin force-push.
- GitHub Actions fijadas por SHA de commit y con `permissions` mínimos por trabajo.
- Dependabot para módulos de Go, Actions e imágenes base.
- Secret scanning con push protection.
- OpenSSF Scorecard publicado en el README.

---

## 10. Proyecto open source

### 10.1 Archivos del repositorio

| Archivo | Contenido |
|---|---|
| `LICENSE` | Apache-2.0 |
| `README.md` | Qué es, inicio rápido, estado real, matriz de compatibilidad |
| `CONTRIBUTING.md` | Cómo compilar, probar y enviar un PR |
| `CODE_OF_CONDUCT.md` | Contributor Covenant |
| `SECURITY.md` | Reporte privado de vulnerabilidades y plazos de respuesta |
| `GOVERNANCE.md` | Quién decide y cómo se llega a ser mantenedor |
| `CHANGELOG.md` | Cambios por versión |
| `docs/adr/` | Una decisión de arquitectura por archivo |
| `.github/` | Plantillas de issue y PR, workflows, `CODEOWNERS`, `dependabot.yml` |

### 10.2 Reglas de contribución

- **DCO en vez de CLA.** Cada commit lleva `Signed-off-by`; no se pide ceder derechos a nadie.
- **Commits convencionales** para generar el changelog.
- **Toda funcionalidad entra con su prueba de compatibilidad** y actualiza la matriz.
- **Los cambios de diseño se discuten antes de programarse**, en un issue y un ADR.
- Issues etiquetados `good first issue` desde el primer día, para que entrar sea fácil.

### 10.3 Política de seguridad

- Las vulnerabilidades se reportan por el canal privado de GitHub, nunca en un issue público.
- Acuse de recibo en 72 horas; divulgación coordinada en un máximo de 90 días.
- Cada corrección de seguridad se publica con un aviso y una versión nueva.

### 10.4 Coherencia con la landing

- `openchuspa.xyz` muestra solo lo que pasa en CI, con enlace a la matriz de compatibilidad.
- La clusterización y el cifrado "Sello Caleño" se mueven al roadmap hasta que existan.
- Los snippets usan la imagen real con versión fija y credenciales que el usuario define.

---

## 11. Criterios de aceptación del MVP

El MVP está listo cuando todo lo siguiente pasa en CI contra el contenedor.

**Funcionales**

```bash
export AWS_ACCESS_KEY_ID=<access key>
export AWS_SECRET_ACCESS_KEY=<secret key>
export AWS_DEFAULT_REGION=cali-san-antonio-1
export ENDPOINT=http://localhost:9000

aws s3 mb s3://feria-de-cali --endpoint-url $ENDPOINT
aws s3 ls --endpoint-url $ENDPOINT

echo "oíste mira" > salsa.txt
aws s3 cp salsa.txt s3://feria-de-cali/test/salsa.txt --endpoint-url $ENDPOINT
aws s3 ls s3://feria-de-cali/test/ --endpoint-url $ENDPOINT
aws s3 cp s3://feria-de-cali/test/salsa.txt bajado.txt --endpoint-url $ENDPOINT
cmp salsa.txt bajado.txt

# Archivo grande: obliga a multipart en la subida y a rangos en la descarga
head -c 104857600 /dev/urandom > grande.bin
aws s3 cp grande.bin s3://feria-de-cali/grande.bin --endpoint-url $ENDPOINT
aws s3 cp s3://feria-de-cali/grande.bin grande.bajado --endpoint-url $ENDPOINT
cmp grande.bin grande.bajado

aws s3 rm s3://feria-de-cali --recursive --endpoint-url $ENDPOINT
aws s3 rb s3://feria-de-cali --endpoint-url $ENDPOINT
```

- El mismo flujo pasa con boto3, `@aws-sdk/client-s3` y `aws-sdk-go-v2` en su configuración por defecto.
- Los datos sobreviven al reinicio del contenedor.
- Un `kill -9` en mitad de una subida no deja objetos corruptos ni visibles a medias.

**De seguridad**

| Caso | Resultado esperado |
|---|---|
| Arranque sin credenciales | El proceso termina con error |
| Petición sin firma | 403 `AccessDenied` |
| Firma con secreto incorrecto | 403 `SignatureDoesNotMatch` |
| Fecha fuera de la ventana de 15 minutos | 403 `RequestTimeTooSkewed` |
| Cuerpo alterado después de firmar | 400, sin objeto guardado |
| Checksum incorrecto | 400 `BadDigest`, sin objeto guardado |
| Key `../../etc/passwd` | Se guarda como un objeto normal con ese nombre; nada sale de `/data` |
| Headers o cuerpo por encima de los límites | Rechazo sin agotar memoria |
| Búsqueda de secretos en los logs de toda la suite | Cero coincidencias |

**De conformidad**

- Se publica el porcentaje de `ceph/s3-tests` aprobado para las operaciones de la fase 1.
- La meta de salida del MVP la fijan los mantenedores tras la primera ejecución completa.

---

## 12. Plan

### 12.1 MVP por rebanadas verticales

Cada rebanada termina cuando el cliente real pasa en CI, no cuando el código compila.

| # | Rebanada | Criterio de salida |
|---|---|---|
| 0 | Base del proyecto: licencia, archivos de §10.1, CI, protección de rama | Un PR vacío pasa el CI completo |
| 1 | Esqueleto, configuración segura, `healthcheck`, almacenamiento con índice | Las pruebas de arranque de §11 pasan |
| 2 | Buckets | `aws s3 mb`, `ls` y `rb` funcionan en modo anónimo local |
| 3 | PutObject, GetObject, HeadObject y DeleteObject, con `aws-chunked` y checksums | `aws s3 cp` de ida y vuelta con un archivo pequeño |
| 4 | ListObjectsV2 | `aws s3 ls` con prefijo y paginación de más de 1 000 objetos |
| 5 | SigV4 | Los cuatro clientes pasan con auth; las pruebas negativas pasan |
| 6 | Multipart y `Range` | Archivo de 100 MB de ida y vuelta, idéntico |
| 7 | Imagen, firma y primera versión `0.1.0` | La imagen se verifica con cosign y pasa toda la suite |
| 8 | Alinear la landing | Los snippets de `openchuspa.xyz` funcionan copiados tal cual |

### 12.2 Roadmap posterior

| Fase | Funcionalidad | Seguridad |
|---|---|---|
| 1.1 | DeleteObjects, CopyObject, ListObjects v1, ListMultipartUploads | — |
| 2 | URLs prefirmadas, CORS, tags, host virtual, métricas Prometheus | TLS nativo, varios usuarios con llaves propias, políticas (el "Reglamento del Parche"), log de auditoría, límite de peticiones |
| 3 | Versionado, lifecycle, object lock | Cifrado en reposo ("Sello Caleño"): AES-256-GCM con una llave por objeto, envuelta por una llave maestra en un KMS o en Vault |
| 4 | Replicación y erasure coding | Auditoría de seguridad externa antes de declararlo apto para producción |

Dos advertencias sobre el roadmap:

- SigV4 obliga al servidor a conocer el secreto en claro, así que no se puede guardar como hash. Desde la fase 2 los secretos de usuario se guardan cifrados con la llave maestra.
- La fase 4 es donde este tipo de proyectos tarda años. No lleva fecha, y el nodo único se mantiene como producto de primera clase.

---

## 13. Riesgos

| Riesgo | Tratamiento |
|---|---|
| Los clientes cambian su comportamiento por defecto, como pasó con los checksums | El CI usa las últimas versiones de los clientes y corre cada semana aunque no haya commits |
| La compatibilidad S3 es enorme | Subconjunto declarado y medido; lo no soportado responde `NotImplemented`, nunca un éxito falso |
| Errores sutiles en firmas y chunks | Firmador oficial, fuzzing y pruebas negativas |
| El índice se corrompe | Transacciones de bbolt, copia periódica del índice y un subcomando para reconstruirlo |
| Alguien lo expone a internet sin TLS | Loopback por defecto, avisos en el log y documentación de despliegue con proxy |
| Pocos mantenedores | Documentación para contribuir, issues de entrada y reglas de gobierno desde el inicio |

---

## 14. Decisiones pendientes de los mantenedores

1. Confirmar **Apache-2.0** y añadir `LICENSE` a este repositorio y al de la landing, que hoy es público y no tiene licencia.
2. Decidir cuándo se hace público este repositorio. La recomendación es hacerlo al cerrar la rebanada 0.
3. Reservar el nombre `openchuspa` en Docker Hub. El 2026-10-13 seguía libre, y la landing invita a ejecutar `openchuspa/chuspa`: cualquiera podría registrarlo y publicar una imagen maliciosa.
4. Definir quiénes son los mantenedores iniciales y quién recibe los reportes de seguridad.
5. Fijar la meta de `ceph/s3-tests` para dar por cerrado el MVP.

---

## 15. Referencias

Consultadas el 2026-10-13.

- Configuración de S3 en el AWS CLI (`multipart_threshold` de 8 MB): https://docs.aws.amazon.com/cli/latest/topic/s3-config.html
- Protecciones de integridad de datos en los SDK de AWS: https://docs.aws.amazon.com/sdkref/latest/guide/feature-dataintegrity.html
- Referencia de la API de Amazon S3: https://docs.aws.amazon.com/AmazonS3/latest/API/Welcome.html
- Suite de conformidad `ceph/s3-tests`: https://github.com/ceph/s3-tests
- Developer Certificate of Origin: https://developercertificate.org
- OpenSSF Scorecard: https://github.com/ossf/scorecard
- Sigstore cosign: https://github.com/sigstore/cosign
