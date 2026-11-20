package main

import (
	"strings"
	"testing"
	"time"
)

func TestValidateTargetUserAllowsDirectoryNames(t *testing.T) {
	valid := []string{
		"root",
		"admin",
		`VDI\111360128`,
		"user@example.com",
		`DOMAIN\user.name`,
	}
	for _, user := range valid {
		if err := ValidateTargetUser(user); err != nil {
			t.Fatalf("ValidateTargetUser(%q) returned error: %v", user, err)
		}
	}
}

func TestValidateTargetUserRejectsUnsafeNames(t *testing.T) {
	invalid := []string{
		"-root",
		"bad:user",
		"bad user",
		"bad\nuser",
		"bad\tuser",
	}
	for _, user := range invalid {
		if err := ValidateTargetUser(user); err == nil {
			t.Fatalf("ValidateTargetUser(%q) unexpectedly succeeded", user)
		}
	}
}

func TestValidateSSHConfigTokenAllowsBackslashUsers(t *testing.T) {
	if err := ValidateSSHConfigToken("target user", `VDI\111360128`); err != nil {
		t.Fatalf("ValidateSSHConfigToken rejected domain user: %v", err)
	}
}

func TestSignJWTUsesConfiguredTTL(t *testing.T) {
	ttl := 48 * time.Hour
	before := time.Now()
	tok, err := SignJWT("test-secret", "u1", "admin", "owner", ttl)
	if err != nil {
		t.Fatalf("SignJWT returned error: %v", err)
	}
	claims, err := ParseJWT("test-secret", tok)
	if err != nil {
		t.Fatalf("ParseJWT returned error: %v", err)
	}
	if claims.ExpiresAt == nil {
		t.Fatal("token has no expiry")
	}
	min := before.Add(ttl - time.Second)
	max := before.Add(ttl + time.Second)
	if claims.ExpiresAt.Time.Before(min) || claims.ExpiresAt.Time.After(max) {
		t.Fatalf("expiry %s outside expected range [%s, %s]", claims.ExpiresAt.Time, min, max)
	}
}

// PoC from daybreak P2-2 / Opus P2-2.
func TestPublicKeyMustBeSingleCanonicalLine(t *testing.T) {
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	for _, bad := range []string{
		key + " c\n" + key + " second",
		key + "\r\n" + key,
		`command="/bin/sh" ` + key,
		key + " c\x00",
		"",
	} {
		if _, err := ValidateSSHPublicKey(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
		if _, err := authorizedKeyLine(9200, bad, ""); err == nil {
			t.Fatalf("authorizedKeyLine accepted %q", bad)
		}
	}
	line, err := authorizedKeyLine(9200, "  "+key+"   my comment  ", "2030-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(line, "\n") != 0 || !strings.HasSuffix(line, " "+key) || !strings.Contains(line, `permitlisten="127.0.0.1:9200"`) || !strings.Contains(line, "expiry-time=") {
		t.Fatalf("unexpected authorized_keys line %q", line)
	}
}

// Opus P2-3: rt-* accounts must not create Unix sockets or run ~/.ssh/rc.
func TestSSHDMatchBlockDisablesStreamLocalAndUserRC(t *testing.T) {
	conf := sshdMatchConfig("rt-abcdefgh", 9200)
	for _, want := range []string{"AllowStreamLocalForwarding no", "PermitUserRC no", "AllowTcpForwarding remote", "PermitListen 127.0.0.1:9200", "ForceCommand /bin/false"} {
		if !strings.Contains(conf, want) {
			t.Fatalf("Match block missing %q", want)
		}
	}
	if strings.Contains(conf, "DisableForwarding yes") {
		t.Fatal("DisableForwarding would break the reverse tunnel")
	}
}
