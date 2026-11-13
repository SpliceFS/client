package main

import (
	"errors"
	"io"

	"go.yaml.in/yaml/v3"

	"github.com/SpliceFS/client/config"
)

// shownConfig is what config show prints. It holds only paths to password files, never a
// password, so nothing secret can reach the output.
type shownConfig struct {
	Cache    config.Cache              `yaml:"cache"`
	Clusters map[string]config.Cluster `yaml:"clusters"`
	Mounts   []shownMount              `yaml:"mounts"`
}

// shownMount is a mount led by its name, which the configuration takes from the file name,
// so that the list says which mount each entry is.
type shownMount struct {
	Name string `yaml:"name"`
	// Mount is inlined, not embedded: embedded fields must come first, and the name has to
	// lead the entry.
	Mount config.Mount `yaml:",inline"`
}

// yamlIndent is the number of spaces config show indents a level by.
const yamlIndent = 2

// showConfig writes the effective configuration as YAML. A value that a file set ends with
// a comment naming that file; a value nobody set is a default and has none.
func showConfig(w io.Writer, cfg *config.Config) error {
	shown := shownConfig{Cache: cfg.Cache, Clusters: cfg.Clusters}
	for i := range cfg.Mounts {
		shown.Mounts = append(shown.Mounts, shownMount{Name: cfg.Mounts[i].Name, Mount: cfg.Mounts[i]})
	}
	// Encoding these types cannot fail; if it ever did, the encoder below reports the empty
	// node and this error together.
	var doc yaml.Node
	encodeErr := doc.Encode(shown)
	annotate(&doc, "", cfg.Sources)
	enc := yaml.NewEncoder(w)
	enc.SetIndent(yamlIndent)
	return errors.Join(encodeErr, enc.Encode(&doc))
}

// annotate puts the file of every value in sources behind that value; n is a mapping. The
// entries of the mounts list are keyed by their name, as sources is.
func annotate(n *yaml.Node, prefix string, sources map[string]string) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		path := key.Value
		if prefix != "" {
			path = prefix + "." + path
		}
		file, set := sources[path]
		switch {
		case value.Kind == yaml.MappingNode:
			annotate(value, path, sources)
		case value.Kind == yaml.SequenceNode:
			for _, item := range value.Content {
				annotate(item, path+"."+item.Content[1].Value, sources)
			}
		case set:
			value.LineComment = "# " + file
		default:
			// A value nobody set is a default and carries no comment.
		}
	}
}
