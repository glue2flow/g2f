package main

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/glue2flow/g2f/internal/plugin"
	"github.com/glue2flow/g2f/internal/plugin/signing"
)

const pluginUsage = `Usage: g2f plugin <command> [flags]

Commands:
  keygen   --name <name> [--out <file>]                  create a signing key
  sign     --key <file> [--out <file>] <bundle>          sign a .zip or .yaml plugin
  verify   [--keys <name=key,...>] [--sig <file>] <bundle>
  catalog  <dir> [--base-url <url>] [--key <file>] [--name <name>] [--out <file>]
                                                         write a catalog index.json

Run "g2f plugin <command> --help" for details. See docs/plugins.md.
`

// pluginCommand runs `g2f plugin ...` and returns the exit code.
func pluginCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stderr, pluginUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	cmds := map[string]func([]string, io.Writer, io.Writer) error{
		"keygen": pluginKeygen, "sign": pluginSign, "verify": pluginVerify, "catalog": pluginCatalog,
	}
	run, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], pluginUsage)
		return 2
	}
	if err := run(args[1:], stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

// parseFlags parses flags before and after positional arguments
// ("catalog <dir> --base-url x") and returns the positional ones.
func parseFlags(fs *flag.FlagSet, args []string, stderr io.Writer) ([]string, error) {
	fs.SetOutput(stderr)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func pluginKeygen(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("g2f plugin keygen", flag.ContinueOnError)
	name := fs.String("name", "", "key name, shown as the signer of plugins (required)")
	out := fs.String("out", "", "private key file to create (default <name>.key); never overwritten")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: g2f plugin keygen --name <name> [--out <file>]\n\nCreates an Ed25519 signing key. The private key file is written with mode 0600;\nthe line printed on stdout goes into G2F_PLUGIN_TRUSTED_KEYS.")
		fs.PrintDefaults()
	}
	if pos, err := parseFlags(fs, args, stderr); err != nil {
		return err
	} else if len(pos) > 0 || *name == "" {
		fs.Usage()
		return errors.New("--name is required and no arguments are accepted")
	}
	if *out == "" {
		*out = *name + ".key"
	}
	kf, entry, err := signing.GenerateKey(*name)
	if err != nil {
		return err
	}
	if err := signing.WriteKeyFile(*out, kf); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Private key written to %s. Keep it secret.\nAdd this entry to G2F_PLUGIN_TRUSTED_KEYS:\n", *out)
	fmt.Fprintln(stdout, entry)
	return nil
}

func pluginSign(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("g2f plugin sign", flag.ContinueOnError)
	keyFile := fs.String("key", "", "private key file from `g2f plugin keygen` (required)")
	out := fs.String("out", "", "signature file (default <bundle>.sig)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: g2f plugin sign --key <file> [--out <file>] <bundle.zip|plugin.yaml>\n\nWrites the detached signature next to the bundle. Upload both, or list the\nsignature in a catalog.")
		fs.PrintDefaults()
	}
	pos, err := parseFlags(fs, args, stderr)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *keyFile == "" {
		fs.Usage()
		return errors.New("--key and exactly one bundle are required")
	}
	keyID, key, err := signing.ReadKeyFile(*keyFile)
	if err != nil {
		return err
	}
	sigFile, err := signBundle(pos[0], *out, keyID, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s signed by %q: %s\n", pos[0], keyID, sigFile)
	return nil
}

func signBundle(bundle, out, keyID string, key ed25519.PrivateKey) (string, error) {
	data, err := os.ReadFile(bundle)
	if err != nil {
		return "", err
	}
	// Refuse what the server would refuse, before anyone ships it.
	if _, err := plugin.ParseBundle(filepath.Base(bundle), data); err != nil {
		return "", fmt.Errorf("%s: %w", bundle, err)
	}
	sig, err := signing.Sign(filepath.Base(bundle), data, keyID, key)
	if err != nil {
		return "", err
	}
	if out == "" {
		out = bundle + ".sig"
	}
	return out, os.WriteFile(out, sig.Marshal(), 0o644)
}

func pluginVerify(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("g2f plugin verify", flag.ContinueOnError)
	keys := fs.String("keys", os.Getenv("G2F_PLUGIN_TRUSTED_KEYS"), "trusted keys, name=base64,... (default $G2F_PLUGIN_TRUSTED_KEYS)")
	sigFile := fs.String("sig", "", "signature file (default <bundle>.sig)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: g2f plugin verify [--keys <name=key,...>] [--sig <file>] <bundle>\n\nChecks a bundle's signature like the server does on import.")
		fs.PrintDefaults()
	}
	pos, err := parseFlags(fs, args, stderr)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errors.New("exactly one bundle is required")
	}
	trusted, err := signing.ParseKeys(*keys)
	if err != nil {
		return fmt.Errorf("--keys: %w", err)
	}
	if len(trusted) == 0 {
		return errors.New("no trusted keys: pass --keys or set G2F_PLUGIN_TRUSTED_KEYS")
	}
	signer, err := verifyBundle(pos[0], *sigFile, trusted)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "OK: %s is signed by %q\n", pos[0], signer)
	return nil
}

func verifyBundle(bundle, sigFile string, keys signing.Keys) (string, error) {
	data, err := os.ReadFile(bundle)
	if err != nil {
		return "", err
	}
	if sigFile == "" {
		sigFile = bundle + ".sig"
	}
	raw, err := os.ReadFile(sigFile)
	if err != nil {
		return "", err
	}
	sig, err := signing.Parse(raw)
	if err != nil {
		return "", err
	}
	return keys.Verify(filepath.Base(bundle), data, sig)
}

func pluginCatalog(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("g2f plugin catalog", flag.ContinueOnError)
	baseURL := fs.String("base-url", "", "URL the bundles are served under (default: URLs relative to the index)")
	keyFile := fs.String("key", "", "sign every bundle with this key (default: use <bundle>.sig files if present)")
	name := fs.String("name", "", "catalog name shown in the UI")
	out := fs.String("out", "", "index file (default <dir>/index.json)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: g2f plugin catalog <dir> [--base-url <url>] [--key <file>] [--name <name>] [--out <file>]\n\nScans <dir> for .zip and .yaml bundles and writes a catalog index with their\nsha256 and signatures. Serve the directory over https and add the index URL\nto G2F_PLUGIN_CATALOGS.")
		fs.PrintDefaults()
	}
	pos, err := parseFlags(fs, args, stderr)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errors.New("exactly one directory is required")
	}
	var keyID string
	var key ed25519.PrivateKey
	if *keyFile != "" {
		if keyID, key, err = signing.ReadKeyFile(*keyFile); err != nil {
			return err
		}
	}
	idx, err := plugin.BuildCatalog(pos[0], *name, *baseURL, keyID, key)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = filepath.Join(pos[0], "index.json")
	}
	data, _ := json.MarshalIndent(idx, "", "  ")
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: %d plugins\n", *out, len(idx.Plugins))
	return nil
}
