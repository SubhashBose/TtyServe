package config

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// headerEnvRe matches ${header.NAME} placeholders in env values. NAME is a
// standard HTTP header token (letters, digits, hyphen, underscore).
var headerEnvRe = regexp.MustCompile(`\$\{header\.([A-Za-z0-9_-]+)\}`)

// ExpandHeaderEnv replaces ${header.NAME} placeholders in each env entry with
// get(NAME) — typically a request-header lookup. A missing/empty header
// expands to the empty string. Entries with no placeholder are unchanged.
func ExpandHeaderEnv(env []string, get func(string) string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, len(env))
	for i, e := range env {
		out[i] = headerEnvRe.ReplaceAllStringFunc(e, func(m string) string {
			return get(headerEnvRe.FindStringSubmatch(m)[1])
		})
	}
	return out
}

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
	// tls-cert-file/tls-key-file are configured.
	Listen string `yaml:"listen"`

	// Port is the TCP port to listen on (ignored for unix sockets).
	Port int `yaml:"port"`

	// Command run for each terminal session, as a single shell-style line
	// (e.g. "/usr/bin/tmux new -A -s main"). Quote arguments that contain
	// spaces. Validate splits it into Command (argv[0]) and Args.
	Command string   `yaml:"command"`
	Args    []string `yaml:"-"`

	// Working directory for spawned terminals. Empty = server's cwd.
	WorkingDir string `yaml:"working-dir"`

	// Extra environment variables (KEY=VALUE) applied to each terminal.
	Env []string `yaml:"env"`

	// SessionPersistence turns persistence on/off entirely. When false, a
	// session is bound to one websocket and dies on disconnect.
	SessionPersistence bool `yaml:"session-persistence"`

	// PersistenceMode is "user" or "short_term". Only meaningful when
	// SessionPersistence is true.
	PersistenceMode PersistenceMode `yaml:"persistence-mode"`

	// MultiSession enables multiple terminal sessions (tabs) per client.
	// When false, each client gets exactly one session and no tab bar.
	MultiSession bool `yaml:"multi-session"`

	// MaxSessionsPerClient caps tabs per client. 0 = unlimited.
	MaxSessionsPerClient int `yaml:"max-sessions-per-client"`

	// TabBarPosition is "top" or "right".
	TabBarPosition string `yaml:"tab-bar-position"`

	// --- Auth (PersistByUser) ---
	Users []User `yaml:"users"`

	// AuthRealm is the basic-auth realm shown to browsers.
	AuthRealm string `yaml:"auth-realm"`

	// --- Proxy header identity (PersistProxyHeader) ---
	// ProxyHeaderName is the request header whose value identifies the user.
	ProxyHeaderName string `yaml:"proxy-header-name"`

	// --- Short term sessions ---
	// IdleTimeout is how long a short-term session survives with no active
	// websocket before being reaped. Also used as the cookie max-age.
	IdleTimeout time.Duration `yaml:"idle-timeout"`

	// CookieName for short-term session identification.
	CookieName string `yaml:"cookie-name"`

	// CookieSecure marks the session cookie Secure (HTTPS only). Set true in
	// production behind TLS.
	CookieSecure bool `yaml:"cookie-secure"`

	// AllowOrigins lists extra Origin values permitted to open websockets,
	// beyond same-host which is always allowed. Use ["*"] to allow all
	// (matches previous behavior; not recommended with cookie auth).
	AllowOrigins []string `yaml:"allow-origins"`

	// --- TLS ---
	TLSCertFile string `yaml:"tls-cert-file"`
	TLSKeyFile  string `yaml:"tls-key-file"`

	// --- Behaviour, ttyd-inspired ---
	// Readonly disables client keyboard input: terminals become
	// view/share-only.
	Readonly bool `yaml:"readonly"`

	// URLArg appends the page URL's query parameters to the command's
	// arguments: /?arg1&arg2=5 runs "command arg1 arg2=5". SECURITY: this
	// lets any client who can reach the server influence the command line.
	URLArg bool `yaml:"url-arg"`

	// URLEnv turns the page URL's query parameters into extra environment
	// variables: /?arg1&arg2=5 runs the command with arg1= and arg2=5 set.
	// Mutually exclusive with URLArg. Same security caveat.
	URLEnv bool `yaml:"url-env"`

	// MaxClientsPerSession limits concurrent websockets attached to one
	// session (allows shared viewing). 0 = unlimited.
	MaxClientsPerSession int `yaml:"max-clients-per-session"`

	// PingInterval is the websocket keepalive ping period.
	PingInterval time.Duration `yaml:"ping-interval"`

	// ScrollbackLines is how many lines of output are buffered server-side so
	// a reconnecting client can repaint the screen. 0 disables replay.
	ScrollbackBytes int `yaml:"scrollback-bytes"`

	// FontSize is the terminal font size in CSS pixels.
	FontSize int `yaml:"font-size"`

	// DisableHyperlink turns off clickable links in the terminal (both
	// auto-detected URLs and OSC 8 explicit hyperlinks).
	DisableHyperlink bool `yaml:"disable-hyperlink"`

	// MiddleclickPaste pastes the clipboard on middle click (like a Linux
	// terminal). Set false to disable.
	MiddleclickPaste bool `yaml:"middleclick-paste"`

	// DOMRenderer uses xterm's DOM text renderer instead of the canvas one.
	// Slower scrolling, but immune to mobile GPU canvas blanking (Android
	// Chromium can black out the text canvas when sixel images allocate
	// GPU memory). Graphics still work: images draw on their own layer.
	DOMRenderer bool `yaml:"dom-renderer"`

	// EnableGraphics loads the xterm.js image addon, enabling inline
	// graphics via sixel and the iTerm2 inline image protocol. When false
	// the decoder isn't loaded and sixel support is not advertised to
	// applications.
	EnableGraphics bool `yaml:"enable-graphics"`

	// Title is the browser/page title.
	Title string `yaml:"title"`

	// --- Tab titles ---
	// Auto tab titles are "<process> <dir>" by default: the foreground
	// process name and the shell's cwd basename. Precedence of the options
	// below: TabTitle > TabShowPS1 > TabShowPsname/TabShowCwd.

	// TabShowPsname includes the foreground process name in auto titles.
	TabShowPsname bool `yaml:"tab-show-psname"`

	// TabShowCwd includes the working directory basename in auto titles.
	TabShowCwd bool `yaml:"tab-show-cwd"`

	// TabShowPS1 titles tabs with what the shell announces via OSC 0/2
	// window-title sequences (most PS1 setups emit "user@host:dir").
	// Overrides TabShowPsname/TabShowCwd.
	TabShowPS1 bool `yaml:"tab-show-ps1"`

	// TabTitle fixes every tab's title, overriding all auto-titling.
	TabTitle string `yaml:"tab-title"`

	// CloseOnExit: when the shell process exits, remove the session.
	CloseOnExit bool `yaml:"close-on-exit"`

	// AutoRespawn: when the last session ends, immediately start a new one.
	// When false (default), multi-session mode just leaves the tab bar
	// empty, and single-session mode offers restart on Enter. The first
	// page load always starts a terminal regardless.
	AutoRespawn bool `yaml:"auto-respawn"`
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
		DOMRenderer:          false,
		EnableGraphics:       true,
		DisableHyperlink:     false,
		MiddleclickPaste:     true,
		Title:                "TtyServe",
		CloseOnExit:          true,
		AutoRespawn:          false,
		TabShowPsname:        true,
		TabShowCwd:           true,
		TabShowPS1:           false,
		TabTitle:              "",
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
		return fmt.Errorf("tab-bar-position must be 'top' or 'right', got %q", c.TabBarPosition)
	}
	if c.SessionPersistence {
		switch c.PersistenceMode {
		case PersistByUser:
			if len(c.Users) == 0 {
				return fmt.Errorf("persistence-mode 'user' requires at least one user")
			}
		case PersistShortTerm:
			if c.IdleTimeout <= 0 {
				return fmt.Errorf("idle-timeout must be > 0 for short_term mode")
			}
		case PersistProxyHeader:
			if c.ProxyHeaderName == "" {
				c.ProxyHeaderName = "X-Forwarded-User"
			}
		case "":
			c.PersistenceMode = PersistShortTerm
		default:
			return fmt.Errorf("persistence-mode must be 'user', 'short_term' or 'proxy_header', got %q", c.PersistenceMode)
		}
	} else if c.IdleTimeout <= 0 {
		// Ephemeral mode uses the idle timeout to sweep stale page
		// identities; a non-positive value would reap a client in the gap
		// between page load and websocket connect.
		c.IdleTimeout = 5 * time.Minute
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
		return fmt.Errorf("url-arg and url-env are mutually exclusive")
	}
	// Tab title precedence: a fixed name beats PS1, which beats psname/cwd.
	if c.TabTitle != "" {
		c.TabShowPS1 = false
		c.TabShowPsname = false
		c.TabShowCwd = false
	}
	if c.TabShowPS1 {
		c.TabShowPsname = false
		c.TabShowCwd = false
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return fmt.Errorf("tls-cert-file and tls-key-file must both be set or both empty")
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
		return nil, fmt.Errorf("listen: unixs:// is not a thing; use unix:// — TLS is enabled by tls-cert-file/tls-key-file")
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
