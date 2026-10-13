// Package checksum implementa los algoritmos de integridad que envían los
// clientes de S3 en los headers o trailers x-amz-checksum-*.
package checksum

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"strconv"
	"strings"
)

// HeaderPrefix es el prefijo común de los headers de checksum.
const HeaderPrefix = "x-amz-checksum-"

// Algoritmos soportados, con el nombre que usan en el header.
const (
	CRC32     = "crc32"
	CRC32C    = "crc32c"
	CRC64NVME = "crc64nvme"
	SHA1      = "sha1"
	SHA256    = "sha256"
)

// Algorithms lista los algoritmos soportados.
var Algorithms = []string{CRC32, CRC32C, CRC64NVME, SHA1, SHA256}

var (
	castagnoli = crc32.MakeTable(crc32.Castagnoli)
	// Polinomio de CRC-64/NVME (0xad93d23594c93659) en forma reflejada.
	nvme = crc64.MakeTable(0x9a6c9329ac4bc9b5)
)

// Normalize devuelve el nombre canónico del algoritmo ("CRC32" -> "crc32") y
// si está soportado.
func Normalize(name string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, a := range Algorithms {
		if a == n {
			return a, true
		}
	}
	return "", false
}

// New devuelve el hash del algoritmo, o nil si no está soportado.
func New(algo string) hash.Hash {
	switch algo {
	case CRC32:
		return crc32.NewIEEE()
	case CRC32C:
		return crc32.New(castagnoli)
	case CRC64NVME:
		return crc64.New(nvme)
	case SHA1:
		return sha1.New()
	case SHA256:
		return sha256.New()
	}
	return nil
}

// FullObject indica si el algoritmo admite un checksum del objeto completo en
// subidas multipart (los CRC); los demás solo admiten el compuesto.
func FullObject(algo string) bool {
	return algo == CRC32 || algo == CRC32C || algo == CRC64NVME
}

// Encode devuelve el valor del hash como lo espera S3: base64 de los bytes.
func Encode(h hash.Hash) string {
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Composite calcula el checksum compuesto de una subida multipart: el
// checksum de la concatenación de los checksums de cada parte, con el sufijo
// "-N". Devuelve falso si algún valor no es base64 válido.
func Composite(algo string, parts []string) (string, bool) {
	h := New(algo)
	if h == nil {
		return "", false
	}
	for _, p := range parts {
		raw, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			return "", false
		}
		h.Write(raw)
	}
	return Encode(h) + "-" + strconv.Itoa(len(parts)), true
}
