// (c) 2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package api

import (
	"context"
	"net/http"
	"time"

	"github.com/lasthyphen/archiveus/cfg"
	"github.com/lasthyphen/archiveus/models"
	"github.com/lasthyphen/archiveus/services"
	"github.com/lasthyphen/archiveus/services/indexes/djtx"
	"github.com/lasthyphen/archiveus/servicesctrl"
	"github.com/lasthyphen/archiveus/stream/consumers"
	"github.com/lasthyphen/archiveus/utils"
	"github.com/gocraft/web"
)

// Server is an HTTP server configured with various ortelius APIs
type Server struct {
	sc     *servicesctrl.Control
	server *http.Server
}

// NewServer creates a new *Server based on the given config
func NewServer(sc *servicesctrl.Control, conf cfg.Config) (*Server, error) {
	router, err := newRouter(sc, conf)
	if err != nil {
		return nil, err
	}

	// Set address prefix to use the configured network
	models.SetBech32HRP(conf.NetworkID)

	return &Server{
		sc: sc,
		server: &http.Server{
			Addr:         conf.ListenAddr,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: cfg.HTTPWriteTimeout,
			IdleTimeout:  15 * time.Second,
			Handler:      router,
		},
	}, err
}

// Listen begins listening for new socket connections and blocks until closed
func (s *Server) Listen() error {
	s.sc.Log.Info("Server listening on %s", s.server.Addr)
	return s.server.ListenAndServe()
}

// Close shuts the server down
func (s *Server) Close() error {
	s.sc.Log.Info("Server shutting down")
	ctx, cancelFn := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFn()
	return s.server.Shutdown(ctx)
}

func newRouter(sc *servicesctrl.Control, conf cfg.Config) (*web.Router, error) {
	sc.Log.Info("Router chainID %s", sc.GenesisContainer.XChainID.String())

	indexBytes, err := newIndexResponse(conf.NetworkID, sc.GenesisContainer.XChainID, sc.GenesisContainer.DjtxAssetID)
	if err != nil {
		return nil, err
	}

	legacyIndexResponse, err := newLegacyIndexResponse(conf.NetworkID, sc.GenesisContainer.XChainID, sc.GenesisContainer.DjtxAssetID)
	if err != nil {
		return nil, err
	}

	// Create connections and readers
	connections, err := sc.DatabaseRO()
	if err != nil {
		return nil, err
	}

	cache := utils.NewCache()
	delayCache := utils.NewDelayCache(cache)

	consumersmap := make(map[string]services.Consumer)
	for chid, chain := range conf.Chains {
		consumer, err := consumers.IndexerConsumer(conf.NetworkID, chain.VMType, chid)
		if err != nil {
			return nil, err
		}
		consumersmap[chid] = consumer
	}
	consumercchain, err := consumers.IndexerConsumerCChain(conf.NetworkID, conf.CchainID)
	if err != nil {
		return nil, err
	}
	djtxReader, err := djtx.NewReader(conf.NetworkID, connections, consumersmap, consumercchain, sc)
	if err != nil {
		return nil, err
	}

	ctx := Context{sc: sc}

	// Build router
	router := web.New(ctx).
		Middleware(newContextSetter(sc, conf.NetworkID, connections, delayCache)).
		Middleware((*Context).setHeaders).
		Get("/", func(c *Context, resp web.ResponseWriter, _ *web.Request) {
			if _, err := resp.Write(indexBytes); err != nil {
				sc.Log.Warn("resp write %v", err)
			}
		}).
		NotFound((*Context).notFoundHandler).
		Middleware(func(c *Context, w web.ResponseWriter, r *web.Request, next web.NextMiddlewareFunc) {
			c.djtxReader = djtxReader
			c.djtxAssetID = sc.GenesisContainer.DjtxAssetID

			next(w, r)
		})

	AddV2Routes(&ctx, router, "/v2", indexBytes, nil)

	// Legacy routes.
	AddV2Routes(&ctx, router, "/x", legacyIndexResponse, &sc.GenesisContainer.XChainID)
	AddV2Routes(&ctx, router, "/X", legacyIndexResponse, &sc.GenesisContainer.XChainID)

	return router, nil
}
