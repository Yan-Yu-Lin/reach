package main

import (
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
