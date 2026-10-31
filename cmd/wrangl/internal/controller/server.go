package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/nreid/devhost/cmd/wrangl/internal/config"
	"github.com/nreid/devhost/cmd/wrangl/internal/db"
)

// JetStream streams (spec §7).
const (
	StreamEvents = "WR_EVT"
	StreamPTY    = "WR_PTY"
)

// Streams returns the stream configs the controller ensures on start.
func Streams() []jetstream.StreamConfig {
	return []jetstream.StreamConfig{
		{Name: StreamEvents, Subjects: []string{"wr.evt.>"}, Storage: jetstream.FileStorage, MaxAge: 7 * 24 * time.Hour},
		{Name: StreamPTY, Subjects: []string{"wr.pty.>"}, Storage: jetstream.FileStorage, MaxAge: 3 * 24 * time.Hour, MaxBytes: 2 << 30},
	}
}

// Server is the controller process: database, embedded NATS + JetStream and HTTP.
type Server struct {
	cfg   *config.Controller
	log   *slog.Logger
	ready chan struct{}

	mu       sync.Mutex
	httpAddr string
	natsURL  string

	db *db.DB
	ns *server.Server
	nc *nats.Conn
	js jetstream.JetStream
}

// New validates cfg and returns an unstarted server.
func New(cfg *config.Controller, log *slog.Logger) (*Server, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("data_dir must be set")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, log: log, ready: make(chan struct{})}, nil
}

// Ready is closed once every listener is accepting.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// HTTPAddr is the bound TCP address of the HTTP listener (valid after Ready).
func (s *Server) HTTPAddr() string { s.mu.Lock(); defer s.mu.Unlock(); return s.httpAddr }

// NATSURL is the client URL of the embedded NATS server (valid after Ready).
func (s *Server) NATSURL() string { s.mu.Lock(); defer s.mu.Unlock(); return s.natsURL }

// Run starts everything, blocks until ctx is cancelled, then shuts down HTTP, NATS and
// the database in that order.
func (s *Server) Run(ctx context.Context) (err error) {
	if s.db, err = db.Open(ctx, DBPath(s.cfg.DataDir)); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.db.Close()) }()

	if err := s.startNATS(ctx); err != nil {
		return err
	}
	defer func() {
		s.nc.Close()
		s.ns.Shutdown()
		s.ns.WaitForShutdown()
	}()

	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	tcp, err := net.Listen("tcp", s.cfg.Listen.HTTP)
	if err != nil {
		return fmt.Errorf("http listen: %w", err)
	}
	listeners := []net.Listener{tcp}
	if s.cfg.Listen.Unix != "" {
		unix, err := listenUnix(s.cfg.Listen.Unix)
		if err != nil {
			tcp.Close()
			return err
		}
		listeners = append(listeners, unix)
	}
	s.mu.Lock()
	s.httpAddr = tcp.Addr().String()
	s.mu.Unlock()

	errc := make(chan error, len(listeners))
	for _, l := range listeners {
		go func(l net.Listener) {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(l)
	}
	s.log.Info("controller serving", "http", s.httpAddr, "unix", s.cfg.Listen.Unix, "nats", s.natsURL)
	close(s.ready)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(err, srv.Shutdown(shutCtx)) // closes listeners; unix socket is unlinked
}

func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	// A previous unclean exit leaves the socket file behind; refuse only if something
	// is actually listening on it.
	if c, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("%s: another controller is already listening", path)
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("unix listen: %w", err)
	}
	if err := os.Chmod(path, 0o660); err != nil { // wrangl group may connect
		l.Close()
		return nil, err
	}
	return l, nil
}

func (s *Server) startNATS(ctx context.Context) error {
	host, portStr, err := net.SplitHostPort(s.cfg.Listen.NATS)
	if err != nil {
		return fmt.Errorf("listen.nats %q: %w", s.cfg.Listen.NATS, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("listen.nats port %q: %w", portStr, err)
	}
	// M2: operator/JWT auth and TLS. Until then NATS must only bind loopback.
	opts := &server.Options{
		ServerName: "wrangl-controller",
		Host:       host,
		Port:       port, // -1 = random (tests)
		JetStream:  true,
		StoreDir:   filepath.Join(s.cfg.DataDir, "nats", "js"),
		NoSigs:     true,
		NoLog:      true,
	}
	if s.ns, err = server.NewServer(opts); err != nil {
		return fmt.Errorf("nats: %w", err)
	}
	go s.ns.Start()
	if !s.ns.ReadyForConnections(10 * time.Second) {
		s.ns.Shutdown()
		return errors.New("nats: not ready after 10s")
	}
	if s.nc, err = nats.Connect(s.ns.ClientURL(), nats.InProcessServer(s.ns), nats.Name("wrangl-controller")); err != nil {
		s.ns.Shutdown()
		return fmt.Errorf("nats in-process connect: %w", err)
	}
	if s.js, err = jetstream.New(s.nc); err != nil {
		return err
	}
	for _, sc := range Streams() {
		if _, err := s.js.CreateOrUpdateStream(ctx, sc); err != nil {
			s.nc.Close()
			s.ns.Shutdown()
			return fmt.Errorf("ensure stream %s: %w", sc.Name, err)
		}
	}
	s.mu.Lock()
	s.natsURL = s.ns.ClientURL()
	s.mu.Unlock()
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	return mux
}

// healthz reports each dependency; any failure returns 503.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	st := map[string]string{"db": "ok", "nats": "ok", "jetstream": "ok"}
	code := http.StatusOK
	fail := func(k string, err error) { st[k] = err.Error(); code = http.StatusServiceUnavailable }
	if err := s.db.R.QueryRowContext(ctx, "SELECT 1").Scan(new(int)); err != nil {
		fail("db", err)
	}
	if !s.ns.Running() || !s.nc.IsConnected() {
		fail("nats", errors.New("not connected"))
	}
	if _, err := s.js.AccountInfo(ctx); err != nil {
		fail("jetstream", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(st)
}
