package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/ssh"
)

var (
	slugRE           = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)
	unixUserRE       = regexp.MustCompile(`^rt-[a-z0-9]{8}$`)
	sshConfigTokenRE = regexp.MustCompile(`^[A-Za-z0-9_.@%+=:,/\\-]+$`)
)

type argonParams struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	saltLength  uint32
	keyLength   uint32
}

var defaultArgon = argonParams{memory: 64 * 1024, iterations: 3, parallelism: 2, saltLength: 16, keyLength: 32}

func HashSecret(secret string) (string, error) {
	if secret == "" {
		return "", errors.New("secret is empty")
	}
	salt := make([]byte, defaultArgon.saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(secret), salt, defaultArgon.iterations, defaultArgon.memory, defaultArgon.parallelism, defaultArgon.keyLength)
	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Hash := base64.RawStdEncoding.EncodeToString(hash)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", defaultArgon.memory, defaultArgon.iterations, defaultArgon.parallelism, b64Salt, b64Hash), nil
}

func VerifySecret(secret, encoded string) bool {
	if secret == "" || encoded == "" {
		return false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory, iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	actual := argon2.IDKey([]byte(secret), salt, iterations, memory, parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func RandomToken(prefix string, n int) (string, error) {
	if n <= 0 {
		n = 24
	}
	s, err := randomAlphabet(n)
	if err != nil {
		return "", err
	}
	if prefix != "" {
		return prefix + "_" + s, nil
	}
	return s, nil
}

func RandomID(prefix string) (string, error) { return RandomToken(prefix, 6) }

func RandomShortID() (string, error) { return randomAlphabet(8) }

func randomAlphabet(n int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = alphabet[v.Int64()]
	}
	return string(out), nil
}

func ValidateSlug(s string) error {
	if !slugRE.MatchString(s) {
		return fmt.Errorf("invalid slug %q: must be lowercase [a-z0-9-], 3-32 chars, no leading/trailing hyphen", s)
	}
	return nil
}

func ValidateTargetUser(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > 128 || strings.HasPrefix(s, "-") || strings.Contains(s, ":") || strings.ContainsAny(s, "\x00\r\n\t ") {
		return fmt.Errorf("invalid target_user %q", s)
	}
	return nil
}

func ValidateUnixUser(s string) error {
	if !unixUserRE.MatchString(s) {
		return fmt.Errorf("invalid reach unix user %q", s)
	}
	return nil
}

func ValidateSSHConfigToken(name, s string) error {
	if s == "" || !sshConfigTokenRE.MatchString(s) || strings.HasPrefix(s, "-") || strings.ContainsAny(s, "\r\n\t ") {
		return fmt.Errorf("invalid %s for SSH config", name)
	}
	return nil
}

func NormalizeSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastHyphen := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastHyphen = false
			continue
		}
		if !lastHyphen {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 32 {
		out = strings.Trim(out[:32], "-")
	}
	if len(out) < 3 {
		out = "box-" + out
	}
	return out
}

func ValidateSSHPublicKey(pub string) (kind string, err error) {
	pub = strings.TrimSpace(pub)
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil {
		return "", err
	}
	return key.Type(), nil
}

type Claims struct {
	UserID   string `json:"uid"`
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

func SignJWT(secret, userID, username, role string, ttl time.Duration) (string, error) {
	if secret == "" {
		return "", errors.New("jwt secret is empty")
	}
	if ttl <= 0 {
		return "", errors.New("jwt ttl must be positive")
	}
	now := time.Now()
	claims := Claims{
		UserID: userID, Username: username, Role: role,
		RegisteredClaims: jwt.RegisteredClaims{IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(ttl))},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

func ParseJWT(secret, tokenString string) (*Claims, error) {
	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}

func Fingerprint(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])[:16]
}

func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
