// Copyright 2014 Canonical Ltd.

package identity

import (
	"fmt"
	"html/template"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/juju/httprequest"
	"github.com/juju/idmclient/params"
	"github.com/juju/loggo"
	"github.com/julienschmidt/httprouter"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/context"
	"gopkg.in/errgo.v1"
	"gopkg.in/macaroon-bakery.v2-unstable/bakery"
	"gopkg.in/mgo.v2"
	"launchpad.net/lpad"

	"github.com/CanonicalLtd/blues-identity/idp"
	"github.com/CanonicalLtd/blues-identity/internal/auth"
	"github.com/CanonicalLtd/blues-identity/internal/store"
	"github.com/CanonicalLtd/blues-identity/meeting"
)

var logger = loggo.GetLogger("identity.internal.identity")

// NewAPIHandlerFunc is a function that returns set of httprequest
// handlers that uses the given Store pool, and server params.
type NewAPIHandlerFunc func(HandlerParams) ([]httprequest.Handler, error)

// New returns a handler that serves the given identity API versions using the
// db to store identity data. The key of the versions map is the version name.
func New(db *mgo.Database, sp ServerParams, versions map[string]NewAPIHandlerFunc) (*Server, error) {
	if len(versions) == 0 {
		return nil, errgo.Newf("identity server must serve at least one version of the API")
	}
	var groupGetter store.ExternalGroupGetter
	if sp.Launchpad != "" {
		groupGetter = store.NewLaunchpadGroups(sp.Launchpad, 10*time.Minute)
	}
	// Create the identities store.
	pool, err := store.NewPool(db, store.StoreParams{
		ExternalGroupGetter: groupGetter,
		MaxMgoSessions:      sp.MaxMgoSessions,
		RequestTimeout:      sp.RequestTimeout,
		AdminAgentPublicKey: sp.AdminAgentPublicKey,
	})
	if err != nil {
		return nil, errgo.Notef(err, "cannot make store")
	}

	// Create the bakery parts.
	if sp.Key == nil {
		var err error
		sp.Key, err = bakery.GenerateKey()
		if err != nil {
			return nil, errgo.Notef(err, "cannot generate key")
		}
	}
	locator := bakery.NewThirdPartyStore()
	locator.AddInfo(sp.Location, bakery.ThirdPartyInfo{
		PublicKey: sp.Key.Public,
		Version:   bakery.LatestVersion,
	})
	var rksf func([]bakery.Op) bakery.RootKeyStore
	if sp.RootKeyStore != nil {
		rksf = func([]bakery.Op) bakery.RootKeyStore {
			return sp.RootKeyStore
		}
	}
	oven := bakery.NewOven(bakery.OvenParams{
		Namespace:          auth.Checker.Namespace(),
		RootKeyStoreForOps: rksf,
		Key:                sp.Key,
		Locator:            locator,
		Location:           "identity",
	})
	auth := auth.New(auth.Params{
		AdminUsername:   sp.AuthUsername,
		AdminPassword:   sp.AuthPassword,
		Location:        sp.Location,
		MacaroonOpStore: oven,
	})

	// Create the HTTP server.
	srv := &Server{
		router: httprouter.New(),
		pool:   pool,
	}
	// Disable the automatic rerouting in order to maintain
	// compatibility. It might be worthwhile relaxing this in the
	// future.
	srv.router.RedirectTrailingSlash = false
	srv.router.RedirectFixedPath = false
	srv.router.NotFound = http.HandlerFunc(notFound)
	srv.router.MethodNotAllowed = http.HandlerFunc(srv.methodNotAllowed)

	srv.router.Handle("OPTIONS", "/*path", srv.options)
	srv.router.Handler("GET", "/metrics", prometheus.Handler())
	srv.router.Handler("GET", "/static/*path", http.StripPrefix("/static", http.FileServer(sp.StaticFileSystem)))
	for name, newAPI := range versions {
		handlers, err := newAPI(HandlerParams{
			ServerParams: sp,
			Pool:         pool,
			Oven:         oven,
			Authorizer:   auth,
		})
		if err != nil {
			return nil, errgo.Notef(err, "cannot create API %s", name)
		}
		for _, h := range handlers {
			srv.router.Handle(h.Method, h.Path, h.Handle)
		}
	}
	return srv, nil
}

// Server serves the identity endpoints.
type Server struct {
	router *httprouter.Router
	pool   *store.Pool
}

// ServeHTTP implements http.Handler.
func (srv *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	defer func() {
		if v := recover(); v != nil {
			logger.Errorf("PANIC!: %v\n%s", v, debug.Stack())
			httprequest.WriteJSON(w, http.StatusInternalServerError, params.Error{
				Code:    "panic",
				Message: fmt.Sprintf("%v", v),
			})
		}
	}()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Bakery-Protocol-Version, Macaroons, X-Requested-With, Content-Type")
	w.Header().Set("Access-Control-Cache-Max-Age", "600")
	srv.router.ServeHTTP(w, req)
}

// Close  closes any resources held by this Handler.
func (s *Server) Close() {
	logger.Debugf("Closing Server")
	s.pool.Close()
}

// ServerParams contains configuration parameters for a server.
type ServerParams struct {
	// Place holds the meeting place that will be used for rendezvous
	// within the identity server.
	Place *meeting.Place

	// RootKeyStore holds the root key store that will be used to
	// store macaroon root keys within the identity server.
	RootKeyStore bakery.RootKeyStore

	// AuthUsername holds the username for admin login.
	AuthUsername string

	// AuthPassword holds the password for admin login.
	AuthPassword string

	// Key holds the keypair to use with the bakery service.
	Key *bakery.KeyPair

	// Location holds a URL representing the externally accessible
	// base URL of the service, without a trailing slash.
	Location string

	// Launchpad holds the address of the launchpad server to use to
	// get group information.
	Launchpad lpad.APIBase

	// MaxMgoSession holds the maximum number of concurrent mgo
	// sessions.
	MaxMgoSessions int

	// RequestTimeout holds the time to wait for a request to be able
	// to start.
	RequestTimeout time.Duration

	// PrivateAddr should hold a dialable address that will be used
	// for communication between identity servers. Note that this
	// should not contain a port.
	PrivateAddr string

	// IdentityProviders contains the set of identity providers that
	// should be initialised by the service.
	IdentityProviders []idp.IdentityProvider

	// DebugTeams contains the set of launchpad teams that may access
	// the restricted debug endpoints.
	DebugTeams []string

	// AdminAgentPublicKey contains the public key of the admin agent.
	AdminAgentPublicKey *bakery.PublicKey

	// StaticFileSystem contains an http.FileSystem that can be used
	// to serve static files.
	StaticFileSystem http.FileSystem

	// Template contains a set of templates that are used to generate
	// html output.
	Template *template.Template
}

type HandlerParams struct {
	ServerParams

	// Pool contains a store.Pool that is used in handlers to get
	// stores.
	Pool *store.Pool

	// Oven contains a bakery.Oven that should be used by handlers to
	// mint new macaroons.
	Oven *bakery.Oven

	// Authorizer contains an auth.Authroizer that should be used by
	// handlers to authorize requests.
	Authorizer *auth.Authorizer
}

//notFound is the handler that is called when a handler cannot be found
//for the requested endpoint.
func notFound(w http.ResponseWriter, req *http.Request) {
	WriteError(context.TODO(), w, errgo.WithCausef(nil, params.ErrNotFound, "not found: %s", req.URL.Path))
}

//methodNotAllowed is the handler that is called when a handler cannot
//be found for the requested endpoint with the request method, but
//there is a handler avaiable using a different method.
func (s *Server) methodNotAllowed(w http.ResponseWriter, req *http.Request) {
	// Check that the match method is not OPTIONS
	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "HEAD", "PATCH"} {
		if method == req.Method {
			continue
		}
		if h, _, _ := s.router.Lookup(method, req.URL.Path); h != nil {
			WriteError(context.TODO(), w, errgo.WithCausef(nil, params.ErrMethodNotAllowed, "%s not allowed for %s", req.Method, req.URL.Path))
			return
		}
	}
	notFound(w, req)
}

// options handles every OPTIONS request and always succeeds.
func (s *Server) options(http.ResponseWriter, *http.Request, httprouter.Params) {}
