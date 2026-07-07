package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// PersistenceMode selects how sessions are tied to a client.
type PersistenceMode string

const (
	// PersistByUser ties sessions to an HTTP basic-auth user. Sessions live
	// until explicitly killed (or the server restarts).
	PersistByUser PersistenceMode = "user"
	// PersistShortTerm ties sessions to a browser cookie. Sessions are reaped
	// after IdleTimeout of no active websocket connection.
	PersistShortTerm PersistenceMode = "short_term"
	// PersistProxyHeader ties sessions to the value of a request header set
	// by a trusted reverse proxy (e.g. X-Forwarded-User). Like "user" mode,
	// but authentication is the proxy's job. Only safe when clients cannot
	// reach ttyserve directly — bind to unix:// or 127.0.0.1.
	PersistProxyHeader PersistenceMode = "proxy_header"
)

// User is a single basic-auth credential.
type User struct {
	Name     string `yaml:"name"`
	Password string `yaml:"password"`
}

// Config is the full application configuration.
type Config struct {
	// Listen is what to bind: an IP address ("127.0.0.1", "::1"), an
	// interface name ("eth0"), or a unix socket ("unix:///run/tty.sock").
	// Empty = all interfaces. TLS applies to any listener whenever
	// tls_cert_file/tls_key_file are configured.
	Listen string `yaml:"listen"`

	// Port is the TCP port to listen on (ignored for unix sockets).
	Port int `yaml:"port"`

	// Command run for each terminal session, as a single shell-style line
	// (e.g. "/usr/bin/tmux new -A -s main"). Quote arguments that contain
	// spaces. Validate splits it into Command (argv[0]) and Args.
	Command string   `yaml:"command"`
	Args    []string `yaml:"-"`

	// Working directory for spawned terminals. Empty = server's cwd.
	WorkingDir string `yaml:"working_dir"`

	// Extra environment variables (KEY=VALUE) applied to each terminal.
	Env []string `yaml:"env"`

	// SessionPersistence turns persistence on/off entirely. When false, a
	// session is bound to one websocket and dies on disconnect.
	SessionPersistence bool `yaml:"session_persistence"`

	// PersistenceMode is "user" or "short_term". Only meaningful when
	// SessionPersistence is true.
	PersistenceMode PersistenceMode `yaml:"persistence_mode"`

	// MultiSession enables multiple terminal sessions (tabs) per client.
	// When false, each client gets exactly one session and no tab bar.
	MultiSession bool `yaml:"multi_session"`

	// MaxSessionsPerClient caps tabs per client. 0 = unlimited.
	MaxSessionsPerClient int `yaml:"max_sessions_per_client"`

	// TabBarPosition is "top" or "right".
	TabBarPosition string `yaml:"tab_bar_position"`

	// --- Auth (PersistByUser) ---
	Users []User `yaml:"users"`

	// AuthRealm is the basic-auth realm shown to browsers.
	AuthRealm string `yaml:"auth_realm"`

	// --- Proxy header identity (PersistProxyHeader) ---
	// ProxyHeaderName is the request header whose value identifies the user.
	ProxyHeaderName string `yaml:"proxy_header_name"`

	// --- Short term sessions ---
	// IdleTimeout is how long a short-term session survives with no active
	// websocket before being reaped. Also used as the cookie max-age.
	IdleTimeout time.Duration `yaml:"idle_timeout"`

	// CookieName for short-term session identification.
	CookieName string `yaml:"cookie_name"`

	// CookieSecure marks the session cookie Secure (HTTPS only). Set true in
	// production behind TLS.
	CookieSecure bool `yaml:"cookie_secure"`

	// AllowOrigins lists extra Origin values permitted to open websockets,
	// beyond same-host which is always allowed. Use ["*"] to allow all
	// (matches previous behavior; not recommended with cookie auth).
	AllowOrigins []string `yaml:"allow_origins"`

	// --- TLS ---
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`

	// --- Behaviour, ttyd-inspired ---
	// Readonly disables client keyboard input: terminals become
	// view/share-only.
	Readonly bool `yaml:"readonly"`

	// URLArg appends the page URL's query parameters to the command's
	// arguments: /?arg1&arg2=5 runs "command arg1 arg2=5". SECURITY: this
	// lets any client who can reach the server influence the command line.
	URLArg bool `yaml:"url_arg"`

	// URLEnv turns the page URL's query parameters into extra environment
	// variables: /?arg1&arg2=5 runs the command with arg1= and arg2=5 set.
	// Mutually exclusive with URLArg. Same security caveat.
	URLEnv bool `yaml:"url_env"`

	// MaxClientsPerSession limits concurrent websockets attached to one
	// session (allows shared viewing). 0 = unlimited.
	MaxClientsPerSession int `yaml:"max_clients_per_session"`

	// PingInterval is the websocket keepalive ping period.
	PingInterval time.Duration `yaml:"ping_interval"`

	// ScrollbackLines is how many lines of output are buffered server-side so
	// a reconnecting client can repaint the screen. 0 disables replay.
	ScrollbackBytes int `yaml:"scrollback_bytes"`

	// FontSize is the terminal font size in CSS pixels.
	FontSize int `yaml:"font_size"`

	// DisableHyperlink turns off clickable links in the terminal (both
	// auto-detected URLs and OSC 8 explicit hyperlinks).
	DisableHyperlink bool `yaml:"disable_hyperlink"`

	// MiddleclickPaste pastes the clipboard on middle click (like a Linux
	// terminal). Set false to disable.
	MiddleclickPaste bool `yaml:"middleclick_paste"`

	// EnableGraphics loads the xterm.js image addon, enabling inline
	// graphics via sixel and the iTerm2 inline image protocol. When false
	// the decoder isn't loaded and sixel support is not advertised to
	// applications.
	EnableGraphics bool `yaml:"enable_graphics"`

	// Title is the browser/page title.
	Title string `yaml:"title"`

	// CloseOnExit: when the shell process exits, remove the session.
	CloseOnExit bool `yaml:"close_on_exit"`
}

// Default returns a Config populated with reasonable defaults.
func Default() Config {
	return Config{
		Listen:               "", // all interfaces
		Port:                 7681,
		Command:              defaultShell(),
		SessionPersistence:   true,
		PersistenceMode:      PersistShortTerm,
		MultiSession:         true,
		MaxSessionsPerClient: 0,
		TabBarPosition:       "top",
		AuthRealm:            "ttyserve",
		ProxyHeaderName:      "X-Forwarded-User",
		IdleTimeout:          5 * time.Minute,
		CookieName:           "ttyserve_session",
		CookieSecure:         false,
		Readonly:             false,
		MaxClientsPerSession: 0,
		PingInterval:         20 * time.Second,
		ScrollbackBytes:      256 * 1024,
		FontSize:             14,
		EnableGraphics:       true,
		DisableHyperlink:     false,
		MiddleclickPaste:     true,
		Title:                "TtyServe",
		CloseOnExit:          true,
	}
}

func defaultShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/bash"
}

// Load reads a YAML config file, overlaying it on top of defaults. It does
// NOT validate: the caller applies any CLI overrides first, then calls
// Validate once.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// Validate checks config invariants and normalizes a few fields.
func (c *Config) Validate() error {
	argv, err := splitCommand(c.Command)
	if err != nil {
		return fmt.Errorf("command: %w", err)
	}
	if len(argv) == 0 {
		return fmt.Errorf("command must not be empty")
	}
	c.Command = argv[0]
	c.Args = argv[1:]
	switch c.TabBarPosition {
	case "top", "right":
	case "":
		c.TabBarPosition = "top"
	default:
		return fmt.Errorf("tab_bar_position must be 'top' or 'right', got %q", c.TabBarPosition)
	}
	if c.SessionPersistence {
		switch c.PersistenceMode {
		case PersistByUser:
			if len(c.Users) == 0 {
				return fmt.Errorf("persistence_mode 'user' requires at least one user")
			}
		case PersistShortTerm:
			if c.IdleTimeout <= 0 {
				return fmt.Errorf("idle_timeout must be > 0 for short_term mode")
			}
		case PersistProxyHeader:
			if c.ProxyHeaderName == "" {
				c.ProxyHeaderName = "X-Forwarded-User"
			}
		case "":
			c.PersistenceMode = PersistShortTerm
		default:
			return fmt.Errorf("persistence_mode must be 'user', 'short_term' or 'proxy_header', got %q", c.PersistenceMode)
		}
	}
	if c.CookieName == "" {
		c.CookieName = "ttyserve_session"
	}
	if c.PingInterval <= 0 {
		c.PingInterval = 20 * time.Second
	}
	if c.FontSize <= 0 {
		c.FontSize = 14
	}
	if c.Port == 0 {
		c.Port = 7681
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be 1-65535, got %d", c.Port)
	}
	if c.URLArg && c.URLEnv {
		return fmt.Errorf("url_arg and url_env are mutually exclusive")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return fmt.Errorf("tls_cert_file and tls_key_file must both be set or both empty")
	}
	return nil
}

// splitCommand tokenizes a command line shell-style: words separated by
// spaces/tabs, with single quotes, double quotes, and backslash escapes
// (outside single quotes) grouping words together.
func splitCommand(s string) ([]string, error) {
	var argv []string
	var cur []rune
	var quote rune
	escaped, inWord := false, false
	for _, r := range s {
		switch {
		case escaped:
			cur = append(cur, r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
			inWord = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur = append(cur, r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t':
			if inWord {
				argv = append(argv, string(cur))
				cur = cur[:0]
				inWord = false
			}
		default:
			cur = append(cur, r)
			inWord = true
		}
	}
	if escaped {
		return nil, fmt.Errorf("trailing backslash")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unclosed quote")
	}
	if inWord {
		argv = append(argv, string(cur))
	}
	return argv, nil
}

// TLSEnabled reports whether TLS files are configured.
func (c *Config) TLSEnabled() bool {
	return c.TLSCertFile != "" && c.TLSKeyFile != ""
}

// ListenSpec describes one listener to open.
type ListenSpec struct {
	Network string // "tcp" or "unix"
	Address string // host:port, or socket path
	TLS     bool   // serve TLS on this listener
}

// ListenSpecs resolves Listen+Port to the listeners to bind: all interfaces
// when Listen is empty, the address itself for an IP, every address of a
// named interface, or a unix socket for unix://<path>. TLS is decided
// uniformly by the TLS config, never by the listen syntax.
func (c *Config) ListenSpecs() ([]ListenSpec, error) {
	tls := c.TLSEnabled()
	if path, ok := strings.CutPrefix(c.Listen, "unix://"); ok {
		if path == "" {
			return nil, fmt.Errorf("listen: unix:// requires a socket path")
		}
		return []ListenSpec{{Network: "unix", Address: path, TLS: tls}}, nil
	}
	if strings.HasPrefix(c.Listen, "unixs://") {
		return nil, fmt.Errorf("listen: unixs:// is not a thing; use unix:// — TLS is enabled by tls_cert_file/tls_key_file")
	}

	port := strconv.Itoa(c.Port)
	if c.Listen == "" {
		return []ListenSpec{{Network: "tcp", Address: ":" + port, TLS: tls}}, nil
	}
	if ip := net.ParseIP(c.Listen); ip != nil {
		return []ListenSpec{{Network: "tcp", Address: net.JoinHostPort(c.Listen, port), TLS: tls}}, nil
	}
	if _, _, err := net.SplitHostPort(c.Listen); err == nil {
		return nil, fmt.Errorf("listen %q looks like host:port; put the address/interface in 'listen' and the port in 'port'", c.Listen)
	}
	ifi, err := net.InterfaceByName(c.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen: %q is neither an IP address, an interface name, nor a unix:// socket", c.Listen)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, fmt.Errorf("listen: addresses of %s: %w", c.Listen, err)
	}
	var out []ListenSpec
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		host := ipn.IP.String()
		if ipn.IP.To4() == nil && ipn.IP.IsLinkLocalUnicast() {
			host += "%" + c.Listen // link-local v6 needs the zone
		}
		out = append(out, ListenSpec{Network: "tcp", Address: net.JoinHostPort(host, port), TLS: tls})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("listen: interface %s has no usable addresses", c.Listen)
	}
	return out, nil
}
