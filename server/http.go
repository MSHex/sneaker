package server

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/alioygur/gores"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

type httpServer struct {
	sync.Mutex

	config   *Config
	sessions map[string]*serverSession
	discord  *DiscordIntegration
}

func newHttpServer(config *Config) *httpServer {
	return &httpServer{
		config:   config,
		sessions: make(map[string]*serverSession),
	}
}

func (h *httpServer) getServerMetadata(server *TacViewServerConfig) serverMetadata {
	result := serverMetadata{
		Name:            server.Name,
		GroundUnitModes: getGroundUnitModes(server),
		GCIs:            []gciMetadata{},
	}

	session, err := h.getOrCreateSession(server.Name)
	if err == nil {
		result.Players = session.GetPlayerList()
	}

	if h.discord != nil {
		gciList := h.discord.GetGCIList(server.Name)
		for _, gciState := range gciList {
			result.GCIs = append(result.GCIs, gciMetadata{
				Id:        gciState.DiscordId,
				Notes:     gciState.Notes,
				ExpiresAt: gciState.ExpiresAt,
			})
		}
	}

	return result
}

// Returns a list of available servers
func (h *httpServer) getServerList(w http.ResponseWriter, r *http.Request) {
	result := make([]serverMetadata, len(h.config.Servers))
	for idx, server := range h.config.Servers {
		// note: safe, we're not leaking this reference anywhere
		result[idx] = h.getServerMetadata(&server)

	}

	gores.JSON(w, 200, result)
}

type serverMetadata struct {
	Name            string           `json:"name"`
	GroundUnitModes []string         `json:"ground_unit_modes"`
	Players         []PlayerMetadata `json:"players"`
	GCIs            []gciMetadata    `json:"gcis"`
}

func getGroundUnitModes(config *TacViewServerConfig) []string {
	result := []string{}
	if config.EnableEnemyGroundUnits {
		result = append(result, "enemy")
	}
	if config.EnableFriendlyGroundUnits {
		result = append(result, "friendly")
	}
	return result
}

func (h *httpServer) ensureServer(w http.ResponseWriter, r *http.Request) *TacViewServerConfig {
	serverName := serverNameParam(r)

	var server *TacViewServerConfig
	for _, checkServer := range h.config.Servers {
		if checkServer.Name == serverName {
			server = &checkServer
			break
		}
	}
	if server == nil {
		gores.Error(w, 404, "server not found")
		return nil
	}
	return server
}

type gciMetadata struct {
	Id        string    `json:"id"`
	Notes     string    `json:"notes"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Return information about a specific server
func (h *httpServer) getServer(w http.ResponseWriter, r *http.Request) {
	server := h.ensureServer(w, r)
	if server == nil {
		return
	}

	gores.JSON(w, 200, h.getServerMetadata(server))
}

// serverNameParam returns the decoded server name from the URL. DCS server
// names (which DCSServerBot uses as sneaker server names) often contain
// characters such as "#", "|" or spaces that arrive percent-encoded.
func serverNameParam(r *http.Request) string {
	raw := chi.URLParam(r, "serverName")
	if decoded, err := url.PathUnescape(raw); err == nil {
		return decoded
	}
	return raw
}

var errNoServerFound = errors.New("no server by that name was found")

func (h *httpServer) getOrCreateSession(serverName string) (*serverSession, error) {
	h.Lock()
	defer h.Unlock()

	existingSession := h.sessions[serverName]
	if existingSession != nil {
		return existingSession, nil
	}

	var server *TacViewServerConfig
	for _, checkServer := range h.config.Servers {
		if checkServer.Name == serverName {
			server = &checkServer
			break
		}
	}
	if server == nil {
		return nil, errNoServerFound
	}

	var err error
	h.sessions[serverName], err = newServerSession(server)
	if err != nil {
		return nil, err
	}

	go h.sessions[serverName].run()
	return h.sessions[serverName], nil
}

// Streams events for a given server
func (h *httpServer) streamServerEvents(w http.ResponseWriter, r *http.Request) {
	session, err := h.getOrCreateSession(serverNameParam(r))
	if err != nil {
		if err == errNoServerFound {
			gores.Error(w, 404, "server not found")
			return
		}

		gores.Error(w, 500, "failed to find or create server session")
		return
	}

	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	sub, subCloser := session.addSub()
	defer subCloser()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Stop reverse proxies (nginx / Nginx Proxy Manager) from buffering the stream
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	write := func(msg []byte) error {
		outgoing := make([]byte, 0, len(msg)+8)
		outgoing = append(outgoing, "data: "...)
		outgoing = append(outgoing, msg...)
		outgoing = append(outgoing, '\n', '\n')
		if _, err := w.Write(outgoing); err != nil {
			return err
		}
		f.Flush()
		return nil
	}

	// Send initial data
	for _, msg := range session.getInitialState() {
		if write(msg) != nil {
			return
		}
	}
	f.Flush()

	// Periodic comment keeps idle connections (and proxies) alive
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()

	ctx := r.Context()
	for {
		select {
		case msg, ok := <-sub:
			if !ok {
				return
			}
			if write(msg) != nil {
				return
			}
		case <-keepAlive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			f.Flush()
		case <-ctx.Done():
			return
		}
	}
}

func Run(config *Config) error {
	server := newHttpServer(config)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		server.serveEmbeddedFile("index.html", w, r)
	})
	r.Get("/static/*", server.serveEmbeddedStaticAssets)
	r.Get("/api/servers", server.getServerList)
	r.Get("/api/servers/{serverName}", server.getServer)
	r.Get("/api/servers/{serverName}/events", server.streamServerEvents)

	if config.Discord != nil {
		server.discord = NewDiscordIntegration(server, config.Discord)
		r.Handle("/api/discord/*", server.discord)

		err := server.discord.Setup()
		if err != nil {
			log.Panicf("Failed to setup discord integration: %v", err)
		}
	}

	log.Printf("Starting up %v Tacview clients", len(config.Servers))
	for _, serverConfig := range config.Servers {
		server.getOrCreateSession(serverConfig.Name)
	}

	log.Printf("Sneaker web UI listening on http://%v", config.Bind)
	httpServer := &http.Server{
		Addr:              config.Bind,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return httpServer.ListenAndServe()
}
