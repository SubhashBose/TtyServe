package main

import (
	"context"
	//"flag"
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

	"github.com/SubhashBose/GoPkg-selfupdater"
	flag "github.com/spf13/pflag"
)

var version = "0.3"
var buildDate = ""

func main() {
	def := config.Default()

	// Every config option has a CLI flag with the same name as its YAML key.
	// Flag defaults mirror config defaults so `-help` documents them; only
	// flags the user actually set override the config file.
	command := flag.StringP("command", "c", def.Command, "shell-style command line run for each terminal including args")
	workingDir := flag.StringP("working-dir", "w", def.WorkingDir, "working directory for terminal command (default: server's cwd)")
	env := flag.StringP("env", "e", strings.Join(def.Env, ","), "extra environment variables for terminal command, comma-separated `KEY=VALUE` pairs.\nAlso supports HTTP header value substitution as ${header.KEY}")
	cfgPath := flag.StringP("config", "C", "", "path to YAML config `file`")
	listen := flag.StringP("listen", "l", def.Listen, "IP `address`, interface name, or unix://<path> socket to listen on (default: all interfaces)")
	port := flag.IntP("port", "p", def.Port, "TCP `port` to listen on")
	socketPerm := flag.String("socket-perm", def.SocketPerm, "unix socket permissions: `mode[:user[:group]]`, e.g. 660 or 0660::www-data")
	multiSession := flag.BoolP("multi-session", "M", def.MultiSession, "enable multiple sessions (tabs) per client")
	maxSessions := flag.Int("max-sessions-per-client", def.MaxSessionsPerClient, "cap on tabs per client (0 = unlimited)")
	closeOnExit := flag.Bool("close-on-exit", def.CloseOnExit, "remove a session/tab when its command exits")
	autoRespawn := flag.Bool("auto-respawn", def.AutoRespawn, "start a new session immediately when the last one ends")
	sessionPersistence := flag.BoolP("session-persistence", "P", def.SessionPersistence, "keep sessions alive across disconnects")
	persistenceMode := flag.String("persistence-mode", string(def.PersistenceMode), "how sessions are tied to a client: 'short_term', 'user' or 'proxy_header'")
	idleTimeout := flag.Duration("idle-timeout", def.IdleTimeout, "for 'short_term' mode: reap sessions with no connection for this long")
	users := flag.StringP("users", "u", "", "HTTP basic-auth users for 'user' mode, comma-separated `name:password` pairs, each user gets their own session.\nWhen session-persistence=false, user(s) is a plain access gate (login required, sessions stay ephemeral)")
	authRealm := flag.String("auth-realm", def.AuthRealm, "HTTP basic-auth realm")
	proxyHeaderName := flag.String("proxy-header-name", def.ProxyHeaderName, "header `key` carrying the user identity for 'proxy_header' persistance mode, each user ID gets their own\nsession")
	scrollback := flag.Int("scrollback-bytes", def.ScrollbackBytes, "server-side replay buffer per persistent session")
	allowSharing := flag.Bool("allow-sharing", def.AllowSharing, "allow sharing a terminal tab with another authenticated user via a link (need persistent-mode)")
	maxClients := flag.Int("max-clients-per-session", def.MaxClientsPerSession, "concurrent viewers per session (0 = unlimited)")
	cookieName := flag.String("cookie-name", def.CookieName, "short_term session cookie name")
	cookieSecure := flag.Bool("cookie-secure", def.CookieSecure, "mark the session cookie Secure (HTTPS only)")
	allowOrigins := flag.String("allow-origins", strings.Join(def.AllowOrigins, ","), "extra websocket `Origins` allowed, comma-separated ('*' = any)")
	tlsCert := flag.String("tls-cert-file", def.TLSCertFile, "TLS certificate `file` (enables HTTPS)")
	tlsKey := flag.String("tls-key-file", def.TLSKeyFile, "TLS key `file`")
	readonly := flag.BoolP("readonly", "r", def.Readonly, "read-only terminals: no client input accepted")
	urlArg := flag.Bool("url-arg", def.URLArg, "append URL query parameters to the command arguments (see security notes)\ne.g., /?arg1&arg2=5 -> 'command arg1 arg2=5'")
	urlEnv := flag.Bool("url-env", def.URLEnv, "turn URL query parameters into extra environment variables (see security notes)\ne.g., /?arg1&arg2=5 -> sets ENV var as 'arg1= arg2=5'")
	pingInterval := flag.Duration("ping-interval", def.PingInterval, "websocket keepalive ping period")
	fontSize := flag.IntP("font-size", "F", def.FontSize, "terminal font size in px")
	domRenderer := flag.Bool("dom-renderer", def.DOMRenderer, "use the DOM text renderer instead of canvas (for mobile GPU issues)")
	enableGraphics := flag.Bool("enable-graphics", def.EnableGraphics, "inline graphics in the terminal (sixel + iTerm2 image protocol)")
	disableHyperlink := flag.Bool("disable-hyperlink", def.DisableHyperlink, "turn off clickable links in the terminal")
	middleclickPaste := flag.Bool("middleclick-paste", def.MiddleclickPaste, "paste clipboard on middle click")
	clipboardWrite := flag.Bool("clipboard-write", def.ClipboardWrite, "let terminal programs set the system clipboard via OSC 52 (tmux, vim, ai-harnesses, etc.)")
	bell := flag.String("bell", def.Bell, "terminal bell (BEL / \\a): 'none', 'sound', 'visual' or 'both'")
	title := flag.StringP("title", "t", def.Title, "browser page title")
	favicon := flag.String("favicon", def.Favicon, "custom favicon: `file` path or data: URI (default: built-in icon)")
	tabBarPosition := flag.String("tab-bar-position", def.TabBarPosition, "tab bar `position`: 'top' or 'right'")
	tabShowPsname := flag.Bool("tab-show-psname", def.TabShowPsname, "include the foreground process name in auto tab titles")
	tabShowCwd := flag.Bool("tab-show-cwd", def.TabShowCwd, "include the working directory basename in auto tab titles")
	tabShowPS1 := flag.Bool("tab-show-ps1", def.TabShowPS1, "title tabs from the shell's OSC 0/2 window title (overrides tab-show-psname/cwd)")
	tabTitle := flag.String("tab-title", def.TabTitle, "fixed tab title, overrides all auto-titling")
	showVersion := flag.BoolP("version", "V", false, "print version and exit")
	doUpgrade := flag.Bool("upgrade", false, "self-upgrade the binary to the latest release and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "TtyServe v%s - Serve Terminal on the web\n\n", version)
		fmt.Fprintf(os.Stderr, "Command line Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s --command \"<program> [args...]\" [options]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  or\n")
		fmt.Fprintf(os.Stderr, "  %s --config config.yaml\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Can be configured through CLI options, or YAML config file, or combined. Config file keys names are same as CLI options.\n")
		fmt.Fprintf(os.Stderr, "To turn off a boolean flag which defaults to true, set it false as '--option=false'\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n\nFull documentation: https://github.com/SubhashBose/TtyServe\n")
	}

	flag.CommandLine.SortFlags = false
	flag.Parse()

	// --version and --upgrade short-circuit before any config/server work.
	if *showVersion {
		if buildDate != "" {
			fmt.Printf("TtyServe version %s (built %s)\n", version, buildDate)
		} else {
			fmt.Printf("TtyServe version %s\n", version)
		}
		return
	}
	if *doUpgrade {
		runUpgrade()
		return
	}

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
		case "socket-perm":
			cfg.SocketPerm = *socketPerm
		case "command":
			cfg.Command = *command
		case "working-dir":
			cfg.WorkingDir = *workingDir
		case "env":
			cfg.Env = splitCSV(*env)
		case "session-persistence":
			cfg.SessionPersistence = *sessionPersistence
		case "persistence-mode":
			cfg.PersistenceMode = config.PersistenceMode(*persistenceMode)
		case "multi-session":
			cfg.MultiSession = *multiSession
		case "max-sessions-per-client":
			cfg.MaxSessionsPerClient = *maxSessions
		case "tab-bar-position":
			cfg.TabBarPosition = *tabBarPosition
		case "users":
			us, err := parseUsers(*users)
			if err != nil {
				flagErr = err
				return
			}
			cfg.Users = us
		case "auth-realm":
			cfg.AuthRealm = *authRealm
		case "proxy-header-name":
			cfg.ProxyHeaderName = *proxyHeaderName
		case "idle-timeout":
			cfg.IdleTimeout = *idleTimeout
		case "cookie-name":
			cfg.CookieName = *cookieName
		case "cookie-secure":
			cfg.CookieSecure = *cookieSecure
		case "allow-origins":
			cfg.AllowOrigins = splitCSV(*allowOrigins)
		case "tls-cert-file":
			cfg.TLSCertFile = *tlsCert
		case "tls-key-file":
			cfg.TLSKeyFile = *tlsKey
		case "readonly":
			cfg.Readonly = *readonly
		case "url-arg":
			cfg.URLArg = *urlArg
		case "url-env":
			cfg.URLEnv = *urlEnv
		case "max-clients-per-session":
			cfg.MaxClientsPerSession = *maxClients
		case "ping-interval":
			cfg.PingInterval = *pingInterval
		case "scrollback-bytes":
			cfg.ScrollbackBytes = *scrollback
		case "font-size":
			cfg.FontSize = *fontSize
		case "dom-renderer":
			cfg.DOMRenderer = *domRenderer
		case "enable-graphics":
			cfg.EnableGraphics = *enableGraphics
		case "disable-hyperlink":
			cfg.DisableHyperlink = *disableHyperlink
		case "middleclick-paste":
			cfg.MiddleclickPaste = *middleclickPaste
		case "clipboard-write":
			cfg.ClipboardWrite = *clipboardWrite
		case "bell":
			cfg.Bell = *bell
		case "title":
			cfg.Title = *title
		case "favicon":
			cfg.Favicon = *favicon
		case "close-on-exit":
			cfg.CloseOnExit = *closeOnExit
		case "auto-respawn":
			cfg.AutoRespawn = *autoRespawn
		case "allow-sharing":
			cfg.AllowSharing = *allowSharing
		case "tab-show-psname":
			cfg.TabShowPsname = *tabShowPsname
		case "tab-show-cwd":
			cfg.TabShowCwd = *tabShowCwd
		case "tab-show-ps1":
			cfg.TabShowPS1 = *tabShowPS1
		case "tab-title":
			cfg.TabTitle = *tabTitle
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

	httpSrv := &http.Server{
		Handler: srv.Handler(),
		// Bound how long a client may dribble request headers (slow-loris);
		// deliberately no overall Read/WriteTimeout — long-lived websockets
		// manage their own deadlines.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Startup summary: one line for the version + effective options, then one
	// line per bound listener below.
	v := version
	if buildDate != "" {
		v += " (built " + buildDate + ")"
	}
	log.Printf("TtyServe v%s starting", v)
	log.Printf("options: persistence=%v mode=%s multi-session=%v readonly=%v sharing=%v tls=%v",
		cfg.SessionPersistence, cfg.PersistenceMode, cfg.MultiSession,
		cfg.Readonly, cfg.AllowSharing && cfg.SessionPersistence, cfg.TLSEnabled())

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
		// Apply socket-perm (already validated) before serving, so no
		// connection is ever accepted under the wrong permissions.
		if sp.Network == "unix" {
			if perm, _ := cfg.ParseSocketPerm(); perm != nil {
				if perm.UID != -1 || perm.GID != -1 {
					if err := os.Chown(sp.Address, perm.UID, perm.GID); err != nil {
						log.Fatalf("socket-perm: chown %s: %v", sp.Address, err)
					}
				}
				if err := os.Chmod(sp.Address, perm.Mode); err != nil {
					log.Fatalf("socket-perm: chmod %s: %v", sp.Address, err)
				}
			}
		}
		// Mirror the user's listen syntax: unix://<path> for sockets,
		// http(s)://host:port for TCP (the options line above carries tls=).
		if sp.Network == "unix" {
			log.Printf("listening on unix://%s", ln.Addr())
		} else {
			scheme := "http"
			if sp.TLS {
				scheme = "https"
			}
			log.Printf("listening on %s://%s", scheme, ln.Addr())
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

func runUpgrade() {
	cfg := selfupdate.Config{
		RepoURL:        "https://github.com/SubhashBose/TtyServe",
		BinaryPrefix:   "ttyserve-",
		OSSep:          "-",
		CurrentVersion: version, // your build-time var
	}

	fmt.Printf("Current version: %s\nChecking for updates…", version)

	res, err := selfupdate.Update(cfg)

	if res.LatestVersion != "" {
		fmt.Printf(" Latest version: %s\n", res.LatestVersion)
	} else {
		fmt.Printf("\n")
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "Update failed:", err)
		os.Exit(1)
	}

	if !res.Updated {
		fmt.Printf("Already up to date (latest: %s)\n", res.LatestVersion)
		return
	}

	fmt.Printf("Successfully updated to v%s (asset: %s)\nPlease restart any running instances of the program.\n",
		res.LatestVersion, res.AssetName)
}
