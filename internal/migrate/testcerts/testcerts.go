// Package testcerts makes a CA and certificates signed by it, for tests
// of migration over TLS.
package testcerts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Files are the paths Write made.
type Files struct {
	CA                    string
	ServerCert, ServerKey string // for 127.0.0.1
	ClientCert, ClientKey string
	// OtherCA signed nothing here: a source or target that trusts only
	// it trusts neither side.
	OtherCA string
}

// Write makes the files in dir.
func Write(t testing.TB, dir string) Files {
	t.Helper()
	ca, caKey := issue(t, nil, nil, "test CA", true)
	other, _ := issue(t, nil, nil, "other CA", true)
	server, serverKey := issue(t, ca, caKey, "target", false)
	client, clientKey := issue(t, ca, caKey, "source", false)
	f := Files{
		CA:         filepath.Join(dir, "ca.crt"),
		ServerCert: filepath.Join(dir, "server.crt"), ServerKey: filepath.Join(dir, "server.key"),
		ClientCert: filepath.Join(dir, "client.crt"), ClientKey: filepath.Join(dir, "client.key"),
		OtherCA: filepath.Join(dir, "other.crt"),
	}
	write(t, f.CA, "CERTIFICATE", ca.Raw)
	write(t, f.OtherCA, "CERTIFICATE", other.Raw)
	write(t, f.ServerCert, "CERTIFICATE", server.Raw)
	write(t, f.ClientCert, "CERTIFICATE", client.Raw)
	write(t, f.ServerKey, "PRIVATE KEY", marshal(t, serverKey))
	write(t, f.ClientKey, "PRIVATE KEY", marshal(t, clientKey))
	return f
}

func issue(t testing.TB, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, name string, isCA bool) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if isCA {
		tmpl.IsCA, tmpl.BasicConstraintsValid = true, true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
		parent, parentKey = tmpl, key
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func marshal(t testing.TB, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	b, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func write(t testing.TB, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
