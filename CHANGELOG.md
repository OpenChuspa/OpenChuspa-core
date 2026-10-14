# Changelog

Los cambios notables del proyecto. El formato sigue [Keep a Changelog](https://keepachangelog.com/es-ES/1.1.0/) y las versiones, [SemVer](https://semver.org/lang/es/). Mientras el proyecto esté en `0.x`, una versión menor puede romper compatibilidad y lo dirá aquí.

## [Sin publicar]

### Añadido

- Servidor S3 de un solo nodo: chuspas, objetos con `Range`, listados v1 y v2, multipart, copias y borrado masivo.
- Autenticación SigV4 por header, cuerpos `aws-chunked` firmados y sin firmar, y checksums CRC32, CRC32C, CRC64NVME, SHA-1 y SHA-256.
- Arranque seguro: el servidor no arranca sin credenciales, con un secreto corto ni en modo anónimo fuera de loopback.
- Almacenamiento con blobs opacos e índice bbolt, escritura atómica, limpieza de huérfanos y expiración de subidas multipart abandonadas.
- Escrituras y borrados condicionales, y completado idempotente de subidas multipart.
- Subcomandos `healthcheck`, `inspect` y `export`.
- CLI `ve` con los comandos `mira`, `crea`, `subi`, `baja`, `borra` y `sincroniza` sobre direcciones `chuspa://`, incluida en los binarios del release.
- Imagen distroless sin root y `docker-compose.yml` endurecido.
- Suite de compatibilidad con AWS CLI v2, boto3, `@aws-sdk/client-s3`, `aws-sdk-go-v2` y curl, y conformidad medida con `ceph/s3-tests`.
