package s3api

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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
		Expires:            h.Get("Expires"),
	}
	// aws-chunked describe el transporte, no el objeto: se quita y el resto
	// se conserva tal como llegó.
	m.ContentEncoding = h.Get("Content-Encoding")
	if strings.Contains(strings.ToLower(m.ContentEncoding), "aws-chunked") {
		var encodings []string
		for _, e := range strings.Split(m.ContentEncoding, ",") {
			if e = strings.TrimSpace(e); e != "" && !strings.EqualFold(e, "aws-chunked") {
				encodings = append(encodings, e)
			}
		}
		m.ContentEncoding = strings.Join(encodings, ", ")
	}

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

// unsupportedWriteHeaders rechaza los headers que piden algo que el motor no
// hace. Ignorarlos sería un éxito falso: el cliente creería que su objeto
// quedó cifrado, público, con tags o bloqueado.
func unsupportedWriteHeaders(h http.Header) *APIError {
	if acl := h.Get("X-Amz-Acl"); acl != "" && acl != "private" {
		return ErrNotImplemented
	}
	if class := h.Get("X-Amz-Storage-Class"); class != "" && class != "STANDARD" {
		return ErrNotImplemented
	}
	for name := range h {
		switch {
		case name == "X-Amz-Tagging", name == "X-Amz-Website-Redirect-Location",
			strings.HasPrefix(name, "X-Amz-Server-Side-Encryption"),
			strings.HasPrefix(name, "X-Amz-Object-Lock-"),
			strings.HasPrefix(name, "X-Amz-Grant-"):
			return ErrNotImplemented
		}
	}
	return nil
}

func (s *Server) getObjectTagging(r *request) *APIError {
	if _, err := s.Backend.HeadObject(r.Context(), r.bucket, r.key); err != nil {
		return storageError(err)
	}
	writeXML(r.w, http.StatusOK, tagging{})
	return nil
}

func (s *Server) putObject(r *request) *APIError {
	if apiErr := unsupportedWriteHeaders(r.Header); apiErr != nil {
		return apiErr
	}
	if _, err := s.Backend.GetBucket(r.Context(), r.bucket); err != nil {
		return storageError(err)
	}
	meta, apiErr := metadataFromHeaders(r.Header)
	if apiErr != nil {
		return apiErr
	}
	cond, apiErr := writePrecondition(r.Header)
	if apiErr != nil {
		return apiErr
	}
	in, apiErr := s.ingest(r)
	if apiErr != nil {
		return apiErr
	}
	obj, err := s.Backend.PutObject(r.Context(), r.bucket, r.key, in.staged, storage.PutInput{
		ETag: in.etag, ChecksumAlgo: in.checksumAlgo, Checksum: in.checksum, Metadata: meta, Cond: cond,
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

// writePrecondition interpreta las escrituras condicionales: If-None-Match: *
// (crear solo si no existe) e If-Match: <etag> (reemplazar solo esa versión).
func writePrecondition(h http.Header) (storage.Precondition, *APIError) {
	var cond storage.Precondition
	if v := strings.TrimSpace(h.Get("If-None-Match")); v != "" {
		if v != "*" {
			return cond, ErrNotImplemented // S3 solo admite * en escrituras
		}
		cond.IfNoneMatch = true
	}
	if v := strings.TrimSpace(h.Get("If-Match")); v != "" {
		if strings.Contains(v, ",") {
			return cond, ErrNotImplemented
		}
		cond.IfMatch = unquote(v)
	}
	return cond, nil
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
	// Leer una sola parte de un objeto multipart no está soportado, y
	// devolver el objeto entero en su lugar sería mentir.
	if r.URL.Query().Has("partNumber") {
		return ErrNotImplemented
	}

	h := r.w.Header()
	h.Set("ETag", quote(obj.ETag))
	h.Set("Last-Modified", obj.Modified.Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")

	// Condiciones, con la precedencia de HTTP: las de ETag mandan sobre las
	// de fecha, que se comparan con resolución de un segundo.
	modified := obj.Modified.Truncate(time.Second)
	ifMatch, ifNoneMatch := r.Header.Get("If-Match"), r.Header.Get("If-None-Match")
	switch {
	case ifMatch != "":
		if !etagMatches(ifMatch, obj.ETag) {
			return ErrPreconditionFailed
		}
	case r.Header.Get("If-Unmodified-Since") != "":
		if t, err := http.ParseTime(r.Header.Get("If-Unmodified-Since")); err == nil && modified.After(t) {
			return ErrPreconditionFailed
		}
	}
	switch {
	case ifNoneMatch != "":
		if etagMatches(ifNoneMatch, obj.ETag) {
			r.w.WriteHeader(http.StatusNotModified)
			return nil
		}
	case r.Header.Get("If-Modified-Since") != "":
		if t, err := http.ParseTime(r.Header.Get("If-Modified-Since")); err == nil && !modified.After(t) {
			r.w.WriteHeader(http.StatusNotModified)
			return nil
		}
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
	setIfNotEmpty(h, "Expires", obj.Expires)
	// El cliente puede pedir que la respuesta lleve otros headers, por ejemplo
	// para forzar la descarga desde un navegador.
	for param, header := range responseOverrides {
		if v := r.URL.Query().Get(param); v != "" {
			h.Set(header, v)
		}
	}
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
	if err := s.send(r.w, io.LimitReader(content, length)); err != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}

// send copia src a la respuesta renovando el plazo de escritura en cada
// bloque, para soltar las descargas de clientes que dejan de leer.
func (s *Server) send(w http.ResponseWriter, src io.Reader) error {
	rc := http.NewResponseController(w)
	defer rc.SetWriteDeadline(time.Time{})
	buf := make([]byte, 256<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			rc.SetWriteDeadline(time.Now().Add(s.idleTimeout()))
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// copySource interpreta x-amz-copy-source ("chuspa/key", codificado como URL).
func copySource(r *http.Request) (bucket, key string, apiErr *APIError) {
	raw := r.Header.Get("X-Amz-Copy-Source")
	if strings.Contains(raw, "?") {
		return "", "", ErrNotImplemented // copiar una versión concreta
	}
	source, err := url.PathUnescape(raw)
	if err != nil {
		return "", "", invalidArgument("x-amz-copy-source inválido.")
	}
	bucket, key, _ = strings.Cut(strings.TrimPrefix(source, "/"), "/")
	if !validBucketName(bucket) || key == "" || validateKey(key) != nil {
		return "", "", invalidArgument("x-amz-copy-source debe tener la forma chuspa/key.")
	}
	for _, h := range []string{"X-Amz-Copy-Source-If-Modified-Since", "X-Amz-Copy-Source-If-Unmodified-Since"} {
		if r.Header.Get(h) != "" {
			return "", "", ErrNotImplemented
		}
	}
	return bucket, key, nil
}

func copyPreconditions(r *http.Request, etag string) *APIError {
	if v := r.Header.Get("X-Amz-Copy-Source-If-Match"); v != "" && !etagMatches(v, etag) {
		return ErrPreconditionFailed
	}
	if v := r.Header.Get("X-Amz-Copy-Source-If-None-Match"); v != "" && etagMatches(v, etag) {
		return ErrPreconditionFailed
	}
	return nil
}

func (s *Server) copyObject(r *request) *APIError {
	drain(r.Request)
	if apiErr := unsupportedWriteHeaders(r.Header); apiErr != nil {
		return apiErr
	}
	// COPY (por defecto) conserva los tags del origen, que nunca existen.
	if d := r.Header.Get("X-Amz-Tagging-Directive"); d != "" && !strings.EqualFold(d, "COPY") {
		return ErrNotImplemented
	}
	srcBucket, srcKey, apiErr := copySource(r.Request)
	if apiErr != nil {
		return apiErr
	}
	if r.Header.Get("X-Amz-Copy-Source-Range") != "" {
		return invalidArgument("x-amz-copy-source-range solo aplica al copiar una parte.")
	}
	replace := false
	switch strings.ToUpper(r.Header.Get("X-Amz-Metadata-Directive")) {
	case "", "COPY":
	case "REPLACE":
		replace = true
	default:
		return invalidArgument("x-amz-metadata-directive debe ser COPY o REPLACE.")
	}
	if srcBucket == r.bucket && srcKey == r.key && !replace {
		return invalidRequest("Copiar un objeto sobre sí mismo exige cambiar sus metadatos (x-amz-metadata-directive: REPLACE).")
	}
	if _, err := s.Backend.GetBucket(r.Context(), r.bucket); err != nil {
		return storageError(err)
	}

	obj, content, err := s.Backend.GetObject(r.Context(), srcBucket, srcKey)
	if err != nil {
		return storageError(err)
	}
	defer content.Close()
	if apiErr := copyPreconditions(r.Request, obj.ETag); apiErr != nil {
		return apiErr
	}
	meta := obj.Metadata
	if replace {
		var apiErr *APIError
		if meta, apiErr = metadataFromHeaders(r.Header); apiErr != nil {
			return apiErr
		}
	}

	staged, err := s.Backend.Stage(r.Context(), content)
	if err != nil {
		return storageError(err)
	}
	// El contenido es idéntico: el ETag y el checksum del origen siguen valiendo.
	copied, err := s.Backend.PutObject(r.Context(), r.bucket, r.key, staged, storage.PutInput{
		ETag: obj.ETag, ChecksumAlgo: obj.ChecksumAlgo, Checksum: obj.Checksum, Metadata: meta,
	})
	if err != nil {
		return storageError(err)
	}
	writeXML(r.w, http.StatusOK, copyObjectResult{LastModified: copied.Modified.Format(timeFormat), ETag: quote(copied.ETag)})
	return nil
}

func (s *Server) deleteObjects(r *request) *APIError {
	if _, err := s.Backend.GetBucket(r.Context(), r.bucket); err != nil {
		return storageError(err)
	}
	// Igual que S3: este cuerpo siempre viaja con una suma de integridad.
	if algo, _, _ := declaredChecksum(r.Request); algo == "" && r.Header.Get("Content-MD5") == "" {
		return invalidRequest("Falta Content-MD5 o un header x-amz-checksum-*.")
	}
	data, apiErr := s.readXML(r, true)
	if apiErr != nil {
		return apiErr
	}
	var in deleteRequest
	if err := xml.Unmarshal(data, &in); err != nil || len(in.Objects) == 0 || len(in.Objects) > maxListKeys {
		return ErrMalformedXML
	}
	var out deleteResult
	for _, o := range in.Objects {
		apiErr := validateKey(o.Key)
		if apiErr == nil && o.VersionID != "" && o.VersionID != nullVersion {
			apiErr = ErrNoSuchVersion
		}
		if apiErr == nil && (o.LastModifiedTime != "" || o.Size != "") {
			apiErr = ErrNotImplemented // solo se soporta la condición por ETag
		}
		if apiErr == nil {
			err := s.Backend.DeleteObject(r.Context(), r.bucket, o.Key, unquote(o.ETag))
			// Con una condición, que el objeto no exista también es un error.
			if err != nil {
				apiErr = storageError(err)
			}
		}
		switch {
		case apiErr != nil:
			apiErr = s.hideInternal(apiErr, r.w.Header().Get("x-amz-request-id"))
			out.Errors = append(out.Errors, deleteError{Key: o.Key, Code: apiErr.Code, Message: apiErr.Message})
		case !in.Quiet:
			out.Deleted = append(out.Deleted, deletedEntry{Key: o.Key})
		}
	}
	writeXML(r.w, http.StatusOK, out)
	return nil
}

var responseOverrides = map[string]string{
	"response-content-type":        "Content-Type",
	"response-content-language":    "Content-Language",
	"response-content-encoding":    "Content-Encoding",
	"response-content-disposition": "Content-Disposition",
	"response-cache-control":       "Cache-Control",
	"response-expires":             "Expires",
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
	if r.Header.Get("X-Amz-If-Match-Last-Modified-Time") != "" || r.Header.Get("X-Amz-If-Match-Size") != "" {
		return ErrNotImplemented
	}
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	if strings.Contains(ifMatch, ",") {
		return ErrNotImplemented
	}
	if err := s.Backend.DeleteObject(r.Context(), r.bucket, r.key, unquote(ifMatch)); err != nil {
		return storageError(err)
	}
	r.w.WriteHeader(http.StatusNoContent)
	return nil
}
