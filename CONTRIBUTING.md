# Cómo contribuir

Gracias por querer meterle la mano al motor. Esta guía es corta a propósito.

## Antes de empezar

- Para un bug o una mejora pequeña, abra el pull request directamente.
- Para un cambio de diseño (formato en disco, protocolo, seguridad, una dependencia nueva), abra primero un issue. Esos cambios se discuten antes de programarse y quedan en un ADR en `docs/adr/`.
- Una vulnerabilidad **no** va en un issue: siga [SECURITY.md](./SECURITY.md).

## Preparar el entorno

Necesita Go (la versión de `go.mod`). Para la suite de compatibilidad, además: AWS CLI v2 y curl; opcionalmente `python3` con boto3 y Node.js con npm.

```bash
go build ./...
go vet ./...
go test -race ./...
test/compat/run.sh
```

`test/compat/run.sh` compila el motor, lo arranca en una carpeta temporal con credenciales aleatorias y lo prueba con clientes reales. No toca su configuración de AWS.

## Reglas del código

- **Sala limpia.** No copie ni traduzca código de proyectos con licencia AGPL, GPL, SSPL o BUSL (MinIO entre ellos). Se implementa desde la documentación pública del protocolo.
- **Dependencias:** una nueva solo entra con un issue que explique por qué la biblioteca estándar no basta, cuál es su licencia y quién la mantiene. Licencias admitidas: Apache-2.0, MIT, BSD e ISC.
- **Nunca un éxito falso.** Lo que no esté implementado responde `NotImplemented`. Un header o parámetro que pida algo que el motor no hace se rechaza, no se ignora.
- **Toda entrada del cliente tiene límite** de tamaño y formato. Si agrega un analizador de entrada no confiable, agréguele una prueba de fuzzing.
- **Toda funcionalidad entra con su prueba**, y si es una operación de S3, con su caso en `test/compat/` usando un cliente real.
- **Nada de secretos en los logs:** ni `Authorization`, ni firmas, ni llaves, ni la query de la petición.
- `gofmt`, `go vet` y `staticcheck` sin observaciones.

## Commits y pull requests

- **Commits convencionales:** `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`. Con ellos se arma el changelog.
- **DCO:** cada commit lleva `Signed-off-by` (`git commit -s`). Con eso usted certifica el [Developer Certificate of Origin](https://developercertificate.org): que el código es suyo o tiene derecho a aportarlo bajo la licencia del proyecto. No hay CLA ni se ceden derechos a nadie.
- Un pull request, un tema. Describa qué cambia, por qué y cómo lo probó.
- El CI debe pasar completo y un mantenedor debe aprobarlo.

## Licencia

Al contribuir acepta que su aporte se publica bajo la licencia [Apache-2.0](./LICENSE).
