package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/giovibal/intreccio"
)

// writeTestCerts creates a throwaway CA and a leaf certificate valid for
// localhost/127.0.0.1 (usable for both server and client auth), writing them as
// PEM files. It returns the CA, certificate and key paths.
func writeTestCerts(t *testing.T) (caFile, certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "intreccio-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "intreccio-node"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}

	return writePEM(t, dir, "ca.pem", "CERTIFICATE", caDER),
		writePEM(t, dir, "cert.pem", "CERTIFICATE", leafDER),
		writePEM(t, dir, "key.pem", "EC PRIVATE KEY", leafKeyDER)
}

func writePEM(t *testing.T, dir, name, typ string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	defer func() { _ = f.Close() }()
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	return path
}

// TestTLSConfigValidation rejects an incomplete key pair.
func TestTLSConfigValidation(t *testing.T) {
	if _, _, err := (&TLSConfig{CertFile: "cert.pem"}).configs(); err == nil {
		t.Fatal("expected an error when KeyFile is missing")
	}
}

// TestTLSClusterReplicationAndForwarding runs a 3-voter cluster with mutual TLS
// and checks that replication, a forwarded write and linearizable reads all work
// over encrypted connections.
func TestTLSClusterReplicationAndForwarding(t *testing.T) {
	ca, cert, key := writeTestCerts(t)
	tlsCfg := &TLSConfig{CertFile: cert, KeyFile: key, CAFile: ca}
	tc := startCluster(t, 3, func(c *Config) { c.TLS = tlsCfg })
	tc.leaderIndex(t) // wait until the cluster is up

	// A write issued on a follower is forwarded to the leader over mTLS, then
	// replicated to every voter over the TLS transport.
	follower := tc.aFollower(t)
	if _, err := tc.dbs[follower].Query(context.Background(),
		"CREATE (n:Person {name: 'Alice'})", nil); err != nil {
		t.Fatalf("forwarded write over mTLS: %v", err)
	}

	other := tc.aFollower(t)
	if got := mustQuery(t, tc.dbs[other], matchNames, intreccio.Linearizable()); !equal(got, []string{"Alice"}) {
		t.Fatalf("linearizable read over mTLS = %v, want [Alice]", got)
	}
	for i := range tc.nodes {
		waitForLocal(t, tc.dbs[i], []string{"Alice"})
	}
}

// TestTLSClusterClient verifies a dataless client can forward queries to the
// voters over mutual TLS.
func TestTLSClusterClient(t *testing.T) {
	ca, cert, key := writeTestCerts(t)
	tlsCfg := &TLSConfig{CertFile: cert, KeyFile: key, CAFile: ca}
	tc := startCluster(t, 3, func(c *Config) { c.TLS = tlsCfg })
	tc.leaderIndex(t)

	db, err := Open(Config{NodeID: "client-1", Role: RoleClient, Peers: tc.peers, TLS: tlsCfg})
	if err != nil {
		t.Fatalf("open TLS client: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	if _, err := db.Query(ctx, "CREATE (n:Person {name: 'Bob'})", nil); err != nil {
		t.Fatalf("client write over mTLS: %v", err)
	}
	if got := mustQuery(t, db, matchNames, intreccio.Linearizable()); !equal(got, []string{"Bob"}) {
		t.Fatalf("client linearizable read over mTLS = %v, want [Bob]", got)
	}
}

// TestTLSRejectsPlaintext verifies the forwarding endpoint requires TLS: a
// plaintext RPC client cannot execute a query against it.
func TestTLSRejectsPlaintext(t *testing.T) {
	ca, cert, key := writeTestCerts(t)
	cfg := Config{
		NodeID:      "n1",
		DataDir:     t.TempDir(),
		BindAddr:    freeAddr(t),
		ForwardAddr: freeAddr(t),
		Role:        RoleVoter,
		Bootstrap:   true,
		TLS:         &TLSConfig{CertFile: cert, KeyFile: key, CAFile: ca},
	}
	node, err := newNode(cfg)
	if err != nil {
		t.Fatalf("newNode: %v", err)
	}
	defer func() { _ = node.Close() }()

	c, err := rpc.Dial("tcp", cfg.ForwardAddr)
	if err != nil {
		t.Fatalf("plaintext dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	var reply ForwardReply
	if err := c.Call("Forward.Query", &ForwardArgs{Cypher: "RETURN 1"}, &reply); err == nil {
		t.Fatal("plaintext call against a TLS endpoint unexpectedly succeeded")
	}
}
