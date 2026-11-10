package schema

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/api7/aisix-controller/api/v1alpha1"
)

// File is one generated CRD manifest.
type File struct {
	Name string
	Body []byte
}

// Generate renders every kind's CRD from a vendored schema directory
// (holding VERSION and schemas/).
func Generate(dir string) ([]File, error) {
	version, err := os.ReadFile(filepath.Join(dir, "VERSION"))
	if err != nil {
		return nil, err
	}
	var out []File
	for _, k := range v1alpha1.Kinds {
		s, err := LoadSchema(filepath.Join(dir, "schemas"), k)
		if err != nil {
			return nil, err
		}
		body, err := CRDYAML(k, s, strings.TrimSpace(string(version)))
		if err != nil {
			return nil, err
		}
		out = append(out, File{Name: FileName(k), Body: body})
	}
	return out, nil
}
