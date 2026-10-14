package main

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/aws/smithy-go"
)

// targetError asocia un error con la dirección sobre la que se operaba.
type targetError struct {
	loc location
	err error
}

func (e targetError) Error() string { return e.loc.String() + ": " + e.err.Error() }
func (e targetError) Unwrap() error { return e.err }

func target(loc location, err error) error { return targetError{loc, err} }

func apiCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

// explain traduce los errores más comunes a un mensaje que dice qué hacer.
func explain(err error, endpoint string) string {
	prefix := ""
	var t targetError
	if errors.As(err, &t) {
		prefix = t.loc.String() + ": "
		if t.loc.Key == "" {
			prefix = "chuspa://" + t.loc.Bucket + ": "
		}
	}

	switch code := apiCode(err); code {
	case "NoSuchBucket":
		return prefix + "esa chuspa no existe"
	case "NoSuchKey", "NotFound":
		return prefix + "esa chuchería no existe"
	case "BucketAlreadyOwnedByYou":
		return prefix + "esa chuspa ya existe y es suya"
	case "BucketAlreadyExists":
		return prefix + "ese nombre de chuspa ya está ocupado"
	case "BucketNotEmpty":
		return prefix + "la chuspa no está vacía: use ve borra -r para vaciarla y borrarla"
	case "InvalidBucketName":
		return prefix + "nombre de chuspa inválido: de 3 a 63 caracteres entre minúsculas, dígitos, puntos y guiones"
	case "InvalidAccessKeyId", "SignatureDoesNotMatch", "AccessDenied", "AuthorizationHeaderMalformed":
		return fmt.Sprintf("%sel motor rechazó las credenciales (%s): revise AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY y la región", prefix, code)
	case "":
	default:
		var api smithy.APIError
		errors.As(err, &api)
		return fmt.Sprintf("%s%s: %s", prefix, code, api.ErrorMessage())
	}

	var netErr *net.OpError
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelado"
	case errors.As(err, &netErr) && netErr.Op == "dial":
		return fmt.Sprintf("no encuentro el motor en %s: ¿está corriendo? Ajuste VE_ENDPOINT si escucha en otra dirección", endpoint)
	}
	if t.err != nil {
		return prefix + t.err.Error()
	}
	return err.Error()
}
