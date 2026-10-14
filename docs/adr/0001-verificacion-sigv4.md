# ADR 0001: la firma SigV4 se verifica con canonicalización propia

- **Estado:** aceptada
- **Fecha:** 2026-10-13
- **Reemplaza:** la fila 6 de la sección 0 y el paso 4 de la sección 7.3 de `MVP-ENGINE-v2.md`

## Contexto

El diseño v2 proponía recalcular la firma esperada con el firmador oficial
(`github.com/aws/aws-sdk-go-v2/aws/signer/v4`) para no reescribir la pieza más
propensa a errores.

Al implementarlo apareció un problema de fondo: un firmador decide **qué
headers firmar**, y un verificador tiene que firmar **exactamente los que el
cliente declaró** en `SignedHeaders`. El firmador oficial:

- ignora siempre `User-Agent`, `Expect`, `Transfer-Encoding` y
  `X-Amzn-Trace-Id`, aunque el cliente los haya firmado;
- solo incluye `content-length` cuando es mayor que cero, y hay clientes que
  firman `content-length: 0`;
- no expone la petición canónica ni permite fijar la lista de headers.

Con él, peticiones legítimas de otros SDK serían rechazadas sin que el motor
pudiera hacer nada.

## Decisión

`internal/auth` construye la petición canónica con la lista de headers que
trae el cliente y calcula el HMAC con la biblioteca estándar. El firmador
oficial se queda como **oráculo de pruebas**: los tests firman peticiones con
él y exigen que el verificador las acepte, y que rechace cualquier alteración.

La verificación sigue fallando cerrado: header mal formado, header firmado
ausente, llave desconocida, región o servicio distintos y firma incorrecta
responden lo mismo (`SignatureDoesNotMatch`), y la firma se calcula siempre
para que el tiempo de respuesta no revele el motivo.

## Consecuencias

- `aws-sdk-go-v2` pasa a ser una dependencia solo de pruebas.
- La canonicalización es código propio (unas 100 líneas) y por eso lleva
  pruebas contra el firmador oficial, fuzzing del header `Authorization` y la
  suite con clientes reales (`test/compat/run.sh`).
- El enrutado tampoco usa `http.ServeMux`, como sugería la sección 3.1: el mux
  limpia la ruta (`..`, `//`) y redirige, lo que rompe keys que S3 considera
  válidas. El enrutador es un `switch` por método y query en `internal/s3api`.
