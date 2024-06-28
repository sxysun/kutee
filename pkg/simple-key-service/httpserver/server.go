package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"simple-key-service/common"
	"simple-key-service/metrics"

	"github.com/flashbots/go-utils/httplogger"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/atomic"
)

type HTTPServerConfig struct {
	ListenAddr  string
	MetricsAddr string
	EnablePprof bool
	Log         *slog.Logger

	DrainDuration            time.Duration
	GracefulShutdownDuration time.Duration
	ReadTimeout              time.Duration
	WriteTimeout             time.Duration
}

type Server struct {
	cfg     *HTTPServerConfig
	isReady atomic.Bool
	log     *slog.Logger

	ksApi *KeyServiceAPI

	srv     *http.Server
	metrics *metrics.MetricsServer
}

func New(cfg *HTTPServerConfig) (srv *Server, err error) {
	metricsSrv, err := metrics.New(common.PackageName, cfg.MetricsAddr)
	if err != nil {
		return nil, err
	}

	srv = &Server{
		cfg:     cfg,
		log:     cfg.Log,
		ksApi:   NewKeyServiceAPI(),
		srv:     nil,
		metrics: metricsSrv,
	}
	srv.isReady.Swap(true)

	mux := chi.NewRouter()

	measure_and_handle := func(name string, handler func(w http.ResponseWriter, r *http.Request)) func(w http.ResponseWriter, r *http.Request) {
		histogram_name := "request_duration_" + name
		return func(w http.ResponseWriter, r *http.Request) {
			m := srv.metrics.Float64Histogram(
				histogram_name,
				"API request handling duration",
				metrics.UomMicroseconds,
				metrics.BucketsRequestDuration...,
			)

			start := time.Now()

			handler(w, r)
			m.Record(r.Context(), float64(time.Since(start).Microseconds()))
		}
	}

	/* Derives and registers public key for a specific service.
	The corresponding private key is a derivation of an internal secret an a service token. Think of it as 2-of-2 */
	mux.With(srv.httpLogger).Post("/api/derive_pubkey", measure_and_handle("derive_pubkey", srv.ksApi.handleDerivePubkey))

	/* Returns the previously derived pubkey */
	mux.With(srv.httpLogger).Post("/api/get_pubkey", measure_and_handle("get_pubkey", srv.ksApi.handleGetPubkey))

	/* Encrypts data to a specific service. Requires the service is already registered */
	mux.With(srv.httpLogger).Post("/api/encrypt", measure_and_handle("encrypt", srv.ksApi.handleEncrypt))

	/* Decrypts data based on internal secret and a secret token passed in.
	Requires the service is already registered, and that data was encrypted to derived_pubkey */
	mux.With(srv.httpLogger).Post("/api/decrypt", measure_and_handle("decrypt", srv.ksApi.handleDecrypt))

	mux.With(srv.httpLogger).Get("/livez", srv.handleLivenessCheck)
	mux.With(srv.httpLogger).Get("/readyz", srv.handleReadinessCheck)
	mux.With(srv.httpLogger).Get("/drain", srv.handleDrain)
	mux.With(srv.httpLogger).Get("/undrain", srv.handleUndrain)

	if cfg.EnablePprof {
		srv.log.Info("pprof API enabled")
		mux.Mount("/debug", middleware.Profiler())
	}

	srv.srv = &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}

	return srv, nil
}

func (s *Server) httpLogger(next http.Handler) http.Handler {
	return httplogger.LoggingMiddlewareSlog(s.log, next)
}

func (s *Server) RunInBackground() {
	// metrics
	if s.cfg.MetricsAddr != "" {
		go func() {
			s.log.With("metricsAddress", s.cfg.MetricsAddr).Info("Starting metrics server")
			err := s.metrics.ListenAndServe()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Error("HTTP server failed", "err", err)
			}
		}()
	}

	// api
	go func() {
		s.log.Info("Starting HTTP server", "listenAddress", s.cfg.ListenAddr)
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("HTTP server failed", "err", err)
		}
	}()
}

func (s *Server) Shutdown() {
	// api
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.GracefulShutdownDuration)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		s.log.Error("Graceful HTTP server shutdown failed", "err", err)
	} else {
		s.log.Info("HTTP server gracefully stopped")
	}

	// metrics
	if len(s.cfg.MetricsAddr) != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.GracefulShutdownDuration)
		defer cancel()

		if err := s.metrics.Shutdown(ctx); err != nil {
			s.log.Error("Graceful metrics server shutdown failed", "err", err)
		} else {
			s.log.Info("Metrics server gracefully stopped")
		}
	}
}