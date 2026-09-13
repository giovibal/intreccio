package cluster

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/hashicorp/raft"
)

// TLSConfig enables TLS for a cluster node's Raft transport and forwarding RPC
// endpoint. Both are enabled together and share the same key pair, so every
// inter-node connection (Raft replication and query forwarding) is encrypted.
//
// CertFile and KeyFile are required when TLSConfig is set. When CAFile is also
// set, peer verification is mutual: the server requires and verifies a client
// certificate, and the client verifies the server against CAFile — so only
// nodes holding a certificate signed by that CA can join or send queries.
// Without CAFile, the server does not request a client certificate and the client
// verifies the server against the system roots (unless InsecureSkipVerify).
//
// A nil TLSConfig (the default) keeps the cluster on plaintext TCP.
type TLSConfig struct {
	// CertFile is the PEM certificate (with any intermediate chain) this node
	// presents to peers.
	CertFile string
	// KeyFile is the PEM private key for CertFile.
	KeyFile string
	// CAFile is the PEM bundle used to verify peers. Setting it turns on mutual
	// TLS (mTLS).
	CAFile string
	// ServerName overrides the name the client expects in the server
	// certificate. Empty uses the dialed host, which is usually correct.
	ServerName string
	// InsecureSkipVerify disables server certificate verification on outgoing
	// connections. For tests only; never set it in production.
	InsecureSkipVerify bool
}

// configs builds the server and client TLS configurations from the file paths.
func (c *TLSConfig) configs() (server, client *tls.Config, err error) {
	if c.CertFile == "" || c.KeyFile == "" {
		return nil, nil, errors.New("cluster: TLS requires both CertFile and KeyFile")
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("cluster: load TLS key pair: %w", err)
	}

	server = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	client = &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         tls.VersionTLS12,
		ServerName:         c.ServerName,
		InsecureSkipVerify: c.InsecureSkipVerify,
	}

	if c.CAFile != "" {
		pool, err := loadCAPool(c.CAFile)
		if err != nil {
			return nil, nil, err
		}
		// Mutual TLS: require and verify the peer's certificate.
		server.ClientCAs = pool
		server.ClientAuth = tls.RequireAndVerifyClientCert
		client.RootCAs = pool
	}
	return server, client, nil
}

// loadCAPool reads a PEM bundle of CA certificates into a pool.
func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cluster: read CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("cluster: no certificates found in CA file %s", path)
	}
	return pool, nil
}

// newTransport builds the Raft network transport, on TLS when tlsServer is set
// and on plaintext TCP otherwise.
func newTransport(bindAddr string, advertise net.Addr, tlsServer, tlsClient *tls.Config, logOutput io.Writer) (*raft.NetworkTransport, error) {
	if tlsServer == nil {
		return raft.NewTCPTransport(bindAddr, advertise, 3, 10*time.Second, logOutput)
	}
	ln, err := tls.Listen("tcp", bindAddr, tlsServer)
	if err != nil {
		return nil, err
	}
	stream := newTLSStreamLayer(advertise, ln, tlsClient)
	return raft.NewNetworkTransport(stream, 3, 10*time.Second, logOutput), nil
}

// tlsStreamLayer adapts a TLS listener to Raft's StreamLayer, so the Raft
// transport dials and accepts encrypted connections. It mirrors raft's
// TCPStreamLayer, including honoring an explicit advertise address.
type tlsStreamLayer struct {
	advertise net.Addr
	listener  net.Listener
	client    *tls.Config
}

var _ raft.StreamLayer = (*tlsStreamLayer)(nil)

// newTLSStreamLayer wraps a TLS listener. advertise is the address peers should
// dial (it may differ from the listener's bound address); client configures
// outgoing connections.
func newTLSStreamLayer(advertise net.Addr, listener net.Listener, client *tls.Config) *tlsStreamLayer {
	return &tlsStreamLayer{advertise: advertise, listener: listener, client: client}
}

func (s *tlsStreamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	return tls.DialWithDialer(dialer, "tcp", string(address), s.client)
}

func (s *tlsStreamLayer) Accept() (net.Conn, error) { return s.listener.Accept() }

func (s *tlsStreamLayer) Close() error { return s.listener.Close() }

func (s *tlsStreamLayer) Addr() net.Addr {
	if s.advertise != nil {
		return s.advertise
	}
	return s.listener.Addr()
}
