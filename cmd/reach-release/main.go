package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aead.dev/minisign"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "reach-release: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "manifest":
		return manifestCommand(args[1:])
	case "sign":
		return signCommand(args[1:])
	case "keygen":
		return keygenCommand(args[1:])
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type releaseManifest struct {
	Schema    int                     `json:"schema"`
	Project   string                  `json:"project"`
	Version   string                  `json:"version"`
	GitCommit string                  `json:"git_commit,omitempty"`
	CreatedAt string                  `json:"created_at"`
	Assets    map[string]releaseAsset `json:"assets"`
}

type releaseAsset struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func manifestCommand(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
	dir := fs.String("dir", ".", "directory containing release assets")
	version := fs.String("version", "", "release version")
	commit := fs.String("commit", "", "git commit")
	outPath := fs.String("out", "", "manifest output path (default: DIR/manifest.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *version == "" {
		return fmt.Errorf("--version is required")
	}
	if *outPath == "" {
		*outPath = filepath.Join(*dir, "manifest.json")
	}
	assets := map[string]releaseAsset{}
	assetNames := []string{
		"reach-agent_linux_amd64",
		"reach-agent_linux_arm64",
		"reach-agent_linux_386",
		"reach-agent_linux_armv6",
		"reach-agent_linux_armv7",
		"reach-agent_darwin_amd64",
		"reach-agent_darwin_arm64",
	}
	for _, name := range assetNames {
		path := filepath.Join(*dir, name)
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		assets[name] = releaseAsset{SHA256: hex.EncodeToString(sum[:]), Size: st.Size()}
	}
	manifest := releaseManifest{Schema: 1, Project: "reach-agent", Version: *version, GitCommit: *commit, CreatedAt: nowUTC(), Assets: assets}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.WriteFile(*outPath, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", *outPath)
	return nil
}

func signCommand(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyPath := fs.String("key", defaultReleaseKeyPath(), "minisign private key path")
	passwordFile := fs.String("password-file", "", "file containing the minisign private key password")
	passwordEnv := fs.String("password-env", "REACH_RELEASE_KEY_PASSWORD", "environment variable containing the minisign private key password")
	manifestPath := fs.String("manifest", "", "manifest.json path to sign")
	outPath := fs.String("out", "", "signature output path (default: manifest.json.minisig)")
	trustedComment := fs.String("trusted-comment", "", "trusted minisign comment")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *manifestPath == "" {
		return fmt.Errorf("--manifest is required")
	}
	if *outPath == "" {
		*outPath = *manifestPath + ".minisig"
	}
	password, err := releaseKeyPassword(*passwordFile, *passwordEnv)
	if err != nil {
		return err
	}
	privateKey, err := minisign.PrivateKeyFromFile(password, *keyPath)
	if err != nil {
		return fmt.Errorf("read private key: %w", err)
	}
	manifest, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	comment := *trustedComment
	if comment == "" {
		sum := sha256.Sum256(manifest)
		comment = "reach-agent release manifest sha256=" + hex.EncodeToString(sum[:])
	}
	sig := minisign.SignWithComments(privateKey, manifest, comment, "untrusted comment: Reach release manifest signature")
	if err := os.WriteFile(*outPath, sig, 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s -> %s\n", *manifestPath, *outPath)
	return nil
}

func keygenCommand(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	keyPath := fs.String("key", defaultReleaseKeyPath(), "minisign private key path")
	pubPath := fs.String("pub", "", "public key output path")
	passwordFile := fs.String("password-file", "", "password file to create")
	force := fs.Bool("force", false, "overwrite existing key files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pubPath == "" {
		*pubPath = strings.TrimSuffix(*keyPath, filepath.Ext(*keyPath)) + ".pub"
	}
	for _, p := range []string{*keyPath, *pubPath, *passwordFile} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil && !*force {
			return fmt.Errorf("%s already exists; use --force to overwrite", p)
		}
	}
	publicKey, privateKey, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	password := ""
	if *passwordFile != "" {
		passwordRaw := make([]byte, 32)
		if _, err := rand.Read(passwordRaw); err != nil {
			return err
		}
		password = base64.RawStdEncoding.EncodeToString(passwordRaw)
	}
	privateKeyText, err := privateKey.MarshalText()
	if err != nil {
		return err
	}
	if password != "" {
		privateKeyText, err = minisign.EncryptKey(password, privateKey)
		if err != nil {
			return err
		}
	}
	publicKeyText, err := publicKey.MarshalText()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*keyPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(*keyPath, privateKeyText, 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*pubPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*pubPath, publicKeyText, 0o644); err != nil {
		return err
	}
	if *passwordFile != "" {
		if err := os.MkdirAll(filepath.Dir(*passwordFile), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(*passwordFile, []byte(password+"\n"), 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("public key: %s\n", publicKey.String())
	fmt.Printf("private key: %s\n", *keyPath)
	if *passwordFile != "" {
		fmt.Printf("password file: %s\n", *passwordFile)
	}
	return nil
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func releaseKeyPassword(passwordFile, passwordEnv string) (string, error) {
	if passwordFile != "" {
		b, err := os.ReadFile(passwordFile)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	if passwordEnv != "" {
		return os.Getenv(passwordEnv), nil
	}
	return "", nil
}

func defaultReleaseKeyPath() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".minisign", "reach-release.key")
	}
	return "reach-release.key"
}

func usage() {
	fmt.Print(`Reach release helper

Usage:
  reach-release manifest --dir DIR --version VERSION [--commit COMMIT]
  reach-release sign --manifest manifest.json [--key ~/.minisign/reach-release.key] [--password-file FILE]
  reach-release keygen [--key ~/.minisign/reach-release.key] [--pub ~/.minisign/reach-release.pub] [--password-file FILE]
`)
}
