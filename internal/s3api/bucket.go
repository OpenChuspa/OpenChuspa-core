package s3api

import (
	"encoding/base64"
	"io"
	"net/http"
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

func (s *Server) listObjectsV2(r *request) *APIError {
	q := r.URL.Query()
	in := storage.ListInput{Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"), MaxKeys: maxListKeys}
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return invalidArgument("max-keys debe ser un entero no negativo.")
		}
		in.MaxKeys = min(n, maxListKeys)
	}
	encode := func(s string) string { return s }
	switch q.Get("encoding-type") {
	case "":
	case "url":
		encode = urlEncodeKey
	default:
		return invalidArgument("encoding-type solo admite el valor url.")
	}
	token := q.Get("continuation-token")
	if token != "" {
		after, isPrefix, ok := decodeToken(token)
		if !ok {
			return invalidArgument("continuation-token inválido.")
		}
		in.After, in.AfterPrefix = after, isPrefix
	} else {
		in.After = q.Get("start-after")
	}

	res, err := s.Backend.ListObjects(r.Context(), r.bucket, in)
	if err != nil {
		return storageError(err)
	}
	out := listObjectsV2Result{
		Name: r.bucket, Prefix: encode(in.Prefix), Delimiter: encode(in.Delimiter),
		StartAfter: encode(q.Get("start-after")), MaxKeys: in.MaxKeys, EncodingType: q.Get("encoding-type"),
		KeyCount: len(res.Objects) + len(res.CommonPrefixes), IsTruncated: res.Truncated,
		ContinuationToken: token,
	}
	if res.Truncated {
		out.NextContinuationToken = encodeToken(res.Next, res.NextIsPrefix)
	}
	for _, o := range res.Objects {
		out.Contents = append(out.Contents, objectEntry{
			Key: encode(o.Key), LastModified: o.Modified.Format(timeFormat),
			ETag: quote(o.ETag), Size: o.Size, StorageClass: "STANDARD",
		})
	}
	for _, p := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{Prefix: encode(p)})
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
