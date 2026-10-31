package natsd

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// Embedded is a running server plus the controller's in-process connection.
type Embedded struct {
	Server *server.Server
	Conn   *nats.Conn
	Keys   *Keys
	CA     *CA
}

// Start launches NATS in operator mode with TLS on listen (host:port; port -1 = random).
func Start(dataDir, listen string) (*Embedded, error) {
	natsDir := filepath.Join(dataDir, "nats")
	keys, err := EnsureKeys(natsDir)
	if err != nil {
		return nil, err
	}
	ca, err := EnsureCA(natsDir)
	if err != nil {
		return nil, err
	}
	host, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("listen.nats %q: %w", listen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("listen.nats port %q: %w", portStr, err)
	}
	tlsCfg, err := ca.ServerTLS(host)
	if err != nil {
		return nil, err
	}
	op, sysJWT, accJWT, err := keys.claims()
	if err != nil {
		return nil, err
	}
	resolver := &server.MemAccResolver{}
	if err := resolver.Store(pub(keys.System), sysJWT); err != nil {
		return nil, err
	}
	if err := resolver.Store(pub(keys.Account), accJWT); err != nil {
		return nil, err
	}
	opts := &server.Options{
		ServerName:       "wrangl-controller",
		Host:             host,
		Port:             port,
		JetStream:        true,
		StoreDir:         filepath.Join(natsDir, "js"),
		TrustedOperators: []*jwt.OperatorClaims{op},
		SystemAccount:    pub(keys.System),
		AccountResolver:  resolver,
		TLSConfig:        tlsCfg,
		TLS:              true,
		TLSTimeout:       5,
		NoSigs:           true,
		NoLog:            true,
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("nats: %w", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		ns.Shutdown()
		return nil, errors.New("nats: not ready after 10s")
	}
	userJWT, kp, err := keys.controllerUser()
	if err != nil {
		ns.Shutdown()
		return nil, err
	}
	seed, _ := kp.Seed()
	// TLS applies to the in-process pipe too: without a client TLS config the handshake
	// fails and nats.go deadlocks closing the pipe (found in M2; see plan).
	clientTLS, err := ClientTLS(ca.CertPEM)
	if err != nil {
		ns.Shutdown()
		return nil, err
	}
	clientTLS.ServerName = "localhost"
	nc, err := nats.Connect(ns.ClientURL(),
		nats.InProcessServer(ns),
		nats.Secure(clientTLS),
		nats.UserJWTAndSeed(userJWT, string(seed)),
		nats.Name("wrangl-controller"),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		ns.Shutdown()
		return nil, fmt.Errorf("nats in-process connect: %w", err)
	}
	return &Embedded{Server: ns, Conn: nc, Keys: keys, CA: ca}, nil
}

// URL is the TLS client URL runners use (host from the listener).
func (e *Embedded) URL() string {
	return "tls://" + e.Server.Addr().String()
}

// Close drains nothing; it closes the connection and stops the server.
func (e *Embedded) Close() {
	e.Conn.Close()
	e.Server.Shutdown()
	e.Server.WaitForShutdown()
}
