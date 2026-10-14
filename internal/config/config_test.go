package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodSecret = "0123456789abcdef0123456789abcdef"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"sin credenciales no arranca", map[string]string{}, "faltan credenciales"},
		{"solo usuario", map[string]string{"CHUSPA_ROOT_USER": "parcero"}, "faltan credenciales"},
		{"secreto corto", map[string]string{"CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD": "corto"}, "muy corto"},
		{"válida en loopback", map[string]string{"CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD": goodSecret}, ""},
		{"válida expuesta", map[string]string{"CHUSPA_ADDR": ":9000", "CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD": goodSecret}, ""},
		{"credencial de ejemplo expuesta", map[string]string{"CHUSPA_ADDR": "0.0.0.0:9000", "CHUSPA_ROOT_USER": "clave-del-parchao", "CHUSPA_ROOT_PASSWORD": goodSecret}, "credenciales de ejemplo"},
		{"credencial de ejemplo en loopback", map[string]string{"CHUSPA_ROOT_USER": "clave-del-parchao", "CHUSPA_ROOT_PASSWORD": goodSecret}, ""},
		{"anónimo en loopback", map[string]string{"CHUSPA_ALLOW_ANONYMOUS": "true"}, ""},
		{"anónimo expuesto", map[string]string{"CHUSPA_ADDR": ":9000", "CHUSPA_ALLOW_ANONYMOUS": "true"}, "solo se permite escuchando en loopback"},
		{"usuario con barra", map[string]string{"CHUSPA_ROOT_USER": "a/b", "CHUSPA_ROOT_PASSWORD": goodSecret}, "CHUSPA_ROOT_USER inválido"},
		{"dirección inválida", map[string]string{"CHUSPA_ADDR": "9000"}, "CHUSPA_ADDR inválida"},
		{"expiración inválida", map[string]string{"CHUSPA_MULTIPART_EXPIRY": "una semana"}, "CHUSPA_MULTIPART_EXPIRY inválida"},
		{"verificación inválida", map[string]string{"CHUSPA_SCRUB_INTERVAL": "-1h"}, "CHUSPA_SCRUB_INTERVAL inválida"},
		{"métricas inválidas", map[string]string{"CHUSPA_METRICS_ADDR": "9100"}, "CHUSPA_METRICS_ADDR inválida"},
		{"métricas en el puerto de la API", map[string]string{"CHUSPA_METRICS_ADDR": "127.0.0.1:9000"}, "distinta de CHUSPA_ADDR"},
		{"bloqueo inválido", map[string]string{"CHUSPA_AUTH_MAX_FAILURES": "-1"}, "CHUSPA_AUTH_MAX_FAILURES inválido"},
		{"espacio libre inválido", map[string]string{"CHUSPA_MIN_FREE_MB": "1GB"}, "CHUSPA_MIN_FREE_MB inválido"},
		{"rotación válida", map[string]string{"CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD": goodSecret, "CHUSPA_ROOT_PASSWORD_PREVIOUS": "fedcba9876543210fedcba9876543210"}, ""},
		{"secreto anterior corto", map[string]string{"CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD": goodSecret, "CHUSPA_ROOT_PASSWORD_PREVIOUS": "corto"}, "PREVIOUS es muy corto"},
		{"secreto anterior igual", map[string]string{"CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD": goodSecret, "CHUSPA_ROOT_PASSWORD_PREVIOUS": goodSecret}, "igual al secreto actual"},
		{"TLS a medias", map[string]string{"CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD": goodSecret, "CHUSPA_TLS_CERT_FILE": "/x"}, "juntas"},
		{"certificado ilegible", map[string]string{"CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD": goodSecret, "CHUSPA_TLS_CERT_FILE": "/no/existe", "CHUSPA_TLS_KEY_FILE": "/no/existe"}, "certificado TLS"},
		{"dos fuentes del secreto", map[string]string{"CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD": goodSecret, "CHUSPA_ROOT_PASSWORD_FILE": "/x"}, "no ambas"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("error inesperado: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("se esperaba un error con %q, se obtuvo: %v", tt.wantErr, err)
			}
		})
	}
}

func TestLoadSecretFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secreto")
	if err := os.WriteFile(path, []byte(goodSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(env(map[string]string{"CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD_FILE": path}))
	if err != nil {
		t.Fatal(err)
	}
	if c.RootPassword != goodSecret {
		t.Fatalf("el secreto leído del archivo no coincide")
	}
}
