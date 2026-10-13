// Package chunked decodifica cuerpos con Content-Encoding: aws-chunked, en
// sus variantes firmada (STREAMING-AWS4-HMAC-SHA256-PAYLOAD[-TRAILER]) y sin
// firmar (STREAMING-UNSIGNED-PAYLOAD-TRAILER).
//
// Formato:
//
//	<tamaño en hex>[;chunk-signature=<firma>]\r\n
//	<datos>\r\n
//	...
//	0[;chunk-signature=<firma>]\r\n
//	[<trailer>:<valor>\r\n]...
//	[x-amz-trailer-signature:<firma>\r\n]
//	\r\n
//
// Todo lo que entra aquí viene del cliente: las líneas y los trailers tienen
// límite de tamaño y cualquier desviación del formato es un error.
package chunked

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"strconv"
	"strings"
)

const (
	maxLineLen      = 4096
	maxTrailerBytes = 16 << 10
	maxTrailers     = 16

	trailerSignatureHeader = "x-amz-trailer-signature"
	emptySHA256            = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

var (
	// ErrMalformed indica que el cuerpo no respeta el formato aws-chunked.
	ErrMalformed = errors.New("chunked: formato aws-chunked inválido")
	// ErrSignature indica que la firma de un chunk o del trailer no coincide.
	ErrSignature = errors.New("chunked: la firma del chunk no coincide")
)

// Signing lleva lo necesario para verificar la firma encadenada de los chunks.
type Signing struct {
	Key   []byte // llave de firma derivada de SigV4
	Date  string // x-amz-date de la petición (20060102T150405Z)
	Scope string // fecha/región/servicio/aws4_request
	Seed  string // firma de la petición, ya verificada
}

// Options define la variante del formato que se espera.
type Options struct {
	// Signed exige chunk-signature en cada chunk.
	Signed bool
	// Trailer permite headers al final del cuerpo.
	Trailer bool
	// Signing verifica las firmas. Si es nil con Signed, las firmas se leen
	// pero no se verifican (modo anónimo en loopback).
	Signing *Signing
}

// Reader entrega el contenido decodificado. Devuelve io.EOF solo después de
// leer y verificar el último chunk y los trailers.
type Reader struct {
	br   *bufio.Reader
	opts Options

	remaining int64
	inChunk   bool
	chunkSig  string
	prevSig   string
	hash      hash.Hash
	trailers  map[string]string
	err       error
}

// NewReader envuelve r, que debe ser el cuerpo crudo de la petición.
func NewReader(r io.Reader, opts Options) *Reader {
	d := &Reader{br: bufio.NewReaderSize(r, 64<<10), opts: opts, trailers: map[string]string{}}
	if opts.Signing != nil {
		d.prevSig = opts.Signing.Seed
		d.hash = sha256.New()
	}
	return d
}

// Trailers devuelve los trailers recibidos, con el nombre en minúsculas. Solo
// es válido después de que Read devuelva io.EOF.
func (d *Reader) Trailers() map[string]string { return d.trailers }

func (d *Reader) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if d.remaining == 0 {
		if err := d.nextChunk(); err != nil {
			d.err = err
			return 0, err
		}
	}
	if int64(len(p)) > d.remaining {
		p = p[:d.remaining]
	}
	n, err := d.br.Read(p)
	if n > 0 {
		d.remaining -= int64(n)
		if d.hash != nil {
			d.hash.Write(p[:n])
		}
	}
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		d.err = err
		return n, err
	}
	return n, nil
}

// nextChunk cierra el chunk en curso y abre el siguiente. Devuelve io.EOF si
// el siguiente es el chunk final.
func (d *Reader) nextChunk() error {
	if d.inChunk {
		if err := d.expectCRLF(); err != nil {
			return err
		}
		if err := d.verifyChunk(); err != nil {
			return err
		}
	}
	size, sig, err := d.readChunkHeader()
	if err != nil {
		return err
	}
	d.chunkSig, d.inChunk = sig, true
	if size > 0 {
		d.remaining = size
		return nil
	}
	if err := d.verifyChunk(); err != nil {
		return err
	}
	if err := d.readTrailers(); err != nil {
		return err
	}
	return io.EOF
}

func (d *Reader) readChunkHeader() (int64, string, error) {
	line, err := d.readLine()
	if err != nil {
		return 0, "", err
	}
	sizeHex, ext, hasExt := strings.Cut(line, ";")
	if sizeHex == "" || len(sizeHex) > 16 {
		return 0, "", ErrMalformed
	}
	size, err := strconv.ParseInt(sizeHex, 16, 64)
	if err != nil || size < 0 {
		return 0, "", ErrMalformed
	}
	var sig string
	if hasExt {
		v, ok := strings.CutPrefix(ext, "chunk-signature=")
		if !ok || !isHex64(v) {
			return 0, "", ErrMalformed
		}
		sig = v
	}
	if d.opts.Signed != (sig != "") {
		return 0, "", ErrMalformed
	}
	return size, sig, nil
}

func (d *Reader) verifyChunk() error {
	s := d.opts.Signing
	if s == nil || !d.opts.Signed {
		return nil
	}
	want := s.sign("AWS4-HMAC-SHA256-PAYLOAD", d.prevSig, emptySHA256, hex.EncodeToString(d.hash.Sum(nil)))
	d.hash.Reset()
	if subtle.ConstantTimeCompare([]byte(want), []byte(d.chunkSig)) != 1 {
		return ErrSignature
	}
	d.prevSig = want
	return nil
}

func (d *Reader) readTrailers() error {
	var canonical bytes.Buffer
	var trailerSig string
	total := 0
	for {
		line, err := d.readLine()
		if err != nil {
			return err
		}
		if line == "" {
			break
		}
		total += len(line)
		if !d.opts.Trailer || total > maxTrailerBytes || len(d.trailers) >= maxTrailers {
			return ErrMalformed
		}
		name, value, ok := strings.Cut(line, ":")
		name, value = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(value)
		if !ok || name == "" {
			return ErrMalformed
		}
		if name == trailerSignatureHeader {
			if trailerSig != "" || !isHex64(value) {
				return ErrMalformed
			}
			trailerSig = value
			continue
		}
		if _, dup := d.trailers[name]; dup || trailerSig != "" {
			return ErrMalformed
		}
		d.trailers[name] = value
		canonical.WriteString(name + ":" + value + "\n")
	}

	if !d.opts.Signed || !d.opts.Trailer {
		if trailerSig != "" {
			return ErrMalformed
		}
		return nil
	}
	if trailerSig == "" {
		return ErrMalformed
	}
	if s := d.opts.Signing; s != nil {
		sum := sha256.Sum256(canonical.Bytes())
		want := s.sign("AWS4-HMAC-SHA256-TRAILER", d.prevSig, hex.EncodeToString(sum[:]))
		if subtle.ConstantTimeCompare([]byte(want), []byte(trailerSig)) != 1 {
			return ErrSignature
		}
	}
	return nil
}

// readLine lee una línea terminada en \r\n, sin el terminador.
func (d *Reader) readLine() (string, error) {
	var line []byte
	for {
		frag, err := d.br.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) > maxLineLen {
			return "", ErrMalformed
		}
		if err == nil {
			break
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			return "", io.ErrUnexpectedEOF
		}
		return "", err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return "", ErrMalformed
	}
	return string(line[:len(line)-2]), nil
}

func (d *Reader) expectCRLF() error {
	var crlf [2]byte
	if _, err := io.ReadFull(d.br, crlf[:]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	if crlf != [2]byte{'\r', '\n'} {
		return ErrMalformed
	}
	return nil
}

func (s *Signing) sign(algorithm, prevSig string, hashes ...string) string {
	parts := append([]string{algorithm, s.Date, s.Scope, prevSig}, hashes...)
	mac := hmac.New(sha256.New, s.Key)
	mac.Write([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(mac.Sum(nil))
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
