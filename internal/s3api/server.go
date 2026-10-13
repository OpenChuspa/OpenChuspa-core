// Package s3api implementa el subconjunto del protocolo S3 que soporta el
// motor: enrutado por estilo de ruta, handlers, XML y catálogo de errores.
// Lo que no está implementado responde NotImplemented, nunca un éxito falso.
package s3api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/OpenChuspa/OpenChuspa-core/internal/auth"
	"github.com/OpenChuspa/OpenChuspa-core/internal/storage"
)

const (
	// HealthPath no choca con ninguna chuspa: "-" no es un nombre válido.
	HealthPath = "/-/health"

	// DefaultMaxObjectSize es el tamaño máximo de un PutObject o de una parte.
	DefaultMaxObjectSize = 5 << 30

	maxXMLBody = 1 << 20
	maxParts   = 10000
)

// Server atiende la API de S3 sobre un storage.Backend.
type Server struct {
	Backend storage.Backend
	// Verifier verifica SigV4. Si es nil el servidor está en modo anónimo, que
	// la configuración solo permite en loopback.
	Verifier      *auth.Verifier
	Region        string
	Log           *slog.Logger
	MaxObjectSize int64
}

// request agrupa lo que los handlers necesitan de una petición ya autenticada.
type request struct {
	*http.Request
	w       http.ResponseWriter
	id      *auth.Identity // nil en modo anónimo
	payload auth.Payload
	bucket  string
	key     string
}

// Subrecursos de S3 que el motor todavía no implementa.
var unsupportedSubresources = []string{
	"accelerate", "acl", "analytics", "attributes", "cors", "delete", "encryption",
	"intelligent-tiering", "inventory", "legal-hold", "lifecycle", "logging", "metrics",
	"notification", "object-lock", "ownershipControls", "policy", "policyStatus",
	"publicAccessBlock", "replication", "requestPayment", "restore", "retention",
	"select", "tagging", "torrent", "versionId", "versioning", "versions", "website",
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == HealthPath && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
		return
	}

	req := &request{Request: r, w: w}
	if s.Verifier != nil {
		id, err := s.Verifier.Verify(r)
		if err != nil {
			WriteError(w, r, authError(err))
			return
		}
		req.id = id
	}
	payload, err := auth.ParsePayload(r.Header.Get("X-Amz-Content-Sha256"))
	if err != nil {
		WriteError(w, r, invalidRequest("Valor de x-amz-content-sha256 no soportado."))
		return
	}
	req.payload = payload

	if apiErr := s.route(req); apiErr != nil {
		if apiErr.Status >= 500 && apiErr != ErrNotImplemented {
			s.Log.Error("error interno", "request_id", w.Header().Get("x-amz-request-id"), "error", apiErr.Message)
			apiErr = ErrInternal
		}
		WriteError(w, r, apiErr)
	}
}

func authError(err error) *APIError {
	switch {
	case errors.Is(err, auth.ErrMissing):
		return ErrAccessDenied
	case errors.Is(err, auth.ErrSkew):
		return ErrRequestTimeTooSkewed
	case errors.Is(err, auth.ErrUnsupported):
		return ErrNotImplemented
	}
	return ErrSignatureDoesNotMatch
}

func (s *Server) route(r *request) *APIError {
	query := r.URL.Query()
	for _, sub := range unsupportedSubresources {
		if query.Has(sub) {
			return ErrNotImplemented
		}
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	r.bucket, r.key, _ = strings.Cut(path, "/")

	if r.bucket == "" {
		if r.Method == http.MethodGet {
			return s.listBuckets(r)
		}
		return ErrMethodNotAllowed
	}
	if !validBucketName(r.bucket) {
		return ErrInvalidBucketName
	}

	if r.key == "" {
		switch r.Method {
		case http.MethodPut:
			return s.createBucket(r)
		case http.MethodHead:
			return s.headBucket(r)
		case http.MethodDelete:
			return s.deleteBucket(r)
		case http.MethodGet:
			switch {
			case query.Has("location"):
				return s.getBucketLocation(r)
			case query.Has("uploads"):
				return ErrNotImplemented // ListMultipartUploads: fase 1.1
			case query.Get("list-type") == "2":
				return s.listObjectsV2(r)
			}
			return ErrNotImplemented // ListObjects v1: fase 1.1
		case http.MethodPost:
			return ErrNotImplemented
		}
		return ErrMethodNotAllowed
	}

	if err := validateKey(r.key); err != nil {
		return err
	}
	switch r.Method {
	case http.MethodPut:
		switch {
		case r.Header.Get("X-Amz-Copy-Source") != "":
			return ErrNotImplemented // CopyObject y UploadPartCopy: fase 1.1
		case query.Has("uploadId"):
			return s.uploadPart(r)
		}
		return s.putObject(r)
	case http.MethodGet:
		if query.Has("uploadId") {
			return s.listParts(r)
		}
		return s.getObject(r, true)
	case http.MethodHead:
		return s.getObject(r, false)
	case http.MethodDelete:
		if query.Has("uploadId") {
			return s.abortUpload(r)
		}
		return s.deleteObject(r)
	case http.MethodPost:
		switch {
		case query.Has("uploads"):
			return s.createUpload(r)
		case query.Has("uploadId"):
			return s.completeUpload(r)
		}
	}
	return ErrMethodNotAllowed
}

// storageError traduce los errores del almacenamiento al protocolo.
func storageError(err error) *APIError {
	switch {
	case errors.Is(err, storage.ErrBucketNotFound):
		return ErrNoSuchBucket
	case errors.Is(err, storage.ErrBucketExists):
		return ErrBucketAlreadyOwned
	case errors.Is(err, storage.ErrBucketNotEmpty):
		return ErrBucketNotEmpty
	case errors.Is(err, storage.ErrObjectNotFound):
		return ErrNoSuchKey
	case errors.Is(err, storage.ErrUploadNotFound):
		return ErrNoSuchUpload
	case errors.Is(err, storage.ErrInvalidPart):
		return ErrInvalidPart
	case errors.Is(err, storage.ErrPartOrder):
		return ErrInvalidPartOrder
	case errors.Is(err, storage.ErrPartTooSmall):
		return ErrEntityTooSmall
	case errors.Is(err, storage.ErrBadDigest):
		return ErrBadDigest
	}
	return &APIError{"InternalError", err.Error(), http.StatusInternalServerError}
}
