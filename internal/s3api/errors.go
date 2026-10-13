package s3api

import (
	"encoding/xml"
	"net/http"
)

// APIError es un error del protocolo S3: el código que entienden los
// clientes, un mensaje para humanos y el estado HTTP.
type APIError struct {
	Code    string
	Message string
	Status  int
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

var (
	ErrAccessDenied          = &APIError{"AccessDenied", "Acceso denegado.", http.StatusForbidden}
	ErrSignatureDoesNotMatch = &APIError{"SignatureDoesNotMatch", "La firma de la petición no coincide con la calculada por el servidor.", http.StatusForbidden}
	ErrRequestTimeTooSkewed  = &APIError{"RequestTimeTooSkewed", "La hora de la petición se aleja demasiado de la del servidor.", http.StatusForbidden}
	ErrNotImplemented        = &APIError{"NotImplemented", "Esta operación todavía no está implementada en OpenChuspa.", http.StatusNotImplemented}
	ErrMethodNotAllowed      = &APIError{"MethodNotAllowed", "El método no está permitido sobre este recurso.", http.StatusMethodNotAllowed}
	ErrInvalidRequest        = &APIError{"InvalidRequest", "La petición no es válida.", http.StatusBadRequest}
	ErrInvalidArgument       = &APIError{"InvalidArgument", "Argumento inválido.", http.StatusBadRequest}
	ErrInvalidBucketName     = &APIError{"InvalidBucketName", "El nombre de la chuspa no es válido: de 3 a 63 caracteres entre minúsculas, dígitos y guiones.", http.StatusBadRequest}
	ErrKeyTooLong            = &APIError{"KeyTooLongError", "La key supera los 1024 bytes.", http.StatusBadRequest}
	ErrInvalidKey            = &APIError{"InvalidArgument", "La key debe ser UTF-8 válido.", http.StatusBadRequest}
	ErrNoSuchBucket          = &APIError{"NoSuchBucket", "La chuspa no existe.", http.StatusNotFound}
	ErrNoSuchKey             = &APIError{"NoSuchKey", "El objeto no existe.", http.StatusNotFound}
	ErrNoSuchVersion         = &APIError{"NoSuchVersion", "La versión indicada no existe: OpenChuspa todavía no versiona objetos.", http.StatusNotFound}
	ErrNoSuchUpload          = &APIError{"NoSuchUpload", "La subida multipart no existe; pudo haber sido abortada o completada.", http.StatusNotFound}
	ErrBucketAlreadyOwned    = &APIError{"BucketAlreadyOwnedByYou", "La chuspa ya existe y es suya.", http.StatusConflict}
	ErrBucketNotEmpty        = &APIError{"BucketNotEmpty", "La chuspa no está vacía.", http.StatusConflict}
	ErrMissingContentLength  = &APIError{"MissingContentLength", "Falta el header Content-Length.", http.StatusLengthRequired}
	ErrEntityTooLarge        = &APIError{"EntityTooLarge", "El contenido supera el tamaño máximo permitido.", http.StatusBadRequest}
	ErrEntityTooSmall        = &APIError{"EntityTooSmall", "Una parte que no es la última es menor al mínimo de 5 MiB.", http.StatusBadRequest}
	ErrIncompleteBody        = &APIError{"IncompleteBody", "El cuerpo recibido no coincide con el tamaño declarado.", http.StatusBadRequest}
	ErrBadDigest             = &APIError{"BadDigest", "El checksum declarado no coincide con el contenido recibido.", http.StatusBadRequest}
	ErrInvalidDigest         = &APIError{"InvalidDigest", "El checksum declarado no tiene un formato válido.", http.StatusBadRequest}
	ErrContentSHA256Mismatch = &APIError{"XAmzContentSHA256Mismatch", "El x-amz-content-sha256 declarado no coincide con el contenido recibido.", http.StatusBadRequest}
	ErrMalformedXML          = &APIError{"MalformedXML", "El XML enviado no es válido para esta operación.", http.StatusBadRequest}
	ErrMetadataTooLarge      = &APIError{"MetadataTooLarge", "Los metadatos del objeto superan el máximo permitido.", http.StatusBadRequest}
	ErrInvalidPart           = &APIError{"InvalidPart", "Alguna parte no existe o su ETag no coincide.", http.StatusBadRequest}
	ErrInvalidPartOrder      = &APIError{"InvalidPartOrder", "Las partes deben venir en orden ascendente.", http.StatusBadRequest}
	ErrInvalidRange          = &APIError{"InvalidRange", "El rango pedido no se puede satisfacer.", http.StatusRequestedRangeNotSatisfiable}
	ErrPreconditionFailed    = &APIError{"PreconditionFailed", "No se cumplió una de las condiciones de la petición.", http.StatusPreconditionFailed}
	ErrRequestTimeout        = &APIError{"RequestTimeout", "La conexión estuvo demasiado tiempo sin enviar datos.", http.StatusBadRequest}
	ErrInternal              = &APIError{"InternalError", "Error interno del servidor.", http.StatusInternalServerError}
)

func invalidArgument(msg string) *APIError {
	return &APIError{"InvalidArgument", msg, http.StatusBadRequest}
}

func invalidRequest(msg string) *APIError {
	return &APIError{"InvalidRequest", msg, http.StatusBadRequest}
}

type errorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string
	Message   string
	Resource  string
	RequestID string `xml:"RequestId"`
}

// WriteError responde con el XML de error de S3.
func WriteError(w http.ResponseWriter, r *http.Request, e *APIError) {
	if r.Method == http.MethodHead {
		w.WriteHeader(e.Status) // HEAD no lleva cuerpo
		return
	}
	writeXML(w, e.Status, errorResponse{
		Code: e.Code, Message: e.Message, Resource: r.URL.Path,
		RequestID: w.Header().Get("x-amz-request-id"),
	})
}

func writeXML(w http.ResponseWriter, status int, v any) {
	body, err := xml.Marshal(v)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	w.Write([]byte(xml.Header))
	w.Write(body)
}
