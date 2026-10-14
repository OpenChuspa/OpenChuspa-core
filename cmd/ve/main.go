// ve es la CLI caleña de OpenChuspa: traduce comandos como "ve subi" o
// "ve mira" a llamadas S3 contra el motor (o contra cualquier servicio
// compatible con S3).
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// version se fija al compilar con -ldflags "-X main.version=...".
var version = "dev"

const (
	defaultEndpoint = "http://localhost:9000"
	defaultRegion   = "cali-san-antonio-1"
)

const usage = `ve: la CLI caleña de OpenChuspa

Uso:
  ve mira [chuspa://[chuspa[/prefijo]]] [-r]     lista chuspas o el contenido de una
  ve crea chuspa://chuspa                        crea una chuspa
  ve subi <archivo>... [a] chuspa://chuspa/[key] sube archivos
  ve baja chuspa://chuspa/key [a] [destino]      descarga un objeto (por defecto en .)
  ve borra chuspa://chuspa/key                   borra un objeto
  ve borra chuspa://chuspa[/prefijo] -r          borra todo lo que hay bajo el prefijo
  ve borra chuspa://chuspa                       borra una chuspa vacía (con -r, la vacía antes)
  ve sincroniza <origen> <destino>               copia lo nuevo o cambiado entre una carpeta y una chuspa
  ve version                                     muestra la versión

Si el destino de "subi" termina en "/", la key es ese prefijo más el nombre
del archivo. "sincroniza" solo copia: nunca borra en el destino.

Configuración por variables de entorno:
  VE_ENDPOINT             dirección del motor (por defecto ` + defaultEndpoint + `)
  VE_REGION               región; si no está, AWS_REGION o AWS_DEFAULT_REGION
                          (por defecto ` + defaultRegion + `)
  AWS_ACCESS_KEY_ID       la clave del parchao
  AWS_SECRET_ACCESS_KEY   la secreta de la cuadra
  AWS_SESSION_TOKEN       opcional
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println(version)
		return
	case "ayuda", "help", "--help", "-h":
		fmt.Print(usage)
		return
	}

	run, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(os.Stderr, "ve: no conozco el comando %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig(os.Getenv)
	if err == nil {
		err = run(ctx, newClient(cfg), args)
	}
	if err != nil {
		var u usageError
		if errors.As(err, &u) {
			fmt.Fprintf(os.Stderr, "ve %s: %v\n\n%s", cmd, err, usage)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "ve %s: %s\n", cmd, explain(err, cfg.Endpoint))
		os.Exit(1)
	}
}

// usageError marca un error en los argumentos, que se acompaña de la ayuda.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

type config struct {
	Endpoint string
	Region   string
	Creds    aws.Credentials
}

func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		Endpoint: firstNonEmpty(getenv("VE_ENDPOINT"), defaultEndpoint),
		Region:   firstNonEmpty(getenv("VE_REGION"), getenv("AWS_REGION"), getenv("AWS_DEFAULT_REGION"), defaultRegion),
		Creds: aws.Credentials{
			AccessKeyID:     getenv("AWS_ACCESS_KEY_ID"),
			SecretAccessKey: getenv("AWS_SECRET_ACCESS_KEY"),
			SessionToken:    getenv("AWS_SESSION_TOKEN"),
			Source:          "ve",
		},
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, fmt.Errorf("VE_ENDPOINT inválido (%q): se espera algo como %s", c.Endpoint, defaultEndpoint)
	}
	if c.Creds.AccessKeyID == "" || c.Creds.SecretAccessKey == "" {
		return c, errors.New("faltan credenciales: defina AWS_ACCESS_KEY_ID y AWS_SECRET_ACCESS_KEY")
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		fmt.Fprintf(os.Stderr, "ve: ojo, %s va sin TLS; el contenido viaja en claro\n", c.Endpoint)
	}
	return c, nil
}

func newClient(c config, optFns ...func(*s3.Options)) *s3.Client {
	return s3.New(s3.Options{
		Region:       c.Region,
		BaseEndpoint: aws.String(c.Endpoint),
		UsePathStyle: true,
		Credentials:  aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return c.Creds, nil }),
	}, optFns...)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}
