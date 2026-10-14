// Package config lee y valida la configuración del motor desde variables de
// entorno. La regla es fallar cerrado: ante una configuración incompleta o
// insegura el servidor no arranca.
package config

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultAddr    = "127.0.0.1:9000"
	DefaultDataDir = "/data"
	DefaultRegion  = "cali-san-antonio-1"

	// DefaultMultipartExpiry es la edad a la que una subida multipart sin
	// completar se considera abandonada.
	DefaultMultipartExpiry = 7 * 24 * time.Hour

	// MinSecretLen es el largo mínimo del secreto raíz.
	MinSecretLen = 32

	// DefaultMinFreeMB es el espacio libre en disco por debajo del cual se
	// rechazan escrituras.
	DefaultMinFreeMB = 1024

	// DefaultScrubInterval es cada cuánto se relee todo el contenido para
	// detectar daños en disco.
	DefaultScrubInterval = 7 * 24 * time.Hour

	// DefaultAuthMaxFailures son las autenticaciones fallidas por minuto que
	// bloquean una IP durante 5 minutos.
	DefaultAuthMaxFailures = 20
)

// Credenciales que aparecen en la documentación y en la landing. Nunca se
// aceptan fuera de loopback.
var docCredentials = map[string]bool{
	"clave-del-parchao":       true,
	"la-secreta-de-la-cuadra": true,
}

var (
	accessKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{3,128}$`)
	regionRe    = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
)

// Config es la configuración ya validada.
type Config struct {
	Addr         string
	DataDir      string
	RootUser     string
	RootPassword string
	// PreviousRootPassword se acepta además de RootPassword mientras se rota.
	PreviousRootPassword string
	// TLSCertFile y TLSKeyFile activan TLS nativo; el certificado se relee
	// cuando cambia en disco.
	TLSCertFile    string
	TLSKeyFile     string
	Region         string
	AllowAnonymous bool
	LogFormat      string
	// MultipartExpiry en cero desactiva la expiración.
	MultipartExpiry time.Duration
	// MinFreeBytes en cero desactiva la comprobación de espacio libre.
	MinFreeBytes int64
	// ScrubInterval en cero desactiva la verificación periódica.
	ScrubInterval time.Duration
	// AuthMaxFailures es cuántas autenticaciones fallidas por minuto bloquean
	// una IP; en cero no se bloquea.
	AuthMaxFailures int
}

// Loopback indica si el servidor escucha solo en la interfaz local.
func (c Config) Loopback() bool { return isLoopback(c.Addr) }

// Load construye la configuración a partir de getenv (normalmente os.Getenv).
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		Addr:      orDefault(getenv("CHUSPA_ADDR"), DefaultAddr),
		DataDir:   orDefault(getenv("CHUSPA_DATA_DIR"), DefaultDataDir),
		Region:    orDefault(getenv("CHUSPA_REGION"), DefaultRegion),
		LogFormat: orDefault(getenv("CHUSPA_LOG_FORMAT"), "json"),
		RootUser:  getenv("CHUSPA_ROOT_USER"),

		MultipartExpiry: DefaultMultipartExpiry,
		MinFreeBytes:    DefaultMinFreeMB << 20,
		ScrubInterval:   DefaultScrubInterval,
		AuthMaxFailures: DefaultAuthMaxFailures,
	}
	if v := getenv("CHUSPA_AUTH_MAX_FAILURES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("CHUSPA_AUTH_MAX_FAILURES inválido (%q): use un número, o 0 para desactivar el bloqueo", v)
		}
		c.AuthMaxFailures = n
	}
	for _, d := range []struct {
		name string
		dst  *time.Duration
	}{{"CHUSPA_MULTIPART_EXPIRY", &c.MultipartExpiry}, {"CHUSPA_SCRUB_INTERVAL", &c.ScrubInterval}} {
		if v := getenv(d.name); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil || parsed < 0 {
				return Config{}, fmt.Errorf("%s inválida (%q): use una duración como 168h, o 0 para desactivarla", d.name, v)
			}
			*d.dst = parsed
		}
	}
	if v := getenv("CHUSPA_MIN_FREE_MB"); v != "" {
		mb, err := strconv.ParseInt(v, 10, 64)
		if err != nil || mb < 0 || mb > 1<<40 {
			return Config{}, fmt.Errorf("CHUSPA_MIN_FREE_MB inválido (%q): use un número de MiB, o 0 para desactivarlo", v)
		}
		c.MinFreeBytes = mb << 20
	}

	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		return Config{}, fmt.Errorf("CHUSPA_ADDR inválida (%q): se espera host:puerto", c.Addr)
	}
	if !regionRe.MatchString(c.Region) {
		return Config{}, fmt.Errorf("CHUSPA_REGION inválida (%q): solo minúsculas, dígitos y guiones", c.Region)
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		return Config{}, fmt.Errorf("CHUSPA_LOG_FORMAT inválido (%q): use json o text", c.LogFormat)
	}

	switch v := strings.ToLower(getenv("CHUSPA_ALLOW_ANONYMOUS")); v {
	case "", "false", "0":
	case "true", "1":
		c.AllowAnonymous = true
	default:
		return Config{}, fmt.Errorf("CHUSPA_ALLOW_ANONYMOUS inválido (%q): use true o false", v)
	}

	secret, err := readSecret(getenv, "CHUSPA_ROOT_PASSWORD")
	if err != nil {
		return Config{}, err
	}
	c.RootPassword = secret
	if c.PreviousRootPassword, err = readSecret(getenv, "CHUSPA_ROOT_PASSWORD_PREVIOUS"); err != nil {
		return Config{}, err
	}

	c.TLSCertFile, c.TLSKeyFile = getenv("CHUSPA_TLS_CERT_FILE"), getenv("CHUSPA_TLS_KEY_FILE")
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return Config{}, errors.New("defina CHUSPA_TLS_CERT_FILE y CHUSPA_TLS_KEY_FILE juntas")
	}
	if c.TLSCertFile != "" {
		if _, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile); err != nil {
			return Config{}, fmt.Errorf("no se pudo cargar el certificado TLS: %w", err)
		}
	}

	if c.AllowAnonymous {
		if !c.Loopback() {
			return Config{}, errors.New("CHUSPA_ALLOW_ANONYMOUS=true solo se permite escuchando en loopback (127.0.0.1 o ::1)")
		}
		if c.RootUser == "" && c.RootPassword == "" {
			return c, nil
		}
	}

	if c.RootUser == "" || c.RootPassword == "" {
		return Config{}, errors.New("faltan credenciales: defina CHUSPA_ROOT_USER y CHUSPA_ROOT_PASSWORD " +
			"(o CHUSPA_ROOT_PASSWORD_FILE). El servidor no arranca abierto; para pruebas locales sin " +
			"autenticación use CHUSPA_ALLOW_ANONYMOUS=true, que solo funciona en loopback")
	}
	if !accessKeyRe.MatchString(c.RootUser) {
		return Config{}, errors.New("CHUSPA_ROOT_USER inválido: de 3 a 128 caracteres entre letras, dígitos, guion y guion bajo")
	}
	if len(c.RootPassword) < MinSecretLen {
		return Config{}, fmt.Errorf("CHUSPA_ROOT_PASSWORD es muy corto: mínimo %d caracteres", MinSecretLen)
	}
	if c.PreviousRootPassword != "" {
		if len(c.PreviousRootPassword) < MinSecretLen {
			return Config{}, fmt.Errorf("CHUSPA_ROOT_PASSWORD_PREVIOUS es muy corto: mínimo %d caracteres", MinSecretLen)
		}
		if c.PreviousRootPassword == c.RootPassword {
			return Config{}, errors.New("CHUSPA_ROOT_PASSWORD_PREVIOUS es igual al secreto actual: para rotar, ponga el nuevo en CHUSPA_ROOT_PASSWORD")
		}
	}
	if !c.Loopback() && (docCredentials[c.RootUser] || docCredentials[c.RootPassword] || docCredentials[c.PreviousRootPassword]) {
		return Config{}, errors.New("las credenciales de ejemplo de la documentación no se aceptan fuera de loopback: defina unas propias")
	}
	return c, nil
}

// readSecret lee el secreto de la variable name o del archivo en name_FILE.
func readSecret(getenv func(string) string, name string) (string, error) {
	direct, file := getenv(name), getenv(name+"_FILE")
	if direct != "" && file != "" {
		return "", fmt.Errorf("defina %s o %s_FILE, no ambas", name, name)
	}
	if file == "" {
		return direct, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("no se pudo leer %s_FILE: %w", name, err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
