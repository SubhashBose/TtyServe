package config

import (
	"reflect"
	"testing"
)

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
