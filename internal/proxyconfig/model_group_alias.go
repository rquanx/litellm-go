package proxyconfig

import (
	"errors"

	"go.yaml.in/yaml/v3"
)

type modelGroupAlias struct {
	Model string `yaml:"model"`
	Hidden bool `yaml:"hidden"`
}

func (alias *modelGroupAlias) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		alias.Model = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return errors.New("expected a model group alias string or object")
	}
	for index := 0; index < len(node.Content); index += 2 {
		switch node.Content[index].Value {
		case "model", "hidden":
		default:
			return errors.New("unsupported model group alias field")
		}
	}
	type fields modelGroupAlias
	return node.Decode((*fields)(alias))
}
