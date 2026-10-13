package s3api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
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
		{"GET", "/chuspa?versioning"}, {"PUT", "/chuspa?acl"}, {"GET", "/chuspa"}, {"GET", "/chuspa?uploads"},
		{"POST", "/chuspa?delete"}, {"PUT", "/chuspa/k?tagging"}, {"GET", "/chuspa/k?versionId=1"},
	}
	for _, tc := range cases {
		status, body, _ := call(t, tc.method, srv.URL+tc.target, "")
		wantStatus(t, tc.method+" "+tc.target, status, body, 501, "NotImplemented")
	}
	status, body, _ := call(t, "PUT", srv.URL+"/chuspa/copia", "", "X-Amz-Copy-Source", "/chuspa/k")
	wantStatus(t, "CopyObject", status, body, 501, "NotImplemented")
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
