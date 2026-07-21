package config

import (
	"reflect"
	"testing"
)

func TestExpandHeaderEnv(t *testing.T) {
	headers := map[string]string{"X-Forwarded-User": "alice", "X-Api-Key": "k3y"}
	get := func(name string) string { return headers[name] }

	in := []string{
		"USER=${header.X-Forwarded-User}",
		"KEY=${header.X-Api-Key}",
		"PLAIN=bar",
		"MISSING=${header.X-Absent}",
		"TWO=${header.X-Forwarded-User}/${header.X-Api-Key}",
	}
	want := []string{"USER=alice", "KEY=k3y", "PLAIN=bar", "MISSING=", "TWO=alice/k3y"}
	got := ExpandHeaderEnv(in, get)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpandHeaderEnv = %#v, want %#v", got, want)
	}

	if ExpandHeaderEnv(nil, get) != nil {
		t.Error("empty env should return nil")
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"/bin/bash", []string{"/bin/bash"}},
		{"/usr/bin/tmux new -A -s main", []string{"/usr/bin/tmux", "new", "-A", "-s", "main"}},
		{`/bin/sh -c 'echo hi && exec bash'`, []string{"/bin/sh", "-c", "echo hi && exec bash"}},
		{`"/opt/my tool/bin" -x`, []string{"/opt/my tool/bin", "-x"}},
		{`echo a\ b`, []string{"echo", "a b"}},
		{`echo "it's"`, []string{"echo", "it's"}},
		{"  spaced   out  ", []string{"spaced", "out"}},
		{"", nil},
	}
	for _, c := range cases {
		got, err := splitCommand(c.in)
		if err != nil {
			t.Errorf("splitCommand(%q): unexpected error %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitCommand(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}

	for _, bad := range []string{`echo "unclosed`, `echo 'unclosed`, `trailing\`} {
		if _, err := splitCommand(bad); err == nil {
			t.Errorf("splitCommand(%q): expected error, got none", bad)
		}
	}
}

func TestValidateSplitsCommand(t *testing.T) {
	cfg := Default()
	cfg.Command = "/usr/bin/env FOO=1 bash -l"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Command != "/usr/bin/env" {
		t.Errorf("Command = %q, want /usr/bin/env", cfg.Command)
	}
	if want := []string{"FOO=1", "bash", "-l"}; !reflect.DeepEqual(cfg.Args, want) {
		t.Errorf("Args = %#v, want %#v", cfg.Args, want)
	}

	cfg = Default()
	cfg.Command = "   "
	if err := cfg.Validate(); err == nil {
		t.Error("Validate: expected error for blank command")
	}
}

func TestValidateOptionInteractions(t *testing.T) {
	cfg := Default()
	cfg.URLArg = true
	cfg.URLEnv = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: url-arg + url-env may be combined: %v", err)
	}

	cfg = Default()
	cfg.Readonly = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !cfg.Readonly {
		t.Error("Validate must not reset Readonly")
	}

	for _, v := range []string{"none", "sound", "visual", "both"} {
		cfg = Default()
		cfg.Bell = v
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate: bell %q rejected: %v", v, err)
		}
	}
	cfg = Default()
	cfg.Bell = ""
	if err := cfg.Validate(); err != nil || cfg.Bell != "sound" {
		t.Errorf("Validate: empty bell should default to sound, got %q err %v", cfg.Bell, err)
	}
	cfg = Default()
	cfg.Bell = "loud"
	if err := cfg.Validate(); err == nil {
		t.Error("Validate: invalid bell should be rejected")
	}
}

func TestParseSocketPerm(t *testing.T) {
	cases := []struct {
		in       string
		mode     uint32
		uid, gid int
		wantErr  bool
	}{
		{"", 0, 0, 0, false}, // unset -> nil spec
		{"0660", 0o660, -1, -1, false},
		{"660", 0o660, -1, -1, false},
		{"0600:0", 0o600, 0, -1, false},
		{"0660:0:0", 0o660, 0, 0, false},
		{"0660::0", 0o660, -1, 0, false},
		{"0999", 0, 0, 0, true},
		{"abc", 0, 0, 0, true},
		{"0660:nonexistent-user-xyz", 0, 0, 0, true},
		{"0660:0:nonexistent-group-xyz", 0, 0, 0, true},
		{"0660:0:0:extra", 0, 0, 0, true},
	}
	for _, c := range cases {
		cfg := Default()
		cfg.SocketPerm = c.in
		spec, err := cfg.ParseSocketPerm()
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseSocketPerm(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSocketPerm(%q): %v", c.in, err)
			continue
		}
		if c.in == "" {
			if spec != nil {
				t.Errorf("ParseSocketPerm(\"\"): want nil spec")
			}
			continue
		}
		if uint32(spec.Mode) != c.mode || spec.UID != c.uid || spec.GID != c.gid {
			t.Errorf("ParseSocketPerm(%q) = mode %o uid %d gid %d, want %o %d %d",
				c.in, spec.Mode, spec.UID, spec.GID, c.mode, c.uid, c.gid)
		}
	}
}

func TestValidateTabTitlePrecedence(t *testing.T) {
	cfg := Default()
	cfg.TabShowPS1 = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.TabShowPsname || cfg.TabShowCwd {
		t.Error("tab-show-ps1 must disable psname/cwd titling")
	}

	cfg = Default()
	cfg.TabTitle = "fixed"
	cfg.TabShowPS1 = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.TabShowPS1 || cfg.TabShowPsname || cfg.TabShowCwd {
		t.Error("tab-title must disable all auto-titling")
	}
}
