package config

import (
	"fmt"
	"os"
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
)

// User is a single basic-auth credential.
type User struct {
	Name     string `yaml:"name"`
	Password string `yaml:"password"`
}

// Config is the full application configuration.
type Config struct {
	// Listen address, e.g. ":7681" or "127.0.0.1:7681".
	Listen string `yaml:"listen"`

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
	// WriteEnabled allows client keyboard input. When false, terminals are
	// read-only (view/share mode).
	WriteEnabled bool `yaml:"write_enabled"`

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

	// Title is the browser/page title.
	Title string `yaml:"title"`

	// CloseOnExit: when the shell process exits, remove the session.
	CloseOnExit bool `yaml:"close_on_exit"`
}

// Default returns a Config populated with reasonable defaults.
func Default() Config {
	return Config{
		Listen:               ":7681",
		Command:              defaultShell(),
		SessionPersistence:   true,
		PersistenceMode:      PersistShortTerm,
		MultiSession:         true,
		MaxSessionsPerClient: 0,
		TabBarPosition:       "top",
		AuthRealm:            "ttyserve",
		IdleTimeout:          5 * time.Minute,
		CookieName:           "ttyserve_session",
		CookieSecure:         false,
		WriteEnabled:         true,
		MaxClientsPerSession: 0,
		PingInterval:         20 * time.Second,
		ScrollbackBytes:      256 * 1024,
		FontSize:             14,
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

// Load reads a YAML config file, overlaying it on top of defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, cfg.Validate()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, cfg.Validate()
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
		case "":
			c.PersistenceMode = PersistShortTerm
		default:
			return fmt.Errorf("persistence_mode must be 'user' or 'short_term', got %q", c.PersistenceMode)
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
