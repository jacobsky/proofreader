package web

import (
	"embed"
)

// Files contains the necessary static resources used by the webservice such as css and javascript modules
//
//go:embed "assets"
var Files embed.FS

// AnalyzerPrompt contains the prompt data for the analysis agent
//
//go:embed "prompts/analyzer.md"
var AnalyzerPrompt string

// ProofreaderPrompt contains the prompt data for the proofreading agent
//
//go:embed "prompts/proofreader.md"
var ProofreaderPrompt string
