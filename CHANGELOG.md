# Changelog

Los cambios notables del proyecto. El formato sigue [Keep a Changelog](https://keepachangelog.com/es-ES/1.1.0/) y las versiones, [SemVer](https://semver.org/lang/es/). Mientras el proyecto esté en `0.x`, una versión menor puede romper compatibilidad y lo dirá aquí. El formato de los datos en disco nunca se rompe sin migración automática.

## [Sin publicar]

## [0.3.0] - 2026-10-13

Primera versión que cierra el MVP: un nodo, durable, verificable y endurecido.

### Añadido

- Formato de datos versionado: los datos de 0.1.x y 0.2.x se migran solos al arrancar y una versión vieja se niega a abrir datos de una más nueva. Prueba de actualización con datos reales de v0.2.0.
- Metadatos junto a cada blob y una marca por chuspa: `chuspa rebuild-index` reconstruye el índice sin borrar nada.
- SHA-256 de cada objeto calculado por el servidor, `chuspa verify` y verificación periódica del contenido (`CHUSPA_SCRUB_INTERVAL`).
- Espacio libre mínimo en disco (`CHUSPA_MIN_FREE_MB`): por debajo, las escrituras responden `507 InsufficientStorage`.
- TLS nativo (`CHUSPA_TLS_CERT_FILE`, `CHUSPA_TLS_KEY_FILE`), mínimo TLS 1.2, con recarga del certificado sin reiniciar.
- Rotación del secreto sin corte (`CHUSPA_ROOT_PASSWORD_PREVIOUS`).
- Métricas de Prometheus en un puerto aparte (`CHUSPA_METRICS_ADDR`).
- Prueba de resistencia con `kill -9` (`test/soak`) y CI nocturno con fuzzing largo y resistencia.
- Bloqueo por fuerza bruta: 20 autenticaciones fallidas por minuto bloquean la IP 5 minutos (`CHUSPA_AUTH_MAX_FAILURES`).

## [0.2.0] - 2026-10-13

### Añadido

- CLI `ve` con los comandos `mira`, `crea`, `subi`, `baja`, `borra` y `sincroniza` sobre direcciones `chuspa://`, incluida en los binarios del release.

## [0.1.0] - 2026-10-13

### Añadido

- Servidor S3 de un solo nodo: chuspas, objetos con `Range`, listados v1 y v2, multipart, copias y borrado masivo.
- Autenticación SigV4 por header, cuerpos `aws-chunked` firmados y sin firmar, y checksums CRC32, CRC32C, CRC64NVME, SHA-1 y SHA-256.
- Arranque seguro: el servidor no arranca sin credenciales, con un secreto corto ni en modo anónimo fuera de loopback.
- Almacenamiento con blobs opacos e índice bbolt, escritura atómica, limpieza de huérfanos y expiración de subidas multipart abandonadas.
- Escrituras y borrados condicionales, y completado idempotente de subidas multipart.
- Subcomandos `healthcheck`, `inspect` y `export`.
- Imagen distroless sin root, publicada firmada en `ghcr.io/openchuspa/chuspa` y `docker.io/openchuspa/chuspa`, y `docker-compose.yml` endurecido.
- Suite de compatibilidad con AWS CLI v2, boto3, `@aws-sdk/client-s3`, `aws-sdk-go-v2` y curl, y conformidad medida con `ceph/s3-tests`.
