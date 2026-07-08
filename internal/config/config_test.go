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
	if err := cfg.Validate(); err == nil {
		t.Error("Validate: url_arg + url_env should be rejected")
	}

	cfg = Default()
	cfg.Readonly = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !cfg.Readonly {
		t.Error("Validate must not reset Readonly")
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
