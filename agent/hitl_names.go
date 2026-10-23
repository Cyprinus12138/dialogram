package agent

import "github.com/Cyprinus12138/dialogram/tool"

// Local aliases of the built-in pause tool names, so the detectors read
// cleanly without repeating the package qualifier.
const (
	hitlInputName  = tool.HITLInputToolName
	hitlChoiceName = tool.HITLChoiceToolName
	hitlFormName   = tool.HITLFormToolName
)

func isBuiltinHITL(name string) bool { return tool.IsHITLTool(name) }
