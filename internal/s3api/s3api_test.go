package s3api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/OpenChuspa/OpenChuspa-core/internal/auth"
	"github.com/OpenChuspa/OpenChuspa-core/internal/checksum"
	"github.com/OpenChuspa/OpenChuspa-core/internal/storage/fsbolt"
)

const (
	testKey    = "parcero"
	testSecret = "0123456789abcdef0123456789abcdef"
	testRegion = "cali-san-antonio-1"
)

// newServer levanta la API sobre un almacenamiento temporal. Con signed=false
// queda en modo anónimo.
func newServer(t *testing.T, signed bool) *httptest.Server {
	t.Helper()
	backend, _, err := fsbolt.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	api := &Server{Backend: backend, Region: testRegion, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if signed {
		api.Verifier = &auth.Verifier{AccessKey: testKey, SecretKey: testSecret, Region: testRegion}
	}
	srv := httptest.NewServer(api)
	t.Cleanup(func() { srv.Close(); backend.Close() })
	return srv
}

func do(t *testing.T, req *http.Request) (int, string, http.Header) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func call(t *testing.T, method, url, body string, headers ...string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return do(t, req)
}

func wantStatus(t *testing.T, what string, status int, body string, want int, code string) {
	t.Helper()
	if status != want || !strings.Contains(body, code) {
		t.Fatalf("%s: HTTP %d %s; se esperaba %d %s", what, status, body, want, code)
	}
}

func crc32Of(s string) string {
	h := crc32.NewIEEE()
	h.Write([]byte(s))
	return checksum.Encode(h)
}

func TestTrailerChecksum(t *testing.T) {
	srv := newServer(t, false)
	call(t, "PUT", srv.URL+"/chuspa", "")
	content := "oíste mira, vé"
	chunkedBody := func(sum string) string {
		return fmt.Sprintf("%x\r\n%s\r\n0\r\nx-amz-checksum-crc32:%s\r\n\r\n", len(content), content, sum)
	}
	headers := []string{
		"X-Amz-Content-Sha256", auth.StreamingUnsignedTrailer, "Content-Encoding", "aws-chunked",
		"X-Amz-Trailer", "x-amz-checksum-crc32", "X-Amz-Decoded-Content-Length", fmt.Sprint(len(content)),
	}

	status, body, _ := call(t, "PUT", srv.URL+"/chuspa/malo", chunkedBody("AAAAAA=="), headers...)
	wantStatus(t, "trailer con checksum incorrecto", status, body, 400, "BadDigest")
	status, body, _ = call(t, "GET", srv.URL+"/chuspa/malo", "")
	wantStatus(t, "objeto rechazado", status, body, 404, "NoSuchKey")

	status, body, h := call(t, "PUT", srv.URL+"/chuspa/bueno", chunkedBody(crc32Of(content)), headers...)
	if status != 200 || h.Get("x-amz-checksum-crc32") != crc32Of(content) {
		t.Fatalf("trailer válido: HTTP %d %s %v", status, body, h)
	}
	status, body, h = call(t, "GET", srv.URL+"/chuspa/bueno", "", "X-Amz-Checksum-Mode", "ENABLED")
	if status != 200 || body != content || h.Get("x-amz-checksum-crc32") != crc32Of(content) || h.Get("Content-Encoding") != "" {
		t.Fatalf("lectura: HTTP %d %q %v", status, body, h)
	}

	// Tamaño declarado distinto del real.
	short := append([]string{}, headers...)
	short[len(short)-1] = "3"
	status, body, _ = call(t, "PUT", srv.URL+"/chuspa/corto", chunkedBody(crc32Of(content)), short...)
	wantStatus(t, "tamaño decodificado incorrecto", status, body, 400, "IncompleteBody")
}

func TestRangeAndConditionals(t *testing.T) {
	srv := newServer(t, false)
	call(t, "PUT", srv.URL+"/chuspa", "")
	_, _, h := call(t, "PUT", srv.URL+"/chuspa/k", "0123456789")
	etag := h.Get("ETag")

	for rng, want := range map[string]string{"bytes=0-3": "0123", "bytes=7-": "789", "bytes=-2": "89", "bytes=5-99": "56789"} {
		status, body, h := call(t, "GET", srv.URL+"/chuspa/k", "", "Range", rng)
		if status != 206 || body != want || !strings.HasPrefix(h.Get("Content-Range"), "bytes ") {
			t.Errorf("%s: HTTP %d %q %q", rng, status, body, h.Get("Content-Range"))
		}
	}
	status, body, h := call(t, "GET", srv.URL+"/chuspa/k", "", "Range", "bytes=10-")
	wantStatus(t, "rango fuera del objeto", status, body, 416, "InvalidRange")
	if h.Get("Content-Range") != "bytes */10" {
		t.Errorf("Content-Range = %q", h.Get("Content-Range"))
	}
	if status, body, _ := call(t, "GET", srv.URL+"/chuspa/k", "", "Range", "bytes=0-1,4-5"); status != 200 || body != "0123456789" {
		t.Errorf("varios rangos: HTTP %d %q", status, body)
	}
	if status, _, _ := call(t, "GET", srv.URL+"/chuspa/k", "", "If-None-Match", etag); status != 304 {
		t.Errorf("If-None-Match: HTTP %d", status)
	}
	status, body, _ = call(t, "GET", srv.URL+"/chuspa/k", "", "If-Match", `"otro"`)
	wantStatus(t, "If-Match", status, body, 412, "PreconditionFailed")
	if status, body, h := call(t, "HEAD", srv.URL+"/chuspa/k", ""); status != 200 || body != "" || h.Get("Content-Length") != "10" {
		t.Errorf("HEAD: HTTP %d %q %v", status, body, h)
	}
}

func TestNeverFakeSuccess(t *testing.T) {
	srv := newServer(t, false)
	call(t, "PUT", srv.URL+"/chuspa", "")
	call(t, "PUT", srv.URL+"/chuspa/k", "x")
	cases := []struct{ method, target string }{
		{"GET", "/chuspa?versioning"}, {"PUT", "/chuspa?acl"}, {"GET", "/chuspa?uploads&delimiter=/"},
		{"POST", "/chuspa"}, {"PUT", "/chuspa/k?tagging"}, {"PUT", "/chuspa?versioning"},
	}
	for _, tc := range cases {
		status, body, _ := call(t, tc.method, srv.URL+tc.target, "")
		wantStatus(t, tc.method+" "+tc.target, status, body, 501, "NotImplemented")
	}
	status, body, _ := call(t, "PUT", srv.URL+"/chuspa/copia", "", "X-Amz-Copy-Source", "/chuspa/k?versionId=1")
	wantStatus(t, "copia de una versión", status, body, 501, "NotImplemented")
	// Headers que piden algo que el motor no hace: nunca se ignoran en silencio.
	for _, h := range [][2]string{
		{"X-Amz-Acl", "public-read"}, {"X-Amz-Server-Side-Encryption", "AES256"}, {"X-Amz-Tagging", "a=b"},
		{"X-Amz-Storage-Class", "GLACIER"}, {"X-Amz-Object-Lock-Mode", "GOVERNANCE"},
	} {
		status, body, _ := call(t, "PUT", srv.URL+"/chuspa/con-header", "x", h[0], h[1])
		wantStatus(t, h[0], status, body, 501, "NotImplemented")
	}
	if status, _, _ := call(t, "HEAD", srv.URL+"/chuspa/con-header", ""); status != 404 {
		t.Fatalf("se guardó un objeto con un header no soportado: HTTP %d", status)
	}
	if status, body, _ := call(t, "PUT", srv.URL+"/chuspa/privado", "x", "X-Amz-Acl", "private"); status != 200 {
		t.Fatalf("acl private: HTTP %d %s", status, body)
	}
	status, body, _ = call(t, "GET", srv.URL+"/chuspa/k?tagging", "")
	if status != 200 || !strings.Contains(body, "<TagSet></TagSet>") {
		t.Fatalf("GetObjectTagging: HTTP %d %s", status, body)
	}
	status, body, _ = call(t, "GET", srv.URL+"/chuspa/no-existe?tagging", "")
	wantStatus(t, "tags de un objeto inexistente", status, body, 404, "NoSuchKey")
	status, body, _ = call(t, "PUT", srv.URL+"/chuspa/k", "", "X-Amz-Content-Sha256", "STREAMING-AWS4-ECDSA-P256-SHA256-PAYLOAD")
	wantStatus(t, "modo de cuerpo desconocido", status, body, 400, "InvalidRequest")
	status, body, _ = call(t, "PUT", srv.URL+"/chuspa/"+strings.Repeat("k", 1025), "x")
	wantStatus(t, "key larga", status, body, 400, "KeyTooLongError")
}

// signedRequest firma con el firmador oficial del SDK, a la hora indicada.
func signedRequest(t *testing.T, method, url string, body []byte, payloadHash string, at time.Time, headers ...string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	creds := aws.Credentials{AccessKeyID: testKey, SecretAccessKey: testSecret}
	err = v4.NewSigner().SignHTTP(context.Background(), creds, req, payloadHash, "s3", testRegion, at,
		func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestSignedRequests(t *testing.T) {
	srv := newServer(t, true)
	now := time.Now()
	empty := sha256Hex(nil)

	status, body, _ := call(t, "GET", srv.URL+"/", "")
	wantStatus(t, "sin firma", status, body, 403, "AccessDenied")

	status, body, _ = do(t, signedRequest(t, "PUT", srv.URL+"/chuspa", nil, empty, now))
	if status != 200 {
		t.Fatalf("crear chuspa firmada: HTTP %d %s", status, body)
	}
	status, body, _ = do(t, signedRequest(t, "GET", srv.URL+"/", nil, empty, now.Add(-16*time.Minute)))
	wantStatus(t, "fecha vieja", status, body, 403, "RequestTimeTooSkewed")

	content := []byte("la firma cubre este contenido")
	req := signedRequest(t, "PUT", srv.URL+"/chuspa/k", []byte("otro contenido del mismo largo"), sha256Hex(content), now)
	status, body, _ = do(t, req)
	wantStatus(t, "cuerpo alterado tras firmar", status, body, 400, "XAmzContentSHA256Mismatch")
	status, body, _ = do(t, signedRequest(t, "GET", srv.URL+"/chuspa/k", nil, empty, now))
	wantStatus(t, "objeto alterado no guardado", status, body, 404, "NoSuchKey")

	status, body, _ = do(t, signedRequest(t, "PUT", srv.URL+"/chuspa/k", content, sha256Hex(content), now))
	if status != 200 {
		t.Fatalf("subida firmada: HTTP %d %s", status, body)
	}
}

// Chunks firmados de punta a punta: la firma de cada chunk se encadena con la
// de la petición.
func TestSignedChunkedUpload(t *testing.T) {
	srv := newServer(t, true)
	now := time.Now().UTC()
	do(t, signedRequest(t, "PUT", srv.URL+"/chuspa", nil, sha256Hex(nil), now))

	content := []byte(strings.Repeat("salsa ", 1000))
	build := func(tamper bool) *http.Request {
		// Se firma primero con un cuerpo vacío para obtener la firma semilla.
		req := signedRequest(t, "PUT", srv.URL+"/chuspa/chunks", nil, auth.StreamingSigned, now,
			"Content-Encoding", "aws-chunked", "X-Amz-Decoded-Content-Length", fmt.Sprint(len(content)))
		authz, _ := auth.ParseAuthorization(req.Header.Get("Authorization"))
		date := now.Format("20060102")
		key := auth.DeriveKey(testSecret, date, testRegion, "s3")
		scope := date + "/" + testRegion + "/s3/aws4_request"
		amzDate := req.Header.Get("X-Amz-Date")

		var body bytes.Buffer
		prev := authz.Signature
		for _, chunk := range [][]byte{content[:4096], content[4096:], nil} {
			sts := strings.Join([]string{"AWS4-HMAC-SHA256-PAYLOAD", amzDate, scope, prev, sha256Hex(nil), sha256Hex(chunk)}, "\n")
			prev = hex.EncodeToString(hmacSHA256(key, sts))
			fmt.Fprintf(&body, "%x;chunk-signature=%s\r\n", len(chunk), prev)
			if tamper && len(chunk) > 0 {
				chunk = bytes.ToUpper(chunk)
			}
			body.Write(chunk)
			body.WriteString("\r\n")
		}
		req.Body = io.NopCloser(&body)
		req.ContentLength = int64(body.Len())
		return req
	}

	status, body, _ := do(t, build(true))
	wantStatus(t, "chunk alterado", status, body, 403, "SignatureDoesNotMatch")
	if status, body, _ := do(t, build(false)); status != 200 {
		t.Fatalf("chunks firmados: HTTP %d %s", status, body)
	}
	status, body, _ = do(t, signedRequest(t, "GET", srv.URL+"/chuspa/chunks", nil, sha256Hex(nil), now))
	if status != 200 || body != string(content) {
		t.Fatalf("lectura tras chunks firmados: HTTP %d, %d bytes", status, len(body))
	}
}

func TestParseRange(t *testing.T) {
	tests := []struct {
		header        string
		size          int64
		start, length int64
		ok, sat       bool
	}{
		{"", 10, 0, 0, false, true},
		{"bytes=0-0", 10, 0, 1, true, true},
		{"bytes=2-5", 10, 2, 4, true, true},
		{"bytes=5-", 10, 5, 5, true, true},
		{"bytes=-3", 10, 7, 3, true, true},
		{"bytes=-30", 10, 0, 10, true, true},
		{"bytes=9-100", 10, 9, 1, true, true},
		{"bytes=10-", 10, 0, 0, true, false},
		{"bytes=-0", 10, 0, 0, true, false},
		{"bytes=0-", 0, 0, 0, true, false},
		{"bytes=5-2", 10, 0, 0, false, true},
		{"bytes=a-b", 10, 0, 0, false, true},
		{"bytes=0-1,3-4", 10, 0, 0, false, true},
		{"lines=0-1", 10, 0, 0, false, true},
		{"bytes=99999999999999999999-", 10, 0, 0, false, true},
	}
	for _, tt := range tests {
		start, length, ok, sat := parseRange(tt.header, tt.size)
		if start != tt.start || length != tt.length || ok != tt.ok || sat != tt.sat {
			t.Errorf("parseRange(%q, %d) = %d, %d, %v, %v", tt.header, tt.size, start, length, ok, sat)
		}
	}
}

func FuzzParseRange(f *testing.F) {
	f.Add("bytes=0-9", int64(10))
	f.Add("bytes=-5", int64(3))
	f.Add("bytes=7-", int64(0))
	f.Fuzz(func(t *testing.T, header string, size int64) {
		if size < 0 {
			return
		}
		start, length, ok, sat := parseRange(header, size)
		if ok && sat && (start < 0 || length <= 0 || start+length > size) {
			t.Fatalf("parseRange(%q, %d) = %d, %d fuera del objeto", header, size, start, length)
		}
	})
}

func TestContinuationToken(t *testing.T) {
	for _, tc := range []struct {
		last     string
		isPrefix bool
	}{{"a/b", false}, {"fotos/", true}, {"ñ\x00raro", false}} {
		last, isPrefix, ok := decodeToken(encodeToken(tc.last, tc.isPrefix))
		if !ok || last != tc.last || isPrefix != tc.isPrefix {
			t.Errorf("token de %q: %q %v %v", tc.last, last, isPrefix, ok)
		}
	}
	for _, bad := range []string{"", "!!!", "eA", "eGFiYw"} {
		if _, _, ok := decodeToken(bad); ok {
			t.Errorf("se aceptó el token inválido %q", bad)
		}
	}
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func TestCopyObject(t *testing.T) {
	srv := newServer(t, false)
	call(t, "PUT", srv.URL+"/origen", "")
	call(t, "PUT", srv.URL+"/destino", "")
	_, _, h := call(t, "PUT", srv.URL+"/origen/con espacio.txt", "contenido", "Content-Type", "text/plain", "X-Amz-Meta-Barrio", "Granada")
	etag := h.Get("ETag")

	status, body, _ := call(t, "PUT", srv.URL+"/destino/copia.txt", "", "X-Amz-Copy-Source", "/origen/con%20espacio.txt")
	if status != 200 || !strings.Contains(body, "<CopyObjectResult") || !strings.Contains(body, strings.Trim(etag, `"`)) {
		t.Fatalf("copia: HTTP %d %s", status, body)
	}
	status, body, h = call(t, "GET", srv.URL+"/destino/copia.txt", "")
	if status != 200 || body != "contenido" || h.Get("Content-Type") != "text/plain" || h.Get("x-amz-meta-barrio") != "Granada" {
		t.Fatalf("lectura de la copia: HTTP %d %q %v", status, body, h)
	}

	call(t, "PUT", srv.URL+"/destino/nueva.txt", "", "X-Amz-Copy-Source", "origen/con%20espacio.txt",
		"X-Amz-Metadata-Directive", "REPLACE", "Content-Type", "text/markdown")
	if _, _, h := call(t, "HEAD", srv.URL+"/destino/nueva.txt", ""); h.Get("Content-Type") != "text/markdown" || h.Get("x-amz-meta-barrio") != "" {
		t.Fatalf("REPLACE no cambió los metadatos: %v", h)
	}

	status, body, _ = call(t, "PUT", srv.URL+"/origen/con espacio.txt", "", "X-Amz-Copy-Source", "/origen/con%20espacio.txt")
	wantStatus(t, "copia sobre sí mismo", status, body, 400, "InvalidRequest")
	status, body, _ = call(t, "PUT", srv.URL+"/destino/x", "", "X-Amz-Copy-Source", "/origen/no-existe")
	wantStatus(t, "origen inexistente", status, body, 404, "NoSuchKey")
	status, body, _ = call(t, "PUT", srv.URL+"/destino/x", "", "X-Amz-Copy-Source", "/origen/con%20espacio.txt", "X-Amz-Copy-Source-If-Match", `"otro"`)
	wantStatus(t, "condición de copia", status, body, 412, "PreconditionFailed")
}

func TestDeleteObjects(t *testing.T) {
	srv := newServer(t, false)
	call(t, "PUT", srv.URL+"/chuspa", "")
	for _, k := range []string{"a", "b", "c"} {
		call(t, "PUT", srv.URL+"/chuspa/"+k, "x")
	}
	payload := "<Delete><Object><Key>a</Key></Object><Object><Key>no-existe</Key></Object>" +
		"<Object><Key>b</Key><VersionId>v1</VersionId></Object></Delete>"

	status, body, _ := call(t, "POST", srv.URL+"/chuspa?delete", payload)
	wantStatus(t, "sin suma de integridad", status, body, 400, "InvalidRequest")
	status, body, _ = call(t, "POST", srv.URL+"/chuspa?delete", payload, "X-Amz-Checksum-Crc32", "AAAAAA==")
	wantStatus(t, "suma incorrecta", status, body, 400, "BadDigest")

	status, body, _ = call(t, "POST", srv.URL+"/chuspa?delete", payload, "X-Amz-Checksum-Crc32", crc32Of(payload))
	if status != 200 || !strings.Contains(body, "<Deleted><Key>a</Key></Deleted>") ||
		!strings.Contains(body, "<Deleted><Key>no-existe</Key></Deleted>") ||
		!strings.Contains(body, "<Error><Key>b</Key><Code>NoSuchVersion</Code>") {
		t.Fatalf("borrado masivo: HTTP %d %s", status, body)
	}
	if status, _, _ := call(t, "HEAD", srv.URL+"/chuspa/a", ""); status != 404 {
		t.Fatalf("a sigue existiendo: HTTP %d", status)
	}
	if status, _, _ := call(t, "HEAD", srv.URL+"/chuspa/b", ""); status != 200 {
		t.Fatalf("b no debía borrarse: HTTP %d", status)
	}
	status, body, _ = call(t, "POST", srv.URL+"/chuspa?delete", "<Delete></Delete>", "X-Amz-Checksum-Crc32", crc32Of("<Delete></Delete>"))
	wantStatus(t, "lista vacía", status, body, 400, "MalformedXML")
}

func TestListObjectsV1(t *testing.T) {
	srv := newServer(t, false)
	call(t, "PUT", srv.URL+"/chuspa", "")
	for _, k := range []string{"a.txt", "fotos/1.jpg", "fotos/2.jpg", "z.txt"} {
		call(t, "PUT", srv.URL+"/chuspa/"+k, "x")
	}
	// Con delimitador y de a un elemento: el NextMarker "fotos/" no debe repetirse.
	var seen []string
	marker := ""
	for i := 0; i < 6; i++ {
		_, body, _ := call(t, "GET", srv.URL+"/chuspa?delimiter=/&max-keys=1&marker="+marker, "")
		var res struct {
			IsTruncated bool
			NextMarker  string
			Contents    []struct{ Key string }
			Prefixes    []struct{ Prefix string } `xml:"CommonPrefixes"`
		}
		if err := xml.Unmarshal([]byte(body), &res); err != nil {
			t.Fatal(err, body)
		}
		for _, c := range res.Contents {
			seen = append(seen, c.Key)
		}
		for _, p := range res.Prefixes {
			seen = append(seen, p.Prefix)
		}
		if !res.IsTruncated {
			break
		}
		marker = res.NextMarker
	}
	if got := strings.Join(seen, " "); got != "a.txt fotos/ z.txt" {
		t.Fatalf("listado v1 paginado: %q", got)
	}
}

func TestListMultipartUploads(t *testing.T) {
	srv := newServer(t, false)
	call(t, "PUT", srv.URL+"/chuspa", "")
	for _, k := range []string{"b.bin", "a.bin", "otra/c.bin"} {
		if status, body, _ := call(t, "POST", srv.URL+"/chuspa/"+k+"?uploads", ""); status != 200 {
			t.Fatal(status, body)
		}
	}
	_, body, _ := call(t, "GET", srv.URL+"/chuspa?uploads", "")
	a, b, c := strings.Index(body, "<Key>a.bin</Key>"), strings.Index(body, "<Key>b.bin</Key>"), strings.Index(body, "<Key>otra/c.bin</Key>")
	if a < 0 || b < a || c < b {
		t.Fatalf("subidas en curso fuera de orden: %s", body)
	}
	_, body, _ = call(t, "GET", srv.URL+"/chuspa?uploads&prefix=otra/", "")
	if strings.Contains(body, "a.bin") || !strings.Contains(body, "otra/c.bin") {
		t.Fatalf("filtro por prefijo: %s", body)
	}
	_, body, _ = call(t, "GET", srv.URL+"/chuspa?uploads&max-uploads=1", "")
	if !strings.Contains(body, "<IsTruncated>true</IsTruncated>") || !strings.Contains(body, "<NextKeyMarker>a.bin</NextKeyMarker>") {
		t.Fatalf("paginación: %s", body)
	}
	_, body, _ = call(t, "GET", srv.URL+"/chuspa?uploads&key-marker=a.bin", "")
	if strings.Contains(body, "<Key>a.bin</Key>") || !strings.Contains(body, "<Key>b.bin</Key>") {
		t.Fatalf("key-marker: %s", body)
	}
}

// Un cliente que deja de enviar el cuerpo no puede retener la conexión.
func TestBodyIdleTimeout(t *testing.T) {
	backend, _, err := fsbolt.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	api := &Server{Backend: backend, Region: testRegion, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), BodyIdleTimeout: 200 * time.Millisecond}
	srv := httptest.NewServer(api)
	defer func() { srv.Close(); backend.Close() }()
	call(t, "PUT", srv.URL+"/chuspa", "")

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "PUT /chuspa/lento HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nsolo un pedazo")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply, _ := io.ReadAll(conn)
	if !strings.Contains(string(reply), "RequestTimeout") {
		t.Fatalf("se esperaba RequestTimeout, se obtuvo: %q", reply)
	}
	if status, _, _ := call(t, "HEAD", srv.URL+"/chuspa/lento", ""); status != 404 {
		t.Fatalf("quedó un objeto a medias: HTTP %d", status)
	}
}

func TestUploadPartCopy(t *testing.T) {
	srv := newServer(t, false)
	call(t, "PUT", srv.URL+"/chuspa", "")
	call(t, "PUT", srv.URL+"/chuspa/origen", "0123456789")
	_, body, _ := call(t, "POST", srv.URL+"/chuspa/destino?uploads", "")
	var init struct{ UploadId string }
	if err := xml.Unmarshal([]byte(body), &init); err != nil || init.UploadId == "" {
		t.Fatal(err, body)
	}
	target := srv.URL + "/chuspa/destino?uploadId=" + init.UploadId + "&partNumber="

	status, body, _ := call(t, "PUT", target+"1", "", "X-Amz-Copy-Source", "/chuspa/origen", "X-Amz-Copy-Source-Range", "bytes=20-30")
	wantStatus(t, "rango fuera del origen", status, body, 416, "InvalidRange")
	status, body, _ = call(t, "PUT", target+"1", "", "X-Amz-Copy-Source", "/chuspa/origen", "X-Amz-Copy-Source-Range", "bytes=5-")
	wantStatus(t, "rango sin fin", status, body, 400, "InvalidArgument")

	var etags []string
	for i, rng := range []string{"bytes=6-9", "bytes=0-2"} {
		status, body, _ := call(t, "PUT", target+fmt.Sprint(i+1), "", "X-Amz-Copy-Source", "/chuspa/origen", "X-Amz-Copy-Source-Range", rng)
		var res struct{ ETag string }
		if err := xml.Unmarshal([]byte(body), &res); status != 200 || err != nil || res.ETag == "" {
			t.Fatalf("copia de parte %s: HTTP %d %s", rng, status, body)
		}
		etags = append(etags, res.ETag)
	}
	// La primera parte mide menos de 5 MiB: completar debe fallar sin publicar nada.
	complete := fmt.Sprintf("<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part>"+
		"<Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>", etags[0], etags[1])
	status, body, _ = call(t, "POST", srv.URL+"/chuspa/destino?uploadId="+init.UploadId, complete)
	wantStatus(t, "parte intermedia pequeña", status, body, 400, "EntityTooSmall")

	one := fmt.Sprintf("<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>", etags[0])
	if status, body, _ := call(t, "POST", srv.URL+"/chuspa/destino?uploadId="+init.UploadId, one); status != 200 {
		t.Fatalf("completar: HTTP %d %s", status, body)
	}
	if _, body, _ := call(t, "GET", srv.URL+"/chuspa/destino", ""); body != "6789" {
		t.Fatalf("contenido copiado por rango: %q", body)
	}
}

// Sin versionado, cada objeto es su única versión "null".
func TestUnversionedVersionsView(t *testing.T) {
	srv := newServer(t, false)
	call(t, "PUT", srv.URL+"/chuspa", "")
	call(t, "PUT", srv.URL+"/chuspa/a", "x")
	call(t, "PUT", srv.URL+"/chuspa/b", "x")

	status, body, _ := call(t, "GET", srv.URL+"/chuspa?versions&max-keys=1", "")
	if status != 200 || !strings.Contains(body, "<Key>a</Key><VersionId>null</VersionId><IsLatest>true</IsLatest>") ||
		!strings.Contains(body, "<NextKeyMarker>a</NextKeyMarker>") || strings.Contains(body, "<Key>b</Key>") {
		t.Fatalf("listado de versiones: HTTP %d %s", status, body)
	}
	_, body, _ = call(t, "GET", srv.URL+"/chuspa?versions&key-marker=a&version-id-marker=null", "")
	if strings.Contains(body, "<Key>a</Key>") || !strings.Contains(body, "<Key>b</Key>") {
		t.Fatalf("segunda página: %s", body)
	}
	if status, body, _ := call(t, "GET", srv.URL+"/chuspa/a?versionId=null", ""); status != 200 || body != "x" {
		t.Fatalf("versión null: HTTP %d %q", status, body)
	}
	status, body, _ = call(t, "GET", srv.URL+"/chuspa/a?versionId=abc", "")
	wantStatus(t, "versión inexistente", status, body, 404, "NoSuchVersion")
	status, body, _ = call(t, "DELETE", srv.URL+"/chuspa/a?versionId=abc", "")
	wantStatus(t, "borrar versión inexistente", status, body, 404, "NoSuchVersion")
	if status, _, _ := call(t, "DELETE", srv.URL+"/chuspa/a?versionId=null", ""); status != 204 {
		t.Fatalf("borrar versión null: HTTP %d", status)
	}
}

func TestConditionalWrites(t *testing.T) {
	srv := newServer(t, false)
	call(t, "PUT", srv.URL+"/chuspa", "")

	status, body, _ := call(t, "PUT", srv.URL+"/chuspa/k", "uno", "If-Match", `"cualquiera"`)
	wantStatus(t, "If-Match sin objeto", status, body, 404, "NoSuchKey")
	status, _, h := call(t, "PUT", srv.URL+"/chuspa/k", "uno", "If-None-Match", "*")
	if status != 200 {
		t.Fatalf("crear si no existe: HTTP %d", status)
	}
	etag := h.Get("ETag")
	status, body, _ = call(t, "PUT", srv.URL+"/chuspa/k", "dos", "If-None-Match", "*")
	wantStatus(t, "crear sobre uno existente", status, body, 412, "PreconditionFailed")
	status, body, _ = call(t, "PUT", srv.URL+"/chuspa/k", "dos", "If-Match", `"otro"`)
	wantStatus(t, "reemplazar otra versión", status, body, 412, "PreconditionFailed")
	if _, body, _ := call(t, "GET", srv.URL+"/chuspa/k", ""); body != "uno" {
		t.Fatalf("una escritura rechazada cambió el objeto: %q", body)
	}
	if status, _, _ := call(t, "PUT", srv.URL+"/chuspa/k", "dos", "If-Match", etag); status != 200 {
		t.Fatalf("reemplazar la versión esperada: HTTP %d", status)
	}

	status, body, _ = call(t, "DELETE", srv.URL+"/chuspa/k", "", "If-Match", etag)
	wantStatus(t, "borrar con ETag viejo", status, body, 412, "PreconditionFailed")
	status, body, _ = call(t, "DELETE", srv.URL+"/chuspa/k", "", "X-Amz-If-Match-Size", "3")
	wantStatus(t, "condición por tamaño", status, body, 501, "NotImplemented")
	if status, _, _ := call(t, "HEAD", srv.URL+"/chuspa/k", ""); status != 200 {
		t.Fatalf("un borrado rechazado borró el objeto: HTTP %d", status)
	}
}

func TestGetConditionsAndOverrides(t *testing.T) {
	srv := newServer(t, false)
	call(t, "PUT", srv.URL+"/chuspa", "")
	call(t, "PUT", srv.URL+"/chuspa/k", "x", "Expires", "Thu, 01 Dec 2033 16:00:00 GMT", "Content-Encoding", "deflate, gzip")
	past, future := "Sat, 01 Jan 2000 00:00:00 GMT", "Fri, 01 Jan 2100 00:00:00 GMT"

	if status, _, _ := call(t, "GET", srv.URL+"/chuspa/k", "", "If-Modified-Since", future); status != 304 {
		t.Errorf("If-Modified-Since futuro: HTTP %d", status)
	}
	if status, _, _ := call(t, "GET", srv.URL+"/chuspa/k", "", "If-Modified-Since", past); status != 200 {
		t.Errorf("If-Modified-Since pasado: HTTP %d", status)
	}
	status, body, _ := call(t, "GET", srv.URL+"/chuspa/k", "", "If-Unmodified-Since", past)
	wantStatus(t, "If-Unmodified-Since pasado", status, body, 412, "PreconditionFailed")

	_, _, h := call(t, "GET", srv.URL+"/chuspa/k?response-content-type=foo/bar&response-content-disposition=attachment", "")
	if h.Get("Content-Type") != "foo/bar" || h.Get("Content-Disposition") != "attachment" ||
		h.Get("Expires") != "Thu, 01 Dec 2033 16:00:00 GMT" || h.Get("Content-Encoding") != "deflate, gzip" {
		t.Errorf("headers de respuesta: %v", h)
	}
	status, body, _ = call(t, "GET", srv.URL+"/chuspa/k?partNumber=1", "")
	wantStatus(t, "lectura por parte", status, body, 501, "NotImplemented")
}

// En CompleteMultipartUpload, x-amz-checksum-* es el checksum del objeto.
func TestCompositeChecksumAndIdempotentComplete(t *testing.T) {
	srv := newServer(t, false)
	call(t, "PUT", srv.URL+"/chuspa", "")
	_, body, _ := call(t, "POST", srv.URL+"/chuspa/k?uploads", "", "X-Amz-Checksum-Algorithm", "SHA256")
	var init struct{ UploadId string }
	xml.Unmarshal([]byte(body), &init)
	content := strings.Repeat("A", 1024)
	_, _, h := call(t, "PUT", srv.URL+"/chuspa/k?partNumber=1&uploadId="+init.UploadId, content,
		"X-Amz-Checksum-Sha256", "arcu6553sHVAiX4MjW0j7I7vD4w6R+Gz9Ok0Q9lTa+0=")
	complete := "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>" + h.Get("ETag") + "</ETag></Part></CompleteMultipartUpload>"
	target := srv.URL + "/chuspa/k?uploadId=" + init.UploadId

	status, body, _ := call(t, "POST", target, complete, "X-Amz-Checksum-Sha256", "bad")
	wantStatus(t, "checksum compuesto incorrecto", status, body, 400, "BadDigest")
	// Valor conocido de ceph/s3-tests para este contenido.
	const composite = "Ok6Cs5b96ux6+MWQkJO7UBT5sKPBeXBLwvj/hK89smg=-1"
	status, first, _ := call(t, "POST", target, complete, "X-Amz-Checksum-Sha256", composite)
	if status != 200 || !strings.Contains(first, "<ChecksumSHA256>"+composite+"</ChecksumSHA256>") {
		t.Fatalf("completar: HTTP %d %s", status, first)
	}
	status, again, _ := call(t, "POST", target, complete, "X-Amz-Checksum-Sha256", composite)
	if status != 200 || again != first {
		t.Fatalf("reintento de completar: HTTP %d %s", status, again)
	}
	status, body, _ = call(t, "POST", srv.URL+"/chuspa/k?uploadId=no-existe", complete)
	wantStatus(t, "subida inexistente", status, body, 404, "NoSuchUpload")
}

func TestListBucketsPagination(t *testing.T) {
	srv := newServer(t, false)
	for _, name := range []string{"chuspa-c", "chuspa-a", "chuspa-b", "otra"} {
		call(t, "PUT", srv.URL+"/"+name, "")
	}
	var seen []string
	token := ""
	for i := 0; i < 5; i++ {
		_, body, _ := call(t, "GET", srv.URL+"/?max-buckets=1&prefix=chuspa-&continuation-token="+token, "")
		var res struct {
			Buckets           []struct{ Name string } `xml:"Buckets>Bucket"`
			ContinuationToken string
		}
		if err := xml.Unmarshal([]byte(body), &res); err != nil {
			t.Fatal(err, body)
		}
		for _, b := range res.Buckets {
			seen = append(seen, b.Name)
		}
		if token = res.ContinuationToken; token == "" {
			break
		}
	}
	if got := strings.Join(seen, " "); got != "chuspa-a chuspa-b chuspa-c" {
		t.Fatalf("chuspas paginadas: %q", got)
	}
}

func TestUnknownLengthUpload(t *testing.T) {
	srv := newServer(t, false)
	call(t, "PUT", srv.URL+"/chuspa", "")
	// io.MultiReader oculta el tamaño: net/http usa Transfer-Encoding: chunked.
	req, _ := http.NewRequest("PUT", srv.URL+"/chuspa/k", io.MultiReader(strings.NewReader("sin "), strings.NewReader("tamaño")))
	if status, body, _ := do(t, req); status != 200 {
		t.Fatalf("subida sin Content-Length: HTTP %d %s", status, body)
	}
	if _, body, _ := call(t, "GET", srv.URL+"/chuspa/k", ""); body != "sin tamaño" {
		t.Fatalf("contenido: %q", body)
	}
}
