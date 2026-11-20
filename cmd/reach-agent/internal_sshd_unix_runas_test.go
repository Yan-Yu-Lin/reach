//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/user"
	"reflect"
	"strings"
	"testing"
)

func TestFilterClientEnvDropsIdentityAndLoader(t *testing.T) {
	got := filterClientEnv([]string{"LANG=C", "HOME=/root", "LD_PRELOAD=x", "DYLD_INSERT_LIBRARIES=y", "PATH=/evil", "TERM=xterm"})
	if !reflect.DeepEqual(got, []string{"LANG=C", "TERM=xterm"}) {
		t.Fatalf("filtered env = %v", got)
	}
}

func TestLookupRunAsRefusesRoot(t *testing.T) {
	if _, err := lookupRunAs("root"); err == nil {
		t.Fatal("run-as root accepted")
	}
	me, err := user.Current()
	if err != nil || me.Uid == "0" {
		t.Skip("needs a non-root current user")
	}
	c, err := lookupRunAs(me.Username)
	if err != nil {
		t.Fatal(err)
	}
	if c.home == "" || c.name != me.Username {
		t.Fatalf("cred = %+v", c)
	}
}

// Run the real session code path as root when available (CI/containers);
// on a developer Mac this is skipped.
func TestRunAsDropsPrivilegesWhenRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("no nobody user")
	}
	c, err := lookupRunAs(u.Username)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/id", "-u")
	cmd.SysProcAttr = runAsSysProcAttr(c)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != u.Uid {
		t.Fatalf("ran as uid %s, want %s", out, u.Uid)
	}
}
