package agent

import (
	"fmt"
	"strings"
)

const EngineNodeNameParam = "fev.node_name"

const DefaultNodeName = "fev"

func CheckNodeName(name string) error {
	if name == "" || len(name) > 253 {
		return fmt.Errorf("a node name is 1 to 253 characters")
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("each dot-separated part of a node name is 1 to 63 characters")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("a part of a node name does not begin or end with a hyphen")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("a node name has letters, digits, hyphens and dots only, not %q", c)
			}
		}
	}
	return nil
}
