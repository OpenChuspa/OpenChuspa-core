// Package auth verifica la firma AWS Signature Version 4 de las peticiones.
//
// La petición canónica se reconstruye exactamente con los headers que el
// cliente declaró en SignedHeaders. Las pruebas usan el firmador oficial de
// aws-sdk-go-v2 como oráculo (ver docs/adr/0001-verificacion-sigv4.md).
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OpenChuspa/OpenChuspa-core/internal/chunked"
)

const (
	algorithm  = "AWS4-HMAC-SHA256"
	timeFormat = "20060102T150405Z"
	service    = "s3"
	terminator = "aws4_request"

	// MaxSkew es la diferencia máxima entre x-amz-date y el reloj del servidor.
	MaxSkew = 15 * time.Minute
)

var (
	// ErrMissing: la petición no trae firma.
	ErrMissing = errors.New("auth: petición sin firma")
	// ErrSignature: la firma no coincide o no se puede evaluar. Es el mismo
	// error para una llave desconocida, un secreto incorrecto o un header mal
	// formado, para no revelar cuál fue el motivo.
	ErrSignature = errors.New("auth: la firma no coincide")
	// ErrSkew: x-amz-date está fuera de la ventana permitida.
	ErrSkew = errors.New("auth: fecha fuera de la ventana permitida")
	// ErrUnsupported: mecanismo de autenticación que el motor aún no soporta.
	ErrUnsupported = errors.New("auth: mecanismo de firma no soportado")
)

// Verifier verifica peticiones contra una única credencial raíz.
type Verifier struct {
	AccessKey string
	SecretKey string
	Region    string
	Now       func() time.Time // reloj inyectable para pruebas; por defecto time.Now
}

// Identity es el resultado de una verificación exitosa.
type Identity struct {
	// Signing permite verificar las firmas de chunk de un cuerpo en streaming.
	Signing *chunked.Signing
}

// Authorization es el header Authorization ya analizado.
type Authorization struct {
	AccessKey     string
	Date          string // fecha del scope (20060102)
	Region        string
	Service       string
	SignedHeaders []string
	Signature     string
}

// ParseAuthorization analiza el header de forma estricta: cualquier
// desviación del formato es un error.
func ParseAuthorization(h string) (Authorization, error) {
	var a Authorization
	rest, ok := strings.CutPrefix(h, algorithm+" ")
	if !ok || len(h) > 4096 {
		return a, ErrSignature
	}
	seen := map[string]bool{}
	for _, field := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok || v == "" || seen[k] {
			return a, ErrSignature
		}
		seen[k] = true
		switch k {
		case "Credential":
			parts := strings.Split(v, "/")
			if len(parts) != 5 || parts[4] != terminator || parts[0] == "" {
				return a, ErrSignature
			}
			a.AccessKey, a.Date, a.Region, a.Service = parts[0], parts[1], parts[2], parts[3]
		case "SignedHeaders":
			a.SignedHeaders = strings.Split(v, ";")
			for i, name := range a.SignedHeaders {
				if name == "" || name != strings.ToLower(name) || strings.ContainsAny(name, " \t:") {
					return a, ErrSignature
				}
				if i > 0 && a.SignedHeaders[i-1] >= name {
					return a, ErrSignature // deben venir ordenados y sin repetir
				}
			}
		case "Signature":
			if !isHex64(v) {
				return a, ErrSignature
			}
			a.Signature = v
		default:
			return a, ErrSignature
		}
	}
	if len(seen) != 3 {
		return a, ErrSignature
	}
	return a, nil
}

// Verify comprueba la firma de la petición. No lee el cuerpo: su integridad
// se valida después, según el valor de x-amz-content-sha256.
func (v *Verifier) Verify(r *http.Request) (*Identity, error) {
	if r.URL.Query().Has("X-Amz-Signature") || r.URL.Query().Has("Signature") {
		return nil, ErrUnsupported // URLs prefirmadas: fase 2
	}
	header := r.Header.Get("Authorization")
	if header == "" {
		return nil, ErrMissing
	}
	if strings.HasPrefix(header, "AWS ") {
		return nil, ErrUnsupported // SigV2
	}
	a, err := ParseAuthorization(header)
	if err != nil {
		return nil, err
	}

	amzDate := r.Header.Get("X-Amz-Date")
	t, err := time.Parse(timeFormat, amzDate)
	if err != nil {
		return nil, ErrSignature
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	if d := now().Sub(t); d > MaxSkew || d < -MaxSkew {
		return nil, ErrSkew
	}

	for _, required := range []string{"host", "x-amz-content-sha256", "x-amz-date"} {
		if !contains(a.SignedHeaders, required) {
			return nil, ErrSignature
		}
	}
	canonical, ok := canonicalRequest(r, a.SignedHeaders)
	if !ok {
		return nil, ErrSignature
	}

	scope := strings.Join([]string{a.Date, a.Region, a.Service, terminator}, "/")
	sum := sha256.Sum256([]byte(canonical))
	stringToSign := strings.Join([]string{algorithm, amzDate, scope, hex.EncodeToString(sum[:])}, "\n")

	// La firma se calcula siempre con el secreto configurado, también cuando
	// la llave o el scope no coinciden, para que el tiempo de respuesta no
	// dependa del motivo del rechazo.
	key := DeriveKey(v.SecretKey, a.Date, a.Region, a.Service)
	want := hex.EncodeToString(hmacSHA256(key, stringToSign))

	valid := subtle.ConstantTimeCompare([]byte(want), []byte(a.Signature))
	valid &= subtle.ConstantTimeCompare([]byte(a.AccessKey), []byte(v.AccessKey))
	valid &= subtle.ConstantTimeCompare([]byte(a.Region), []byte(v.Region))
	valid &= subtle.ConstantTimeCompare([]byte(a.Service), []byte(service))
	valid &= subtle.ConstantTimeCompare([]byte(a.Date), []byte(t.Format("20060102")))
	if valid != 1 {
		return nil, ErrSignature
	}
	return &Identity{Signing: &chunked.Signing{Key: key, Date: amzDate, Scope: scope, Seed: a.Signature}}, nil
}

// DeriveKey deriva la llave de firma de SigV4.
func DeriveKey(secret, date, region, service string) []byte {
	key := hmacSHA256([]byte("AWS4"+secret), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	return hmacSHA256(key, terminator)
}

func canonicalRequest(r *http.Request, signedHeaders []string) (string, bool) {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	path, ok := canonicalPath(r.URL)
	if !ok {
		return "", false
	}
	b.WriteString(path)
	b.WriteByte('\n')
	query, ok := canonicalQuery(r.URL.RawQuery)
	if !ok {
		return "", false
	}
	b.WriteString(query)
	b.WriteByte('\n')
	for _, name := range signedHeaders {
		var values []string
		switch name {
		case "host":
			values = []string{r.Host}
		case "content-length":
			// net/http lo saca del mapa de headers.
			if cl := r.Header.Get("Content-Length"); cl != "" {
				values = []string{cl}
			} else if r.ContentLength >= 0 {
				values = []string{strconv.FormatInt(r.ContentLength, 10)}
			}
		default:
			values = r.Header.Values(name)
		}
		if len(values) == 0 {
			return "", false // header firmado que no llegó
		}
		b.WriteString(name)
		b.WriteByte(':')
		for i, val := range values {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strings.Join(strings.Fields(val), " "))
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(strings.Join(signedHeaders, ";"))
	b.WriteByte('\n')
	b.WriteString(r.Header.Get("X-Amz-Content-Sha256"))
	return b.String(), true
}

// canonicalPath recodifica cada segmento de la ruta tal como llegó, de modo
// que un %2F enviado por el cliente siga siendo %2F y no un separador.
func canonicalPath(u *url.URL) (string, bool) {
	segments := strings.Split(u.EscapedPath(), "/")
	for i, seg := range segments {
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			return "", false
		}
		segments[i] = URIEncode(decoded, true)
	}
	if path := strings.Join(segments, "/"); path != "" {
		return path, true
	}
	return "/", true
}

func canonicalQuery(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	type pair struct{ k, v string }
	var pairs []pair
	for _, field := range strings.Split(raw, "&") {
		if field == "" {
			continue
		}
		k, v, _ := strings.Cut(field, "=")
		dk, err1 := url.QueryUnescape(k)
		dv, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil {
			return "", false
		}
		pairs = append(pairs, pair{URIEncode(dk, true), URIEncode(dv, true)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k + "=" + p.v)
	}
	return b.String(), true
}

// URIEncode aplica la codificación de SigV4: todo byte fuera de los no
// reservados (A-Z a-z 0-9 - _ . ~) se escribe como %XX en mayúsculas.
func URIEncode(s string, encodeSlash bool) string {
	const upperHex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0xf])
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
