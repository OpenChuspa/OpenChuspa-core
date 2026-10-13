package s3api

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/OpenChuspa/OpenChuspa-core/internal/checksum"
	"github.com/OpenChuspa/OpenChuspa-core/internal/storage"
)

const (
	defaultContentType = "binary/octet-stream"
	userMetaPrefix     = "X-Amz-Meta-"
	maxUserMetadata    = 2 << 10
)

// metadataFromHeaders extrae los metadatos que el cliente define al subir.
func metadataFromHeaders(h http.Header) (storage.Metadata, *APIError) {
	m := storage.Metadata{
		ContentType:        h.Get("Content-Type"),
		ContentDisposition: h.Get("Content-Disposition"),
		ContentLanguage:    h.Get("Content-Language"),
		CacheControl:       h.Get("Cache-Control"),
	}
	// aws-chunked describe el transporte, no el objeto.
	var encodings []string
	for _, e := range strings.Split(h.Get("Content-Encoding"), ",") {
		if e = strings.TrimSpace(e); e != "" && !strings.EqualFold(e, "aws-chunked") {
			encodings = append(encodings, e)
		}
	}
	m.ContentEncoding = strings.Join(encodings, ",")

	size := 0
	for name, values := range h {
		suffix, ok := strings.CutPrefix(name, userMetaPrefix)
		if !ok || suffix == "" {
			continue
		}
		if m.User == nil {
			m.User = map[string]string{}
		}
		key, value := strings.ToLower(suffix), strings.Join(values, ",")
		m.User[key] = value
		size += len(key) + len(value)
	}
	if size > maxUserMetadata {
		return m, ErrMetadataTooLarge
	}
	return m, nil
}

func (s *Server) putObject(r *request) *APIError {
	if _, err := s.Backend.GetBucket(r.Context(), r.bucket); err != nil {
		return storageError(err)
	}
	meta, apiErr := metadataFromHeaders(r.Header)
	if apiErr != nil {
		return apiErr
	}
	in, apiErr := s.ingest(r)
	if apiErr != nil {
		return apiErr
	}
	obj, err := s.Backend.PutObject(r.Context(), r.bucket, r.key, in.staged, storage.PutInput{
		ETag: in.etag, ChecksumAlgo: in.checksumAlgo, Checksum: in.checksum, Metadata: meta,
	})
	if err != nil {
		return storageError(err)
	}
	h := r.w.Header()
	h.Set("ETag", quote(obj.ETag))
	setChecksumHeaders(h, obj.ChecksumAlgo, obj.Checksum)
	r.w.WriteHeader(http.StatusOK)
	return nil
}

func setChecksumHeaders(h http.Header, algo, sum string) {
	if algo == "" || sum == "" {
		return
	}
	h.Set(checksum.HeaderPrefix+algo, sum)
	if strings.Contains(sum, "-") {
		h.Set("x-amz-checksum-type", "COMPOSITE")
	} else {
		h.Set("x-amz-checksum-type", "FULL_OBJECT")
	}
}

// getObject atiende GetObject (withBody) y HeadObject.
func (s *Server) getObject(r *request, withBody bool) *APIError {
	var (
		obj     storage.Object
		content io.ReadSeekCloser
		err     error
	)
	if withBody {
		obj, content, err = s.Backend.GetObject(r.Context(), r.bucket, r.key)
		if err == nil {
			defer content.Close()
		}
	} else {
		obj, err = s.Backend.HeadObject(r.Context(), r.bucket, r.key)
	}
	if err != nil {
		return storageError(err)
	}

	h := r.w.Header()
	h.Set("ETag", quote(obj.ETag))
	h.Set("Last-Modified", obj.Modified.Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")

	if v := r.Header.Get("If-Match"); v != "" && !etagMatches(v, obj.ETag) {
		return ErrPreconditionFailed
	}
	if v := r.Header.Get("If-None-Match"); v != "" && etagMatches(v, obj.ETag) {
		r.w.WriteHeader(http.StatusNotModified)
		return nil
	}

	start, length, partial, satisfiable := parseRange(r.Header.Get("Range"), obj.Size)
	if !satisfiable {
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(obj.Size, 10))
		return ErrInvalidRange
	}
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
		h.Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(start+length-1, 10)+"/"+strconv.FormatInt(obj.Size, 10))
	} else {
		start, length = 0, obj.Size
		// El checksum guardado es del objeto completo: no aplica a un rango.
		if strings.EqualFold(r.Header.Get("X-Amz-Checksum-Mode"), "ENABLED") {
			setChecksumHeaders(h, obj.ChecksumAlgo, obj.Checksum)
		}
	}

	contentType := obj.ContentType
	if contentType == "" {
		contentType = defaultContentType
	}
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	setIfNotEmpty(h, "Content-Encoding", obj.ContentEncoding)
	setIfNotEmpty(h, "Content-Disposition", obj.ContentDisposition)
	setIfNotEmpty(h, "Content-Language", obj.ContentLanguage)
	setIfNotEmpty(h, "Cache-Control", obj.CacheControl)
	for k, v := range obj.User {
		// Asignación directa: http.Header.Set pondría "X-Amz-Meta-Barrio" y los
		// SDK devuelven el nombre del metadato tal como llega.
		h["x-amz-meta-"+k] = []string{v}
	}

	if !withBody {
		r.w.WriteHeader(status)
		return nil
	}
	if _, err := content.Seek(start, io.SeekStart); err != nil {
		return storageError(err)
	}
	r.w.WriteHeader(status)
	// Con los headers ya enviados, un fallo aquí solo puede cortar la conexión.
	if _, err := io.CopyN(r.w, content, length); err != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}

func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" || unquote(candidate) == etag {
			return true
		}
	}
	return false
}

func setIfNotEmpty(h http.Header, name, value string) {
	if value != "" {
		h.Set(name, value)
	}
}

func (s *Server) deleteObject(r *request) *APIError {
	if err := s.Backend.DeleteObject(r.Context(), r.bucket, r.key); err != nil {
		return storageError(err)
	}
	r.w.WriteHeader(http.StatusNoContent)
	return nil
}
