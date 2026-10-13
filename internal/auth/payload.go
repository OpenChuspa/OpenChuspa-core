package auth

import "errors"

// Valores especiales del header x-amz-content-sha256.
const (
	UnsignedPayload          = "UNSIGNED-PAYLOAD"
	StreamingSigned          = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	StreamingSignedTrailer   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	StreamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
)

// ErrPayloadMode: valor de x-amz-content-sha256 que el motor no soporta.
var ErrPayloadMode = errors.New("auth: valor de x-amz-content-sha256 no soportado")

// Payload describe cómo viene el cuerpo de la petición.
type Payload struct {
	// SHA256 es el hash declarado del cuerpo, en hexadecimal; vacío si el
	// cuerpo no viene firmado o viene en streaming.
	SHA256 string
	// Streaming indica un cuerpo aws-chunked.
	Streaming bool
	// SignedChunks indica que cada chunk trae su firma.
	SignedChunks bool
	// Trailer indica que el cuerpo termina con trailers.
	Trailer bool
}

// ParsePayload interpreta el valor de x-amz-content-sha256. Un valor vacío
// equivale a un cuerpo sin firmar.
func ParsePayload(v string) (Payload, error) {
	switch v {
	case "", UnsignedPayload:
		return Payload{}, nil
	case StreamingSigned:
		return Payload{Streaming: true, SignedChunks: true}, nil
	case StreamingSignedTrailer:
		return Payload{Streaming: true, SignedChunks: true, Trailer: true}, nil
	case StreamingUnsignedTrailer:
		return Payload{Streaming: true, Trailer: true}, nil
	}
	if isHex64(v) {
		return Payload{SHA256: v}, nil
	}
	return Payload{}, ErrPayloadMode
}
