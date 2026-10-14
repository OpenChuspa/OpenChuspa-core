# Compatibilidad

Qué funciona hoy contra clientes reales y cuánto de la suite de conformidad pasa. Todo lo que aparece aquí se ejecuta en CI en cada pull request; lo que no aparece no se promete.

Última actualización: 2026-10-14 (v1.0.0).

## Clientes

Flujo completo de cada cliente en su configuración por defecto, contra el binario real ([`test/compat/run.sh`](../test/compat/run.sh)):

| Cliente | Versión probada | Qué recorre |
|---|---|---|
| AWS CLI v2 | la del runner de CI (2.37 en la última ejecución local) | `mb`, `ls`, `cp` de ida y vuelta, archivo de 100 MB (multipart y rangos), 1 050 objetos, `cp` y `mv` entre chuspas, `rm --recursive`, `rb`, `s3api` |
| boto3 | la última de PyPI | objeto con metadatos, rango, `upload_file`/`download_file` de 20 MB, multipart manual, paginador |
| `@aws-sdk/client-s3` | 3.1151.0 (fijada en `test/compat/js`) | objeto con metadatos, cuerpo en stream con trailer, rango, `lib-storage`, copia, paginación, borrado masivo |
| `aws-sdk-go-v2` | `service/s3` 1.114.3 (fijada en `test/compat/gosdk`) | objeto con metadatos, rango, gestor de subidas y descargas, copia, paginador, borrado masivo |
| curl | la del sistema | firma con `--aws-sigv4`, peticiones sin firma, cuerpo alterado, headers gigantes |

Los SDK de JavaScript y Go necesitan *path style* activado; el estilo de host virtual no está soportado.

## Criterios de aceptación del MVP

Los de la sección 11 del diseño, todos en `test/compat/run.sh` o en las pruebas unitarias:

| Criterio | Dónde se verifica |
|---|---|
| Flujo funcional con archivo de 100 MB | `run.sh` |
| Los datos sobreviven al reinicio | `run.sh` y job de la imagen |
| `kill -9` en mitad de una subida no deja objetos a medias | `run.sh` |
| Arranque sin credenciales | `run.sh`, job de la imagen, `internal/config` |
| Petición sin firma → `AccessDenied` | `run.sh`, `internal/s3api` |
| Secreto incorrecto → `SignatureDoesNotMatch` | `run.sh`, `internal/auth` |
| Fecha fuera de la ventana → `RequestTimeTooSkewed` | `internal/auth`, `internal/s3api` |
| Cuerpo alterado después de firmar → 400, sin objeto | `run.sh`, `internal/s3api` |
| Checksum incorrecto → `BadDigest`, sin objeto | `run.sh`, `internal/s3api` |
| Key `../../etc/passwd` es un objeto normal | `run.sh`, `internal/storage/fsbolt` |
| Headers o cuerpo por encima de los límites | `run.sh` |
| Cero secretos en los logs de toda la suite | `run.sh` |

## Conformidad con ceph/s3-tests

Suite: [`ceph/s3-tests`](https://github.com/ceph/s3-tests) en el commit `5b3987116eca`, archivo `s3tests/functional/test_s3.py`.

| | Pruebas |
|---|---|
| Total en el archivo | 838 |
| Omitidas por la propia suite | 94 |
| Ejecutadas | 744 |
| **Pasan** | **222 (29,8 % de las ejecutadas)** |

Las 222 están listadas en [`test/s3tests/passing.txt`](../test/s3tests/passing.txt) y el CI las ejecuta todas en cada pull request: si una deja de pasar, el CI falla. El número solo puede subir.

Las demás prueban operaciones que OpenChuspa no ofrece y que responden `501 NotImplemented`: ACL y acceso anónimo, versionado, políticas, cifrado SSE, lifecycle, subidas por formulario, URLs prefirmadas, tags, CORS, object lock, logging, ownership controls, `GetObjectAttributes`, una segunda identidad y comportamientos propios de Ceph RGW (`x-rgw-*`, cuotas).

Diferencias deliberadas con lo que espera la suite:

- Crear una chuspa que ya existe responde `BucketAlreadyOwnedByYou` (409) siempre; la suite espera el éxito que da S3 en `us-east-1`.
- Los nombres de chuspa con puntos se rechazan: el direccionamiento es solo por ruta.
- Completar una subida con el mismo número de parte repetido responde `InvalidPartOrder`.

**Criterio de la serie 1.x:** cero regresiones sobre las 222 y ninguna falla atribuible a una operación que el motor declara soportar. Medido el 2026-10-14 con `run.sh --all`: 222 de 744.

Para recalcular la lista:

```bash
test/s3tests/run.sh --all   # reescribe test/s3tests/passing.txt
git diff test/s3tests/passing.txt
```
