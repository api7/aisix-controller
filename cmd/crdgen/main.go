// Command crdgen writes the CRDs generated from the vendored AISIX schemas.
//
//	go run ./cmd/crdgen -schemas third_party/aisix -out config/crd
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/api7/aisix-controller/internal/schema"
)

func main() {
	src := flag.String("schemas", "third_party/aisix", "vendored AISIX schemas (with VERSION and schemas/)")
	out := flag.String("out", "config/crd", "output directory")
	flag.Parse()

	if err := run(*src, *out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(src, out string) error {
	files, err := schema.Generate(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	var names []string
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(out, f.Name), f.Body, 0o644); err != nil {
			return err
		}
		names = append(names, f.Name)
	}
	kustomization := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n"
	for _, n := range names {
		kustomization += "  - " + n + "\n"
	}
	if err := os.WriteFile(filepath.Join(out, "kustomization.yaml"), []byte(kustomization), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", strings.Join(names, ", "))
	return nil
}
