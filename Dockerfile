# Los digest fijan la imagen exacta; Dependabot los actualiza por PR.

# ---- build ----
FROM golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -buildid= -X main.version=${VERSION}" \
      -o /out/chuspa ./cmd/chuspa
RUN mkdir -p /out/data

# ---- runtime ----
# Sin shell, sin gestor de paquetes y sin root.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/chuspa /usr/local/bin/chuspa
COPY --from=build --chown=nonroot:nonroot /out/data /data
USER nonroot:nonroot
EXPOSE 9000
VOLUME ["/data"]
# Dentro del contenedor el aislamiento lo da la publicación del puerto.
ENV CHUSPA_ADDR=":9000" CHUSPA_DATA_DIR="/data"
# El healthcheck es un subcomando del binario porque la imagen no trae curl.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD ["/usr/local/bin/chuspa", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/chuspa"]
CMD ["serve"]
