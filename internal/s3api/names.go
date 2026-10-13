package s3api

import (
	"regexp"
	"unicode/utf8"
)

const maxKeyLen = 1024

// Sin puntos en el MVP: el estilo de host virtual llega en la fase 2.
var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

func validBucketName(name string) bool { return bucketNameRe.MatchString(name) }

// validateKey no restringe caracteres: la key nunca se usa como ruta de
// archivo, así que "..", "/" y compañía son nombres como cualquier otro.
func validateKey(key string) *APIError {
	if len(key) > maxKeyLen {
		return ErrKeyTooLong
	}
	if !utf8.ValidString(key) {
		return ErrInvalidKey
	}
	return nil
}
