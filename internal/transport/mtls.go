package transport

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"

	"google.golang.org/grpc/credentials"
)

type CredentialsBundle struct {
	Server credentials.TransportCredentials
	Client credentials.TransportCredentials
}

type FileCredentialsConfig struct {
	CACertPath string
	CertPath   string
	KeyPath    string
	ServerName string
}

func GenerateDevMTLS(hosts []string, ips []net.IP) (*CredentialsBundle, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "lecrev-dev-ca",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	serverCert, err := issueLeaf(caCert, caKey, "lecrev-dev-server", hosts, ips, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	if err != nil {
		return nil, err
	}
	clientCert, err := issueLeaf(caCert, caKey, "lecrev-dev-client", nil, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	if err != nil {
		return nil, err
	}

	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))

	serverTLS := credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
	})
	clientTLS := credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caPool,
		ServerName:   hosts[0],
	})

	return &CredentialsBundle{
		Server: serverTLS,
		Client: clientTLS,
	}, nil
}

func LoadServerMTLS(cfg FileCredentialsConfig) (credentials.TransportCredentials, error) {
	if cfg.CACertPath == "" || cfg.CertPath == "" || cfg.KeyPath == "" {
		return nil, fmt.Errorf("ca cert, cert, and key paths are required for server mTLS")
	}
	caPool, err := loadCertPool(cfg.CACertPath)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
	}), nil
}

func LoadClientMTLS(cfg FileCredentialsConfig) (credentials.TransportCredentials, error) {
	if cfg.CACertPath == "" || cfg.CertPath == "" || cfg.KeyPath == "" {
		return nil, fmt.Errorf("ca cert, cert, and key paths are required for client mTLS")
	}
	if cfg.ServerName == "" {
		return nil, fmt.Errorf("server name is required for client mTLS")
	}
	caPool, err := loadCertPool(cfg.CACertPath)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("load client keypair: %w", err)
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		ServerName:   cfg.ServerName,
	}), nil
}

func issueLeaf(caCert *x509.Certificate, caKey *rsa.PrivateKey, commonName string, hosts []string, ips []net.IP, usages []x509.ExtKeyUsage) (tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: commonName,
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: usages,
		DNSNames:    hosts,
		IPAddresses: ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("build keypair: %w", err)
	}
	return cert, nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ca cert %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(data); !ok {
		return nil, fmt.Errorf("append ca certs from %s", path)
	}
	return pool, nil
}
