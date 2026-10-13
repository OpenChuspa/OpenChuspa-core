## Qué cambia y por qué

## Cómo se probó

- [ ] `go vet ./...` y `go test -race ./...`
- [ ] `test/compat/run.sh`

## Lista de revisión

- [ ] Los commits siguen el formato convencional y llevan `Signed-off-by`.
- [ ] Si es una operación de S3, tiene su caso con un cliente real en `test/compat/`.
- [ ] Si analiza entrada del cliente, tiene límites y prueba de fuzzing.
- [ ] Si cambia el diseño, hay un ADR en `docs/adr/`.
- [ ] El README y el CHANGELOG reflejan el cambio.
