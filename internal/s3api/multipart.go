package s3api

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	"github.com/OpenChuspa/OpenChuspa-core/internal/checksum"
	"github.com/OpenChuspa/OpenChuspa-core/internal/storage"
)

func (s *Server) createUpload(r *request) *APIError {
	drain(r.Request)
	meta, apiErr := metadataFromHeaders(r.Header)
	if apiErr != nil {
		return apiErr
	}
	u := storage.Upload{Bucket: r.bucket, Key: r.key, Metadata: meta}
	if v := r.Header.Get("X-Amz-Checksum-Algorithm"); v != "" {
		algo, ok := checksum.Normalize(v)
		if !ok {
			return invalidRequest("Algoritmo de checksum no soportado: " + v)
		}
		u.ChecksumAlgo = algo
		switch strings.ToUpper(r.Header.Get("X-Amz-Checksum-Type")) {
		case "": // por defecto, CRC64NVME es de objeto completo y el resto compuesto
			u.ChecksumFullObject = algo == checksum.CRC64NVME
		case "FULL_OBJECT":
			if !checksum.FullObject(algo) {
				return invalidRequest("El algoritmo " + v + " no admite checksum de objeto completo.")
			}
			u.ChecksumFullObject = true
		case "COMPOSITE":
			if algo == checksum.CRC64NVME {
				return invalidRequest("CRC64NVME solo admite checksum de objeto completo.")
			}
		default:
			return invalidRequest("x-amz-checksum-type inválido.")
		}
	}
	id, err := s.Backend.CreateUpload(r.Context(), u)
	if err != nil {
		return storageError(err)
	}
	if u.ChecksumAlgo != "" {
		r.w.Header().Set("x-amz-checksum-algorithm", strings.ToUpper(u.ChecksumAlgo))
		r.w.Header().Set("x-amz-checksum-type", checksumType(u.ChecksumFullObject))
	}
	writeXML(r.w, http.StatusOK, initiateMultipartUploadResult{Bucket: r.bucket, Key: r.key, UploadID: id})
	return nil
}

func checksumType(fullObject bool) string {
	if fullObject {
		return "FULL_OBJECT"
	}
	return "COMPOSITE"
}

func (s *Server) uploadPart(r *request) *APIError {
	q := r.URL.Query()
	number, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil || number < 1 || number > maxParts {
		return invalidArgument("partNumber debe ser un entero entre 1 y 10000.")
	}
	uploadID := q.Get("uploadId")
	if _, err := s.Backend.GetUpload(r.Context(), r.bucket, r.key, uploadID); err != nil {
		return storageError(err)
	}
	in, apiErr := s.ingest(r)
	if apiErr != nil {
		return apiErr
	}
	part, err := s.Backend.PutPart(r.Context(), r.bucket, r.key, uploadID, number, in.staged, storage.PutInput{
		ETag: in.etag, ChecksumAlgo: in.checksumAlgo, Checksum: in.checksum,
	})
	if err != nil {
		return storageError(err)
	}
	h := r.w.Header()
	h.Set("ETag", quote(part.ETag))
	if part.ChecksumAlgo != "" {
		h.Set(checksum.HeaderPrefix+part.ChecksumAlgo, part.Checksum)
	}
	r.w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) completeUpload(r *request) *APIError {
	uploadID := r.URL.Query().Get("uploadId")
	upload, err := s.Backend.GetUpload(r.Context(), r.bucket, r.key, uploadID)
	if err != nil {
		return storageError(err)
	}
	data, apiErr := s.readXML(r)
	if apiErr != nil {
		return apiErr
	}
	var in completeMultipartUpload
	if err := xml.Unmarshal(data, &in); err != nil || len(in.Parts) == 0 || len(in.Parts) > maxParts {
		return ErrMalformedXML
	}
	parts := make([]storage.CompletedPart, len(in.Parts))
	for i, p := range in.Parts {
		parts[i] = storage.CompletedPart{Number: p.PartNumber, ETag: unquote(p.ETag)}
	}
	var want string
	if upload.ChecksumAlgo != "" {
		want = r.Header.Get(checksum.HeaderPrefix + upload.ChecksumAlgo)
	}

	obj, err := s.Backend.CompleteUpload(r.Context(), r.bucket, r.key, uploadID, parts, want)
	if err != nil {
		return storageError(err)
	}
	out := completeMultipartUploadResult{
		Location: "/" + r.bucket + "/" + r.key, Bucket: r.bucket, Key: r.key, ETag: quote(obj.ETag),
	}
	if obj.Checksum != "" {
		out.ChecksumType = checksumType(!strings.Contains(obj.Checksum, "-"))
		switch obj.ChecksumAlgo {
		case checksum.CRC32:
			out.ChecksumCRC32 = obj.Checksum
		case checksum.CRC32C:
			out.ChecksumCRC32C = obj.Checksum
		case checksum.CRC64NVME:
			out.ChecksumCRC64NVME = obj.Checksum
		case checksum.SHA1:
			out.ChecksumSHA1 = obj.Checksum
		case checksum.SHA256:
			out.ChecksumSHA256 = obj.Checksum
		}
	}
	writeXML(r.w, http.StatusOK, out)
	return nil
}

func (s *Server) abortUpload(r *request) *APIError {
	if err := s.Backend.AbortUpload(r.Context(), r.bucket, r.key, r.URL.Query().Get("uploadId")); err != nil {
		return storageError(err)
	}
	r.w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listParts(r *request) *APIError {
	q := r.URL.Query()
	uploadID := q.Get("uploadId")
	maxResults, marker := 1000, 0
	if v := q.Get("max-parts"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return invalidArgument("max-parts debe ser un entero no negativo.")
		}
		maxResults = min(n, 1000)
	}
	if v := q.Get("part-number-marker"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return invalidArgument("part-number-marker debe ser un entero no negativo.")
		}
		marker = n
	}
	parts, err := s.Backend.ListParts(r.Context(), r.bucket, r.key, uploadID)
	if err != nil {
		return storageError(err)
	}
	out := listPartsResult{Bucket: r.bucket, Key: r.key, UploadID: uploadID, PartNumberMarker: marker, MaxParts: maxResults}
	for _, p := range parts {
		if p.Number <= marker {
			continue
		}
		if len(out.Parts) == maxResults {
			out.IsTruncated = true
			break
		}
		out.Parts = append(out.Parts, partEntry{
			PartNumber: p.Number, LastModified: p.Modified.Format(timeFormat), ETag: quote(p.ETag), Size: p.Size,
		})
		out.NextPartNumberMarker = p.Number
	}
	writeXML(r.w, http.StatusOK, out)
	return nil
}
