// Package prompts embeds the prompt text the service sends to the model.
//
// The prompt is compiled into the binary rather than read from disk or
// configuration so that a given build always sends exactly the prompt it
// was tested against: changing it is a code change with a diff and a
// review, not a runtime surprise.
package prompts

import _ "embed"

// AnalysisSystem is the system message for the statement analysis call.
//
//go:embed analysis_system.md
var AnalysisSystem string
