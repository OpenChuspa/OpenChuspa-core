package s3api

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/OpenChuspa/OpenChuspa-core/internal/auth"
	"github.com/OpenChuspa/OpenChuspa-core/internal/storage"
)

const maxListKeys = 1000

func (s *Server) listBuckets(r *request) *APIError {
	buckets, err := s.Backend.ListBuckets(r.Context())
	if err != nil {
		return storageError(err)
	}
	out := listBucketsResult{Owner: owner{ID: ownerID, DisplayName: ownerID}}
	for _, b := range buckets {
		out.Buckets = append(out.Buckets, bucketEntry{Name: b.Name, CreationDate: b.Created.Format(timeFormat)})
	}
	writeXML(r.w, http.StatusOK, out)
	return nil
}

func (s *Server) createBucket(r *request) *APIError {
	// El cuerpo opcional (CreateBucketConfiguration) solo trae la región, que
	// en un nodo único no cambia nada; se verifica su integridad y se descarta.
	if r.ContentLength != 0 {
		if _, apiErr := s.readXML(r); apiErr != nil {
			return apiErr
		}
	}
	if err := s.Backend.CreateBucket(r.Context(), r.bucket); err != nil {
		return storageError(err)
	}
	r.w.Header().Set("Location", "/"+r.bucket)
	r.w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) headBucket(r *request) *APIError {
	if _, err := s.Backend.GetBucket(r.Context(), r.bucket); err != nil {
		return storageError(err)
	}
	r.w.Header().Set("x-amz-bucket-region", s.Region)
	r.w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) getBucketLocation(r *request) *APIError {
	if _, err := s.Backend.GetBucket(r.Context(), r.bucket); err != nil {
		return storageError(err)
	}
	writeXML(r.w, http.StatusOK, locationConstraint{Region: s.Region})
	return nil
}

func (s *Server) deleteBucket(r *request) *APIError {
	if err := s.Backend.DeleteBucket(r.Context(), r.bucket); err != nil {
		return storageError(err)
	}
	r.w.WriteHeader(http.StatusNoContent)
	return nil
}

// listParams son los parámetros comunes a las dos versiones del listado.
type listParams struct {
	in       storage.ListInput
	encoding string
	encode   func(string) string
}

func parseListParams(q url.Values, maxParam string) (listParams, *APIError) {
	p := listParams{
		in:     storage.ListInput{Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"), MaxKeys: maxListKeys},
		encode: func(s string) string { return s },
	}
	if v := q.Get(maxParam); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return p, invalidArgument(maxParam + " debe ser un entero no negativo.")
		}
		p.in.MaxKeys = min(n, maxListKeys)
	}
	switch p.encoding = q.Get("encoding-type"); p.encoding {
	case "":
	case "url":
		p.encode = urlEncodeKey
	default:
		return p, invalidArgument("encoding-type solo admite el valor url.")
	}
	return p, nil
}

func objectEntries(objects []storage.Object, encode func(string) string) []objectEntry {
	out := make([]objectEntry, 0, len(objects))
	for _, o := range objects {
		out = append(out, objectEntry{
			Key: encode(o.Key), LastModified: o.Modified.Format(timeFormat),
			ETag: quote(o.ETag), Size: o.Size, StorageClass: "STANDARD",
		})
	}
	return out
}

func prefixEntries(prefixes []string, encode func(string) string) []commonPrefix {
	out := make([]commonPrefix, 0, len(prefixes))
	for _, p := range prefixes {
		out = append(out, commonPrefix{Prefix: encode(p)})
	}
	return out
}

func (s *Server) listObjectsV2(r *request) *APIError {
	q := r.URL.Query()
	p, apiErr := parseListParams(q, "max-keys")
	if apiErr != nil {
		return apiErr
	}
	token := q.Get("continuation-token")
	if token != "" {
		after, isPrefix, ok := decodeToken(token)
		if !ok {
			return invalidArgument("continuation-token inválido.")
		}
		p.in.After, p.in.AfterPrefix = after, isPrefix
	} else {
		p.in.After = q.Get("start-after")
	}

	res, err := s.Backend.ListObjects(r.Context(), r.bucket, p.in)
	if err != nil {
		return storageError(err)
	}
	out := listObjectsV2Result{
		Name: r.bucket, Prefix: p.encode(p.in.Prefix), Delimiter: p.encode(p.in.Delimiter),
		StartAfter: p.encode(q.Get("start-after")), MaxKeys: p.in.MaxKeys, EncodingType: p.encoding,
		KeyCount: len(res.Objects) + len(res.CommonPrefixes), IsTruncated: res.Truncated,
		ContinuationToken: token,
		Contents:          objectEntries(res.Objects, p.encode),
		CommonPrefixes:    prefixEntries(res.CommonPrefixes, p.encode),
	}
	if res.Truncated {
		out.NextContinuationToken = encodeToken(res.Next, res.NextIsPrefix)
	}
	writeXML(r.w, http.StatusOK, out)
	return nil
}

// listObjectsV1 es el listado original, que pagina con marker.
func (s *Server) listObjectsV1(r *request) *APIError {
	q := r.URL.Query()
	p, apiErr := parseListParams(q, "max-keys")
	if apiErr != nil {
		return apiErr
	}
	marker := q.Get("marker")
	p.in.After = marker
	// Un marker que es un prefijo común (el NextMarker de la página anterior)
	// salta todo lo que cuelga de él.
	p.in.AfterPrefix = p.in.Delimiter != "" && strings.HasSuffix(marker, p.in.Delimiter) && strings.HasPrefix(marker, p.in.Prefix)

	res, err := s.Backend.ListObjects(r.Context(), r.bucket, p.in)
	if err != nil {
		return storageError(err)
	}
	out := listObjectsV1Result{
		Name: r.bucket, Prefix: p.encode(p.in.Prefix), Marker: p.encode(marker), Delimiter: p.encode(p.in.Delimiter),
		MaxKeys: p.in.MaxKeys, EncodingType: p.encoding, IsTruncated: res.Truncated,
		Contents:       objectEntries(res.Objects, p.encode),
		CommonPrefixes: prefixEntries(res.CommonPrefixes, p.encode),
	}
	if res.Truncated {
		out.NextMarker = p.encode(res.Next)
	}
	writeXML(r.w, http.StatusOK, out)
	return nil
}

func (s *Server) listUploads(r *request) *APIError {
	q := r.URL.Query()
	if q.Get("delimiter") != "" {
		return ErrNotImplemented
	}
	p, apiErr := parseListParams(q, "max-uploads")
	if apiErr != nil {
		return apiErr
	}
	keyMarker, idMarker := q.Get("key-marker"), q.Get("upload-id-marker")
	uploads, err := s.Backend.ListUploads(r.Context(), r.bucket)
	if err != nil {
		return storageError(err)
	}
	out := listUploadsResult{
		Bucket: r.bucket, KeyMarker: p.encode(keyMarker), UploadIDMarker: idMarker,
		Prefix: p.encode(p.in.Prefix), MaxUploads: p.in.MaxKeys, EncodingType: p.encoding,
	}
	me := owner{ID: ownerID, DisplayName: ownerID}
	for _, u := range uploads {
		if !strings.HasPrefix(u.Key, p.in.Prefix) {
			continue
		}
		// Después del marcador: keys mayores o, con la misma key, ids mayores.
		if keyMarker != "" && (u.Key < keyMarker || (u.Key == keyMarker && (idMarker == "" || u.ID <= idMarker))) {
			continue
		}
		if len(out.Uploads) == p.in.MaxKeys {
			out.IsTruncated = true
			break
		}
		out.Uploads = append(out.Uploads, uploadEntry{
			Key: p.encode(u.Key), UploadID: u.ID, Initiator: me, Owner: me,
			StorageClass: "STANDARD", Initiated: u.Initiated.Format(timeFormat),
		})
		out.NextKeyMarker, out.NextUploadIDMarker = p.encode(u.Key), u.ID
	}
	if !out.IsTruncated {
		out.NextKeyMarker, out.NextUploadIDMarker = "", ""
	}
	writeXML(r.w, http.StatusOK, out)
	return nil
}

// El token de continuación es opaco para el cliente: el último elemento
// devuelto, marcado como objeto (k) o prefijo común (p).
func encodeToken(last string, isPrefix bool) string {
	kind := "k"
	if isPrefix {
		kind = "p"
	}
	return base64.RawURLEncoding.EncodeToString([]byte(kind + last))
}

func decodeToken(token string) (last string, isPrefix, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < 2 || len(raw) > maxKeyLen+1 || (raw[0] != 'k' && raw[0] != 'p') {
		return "", false, false
	}
	return string(raw[1:]), raw[0] == 'p', true
}

// urlEncodeKey codifica una key para encoding-type=url, conservando "/".
func urlEncodeKey(s string) string {
	return auth.URIEncode(s, false)
}

func quote(etag string) string { return `"` + etag + `"` }

func unquote(etag string) string { return strings.Trim(strings.TrimSpace(etag), `"`) }

// drain descarta lo que quede del cuerpo para poder reutilizar la conexión.
func drain(r *http.Request) { io.Copy(io.Discard, io.LimitReader(r.Body, maxXMLBody)) }
