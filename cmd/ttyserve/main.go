package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ttyserve/internal/auth"
	"ttyserve/internal/config"
	"ttyserve/internal/server"
	"ttyserve/internal/session"
)

func main() {
	def := config.Default()

	// Every config option has a CLI flag with the same name as its YAML key.
	// Flag defaults mirror config defaults so `-help` documents them; only
	// flags the user actually set override the config file.
	cfgPath := flag.String("config", "", "path to YAML config file")
	listen := flag.String("listen", def.Listen, "IP address, interface name, or unix://<path> socket to listen on (default: all interfaces)")
	port := flag.Int("port", def.Port, "TCP port to listen on")
	command := flag.String("command", def.Command, "shell-style command line run for each terminal")
	workingDir := flag.String("working_dir", def.WorkingDir, "working directory for terminals (default: server's cwd)")
	env := flag.String("env", strings.Join(def.Env, ","), "extra environment variables, comma-separated KEY=VALUE pairs")
	sessionPersistence := flag.Bool("session_persistence", def.SessionPersistence, "keep sessions alive across disconnects")
	persistenceMode := flag.String("persistence_mode", string(def.PersistenceMode), "how sessions are tied to a client: 'user', 'short_term' or 'proxy_header'")
	multiSession := flag.Bool("multi_session", def.MultiSession, "enable multiple sessions (tabs) per client")
	maxSessions := flag.Int("max_sessions_per_client", def.MaxSessionsPerClient, "cap on tabs per client (0 = unlimited)")
	tabBarPosition := flag.String("tab_bar_position", def.TabBarPosition, "tab bar position: 'top' or 'right'")
	users := flag.String("users", "", "basic-auth users for 'user' mode, comma-separated name:password pairs")
	authRealm := flag.String("auth_realm", def.AuthRealm, "HTTP basic-auth realm")
	proxyHeaderName := flag.String("proxy_header_name", def.ProxyHeaderName, "proxy_header mode: header carrying the user identity")
	idleTimeout := flag.Duration("idle_timeout", def.IdleTimeout, "short_term: reap sessions with no connection for this long")
	cookieName := flag.String("cookie_name", def.CookieName, "short_term session cookie name")
	cookieSecure := flag.Bool("cookie_secure", def.CookieSecure, "mark the session cookie Secure (HTTPS only)")
	allowOrigins := flag.String("allow_origins", strings.Join(def.AllowOrigins, ","), "extra websocket Origins allowed, comma-separated ('*' = any)")
	tlsCert := flag.String("tls_cert_file", def.TLSCertFile, "TLS certificate file (enables HTTPS)")
	tlsKey := flag.String("tls_key_file", def.TLSKeyFile, "TLS key file")
	writeEnabled := flag.Bool("write_enabled", def.WriteEnabled, "allow keyboard input (false = read-only terminals)")
	maxClients := flag.Int("max_clients_per_session", def.MaxClientsPerSession, "concurrent viewers per session (0 = unlimited)")
	pingInterval := flag.Duration("ping_interval", def.PingInterval, "websocket keepalive ping period")
	scrollback := flag.Int("scrollback_bytes", def.ScrollbackBytes, "server-side replay buffer per session")
	fontSize := flag.Int("font_size", def.FontSize, "terminal font size in px")
	enableGraphics := flag.Bool("enable_graphics", def.EnableGraphics, "inline graphics in the terminal (sixel + iTerm2 image protocol)")
	title := flag.String("title", def.Title, "browser page title")
	closeOnExit := flag.Bool("close_on_exit", def.CloseOnExit, "remove a session when its command exits")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	var flagErr error
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "listen":
			cfg.Listen = *listen
		case "port":
			cfg.Port = *port
		case "command":
			cfg.Command = *command
		case "working_dir":
			cfg.WorkingDir = *workingDir
		case "env":
			cfg.Env = splitCSV(*env)
		case "session_persistence":
			cfg.SessionPersistence = *sessionPersistence
		case "persistence_mode":
			cfg.PersistenceMode = config.PersistenceMode(*persistenceMode)
		case "multi_session":
			cfg.MultiSession = *multiSession
		case "max_sessions_per_client":
			cfg.MaxSessionsPerClient = *maxSessions
		case "tab_bar_position":
			cfg.TabBarPosition = *tabBarPosition
		case "users":
			us, err := parseUsers(*users)
			if err != nil {
				flagErr = err
				return
			}
			cfg.Users = us
		case "auth_realm":
			cfg.AuthRealm = *authRealm
		case "proxy_header_name":
			cfg.ProxyHeaderName = *proxyHeaderName
		case "idle_timeout":
			cfg.IdleTimeout = *idleTimeout
		case "cookie_name":
			cfg.CookieName = *cookieName
		case "cookie_secure":
			cfg.CookieSecure = *cookieSecure
		case "allow_origins":
			cfg.AllowOrigins = splitCSV(*allowOrigins)
		case "tls_cert_file":
			cfg.TLSCertFile = *tlsCert
		case "tls_key_file":
			cfg.TLSKeyFile = *tlsKey
		case "write_enabled":
			cfg.WriteEnabled = *writeEnabled
		case "max_clients_per_session":
			cfg.MaxClientsPerSession = *maxClients
		case "ping_interval":
			cfg.PingInterval = *pingInterval
		case "scrollback_bytes":
			cfg.ScrollbackBytes = *scrollback
		case "font_size":
			cfg.FontSize = *fontSize
		case "enable_graphics":
			cfg.EnableGraphics = *enableGraphics
		case "title":
			cfg.Title = *title
		case "close_on_exit":
			cfg.CloseOnExit = *closeOnExit
		}
	})
	if flagErr != nil {
		log.Fatalf("flags: %v", flagErr)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config: %v", err)
	}

	authn, err := auth.New(cfg)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	mgr := session.NewManager(cfg)
	defer mgr.Shutdown()

	srv, err := server.New(cfg, authn, mgr)
	if err != nil {
		log.Fatalf("server: %v", err)
	}

	httpSrv := &http.Server{Handler: srv.Handler()}

	specs, err := cfg.ListenSpecs()
	if err != nil {
		log.Fatalf("%v", err)
	}
	for _, sp := range specs {
		if sp.Network == "unix" {
			// Remove a stale socket left by a previous run (but never a
			// regular file that happens to be at that path).
			if fi, err := os.Stat(sp.Address); err == nil && fi.Mode()&os.ModeSocket != 0 {
				_ = os.Remove(sp.Address)
			}
		}
		ln, err := net.Listen(sp.Network, sp.Address)
		if err != nil {
			log.Fatalf("listen %s: %v", sp.Address, err)
		}
		// Mirror the user's listen syntax: unix://<path> for sockets (with
		// an explicit tls= field), http(s)://host:port for TCP.
		if sp.Network == "unix" {
			log.Printf("ttyserve listening on unix://%s (tls=%v persistence=%v mode=%s multi=%v)",
				ln.Addr(), sp.TLS, cfg.SessionPersistence, cfg.PersistenceMode, cfg.MultiSession)
		} else {
			scheme := "http"
			if sp.TLS {
				scheme = "https"
			}
			log.Printf("ttyserve listening on %s://%s (persistence=%v mode=%s multi=%v)",
				scheme, ln.Addr(), cfg.SessionPersistence, cfg.PersistenceMode, cfg.MultiSession)
		}
		go func(sp config.ListenSpec, ln net.Listener) {
			var err error
			if sp.TLS {
				err = httpSrv.ServeTLS(ln, cfg.TLSCertFile, cfg.TLSKeyFile)
			} else {
				err = httpSrv.Serve(ln)
			}
			if err != nil && err != http.ErrServerClosed {
				log.Fatalf("serve %s: %v", ln.Addr(), err)
			}
		}(sp, ln)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	mgr.Shutdown()
}

// splitCSV splits a comma-separated flag value, trimming whitespace and
// dropping empty items.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseUsers parses "name:password,name2:password2".
func parseUsers(s string) ([]config.User, error) {
	var out []config.User
	for _, p := range splitCSV(s) {
		name, pass, ok := strings.Cut(p, ":")
		if !ok || name == "" {
			return nil, fmt.Errorf("users: %q is not name:password", p)
		}
		out = append(out, config.User{Name: name, Password: pass})
	}
	return out, nil
}
