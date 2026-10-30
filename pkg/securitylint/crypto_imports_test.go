// Package securitylint holds tests that check security properties of the source tree itself, rather than of
// one package's behaviour. It lives under pkg/ so `make test` runs it.
package securitylint

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// allowedCryptoImports are the only cryptographic packages production code may import: standard-library
// primitives, with no custom implementations, no weak algorithms (MD5, SHA-1, DES, RC4, DSA) and no
// third-party crypto. Request signing (SigV4) and TLS to AWS are done by Mountpoint and the AWS SDK, not here.
var allowedCryptoImports = []string{
	"crypto/rand",
	"crypto/sha256",
	"crypto/tls",
	"crypto/x509",
}

// isCryptoImport reports whether an import path is a cryptographic package, standard library or not: any of
// its path elements starts with "crypto", as in crypto/sha256, golang.org/x/crypto or a third-party cryptolib.
func isCryptoImport(path string) bool {
	return slices.ContainsFunc(strings.Split(path, "/"), func(elem string) bool {
		return strings.HasPrefix(elem, "crypto")
	})
}

func TestProductionCodeImportsOnlyApprovedCrypto(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, dir := range []string{"cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			// Test code may use anything; only what ships is checked.
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			checked++
			for _, imp := range file.Imports {
				importPath, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return err
				}
				if isCryptoImport(importPath) && !slices.Contains(allowedCryptoImports, importPath) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s imports %q, which is not an approved cryptographic package %v", rel, importPath, allowedCryptoImports)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Guards against the walk silently finding nothing, e.g. after the package moves.
	if checked < 50 {
		t.Fatalf("checked only %d files under cmd/ and pkg/, expected the whole tree", checked)
	}
}

func TestIsCryptoImport(t *testing.T) {
	for path, want := range map[string]bool{
		"crypto/sha256":                  true,
		"crypto/md5":                     true,
		"golang.org/x/crypto/chacha20":   true,
		"github.com/example/cryptolib":   true,
		"github.com/example/encrypted":   false,
		"github.com/example/crypto/aes":  true,
		"encoding/json":                  false,
		"k8s.io/apimachinery/pkg/util/x": false,
	} {
		if got := isCryptoImport(path); got != want {
			t.Errorf("isCryptoImport(%q) = %v, want %v", path, got, want)
		}
	}
}
