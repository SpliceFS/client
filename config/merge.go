package config

import (
	"bytes"
	"errors"
	"io"

	"go.yaml.in/yaml/v3"
)

// decodeStrict decodes the single YAML document in data into out, which may already hold
// values from an earlier file: the decoder only sets the keys the document has, replaces
// lists whole and merges nested mappings key by key, which is the merge the settings
// files need. It rejects unknown keys and a second document. It returns the dotted key
// path of every value the document sets (a list counts as one value), so the caller can
// tell which file a value came from. An empty document sets nothing.
func decodeStrict(data []byte, out any) ([]string, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err //nolint:wrapcheck // The caller adds the file name, the YAML error says the rest.
	}
	if err := rejectSecondDocument(dec); err != nil {
		return nil, err
	}
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	if err := strict.Decode(out); err != nil {
		return nil, err //nolint:wrapcheck // The caller adds the file name, the YAML error says the rest.
	}
	return leafPaths(doc.Content[0], ""), nil
}

// rejectSecondDocument fails when dec has another YAML document to read.
func rejectSecondDocument(dec *yaml.Decoder) error {
	var next yaml.Node
	err := dec.Decode(&next)
	switch {
	case err == nil:
		return ErrMultipleDocuments
	case !errors.Is(err, io.EOF):
		return err //nolint:wrapcheck // The caller adds the file name, the YAML error says the rest.
	}
	return nil
}

// leafPaths lists the dotted paths of the values under a mapping node. An explicit null
// sets nothing, and an alias is not followed because an anchor may refer to itself.
func leafPaths(n *yaml.Node, prefix string) []string {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	var paths []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		path := n.Content[i].Value
		if prefix != "" {
			path = prefix + "." + path
		}
		value := n.Content[i+1]
		switch {
		case value.Kind == yaml.MappingNode:
			paths = append(paths, leafPaths(value, path)...)
		case value.Kind == yaml.ScalarNode && value.Tag == "!!null":
		default:
			paths = append(paths, path)
		}
	}
	return paths
}
