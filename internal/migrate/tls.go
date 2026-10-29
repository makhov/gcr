//go:build linux

package migrate

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"

	"github.com/containerd/errdefs"
)

// TLS is how migrations to and from a runtime handler's containers are
// secured (its MigrationTLS options). Files are read when a migration
// starts, so certificates can be rotated in place.
//
// With Cert and Key, a target serves TLS only; with CA as well, it asks
// every source for a certificate signed by CA. A source with any of them
// set migrates only to https:// targets, which it verifies against CA
// (the system's roots without one), presenting Cert and Key if set.
type TLS struct {
	Cert string `toml:"Cert"`
	Key  string `toml:"Key"`
	CA   string `toml:"CA"`
}

func (t TLS) Enabled() bool { return t != TLS{} }

func (t TLS) Validate() error {
	if (t.Cert == "") != (t.Key == "") {
		return fmt.Errorf("MigrationTLS: Cert and Key go together: %w", errdefs.ErrInvalidArgument)
	}
	return nil
}

func (t TLS) certificates() ([]tls.Certificate, error) {
	if t.Cert == "" {
		return nil, nil
	}
	c, err := tls.LoadX509KeyPair(t.Cert, t.Key)
	if err != nil {
		return nil, fmt.Errorf("MigrationTLS: %w", err)
	}
	return []tls.Certificate{c}, nil
}

func (t TLS) pool() (*x509.CertPool, error) {
	if t.CA == "" {
		return nil, nil
	}
	b, err := os.ReadFile(t.CA)
	if err != nil {
		return nil, fmt.Errorf("MigrationTLS: %w", err)
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("MigrationTLS: no certificates in %s: %w", t.CA, errdefs.ErrInvalidArgument)
	}
	return p, nil
}

// serverConfig is the target's: nil when it serves plain HTTP.
func (t TLS) serverConfig() (*tls.Config, error) {
	if t.Cert == "" {
		return nil, nil
	}
	certs, err := t.certificates()
	if err != nil {
		return nil, err
	}
	pool, err := t.pool()
	if err != nil {
		return nil, err
	}
	c := &tls.Config{
		Certificates: certs,
		MinVersion:   tls.VersionTLS13,
		// The page stream takes over the connection, which HTTP/2 cannot
		// hand over.
		NextProtos: []string{"http/1.1"},
	}
	if pool != nil {
		c.ClientCAs, c.ClientAuth = pool, tls.RequireAndVerifyClientCert
	}
	return c, nil
}

// clientConfig is the source's for target: nil for plain HTTP.
func (t TLS) clientConfig(target *url.URL) (*tls.Config, error) {
	switch target.Scheme {
	case "https":
	case "http":
		if t.Enabled() {
			return nil, fmt.Errorf("MigrationTLS is set: migrate to https://, not %s: %w", target, errdefs.ErrInvalidArgument)
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("migration target %s: want http:// or https://: %w", target, errdefs.ErrInvalidArgument)
	}
	certs, err := t.certificates()
	if err != nil {
		return nil, err
	}
	pool, err := t.pool()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: certs,
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"http/1.1"},
	}, nil
}
