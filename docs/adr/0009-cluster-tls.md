# ADR 0009 — TLS and mutual authentication for the cluster

Status: accepted — 2026-09-13

Refines ADR 0007 (clustering).

## Context
ADR 0007 introduced the opt-in Raft-replicated cluster with two network surfaces:
the Raft transport between voters and the forwarding RPC endpoint (net/rpc) used
to route writes and linearizable reads to the leader. Both spoke **plaintext
TCP** and neither authenticated peers. Anyone able to reach the Raft or
forwarding port could join the transport or submit arbitrary Cypher — including
reads, writes and deletes — to the database.

Encryption and peer authentication are therefore required for any non-local
deployment. Constraints carried over from the core project:
- **Pure Go, no cgo**: only the standard library `crypto/tls` is acceptable.
- **Opt-in**: the embedded single-binary path must stay TLS-free and unaffected.
- **Configurable**: operators choose whether to enable TLS and whether to require
  mutual authentication.
- **Backward compatible**: an unconfigured cluster keeps behaving as before.

## Decision

### One TLS configuration for both surfaces
A single `cluster.TLSConfig` (`CertFile`, `KeyFile`, `CAFile`, `ServerName`,
`InsecureSkipVerify`) is applied to the Raft transport **and** the forwarding RPC
endpoint, so every inter-node connection is encrypted by the same key pair.
`Config.TLS` is `nil` by default — plaintext, exactly as before.

### Mutual TLS when a CA is supplied
- `CertFile` + `KeyFile` are required and present this node's certificate.
- When `CAFile` is set, peer verification is **mutual**: the server sets
  `ClientAuth = RequireAndVerifyClientCert` with `ClientCAs` from the bundle, and
  the client verifies the server against `RootCAs` from the same bundle while
  presenting its own certificate. Only nodes holding a CA-signed certificate can
  join or submit queries.
- Without `CAFile`, the server does not request a client certificate and the
  client falls back to the system roots (or `InsecureSkipVerify`, for tests).
- `MinVersion` is TLS 1.2.

### Custom Raft stream layer
`hashicorp/raft` ships only a plaintext `TCPStreamLayer`; its `StreamLayer`
interface is the documented extension point. A small `tlsStreamLayer` wraps a
`tls.Listener` and dials with `tls.DialWithDialer`, mirroring the semantics of
`TCPStreamLayer` (including an explicit advertise address). The forwarding server
likewise switches from `net.Listen` to `tls.Listen`, and the connection pool
dials with `tls.Dial`.

### CLI flags
The cluster-capable CLI gains `-cluster-tls-cert`, `-cluster-tls-key`,
`-cluster-tls-ca`, `-cluster-tls-server-name` and `-cluster-tls-insecure`.

## Consequences
- **No new dependencies**: `crypto/tls`, `crypto/x509` and `encoding/pem` are
  standard library.
- Operators must provision certificates, distribute the CA, and give the
  certificate SANs that cover the addresses peers dial (the `advertise`
  addresses).
- TLS is **transport security and peer authentication only**. It does not add
  Cypher-level authorization: any authenticated node or client can still run any
  query it could before. Role-based authorization remains future work.
- `InsecureSkipVerify` exists for tests and must not be used in production.
- Existing plaintext tests and deployments are unaffected (`TLS == nil`).
