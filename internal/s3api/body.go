package s3api

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/OpenChuspa/OpenChuspa-core/internal/checksum"
	"github.com/OpenChuspa/OpenChuspa-core/internal/chunked"
	"github.com/OpenChuspa/OpenChuspa-core/internal/storage"
)

// Margen para el encuadre de aws-chunked sobre el tamaño del contenido.
const chunkFramingSlack = 64 << 20

// ingested es un cuerpo ya guardado en staging y verificado.
type ingested struct {
	staged       storage.Staged
	etag         string // MD5 en hexadecimal
	checksumAlgo string
	checksum     string
}

// body es el cuerpo decodificado de una petición, con las verificaciones
// de integridad que el cliente declaró.
type body struct {
	r        *request
	reader   io.Reader
	chunks   *chunked.Reader
	declared int64
	md5      hash.Hash
	sha256   hash.Hash
	sum      hash.Hash // checksum x-amz-checksum-*, si el cliente lo envía
	sumAlgo  string
	sumWant  string // valor del header; vacío si llega como trailer
}

// openBody prepara la lectura del cuerpo con un tamaño máximo de limit bytes
// de contenido. No lee nada todavía.
func (s *Server) openBody(r *request, limit int64) (*body, *APIError) {
	b := &body{r: r, md5: md5.New()}

	if r.payload.Streaming {
		n, err := strconv.ParseInt(r.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64)
		if err != nil || n < 0 {
			return nil, ErrMissingContentLength
		}
		b.declared = n
	} else {
		if r.ContentLength < 0 {
			return nil, ErrMissingContentLength
		}
		b.declared = r.ContentLength
	}
	if b.declared > limit {
		return nil, ErrEntityTooLarge
	}

	algo, want, apiErr := declaredChecksum(r.Request)
	if apiErr != nil {
		return nil, apiErr
	}
	if algo != "" {
		b.sumAlgo, b.sumWant, b.sum = algo, want, checksum.New(algo)
	}

	var raw io.Reader = http.MaxBytesReader(r.w, r.Body, limit+chunkFramingSlack)
	raw = &idleReader{r: raw, rc: http.NewResponseController(r.w), timeout: s.idleTimeout()}
	if r.payload.Streaming {
		opts := chunked.Options{Signed: r.payload.SignedChunks, Trailer: r.payload.Trailer}
		if r.id != nil {
			opts.Signing = r.id.Signing
		}
		b.chunks = chunked.NewReader(raw, opts)
		raw = b.chunks
	}
	// Un byte de más basta para detectar un cuerpo mayor al declarado.
	raw = io.LimitReader(raw, b.declared+1)

	writers := []io.Writer{b.md5}
	if r.payload.SHA256 != "" {
		b.sha256 = sha256.New()
		writers = append(writers, b.sha256)
	}
	if b.sum != nil {
		writers = append(writers, b.sum)
	}
	b.reader = io.TeeReader(raw, io.MultiWriter(writers...))
	return b, nil
}

// declaredChecksum busca el checksum que anuncia el cliente, como header o
// como trailer. Solo se admite uno por petición.
func declaredChecksum(r *http.Request) (algo, want string, apiErr *APIError) {
	for _, a := range checksum.Algorithms {
		if v := r.Header.Get(checksum.HeaderPrefix + a); v != "" {
			if algo != "" {
				return "", "", invalidRequest("Solo se admite un checksum por petición.")
			}
			algo, want = a, v
		}
	}
	for _, name := range strings.Split(r.Header.Get("X-Amz-Trailer"), ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		a, ok := strings.CutPrefix(name, checksum.HeaderPrefix)
		if a, supported := checksum.Normalize(a); ok && supported && algo == "" {
			algo = a
			continue
		}
		return "", "", invalidRequest("Trailer no soportado: " + name)
	}
	return algo, want, nil
}

// verify comprueba, con todo el cuerpo ya leído, el tamaño y los hashes
// declarados. size es la cantidad de bytes de contenido leídos.
func (b *body) verify(size int64) *APIError {
	if size != b.declared {
		return ErrIncompleteBody
	}
	if b.sha256 != nil {
		got := hex.EncodeToString(b.sha256.Sum(nil))
		if subtle.ConstantTimeCompare([]byte(got), []byte(b.r.payload.SHA256)) != 1 {
			return ErrContentSHA256Mismatch
		}
	}
	if v := b.r.Header.Get("Content-MD5"); v != "" {
		want, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(want) != md5.Size {
			return ErrInvalidDigest
		}
		if !bytes.Equal(want, b.md5.Sum(nil)) {
			return ErrBadDigest
		}
	}
	if b.sum != nil {
		want := b.sumWant
		if want == "" {
			if b.chunks == nil {
				return invalidRequest("Se anunció un trailer sin cuerpo aws-chunked.")
			}
			want = b.chunks.Trailers()[checksum.HeaderPrefix+b.sumAlgo]
		}
		if want != checksum.Encode(b.sum) {
			return ErrBadDigest
		}
	}
	if b.chunks != nil {
		for name := range b.chunks.Trailers() {
			if name != checksum.HeaderPrefix+b.sumAlgo || b.sumWant != "" {
				return invalidRequest("Llegó un trailer que no fue anunciado: " + name)
			}
		}
	}
	return nil
}

// ingest guarda el cuerpo en staging y lo verifica. Si devuelve un error, no
// queda nada en disco.
func (s *Server) ingest(r *request) (*ingested, *APIError) {
	limit := s.MaxObjectSize
	if limit <= 0 {
		limit = DefaultMaxObjectSize
	}
	b, apiErr := s.openBody(r, limit)
	if apiErr != nil {
		return nil, apiErr
	}
	staged, err := s.Backend.Stage(r.Context(), b.reader)
	if err != nil {
		return nil, bodyError(err)
	}
	if apiErr := b.verify(staged.Size()); apiErr != nil {
		staged.Discard()
		return nil, apiErr
	}
	out := &ingested{staged: staged, etag: hex.EncodeToString(b.md5.Sum(nil))}
	if b.sum != nil {
		out.checksumAlgo, out.checksum = b.sumAlgo, checksum.Encode(b.sum)
	}
	return out, nil
}

// readXML lee y verifica un cuerpo XML pequeño.
func (s *Server) readXML(r *request) ([]byte, *APIError) {
	b, apiErr := s.openBody(r, maxXMLBody)
	if apiErr != nil {
		if apiErr == ErrEntityTooLarge {
			return nil, ErrMalformedXML
		}
		return nil, apiErr
	}
	data, err := io.ReadAll(b.reader)
	if err != nil {
		return nil, bodyError(err)
	}
	if apiErr := b.verify(int64(len(data))); apiErr != nil {
		return nil, apiErr
	}
	return data, nil
}

func bodyError(err error) *APIError {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, chunked.ErrSignature):
		return ErrSignatureDoesNotMatch
	case errors.Is(err, chunked.ErrMalformed), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, context.Canceled):
		return ErrIncompleteBody
	case errors.As(err, &tooLarge):
		return ErrEntityTooLarge
	case errors.Is(err, os.ErrDeadlineExceeded):
		return ErrRequestTimeout
	}
	return storageError(err)
}

// idleReader renueva el plazo de lectura de la conexión en cada lectura: un
// cliente puede tardar lo que sea en subir, pero no quedarse callado.
type idleReader struct {
	r       io.Reader
	rc      *http.ResponseController
	timeout time.Duration
}

func (i *idleReader) Read(p []byte) (int, error) {
	i.rc.SetReadDeadline(time.Now().Add(i.timeout))
	n, err := i.r.Read(p)
	// El plazo solo se levanta al terminar bien. Tras un error se deja puesto:
	// net/http intenta vaciar el cuerpo antes de responder y sin plazo se
	// quedaría esperando al mismo cliente que dejó de enviar.
	if err == io.EOF {
		i.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}
